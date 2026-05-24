// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// watcher fans one inotify instance out to many cache entries: it watches every
// directory under each registered worktree root and flips the owning entry's
// dirty flag when anything inside changes. There is no per-event work beyond
// the flip — the next snapshot read does the recompute, so multiple events
// between reads coalesce naturally.
type watcher struct {
	fs   *fsnotify.Watcher
	log  *slog.Logger
	mu   sync.Mutex
	// dirOwner maps an absolute directory path to the thread whose entry
	// should be invalidated when an event fires inside it.
	dirOwner map[string]string
}

func newWatcher(log *slog.Logger) (*watcher, error) {
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &watcher{
		fs:       fs,
		log:      log,
		dirOwner: make(map[string]string),
	}, nil
}

func (w *watcher) Close() error {
	if w == nil || w.fs == nil {
		return nil
	}
	return w.fs.Close()
}

// Watch starts watching every directory under root for the given thread.
// Directories in the skip set (.git, node_modules, dotfile dirs, …) are not
// descended into; the rest are added one at a time because inotify is not
// recursive on Linux.
func (w *watcher) Watch(threadID, root string) {
	w.addDirRecursive(threadID, root, true)
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

// run dispatches inotify events to cache invalidation. Returns when the
// watcher's event channel closes (i.e. Close has been called).
func (w *watcher) run(c *Cache) {
	for {
		select {
		case e, ok := <-w.fs.Events:
			if !ok {
				return
			}
			w.handleEvent(c, e)
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			w.log.Warn("fs watcher error", "err", err)
		}
	}
}

func (w *watcher) handleEvent(c *Cache, e fsnotify.Event) {
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
	c.Invalidate(owner)

	// A new directory needs its own watch added; otherwise files created
	// inside it won't generate any further events.
	if e.Has(fsnotify.Create) {
		if info, err := os.Stat(e.Name); err == nil && info.IsDir() {
			if !shouldSkipDir(filepath.Base(e.Name)) {
				w.addDirRecursive(owner, e.Name, false)
			}
		}
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
