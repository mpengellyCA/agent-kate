// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

// Package gitstatus serves read-only git state — per-worktree status, per-file
// hunks vs HEAD — to the UI over JSON-RPC. It is cached per thread so the
// dashboard's 1 Hz poll and N gutter polls all share one git walk per dirty tick.
//
// Lifecycle is decoupled from query: the worktree package still owns Create /
// Promote / Land / Remove. This package only observes.
package gitstatus

import (
	"sync"
	"time"

	"agentkate/internal/worktree"
)

// Cache holds per-thread git status, keyed by thread id. Snapshots and per-file
// hunks are recomputed on demand and shared across concurrent readers.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

// entry is the cached state for one worktree. Its own mutex serialises the
// "is this dirty? recompute" check so multiple pollers share one git walk.
type entry struct {
	mu sync.Mutex

	wt worktree.Worktree

	snapshot   *Snapshot
	fileHunks  map[string]hunkCacheLine
	generation uint64
	dirty      bool // recompute on next read
}

type hunkCacheLine struct {
	hunks      []Hunk
	generation uint64
}

// NewCache creates an empty cache.
func NewCache() *Cache {
	return &Cache{entries: make(map[string]*entry)}
}

// Register tells the cache a thread exists. The cache only serves status for
// registered threads — that keeps the dashboard view aligned with what the
// supervisor knows about.
func (c *Cache) Register(wt worktree.Worktree) {
	if wt.ThreadID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[wt.ThreadID]; ok {
		existing.mu.Lock()
		existing.wt = wt
		existing.dirty = true
		existing.mu.Unlock()
		return
	}
	c.entries[wt.ThreadID] = &entry{
		wt:        wt,
		fileHunks: make(map[string]hunkCacheLine),
		dirty:     true,
	}
}

// Forget drops a thread from the cache, called when the thread is removed.
func (c *Cache) Forget(threadID string) {
	c.mu.Lock()
	delete(c.entries, threadID)
	c.mu.Unlock()
}

// Invalidate marks a thread's cached data stale so the next read recomputes.
// Without a filesystem watcher (phase 2) callers force this on every read; the
// method also exists so mutating RPC handlers (commit, land) can short-cut the
// next poll.
func (c *Cache) Invalidate(threadID string) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return
	}
	e.mu.Lock()
	e.dirty = true
	e.mu.Unlock()
}

// InvalidateAll marks every thread stale.
func (c *Cache) InvalidateAll() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.entries {
		e.mu.Lock()
		e.dirty = true
		e.mu.Unlock()
	}
}

// Snapshots returns the current snapshot for every registered thread. Stale
// entries are recomputed under each entry's mutex, so concurrent callers share
// the work.
func (c *Cache) Snapshots() []*Snapshot {
	c.mu.RLock()
	entries := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	c.mu.RUnlock()

	out := make([]*Snapshot, 0, len(entries))
	for _, e := range entries {
		if s := c.ensureSnapshot(e); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// SnapshotFor returns the snapshot for a single thread, recomputing if stale.
func (c *Cache) SnapshotFor(threadID string) (*Snapshot, bool) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return c.ensureSnapshot(e), true
}

// FindByPath locates the thread whose worktree contains the absolute path.
// Returns the snapshot plus the path made relative to that worktree's root.
// Without a hit the caller can fall back to the workspace's own git state.
func (c *Cache) FindByPath(absPath string) (*Snapshot, string, bool) {
	c.mu.RLock()
	entries := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	c.mu.RUnlock()

	best, bestRel, bestLen := (*entry)(nil), "", -1
	for _, e := range entries {
		e.mu.Lock()
		root := e.wt.Path
		e.mu.Unlock()
		rel, ok := relativeTo(root, absPath)
		if !ok {
			continue
		}
		// Prefer the most specific match — an isolated worktree path is always
		// longer than its parent repo root, so this picks the worktree.
		if len(root) > bestLen {
			best, bestRel, bestLen = e, rel, len(root)
		}
	}
	if best == nil {
		return nil, "", false
	}
	return c.ensureSnapshot(best), bestRel, true
}

// HunksFor returns line-level hunks for one file vs the worktree's HEAD,
// computing them on first request and caching against the snapshot generation
// so they invalidate together.
func (c *Cache) HunksFor(threadID, relPath string) ([]Hunk, uint64, bool, error) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return nil, 0, false, nil
	}
	s := c.ensureSnapshot(e)
	if s == nil {
		return nil, 0, true, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if line, ok := e.fileHunks[relPath]; ok && line.generation == e.generation {
		return line.hunks, e.generation, true, nil
	}
	hunks, err := computeFileHunks(e.wt, relPath)
	if err != nil {
		return nil, e.generation, true, err
	}
	e.fileHunks[relPath] = hunkCacheLine{hunks: hunks, generation: e.generation}
	return hunks, e.generation, true, nil
}

// ensureSnapshot returns the entry's snapshot, recomputing if dirty or missing.
// The entry mutex serialises concurrent pollers so they share one git walk.
func (c *Cache) ensureSnapshot(e *entry) *Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshot != nil && !e.dirty {
		return e.snapshot
	}
	snap, err := computeSnapshot(e.wt)
	if err != nil {
		// Surface the error in the snapshot rather than failing the whole
		// dashboard — a single broken worktree should not blank the others.
		snap = &Snapshot{
			ThreadID:  e.wt.ThreadID,
			RepoRoot:  e.wt.RepoRoot,
			Branch:    e.wt.Branch,
			Isolated:  e.wt.Isolated,
			Error:     err.Error(),
			UpdatedAt: time.Now().UTC(),
		}
	}
	e.snapshot = snap
	e.generation++
	e.fileHunks = make(map[string]hunkCacheLine)
	e.dirty = false
	return snap
}
