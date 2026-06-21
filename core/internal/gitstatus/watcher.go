// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"agentkate/internal/safe"

	"github.com/fsnotify/fsnotify"
)

// invalidateQueueSize bounds the channel that decouples the inotify read loop
// from invalidation dispatch. A slow or stuck downstream consumer (the UI
// Notify write) must not back-pressure the inotify reader, or every agent's
// git-status freshness freezes at once. When the queue is full we coalesce —
// the next snapshot read recomputes regardless, so a dropped enqueue only
// delays the clean→dirty notification, never correctness.
const invalidateQueueSize = 1024

// watcher fans one inotify instance out to many cache entries: it watches every
// directory under each registered worktree root and flips the owning entry's
// dirty flag when anything inside changes. There is no per-event work beyond
// the flip — the next snapshot read does the recompute, so multiple events
// between reads coalesce naturally.
//
// The inotify read loop is deliberately decoupled from invalidation dispatch:
// the loop only does cheap bookkeeping (resolve owner, add watches for new
// dirs) and hands the thread id to a separate consumer goroutine over a
// buffered channel. That consumer calls Cache.Invalidate, which may fan out to
// a slow UI client; keeping it off the read loop means inotify keeps draining
// even when a downstream Notify is slow or wedged.
type watcher struct {
	fs  *fsnotify.Watcher
	log *slog.Logger
	mu  sync.Mutex
	// dirOwner maps an absolute directory path to the thread whose entry
	// should be invalidated when an event fires inside it.
	dirOwner map[string]string

	// invalidate carries thread ids from the inotify read loop to the
	// dispatch consumer. Buffered so a slow consumer doesn't stall the reader.
	invalidate chan string

	// pendMu guards pending and closed. pending is the set of thread ids
	// already queued on invalidate; it coalesces rapid duplicate events for the
	// same thread so a burst of writes inside one worktree doesn't flood the
	// channel. closed is set once Close has shut the invalidate channel so no
	// enqueue ever sends on a closed channel.
	pendMu  sync.Mutex
	pending map[string]struct{}
	closed  bool
}

func newWatcher(log *slog.Logger) (*watcher, error) {
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &watcher{
		fs:         fs,
		log:        log,
		dirOwner:   make(map[string]string),
		invalidate: make(chan string, invalidateQueueSize),
		pending:    make(map[string]struct{}),
	}, nil
}

func (w *watcher) Close() error {
	if w == nil || w.fs == nil {
		return nil
	}
	// Close the invalidate channel first so the dispatch consumer drains and
	// exits. Guard with closed under pendMu so any in-flight enqueueInvalidate
	// stops sending instead of panicking on a closed channel. Closing the
	// fsnotify watcher then stops the read loop.
	w.pendMu.Lock()
	if !w.closed {
		w.closed = true
		close(w.invalidate)
	}
	w.pendMu.Unlock()
	return w.fs.Close()
}

// Watch starts watching every directory under root for the given thread.
// Directories in the skip set (.git, node_modules, dotfile dirs, …) are not
// descended into; the rest are added one at a time because inotify is not
// recursive on Linux.
//
// .git itself is skipped by the general walk because its objects/ tree is
// huge and noisy, but commits / branch updates / index changes happen *inside*
// .git — so we add a curated set of watches in there too (HEAD, index, refs,
// packed-refs, MERGE_HEAD, …). Without this the dashboard stayed stale after
// any commit that didn't go through the daemon's own git.* RPCs.
func (w *watcher) Watch(threadID, root string) {
	w.addDirRecursive(threadID, root, true)
	w.addGitMetaWatches(threadID, root)
}

// Unwatch removes every watch that was added under this thread's root.
func (w *watcher) Unwatch(threadID string) {
	w.mu.Lock()
	var paths []string
	for dir, owner := range w.dirOwner {
		if owner == threadID {
			paths = append(paths, dir)
		}
	}
	for _, p := range paths {
		delete(w.dirOwner, p)
	}
	w.mu.Unlock()
	for _, p := range paths {
		_ = w.fs.Remove(p)
	}
}

// addGitMetaWatches adds inotify watches inside .git so HEAD / index / refs
// changes invalidate the cache. Skips objects/, logs/, lfs/ which are large
// and write-noisy without affecting visible status. Handles both real .git
// directories and the gitfile pointer used by linked worktrees.
func (w *watcher) addGitMetaWatches(threadID, worktreeRoot string) {
	gitDir := resolveGitDir(worktreeRoot)
	if gitDir == "" {
		return
	}
	skipTop := map[string]bool{"objects": true, "logs": true, "lfs": true}
	_ = filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != gitDir {
			rel, _ := filepath.Rel(gitDir, path)
			first, _, _ := strings.Cut(rel, string(filepath.Separator))
			if skipTop[first] {
				return filepath.SkipDir
			}
		}
		w.mu.Lock()
		w.dirOwner[path] = threadID
		w.mu.Unlock()
		if err := w.fs.Add(path); err != nil {
			w.log.Debug("git meta watch add failed", "path", path, "err", err)
		}
		return nil
	})
}

// resolveGitDir returns the real .git directory for a worktree root. For a
// linked worktree, .git is a file containing "gitdir: <path>" pointing at the
// per-worktree subdir under the main repo's .git.
func resolveGitDir(worktreeRoot string) string {
	gitPath := filepath.Join(worktreeRoot, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return gitPath
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	actual := strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(actual) {
		actual = filepath.Join(worktreeRoot, actual)
	}
	return actual
}

func (w *watcher) addDirRecursive(threadID, root string, isRoot bool) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable dir — skip, don't blow up the whole walk
		}
		if !d.IsDir() {
			return nil
		}
		// Skip the usual large/derived/hidden directories at every level
		// except the worktree root itself (which can validly be e.g. a
		// dot-prefixed name if the user opens one).
		if !(isRoot && path == root) && shouldSkipDir(d.Name()) {
			return filepath.SkipDir
		}
		w.mu.Lock()
		w.dirOwner[path] = threadID
		w.mu.Unlock()
		if err := w.fs.Add(path); err != nil {
			// Likely the inotify limit, or the path was removed between walk
			// and add. Log and move on; missing one watch only delays the
			// next dirty flip until a sibling event fires.
			w.log.Debug("fs watch add failed", "path", path, "err", err)
		}
		return nil
	})
}

// run reads inotify events and dispatches invalidations. It is the entry point
// the cache launches; it owns the inotify read loop and spins up a separate
// dispatch consumer so the read loop never blocks on a slow downstream Notify.
//
// The whole loop is wrapped so a panic logs and the read loop restarts rather
// than silently dying — losing the watcher would degrade git-status freshness
// for every agent with no visible signal. Restart re-enters the same select on
// the still-live fsnotify channels; on Close those channels close and the loop
// returns for good.
func (w *watcher) run(c *Cache) {
	// The consumer is the only thing that calls Cache.Invalidate (and thus may
	// touch a slow UI client). Run it on its own safe.Go goroutine so a panic
	// there is contained and never reaches the read loop.
	safe.Go("gitstatus.watcher.dispatch", func() { w.dispatch(c) })

	for {
		closed := w.readLoop()
		if closed {
			return
		}
		// readLoop returned via a recovered panic rather than channel close;
		// re-enter so a single bad event doesn't kill watcher freshness.
		w.log.Warn("fs watcher read loop recovered; restarting")
	}
}

// readLoop drains the inotify event/error channels. It returns true when a
// channel closed (Close was called) — the only clean stop — and false if it
// unwound via a recovered panic, signalling run to restart it. The recover is
// local so a panic in handleEvent's bookkeeping doesn't tear down the watcher.
func (w *watcher) readLoop() (closed bool) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("fs watcher read loop panic recovered",
				"panic", r)
			closed = false
		}
	}()
	for {
		select {
		case e, ok := <-w.fs.Events:
			if !ok {
				return true
			}
			w.handleEvent(e)
		case err, ok := <-w.fs.Errors:
			if !ok {
				return true
			}
			w.log.Warn("fs watcher error", "err", err)
		}
	}
}

// dispatch is the invalidation consumer. It drains thread ids enqueued by the
// read loop and calls Cache.Invalidate off the inotify path, so a slow Notify
// never back-pressures inotify draining. Returns when the invalidate channel is
// closed at Close.
func (w *watcher) dispatch(c *Cache) {
	for threadID := range w.invalidate {
		w.invalidateOne(c, threadID)
	}
}

// invalidateOne processes a single queued invalidation under a recover, so a
// panic in one Cache.Invalidate (or a downstream callback) can't kill the
// dispatch consumer and silently freeze git-status freshness for every thread —
// matching the read loop's restart-on-panic resilience.
func (w *watcher) invalidateOne(c *Cache, threadID string) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("fs watcher dispatch panic recovered",
				"thread", threadID, "panic", r)
		}
	}()
	w.pendMu.Lock()
	delete(w.pending, threadID)
	w.pendMu.Unlock()
	c.Invalidate(threadID)
}

// handleEvent runs on the inotify read loop. It does only cheap, non-blocking
// bookkeeping — resolve the owning thread, queue an invalidation, and add a
// watch for any newly created directory — and never calls Cache.Invalidate
// directly so a slow downstream consumer can't stall inotify draining.
func (w *watcher) handleEvent(e fsnotify.Event) {
	// Resolve the owning thread. The event's path is the file or dir that
	// changed; its parent directory is the one we have a watch on, so look
	// up by parent first. A freshly created directory is itself the path
	// we just watched, so try that too as a fallback.
	dir := filepath.Dir(e.Name)
	w.mu.Lock()
	owner := w.dirOwner[dir]
	if owner == "" {
		owner = w.dirOwner[e.Name]
	}
	w.mu.Unlock()
	if owner == "" {
		return
	}
	w.enqueueInvalidate(owner)

	// A new directory needs its own watch added; otherwise files created
	// inside it won't generate any further events.
	if e.Has(fsnotify.Create) {
		if info, err := os.Stat(e.Name); err == nil && info.IsDir() {
			if !shouldSkipDir(filepath.Base(e.Name)) {
				w.addDirRecursive(owner, e.Name, false)
			}
		}
	}

	// A removed or renamed-away directory leaves a stale dirOwner entry that
	// would otherwise live until the whole thread is unwatched. Drop the entry
	// (and any descendants that were watched beneath it) so the map can't grow
	// unboundedly across a long session. inotify auto-drops the watch on the
	// gone inode, so the explicit Remove is best-effort cleanup.
	if e.Has(fsnotify.Remove) || e.Has(fsnotify.Rename) {
		w.evictDir(e.Name)
	}
}

// evictDir removes the dirOwner entries for path and anything watched beneath
// it, releasing the fsnotify watches best-effort. Called when a watched
// directory is deleted or renamed away so the owner map and watch set don't
// accumulate dead directories over the life of a thread.
func (w *watcher) evictDir(path string) {
	prefix := path + string(filepath.Separator)
	w.mu.Lock()
	var gone []string
	for dir := range w.dirOwner {
		if dir == path || strings.HasPrefix(dir, prefix) {
			gone = append(gone, dir)
		}
	}
	for _, dir := range gone {
		delete(w.dirOwner, dir)
	}
	w.mu.Unlock()
	for _, dir := range gone {
		_ = w.fs.Remove(dir)
	}
}

// enqueueInvalidate hands a thread id to the dispatch consumer, coalescing
// rapid duplicates: if an invalidation for this thread is already queued and
// not yet consumed, the event is dropped (the queued one covers it). The send
// is non-blocking — if the buffered channel is full the enqueue is dropped
// entirely, which is safe because the next snapshot read recomputes regardless;
// only the clean→dirty notification is delayed until the next event lands.
func (w *watcher) enqueueInvalidate(threadID string) {
	w.pendMu.Lock()
	defer w.pendMu.Unlock()
	if w.closed {
		return
	}
	if _, already := w.pending[threadID]; already {
		return
	}
	w.pending[threadID] = struct{}{}

	// Non-blocking send: the buffered channel means this never blocks under
	// pendMu in the common case. If it is full the enqueue is dropped (the
	// pending mark is undone) and the next read recomputes regardless — only
	// the clean→dirty notification is delayed until the next event lands.
	select {
	case w.invalidate <- threadID:
	default:
		delete(w.pending, threadID)
		w.log.Debug("fs watcher invalidate queue full; dropping", "threadID", threadID)
	}
}

// shouldSkipDir matches directories that are noisy (build outputs), large
// (vendored deps), or conventionally outside the user's editable working set
// (dotfile config dirs). The .agentkate skip avoids double-watching: each
// worktree under .agentkate/worktrees/<id>/ already has its own registration.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".venv", "venv", "__pycache__",
		".cache", ".next", "dist", "build", "target", "vendor",
		".agentkate":
		return true
	}
	// Hidden directories (.idea, .vscode, …) — typically excluded by
	// .gitignore-ish convention and rarely interesting for status.
	return strings.HasPrefix(name, ".")
}
