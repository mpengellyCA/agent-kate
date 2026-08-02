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
	run(t, repo, "git", "config", "user.name", "Agent Kate Test")
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

// TestDiffWorkspaceIncludesNewFiles is audit F41: a workspace-mode agent that
// only CREATES files used to produce an empty diff (`git diff HEAD` is
// tracked-only), and the UI then told the user the agent "has not changed
// anything yet". The diff must show the new file, must not have staged it into
// the human's index, and must leave ignored files alone.
func TestDiffWorkspaceIncludesNewFiles(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-ws-diff", ModeWorkspace)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, ".gitignore"),
		[]byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", ".gitignore")
	run(t, repo, "git", "commit", "-q", "-m", "ignore")

	// The agent creates a brand-new file and touches nothing tracked.
	if err := os.WriteFile(filepath.Join(repo, "brand-new.txt"),
		[]byte("the work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"),
		[]byte("build artefact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := Diff(wt)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "brand-new.txt") || !strings.Contains(diff, "the work") {
		t.Fatalf("workspace diff omitted the new file:\n%s", diff)
	}
	if strings.Contains(diff, "ignored.txt") {
		t.Fatalf("workspace diff included an ignored file:\n%s", diff)
	}

	// The human's index must be untouched — nothing staged behind their back.
	staged, err := exec.Command("git", "-C", repo, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if strings.TrimSpace(string(staged)) != "" {
		t.Fatalf("Diff staged files in the user's index: %q", staged)
	}
}

// TestDiffWorkspaceTrackedAndUntracked pins that the tracked hunks are still
// there once untracked files are appended — the fix must add, never replace.
func TestDiffWorkspaceTrackedAndUntracked(t *testing.T) {
	repo := initRepo(t)
	wt, _ := Create(repo, "t-ws-both", ModeWorkspace)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := Diff(wt)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "a.txt") || !strings.Contains(diff, "b.txt") {
		t.Fatalf("diff missing tracked or untracked change:\n%s", diff)
	}
}

// TestDiffWorkspaceNoCommits covers the fresh project (audit F49's repo shape):
// with no HEAD to diff against, every file the agent wrote is untracked, and
// returning "" there is the same false "nothing changed" F41 is about.
func TestDiffWorkspaceNoCommits(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	wt, err := Create(repo, "t-fresh", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.Isolated {
		t.Fatal("setup: auto on a commitless repo should fall back to the workspace")
	}
	if err := os.WriteFile(filepath.Join(repo, "first.txt"),
		[]byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := Diff(wt)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "first.txt") {
		t.Fatalf("commitless-repo diff omitted the agent's only file:\n%s", diff)
	}
}

// TestDiffWorkspaceSkipsManagedWorktrees: another thread's isolated worktree
// lives under .agentkate/worktrees inside this repo. Its files are not this
// thread's work and must never appear in the workspace agent's diff.
func TestDiffWorkspaceSkipsManagedWorktrees(t *testing.T) {
	repo := initRepo(t)
	ws, _ := Create(repo, "t-ws-mixed", ModeWorkspace)
	iso, err := Create(repo, "t-other", ModeIsolated)
	if err != nil {
		t.Fatalf("Create isolated: %v", err)
	}
	if err := os.WriteFile(filepath.Join(iso.Path, "other-agent.txt"),
		[]byte("someone else's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := Diff(ws)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if strings.Contains(diff, "other-agent.txt") {
		t.Fatalf("workspace diff leaked another thread's worktree:\n%s", diff)
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

func TestLand(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-land", ModeIsolated)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The agent commits work inside its own worktree.
	if err := os.WriteFile(filepath.Join(wt.Path, "feature.txt"),
		[]byte("agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Commit(wt, "add feature"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	target, err := Land(wt)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if target == "" {
		t.Fatal("Land returned no target branch")
	}
	// The committed work is now in the main working tree.
	if b, _ := os.ReadFile(filepath.Join(repo, "feature.txt")); string(b) != "agent work\n" {
		t.Fatalf("feature.txt was not landed into the main tree: %q", b)
	}
}

// TestLandWithOptionsKeepConflicts forces a conflicting merge and asserts the
// workspace is left MERGING with the conflicting path reported.
func TestLandWithOptionsKeepConflicts(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-conflict", ModeIsolated)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Workspace and worktree both change the same line of a.txt.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"),
		[]byte("workspace change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "a.txt")
	run(t, repo, "git", "commit", "-q", "-m", "workspace edit")
	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"),
		[]byte("agent change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Commit(wt, "agent edit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := LandWithOptions(wt, true)
	if err != nil {
		t.Fatalf("LandWithOptions: %v", err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "a.txt" {
		t.Fatalf("expected conflict on a.txt, got %v", res.Conflicts)
	}
	if status := WorkspaceMergeStatus(repo); !status.Merging {
		t.Fatal("workspace should be MERGING after a conflict-keeping land")
	}

	// AbortMerge restores the workspace to pre-merge state.
	if err := AbortMerge(repo); err != nil {
		t.Fatalf("AbortMerge: %v", err)
	}
	if status := WorkspaceMergeStatus(repo); status.Merging {
		t.Fatal("workspace should be clean after AbortMerge")
	}
}

// TestLandWithOptionsAbortDefault verifies the safe default: a conflict rolls
// the workspace back and returns an error.
func TestLandWithOptionsAbortDefault(t *testing.T) {
	repo := initRepo(t)
	wt, _ := Create(repo, "t-rollback", ModeIsolated)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"),
		[]byte("workspace change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "a.txt")
	run(t, repo, "git", "commit", "-q", "-m", "workspace edit")
	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"),
		[]byte("agent change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Commit(wt, "agent edit"); err != nil {
		t.Fatal(err)
	}

	if _, err := LandWithOptions(wt, false); err == nil {
		t.Fatal("default Land should error on conflict")
	}
	if status := WorkspaceMergeStatus(repo); status.Merging {
		t.Fatal("workspace should be clean after a default-mode conflict abort")
	}
}

// TestFinalizeMerge resolves a conflict by hand, then commits the merge.
func TestFinalizeMerge(t *testing.T) {
	repo := initRepo(t)
	wt, _ := Create(repo, "t-finalize", ModeIsolated)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"),
		[]byte("workspace change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "a.txt")
	run(t, repo, "git", "commit", "-q", "-m", "workspace edit")
	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"),
		[]byte("agent change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Commit(wt, "agent edit"); err != nil {
		t.Fatal(err)
	}
	if _, err := LandWithOptions(wt, true); err != nil {
		t.Fatalf("LandWithOptions: %v", err)
	}

	// FinalizeMerge must refuse while conflict markers remain.
	if err := FinalizeMerge(repo); err == nil {
		t.Fatal("FinalizeMerge should refuse while conflicts remain")
	}

	// Resolve by overwriting the file and re-staging.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"),
		[]byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "a.txt")
	if err := FinalizeMerge(repo); err != nil {
		t.Fatalf("FinalizeMerge after resolution: %v", err)
	}
	if status := WorkspaceMergeStatus(repo); status.Merging {
		t.Fatal("workspace should no longer be MERGING after FinalizeMerge")
	}
}

func TestLandNoCommits(t *testing.T) {
	repo := initRepo(t)
	wt, _ := Create(repo, "t-empty", ModeIsolated)
	if _, err := Land(wt); err == nil {
		t.Fatal("Land should fail when the agent has no commits")
	}
}

func TestLandNonIsolated(t *testing.T) {
	repo := initRepo(t)
	wt, _ := Create(repo, "t-ws", ModeWorkspace)
	if _, err := Land(wt); err == nil {
		t.Fatal("Land should fail for a non-isolated thread")
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
