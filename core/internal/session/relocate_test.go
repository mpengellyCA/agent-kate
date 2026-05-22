package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncodeProjectPath(t *testing.T) {
	got := encodeProjectPath("/home/me/Dev/App/.agentkate/worktrees/t-abc")
	want := "-home-me-Dev-App--agentkate-worktrees-t-abc"
	if got != want {
		t.Fatalf("encodeProjectPath = %q, want %q", got, want)
	}
}

func TestPromoteTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	const sid = "11111111-2222-4333-8444-555555555555"
	const threadID = "t-promote1"

	// A transcript stored as if the session had run in /work/proj.
	srcFile := filepath.Join(home, "projects", "-work-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PromoteTranscript(sid, threadID); err != nil {
		t.Fatalf("PromoteTranscript: %v", err)
	}

	want := filepath.Join(home, "projects",
		"-work-proj--agentkate-worktrees-t-promote1", sid+".jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("transcript not relocated to %s: %v", want, err)
	}
	if _, err := os.Stat(srcFile); !os.IsNotExist(err) {
		t.Fatal("the source transcript should be moved, not copied")
	}
}

func TestPromoteTranscriptMissing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := PromoteTranscript("no-such-session", "t-x"); err == nil {
		t.Fatal("PromoteTranscript should error when no transcript exists")
	}
}
