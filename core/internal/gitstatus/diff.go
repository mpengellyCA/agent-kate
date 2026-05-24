// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"agentkate/internal/worktree"
)

// UnifiedDiff returns a unified patch of the worktree vs HEAD, optionally
// restricted to one file. Untracked files are included as full new-file
// diffs so the Changes view shows them even though git diff HEAD wouldn't.
// Unlike the legacy worktree.Diff it does not mutate the index.
//
// relPath is forward-slashed and worktree-relative; "" means whole worktree.
func UnifiedDiff(wt worktree.Worktree, relPath string) (string, error) {
	if wt.Path == "" {
		return "", nil
	}
	var out bytes.Buffer

	// Tracked changes via `git diff HEAD`. This is also a no-op when HEAD
	// does not exist (empty repo): the command fails and we fall through to
	// untracked-only handling.
	args := []string{"diff", "--no-color", "HEAD"}
	if relPath != "" {
		args = append(args, "--", filepath.FromSlash(relPath))
	}
	if patch, err := runGit(wt.Path, args...); err == nil {
		out.WriteString(patch)
	}

	// Untracked files. ls-files --others gives one path per NUL-separated
	// entry, scoped to relPath when supplied so a single-file diff stays
	// single-file.
	lsArgs := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	if relPath != "" {
		lsArgs = append(lsArgs, "--", filepath.FromSlash(relPath))
	}
	if raw, err := runGit(wt.Path, lsArgs...); err == nil {
		for _, name := range strings.Split(raw, "\x00") {
			if name == "" {
				continue
			}
			// `git diff --no-index` always exits 1 on a difference (which a
			// new-file diff always is); treat that as success.
			patch, _ := runGitAllowExit1(wt.Path, "diff", "--no-color",
				"--no-index", "--", "/dev/null", name)
			out.WriteString(patch)
		}
	}

	return out.String(), nil
}

// runGit runs a git command in dir and returns its stdout on success.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// runGitAllowExit1 is for `git diff` family commands that exit 1 to signal
// "differences found", which isn't an error from our perspective.
func runGitAllowExit1(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return string(out), nil
		}
		return "", err
	}
	return string(out), nil
}
