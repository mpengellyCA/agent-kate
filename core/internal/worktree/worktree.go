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
	"syscall"
)

// Worktree describes where an agent thread runs and how to diff its changes.
type Worktree struct {
	ThreadID string `json:"threadId"`
	RepoRoot string `json:"repoRoot"` // the workspace git repo
	Path     string `json:"path"`     // directory the agent runs in
	Branch   string `json:"branch"`   // git branch; empty when not isolated
	Base     string `json:"base"`     // base commit; empty when not isolated
	Isolated bool   `json:"isolated"` // true = real worktree, false = direct in workspace
	// Number is a project-scoped, monotonically assigned agent ID surfaced
	// in the UI as "#3" so users have a short handle for each agent's
	// worktree. Zero until the session layer stamps it.
	Number int `json:"number,omitempty"`
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
	return CommitPaths(wt, message, nil)
}

// CommitPaths stages a chosen subset of paths (or everything when paths is
// empty) and commits the result. Paths are worktree-relative; passing a nil or
// empty slice is the same as Commit's "stage everything" behaviour.
func CommitPaths(wt Worktree, message string, paths []string) error {
	if strings.TrimSpace(message) == "" {
		message = "Agent Kate change"
	}
	if len(paths) == 0 {
		if _, err := git(wt.Path, "add", "-A"); err != nil {
			return err
		}
	} else {
		// `git add --` accepts any number of paths; the explicit `--` keeps
		// pathnames beginning with a dash from being read as options.
		args := append([]string{"add", "--"}, paths...)
		if _, err := git(wt.Path, args...); err != nil {
			return err
		}
	}
	// `git diff --cached --quiet` exits 0 (no error) when nothing is staged.
	if _, err := git(wt.Path, "diff", "--cached", "--quiet"); err == nil {
		return fmt.Errorf("nothing to commit")
	}
	_, err := git(wt.Path, "commit", "-m", message)
	return err
}

// LandResult describes the outcome of a Land. When Conflicts is non-empty the
// workspace is left in MERGING state (only possible when keepConflicts was
// asked for); the caller is responsible for finishing via FinalizeMerge or
// AbortMerge.
type LandResult struct {
	Branch    string   `json:"branch"`    // the agent's branch
	Into      string   `json:"into"`      // the workspace branch we merged into
	Conflicts []string `json:"conflicts"` // empty on a clean merge
}

// Land merges a thread's worktree branch into the repository's main working
// tree, aborting on conflict. Preserved as the "safe" shim around
// LandWithOptions so existing callers (and existing tests) keep their
// always-rollback behaviour.
func Land(wt Worktree) (string, error) {
	r, err := LandWithOptions(wt, false)
	return r.Into, err
}

// LandWithOptions merges the thread's branch into the workspace's current
// branch. Behaviour on conflict depends on keepConflicts:
//
//   - false (the default): the merge is aborted, the workspace is left
//     untouched, and a conflict counts as an error.
//   - true: the merge is left in MERGING state with conflict markers in the
//     working tree, the unmerged paths are returned in Conflicts, and the
//     caller follows up with FinalizeMerge or AbortMerge.
//
// The thread must be isolated with at least one commit, and the workspace
// must have no uncommitted tracked changes in either case.
func LandWithOptions(wt Worktree, keepConflicts bool) (LandResult, error) {
	res := LandResult{Branch: wt.Branch}
	if !wt.Isolated || wt.Branch == "" {
		return res, fmt.Errorf("this agent is not running on its own branch")
	}
	ahead, err := git(wt.RepoRoot, "rev-list", "--count", wt.Base+".."+wt.Branch)
	if err != nil {
		return res, err
	}
	if strings.TrimSpace(ahead) == "0" {
		return res, fmt.Errorf("this agent has no commits to land — commit its changes first")
	}
	status, err := git(wt.RepoRoot, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return res, err
	}
	if strings.TrimSpace(status) != "" {
		return res, fmt.Errorf(
			"the workspace has uncommitted changes — commit or stash them first")
	}
	target, err := git(wt.RepoRoot, "branch", "--show-current")
	if err != nil {
		return res, err
	}
	res.Into = strings.TrimSpace(target)
	if res.Into == "" {
		return res, fmt.Errorf("the workspace is not on a branch")
	}
	out, mergeErr := git(wt.RepoRoot, "merge", "--no-edit", wt.Branch)
	if mergeErr == nil {
		return res, nil
	}
	// Merge failed. Distinguish "conflict, MERGING state ready" from
	// "outright failure" by looking for unmerged paths.
	conflicts := unmergedPaths(wt.RepoRoot)
	if len(conflicts) == 0 || !keepConflicts {
		_, _ = git(wt.RepoRoot, "merge", "--abort")
		if len(conflicts) > 0 {
			return res, fmt.Errorf(
				"merge produced conflicts and was aborted (the workspace was left untouched)")
		}
		return res, fmt.Errorf(
			"merge failed (the workspace was left untouched): %s",
			strings.TrimSpace(out))
	}
	res.Conflicts = conflicts
	return res, nil
}

// (LandResult.Conflicts is similarly emitted as [] not null when clean: it is
// initialised to nil only on the conflict path, and the caller checks
// len(Conflicts) which treats both the same way.)

// unmergedPaths returns repo-relative paths with unresolved conflict markers.
// `git diff --name-only --diff-filter=U` is the canonical query.
func unmergedPaths(repoRoot string) []string {
	out, err := git(repoRoot, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var paths []string
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// AbortMerge rolls back an in-progress merge in repoRoot, restoring the index
// and working tree to pre-merge state. Safe to call when no merge is active.
func AbortMerge(repoRoot string) error {
	if !mergeInProgress(repoRoot) {
		return nil
	}
	_, err := git(repoRoot, "merge", "--abort")
	return err
}

// FinalizeMerge commits the in-progress merge in repoRoot using git's default
// merge message. Fails descriptively when there are still unresolved
// conflicts.
func FinalizeMerge(repoRoot string) error {
	if !mergeInProgress(repoRoot) {
		return fmt.Errorf("no merge is in progress")
	}
	if conflicts := unmergedPaths(repoRoot); len(conflicts) > 0 {
		return fmt.Errorf("%d file(s) still have unresolved conflicts", len(conflicts))
	}
	_, err := git(repoRoot, "commit", "--no-edit")
	return err
}

// OpenConflictTool runs `git mergetool --tool=kdiff3 -y` from repoRoot in the
// background. KDiff3 opens its own window for each unmerged file; -y skips
// the per-file "was the merge successful?" terminal prompt git would otherwise
// emit. Returns once the process has been spawned — the caller does not wait.
func OpenConflictTool(repoRoot string) error {
	if !mergeInProgress(repoRoot) {
		return fmt.Errorf("no merge is in progress")
	}
	if _, err := exec.LookPath("kdiff3"); err != nil {
		return fmt.Errorf("kdiff3 is not installed (looked for it on PATH)")
	}
	cmd := exec.Command("git", "mergetool", "--tool=kdiff3", "-y")
	cmd.Dir = repoRoot
	// Detach: KDiff3 is a GUI, we do not want to inherit its IO and we do
	// not want it killed if akcore exits. Setsid puts it in a new session.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// MergeStatus describes whether a workspace is mid-merge and what is left.
// Used to keep the conflict banner accurate while the human works in KDiff3.
type MergeStatus struct {
	Merging   bool     `json:"merging"`
	Conflicts []string `json:"conflicts"`
}

// WorkspaceMergeStatus reads the merge state of repoRoot: whether a merge is
// in progress and which paths still have conflict markers. Used by the UI's
// conflict banner to know when to dismiss itself.
func WorkspaceMergeStatus(repoRoot string) MergeStatus {
	conflicts := unmergedPaths(repoRoot)
	if conflicts == nil {
		conflicts = []string{} // marshal as [] rather than null
	}
	return MergeStatus{
		Merging:   mergeInProgress(repoRoot),
		Conflicts: conflicts,
	}
}

// mergeInProgress returns true when MERGE_HEAD exists in the repo's gitdir —
// the canonical "we are mid-merge" marker git itself checks.
func mergeInProgress(repoRoot string) bool {
	out, err := git(repoRoot, "rev-parse", "--git-dir")
	if err != nil {
		return false
	}
	gitDir := strings.TrimSpace(out)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoRoot, gitDir)
	}
	_, err = os.Stat(filepath.Join(gitDir, "MERGE_HEAD"))
	return err == nil
}

// PROptions controls what OpenPRWithOptions sends to `gh pr create`. Title is
// the only field for which an empty value is meaningfully different from the
// caller's intent — a blank Body is fine.
type PROptions struct {
	Title string
	Body  string
	Draft bool
}

// OpenPR pushes the thread's branch and opens a GitHub pull request through the
// gh CLI, returning the PR URL. It fails descriptively when the thread is not
// isolated, gh is missing, or there is no 'origin' remote.
//
// Kept as a back-compatible shim around OpenPRWithOptions; new callers should
// prefer the options struct so title and body can be filled independently.
func OpenPR(wt Worktree, title string) (string, error) {
	return OpenPRWithOptions(wt, PROptions{Title: title})
}

// OpenPRWithOptions is the full PR-creation surface. Falls through to a
// generated default title and body when the caller leaves them blank, so the
// UI's "Open PR" can short-circuit straight from a button without showing the
// dialog when the user wants the quick path.
func OpenPRWithOptions(wt Worktree, opts PROptions) (string, error) {
	if !wt.Isolated {
		return "", fmt.Errorf("this agent is not running on its own branch")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("the GitHub CLI (gh) is not installed")
	}
	if _, err := git(wt.RepoRoot, "remote", "get-url", "origin"); err != nil {
		return "", fmt.Errorf("no 'origin' git remote is configured")
	}
	title := strings.TrimSpace(opts.Title)
	body := opts.Body
	if title == "" || body == "" {
		// Fall back to a draft for whatever the caller didn't supply.
		fbTitle, fbBody, _ := PRDraft(wt)
		if title == "" {
			title = fbTitle
		}
		if body == "" {
			body = fbBody
		}
	}
	if title == "" {
		title = "Agent Kate: " + wt.Branch
	}

	if _, err := git(wt.Path, "push", "-u", "origin", wt.Branch); err != nil {
		return "", fmt.Errorf("push failed: %w", err)
	}
	args := []string{"pr", "create", "--title", title, "--body", body,
		"--head", wt.Branch}
	if opts.Draft {
		args = append(args, "--draft")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = wt.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// PRDraft suggests a title and body for the thread's branch, drawn from its
// commit history since the fork point and a short diff stat. Pure read; no
// network. Used to prefill the PR dialog.
func PRDraft(wt Worktree) (title, body string, err error) {
	if !wt.Isolated || wt.Branch == "" {
		return "", "", fmt.Errorf("this agent is not running on its own branch")
	}
	if wt.Base == "" {
		return "", "", fmt.Errorf("this agent has no recorded base commit")
	}

	// Title — the most recent commit's subject. A one-commit branch's
	// subject IS the change; multi-commit branches tend to have the
	// summary line in the most recent commit, which is what we want.
	out, _ := git(wt.RepoRoot, "log", "-1", "--pretty=format:%s", wt.Branch)
	title = strings.TrimSpace(out)
	if title == "" {
		title = "Agent Kate: " + wt.Branch
	}

	// Body — bullet list of commit subjects (oldest first, the natural
	// reading order for a PR description) plus a compact diff stat.
	var sb strings.Builder
	commitLog, _ := git(wt.RepoRoot, "log",
		"--reverse", "--pretty=format:- %s (%h)",
		wt.Base+".."+wt.Branch)
	if strings.TrimSpace(commitLog) != "" {
		sb.WriteString("## Commits\n\n")
		sb.WriteString(strings.TrimRight(commitLog, "\n"))
		sb.WriteString("\n\n")
	}
	stat, _ := git(wt.RepoRoot, "diff", "--shortstat", wt.Base+".."+wt.Branch)
	if s := strings.TrimSpace(stat); s != "" {
		sb.WriteString("## Diff\n\n")
		sb.WriteString(s)
		sb.WriteString("\n\n")
	}
	sb.WriteString("---\nOpened by Agent Kate from ")
	sb.WriteString(wt.Branch)
	sb.WriteString(".\n")
	body = sb.String()
	return title, body, nil
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
