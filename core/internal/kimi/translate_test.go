package kimi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// upd builds a session/update notification's params with the given
// sessionUpdate kind and extra update fields.
func upd(kind string, extra map[string]any) json.RawMessage {
	u := map[string]any{"sessionUpdate": kind}
	for k, v := range extra {
		u[k] = v
	}
	b, _ := json.Marshal(map[string]any{"sessionId": "s1", "update": u})
	return b
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad expected JSON: %v", err)
	}
	return v
}

// assertEvents compares got against the expected JSON documents, in order.
func assertEvents(t *testing.T, got []json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d\n got: %s", len(got), len(want), got)
	}
	for i, w := range want {
		var g any
		if err := json.Unmarshal(got[i], &g); err != nil {
			t.Fatalf("event %d not JSON: %v", i, err)
		}
		if !reflect.DeepEqual(g, mustJSON(t, w)) {
			t.Errorf("event %d mismatch\n got: %s\nwant: %s", i, got[i], w)
		}
	}
}

func TestInitEvent(t *testing.T) {
	tr := newTranslator("session_abc", "kimi-code/k3")
	assertEvents(t, []json.RawMessage{tr.initEvent()},
		`{"type":"system","subtype":"init","session_id":"session_abc","model":"kimi-code/k3"}`)
}

// Text deltas accumulate silently and flush as ONE assistant event when a
// tool_call interrupts the message — the UI appends one card per event, so
// partial snapshots must never be emitted.
func TestTextChunksFlushOnToolCall(t *testing.T) {
	tr := newTranslator("s1", "")
	if ev := tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "Hello "},
	})); len(ev) != 0 {
		t.Fatalf("chunk emitted %d events, want 0", len(ev))
	}
	if ev := tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "world"},
	})); len(ev) != 0 {
		t.Fatalf("chunk emitted %d events, want 0", len(ev))
	}
	got := tr.update(upd("tool_call", map[string]any{
		"toolCallId": "tc1", "title": "Bash", "kind": "execute",
		"status": "pending", "rawInput": map[string]any{"command": "ls"},
	}))
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tc1","name":"Bash","input":{"command":"ls"}}]}}`)
}

// Buffered text flushes ahead of the result event at turn end.
func TestTextFlushAtEndTurn(t *testing.T) {
	tr := newTranslator("s1", "")
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "done"},
	}))
	got := tr.endTurn()
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// Kimi streams the raw input JSON as content snapshots on tool_call_update
// rather than in the tool_call itself. The tool card ships once the snapshot
// parses — exactly once — carrying the parsed input.
func TestStreamedInputSnapshot(t *testing.T) {
	tr := newTranslator("s1", "")
	if ev := tr.update(upd("tool_call", map[string]any{
		"toolCallId": "tc1", "title": "Bash", "kind": "execute", "status": "pending",
	})); len(ev) != 0 {
		t.Fatalf("tool_call without input emitted %d events, want 0", len(ev))
	}
	// A partial (unparseable) snapshot still holds the card back.
	if ev := tr.update(upd("tool_call_update", map[string]any{
		"toolCallId": "tc1", "status": "in_progress",
		"content": []any{map[string]any{"type": "content",
			"content": map[string]any{"type": "text", "text": `{"command`}}},
	})); len(ev) != 0 {
		t.Fatalf("partial snapshot emitted %d events, want 0", len(ev))
	}
	got := tr.update(upd("tool_call_update", map[string]any{
		"toolCallId": "tc1", "status": "in_progress",
		"content": []any{map[string]any{"type": "content",
			"content": map[string]any{"type": "text", "text": `{"command": "ls -la /tmp"}`}}},
	}))
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tc1","name":"Bash","input":{"command":"ls -la /tmp"}}]}}`)
	// Completion emits only the tool_result — no second tool card.
	got = tr.update(upd("tool_call_update", map[string]any{
		"toolCallId": "tc1", "status": "completed", "rawOutput": "file.txt",
	}))
	assertEvents(t, got,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tc1","content":"file.txt"}]}}`)
}

// A failed tool call maps to an is_error tool_result.
func TestToolResultFailed(t *testing.T) {
	tr := newTranslator("s1", "")
	tr.update(upd("tool_call", map[string]any{
		"toolCallId": "tc9", "title": "Bash", "kind": "execute",
		"status": "pending", "rawInput": map[string]any{"command": "false"},
	}))
	got := tr.update(upd("tool_call_update", map[string]any{
		"toolCallId": "tc9", "status": "failed", "rawOutput": "exit 1",
	}))
	assertEvents(t, got,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tc9","content":"exit 1","is_error":true}]}}`)
}

func TestToolNameMapping(t *testing.T) {
	cases := []struct {
		title, kind, want string
	}{
		{"Bash", "execute", "Bash"},
		{"str_replace_editor", "edit", "Edit"},
		{"view", "read", "Read"},
		{"fetch", "fetch", "WebFetch"},
		{"Custom Tool", "other", "Custom Tool"},
		{"Custom Tool", "", "Custom Tool"},
		{"", "other", "Tool"},
	}
	for _, c := range cases {
		if got := toolName(c.title, c.kind); got != c.want {
			t.Errorf("toolName(%q, %q) = %q, want %q", c.title, c.kind, got, c.want)
		}
	}
}

// Updates with no Claude-shaped counterpart are dropped silently.
func TestIgnoredUpdates(t *testing.T) {
	tr := newTranslator("s1", "")
	for _, kind := range []string{
		"agent_thought_chunk", "plan", "config_option_update", "available_commands_update",
	} {
		if ev := tr.update(upd(kind, map[string]any{
			"content": map[string]any{"type": "text", "text": "ignored"},
		})); len(ev) != 0 {
			t.Errorf("%s emitted %d events, want 0", kind, len(ev))
		}
	}
}

// The permission bridge reads the best-known tool name and parsed input for
// its prompt to the human.
func TestToolForPermission(t *testing.T) {
	tr := newTranslator("s1", "")
	tr.update(upd("tool_call", map[string]any{
		"toolCallId": "tc1", "title": "Bash", "kind": "execute", "status": "pending",
	}))
	tr.update(upd("tool_call_update", map[string]any{
		"toolCallId": "tc1", "status": "in_progress",
		"content": []any{map[string]any{"type": "content",
			"content": map[string]any{"type": "text", "text": `{"command": "ls"}`}}},
	}))
	name, input := tr.toolForPermission("tc1")
	if name != "Bash" {
		t.Errorf("name = %q, want Bash", name)
	}
	var parsed map[string]any
	if err := json.Unmarshal(input, &parsed); err != nil || parsed["command"] != "ls" {
		t.Errorf("input = %s, want a JSON object with command=ls", input)
	}
	if name, _ := tr.toolForPermission("unknown"); name != "" {
		t.Errorf("unknown toolCallId gave name %q, want empty", name)
	}
}
