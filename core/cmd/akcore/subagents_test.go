package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Both CLIs write a per-subagent conversation, in different layouts — probed on
// the real tools (plan 16 P6):
//
//	claude 2.1.220: <project>/<session>/subagents/agent-<id>.jsonl   (files)
//	kimi 0.30.0:    <session-dir>/agents/<id>/wire.jsonl             (directories)
//
// scanSubagentDir is the one place that knows the difference, so these fixtures
// mirror the real trees exactly — including kimi's "main" directory, which is
// the THREAD's own log and must never be offered as one of its helpers.
func TestScanSubagentDirClaudeLayout(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"agent-a47506c95ca4200c5.jsonl",
		"agent-ad6a0dbc3a9d1a2ec.jsonl",
		"notes.txt", // not a transcript
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := scanSubagentDir(dir, false)
	if len(got) != 2 {
		t.Fatalf("found %d transcripts, want 2: %+v", len(got), got)
	}
	if got[0].ID != "agent-a47506c95ca4200c5" {
		t.Errorf("id = %q, want the file stem", got[0].ID)
	}
	if filepath.Base(got[0].Path) != "agent-a47506c95ca4200c5.jsonl" {
		t.Errorf("path = %q", got[0].Path)
	}
}

func TestScanSubagentDirKimiLayout(t *testing.T) {
	dir := t.TempDir()
	write := func(agent, body string) {
		d := filepath.Join(dir, agent)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "wire.jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main", `{"type":"metadata"}`+"\n")
	write("agent-0", `{"type":"metadata","protocol_version":"1.4"}`+"\n"+
		`{"type":"config.update","profileName":"explore"}`+"\n")
	write("agent-1", `{"type":"metadata"}`+"\n")
	// A directory with no wire log yet (the subagent just started).
	if err := os.MkdirAll(filepath.Join(dir, "agent-2"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := scanSubagentDir(dir, true)
	if len(got) != 2 {
		t.Fatalf("found %d transcripts, want agent-0 and agent-1: %+v", len(got), got)
	}
	for _, g := range got {
		if g.ID == "main" {
			t.Error(`"main" is the thread's own log, not a helper`)
		}
		if filepath.Base(g.Path) != "wire.jsonl" {
			t.Errorf("path = %q, want the wire log", g.Path)
		}
	}
	// The label comes out of the head of the log, where kimi records which
	// built-in profile ran (coder / explore / plan).
	if label := kimiSubagentLabel(got[0].Path); label != "explore" {
		t.Errorf("label = %q, want explore", label)
	}
	if label := kimiSubagentLabel(got[1].Path); label != "" {
		t.Errorf("label = %q for a log with no profile line, want empty", label)
	}
	if label := kimiSubagentLabel(filepath.Join(dir, "nope", "wire.jsonl")); label != "" {
		t.Errorf("label = %q for a missing file", label)
	}
}

func TestScanSubagentDirMissing(t *testing.T) {
	if got := scanSubagentDir(filepath.Join(t.TempDir(), "nope"), false); got != nil {
		t.Errorf("missing directory returned %+v", got)
	}
	if got := scanSubagentDir(filepath.Join(t.TempDir(), "nope"), true); got != nil {
		t.Errorf("missing directory returned %+v", got)
	}
}
