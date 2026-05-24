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
	"log/slog"
	"sort"
	"sync"
	"time"

	"agentkate/internal/worktree"
)

// Cache holds per-thread git status, keyed by thread id. Snapshots and per-file
// hunks are recomputed on demand and shared across concurrent readers.
//
// If an fs watcher is attached, entries flip to dirty on any change inside the
// worktree and the next read recomputes; without a watcher (older kernels, no
// inotify) callers must Invalidate explicitly to bust the cache.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*entry

	watcher      *watcher // nil when fsnotify is unavailable
	log          *slog.Logger
	onInvalidate func(threadID string)
	onHeadChange func(threadID, newHeadSHA string)
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

// NewCache creates an empty cache. If log is non-nil it also spins up an
// fsnotify-backed watcher; on platforms or systems where that fails the
// watcher is silently skipped and callers fall back to Invalidate-driven
// freshness.
func NewCache(log *slog.Logger) *Cache {
	c := &Cache{
		entries: make(map[string]*entry),
		log:     log,
	}
	if log != nil {
		if w, err := newWatcher(log); err == nil {
			c.watcher = w
			go w.run(c)
		} else {
			log.Warn("git fs watcher unavailable; per-snapshot recompute will be used",
				"err", err)
		}
	}
	return c
}

// OnInvalidate registers a callback fired whenever an entry's dirty flag flips
// from clean to dirty. This is the bus push hook: the core wires it to send a
// git.invalidated notification so the UI can short-cut its next poll.
func (c *Cache) OnInvalidate(fn func(threadID string)) {
	c.mu.Lock()
	c.onInvalidate = fn
	c.mu.Unlock()
}

// OnHeadChange registers a callback fired whenever a recomputed snapshot's
// HeadSHA differs from the previously cached one. It fires far less often
// than OnInvalidate (fs events on tracked files don't move HEAD), so it's the
// right signal for the log viewer to refetch its first page on.
func (c *Cache) OnHeadChange(fn func(threadID, newHeadSHA string)) {
	c.mu.Lock()
	c.onHeadChange = fn
	c.mu.Unlock()
}

// Close stops the fs watcher. The Cache stays usable; reads just fall back to
// Invalidate-driven freshness.
func (c *Cache) Close() error {
	if c.watcher == nil {
		return nil
	}
	return c.watcher.Close()
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
	if c.watcher != nil {
		c.watcher.Watch(wt.ThreadID, wt.Path)
	}
}

// Forget drops a thread from the cache, called when the thread is removed.
func (c *Cache) Forget(threadID string) {
	c.mu.Lock()
	delete(c.entries, threadID)
	c.mu.Unlock()
	if c.watcher != nil {
		c.watcher.Unwatch(threadID)
	}
}

// Invalidate marks a thread's cached data stale so the next read recomputes.
// Called by the fs watcher on each event and by mutating RPC handlers (commit,
// land) so the next poll sees the new state without waiting for inotify.
func (c *Cache) Invalidate(threadID string) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	cb := c.onInvalidate
	c.mu.RUnlock()
	if !ok {
		return
	}
	e.mu.Lock()
	wasDirty := e.dirty
	e.dirty = true
	e.mu.Unlock()
	if !wasDirty && cb != nil {
		cb(threadID) // only fire on the clean→dirty edge to keep notifications quiet
	}
}

// InvalidateAll marks every thread stale.
func (c *Cache) InvalidateAll() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for id, e := range c.entries {
		e.mu.Lock()
		wasDirty := e.dirty
		e.dirty = true
		e.mu.Unlock()
		if !wasDirty && c.onInvalidate != nil {
			c.onInvalidate(id)
		}
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
	// Go map iteration is randomised, which would make the UI re-order its
	// rows on every poll. Stable sort by thread id pins each row in place.
	sort.Slice(out, func(i, j int) bool { return out[i].ThreadID < out[j].ThreadID })
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
	prevHead := ""
	if e.snapshot != nil {
		prevHead = e.snapshot.HeadSHA
	}
	if e.snapshot != nil && !e.dirty {
		s := e.snapshot
		e.mu.Unlock()
		return s
	}
	snap, err := computeSnapshot(e.wt)
	if err != nil {
		// Surface the error in the snapshot rather than failing the whole
		// dashboard — a single broken worktree should not blank the others.
		snap = &Snapshot{
			ThreadID:  e.wt.ThreadID,
			Number:    e.wt.Number,
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
	threadID := e.wt.ThreadID
	newHead := snap.HeadSHA
	e.mu.Unlock()

	if newHead != "" && newHead != prevHead {
		c.mu.RLock()
		cb := c.onHeadChange
		c.mu.RUnlock()
		if cb != nil {
			cb(threadID, newHead)
		}
	}
	return snap
}
