// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentkate/internal/worktree"
)

// testGit runs a git command in dir, failing the test on non-zero exit. Used by
// the test fixtures to build hand-crafted repo topologies.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Agent Kate Test",
		"GIT_AUTHOR_EMAIL=test@agentkate",
		"GIT_COMMITTER_NAME=Agent Kate Test",
		"GIT_COMMITTER_EMAIL=test@agentkate",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initLinearRepo builds a fresh repo with N linear commits on `main`. Returns
// the repo path and the SHAs in chronological (oldest-first) order.
func initLinearRepo(t *testing.T, n int) (string, []string) {
	t.Helper()
	repo := t.TempDir()
	testGit(t, repo, "init", "-q", "-b", "main")
	shas := make([]string, n)
	for i := 0; i < n; i++ {
		fn := filepath.Join(repo, "f.txt")
		if err := os.WriteFile(fn, []byte(strings.Repeat("x\n", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, repo, "add", "f.txt")
		testGit(t, repo, "commit", "-q", "-m", "commit "+strconv.Itoa(i+1))
		shas[i] = testGit(t, repo, "rev-parse", "HEAD")
	}
	return repo, shas
}

// wtFor returns a worktree.Worktree pointing at path; we don't actually need
// the registry, just the Path field for the gitstatus helpers.
func wtFor(path string) worktree.Worktree {
	return worktree.Worktree{Path: path, RepoRoot: path}
}
