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
	tr := newTranslator("session_abc", "kimi-code/k3", nil)
	assertEvents(t, []json.RawMessage{tr.initEvent()},
		`{"type":"system","subtype":"init","session_id":"session_abc","model":"kimi-code/k3"}`)
}

// The handshake's config options (model / thinking / mode enumerations) ride
// into the init event so the UI can populate real pickers.
func TestInitEventCarriesConfigOptions(t *testing.T) {
	tr := newTranslator("s1", "kimi-code/k3", []ConfigOption{{
		ID: "model", Name: "Model", Category: "model", CurrentValue: "kimi-code/k3",
		Options: []ConfigOptionValue{
			{Value: "kimi-code/k3", Name: "K3"},
			{Value: "kimi-code/kimi-for-coding", Name: "K2.7 Coding"},
		},
	}, {
		ID: "mode", Name: "Mode", Category: "mode", CurrentValue: "default",
		Options: []ConfigOptionValue{
			{Value: "default", Name: "Default", Description: "Manual approvals."},
			{Value: "yolo", Name: "YOLO", Description: "Auto-approve tools."},
		},
	}})
	assertEvents(t, []json.RawMessage{tr.initEvent()},
		`{"type":"system","subtype":"init","session_id":"s1","model":"kimi-code/k3",
		  "configOptions":[
		   {"id":"model","name":"Model","category":"model","currentValue":"kimi-code/k3",
		    "options":[{"value":"kimi-code/k3","name":"K3"},
		               {"value":"kimi-code/kimi-for-coding","name":"K2.7 Coding"}]},
		   {"id":"mode","name":"Mode","category":"mode","currentValue":"default",
		    "options":[{"value":"default","name":"Default","description":"Manual approvals."},
		               {"value":"yolo","name":"YOLO","description":"Auto-approve tools."}]}]}`)
}

// Text deltas accumulate silently and flush as ONE assistant event when a
// tool_call interrupts the message — the UI appends one card per event, so
// partial snapshots must never be emitted.
func TestTextChunksFlushOnToolCall(t *testing.T) {
	tr := newTranslator("s1", "", nil)
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
	tr := newTranslator("s1", "", nil)
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
	tr := newTranslator("s1", "", nil)
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
	tr := newTranslator("s1", "", nil)
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
	tr := newTranslator("s1", "", nil)
	for _, kind := range []string{"config_option_update"} {
		if ev := tr.update(upd(kind, map[string]any{
			"content": map[string]any{"type": "text", "text": "ignored"},
		})); len(ev) != 0 {
			t.Errorf("%s emitted %d events, want 0", kind, len(ev))
		}
	}
	// An empty plan update carries nothing to render either.
	if ev := tr.update(upd("plan", map[string]any{"entries": []any{}})); len(ev) != 0 {
		t.Errorf("empty plan emitted %d events, want 0", len(ev))
	}
}

// The CLI's slash-command list ships as the synthetic _commands event that
// feeds the composer's autocomplete.
func TestAvailableCommandsBecomeCommandsEvent(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	got := tr.update(upd("available_commands_update", map[string]any{
		"availableCommands": []any{
			map[string]any{"name": "init", "description": "Create AGENTS.md"},
			map[string]any{"name": "compact", "description": "Compact the session"},
		},
	}))
	assertEvents(t, got,
		`{"type":"_commands","commands":[
		  {"name":"init","description":"Create AGENTS.md"},
		  {"name":"compact","description":"Compact the session"}]}`)
}

// Thought deltas accumulate silently and flush as ONE thinking-block assistant
// event when the visible message resumes — reasoning reads before the text it
// led to, exactly one card each.
func TestThoughtChunksFlushBeforeText(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	for _, chunk := range []string{"Let me think", " about this."} {
		if ev := tr.update(upd("agent_thought_chunk", map[string]any{
			"content": map[string]any{"type": "text", "text": chunk},
		})); len(ev) != 0 {
			t.Fatalf("thought chunk emitted %d events, want 0", len(ev))
		}
	}
	got := tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "Answer."},
	}))
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think about this."}]}}`)
	got = tr.endTurn()
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Answer."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// A thought followed directly by a tool call (no visible text in between)
// flushes ahead of the tool card; a thought pending at turn end flushes ahead
// of the result.
func TestThoughtFlushOnToolCallAndEndTurn(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	tr.update(upd("agent_thought_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "check the dir"},
	}))
	got := tr.update(upd("tool_call", map[string]any{
		"toolCallId": "tc1", "title": "Bash", "kind": "execute",
		"status": "pending", "rawInput": map[string]any{"command": "ls"},
	}))
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"check the dir"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tc1","name":"Bash","input":{"command":"ls"}}]}}`)

	tr.update(upd("agent_thought_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "all done"},
	}))
	got = tr.endTurn()
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"all done"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// An ACP plan update becomes the TodoWrite tool_use shape the Claude harness
// produces, so both backends feed the one checklist card in the UI.
func TestPlanBecomesTodoWrite(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	got := tr.update(upd("plan", map[string]any{
		"entries": []any{
			map[string]any{"content": "Read the code", "priority": "high", "status": "completed"},
			map[string]any{"content": "Fix the bug", "priority": "high", "status": "in_progress"},
			map[string]any{"content": "Add a test", "priority": "medium", "status": "pending"},
		},
	}))
	assertEvents(t, got,
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"tool_use","id":"acp-plan","name":"TodoWrite","input":{"todos":[
		   {"content":"Read the code","status":"completed"},
		   {"content":"Fix the bug","status":"in_progress"},
		   {"content":"Add a test","status":"pending"}]}}]}}`)
}

// A tool result carrying image blocks passes them through as Claude-shaped
// base64 image blocks (the UI renders thumbnail chips); text-only results
// keep the plain-string shape.
func TestToolResultImagesPassThrough(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	tr.update(upd("tool_call", map[string]any{
		"toolCallId": "tc1", "title": "screenshot", "kind": "other",
		"status": "pending", "rawInput": map[string]any{},
	}))
	got := tr.update(upd("tool_call_update", map[string]any{
		"toolCallId": "tc1", "status": "completed",
		"content": []any{
			map[string]any{"type": "content", "content": map[string]any{
				"type": "text", "text": "captured"}},
			map[string]any{"type": "content", "content": map[string]any{
				"type": "image", "data": "aGk=", "mimeType": "image/png"}},
		},
	}))
	assertEvents(t, got,
		`{"type":"user","message":{"role":"user","content":[
		  {"type":"tool_result","tool_use_id":"tc1","content":[
		   {"type":"text","text":"captured"},
		   {"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}}`)
}

// The permission bridge reads the best-known tool name and parsed input for
// its prompt to the human.
func TestToolForPermission(t *testing.T) {
	tr := newTranslator("s1", "", nil)
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
