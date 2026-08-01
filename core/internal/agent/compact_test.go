package agent

import (
	"encoding/json"
	"testing"
)

func newCompactTestThread() (*Thread, *hotCompact) {
	hc := &hotCompact{done: make(chan struct{})}
	t := &Thread{hotCompact: hc}
	return t, hc
}

func TestObserveHotCompactAccumulatesAssistantTextThenDeliversOnResult(t *testing.T) {
	th, hc := newCompactTestThread()

	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "hello"}},
		},
	}))
	select {
	case <-hc.done:
		t.Fatal("done should not fire on an assistant event")
	default:
	}

	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": " world"}},
		},
	}))

	observeHotCompact(th, mustMarshal(t, json.RawMessage(`{"type":"result","subtype":"success"}`)))

	select {
	case <-hc.done:
		if got := hc.text.String(); got != "hello world" {
			t.Errorf("got %q, want %q", got, "hello world")
		}
	default:
		t.Fatal("done should fire on a result event")
	}
}

func TestObserveHotCompactIgnoresNonAssistantBlocks(t *testing.T) {
	th, hc := newCompactTestThread()

	// A user message with tool_result content should not accumulate.
	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "noise"}},
		},
	}))
	// A non-text block on the assistant side should be ignored.
	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "tool_use", "id": "y", "name": "Read"}},
		},
	}))
	observeHotCompact(th, mustMarshal(t, json.RawMessage(`{"type":"result"}`)))

	<-hc.done
	if got := hc.text.String(); got != "" {
		t.Errorf("expected empty accumulated text, got %q", got)
	}
}

func TestObserveHotCompactNoOpWithoutPending(t *testing.T) {
	th := &Thread{}
	// Should not panic on no pending compact.
	observeHotCompact(th, mustMarshal(t, map[string]any{"type": "assistant"}))
	observeHotCompact(th, mustMarshal(t, map[string]any{"type": "result"}))
}

// finish is idempotent: multiple callers can fire it without double-close
// panics, and the first error wins.
func TestHotCompactFinishIsIdempotent(t *testing.T) {
	hc := &hotCompact{done: make(chan struct{})}
	hc.finish(nil)
	hc.finish(errSentinel{}) // would panic on double close if not guarded
	<-hc.done
	if hc.err != nil {
		t.Errorf("first finish(nil) should win; got err=%v", hc.err)
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

// TestObserveHotCompactIgnoresSubagentEvents: with --forward-subagent-text a
// helper session's events are interleaved with the parent's on the same
// stdout, tagged with parent_tool_use_id. If the compaction observer took
// them, a Task spawned during the compaction turn would write its prose into
// the summary AND end the capture on its own `result` — completing the
// compaction with a truncated, foreign summary.
func TestObserveHotCompactIgnoresSubagentEvents(t *testing.T) {
	th, hc := newCompactTestThread()

	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "real summary"}},
		},
	}))
	// A subagent's prose must not be appended...
	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type":               "assistant",
		"parent_tool_use_id": "toolu_child",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": " SUBAGENT NOISE"}},
		},
	}))
	// ...and a subagent's result must not end the parent's capture.
	observeHotCompact(th, mustMarshal(t, map[string]any{
		"type":               "result",
		"subtype":            "success",
		"parent_tool_use_id": "toolu_child",
	}))
	select {
	case <-hc.done:
		t.Fatal("a subagent result must not complete the parent's compaction")
	default:
	}

	// The parent's own result still does, with only the parent's text.
	observeHotCompact(th, mustMarshal(t, json.RawMessage(`{"type":"result","subtype":"success"}`)))
	select {
	case <-hc.done:
	default:
		t.Fatal("the parent's result should complete the compaction")
	}
	if got := hc.text.String(); got != "real summary" {
		t.Errorf("summary = %q, want %q", got, "real summary")
	}
}
