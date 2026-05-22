// Package worktree isolates each agent thread in its own git worktree so that
// parallel agents never collide. When the workspace is not a git repository,
// or has no commits yet, it falls back to running the agent directly in the
// workspace — reported via the Isolated field.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree describes where an agent thread runs and how to diff its changes.
type Worktree struct {
	ThreadID string `json:"threadId"`
	RepoRoot string `json:"repoRoot"` // the workspace git repo
	Path     string `json:"path"`     // directory the agent runs in
	Branch   string `json:"branch"`   // git branch; empty when not isolated
	Base     string `json:"base"`     // base commit; empty when not isolated
	Isolated bool   `json:"isolated"` // true = real worktree, false = direct in workspace
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// headCommit returns the HEAD commit of the repo containing dir, and whether
// the repo has any commit at all (a prerequisite for `git worktree add`).
func headCommit(dir string) (string, bool) {
	out, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// Isolation modes for Create.
const (
	ModeAuto      = "auto"      // isolate when the repo has commits, else run direct
	ModeIsolated  = "isolated"  // require isolation; fail if the repo has no commits
	ModeWorkspace = "workspace" // always run directly in the workspace
)

// Create sets up where an agent thread runs, honouring an isolation mode:
//
//   - ModeWorkspace      — always run directly in repoRoot, no isolation.
//   - ModeIsolated       — a dedicated git worktree; an error if repoRoot has
//     no commit to branch from.
//   - ModeAuto (default) — a worktree when repoRoot has a commit, otherwise run
//     directly in repoRoot.
//
// An unrecognised mode (including "") is treated as ModeAuto. An isolated
// worktree lives under <repoRoot>/.agentkate/worktrees/ on its own branch.
func Create(repoRoot, threadID, mode string) (Worktree, error) {
	direct := Worktree{
		ThreadID: threadID,
		RepoRoot: repoRoot,
		Path:     repoRoot,
		Isolated: false,
	}
	if mode == ModeWorkspace {
		return direct, nil
	}

	base, ok := headCommit(repoRoot)
	if !ok {
		// No commit to branch from — isolation is impossible.
		if mode == ModeIsolated {
			return Worktree{}, fmt.Errorf(
				"isolation needs at least one commit in %s", repoRoot)
		}
		return direct, nil // ModeAuto falls back to the workspace
	}

	dir := filepath.Join(repoRoot, ".agentkate", "worktrees", threadID)
	branch := "agentkate/" + threadID

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Worktree{}, err
	}
	if _, err := git(repoRoot, "worktree", "add", "-b", branch, dir, base); err != nil {
		return Worktree{}, err
	}
	return Worktree{
		ThreadID: threadID,
		RepoRoot: repoRoot,
		Path:     dir,
		Branch:   branch,
		Base:     base,
		Isolated: true,
	}, nil
}

// Promote turns a non-isolated worktree into an isolated one without losing
// in-progress work: it stashes the working tree, creates a dedicated worktree
// and branch, then re-applies the stash inside that worktree. The main
// workspace is left clean. repoRoot must have at least one commit.
//
// The caller still has to move the agent — and its Claude Code session — into
// the returned worktree.
func Promote(wt Worktree) (Worktree, error) {
	if wt.Isolated {
		return wt, nil // already isolated — nothing to do
	}
	repoRoot := wt.RepoRoot
	base, ok := headCommit(repoRoot)
	if !ok {
		return Worktree{}, fmt.Errorf("cannot isolate: %s has no commit yet", repoRoot)
	}

	// Stash the working tree (including untracked files) so it can be carried
	// into the new worktree.
	out, err := git(repoRoot, "stash", "push", "--include-untracked",
		"-m", "agentkate-promote-"+wt.ThreadID)
	if err != nil {
		return Worktree{}, fmt.Errorf("stash: %w", err)
	}
	stashed := !strings.Contains(out, "No local changes")

	unstash := func() {
		if stashed {
			_, _ = git(repoRoot, "stash", "pop")
		}
	}

	dir := filepath.Join(repoRoot, ".agentkate", "worktrees", wt.ThreadID)
	branch := "agentkate/" + wt.ThreadID
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		unstash()
		return Worktree{}, err
	}
	if _, err := git(repoRoot, "worktree", "add", "-b", branch, dir, base); err != nil {
		unstash()
		return Worktree{}, err
	}

	isolated := Worktree{
		ThreadID: wt.ThreadID,
		RepoRoot: repoRoot,
		Path:     dir,
		Branch:   branch,
		Base:     base,
		Isolated: true,
	}
	if stashed {
		// Apply the stash inside the new worktree. The worktree is clean and at
		// the same commit the stash was taken from, so this applies cleanly.
		if _, err := git(dir, "stash", "pop"); err != nil {
			return isolated, fmt.Errorf(
				"worktree created on %s, but re-applying the changes hit a "+
					"conflict — the stash was kept: %w", branch, err)
		}
	}
	return isolated, nil
}

// Diff returns a unified diff of everything the thread changed.
//
// For an isolated worktree it stages all changes (harmless — the worktree is
// the agent's own) so that newly created files appear, then diffs against the
// base commit. For the non-isolated fallback it diffs tracked changes against
// HEAD without touching the user's index.
func Diff(wt Worktree) (string, error) {
	if wt.Isolated {
		if _, err := git(wt.Path, "add", "-A"); err != nil {
			return "", err
		}
		return git(wt.Path, "diff", "--cached", wt.Base)
	}
	if _, ok := headCommit(wt.Path); !ok {
		return "", nil
	}
	return git(wt.Path, "diff", "HEAD")
}

// Commit stages every change in the worktree and commits it to the thread's
// branch. It returns a friendly error when there is nothing to commit.
func Commit(wt Worktree, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "AgentKate change"
	}
	if _, err := git(wt.Path, "add", "-A"); err != nil {
		return err
	}
	// `git diff --cached --quiet` exits 0 (no error) when nothing is staged.
	if _, err := git(wt.Path, "diff", "--cached", "--quiet"); err == nil {
		return fmt.Errorf("nothing to commit")
	}
	_, err := git(wt.Path, "commit", "-m", message)
	return err
}

// OpenPR pushes the thread's branch and opens a GitHub pull request through the
// gh CLI, returning the PR URL. It fails descriptively when the thread is not
// isolated, gh is missing, or there is no 'origin' remote.
func OpenPR(wt Worktree, title string) (string, error) {
	if !wt.Isolated {
		return "", fmt.Errorf("this agent is not running on its own branch")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("the GitHub CLI (gh) is not installed")
	}
	if _, err := git(wt.RepoRoot, "remote", "get-url", "origin"); err != nil {
		return "", fmt.Errorf("no 'origin' git remote is configured")
	}
	if strings.TrimSpace(title) == "" {
		title = "AgentKate: " + wt.Branch
	}
	if _, err := git(wt.Path, "push", "-u", "origin", wt.Branch); err != nil {
		return "", fmt.Errorf("push failed: %w", err)
	}
	cmd := exec.Command("gh", "pr", "create",
		"--title", title,
		"--body", "Opened by AgentKate from "+wt.Branch+".",
		"--head", wt.Branch)
	cmd.Dir = wt.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Remove deletes an isolated worktree and its branch. It is a no-op when the
// thread was not isolated.
func Remove(wt Worktree) error {
	if !wt.Isolated {
		return nil
	}
	if _, err := git(wt.RepoRoot, "worktree", "remove", "--force", wt.Path); err != nil {
		_ = os.RemoveAll(wt.Path)
		_, _ = git(wt.RepoRoot, "worktree", "prune")
	}
	_, _ = git(wt.RepoRoot, "branch", "-D", wt.Branch)
	return nil
}
