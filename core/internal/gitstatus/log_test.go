// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLog_LinearHistory(t *testing.T) {
	repo, shas := initLinearRepo(t, 5)
	wt := wtFor(repo)

	got, err := Log(wt, LogOptions{})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d entries, want 5", len(got))
	}
	// Newest first: entry[i] should be shas[4-i].
	for i, e := range got {
		want := shas[len(shas)-1-i]
		if e.SHA != want {
			t.Fatalf("entry %d sha = %s, want %s", i, e.SHA, want)
		}
		if e.Lane != 0 {
			t.Fatalf("entry %d on lane %d, want 0 (linear)", i, e.Lane)
		}
	}
	// Subject should round-trip the message we used.
	if !strings.HasPrefix(got[0].Subject, "commit ") {
		t.Fatalf("subject %q does not look like our test fixture", got[0].Subject)
	}
	if got[0].Author != "AgentKate Test" {
		t.Fatalf("author = %q, want %q", got[0].Author, "AgentKate Test")
	}
}

func TestLog_Pagination(t *testing.T) {
	repo, _ := initLinearRepo(t, 12)
	wt := wtFor(repo)

	page1, err := Log(wt, LogOptions{Limit: 5})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 5 {
		t.Fatalf("page1 len = %d, want 5", len(page1))
	}
	page2, err := Log(wt, LogOptions{Skip: 5, Limit: 5})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 5 {
		t.Fatalf("page2 len = %d, want 5", len(page2))
	}
	// No overlap between pages.
	page1Set := make(map[string]bool, len(page1))
	for _, e := range page1 {
		page1Set[e.SHA] = true
	}
	for _, e := range page2 {
		if page1Set[e.SHA] {
			t.Fatalf("page2 entry %s also in page1", e.SHA)
		}
	}
}

func TestLog_MergeProducesTwoLanes(t *testing.T) {
	// Build:
	//   *   merge       (M, parents=[F, T])
	//   |\
	//   | * topic edit  (T)
	//   * | trunk edit  (F)
	//   |/
	//   *   base        (B)
	repo := t.TempDir()
	testGit(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "a.txt", "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-q", "-m", "base")
	testGit(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, repo, "a.txt", "base\ntopic\n")
	testGit(t, repo, "commit", "-q", "-am", "topic edit")
	topicSHA := testGit(t, repo, "rev-parse", "HEAD")
	testGit(t, repo, "checkout", "-q", "main")
	writeFile(t, repo, "b.txt", "trunk\n")
	testGit(t, repo, "add", "b.txt")
	testGit(t, repo, "commit", "-q", "-m", "trunk edit")
	trunkSHA := testGit(t, repo, "rev-parse", "HEAD")
	testGit(t, repo, "merge", "--no-ff", "-q", "-m", "merge topic", topicSHA)
	mergeSHA := testGit(t, repo, "rev-parse", "HEAD")

	got, err := Log(wtFor(repo), LogOptions{})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d entries, want 4", len(got))
	}
	// First entry must be the merge commit and sit on lane 0 with two outgoing
	// lanes.
	if got[0].SHA != mergeSHA {
		t.Fatalf("entry[0] = %s, want merge %s", got[0].SHA, mergeSHA)
	}
	if got[0].Lane != 0 {
		t.Fatalf("merge on lane %d, want 0", got[0].Lane)
	}
	if len(got[0].LanesOut) != 2 {
		t.Fatalf("merge LanesOut = %v, want two lanes", got[0].LanesOut)
	}
	// Trunk and topic should land on different lanes — that's the whole point
	// of a graph rail.
	var trunkLane, topicLane int = -1, -1
	for _, e := range got {
		switch e.SHA {
		case trunkSHA:
			trunkLane = e.Lane
		case topicSHA:
			topicLane = e.Lane
		}
	}
	if trunkLane == -1 || topicLane == -1 {
		t.Fatalf("missing trunk or topic: trunk=%d topic=%d", trunkLane, topicLane)
	}
	if trunkLane == topicLane {
		t.Fatalf("trunk and topic share lane %d — graph would collapse", trunkLane)
	}
}

func TestLog_Refs(t *testing.T) {
	repo, shas := initLinearRepo(t, 3)
	testGit(t, repo, "tag", "v1", shas[1])
	testGit(t, repo, "branch", "feature", shas[2])

	got, err := Log(wtFor(repo), LogOptions{})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	// Find the entries by SHA and check refs.
	refsOf := func(sha string) []string {
		for _, e := range got {
			if e.SHA == sha {
				return e.Refs
			}
		}
		return nil
	}
	if got := refsOf(shas[1]); !containsString(got, "tag:v1") {
		t.Fatalf("commit shas[1] refs = %v, want tag:v1", got)
	}
	headRefs := refsOf(shas[2])
	if !containsString(headRefs, "main") {
		t.Fatalf("HEAD refs = %v, want 'main'", headRefs)
	}
	if !containsString(headRefs, "feature") {
		t.Fatalf("HEAD refs = %v, want 'feature'", headRefs)
	}
}

func TestLog_PathFilter(t *testing.T) {
	// Two files; only commits touching kept.txt should appear when filtered.
	repo := t.TempDir()
	testGit(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "kept.txt", "1\n")
	testGit(t, repo, "add", "kept.txt")
	testGit(t, repo, "commit", "-q", "-m", "kept v1")
	writeFile(t, repo, "other.txt", "x\n")
	testGit(t, repo, "add", "other.txt")
	testGit(t, repo, "commit", "-q", "-m", "other only")
	writeFile(t, repo, "kept.txt", "1\n2\n")
	testGit(t, repo, "commit", "-q", "-am", "kept v2")

	got, err := Log(wtFor(repo), LogOptions{Path: "kept.txt"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (filter should drop 'other only')", len(got))
	}
	for _, e := range got {
		if !strings.Contains(e.Subject, "kept") {
			t.Fatalf("path filter leaked entry %q", e.Subject)
		}
		// Path filter view collapses to lane 0 with no graph topology.
		if e.Lane != 0 {
			t.Fatalf("filtered entry on lane %d, want 0", e.Lane)
		}
	}
}

func writeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
