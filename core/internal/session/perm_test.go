package session

import (
	"os"
	"path/filepath"
	"testing"
)

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestStoreCreatesPrivateFiles pins the created modes. A thread record carries
// the persona, the env overlay and a title cut from the opening prompt; both
// harness CLIs keep the same data class owner-only and so does this.
func TestStoreCreatesPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentkate")
	path := filepath.Join(dir, "threads.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Record{ThreadID: "t-1", Project: "/tmp/p"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Errorf("threads.json mode = %o, want 600", got)
	}
	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode = %o, want 700", got)
	}

	if err := s.Archive("t-1", "test"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if got := permOf(t, filepath.Join(dir, "threads-archive.json")); got != 0o600 {
		t.Errorf("threads-archive.json mode = %o, want 600", got)
	}
}

// TestStoreMigratesWorldReadableStore is the case every existing installation
// is in: the files already exist, created 0644 in a 0755 directory by an
// earlier build. New-file modes alone would never touch them.
func TestStoreMigratesWorldReadableStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentkate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "threads.json")
	archive := filepath.Join(dir, "threads-archive.json")
	if err := os.WriteFile(path, []byte(`{"threads":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte(`{"threads":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode = %o after open, want 700", got)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Errorf("threads.json mode = %o after open, want 600", got)
	}
	if got := permOf(t, archive); got != 0o600 {
		t.Errorf("threads-archive.json mode = %o after open, want 600", got)
	}
}

// TestAttachmentStorePrivate covers the sidecars: created 0600 in a 0700
// directory, and an existing world-readable directory migrated at open —
// sidecars name every file the human ever attached, with its full path.
func TestAttachmentStorePrivate(t *testing.T) {
	root := t.TempDir()

	fresh := filepath.Join(root, "fresh")
	s := NewAttachmentStore(fresh)
	if err := s.Append("t-1", AttachmentTurn{
		Text:        "look at this",
		Attachments: []AttachmentMeta{{Name: "secret.pdf", Kind: "text", Path: "/home/u/secret.pdf"}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := permOf(t, fresh); got != 0o700 {
		t.Errorf("attachment dir mode = %o, want 700", got)
	}
	if got := permOf(t, filepath.Join(fresh, "t-1.json")); got != 0o600 {
		t.Errorf("sidecar mode = %o, want 600", got)
	}

	// Migration: a directory of 0644 sidecars from an earlier build.
	old := filepath.Join(root, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(old, "t-2.json")
	if err := os.WriteFile(sidecar, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	NewAttachmentStore(old)
	if got := permOf(t, old); got != 0o700 {
		t.Errorf("old attachment dir mode = %o after open, want 700", got)
	}
	if got := permOf(t, sidecar); got != 0o600 {
		t.Errorf("old sidecar mode = %o after open, want 600", got)
	}
}
