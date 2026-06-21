package session

import "testing"

func TestClampProposalTags(t *testing.T) {
	got := clampProposalTags([]string{"  Frontend ", "frontend", "BugFix", "auth", "extra"})
	// lowercased, deduped case-insensitively, capped at 3.
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (%v)", len(got), got)
	}
	if got[0] != "frontend" || got[1] != "bugfix" || got[2] != "auth" {
		t.Fatalf("got %v, want [frontend bugfix auth]", got)
	}
	if len(clampProposalTags(nil)) != 0 {
		t.Fatal("nil input should yield no tags")
	}
	if len(clampProposalTags([]string{"  ", ""})) != 0 {
		t.Fatal("all-empty input should yield no tags")
	}
}

func TestStripOrganizeFence(t *testing.T) {
	in := "```json\n[{\"threadId\":\"t-1\",\"tags\":[\"a\"]}]\n```"
	out := stripOrganizeFence(in)
	if out != "[{\"threadId\":\"t-1\",\"tags\":[\"a\"]}]" {
		t.Fatalf("stripOrganizeFence = %q", out)
	}
	// Unfenced text passes through untouched.
	if stripOrganizeFence("plain") != "plain" {
		t.Fatal("unfenced text changed")
	}
}
