// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import "github.com/go-git/go-git/v5"

// openRepo wraps PlainOpenWithOptions with the flag a linked worktree needs:
// EnableDotGitCommonDir teaches go-git to follow the `.git` linkfile that lives
// at the worktree root, so per-worktree HEAD / refs resolve correctly. Without
// it, repo.Head() returns "reference not found" on every isolated agent
// worktree we create.
func openRepo(path string) (*git.Repository, error) {
	return git.PlainOpenWithOptions(path, &git.PlainOpenOptions{
		EnableDotGitCommonDir: true,
	})
}

// WorkspaceHeadBranch returns the short branch name HEAD points at in the
// given repo, or the empty string when HEAD is detached, the repo is not on
// a branch yet, or the directory is not a git repo. Used to label the
// "view workspace branch" entry in the log viewer.
func WorkspaceHeadBranch(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	repo, err := openRepo(repoRoot)
	if err != nil {
		return ""
	}
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	if !head.Name().IsBranch() {
		return ""
	}
	return head.Name().Short()
}
