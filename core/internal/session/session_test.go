package session

import (
	"path/filepath"
	"testing"
	"time"

	"agentkate/internal/worktree"
)

func sampleRecord(id string) Record {
	return Record{
		ThreadID:       id,
		SessionID:      NewID(),
		Project:        "/tmp/proj",
		Worktree:       worktree.Worktree{ThreadID: id, Path: "/tmp/proj/wt/" + id},
		PermissionMode: "acceptEdits",
		Title:          "do a thing",
		Created:        time.Now(),
		Status:         StatusRunning,
	}
}

func TestPutGetList(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Put(sampleRecord("t-a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(sampleRecord("t-b")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := store.Get("t-a"); !ok {
		t.Fatal("Get(t-a) missing")
	}
	if _, ok := store.Get("t-missing"); ok {
		t.Fatal("Get(t-missing) should be absent")
	}
	if got := store.List(""); len(got) != 2 {
		t.Fatalf("List() = %d records, want 2", len(got))
	}
	if got := store.List("/tmp/proj"); len(got) != 2 {
		t.Fatalf("List(project) = %d, want 2", len(got))
	}
	if got := store.List("/other"); len(got) != 0 {
		t.Fatalf("List(/other) = %d, want 0", len(got))
	}
}

func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	rec := sampleRecord("t-keep")
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A second Store over the same file sees the record — and a thread left
	// "running" is reset to "dormant" since no process is live.
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("t-keep")
	if !ok {
		t.Fatal("record did not survive reopen")
	}
	if got.SessionID != rec.SessionID {
		t.Fatalf("sessionId = %q, want %q", got.SessionID, rec.SessionID)
	}
	if got.Status != StatusDormant {
		t.Fatalf("reopened status = %q, want dormant", got.Status)
	}
}

func TestUpdate(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	store.Put(sampleRecord("t-u"))
	if err := store.Update("t-u", func(r *Record) { r.Status = StatusDormant }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := store.Get("t-u"); got.Status != StatusDormant {
		t.Fatalf("status = %q after Update", got.Status)
	}
	// Updating an unknown thread is a harmless no-op.
	if err := store.Update("t-nope", func(r *Record) { r.Status = "x" }); err != nil {
		t.Fatalf("Update(unknown): %v", err)
	}
}

func TestRemove(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	store.Put(sampleRecord("t-r"))
	if err := store.Remove("t-r"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := store.Get("t-r"); ok {
		t.Fatal("record still present after Remove")
	}
}

func TestNewIDFormat(t *testing.T) {
	id := NewID()
	if len(id) != 36 {
		t.Fatalf("id %q length = %d, want 36", id, len(id))
	}
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Fatalf("id %q missing dash at %d", id, pos)
		}
	}
	if id[14] != '4' { // UUID version nibble
		t.Fatalf("id %q is not version 4", id)
	}
	if c := id[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
		t.Fatalf("id %q has bad variant nibble %c", id, c)
	}
	if NewID() == id {
		t.Fatal("NewID returned the same id twice")
	}
}
