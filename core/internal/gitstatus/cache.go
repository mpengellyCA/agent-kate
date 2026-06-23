// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

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

	"agentkate/internal/safe"
	"agentkate/internal/worktree"

	"github.com/go-git/go-git/v5/plumbing/object"
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

	// settleWindow is how long a thread's onInvalidate notification is deferred
	// after the last fs event, so a burst of agent saves collapses into one
	// notification once activity quiets. Configurable for tests; defaults to
	// defaultSettleWindow.
	settleWindow time.Duration
}

// defaultSettleWindow is the debounce window before an fs-event invalidation
// fires its onInvalidate notification. Long enough to swallow a multi-file save
// burst, short enough that a real change still surfaces promptly. Mutating RPC
// handlers (commit, land) notify immediately and bypass this window.
const defaultSettleWindow = 250 * time.Millisecond

// entry is the cached state for one worktree. Its own mutex serialises the
// "is this dirty? recompute" check so multiple pollers share one git walk.
type entry struct {
	mu sync.Mutex

	wt worktree.Worktree

	snapshot   *Snapshot
	fileHunks  map[string]hunkCacheLine
	fileBlame  map[string]blameCacheLine
	generation uint64
	dirty      bool // recompute on next read

	// logWalk caches the full HEAD-graph walk for the log viewer, keyed on the
	// HEAD it was walked from. Resolving refs and re-walking history is the most
	// expensive read path and was previously redone on every page request
	// (quadratic with scroll depth); caching the walk + refs lets paging slice a
	// precomputed array. Only the unfiltered HEAD view is cached here — path /
	// branch-scoped requests still walk directly (they vary per call). Busted on
	// HEAD change.
	logWalk *logWalkCache

	// notifyPending marks that an fs event has landed since the last
	// notification settled and a deferred notify is owed. It is deliberately
	// SEPARATE from dirty: the 1 Hz poll clears dirty in ensureSnapshot mid-burst,
	// and reusing dirty as the notify edge would re-arm a fresh notification on
	// the very next event. notifyPending instead tracks the notification edge
	// independently and is cleared only when the settle window fires.
	notifyPending bool
	// settleTimer fires settleWindow after the last fs event for this thread; it
	// is reset on each new event so the notify is deferred until writes quiet.
	settleTimer *time.Timer
	// sentSnapshot is the last snapshot whose change we actually notified on. The
	// settle pass diffs the freshly recomputed snapshot against this (ignoring the
	// volatile UpdatedAt) and only fires onInvalidate when a meaningful field
	// differs, so a save that leaves git status unchanged stays silent.
	sentSnapshot *Snapshot
}

type hunkCacheLine struct {
	hunks      []Hunk
	generation uint64
}

type blameCacheLine struct {
	lines      []BlameLine
	generation uint64
}

// logWalkCache holds one full HEAD-graph walk so the log viewer's pages slice a
// precomputed array instead of re-walking history per request. headSHA pins the
// walk to the HEAD it was taken from; a mismatch (HEAD moved) forces a re-walk.
type logWalkCache struct {
	headSHA string
	walked  []*object.Commit
	refs    map[string][]string
}

// NewCache creates an empty cache. If log is non-nil it also spins up an
// fsnotify-backed watcher; on platforms or systems where that fails the
// watcher is silently skipped and callers fall back to Invalidate-driven
// freshness.
func NewCache(log *slog.Logger) *Cache {
	c := &Cache{
		entries:      make(map[string]*entry),
		log:          log,
		settleWindow: defaultSettleWindow,
	}
	if log != nil {
		if w, err := newWatcher(log); err == nil {
			c.watcher = w
			safe.Go("gitstatus.watcher.run", func() { w.run(c) })
		} else {
			log.Warn("git fs watcher unavailable; per-snapshot recompute will be used",
				"err", err)
		}
	}
	return c
}

// OnInvalidate registers a callback fired after a thread's invalidation settles
// AND its recomputed snapshot differs meaningfully from the last one we sent.
// This is the bus push hook: the core wires it to send a git.invalidated
// notification so the UI can short-cut its next poll. The settle window
// (debounce) collapses a save burst into one callback, and the snapshot diff
// suppresses callbacks for events that didn't actually change visible git
// status — together they stop the redundant-notification fire-hose that drove
// dashboard flicker.
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
//
// Registration deliberately does NOT start the fs watch. At boot every persisted
// thread across every project is registered so the dashboard can show dormant
// threads and per-thread RPCs resolve; watching them all would walk and inotify
// every worktree of every project the user has ever opened, including ones not
// open now. The watch is started lazily by Activate when a thread actually
// becomes active. Status for an unwatched thread is still correct — it is
// computed on demand on the next read (ensureSnapshot recomputes a nil/dirty
// snapshot); the watch only adds proactive clean→dirty push for a thread in use.
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
		fileBlame: make(map[string]blameCacheLine),
		dirty:     true,
	}
}

// Activate starts watching a registered thread's worktree for live updates.
// Registration alone (boot rehydration) does not watch — a dormant thread in a
// project the user never reopens must not cost a recursive inotify walk every
// boot. Watching begins only when a thread becomes active: an agent starts or
// resumes on it, or the user opens its git detail (diff / log / blame).
//
// Idempotent and safe to call from any read path. The first call for a thread
// hands the recursive walk to a background goroutine so a first git.diff does
// not block on it; later calls short-circuit on isWatching without spawning
// anything, so the hot poll path stays free.
func (c *Cache) Activate(threadID string) {
	if threadID == "" || c.watcher == nil {
		return
	}
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return
	}
	e.mu.Lock()
	root := e.wt.Path
	e.mu.Unlock()
	if root == "" || c.watcher.isWatching(threadID, root) {
		return
	}
	safe.Go("gitstatus.cache.activate", func() { c.watcher.Watch(threadID, root) })
}

// Forget drops a thread from the cache, called when the thread is removed.
func (c *Cache) Forget(threadID string) {
	c.mu.Lock()
	e, ok := c.entries[threadID]
	delete(c.entries, threadID)
	c.mu.Unlock()
	if ok {
		// Stop any armed settle timer so a pending notification for a removed
		// thread can't fire after Forget. A settle that already fired is harmless:
		// it no longer finds the entry in the map and bails.
		e.mu.Lock()
		if e.settleTimer != nil {
			e.settleTimer.Stop()
			e.settleTimer = nil
		}
		e.notifyPending = false
		e.mu.Unlock()
	}
	if c.watcher != nil {
		c.watcher.Unwatch(threadID)
	}
}

// Invalidate marks a thread's cached data stale so the next read recomputes,
// then arms a debounced notification. Called by the fs watcher on each event and
// by mutating RPC handlers (commit, land) so the next poll sees the new state
// without waiting for inotify.
//
// The dirty flag flips immediately (correctness — the next read always sees
// fresh state). The onInvalidate notification, however, is deferred behind a
// per-thread settle timer that resets on each event: a burst of agent saves
// collapses into ONE settle pass once writes quiet. That pass recomputes the
// snapshot and diffs it against the last SENT one, firing onInvalidate only when
// a meaningful field actually changed — so the fire-hose of redundant
// notifications that drove UI flicker is gone, while a real change still
// surfaces within settleWindow.
func (c *Cache) Invalidate(threadID string) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return
	}
	c.armSettle(e)
}

// InvalidateAll marks every thread stale and arms each one's debounced notify.
func (c *Cache) InvalidateAll() {
	c.mu.RLock()
	entries := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	c.mu.RUnlock()
	for _, e := range entries {
		c.armSettle(e)
	}
}

// armSettle flips the entry dirty and (re)arms its per-thread settle timer. The
// notifyPending edge is tracked separately from dirty: the 1 Hz poll clears
// dirty in ensureSnapshot, so reusing dirty here would let a mid-burst poll
// re-arm a fresh notification on the next event. notifyPending is set here and
// cleared only when the settle pass runs, so a burst yields exactly one pass.
func (c *Cache) armSettle(e *entry) {
	e.mu.Lock()
	e.dirty = true
	e.notifyPending = true
	if e.settleTimer != nil {
		// Reset (or restart) the existing timer so the window slides forward to
		// the latest event. Stop's return is ignored: if it already fired, its
		// settle pass found notifyPending still set (or will be re-set here) and
		// a fresh timer below covers the new activity, so no notification is lost.
		e.settleTimer.Stop()
	}
	threadID := e.wt.ThreadID
	e.settleTimer = time.AfterFunc(c.settleWindow, func() {
		safe.Go("gitstatus.settle", func() { c.settle(threadID) })
	})
	e.mu.Unlock()
}

// settle runs once per quiet window after a burst of invalidations. It clears
// the pending edge, recomputes the snapshot (sharing the work via
// ensureSnapshot), and fires onInvalidate only when the new snapshot differs
// meaningfully from the last one we sent — so an event that left git status
// unchanged stays silent, and the final state of a burst is never dropped.
func (c *Cache) settle(threadID string) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	cb := c.onInvalidate
	c.mu.RUnlock()
	if !ok {
		return
	}

	e.mu.Lock()
	if !e.notifyPending {
		// A concurrent settle (or a Forget) already consumed this edge; nothing
		// owed. Bail before recomputing so we don't notify twice for one burst.
		e.mu.Unlock()
		return
	}
	e.notifyPending = false
	e.mu.Unlock()

	// Recompute outside the lock-and-edge dance; ensureSnapshot takes its own
	// entry lock and clears dirty. This is the burst's single git walk.
	snap := c.ensureSnapshot(e)
	if snap == nil {
		return
	}

	e.mu.Lock()
	changed := !snapshotsEqual(e.sentSnapshot, snap)
	if changed {
		e.sentSnapshot = snap
	}
	e.mu.Unlock()

	if changed && cb != nil {
		cb(threadID)
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
	c.Activate(threadID) // a per-thread read means this thread is in use — watch it
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

	best, bestRel, bestLen, bestThread := (*entry)(nil), "", -1, ""
	for _, e := range entries {
		e.mu.Lock()
		root := e.wt.Path
		thread := e.wt.ThreadID
		e.mu.Unlock()
		rel, ok := relativeTo(root, absPath)
		if !ok {
			continue
		}
		// Prefer the most specific match — an isolated worktree path is always
		// longer than its parent repo root, so this picks the worktree.
		if len(root) > bestLen {
			best, bestRel, bestLen, bestThread = e, rel, len(root), thread
		}
	}
	if best == nil {
		return nil, "", false
	}
	c.Activate(bestThread) // the file lives in this thread's worktree — watch it
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
	c.Activate(threadID) // gutter/diff for this thread is open — watch it
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

// BlameFor returns per-line blame for one file vs the worktree's HEAD,
// computing it on first request and caching against the snapshot generation so
// it invalidates together — the same scheme HunksFor uses. The bool reports
// whether the thread is known; an unknown thread yields (nil, false, nil) so
// the caller can fall back. git blame is one of the two heaviest read paths and
// was previously shelled out on every request with no caching at all.
func (c *Cache) BlameFor(threadID, relPath string) ([]BlameLine, bool, error) {
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	c.Activate(threadID) // blame for this thread is open — watch it
	s := c.ensureSnapshot(e)
	if s == nil {
		return nil, true, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if line, ok := e.fileBlame[relPath]; ok && line.generation == e.generation {
		return line.lines, true, nil
	}
	lines, err := Blame(e.wt, relPath)
	if err != nil {
		return nil, true, err
	}
	e.fileBlame[relPath] = blameCacheLine{lines: lines, generation: e.generation}
	return lines, true, nil
}

// LogPageFor returns one page of the thread's HEAD-graph log, caching the full
// history walk + ref map per (thread, HEAD) so paging slices a precomputed
// array instead of re-walking from the top on every request. The bool reports
// whether the thread is known and the request is cacheable; it is false (with
// no error) for an unknown thread OR for a path / branch-scoped request, both
// of which the caller should serve via the bare gitstatus.Log.
//
// Cache results are byte-for-byte identical to the uncached path: the cached
// walk is the same iterator order, and the per-page topo-sort + lane layout are
// computed over exactly the page's commits, just as Log always did.
func (c *Cache) LogPageFor(threadID string, opts LogOptions) ([]LogEntry, bool, error) {
	// Only the unfiltered HEAD view is cacheable: a Branch starts the walk from
	// a different commit (not tracked by HEAD) and a Path changes which commits
	// the walk yields, so neither is keyed by HEAD. Defer both to the caller.
	if opts.Branch != "" || opts.Path != "" {
		return nil, false, nil
	}
	c.mu.RLock()
	e, ok := c.entries[threadID]
	c.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	c.Activate(threadID) // the log viewer is open on this thread — watch it
	s := c.ensureSnapshot(e)
	if s == nil {
		return nil, true, nil
	}
	head := s.HeadSHA

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logWalk == nil || e.logWalk.headSHA != head {
		// The full walk ignores Skip/Limit, so it is taken once per HEAD and
		// every page reuses it. walkLog returns a nil slice for an empty /
		// unborn repo; cache that too so we don't re-walk a repo with no
		// commits on each page.
		walked, refs, err := walkLog(e.wt, LogOptions{})
		if err != nil {
			return nil, true, err
		}
		e.logWalk = &logWalkCache{headSHA: head, walked: walked, refs: refs}
	}
	if e.logWalk.walked == nil {
		return nil, true, nil
	}
	return pageLog(e.logWalk.walked, e.logWalk.refs, opts), true, nil
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
	e.fileBlame = make(map[string]blameCacheLine)
	e.dirty = false
	// logWalk is intentionally NOT cleared here: it is keyed on the HEAD it was
	// walked from and self-invalidates on a HEAD change (see LogPageFor), so it
	// survives the generation bumps that ordinary working-tree saves cause.
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

// snapshotsEqual reports whether two snapshots are equal for the purpose of the
// settle-pass diff: every field the UI renders matches, ignoring the volatile
// UpdatedAt (which changes on every recompute and would defeat the diff). A nil
// previous snapshot is never equal — the first settle after registration always
// notifies. This is the gate that stops a save which left git status unchanged
// (e.g. touching a file then reverting it within one window) from firing a
// redundant git.invalidated.
func snapshotsEqual(a, b *Snapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ThreadID != b.ThreadID ||
		a.Number != b.Number ||
		a.RepoRoot != b.RepoRoot ||
		a.Path != b.Path ||
		a.Branch != b.Branch ||
		a.Isolated != b.Isolated ||
		a.HeadSHA != b.HeadSHA ||
		a.Base != b.Base ||
		a.Ahead != b.Ahead ||
		a.BehindBase != b.BehindBase ||
		a.DirtyCount != b.DirtyCount ||
		a.HasConflicts != b.HasConflicts ||
		a.HasUpstream != b.HasUpstream ||
		a.RemoteAhead != b.RemoteAhead ||
		a.RemoteBehind != b.RemoteBehind ||
		a.Error != b.Error {
		return false
	}
	if len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}
