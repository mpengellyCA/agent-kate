// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"strings"
	"testing"
)

func TestCommitDetail_FilesAndStatuses(t *testing.T) {
	repo := t.TempDir()
	testGit(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "a.txt", "alpha\n")
	writeFile(t, repo, "b.txt", "beta\n")
	testGit(t, repo, "add", "a.txt", "b.txt")
	testGit(t, repo, "commit", "-q", "-m", "initial: two files")
	rootSHA := testGit(t, repo, "rev-parse", "HEAD")

	// Modify a, add c, delete b — three different statuses in one commit.
	writeFile(t, repo, "a.txt", "alpha\nplus\n")
	writeFile(t, repo, "c.txt", "gamma\n")
	testGit(t, repo, "rm", "-q", "b.txt")
	testGit(t, repo, "add", "a.txt", "c.txt")
	testGit(t, repo, "commit", "-q", "-m", "second: mixed")
	mixedSHA := testGit(t, repo, "rev-parse", "HEAD")

	d, err := CommitDetailFn(wtFor(repo), mixedSHA)
	if err != nil {
		t.Fatalf("CommitDetailFn: %v", err)
	}
	if d.SHA != mixedSHA {
		t.Fatalf("sha = %s, want %s", d.SHA, mixedSHA)
	}
	if d.Subject != "second: mixed" {
		t.Fatalf("subject = %q, want 'second: mixed'", d.Subject)
	}
	statusOf := func(path string) string {
		for _, f := range d.Files {
			if f.Path == path {
				return f.Status
			}
		}
		return ""
	}
	if got := statusOf("a.txt"); got != "modified" {
		t.Fatalf("a.txt status = %q, want modified", got)
	}
	if got := statusOf("b.txt"); got != "deleted" {
		t.Fatalf("b.txt status = %q, want deleted", got)
	}
	if got := statusOf("c.txt"); got != "added" {
		t.Fatalf("c.txt status = %q, want added", got)
	}

	// The root commit has no parent — go-git's Stats() should still report
	// its files (every file looks "added").
	root, err := CommitDetailFn(wtFor(repo), rootSHA)
	if err != nil {
		t.Fatalf("CommitDetailFn root: %v", err)
	}
	if len(root.Parents) != 0 {
		t.Fatalf("root parents = %v, want empty", root.Parents)
	}
	if len(root.Files) != 2 {
		t.Fatalf("root files = %d, want 2", len(root.Files))
	}
}

func TestCommitDiff_FullAndScoped(t *testing.T) {
	repo := t.TempDir()
	testGit(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "a.txt", "x\n")
	writeFile(t, repo, "b.txt", "y\n")
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-q", "-m", "two files")
	writeFile(t, repo, "a.txt", "x\nextra\n")
	writeFile(t, repo, "b.txt", "y\nextra\n")
	testGit(t, repo, "commit", "-q", "-am", "touch both")
	sha := testGit(t, repo, "rev-parse", "HEAD")

	full, err := CommitDiff(wtFor(repo), sha, "")
	if err != nil {
		t.Fatalf("CommitDiff full: %v", err)
	}
	if !strings.Contains(full, "a.txt") || !strings.Contains(full, "b.txt") {
		t.Fatalf("full diff missing files:\n%s", full)
	}

	scoped, err := CommitDiff(wtFor(repo), sha, "a.txt")
	if err != nil {
		t.Fatalf("CommitDiff scoped: %v", err)
	}
	if !strings.Contains(scoped, "a.txt") {
		t.Fatalf("scoped diff missing a.txt:\n%s", scoped)
	}
	if strings.Contains(scoped, "+++ b/b.txt") {
		t.Fatalf("scoped diff leaked b.txt:\n%s", scoped)
	}
}
