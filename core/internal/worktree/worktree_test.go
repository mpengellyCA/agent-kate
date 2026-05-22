package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// initRepo creates a git repo with one commit and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	run(t, repo, "git", "config", "user.email", "test@agentkate")
	run(t, repo, "git", "config", "user.name", "AgentKate Test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-q", "-m", "init")
	return repo
}

func TestCreateDiffRemove(t *testing.T) {
	repo := initRepo(t)

	wt, err := Create(repo, "t-test", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !wt.Isolated {
		t.Fatal("expected an isolated worktree for a committed repo")
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}

	// Modify a tracked file and add a brand-new file inside the worktree.
	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "b.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := Diff(wt)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "a.txt") || !strings.Contains(diff, "b.txt") {
		t.Fatalf("diff missing expected changes:\n%s", diff)
	}

	if err := Remove(wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatal("worktree dir should be gone after Remove")
	}
}

func TestCommit(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-commit", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Nothing changed yet.
	if err := Commit(wt, "empty"); err == nil {
		t.Fatal("Commit with no changes should report nothing to commit")
	}

	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Commit(wt, "agent change"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The commit landed on the thread's branch.
	out, err := exec.Command("git", "-C", wt.Path, "log", "-1", "--pretty=%s").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "agent change" {
		t.Fatalf("last commit subject = %q, want %q", got, "agent change")
	}
}

func TestCreateModes(t *testing.T) {
	repo := initRepo(t)

	// ModeWorkspace never isolates, even in a committed repo.
	direct, err := Create(repo, "t-ws", ModeWorkspace)
	if err != nil {
		t.Fatalf("Create workspace: %v", err)
	}
	if direct.Isolated || direct.Path != repo {
		t.Fatalf("workspace mode = %+v, want non-isolated at the repo root", direct)
	}

	// ModeIsolated gives a committed repo a real worktree.
	iso, err := Create(repo, "t-iso", ModeIsolated)
	if err != nil {
		t.Fatalf("Create isolated: %v", err)
	}
	if !iso.Isolated {
		t.Fatal("isolated mode should produce an isolated worktree")
	}

	// ModeIsolated on a repo with no commits is a hard error.
	empty := t.TempDir()
	run(t, empty, "git", "init", "-q")
	if _, err := Create(empty, "t-bad", ModeIsolated); err == nil {
		t.Fatal("isolated mode should fail when the repo has no commits")
	}

	// ModeAuto on that same uncommitted repo falls back quietly.
	auto, err := Create(empty, "t-auto", ModeAuto)
	if err != nil {
		t.Fatalf("Create auto on an uncommitted repo: %v", err)
	}
	if auto.Isolated {
		t.Fatal("auto mode should fall back to non-isolated with no commits")
	}
}

func TestPromote(t *testing.T) {
	repo := initRepo(t) // commits a.txt = "hello\n"

	wt, err := Create(repo, "t-prom", ModeWorkspace)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.Isolated {
		t.Fatal("setup: expected a non-isolated worktree")
	}

	// In-progress work in the main tree: a modified file and a brand-new one.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"),
		[]byte("promoted change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"),
		[]byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	iso, err := Promote(wt)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !iso.Isolated || iso.Branch != "agentkate/t-prom" {
		t.Fatalf("promoted worktree = %+v", iso)
	}

	// The work moved into the worktree.
	if b, _ := os.ReadFile(filepath.Join(iso.Path, "a.txt")); string(b) != "promoted change\n" {
		t.Fatalf("worktree a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(iso.Path, "new.txt")); string(b) != "brand new\n" {
		t.Fatalf("worktree new.txt = %q", b)
	}
	// ...and the main tree is clean again.
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "hello\n" {
		t.Fatalf("main tree a.txt should be restored, got %q", b)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("new.txt should have moved out of the main tree")
	}
}

func TestPromoteAlreadyIsolated(t *testing.T) {
	repo := initRepo(t)
	iso, err := Create(repo, "t-iso", ModeIsolated)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := Promote(iso)
	if err != nil {
		t.Fatalf("Promote of an isolated worktree should be a no-op: %v", err)
	}
	if got.Path != iso.Path {
		t.Fatalf("Promote changed an already-isolated worktree: %+v", got)
	}
}

func TestNonGitFallback(t *testing.T) {
	dir := t.TempDir() // a plain directory, not a git repo
	wt, err := Create(dir, "t-x", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.Isolated {
		t.Fatal("expected a non-isolated fallback for a non-git directory")
	}
	if wt.Path != dir {
		t.Fatalf("non-isolated path = %q, want %q", wt.Path, dir)
	}
}
