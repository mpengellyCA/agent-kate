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

	wt, err := Create(repo, "t-test")
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
	wt, err := Create(repo, "t-commit")
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

func TestNonGitFallback(t *testing.T) {
	dir := t.TempDir() // a plain directory, not a git repo
	wt, err := Create(dir, "t-x")
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
