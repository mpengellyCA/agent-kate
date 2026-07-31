package agent

import (
	"context"
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
		if _, timedOut := tt.Wait(context.Background(), "t-idle", 5*time.Second); timedOut {
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
		text, timedOut := tt.Wait(context.Background(), "t-busy", 5*time.Second)
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
	text, timedOut := tt.Wait(context.Background(), "t-kimi", time.Second)
	if timedOut || text != "KIMIPONG" {
		t.Fatalf("Wait = %q/%v, want KIMIPONG/false", text, timedOut)
	}
}

// The timeout path: a turn that never completes reports timedOut.
func TestTurnWaitTimeout(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-hung")
	start := time.Now()
	_, timedOut := tt.Wait(context.Background(), "t-hung", 100*time.Millisecond)
	if !timedOut {
		t.Fatal("expected timeout on a hung turn")
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("Wait returned before its deadline")
	}
}

// An in-band interrupt during a wait: the aborted turn still ends with a
// result (followed by a turn_aborted lifecycle, which is not terminal), and
// the waiter wakes on it rather than parking until the timeout.
func TestTurnWaitInterruptAbortedTurn(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-int")
	got := make(chan bool, 1)
	go func() {
		_, timedOut := tt.Wait(context.Background(), "t-int", 5*time.Second)
		got <- timedOut
	}()
	time.Sleep(20 * time.Millisecond)
	// The abort sequence the supervisors emit: the aborted turn's result,
	// then the process-stays-resident turn_aborted note.
	tt.Observe("t-int", rawEvent(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
	}))
	tt.Observe("t-int", rawEvent(t, map[string]any{
		"type": "_lifecycle", "phase": "turn_aborted",
		"detail": "interrupted — session kept",
	}))
	select {
	case timedOut := <-got:
		if timedOut {
			t.Fatal("waiter timed out instead of waking on the aborted turn's result")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not wake on the aborted turn's result")
	}
	// turn_aborted is NOT terminal: the thread must still be waitable.
	tt.TurnQueued("t-int")
	if _, timedOut := tt.Wait(context.Background(), "t-int", 50*time.Millisecond); !timedOut {
		t.Fatal("turn_aborted must not mark the thread ended")
	}
}

// Multi-turn counting: two queued turns need two results before idle.
func TestTurnWaitMultiTurnCounting(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-multi")
	tt.TurnQueued("t-multi")
	tt.Observe("t-multi", rawEvent(t, map[string]any{
		"type": "result", "subtype": "success",
	}))
	if _, timedOut := tt.Wait(context.Background(), "t-multi", 50*time.Millisecond); !timedOut {
		t.Fatal("one result must not drain two queued turns")
	}
	tt.Observe("t-multi", rawEvent(t, map[string]any{
		"type": "result", "subtype": "success",
	}))
	if _, timedOut := tt.Wait(context.Background(), "t-multi", time.Second); timedOut {
		t.Fatal("second result should have made the thread idle")
	}
}

// Context cancellation releases a waiter mid-wait (a disconnected bridge must
// not park its handler goroutine until the full timeout).
func TestTurnWaitContextCancel(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-gone")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan bool, 1)
	go func() {
		_, timedOut := tt.Wait(ctx, "t-gone", time.Hour)
		got <- timedOut
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case timedOut := <-got:
		if !timedOut {
			t.Fatal("cancelled wait must report it gave up before idle")
		}
	case <-time.After(time.Second):
		t.Fatal("Wait ignored context cancellation")
	}
}

// Regression for the seeded-resume gap: resumeThread queues the summary
// prompt's turn (agents.go), so a wait racing the resume blocks until the
// acknowledgement turn's result instead of reporting a false idle.
func TestTurnWaitSeededResumeQueuedTurn(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-seed") // what resumeThread does before Launch
	if _, timedOut := tt.Wait(context.Background(), "t-seed", 50*time.Millisecond); !timedOut {
		t.Fatal("seeded resume must count as busy until its result lands")
	}
	tt.Observe("t-seed", rawEvent(t, map[string]any{
		"type": "result", "subtype": "success", "result": "acknowledged",
	}))
	text, timedOut := tt.Wait(context.Background(), "t-seed", time.Second)
	if timedOut || text != "acknowledged" {
		t.Fatalf("Wait = %q/%v after the seeded turn's result", text, timedOut)
	}
}

// A terminal lifecycle phase unblocks waiters even when the result never
// arrives (process died mid-turn), and TurnFailed undoes a failed send.
func TestTurnWaitUnblocksOnThreadEnd(t *testing.T) {
	tt := NewTurnTracker()
	tt.TurnQueued("t-dead")
	got := make(chan bool, 1)
	go func() {
		_, timedOut := tt.Wait(context.Background(), "t-dead", 5*time.Second)
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
	if _, timedOut := tt.Wait(context.Background(), "t-failed", 50*time.Millisecond); timedOut {
		t.Fatal("TurnFailed must leave the thread idle")
	}
}
