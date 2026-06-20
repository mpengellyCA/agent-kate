package session

import (
	"os"
	"path/filepath"
	"strings"
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

func TestDeleteTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	const sid = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	file := filepath.Join(home, "projects", "-work-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DeleteTranscript(sid); err != nil {
		t.Fatalf("DeleteTranscript: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("transcript should be removed from disk")
	}
	// Deleting a session with no transcript is an error, so the caller can
	// surface "already gone".
	if err := DeleteTranscript("no-such-session"); err == nil {
		t.Fatal("DeleteTranscript should error when no transcript exists")
	}
}

func TestPreviewTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	const sid = "cccccccc-dddd-4eee-8fff-000000000000"
	file := filepath.Join(home, "projects", "-work-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"user","message":{"content":"hello there"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi back"}]}}
{"type":"user","message":{"content":"second"}}
`
	if err := os.WriteFile(file, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, truncated, err := PreviewTranscript(sid, 0)
	if err != nil {
		t.Fatalf("PreviewTranscript: %v", err)
	}
	if truncated {
		t.Fatal("3 messages under the default limit should not be truncated")
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "hello there" {
		t.Fatalf("first message = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "hi back" {
		t.Fatalf("second message = %+v", msgs[1])
	}

	// Limiting to 1 keeps only the most recent turn and flags truncation.
	last, trunc, err := PreviewTranscript(sid, 1)
	if err != nil {
		t.Fatalf("PreviewTranscript limit: %v", err)
	}
	if !trunc || len(last) != 1 || last[0].Text != "second" {
		t.Fatalf("limited preview = %+v truncated=%v", last, trunc)
	}
}

// TestPreviewKeepsRichContent guards against the preview clipping multi-line
// turns or silently dropping tool-only turns.
func TestPreviewKeepsRichContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	const sid = "dddddddd-eeee-4fff-8aaa-111111111111"
	file := filepath.Join(home, "projects", "-work-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	// A multi-line assistant turn, then a turn that is only a tool_use block.
	lines := `{"type":"assistant","message":{"content":[{"type":"text","text":"line one\nline two\nline three"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}
`
	if err := os.WriteFile(file, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, _, err := PreviewTranscript(sid, 0)
	if err != nil {
		t.Fatalf("PreviewTranscript: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (tool-only turn must survive)", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "\n") {
		t.Fatalf("newlines were stripped from the turn: %q", msgs[0].Text)
	}
	if msgs[1].Text == "" {
		t.Fatal("tool-only turn should render a placeholder, not be dropped")
	}
}
