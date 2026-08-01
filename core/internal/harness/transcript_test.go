package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func event(n int, pad int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"assistant","n":%d,"pad":%q}`,
		n, strings.Repeat("x", pad)))
}

// TestCapTranscriptPassesThroughSmallReplays: the common case must be untouched
// — no notice, no copy of the history.
func TestCapTranscriptPassesThroughSmallReplays(t *testing.T) {
	in := []json.RawMessage{event(0, 4), event(1, 4)}
	out := CapTranscript(in)
	if len(out) != 2 || string(out[0]) != string(in[0]) {
		t.Fatalf("small replay was rewritten: %s", out)
	}
	if len(CapTranscript(nil)) != 0 {
		t.Fatal("nil replay grew events")
	}
}

// TestCapTranscriptBoundsEventCount pins audit F10: an unbounded transcript is
// an unbounded RPC frame. The reply must keep the most RECENT events (what the
// human reopened the thread to read) and announce the omission.
func TestCapTranscriptBoundsEventCount(t *testing.T) {
	in := make([]json.RawMessage, MaxReplayEvents+500)
	for i := range in {
		in[i] = event(i, 4)
	}
	out := CapTranscript(in)
	if len(out) > MaxReplayEvents+1 { // +1 for the notice
		t.Fatalf("replay of %d events returned %d", len(in), len(out))
	}
	var notice map[string]any
	if err := json.Unmarshal(out[0], &notice); err != nil {
		t.Fatal(err)
	}
	if notice["type"] != "_lifecycle" || notice["phase"] != "notice" {
		t.Fatalf("truncation was not announced: %s", out[0])
	}
	// The tail, not the head, is what survived.
	if last := string(out[len(out)-1]); last != string(in[len(in)-1]) {
		t.Fatalf("last event = %s, want the newest event", last)
	}
	if strings.Contains(string(out[1]), `"n":0`) {
		t.Fatal("kept the oldest events instead of the newest")
	}
}

// TestCapTranscriptBoundsBytes: few events can still be a huge frame.
func TestCapTranscriptBoundsBytes(t *testing.T) {
	in := make([]json.RawMessage, 40)
	for i := range in {
		in[i] = event(i, 1<<20) // ~1 MiB each: 40 MiB total
	}
	out := CapTranscript(in)
	total := 0
	for _, e := range out {
		total += len(e)
	}
	if total > MaxReplayBytes+1024 {
		t.Fatalf("replay returned %d bytes, cap is %d", total, MaxReplayBytes)
	}
	if len(out) < 2 {
		t.Fatal("byte cap discarded the whole conversation")
	}
}

// A single event larger than the whole budget must still come back — better one
// oversized card than an empty panel — and must still say so.
func TestCapTranscriptKeepsOneOversizedEvent(t *testing.T) {
	in := []json.RawMessage{event(0, 8), event(1, MaxReplayBytes+16)}
	out := CapTranscript(in)
	if len(out) != 2 {
		t.Fatalf("got %d events, want notice + the one event", len(out))
	}
	if string(out[1]) != string(in[1]) {
		t.Fatal("the newest event was not the one kept")
	}
}
