// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import "testing"

func TestBranchesListsLocalsWithCurrent(t *testing.T) {
	repo, _ := initLinearRepo(t, 2)
	// Add a second local branch so we exercise ordering + current flagging.
	testGit(t, repo, "branch", "feature-x")

	got, err := Branches(wtFor(repo))
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 branches, got %d: %+v", len(got), got)
	}
	// Alphabetical: "feature-x" before "main".
	if got[0].Name != "feature-x" || got[1].Name != "main" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got[0].Current {
		t.Errorf("feature-x should not be current")
	}
	if !got[1].Current {
		t.Errorf("main should be current")
	}
	for _, b := range got {
		if b.IsRemote {
			t.Errorf("no remotes expected, got %q", b.Name)
		}
	}
}

func TestBranchesEmptyRepoNoError(t *testing.T) {
	repo := t.TempDir()
	got, err := Branches(wtFor(repo))
	if err != nil {
		t.Fatalf("Branches on empty dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty list, got %+v", got)
	}
}
