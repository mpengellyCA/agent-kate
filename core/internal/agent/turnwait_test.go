package agent

import (
	"encoding/json"
	"testing"
	"time"
)

func rawEvent(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// A wait on a thread with no turn in flight returns immediately.
func TestTurnWaitIdleImmediately(t *testing.T) {
	tt := NewTurnTracker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, timedOut := tt.Wait("t-idle", 5*time.Second); timedOut {
			t.Error("idle thread reported timeout")
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait blocked on an idle thread")
	}
}

// A queued turn blocks Wait until its result event lands, and the claude-style
// result text is handed back as lastText.
func TestTurnWaitBlocksUntilResult(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-busy")

	got := make(chan string, 1)
	go func() {
		text, timedOut := tt.Wait("t-busy", 5*time.Second)
		if timedOut {
			t.Error("unexpected timeout")
		}
		got <- text
	}()

	// Still busy: the waiter must not have returned yet.
	select {
	case <-got:
		t.Fatal("Wait returned before the result event")
	case <-time.After(50 * time.Millisecond):
	}

	tt.Observe("t-busy", rawEvent(t, map[string]any{
		"type": "result", "subtype": "success", "result": "DONE",
	}))
	select {
	case text := <-got:
		if text != "DONE" {
			t.Fatalf("lastText = %q, want DONE", text)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not wake on the result event")
	}
}

// A kimi-style result carries no text; the last assistant text event stands in.
func TestTurnWaitLastAssistantTextFallback(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-kimi")
	tt.Observe("t-kimi", rawEvent(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "thinking about it"},
		}},
	}))
	tt.Observe("t-kimi", rawEvent(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "KIMIPONG"},
		}},
	}))
	tt.Observe("t-kimi", rawEvent(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
	}))
	text, timedOut := tt.Wait("t-kimi", time.Second)
	if timedOut || text != "KIMIPONG" {
		t.Fatalf("Wait = %q/%v, want KIMIPONG/false", text, timedOut)
	}
}

// The timeout path: a turn that never completes reports timedOut.
func TestTurnWaitTimeout(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-hung")
	start := time.Now()
	_, timedOut := tt.Wait("t-hung", 100*time.Millisecond)
	if !timedOut {
		t.Fatal("expected timeout on a hung turn")
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("Wait returned before its deadline")
	}
}

// A terminal lifecycle phase unblocks waiters even when the result never
// arrives (process died mid-turn), and TurnFailed undoes a failed send.
func TestTurnWaitUnblocksOnThreadEnd(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-dead")
	got := make(chan bool, 1)
	go func() {
		_, timedOut := tt.Wait("t-dead", 5*time.Second)
		got <- timedOut
	}()
	tt.Observe("t-dead", rawEvent(t, map[string]any{
		"type": "_lifecycle", "phase": "exited", "detail": "exited cleanly",
	}))
	select {
	case timedOut := <-got:
		if timedOut {
			t.Fatal("thread end must not count as a timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not wake on the exited lifecycle")
	}

	tt.TurnQueued("t-failed")
	tt.TurnFailed("t-failed")
	if _, timedOut := tt.Wait("t-failed", 50*time.Millisecond); timedOut {
		t.Fatal("TurnFailed must leave the thread idle")
	}
}
