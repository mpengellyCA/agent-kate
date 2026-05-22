package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeTranscript writes a .jsonl session transcript from raw lines.
func writeTranscript(t *testing.T, root, projectDir, sessionID string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, projectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"),
		[]byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverIn(t *testing.T) {
	root := t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	writeTranscript(t, root, "-real-project", id, []string{
		`{"type":"user","cwd":"/real/project","message":` +
			`{"role":"user","content":"Fix the bug in main.go"}}`,
		`{"type":"assistant","cwd":"/real/project","message":` +
			`{"role":"assistant","content":[{"type":"text","text":"OK"}]}}`,
	})

	found, err := discoverIn(root)
	if err != nil {
		t.Fatalf("discoverIn: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d sessions, want 1", len(found))
	}
	d := found[0]
	if d.SessionID != id {
		t.Fatalf("sessionId = %q", d.SessionID)
	}
	if d.Project != "/real/project" {
		t.Fatalf("project = %q, want /real/project (from cwd)", d.Project)
	}
	if d.Title != "Fix the bug in main.go" {
		t.Fatalf("title = %q", d.Title)
	}
}

func TestDiscoverInSkipsNonTranscripts(t *testing.T) {
	root := t.TempDir()
	// A stray non-UUID .jsonl and a non-.jsonl file must both be ignored.
	writeTranscript(t, root, "-proj", "not-a-uuid", []string{`{"type":"user"}`})
	dir := filepath.Join(root, "-proj")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := discoverIn(root)
	if err != nil {
		t.Fatalf("discoverIn: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found %d, want 0 (stray files must be skipped)", len(found))
	}
}

func TestDiscoverInMissingRoot(t *testing.T) {
	found, err := discoverIn(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if found != nil {
		t.Fatalf("missing root should yield no sessions, got %d", len(found))
	}
}

func TestFirstUserText(t *testing.T) {
	// content as a bare string
	if got := firstUserText(json.RawMessage(`{"content":"hello there"}`)); got != "hello there" {
		t.Fatalf("string content = %q", got)
	}
	// content as an array of blocks
	arr := json.RawMessage(`{"content":[{"type":"image"},{"type":"text","text":"do it"}]}`)
	if got := firstUserText(arr); got != "do it" {
		t.Fatalf("array content = %q", got)
	}
	if got := firstUserText(json.RawMessage(`{}`)); got != "" {
		t.Fatalf("empty content = %q, want \"\"", got)
	}
}

func TestTidyTitle(t *testing.T) {
	if got := tidyTitle("  multi   line\n  text  "); got != "multi line text" {
		t.Fatalf("tidyTitle = %q", got)
	}
	long := tidyTitle(string(make([]byte, 0)) + repeat("a", 250))
	if r := []rune(long); len(r) != 101 { // 100 chars + ellipsis
		t.Fatalf("long title length = %d runes, want 101", len(r))
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
