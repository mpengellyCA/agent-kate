package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// F62: the whole-file archive I/O must run WITHOUT s.mu held, so hot-path
// store callers (the relay's per-turn sessions.Update) never stall behind a
// 4 MB read-modify-write. The archiveIOTestHook parks Archive inside its
// archive-file critical section; every live-store operation must complete
// while it is parked. Inverting the fix (holding s.mu across the archive I/O)
// deadlocks the Update below until the hook releases, and the test fails.
func TestArchiveDoesNotHoldStoreMutexDuringIO(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(sampleRecord("t-archiving")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(sampleRecord("t-live")); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	archiveIOTestHook = func() {
		close(entered)
		<-release
	}
	defer func() { archiveIOTestHook = nil }()

	archived := make(chan error, 1)
	go func() { archived <- store.Archive("t-archiving", "test") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Archive never reached its archive-file write")
	}

	// The archive I/O is parked; the live store must stay fully responsive.
	ops := make(chan error, 1)
	go func() {
		ops <- store.Update("t-live", func(r *Record) { r.Title = "still responsive" })
	}()
	select {
	case err := <-ops:
		if err != nil {
			t.Fatalf("Update during archive I/O: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Update stalled behind in-flight archive I/O — s.mu is held across the file work")
	}
	if _, ok := store.Get("t-live"); !ok {
		t.Fatal("Get failed during archive I/O")
	}
	if got := len(store.List("")); got != 2 {
		t.Fatalf("List during archive I/O = %d records, want 2 (delete happens after the write)", got)
	}

	close(release)
	if err := <-archived; err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, ok := store.Get("t-archiving"); ok {
		t.Fatal("archived record still in live store")
	}
	if arch := store.ListArchived(); len(arch) != 1 || arch[0].ThreadID != "t-archiving" {
		t.Fatalf("archive after release = %+v", arch)
	}
}

// Archive → list → restore round-trips correctly while concurrent readers and
// writers hammer the store (run under -race).
func TestArchiveRestoreRoundTripUnderConcurrency(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := store.Put(sampleRecord(fmt.Sprintf("t-bg-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	rec := sampleRecord("t-round")
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("t-bg-%d", i)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = store.Get(id)
				_ = store.List("")
				_ = store.ListArchived()
				_ = store.Update(id, func(r *Record) { r.Title = "churn" })
			}
		}(i)
	}

	for round := 0; round < 5; round++ {
		if err := store.Archive("t-round", "round trip"); err != nil {
			t.Fatalf("Archive round %d: %v", round, err)
		}
		found := false
		for _, a := range store.ListArchived() {
			if a.ThreadID == "t-round" {
				found = true
				if a.SessionID != rec.SessionID {
					t.Fatalf("round %d: archive lost the record body", round)
				}
			}
		}
		if !found {
			t.Fatalf("round %d: archived record missing from ListArchived", round)
		}
		if err := store.Restore("t-round"); err != nil {
			t.Fatalf("Restore round %d: %v", round, err)
		}
		back, ok := store.Get("t-round")
		if !ok {
			t.Fatalf("round %d: restored record missing from live store", round)
		}
		if back.Status != StatusDormant {
			t.Fatalf("round %d: restored status = %q", round, back.Status)
		}
		for _, a := range store.ListArchived() {
			if a.ThreadID == "t-round" {
				t.Fatalf("round %d: archive entry not consumed by Restore", round)
			}
		}
	}
	close(stop)
	wg.Wait()
}

// ListArchived is served from the in-memory cache once primed — the whole-file
// re-read on every call is what F62 removed. Deleting the file behind the
// store proves the cache answers; Archive and Restore keep it coherent.
func TestListArchivedServedFromCache(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(sampleRecord("t-cache")); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive("t-cache", "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.archivePath()); err != nil {
		t.Fatal(err)
	}
	if arch := store.ListArchived(); len(arch) != 1 || arch[0].ThreadID != "t-cache" {
		t.Fatalf("ListArchived after file removal = %+v; the listing is not cached", arch)
	}
	// Restore invalidates coherently: the entry leaves the cache too.
	if err := store.Restore("t-cache"); err != nil {
		t.Fatal(err)
	}
	if arch := store.ListArchived(); len(arch) != 0 {
		t.Fatalf("cache stale after Restore: %+v", arch)
	}
}
