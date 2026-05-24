package agent

import (
	"encoding/json"
	"testing"
)

func newCompactTestThread() (*Thread, *hotCompact) {
	hc := &hotCompact{done: make(chan string, 1)}
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
	case got := <-hc.done:
		if got != "hello world" {
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

	got := <-hc.done
	if got != "" {
		t.Errorf("expected empty accumulated text, got %q", got)
	}
}

func TestObserveHotCompactNoOpWithoutPending(t *testing.T) {
	th := &Thread{}
	// Should not panic on no pending compact.
	observeHotCompact(th, mustMarshal(t, map[string]any{"type": "assistant"}))
	observeHotCompact(th, mustMarshal(t, map[string]any{"type": "result"}))
}
