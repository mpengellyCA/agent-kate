package fsperm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mode is a small helper: the permission bits of path.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestMkdirAllCreatesPrivate: a directory this package creates is owner-only,
// whatever the process umask happens to be.
func TestMkdirAllCreatesPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store", "nested")
	if err := MkdirAll(dir); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("new dir mode = %o, want 700", got)
	}
}

// TestMkdirAllTightensExisting is the migration half: MkdirAll's mode argument
// applies only to directories it creates, so a store directory an earlier build
// left at 0755 must be chmod'ed explicitly or it stays world-readable forever.
func TestMkdirAllTightensExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAll(dir); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("existing dir mode = %o, want 700 (migration did not run)", got)
	}
}

// TestWriteFileCreatesPrivate pins the file half.
func TestWriteFileCreatesPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	if err := WriteFile(path, []byte("{}")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("new file mode = %o, want 600", got)
	}
}

// TestWriteFileTightensExisting covers the staging-file case: os.WriteFile
// applies its mode only on creation, so a `.tmp` left world-readable by an
// older build's crash would otherwise carry 0644 onto the store file it is
// renamed over.
func TestWriteFileTightensExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json.tmp")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("{}")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("rewritten file mode = %o, want 600", got)
	}
}

// TestHardenTreeMigratesWholeStore: the shape a real upgrade meets — a 0755
// directory of 0644 files.
func TestHardenTreeMigratesWholeStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "summaries")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(root, "t-1.json"),
		filepath.Join(root, "sub", "t-2.json"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n, err := HardenTree(root)
	if err != nil {
		t.Fatalf("HardenTree: %v", err)
	}
	if n != 4 { // root, sub, and two files
		t.Errorf("tightened %d paths, want 4", n)
	}
	if got := mode(t, root); got != 0o700 {
		t.Errorf("root mode = %o, want 700", got)
	}
	if got := mode(t, filepath.Join(root, "sub")); got != 0o700 {
		t.Errorf("subdir mode = %o, want 700", got)
	}
	for _, f := range files {
		if got := mode(t, f); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", f, got)
		}
	}

	// Idempotent: a second pass tightens nothing, so a normal startup on an
	// already-private store logs nothing.
	n, err = HardenTree(root)
	if err != nil {
		t.Fatalf("HardenTree (second pass): %v", err)
	}
	if n != 0 {
		t.Errorf("second pass tightened %d paths, want 0", n)
	}
}

// TestHardenTreeMissingDirIsNotAnError: a store that has never been written to
// has nothing to migrate.
func TestHardenTreeMissingDirIsNotAnError(t *testing.T) {
	n, err := HardenTree(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("HardenTree on a missing dir: %v", err)
	}
	if n != 0 {
		t.Errorf("tightened %d paths, want 0", n)
	}
}

// TestHardenFileRefusesSymlink is the fail-closed case: os.Chmod follows
// symlinks, so a link planted in a store directory would otherwise let the
// migration change the mode of a file the store does not own. Refuse, loudly,
// rather than chmod through it.
func TestHardenFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "threads.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	_, err := HardenFile(link)
	if err == nil {
		t.Fatal("HardenFile followed a symlink instead of refusing")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if got := mode(t, victim); got != 0o644 {
		t.Errorf("victim mode changed to %o — the chmod went through the link", got)
	}
}

// TestHardenFileMissingIsNotAnError: nothing to migrate is not a failure.
func TestHardenFileMissingIsNotAnError(t *testing.T) {
	changed, err := HardenFile(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("HardenFile on a missing file: %v", err)
	}
	if changed {
		t.Error("reported a change on a missing file")
	}
}

// TestHardenDirFollowsSymlink: relocating an XDG data directory by symlinking
// it is a legitimate layout. Refusing it would fail every store open — and so
// every daemon start — on such a machine, which is why HardenDir follows a link
// to a directory where HardenFile refuses one to a file.
func TestHardenDirFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "agentkate")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	changed, err := HardenDir(link)
	if err != nil {
		t.Fatalf("HardenDir on a symlinked store dir: %v", err)
	}
	if !changed {
		t.Error("reported no change on a 0755 directory")
	}
	if got := mode(t, real); got != 0o700 {
		t.Errorf("target dir mode = %o, want 700", got)
	}
}

// TestHardenTreeFollowsSymlinkedRoot: same layout, whole-tree migration.
// WalkDir lstats its root, so without resolving it first the walk would report
// one non-directory entry and skip every file in the store.
func TestHardenTreeFollowsSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(real, "t-1.json")
	if err := os.WriteFile(file, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "summaries")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := HardenTree(link); err != nil {
		t.Fatalf("HardenTree on a symlinked root: %v", err)
	}
	if got := mode(t, file); got != 0o600 {
		t.Errorf("file under a symlinked root = %o, want 600 (the walk skipped it)", got)
	}
}

// TestHardenDirRefusesNonDirectory: something else sitting where a store
// directory belongs is refused, not chmod'ed blind.
func TestHardenDirRefusesNonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentkate")
	if err := os.WriteFile(path, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := HardenDir(path); err == nil {
		t.Fatal("HardenDir accepted a regular file as a store directory")
	}
}
