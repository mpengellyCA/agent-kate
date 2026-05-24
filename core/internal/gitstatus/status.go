// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"agentkate/internal/worktree"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Snapshot is one worktree's git state at a point in time. JSON-serialised
// straight to the UI.
type Snapshot struct {
	ThreadID     string       `json:"threadId"`
	Number       int          `json:"number,omitempty"` // project-scoped agent number (#N)
	RepoRoot     string       `json:"repoRoot"`
	Path         string       `json:"path"`
	Branch       string       `json:"branch"`
	Isolated     bool         `json:"isolated"`
	HeadSHA      string       `json:"headSha"`
	Base         string       `json:"base"`
	Ahead        int          `json:"ahead"`        // commits on Branch since Base
	BehindBase   int          `json:"behindBase"`   // commits on the workspace's current branch since Base
	DirtyCount   int          `json:"dirtyCount"`   // tracked changes + untracked + conflicts
	HasConflicts bool         `json:"hasConflicts"`
	Files        []FileStatus `json:"files"`
	UpdatedAt    time.Time    `json:"updatedAt"`
	// Error is set when this snapshot could not be computed; the rest of the
	// fields hold whatever we managed to gather before failure.
	Error string `json:"error,omitempty"`
}

// FileStatus is one path's state in the worktree. Paths are repo-relative,
// forward-slashed regardless of platform.
type FileStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"` // see StatusXxx constants
}

const (
	StatusClean      = "clean"
	StatusModified   = "modified"
	StatusAdded      = "added"
	StatusDeleted    = "deleted"
	StatusUntracked  = "untracked"
	StatusRenamed    = "renamed"
	StatusConflicted = "conflicted"
)

// computeSnapshot is the read path: opens the worktree's repo, queries
// porcelain status, and computes ahead/behind vs the recorded base commit.
func computeSnapshot(wt worktree.Worktree) (*Snapshot, error) {
	snap := &Snapshot{
		ThreadID:  wt.ThreadID,
		Number:    wt.Number,
		RepoRoot:  wt.RepoRoot,
		Path:      wt.Path,
		Branch:    wt.Branch,
		Isolated:  wt.Isolated,
		Base:      wt.Base,
		UpdatedAt: time.Now().UTC(),
	}

	repo, err := openRepo(wt.Path)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			// Non-git working directory (e.g. a workspace with no repo) — return
			// an empty-but-valid snapshot so the dashboard shows the row.
			return snap, nil
		}
		return snap, err
	}

	if head, err := repo.Head(); err == nil {
		snap.HeadSHA = head.Hash().String()
		if snap.Branch == "" && head.Name().IsBranch() {
			snap.Branch = head.Name().Short()
		}
	}

	wtree, err := repo.Worktree()
	if err != nil {
		return snap, err
	}
	status, err := wtree.Status()
	if err != nil {
		return snap, err
	}
	for path, s := range status {
		fs := classifyStatus(path, s)
		if fs.Status == StatusClean {
			continue
		}
		snap.Files = append(snap.Files, fs)
		if fs.Status == StatusConflicted {
			snap.HasConflicts = true
		}
	}
	snap.DirtyCount = len(snap.Files)

	// Ahead / behind against Base. For an isolated worktree Base is the commit
	// the branch forked from. We compute:
	//   Ahead       — commits on HEAD that are not in Base (the work the agent
	//                 has done since branching)
	//   BehindBase  — commits on the parent repo's current branch that are not
	//                 in Base (how far the workspace has moved on)
	if wt.Base != "" && snap.HeadSHA != "" {
		if n, err := countAncestorsSince(repo, plumbing.NewHash(snap.HeadSHA), wt.Base); err == nil {
			snap.Ahead = n
		}
	}
	if wt.Base != "" && wt.RepoRoot != "" && wt.RepoRoot != wt.Path {
		if parent, err := openRepo(wt.RepoRoot); err == nil {
			if parentHead, err := parent.Head(); err == nil {
				if n, err := countAncestorsSince(parent, parentHead.Hash(), wt.Base); err == nil {
					snap.BehindBase = n
				}
			}
		}
	}

	return snap, nil
}

// countAncestorsSince walks from `from` and counts commits strictly newer than
// `since` on the first-parent path. Stops as soon as it reaches `since`.
func countAncestorsSince(repo *git.Repository, from plumbing.Hash, since string) (int, error) {
	stop := plumbing.NewHash(since)
	iter, err := repo.Log(&git.LogOptions{From: from})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	count := 0
	const safety = 10000 // never walk a runaway history
	for i := 0; i < safety; i++ {
		c, err := iter.Next()
		if err != nil {
			return count, nil
		}
		if c.Hash == stop {
			return count, nil
		}
		count++
	}
	return count, nil
}

// classifyStatus folds go-git's two-axis (staging + worktree) status into the
// single label the UI shows. Conflicts take precedence over everything else.
func classifyStatus(path string, s *git.FileStatus) FileStatus {
	rel := filepath.ToSlash(path)
	out := FileStatus{Path: rel, Status: StatusClean}
	// Conflicts are signalled by UpdatedButUnmerged in either axis.
	if s.Staging == git.UpdatedButUnmerged || s.Worktree == git.UpdatedButUnmerged {
		out.Status = StatusConflicted
		return out
	}
	// Untracked / ignored — only the worktree side carries this.
	switch s.Worktree {
	case git.Untracked:
		out.Status = StatusUntracked
		return out
	}
	// Renames are reported by the staging axis.
	if s.Staging == git.Renamed || s.Worktree == git.Renamed {
		out.Status = StatusRenamed
		return out
	}
	// Additions / deletions / modifications: the worktree side wins when it
	// disagrees with staging, since that's what the user sees on disk.
	switch {
	case s.Worktree == git.Added || s.Staging == git.Added:
		out.Status = StatusAdded
	case s.Worktree == git.Deleted || s.Staging == git.Deleted:
		out.Status = StatusDeleted
	case s.Worktree == git.Modified || s.Staging == git.Modified:
		out.Status = StatusModified
	}
	return out
}

// relativeTo returns absPath relative to root if absPath is inside root.
// Result is forward-slashed.
func relativeTo(root, absPath string) (string, bool) {
	if root == "" || absPath == "" {
		return "", false
	}
	r, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	a, err := filepath.Abs(absPath)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(r, a)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	if strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
