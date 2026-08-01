package kimi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentkate/internal/harness"
)

func writeLog(t *testing.T, dir, threadID string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, threadID+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadTranscriptIsBounded pins audit F10 on the kimi side: however long the
// event log has grown, a replay must return a bounded number of events — the
// newest ones — and say that it dropped the rest.
func TestReadTranscriptIsBounded(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, harness.MaxReplayEvents+1000)
	for i := range lines {
		lines[i] = fmt.Sprintf(`{"type":"assistant","n":%d}`, i)
	}
	writeLog(t, dir, "t1", lines)

	out, err := ReadTranscript(dir, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > harness.MaxReplayEvents {
		t.Fatalf("returned %d events, cap is %d", len(out), harness.MaxReplayEvents)
	}
	var notice map[string]any
	if err := json.Unmarshal(out[0], &notice); err != nil {
		t.Fatal(err)
	}
	if notice["phase"] != "notice" {
		t.Fatalf("truncation not announced: %s", out[0])
	}
	if want := lines[len(lines)-1]; string(out[len(out)-1]) != want {
		t.Fatalf("last event = %s, want the newest (%s)", out[len(out)-1], want)
	}
	// Handler-side capping must then be a no-op — one notice, not two.
	if capped := harness.CapTranscript(out); len(capped) != len(out) {
		t.Fatalf("CapTranscript re-trimmed a bounded kimi replay (%d → %d)",
			len(out), len(capped))
	}
}

// TestReadTranscriptSkipsTornTail pins the crash-tail item: the log is appended
// without fsync, so a crash can leave a half-written last line. It must be
// dropped here, not relayed to the UI as an event it cannot parse.
func TestReadTranscriptSkipsTornTail(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "t2", []string{
		`{"type":"assistant","n":1}`,
		`{"type":"assistant","n":2}`,
		`{"type":"assist`, // torn by a crash mid-write
	})
	out, err := ReadTranscript(dir, "t2")
	if err != nil {
		t.Fatalf("a torn tail must not fail the whole replay: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events, want the 2 intact ones: %s", len(out), out)
	}
	for _, ev := range out {
		if !json.Valid(ev) {
			t.Fatalf("relayed an unparseable event: %s", ev)
		}
	}
}

// No log at all is not an error — a thread that never ran has no history.
func TestReadTranscriptMissingLog(t *testing.T) {
	out, err := ReadTranscript(t.TempDir(), "nope")
	if err != nil || out != nil {
		t.Fatalf("out=%v err=%v, want nil/nil", out, err)
	}
}

// TestEventLogRetention pins the second half of audit F10: the per-thread event
// log is append-only and a resume appends to the SAME file, so without a
// retention policy it grows until the disk does. Past the cap it must be
// trimmed to its recent tail, on a line boundary, with the log still usable
// afterwards.
func TestEventLogRetention(t *testing.T) {
	dir := t.TempDir()
	sup := NewSupervisor("kimi", testLogger(), func(string, []json.RawMessage) {}, nil, dir)
	th := &Thread{ID: "t-retain"}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := sup.eventLogPath(th.ID)
	// Pre-fill past the cap with whole lines.
	line := fmt.Sprintf(`{"type":"assistant","pad":%q}`, strings.Repeat("x", 4096)) + "\n"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var written int64
	for written <= maxEventLogBytes {
		n, err := f.WriteString(line)
		if err != nil {
			t.Fatal(err)
		}
		written += int64(n)
	}
	f.Close()

	lf, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	th.logFile = lf
	th.logBytes = written
	t.Cleanup(func() {
		if th.logFile != nil {
			th.logFile.Close()
		}
	})

	th.mu.Lock()
	sup.trimEventLogLocked(th)
	th.mu.Unlock()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > trimEventLogTo {
		t.Fatalf("log is %d bytes after the trim, want <= %d", st.Size(), trimEventLogTo)
	}
	if st.Size() == 0 {
		t.Fatal("the trim emptied the transcript")
	}
	if th.logBytes != st.Size() {
		t.Fatalf("logBytes = %d, on-disk size = %d", th.logBytes, st.Size())
	}
	if _, err := os.Lstat(path + ".trim"); !os.IsNotExist(err) {
		t.Fatalf("staging file left behind: %v", err)
	}
	// Every surviving line is a whole event, and the log still accepts writes.
	events, err := ReadTranscript(dir, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("nothing replayable survived the trim")
	}
	for _, ev := range events {
		if !json.Valid(ev) {
			t.Fatalf("trim left a partial line: %s", ev)
		}
	}
	if th.logFile == nil {
		t.Fatal("the log was not reopened after the trim")
	}
	if _, err := th.logFile.WriteString(line); err != nil {
		t.Fatalf("log not writable after the trim: %v", err)
	}
}

// TestDeleteTranscript pins the OTHER half of audit F10's retention gap: the
// per-thread event log outlived the thread it belonged to. Nothing deleted it
// on discard and nothing deleted it on cleanup, so the data directory kept a
// full transcript of every thread the user had ever destroyed.
func TestDeleteTranscript(t *testing.T) {
	dir := t.TempDir()
	sup := NewSupervisor("kimi", testLogger(), func(string, []json.RawMessage) {}, nil, dir)

	path := sup.eventLogPath("t-gone")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A crashed trim leaves this beside the log; it must go too.
	if err := os.WriteFile(path+".trim", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sup.DeleteTranscript("t-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("event log survived the delete: %v", err)
	}
	if _, err := os.Stat(path + ".trim"); !os.IsNotExist(err) {
		t.Fatal("orphaned trim file survived the delete")
	}

	// Idempotent: the caller is asking for the log to be gone, and a second
	// discard (or a thread that never logged) must not fail the teardown.
	if err := sup.DeleteTranscript("t-gone"); err != nil {
		t.Fatalf("deleting an absent log must succeed: %v", err)
	}
	if err := sup.DeleteTranscript(""); err != nil {
		t.Fatalf("empty thread id must be a no-op: %v", err)
	}

	// A sibling thread's log is untouched.
	other := sup.eventLogPath("t-keep")
	if err := os.WriteFile(other, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sup.DeleteTranscript("t-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("deleting one thread's log removed another's: %v", err)
	}
}
