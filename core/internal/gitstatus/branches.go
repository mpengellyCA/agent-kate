// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"errors"
	"sort"

	"agentkate/internal/worktree"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// BranchRef is one entry in the branch selector: a local or remote-tracking
// branch, plus whether HEAD currently points at it. The UI renders these in a
// dropdown so the user can scope the log to any branch (read-only — this never
// checks anything out).
type BranchRef struct {
	Name     string `json:"name"`     // "main", "origin/feature-x"
	Current  bool   `json:"current"`  // HEAD points here (local branches only)
	IsRemote bool   `json:"isRemote"` // a remote-tracking ref
}

// Branches lists every local and remote-tracking branch in the worktree's repo,
// local branches first (alphabetical), then remotes (alphabetical). The branch
// HEAD points at is flagged Current. An empty/unborn repo yields an empty list
// rather than an error, so the UI just shows "(current branch)".
//
// It walks go-git's reference iterator rather than shelling `git branch -a`,
// which keeps it consistent with collectRefs() and, crucially, resolves refs
// correctly inside a linked agent worktree (openRepo enables the common-dir
// follow that a raw `git branch` in that directory would also honour). The
// output shape matches what a `git branch -a --format` shell-out would produce.
func Branches(wt worktree.Worktree) ([]BranchRef, error) {
	if wt.Path == "" {
		return []BranchRef{}, nil
	}
	repo, err := openRepo(wt.Path)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return []BranchRef{}, nil
		}
		return nil, err
	}

	// Current branch (empty when detached / unborn).
	var current string
	if head, err := repo.Head(); err == nil && head.Name().IsBranch() {
		current = head.Name().Short()
	}

	iter, err := repo.References()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var locals, remotes []BranchRef
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		name := ref.Name()
		switch {
		case name.IsBranch():
			short := name.Short()
			locals = append(locals, BranchRef{Name: short, Current: short == current})
		case name.IsRemote():
			short := name.Short() // "origin/main"
			// Skip the symbolic "origin/HEAD" pointer — it's noise in a picker.
			if short == "origin/HEAD" {
				return nil
			}
			remotes = append(remotes, BranchRef{Name: short, IsRemote: true})
		}
		return nil
	})

	sort.Slice(locals, func(i, j int) bool { return locals[i].Name < locals[j].Name })
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })

	out := make([]BranchRef, 0, len(locals)+len(remotes))
	out = append(out, locals...)
	out = append(out, remotes...)
	return out, nil
}
