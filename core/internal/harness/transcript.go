package harness

import (
	"encoding/json"
	"fmt"
)

// Replay caps. A dormant thread's transcript is served as ONE JSON-RPC result,
// so an unbounded transcript is an unbounded frame: the core holds it, the UI
// buffers it, and a months-old thread costs both processes hundreds of MB in a
// single message — freeze, then OOM (audit F10). The inbound frame cap is 16
// MiB on each side, so a bigger reply is not merely slow, it is undeliverable.
//
// The event cap is the more important of the two. It is set below the UI's own
// transcript row cap (5000 rows), so nothing is lost that the UI would have
// kept anyway: the oldest events are exactly the ones its ring drops.
const (
	MaxReplayEvents = 4000
	MaxReplayBytes  = 8 << 20 // 8 MiB, half the frame cap, leaving room for sidecars
)

// TruncationNotice is the card the human sees in place of the events that were
// dropped. It is a `_lifecycle`/`notice` event — the wire's existing "something
// happened that changes no panel state" shape — because a silently shortened
// history reads as data loss, and the human deserves to know the conversation
// starts mid-stream.
func TruncationNotice(omittedEvents int) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"type":  "_lifecycle",
		"phase": "notice",
		"detail": fmt.Sprintf(
			"earlier history not replayed: %d older events omitted (the full "+
				"transcript is still on disk, and the agent's own context is unaffected)",
			omittedEvents),
	})
	if err != nil { // marshalling a literal map cannot fail; be explicit anyway
		return nil
	}
	return b
}

// CapTranscript bounds a replay to the most recent events within both caps,
// prepending a TruncationNotice when anything was dropped. The TAIL is kept:
// the recent turns are what the human reopened the thread to see.
//
// Returns events unchanged when it already fits, so a harness that has already
// bounded its own read (kimi does, at the file) passes through untouched.
func CapTranscript(events []json.RawMessage) []json.RawMessage {
	if len(events) == 0 {
		return events
	}
	// Walk backwards accumulating until either cap would be exceeded; keep at
	// least one event so a single oversized event still says something.
	total := 0
	first := len(events)
	for i := len(events) - 1; i >= 0; i-- {
		next := total + len(events[i]) + 1
		if first < len(events) && (next > MaxReplayBytes || len(events)-i > MaxReplayEvents) {
			break
		}
		total = next
		first = i
	}
	if first == 0 {
		return events
	}
	out := make([]json.RawMessage, 0, len(events)-first+1)
	if notice := TruncationNotice(first); notice != nil {
		out = append(out, notice)
	}
	return append(out, events[first:]...)
}
