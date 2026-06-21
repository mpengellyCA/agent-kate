// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"os"
	"time"

	"agentkate/internal/worktree"
)

// CleanupState is the verdict for one worktree-removal candidate. It is the
// single field the UI keys its badge colour off, and the gate the destructive
// path re-derives server-side before it touches git.
type CleanupState string

const (
	// CleanupSafe — isolated, not running, on a branch, fully merged into the
	// workspace branch, clean tree, nothing unpushed, no stash. Removing it
	// loses nothing.
	CleanupSafe CleanupState = "safe"
	// CleanupReview — removable, but at least one Warning means real work would
	// be lost. Requires explicit confirmDestroy before removal.
	CleanupReview CleanupState = "review"
	// CleanupBlocked — at least one hard Blocker. NEVER removable: running,
	// not isolated, detached/no branch, or a snapshot error on a live dir.
	CleanupBlocked CleanupState = "blocked"
	// CleanupOrphaned — the worktree directory no longer exists on disk, so
	// removal only prunes leftover git bookkeeping. Safe.
	CleanupOrphaned CleanupState = "orphaned"
	// CleanupRecordOnly — a direct-workspace ("not isolated") agent. It owns no
	// dedicated worktree: it runs in the user's checkout, so there is nothing on
	// disk to delete. Removal archives only the agent's SESSION (reversibly via
	// cleanup.restore) to clear a stale thread from the list; the checkout is
	// left completely untouched. Removable when the agent is not running.
	CleanupRecordOnly CleanupState = "recordOnly"
)

// CleanupCandidate is the full, JSON-serialisable analysis of one worktree as a
// removal candidate. Blockers/Warnings are human-readable codes the UI maps to
// tooltip text; Removable/State are the machine verdict the server re-derives.
type CleanupCandidate struct {
	ThreadID        string       `json:"threadId"`
	Number          int          `json:"number"`
	Branch          string       `json:"branch"`
	Path            string       `json:"path"`
	Title           string       `json:"title"`
	State           CleanupState `json:"state"`
	Blockers        []string     `json:"blockers"`
	Warnings        []string     `json:"warnings"`
	Merged          bool         `json:"merged"`
	Ahead           int          `json:"ahead"`
	DirtyCount      int          `json:"dirtyCount"`
	UnpushedCommits int          `json:"unpushedCommits"`
	StashCount      int          `json:"stashCount"`
	LastActivity    time.Time    `json:"lastActivity"`
	DiffStat        string       `json:"diffStat"`
	Removable       bool         `json:"removable"`
	// Recommendation / Reason are written ONLY by the optional phase-2 Sonnet
	// advisor (AdviseCleanup). They never affect State or Removable.
	Recommendation string `json:"recommendation,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Blocker / warning codes. Kept as stable strings so the UI can map them to
// localised tooltip text without parsing prose.
const (
	BlockerRunning     = "running"
	BlockerNotIsolated = "notIsolated"
	BlockerDetached    = "detachedOrNoBranch"
	BlockerSnapshot    = "snapshotError"

	WarnUnmerged   = "unmerged"
	WarnDirty      = "dirty"
	WarnUnpushed   = "unpushed"
	WarnHasStash   = "hasStash"
	WarnBranchGone = "branchGoneOnRemote"
)

// AnalyzeCandidate is the deterministic gate. Given a worktree, its current
// snapshot (nil for an orphaned record whose dir is gone), whether a live
// process owns it, the thread title and last activity, it returns the full
// verdict.
//
// It re-derives "merged?" from scratch via IsMergedInto against the workspace
// branch's TIP — never trusting Ahead. It is pure read: the only side effect is
// the read-only git queries IsMergedInto / StashCount / UnpushedCount run.
//
// SAFETY: Removable is false whenever ANY Blocker is present. The destructive
// IPC handler calls this again and refuses on !Removable, so a stale client can
// never bypass the gate.
func AnalyzeCandidate(wt worktree.Worktree, snap *Snapshot, running bool, title string, lastActivity time.Time) CleanupCandidate {
	c := CleanupCandidate{
		ThreadID:     wt.ThreadID,
		Number:       wt.Number,
		Branch:       wt.Branch,
		Path:         wt.Path,
		Title:        title,
		LastActivity: lastActivity,
		Blockers:     []string{},
		Warnings:     []string{},
	}

	// Orphaned: the directory is gone. Removing it only prunes git bookkeeping
	// (the branch ref + worktree admin dir), so it is always safe — no work can
	// be lost from a tree that no longer exists. This takes precedence over
	// every other check, including a stale snapshot error.
	dirExists := wt.Path != "" && pathExists(wt.Path)
	if wt.Isolated && !dirExists {
		c.State = CleanupOrphaned
		c.Removable = true
		return c
	}

	// --- hard blockers (never removable) -------------------------------------
	if running {
		c.Blockers = append(c.Blockers, BlockerRunning)
	}
	// A missing branch is only a fault for an ISOLATED worktree — a detached
	// HEAD we refuse to guess about. A direct-workspace agent legitimately has
	// no branch of its own (it shares the checkout's branch), so this never
	// applies to it; gating on Isolated is what lets such an agent reach the
	// record-only removal path below instead of being wrongly flagged detached.
	if wt.Isolated && wt.Branch == "" {
		c.Blockers = append(c.Blockers, BlockerDetached)
	}
	if snap != nil && snap.Error != "" && dirExists {
		// A live directory we could not read git state for — refuse to guess.
		c.Blockers = append(c.Blockers, BlockerSnapshot)
	}

	// Pull the cheap counts off the snapshot when we have one.
	if snap != nil {
		c.Ahead = snap.Ahead
		c.DirtyCount = snap.DirtyCount
		// A direct-workspace agent has no branch of its own; borrow the
		// checkout's current branch (from the snapshot) for display so the row
		// reads "main" rather than the misleading "(detached)".
		if c.Branch == "" {
			c.Branch = snap.Branch
		}
	}

	// "Merged into the workspace branch?" — the authoritative test. Only
	// meaningful for an isolated branch with a known head; skip it (and the
	// warnings derived from git) when a blocker already means we can't read
	// state, to avoid spurious git calls on a broken tree.
	canQueryGit := wt.Isolated && wt.Branch != "" && wt.RepoRoot != "" &&
		(snap == nil || snap.Error == "")
	if canQueryGit && snap != nil && snap.HeadSHA != "" {
		target := WorkspaceHeadBranch(wt.RepoRoot)
		merged, err := IsMergedInto(wt.RepoRoot, snap.HeadSHA, target)
		if err == nil {
			c.Merged = merged
		}
	}

	// --- warnings (removable, but real work is at stake) ---------------------
	if canQueryGit {
		// Unpushed: prefer the upstream-tracking comparison; fall back to "has
		// local commits and was never pushed" when there is no upstream.
		unpushed, hasUpstream := UnpushedCount(wt.RepoRoot, wt.Branch)
		c.UnpushedCommits = unpushed
		c.StashCount = StashCount(wt.Path)

		if !c.Merged && c.Ahead > 0 {
			c.Warnings = append(c.Warnings, WarnUnmerged)
		}
		if c.DirtyCount > 0 {
			c.Warnings = append(c.Warnings, WarnDirty)
		}
		// "Never pushed" only loses work that isn't already captured elsewhere:
		// if the branch is fully merged into the workspace branch, its commits
		// live on there regardless of any remote, so unpushed is not a concern.
		if hasUpstream && unpushed > 0 {
			c.Warnings = append(c.Warnings, WarnUnpushed)
		} else if !hasUpstream && c.Ahead > 0 && !c.Merged {
			c.Warnings = append(c.Warnings, WarnUnpushed)
		}
		if c.StashCount > 0 {
			c.Warnings = append(c.Warnings, WarnHasStash)
		}
	} else if wt.Isolated && c.DirtyCount > 0 {
		// Even when we can't run the full git query set (e.g. detached), a
		// dirty tree is still a flag for any isolated path that reaches removal.
		// Direct-workspace agents are excluded: a dirty checkout is shared work
		// that archiving the session record cannot lose, so it is not a warning.
		c.Warnings = append(c.Warnings, WarnDirty)
	}

	// --- verdict -------------------------------------------------------------
	c.Removable = len(c.Blockers) == 0
	switch {
	case !c.Removable:
		c.State = CleanupBlocked
	case !wt.Isolated:
		// Removable, but with no worktree to delete: removal archives only the
		// agent's session record. Distinct from "safe" so the UI can say so.
		c.State = CleanupRecordOnly
	case len(c.Warnings) > 0:
		c.State = CleanupReview
	default:
		c.State = CleanupSafe
	}
	return c
}

// pathExists reports whether path exists on disk. Used to detect orphaned
// worktrees whose directory was deleted out-of-band.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
