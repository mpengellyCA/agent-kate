// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentkate/internal/worktree"
)

// makeWorktree creates a real isolated worktree under repo on branch
// agentkate/<id> from the current HEAD, returning the worktree descriptor. It
// mirrors what worktree.Create does but stays inside the test package's git
// fixtures so the cleanup queries (is-ancestor, stash list, rev-list) run for
// real.
func makeWorktree(t *testing.T, repo, id string) worktree.Worktree {
	t.Helper()
	base := testGit(t, repo, "rev-parse", "HEAD")
	dir := filepath.Join(repo, ".agentkate", "worktrees", id)
	branch := "agentkate/" + id
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "worktree", "add", "-b", branch, dir, base)
	return worktree.Worktree{
		ThreadID: id,
		RepoRoot: repo,
		Path:     dir,
		Branch:   branch,
		Base:     base,
		Isolated: true,
		Number:   1,
	}
}

// snapFor computes a real snapshot for wt, the same way the cache would.
func snapFor(t *testing.T, wt worktree.Worktree) *Snapshot {
	t.Helper()
	s, err := computeSnapshot(wt)
	if err != nil {
		t.Fatalf("computeSnapshot: %v", err)
	}
	return s
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// --- IsMergedInto ----------------------------------------------------------

func TestIsMergedInto(t *testing.T) {
	repo, shas := initLinearRepo(t, 3)

	// HEAD of main is an ancestor of itself → merged.
	merged, err := IsMergedInto(repo, shas[2], "main")
	if err != nil {
		t.Fatal(err)
	}
	if !merged {
		t.Fatal("main tip should be ancestor of main")
	}

	// An older commit is also an ancestor of the tip.
	merged, err = IsMergedInto(repo, shas[0], "main")
	if err != nil || !merged {
		t.Fatalf("older commit should be merged: merged=%v err=%v", merged, err)
	}

	// A branch commit NOT on main is not an ancestor.
	wt := makeWorktree(t, repo, "feat")
	fn := filepath.Join(wt.Path, "feat.txt")
	if err := os.WriteFile(fn, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, wt.Path, "add", "feat.txt")
	testGit(t, wt.Path, "commit", "-q", "-m", "feat work")
	head := testGit(t, wt.Path, "rev-parse", "HEAD")

	merged, err = IsMergedInto(repo, head, "main")
	if err != nil {
		t.Fatal(err)
	}
	if merged {
		t.Fatal("unmerged branch commit should not be ancestor of main")
	}

	// After merging into main, the SAME branch tip is an ancestor even though
	// it remains "ahead" of its fork point — proving we must use is-ancestor,
	// not Ahead, to decide "merged".
	testGit(t, repo, "merge", "--no-edit", wt.Branch)
	merged, err = IsMergedInto(repo, head, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !merged {
		t.Fatal("branch tip should be ancestor of main after merge")
	}
}

func TestIsMergedIntoEmptyArgs(t *testing.T) {
	if m, _ := IsMergedInto("", "abc", "main"); m {
		t.Fatal("empty repoRoot should not report merged")
	}
	if m, _ := IsMergedInto("/tmp", "", "main"); m {
		t.Fatal("empty sha should not report merged")
	}
}

// --- StashCount ------------------------------------------------------------

func TestStashCount(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	if n := StashCount(repo); n != 0 {
		t.Fatalf("fresh repo stash count = %d, want 0", n)
	}
	// Create a change and stash it.
	fn := filepath.Join(repo, "f.txt")
	if err := os.WriteFile(fn, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "stash", "push", "-m", "one")
	if n := StashCount(repo); n != 1 {
		t.Fatalf("stash count = %d, want 1", n)
	}
	if n := StashCount(""); n != 0 {
		t.Fatalf("empty repoRoot stash count = %d, want 0", n)
	}
}

// --- UnpushedCount ---------------------------------------------------------

func TestUnpushedCountNoUpstream(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "nopush")
	_, hasUpstream := UnpushedCount(wt.RepoRoot, wt.Branch)
	if hasUpstream {
		t.Fatal("branch with no upstream should report hasUpstream=false")
	}
}

// --- AnalyzeCandidate: every permutation -----------------------------------

func TestAnalyzeSafe(t *testing.T) {
	repo, _ := initLinearRepo(t, 2)
	wt := makeWorktree(t, repo, "safe")
	// Branch has no commits past base → tip == base == on main → merged, clean.
	snap := snapFor(t, wt)

	c := AnalyzeCandidate(wt, snap, false, "safe agent", time.Now())
	if c.State != CleanupSafe {
		t.Fatalf("state = %q, want safe (blockers=%v warnings=%v merged=%v)",
			c.State, c.Blockers, c.Warnings, c.Merged)
	}
	if !c.Removable {
		t.Fatal("safe candidate must be removable")
	}
	if len(c.Warnings) != 0 || len(c.Blockers) != 0 {
		t.Fatalf("safe candidate should have no flags: %v %v", c.Blockers, c.Warnings)
	}
	if !c.Merged {
		t.Fatal("a branch at its base must be merged into main")
	}
}

func TestAnalyzeRunningBlocked(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "run")
	snap := snapFor(t, wt)

	c := AnalyzeCandidate(wt, snap, true /*running*/, "", time.Now())
	if c.State != CleanupBlocked || c.Removable {
		t.Fatalf("running must be blocked+unremovable: state=%q removable=%v", c.State, c.Removable)
	}
	if !hasCode(c.Blockers, BlockerRunning) {
		t.Fatalf("missing running blocker: %v", c.Blockers)
	}
}

// A dormant direct-workspace agent owns no worktree, so it is removable as a
// record-only archive (the checkout is untouched) rather than hard-blocked.
func TestAnalyzeNotIsolatedRecordOnly(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := worktree.Worktree{
		ThreadID: "ws", RepoRoot: repo, Path: repo, Isolated: false,
	}
	c := AnalyzeCandidate(wt, snapFor(t, wt), false, "", time.Now())
	if c.State != CleanupRecordOnly || !c.Removable {
		t.Fatalf("non-isolated, not running must be record-only & removable: state=%q removable=%v",
			c.State, c.Removable)
	}
	// No dedicated worktree means none of the worktree-loss blockers apply.
	if hasCode(c.Blockers, BlockerNotIsolated) || hasCode(c.Blockers, BlockerDetached) {
		t.Fatalf("record-only agent must carry no blockers: %v", c.Blockers)
	}
}

func TestAnalyzeDetachedBlocked(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "det")
	wt.Branch = "" // simulate detached / no branch
	snap := snapFor(t, wt)
	// computeSnapshot may resolve a branch name from HEAD; force it back.
	snap.Branch = ""
	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if !hasCode(c.Blockers, BlockerDetached) {
		t.Fatalf("missing detached blocker: %v", c.Blockers)
	}
	if c.State != CleanupBlocked || c.Removable {
		t.Fatalf("detached must be blocked: state=%q", c.State)
	}
}

func TestAnalyzeSnapshotErrorBlocked(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "err")
	snap := snapFor(t, wt)
	snap.Error = "boom" // simulate a failed git read on a live dir
	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if !hasCode(c.Blockers, BlockerSnapshot) {
		t.Fatalf("missing snapshotError blocker: %v", c.Blockers)
	}
	if c.State != CleanupBlocked {
		t.Fatalf("snapshot error on live dir must block: %q", c.State)
	}
}

func TestAnalyzeUnmergedReview(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "unmerged")
	// Commit work on the branch but do NOT merge into main.
	fn := filepath.Join(wt.Path, "w.txt")
	if err := os.WriteFile(fn, []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, wt.Path, "add", "w.txt")
	testGit(t, wt.Path, "commit", "-q", "-m", "unmerged work")
	snap := snapFor(t, wt)

	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if c.Merged {
		t.Fatal("branch with own commit not on main must be unmerged")
	}
	if c.Ahead != 1 {
		t.Fatalf("ahead = %d, want 1", c.Ahead)
	}
	if c.State != CleanupReview || !c.Removable {
		t.Fatalf("unmerged must be review+removable: state=%q removable=%v", c.State, c.Removable)
	}
	if !hasCode(c.Warnings, WarnUnmerged) {
		t.Fatalf("missing unmerged warning: %v", c.Warnings)
	}
	// No upstream + local commits → also unpushed.
	if !hasCode(c.Warnings, WarnUnpushed) {
		t.Fatalf("missing unpushed warning: %v", c.Warnings)
	}
}

func TestAnalyzeMergedButAheadIsSafe(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "mergedahead")
	fn := filepath.Join(wt.Path, "m.txt")
	if err := os.WriteFile(fn, []byte("merge me"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, wt.Path, "add", "m.txt")
	testGit(t, wt.Path, "commit", "-q", "-m", "to be merged")
	// Merge the branch into main, then advance main further.
	testGit(t, repo, "merge", "--no-edit", wt.Branch)
	fn2 := filepath.Join(repo, "after.txt")
	if err := os.WriteFile(fn2, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", "after.txt")
	testGit(t, repo, "commit", "-q", "-m", "advance main")

	snap := snapFor(t, wt)
	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if !c.Merged {
		t.Fatalf("branch merged into main must be Merged even though Ahead=%d", c.Ahead)
	}
	if c.Ahead == 0 {
		t.Fatal("expected branch to remain ahead of its fork point")
	}
	if hasCode(c.Warnings, WarnUnmerged) {
		t.Fatalf("merged branch must not carry an unmerged warning: %v", c.Warnings)
	}
	if c.State != CleanupSafe {
		t.Fatalf("merged+clean must be safe (Ahead notwithstanding): %q warnings=%v", c.State, c.Warnings)
	}
}

func TestAnalyzeDirtyReview(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "dirty")
	// Uncommitted change in the worktree.
	fn := filepath.Join(wt.Path, "f.txt")
	if err := os.WriteFile(fn, []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapFor(t, wt)
	if snap.DirtyCount == 0 {
		t.Fatal("expected a dirty file")
	}
	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if !hasCode(c.Warnings, WarnDirty) {
		t.Fatalf("missing dirty warning: %v", c.Warnings)
	}
	if c.State != CleanupReview || !c.Removable {
		t.Fatalf("dirty must be review+removable: %q removable=%v", c.State, c.Removable)
	}
}

func TestAnalyzeStashReview(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "stash")
	// Create then stash a change inside the worktree.
	fn := filepath.Join(wt.Path, "f.txt")
	if err := os.WriteFile(fn, []byte("to stash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, wt.Path, "stash", "push", "-m", "wip")
	snap := snapFor(t, wt)
	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if c.StashCount == 0 {
		t.Fatalf("expected a stash entry, got %d", c.StashCount)
	}
	if !hasCode(c.Warnings, WarnHasStash) {
		t.Fatalf("missing hasStash warning: %v", c.Warnings)
	}
	if c.State != CleanupReview {
		t.Fatalf("stash present must be review: %q", c.State)
	}
}

func TestAnalyzeOrphaned(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "orphan")
	// Delete the worktree directory out-of-band; the record/branch survive.
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	// Snapshot is irrelevant for an orphan; pass an error snapshot to prove
	// orphaned takes precedence over snapshotError.
	c := AnalyzeCandidate(wt, &Snapshot{Error: "dir gone"}, false, "", time.Now())
	if c.State != CleanupOrphaned {
		t.Fatalf("missing dir must be orphaned: %q (blockers=%v)", c.State, c.Blockers)
	}
	if !c.Removable {
		t.Fatal("orphaned must be removable (prune only)")
	}
	if len(c.Blockers) != 0 {
		t.Fatalf("orphaned must carry no blockers: %v", c.Blockers)
	}
}

func TestAnalyzeRunningOrphanedStaysBlocked(t *testing.T) {
	// A running process pinned to a missing dir is pathological, but the
	// running guard is reached only when the dir exists; once orphaned we
	// short-circuit to orphaned. We assert the orphaned short-circuit wins so
	// the UI can still prune dangling bookkeeping. (Defensive sup.Stop in the
	// handler covers the live-process edge.)
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "runorphan")
	_ = os.RemoveAll(wt.Path)
	c := AnalyzeCandidate(wt, nil, true, "", time.Now())
	if c.State != CleanupOrphaned {
		t.Fatalf("orphaned short-circuit should win: %q", c.State)
	}
}

// A RUNNING direct-workspace agent is still hard-blocked: it is removable only
// once stopped. Running takes precedence over the record-only path.
func TestAnalyzeRunningWorkspaceBlocked(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := worktree.Worktree{ThreadID: "x", RepoRoot: repo, Path: repo, Isolated: false}
	c := AnalyzeCandidate(wt, snapFor(t, wt), true, "", time.Now())
	if c.Removable || c.State != CleanupBlocked {
		t.Fatalf("running workspace agent must be blocked: state=%q removable=%v",
			c.State, c.Removable)
	}
	if !hasCode(c.Blockers, BlockerRunning) {
		t.Fatalf("expected running blocker: %v", c.Blockers)
	}
	// The "not isolated" / "detached" worktree faults no longer apply to a
	// direct-workspace agent — only the genuine "still running" block does.
	if hasCode(c.Blockers, BlockerNotIsolated) || hasCode(c.Blockers, BlockerDetached) {
		t.Fatalf("workspace agent must not carry worktree blockers: %v", c.Blockers)
	}
}

func TestAnalyzeDirtyAndUnmergedBothFlagged(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "both")
	// Committed work (unmerged) + an extra uncommitted edit (dirty).
	fn := filepath.Join(wt.Path, "a.txt")
	if err := os.WriteFile(fn, []byte("committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, wt.Path, "add", "a.txt")
	testGit(t, wt.Path, "commit", "-q", "-m", "committed work")
	fn2 := filepath.Join(wt.Path, "b.txt")
	if err := os.WriteFile(fn2, []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapFor(t, wt)
	c := AnalyzeCandidate(wt, snap, false, "", time.Now())
	if !hasCode(c.Warnings, WarnUnmerged) || !hasCode(c.Warnings, WarnDirty) {
		t.Fatalf("expected both unmerged and dirty warnings: %v", c.Warnings)
	}
	if c.State != CleanupReview {
		t.Fatalf("state = %q, want review", c.State)
	}
}

// --- phase 2 advisory parsing ---------------------------------------------

func TestParseAdvice(t *testing.T) {
	text := "1: REMOVE - merged, nothing unique\n" +
		"2: KEEP - 3 unmerged commits\n" +
		"garbage line\n" +
		"3: REVIEW - ambiguous"
	got := parseAdvice(text)
	if got[1].rec != "REMOVE" || got[1].reason != "merged, nothing unique" {
		t.Fatalf("line 1 parsed wrong: %+v", got[1])
	}
	if got[2].rec != "KEEP" {
		t.Fatalf("line 2 rec = %q", got[2].rec)
	}
	if got[3].rec != "REVIEW" || got[3].reason != "ambiguous" {
		t.Fatalf("line 3 parsed wrong: %+v", got[3])
	}
	if len(got) != 3 {
		t.Fatalf("garbage line should be skipped, got %d entries", len(got))
	}
}

func TestAdviseCleanupEmpty(t *testing.T) {
	// No candidates → no LLM call, returns unchanged.
	if out := AdviseCleanup(nil, "", "", nil); out != nil {
		t.Fatal("nil candidates should pass through unchanged")
	}
}

func TestAdviseCleanupLLMFailureLeavesVerdictIntact(t *testing.T) {
	// A non-existent binary makes the exec fail; the deterministic fields must
	// survive untouched and Recommendation stays empty.
	cands := []CleanupCandidate{{ThreadID: "x", State: CleanupSafe, Removable: true}}
	out := AdviseCleanup(context.Background(), "/nonexistent/claude-bin-xyz", "m", cands)
	if len(out) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out))
	}
	if out[0].State != CleanupSafe || !out[0].Removable {
		t.Fatal("LLM failure must not change the deterministic verdict")
	}
	if out[0].Recommendation != "" {
		t.Fatalf("no recommendation expected on failure, got %q", out[0].Recommendation)
	}
}

// --- provenance (audit F4) -------------------------------------------------
//
// The UI pre-checks "safe" rows and deletes them on one click, so a record
// whose Worktree.Path was repointed at an unrelated directory — threads.json
// is owner-writable, i.e. reachable by a prompt-injected agent — must be
// classified BLOCKED here, never safe. worktree.Remove refuses it either way;
// this keeps the UI's offer honest instead of showing a one-click delete that
// then errors.

func TestAnalyzeUnmanagedPathBlocked(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	victim := t.TempDir() // a directory Agent Kate never created
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := worktree.Worktree{
		ThreadID: "tampered",
		RepoRoot: repo,
		Path:     victim,
		Branch:   "agentkate/tampered",
		Isolated: true,
	}
	c := AnalyzeCandidate(wt, nil, false, "", time.Now())
	if c.Removable || c.State != CleanupBlocked {
		t.Fatalf("tampered record must be blocked: state=%q removable=%v",
			c.State, c.Removable)
	}
	if !hasCode(c.Blockers, BlockerUnmanaged) {
		t.Fatalf("missing %s blocker: %v", BlockerUnmanaged, c.Blockers)
	}
}

// The same holds when the bogus path does not exist: it must NOT be waved
// through as "orphaned" (removable) just because there is nothing there now.
func TestAnalyzeUnmanagedMissingPathIsBlockedNotOrphaned(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := worktree.Worktree{
		ThreadID: "ghost",
		RepoRoot: repo,
		Path:     filepath.Join(t.TempDir(), "not", "here"),
		Branch:   "agentkate/ghost",
		Isolated: true,
	}
	c := AnalyzeCandidate(wt, nil, false, "", time.Now())
	if c.State == CleanupOrphaned || c.Removable {
		t.Fatalf("a foreign missing path must not be removable: state=%q removable=%v",
			c.State, c.Removable)
	}
}

// A genuinely orphaned worktree of ours (contained path, directory deleted out
// of band) stays removable — the gate must not strand real cleanup rows.
func TestAnalyzeOwnOrphanStillRemovable(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	wt := makeWorktree(t, repo, "orphan-ok")
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	c := AnalyzeCandidate(wt, nil, false, "", time.Now())
	if c.State != CleanupOrphaned || !c.Removable {
		t.Fatalf("own orphan must stay removable: state=%q removable=%v blockers=%v",
			c.State, c.Removable, c.Blockers)
	}
}
