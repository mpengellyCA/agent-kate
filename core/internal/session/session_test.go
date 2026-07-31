package session

import (
	"os"
	"path/filepath"
	"strings"
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

// TestOrchestrationFieldsRoundTrip guards the plan-16 worker linkage: a
// worker's ParentThreadID and Role must persist in threads.json like every
// other field, and ordinary records must keep omitting them.
func TestOrchestrationFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	rec := sampleRecord("t-worker")
	rec.ParentThreadID = "t-controller"
	rec.Role = RoleWorker
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(sampleRecord("t-plain")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("t-worker")
	if !ok {
		t.Fatal("worker record did not survive reopen")
	}
	if got.ParentThreadID != "t-controller" || got.Role != RoleWorker {
		t.Fatalf("linkage = %q/%q, want t-controller/worker",
			got.ParentThreadID, got.Role)
	}
	// omitempty keeps ordinary records free of orchestration keys on disk.
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "parentThreadId"); n != 1 {
		t.Fatalf("parentThreadId appears %d times on disk, want 1", n)
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

// TestUpdateQuietLeavesUpdated guards the staleness clock: lifecycle bookkeeping
// (dormant-on-exit, running-on-resume, compaction) must persist the mutation
// without advancing Updated, or every launch/shutdown would mask a dormant agent.
func TestUpdateQuietLeavesUpdated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	store.Put(sampleRecord("t-q"))
	before, _ := store.Get("t-q")

	if err := store.UpdateQuiet("t-q", func(r *Record) { r.Status = StatusDormant }); err != nil {
		t.Fatalf("UpdateQuiet: %v", err)
	}
	got, _ := store.Get("t-q")
	if got.Status != StatusDormant {
		t.Fatalf("status = %q after UpdateQuiet, want dormant", got.Status)
	}
	if !got.Updated.Equal(before.Updated) {
		t.Fatalf("Updated changed: %v -> %v", before.Updated, got.Updated)
	}
	// The quiet mutation must still survive a reopen.
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if r, _ := reopened.Get("t-q"); r.Status != StatusDormant {
		t.Fatalf("status = %q after reopen, want dormant", r.Status)
	}

	// Update (non-quiet) still advances the clock, so real activity registers.
	if err := store.Update("t-q", func(r *Record) { r.Title = "worked" }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if r, _ := store.Get("t-q"); !r.Updated.After(before.Updated) {
		t.Fatalf("Update did not advance Updated: %v", r.Updated)
	}
}

func TestTagsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	rec := sampleRecord("t-tags")
	rec.Tags = []string{"backend", "wip"}
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tags survive a flush+reopen unchanged and in order.
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("t-tags")
	if !ok {
		t.Fatal("record did not survive reopen")
	}
	if len(got.Tags) != 2 || got.Tags[0] != "backend" || got.Tags[1] != "wip" {
		t.Fatalf("tags = %v, want [backend wip]", got.Tags)
	}

	// A record with no tags omits the field (omitempty) — no migration churn.
	plain := sampleRecord("t-plain")
	if err := store.Put(plain); err != nil {
		t.Fatalf("Put(plain): %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(b), `"threadId": "t-plain"`) &&
		strings.Contains(snippetAround(string(b), "t-plain"), `"tags"`) {
		t.Fatal("a tagless record should not serialize a tags field")
	}

	// Update can mutate Tags in place.
	if err := store.Update("t-tags", func(r *Record) { r.Tags = []string{"infra"} }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := store.Get("t-tags"); len(got.Tags) != 1 || got.Tags[0] != "infra" {
		t.Fatalf("tags after Update = %v, want [infra]", got.Tags)
	}
}

// snippetAround returns the slice of s within ~120 bytes of the first
// occurrence of marker, so the omitempty check stays local to that record.
func snippetAround(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	end := i + 120
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
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

// --- archive / restore -----------------------------------------------------

func TestArchivePreservesRecordAndDropsFromLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	rec := sampleRecord("t-arch")
	rec.Worktree.Isolated = true
	rec.Worktree.Branch = "agentkate/t-arch"
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.Archive("t-arch", "cleanup: safe"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// Gone from the live store.
	if _, ok := store.Get("t-arch"); ok {
		t.Fatal("archived record must be absent from live store")
	}
	if len(store.List("")) != 0 {
		t.Fatal("live List should be empty after archive")
	}
	// Present, in full, in the archive.
	arch := store.ListArchived()
	if len(arch) != 1 {
		t.Fatalf("ListArchived = %d, want 1", len(arch))
	}
	got := arch[0]
	if got.ThreadID != "t-arch" {
		t.Fatalf("archived threadId = %q", got.ThreadID)
	}
	if got.Reason != "cleanup: safe" {
		t.Fatalf("archived reason = %q", got.Reason)
	}
	if got.SessionID != rec.SessionID {
		t.Fatal("archive must preserve the full record (sessionId lost)")
	}
	if got.Status != StatusArchived {
		t.Fatalf("archived status = %q, want archived", got.Status)
	}
	if got.ArchivedAt.IsZero() {
		t.Fatal("archivedAt must be set")
	}
}

func TestArchiveUnknownThread(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err := store.Archive("nope", "x"); err == nil {
		t.Fatal("Archive of unknown thread must error")
	}
}

func TestArchivePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	_ = store.Put(sampleRecord("t-p"))
	if err := store.Archive("t-p", "cleanup: review"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// Reopen: the archive file is on disk independent of the live store.
	store2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	arch := store2.ListArchived()
	if len(arch) != 1 || arch[0].ThreadID != "t-p" {
		t.Fatalf("archive not persisted across reopen: %+v", arch)
	}
	if _, ok := store2.Get("t-p"); ok {
		t.Fatal("archived thread must not reappear in live store")
	}
}

func TestRestoreMovesBackAsDormantNonIsolated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	rec := sampleRecord("t-r")
	rec.Project = "/tmp/proj"
	rec.Worktree.Isolated = true
	rec.Worktree.Branch = "agentkate/t-r"
	rec.Worktree.Path = "/tmp/proj/.agentkate/worktrees/t-r"
	_ = store.Put(rec)
	if err := store.Archive("t-r", "cleanup: safe"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if err := store.Restore("t-r"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	back, ok := store.Get("t-r")
	if !ok {
		t.Fatal("restored record missing from live store")
	}
	if back.Status != StatusDormant {
		t.Fatalf("restored status = %q, want dormant", back.Status)
	}
	if back.Worktree.Isolated {
		t.Fatal("restored worktree must be non-isolated (its worktree is gone)")
	}
	if back.Worktree.Path != back.Project {
		t.Fatalf("restored path = %q, want project %q", back.Worktree.Path, back.Project)
	}
	// Archive entry consumed.
	if len(store.ListArchived()) != 0 {
		t.Fatal("archive entry must be removed after restore")
	}
}

func TestRestoreUnknown(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err := store.Restore("nope"); err == nil {
		t.Fatal("Restore of unknown thread must error")
	}
}

func TestReArchiveReplacesEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, _ := NewStore(path)
	_ = store.Put(sampleRecord("t-x"))
	_ = store.Archive("t-x", "first")
	_ = store.Restore("t-x")
	_ = store.Archive("t-x", "second")
	arch := store.ListArchived()
	if len(arch) != 1 {
		t.Fatalf("re-archive should not duplicate: got %d", len(arch))
	}
	if arch[0].Reason != "second" {
		t.Fatalf("re-archive reason = %q, want second", arch[0].Reason)
	}
}
