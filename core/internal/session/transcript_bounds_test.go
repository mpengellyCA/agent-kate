package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentkate/internal/harness"
)

// writeFakeTranscript lays down a Claude Code transcript with n events, each
// padded to about pad bytes, and returns the session id.
func writeFakeTranscript(t *testing.T, home string, n, pad int) string {
	t.Helper()
	const sid = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	dir := filepath.Join(home, "projects", "-work-proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, sid+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < n; i++ {
		line, err := json.Marshal(map[string]any{
			"type": "user",
			"n":    i,
			"pad":  strings.Repeat("x", pad),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	return sid
}

func eventIndex(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var probe struct {
		N *int `json:"n"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.N == nil {
		return -1
	}
	return *probe.N
}

// The replay must be bounded by EVENT COUNT, keeping the tail, with a visible
// notice standing in for what was dropped. Before this the whole file was read
// into core memory and only capped afterwards — the freeze audit F10 describes.
func TestReadTranscriptCapsEventCount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	sid := writeFakeTranscript(t, home, harness.MaxReplayEvents+500, 16)

	events, err := ReadTranscript(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) > harness.MaxReplayEvents {
		t.Fatalf("returned %d events, cap is %d", len(events), harness.MaxReplayEvents)
	}
	// First event is the truncation notice; the rest are the TAIL.
	var notice struct {
		Type  string `json:"type"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(events[0], &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Type != "_lifecycle" || notice.Phase != "notice" {
		t.Fatalf("first event is not a truncation notice: %s", events[0])
	}
	if got := eventIndex(t, events[len(events)-1]); got != harness.MaxReplayEvents+499 {
		t.Fatalf("last replayed event is %d, want the newest (%d)",
			got, harness.MaxReplayEvents+499)
	}
	// CapTranscript must find nothing left to do — the reader already fits.
	if capped := harness.CapTranscript(events); len(capped) != len(events) {
		t.Fatalf("handler-side cap re-truncated to %d; the reader's budget is wrong",
			len(capped))
	}
}

// The byte cap must bind even when the event count is small — a handful of
// enormous tool results is the realistic shape.
func TestReadTranscriptCapsBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	// 40 events x 512 KiB = 20 MiB on disk, well past the 8 MiB budget.
	sid := writeFakeTranscript(t, home, 40, 512*1024)

	events, err := ReadTranscript(sid)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range events {
		total += len(e)
	}
	if total > harness.MaxReplayBytes {
		t.Fatalf("returned %d bytes, cap is %d", total, harness.MaxReplayBytes)
	}
	if len(events) < 2 {
		t.Fatalf("expected a notice plus some tail, got %d events", len(events))
	}
	if got := eventIndex(t, events[len(events)-1]); got != 39 {
		t.Fatalf("last replayed event is %d, want the newest (39)", got)
	}
}

// A short transcript must come back byte-identical and notice-free.
func TestReadTranscriptShortIsUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	sid := writeFakeTranscript(t, home, 3, 8)

	events, err := ReadTranscript(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, e := range events {
		if got := eventIndex(t, e); got != i {
			t.Fatalf("event %d is %d; order or content changed", i, got)
		}
	}
}

// A torn last line (the CLI killed mid-append) must be skipped, not relayed to
// the UI as a parse error in the panel.
func TestReadTranscriptSkipsTornLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	sid := writeFakeTranscript(t, home, 2, 8)
	path := filepath.Join(home, "projects", "-work-proj", sid+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"user\",\"pa\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err := ReadTranscript(sid)
	if err != nil {
		t.Fatalf("a torn line must not fail the read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want the 2 whole ones", len(events))
	}
}

// The archive is bounded by record count AND bytes, keeping the newest — audit
// F10's "threads-archive.json grows forever".
func TestWriteArchiveCapsRecordCount(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	list := make([]ArchiveRecord, 0, maxArchiveRecords+50)
	for i := 0; i < maxArchiveRecords+50; i++ {
		list = append(list, ArchiveRecord{
			Record:     Record{ThreadID: fmt.Sprintf("t-%04d", i)},
			ArchivedAt: base.Add(time.Duration(i) * time.Minute),
			Reason:     "test",
		})
	}
	if err := s.writeArchive(list); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadArchive()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxArchiveRecords {
		t.Fatalf("archive kept %d records, cap is %d", len(got), maxArchiveRecords)
	}
	// Newest-first, and the newest of all survived.
	if got[0].ThreadID != fmt.Sprintf("t-%04d", maxArchiveRecords+49) {
		t.Fatalf("newest record is %s; the cap dropped the wrong end", got[0].ThreadID)
	}
}

func TestWriteArchiveCapsBytes(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 60 records x ~256 KiB of system prompt = ~15 MiB, past the 4 MiB cap and
	// well under the record cap, so only the byte cap can save us.
	base := time.Now().UTC()
	var list []ArchiveRecord
	for i := 0; i < 60; i++ {
		list = append(list, ArchiveRecord{
			Record: Record{
				ThreadID:     fmt.Sprintf("t-%02d", i),
				SystemPrompt: strings.Repeat("p", 256*1024),
			},
			ArchivedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := s.writeArchive(list); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(s.archivePath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxArchiveBytes {
		t.Fatalf("archive file is %d bytes, cap is %d", st.Size(), maxArchiveBytes)
	}
	got, err := s.loadArchive()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("byte cap emptied the archive; it must always keep the newest")
	}
	if got[0].ThreadID != "t-59" {
		t.Fatalf("newest record is %s, want t-59", got[0].ThreadID)
	}
}

// The newest record must survive even when it alone exceeds the byte cap:
// Archive drops the live record once the write succeeds, so an archive that
// silently kept nothing would lose the thread outright.
func TestWriteArchiveAlwaysKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	list := []ArchiveRecord{{
		Record: Record{
			ThreadID:     "t-huge",
			SystemPrompt: strings.Repeat("p", maxArchiveBytes+1024),
		},
		ArchivedAt: time.Now().UTC(),
	}}
	if err := s.writeArchive(list); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadArchive()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ThreadID != "t-huge" {
		t.Fatalf("oversized newest record was dropped: %+v", got)
	}
}
