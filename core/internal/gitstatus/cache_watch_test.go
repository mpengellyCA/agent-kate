// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// newWatchTestCache builds a real fsnotify-backed cache for the lazy-watch
// tests, skipping the test where inotify is unavailable rather than failing.
func newWatchTestCache(t *testing.T) *Cache {
	t.Helper()
	c := NewCache(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = c.Close() })
	if c.watcher == nil {
		t.Skip("fsnotify watcher unavailable on this platform")
	}
	return c
}

// waitWatching polls until the watcher reports the wanted watch state for a
// thread, up to a short deadline. Activate hands the recursive walk to a
// background goroutine, so the watch arms a moment after the call returns.
func waitWatching(t *testing.T, c *Cache, threadID, root string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.watcher.isWatching(threadID, root) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("isWatching(%s) never reached %v before deadline", threadID, want)
}

// TestRegisterIsLazy pins the core of the lazy-watch contract: registering a
// thread (what boot rehydration does for every persisted thread across every
// project) must NOT start an fs watch. Watching begins only on Activate, and
// Forget stops it again.
func TestRegisterIsLazy(t *testing.T) {
	repo, _ := initLinearRepo(t, 2)
	wt := makeWorktree(t, repo, "t-lazy")
	c := newWatchTestCache(t)

	c.Register(wt)
	if c.watcher.isWatching(wt.ThreadID, wt.Path) {
		t.Fatal("Register started a watch; it must stay lazy until Activate")
	}

	c.Activate(wt.ThreadID)
	waitWatching(t, c, wt.ThreadID, wt.Path, true)

	c.Forget(wt.ThreadID)
	if c.watcher.isWatching(wt.ThreadID, wt.Path) {
		t.Fatal("Forget left the watch armed")
	}
}

// TestRosterReadDoesNotWatch guards the dashboard poll path: Snapshots() reads
// every registered thread but must not arm watches, or a single ~1 Hz poll would
// re-create the boot-time watch storm. A per-thread read (SnapshotFor) does arm.
func TestRosterReadDoesNotWatch(t *testing.T) {
	repo, _ := initLinearRepo(t, 2)
	wt := makeWorktree(t, repo, "t-roster")
	c := newWatchTestCache(t)
	c.Register(wt)

	_ = c.Snapshots()
	if c.watcher.isWatching(wt.ThreadID, wt.Path) {
		t.Fatal("Snapshots() armed a watch; the roster path must stay watch-free")
	}

	if _, ok := c.SnapshotFor(wt.ThreadID); !ok {
		t.Fatal("SnapshotFor returned not-ok for a registered thread")
	}
	waitWatching(t, c, wt.ThreadID, wt.Path, true)
}

// TestActivateIdempotentAndRepoints checks Activate is safe to call repeatedly
// (the read path calls it on every poll) and that a root change — what a promote
// does — re-points the watch onto the new tree instead of leaking the old one.
func TestActivateIdempotentAndRepoints(t *testing.T) {
	repo, _ := initLinearRepo(t, 2)
	wt := makeWorktree(t, repo, "t-repoint")
	c := newWatchTestCache(t)
	c.Register(wt)

	c.Activate(wt.ThreadID)
	c.Activate(wt.ThreadID) // repeat call must be a harmless no-op
	waitWatching(t, c, wt.ThreadID, wt.Path, true)

	// Re-register the same thread on a different worktree root (as promote does)
	// and re-activate; the watch must follow to the new root and drop the old.
	wt2 := makeWorktree(t, repo, "t-repoint-iso")
	wt2.ThreadID = wt.ThreadID
	c.Register(wt2)
	c.Activate(wt.ThreadID)
	waitWatching(t, c, wt.ThreadID, wt2.Path, true)
	if c.watcher.isWatching(wt.ThreadID, wt.Path) {
		t.Fatal("watch still points at the stale root after a re-point")
	}
}
