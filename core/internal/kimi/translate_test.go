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

// --- config_option_update (the stale-mode bug) --------------------------

// Kimi announces every mid-session option change — its own ExitPlanMode flip
// included — and the translator turns that into an `_options` event carrying
// the FULL set with the new current value, so a consumer reads one shape for
// both the vocabulary and what is running now.
func TestConfigOptionUpdateEmitsOptionsEvent(t *testing.T) {
	tr := newTranslator("s1", "kimi-code/k3", []ConfigOption{{
		ID: "mode", Name: "Mode", CurrentValue: "plan",
		Options: []ConfigOptionValue{
			{Value: "plan", Name: "Plan"},
			{Value: "default", Name: "Default"},
		},
	}})
	assertEvents(t, tr.update(upd("config_option_update", map[string]any{
		"configId": "mode", "value": "default",
	})),
		`{"type":"_options","session_id":"s1","changed":["mode"],
		  "configOptions":[{"id":"mode","name":"Mode","currentValue":"default",
		   "options":[{"value":"plan","name":"Plan"},
		              {"value":"default","name":"Default"}]}]}`)
	if got := tr.configValue("mode"); got != "default" {
		t.Errorf("configValue(mode) = %q, want default", got)
	}
}

// The notification's other spellings — a single configOption object, or the
// whole set — carry the same meaning and must translate identically.
func TestConfigOptionUpdateShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"flat id/currentValue", map[string]any{"id": "mode", "currentValue": "yolo"}},
		{"single object", map[string]any{
			"configOption": map[string]any{"id": "mode", "currentValue": "yolo"}}},
		{"full set", map[string]any{
			"configOptions": []any{map[string]any{"id": "mode", "currentValue": "yolo"}}}},
		{"nested config", map[string]any{
			"config": []any{map[string]any{"id": "mode", "currentValue": "yolo"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTranslator("s1", "", []ConfigOption{{ID: "mode", CurrentValue: "plan"}})
			events := tr.update(upd("config_option_update", tc.extra))
			if len(events) != 1 {
				t.Fatalf("event count = %d, want 1: %s", len(events), events)
			}
			if got := tr.configValue("mode"); got != "yolo" {
				t.Errorf("configValue(mode) = %q, want yolo", got)
			}
		})
	}
}

// An update the translator cannot make sense of must change nothing and emit
// nothing — a wrong current value is worse than a stale one.
func TestConfigOptionUpdateUnrecognisedIsDropped(t *testing.T) {
	tr := newTranslator("s1", "", []ConfigOption{{ID: "mode", CurrentValue: "plan"}})
	if events := tr.update(upd("config_option_update", map[string]any{
		"somethingElse": true,
	})); len(events) != 0 {
		t.Fatalf("events = %s, want none", events)
	}
	if got := tr.configValue("mode"); got != "plan" {
		t.Errorf("configValue(mode) = %q, want plan (unchanged)", got)
	}
}

// An option the handshake never listed still arrives with its enumeration
// intact, so a picker can offer it.
func TestConfigOptionUpdateAddsUnknownOption(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	assertEvents(t, tr.update(upd("config_option_update", map[string]any{
		"configOption": map[string]any{
			"id": "thinking", "name": "Thinking", "currentValue": "high",
			"options": []any{map[string]any{"value": "high", "name": "High"}},
		},
	})),
		`{"type":"_options","session_id":"s1","changed":["thinking"],
		  "configOptions":[{"id":"thinking","name":"Thinking","currentValue":"high",
		   "options":[{"value":"high","name":"High"}]}]}`)
}

// --- session/set_config_option's authoritative response ------------------

// The response to a set_config_option is what the CLI actually applied, which
// need not be what was asked for. Folding it in produces the same `_options`
// event the notification does, so both routes converge on one shape.
func TestSetConfigOptionResponseAppliesAuthoritativeValue(t *testing.T) {
	tr := newTranslator("s1", "", []ConfigOption{{ID: "thinking", CurrentValue: "low"}})
	// Asked for "max"; the CLI reports it settled on "high".
	opts := decodeConfigOptions(json.RawMessage(
		`{"configOptions":[{"id":"thinking","currentValue":"high"}]}`))
	assertEvents(t, []json.RawMessage{tr.applyConfigOptions(opts)},
		`{"type":"_options","session_id":"s1","changed":["thinking"],
		  "configOptions":[{"id":"thinking","name":"","currentValue":"high"}]}`)
	if got := tr.configValue("thinking"); got != "high" {
		t.Errorf("configValue(thinking) = %q, want high", got)
	}
}

// A response that says nothing (kimi answers some sets with an empty result)
// leaves the recorded value alone and announces nothing.
func TestSetConfigOptionEmptyResponseChangesNothing(t *testing.T) {
	tr := newTranslator("s1", "", []ConfigOption{{ID: "mode", CurrentValue: "default"}})
	for _, raw := range []string{`{}`, `{"configOptions":[]}`, `null`, ``} {
		if opts := decodeConfigOptions(json.RawMessage(raw)); len(opts) != 0 {
			t.Errorf("decodeConfigOptions(%q) = %+v, want none", raw, opts)
		}
	}
	if ev := tr.applyConfigOptions(nil); ev != nil {
		t.Errorf("event = %s, want none", ev)
	}
	if got := tr.configValue("mode"); got != "default" {
		t.Errorf("configValue(mode) = %q, want default", got)
	}
}

// --- slash-command argument hints ---------------------------------------

// The ACP command list carries an argument hint per command; it rides into the
// `_commands` payload as `hint` so the composer can show `/compact <what>`
// instead of a bare name. Commands without one carry no field at all.
func TestAvailableCommandsCarryArgumentHints(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	assertEvents(t, tr.update(upd("available_commands_update", map[string]any{
		"availableCommands": []any{
			map[string]any{"name": "compact", "description": "Compact the context",
				"input": map[string]any{"hint": "<instructions>"}},
			map[string]any{"name": "usage", "description": "Show token usage",
				"input": "<none>"},
			map[string]any{"name": "clear", "description": "Clear the session"},
		},
	})),
		`{"type":"_commands","commands":[
		  {"name":"compact","description":"Compact the context","hint":"<instructions>"},
		  {"name":"usage","description":"Show token usage","hint":"<none>"},
		  {"name":"clear","description":"Clear the session"}]}`)
}

// --- context usage ------------------------------------------------------

// The `/usage` readout reaches the UI as a `_context` event — the same event
// shape claude's get_context_usage control request produces, which is the one
// the UI's context meter consumes. The result event carries only the model's
// context-window SIZE.
//
// It must never carry `usage.input_tokens` / `output_tokens` (audit F19b):
// kimi's readout is a cumulative context snapshot, and the UI accumulates those
// fields into the session total, so presenting the snapshot in per-turn field
// names invents token spend that never happened. This test is the regression
// gate on that — if the result event grows a `usage` block again, kimi threads
// start reporting fictional billing.
func TestResultEventCarriesNoPerTurnUsage(t *testing.T) {
	tr := newTranslator("s1", "kimi-code/k3", nil)
	assertEvents(t, []json.RawMessage{tr.setUsage(usageInfo{
		PromptTokens: 12345, OutputTokens: 800, ContextWindow: 256000})},
		`{"type":"_context","usedTokens":12345,"maxTokens":256000}`)
	assertEvents(t, tr.endTurn(),
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1",
		  "modelUsage":{"kimi-code/k3":{"contextWindow":256000}}}`)

	// Three turns at a growing context fill: the wire must show no per-turn
	// token counts at all, so nothing can be summed into a session total.
	for _, fill := range []int64{20000, 40000, 60000} {
		tr.setUsage(usageInfo{PromptTokens: fill, ContextWindow: 256000})
		for _, ev := range tr.endTurn() {
			var m map[string]any
			if err := json.Unmarshal(ev, &m); err != nil {
				t.Fatal(err)
			}
			if _, ok := m["usage"]; ok {
				t.Fatalf("result event carries a per-turn usage block: %s", ev)
			}
			mu, _ := m["modelUsage"].(map[string]any)
			for model, v := range mu {
				per, _ := v.(map[string]any)
				for _, k := range []string{"inputTokens", "outputTokens",
					"cacheReadInputTokens", "cacheCreationInputTokens"} {
					if _, ok := per[k]; ok {
						t.Fatalf("modelUsage[%s] carries per-turn field %q: %s", model, k, ev)
					}
				}
			}
		}
	}
}

// A readout with no window (a `/usage` layout we only half-recognised) puts
// nothing on the result event: a context window is the one number there, and a
// missing one is reported as missing rather than guessed.
func TestResultEventWithoutContextWindow(t *testing.T) {
	tr := newTranslator("s1", "kimi-code/k3", nil)
	tr.setUsage(usageInfo{PromptTokens: 5000})
	assertEvents(t, tr.endTurn(),
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// Before any readout exists, the result event stays exactly as it was.
func TestResultEventWithoutUsage(t *testing.T) {
	tr := newTranslator("s1", "kimi-code/k3", nil)
	if ev := tr.setUsage(usageInfo{}); ev != nil {
		t.Errorf("event = %s, want none for an empty readout", ev)
	}
	assertEvents(t, tr.endTurn(),
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

func TestParseUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want usageInfo
	}{
		{"fraction with commas", "Context: 12,345 / 256,000 tokens (5%)",
			usageInfo{PromptTokens: 12345, ContextWindow: 256000}},
		{"fraction with k suffix", "Tokens 12.5k/128k used",
			usageInfo{PromptTokens: 12500, ContextWindow: 128000}},
		{"labelled lines", "Context window: 200000\nTokens used: 41,000\nOutput: 812",
			usageInfo{PromptTokens: 41000, OutputTokens: 812, ContextWindow: 200000}},
		{"percentage only", "Context window: 128k — 25% full",
			usageInfo{PromptTokens: 32000, ContextWindow: 128000}},
		{"ansi coloured", "\x1b[32mContext: 100 / 1,000\x1b[0m",
			usageInfo{PromptTokens: 100, ContextWindow: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseUsage(tc.text)
			if !ok {
				t.Fatalf("parseUsage(%q) reported nothing usable", tc.text)
			}
			if got != tc.want {
				t.Errorf("parseUsage(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

// An unrecognised layout yields nothing at all — the meter stays empty rather
// than showing a number invented from an unrelated figure.
func TestParseUsageRejectsNonsense(t *testing.T) {
	for _, text := range []string{
		"", "   ", "Compacted 3/5 steps", "Session started 2026-07-31",
		"Context: 900,000 / 128,000", // used > window: a misread
	} {
		if got, ok := parseUsage(text); ok {
			t.Errorf("parseUsage(%q) = %+v, want nothing usable", text, got)
		}
	}
}

// --- session/load replay -------------------------------------------------

// A session created outside Agent Kate replays through session/load, whose
// user_message_chunk updates become the Claude-shaped user events the feed
// already renders as "You" cards. Without this, half the conversation is lost.
func TestUserMessageChunkReplaysAsUserEvent(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	if events := tr.update(upd("user_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "fix the auth bug"},
	})); len(events) != 1 {
		t.Fatalf("event count = %d, want 1: %s", len(events), events)
	} else {
		assertEvents(t, events,
			`{"type":"user","message":{"role":"user","content":[
			  {"type":"text","text":"fix the auth bug"}]}}`)
	}
	// Assistant text buffered before a user message flushes ahead of it, so
	// the replayed conversation reads in order.
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "on it"}}))
	assertEvents(t, tr.update(upd("user_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "thanks"}})),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"text","text":"on it"}]}}`,
		`{"type":"user","message":{"role":"user","content":[
		  {"type":"text","text":"thanks"}]}}`)
}

// A replay has no prompt response to close its last message; flush ships it.
func TestFlushShipsBufferedTextWithoutEndingATurn(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	tr.update(upd("agent_thought_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "hmm"}}))
	assertEvents(t, tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "done"}})),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"thinking","thinking":"hmm"}]}}`)
	assertEvents(t, tr.flush(),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"text","text":"done"}]}}`)
	if events := tr.flush(); len(events) != 0 {
		t.Errorf("second flush = %s, want none", events)
	}
}

// --- internal turns ------------------------------------------------------

// A silent internal turn (`/usage`) captures the CLI's reply and leaves no
// trace in the transcript — no cards, no result event.
func TestSilentInternalTurnEmitsNothing(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	tr.beginCapture(true)
	for _, ev := range [][]json.RawMessage{
		tr.update(upd("agent_thought_chunk", map[string]any{
			"content": map[string]any{"type": "text", "text": "counting"}})),
		tr.update(upd("agent_message_chunk", map[string]any{
			"content": map[string]any{"type": "text", "text": "Context: 10 / 1,000"}})),
		tr.update(upd("plan", map[string]any{
			"entries": []any{map[string]any{"content": "x", "status": "pending"}}})),
		tr.endTurn(),
	} {
		if len(ev) != 0 {
			t.Fatalf("silent turn emitted %s, want nothing", ev)
		}
	}
	if got := tr.endCapture(); got != "Context: 10 / 1,000" {
		t.Errorf("captured %q, want the reply text", got)
	}
	// The capture is closed: the next turn is the human's again.
	assertEvents(t, tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "hello"}})))
	assertEvents(t, tr.endTurn(),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"text","text":"hello"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// A visible internal turn (`/compact`) both captures the reply AND renders,
// because the human asked for it.
func TestVisibleInternalTurnStillRenders(t *testing.T) {
	tr := newTranslator("s1", "", nil)
	tr.beginCapture(false)
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "Compacted."}}))
	assertEvents(t, tr.endTurn(),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"text","text":"Compacted."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
	if got := tr.endCapture(); got != "Compacted." {
		t.Errorf("captured %q, want the reply text", got)
	}
}

// TestAbandonedSilentTurnKeepsItsTailOutOfTheFeed: `/usage` is Agent Kate's
// own bookkeeping, and cancelling it does not stop the CLI mid-sentence. The
// chunks that arrive after the turn was abandoned must not surface as an
// assistant card in a transcript the human never asked a question in.
func TestAbandonedSilentTurnKeepsItsTailOutOfTheFeed(t *testing.T) {
	tr := newTranslator("s1", "kimi-code/k3", nil)
	tr.beginCapture(true)
	if got := tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "Context: 4,096"},
	})); len(got) != 0 {
		t.Fatalf("a silent turn emitted %s", got)
	}
	if text := tr.abandonCapture(1); text != "Context: 4,096" {
		t.Errorf("captured text = %q, want what the CLI had said so far", text)
	}
	// The tail of the abandoned turn: still nothing, even though the capture
	// is closed and `silent` with it.
	if got := tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": " / 128,000 tokens"},
	})); len(got) != 0 {
		t.Errorf("the abandoned turn's tail reached the transcript: %s", got)
	}

	// Once the latch is lifted the feed is the human's again.
	tr.clearDrop()
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "Hello"},
	}))
	assertEvents(t, tr.endTurn(),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// A NON-silent internal turn (Compact) that errors mid-reply must not leave
// its partial text buffered: endTurn is skipped on the error path, so without
// a drain in endCapture the fragment would flush into the NEXT turn's
// transcript, attributed to a message the human just sent.
func TestErroredInternalTurnDoesNotLeakIntoTheNextTurn(t *testing.T) {
	tr := newTranslator("s1", "", nil)

	tr.beginCapture(false) // Compact: the human asked, so it is visible
	tr.update(upd("agent_thought_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "thinking about it"}}))
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "half a compact rep"}}))
	// onPromptDone's error path: no endTurn, straight to endCapture.
	if got := tr.endCapture(); got != "half a compact rep" {
		t.Errorf("captured text = %q, want the partial reply", got)
	}

	// The next human turn carries its own output and nothing else.
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "the real answer"}}))
	assertEvents(t, tr.endTurn(),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"text","text":"the real answer"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}

// Same leak, reached through the abandon path (a user send pre-empted the
// turn, or it timed out). abandonCapture must drain too — and latch away the
// late chunks the cancelled prompt is still streaming, which would otherwise
// re-buffer into the next turn behind the drain.
func TestAbandonedInternalTurnDoesNotLeakIntoTheNextTurn(t *testing.T) {
	tr := newTranslator("s1", "", nil)

	tr.beginCapture(false)
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "compacting"}}))
	if got := tr.abandonCapture(1); got != "compacting" {
		t.Errorf("captured text = %q, want %q", got, "compacting")
	}
	// The CLI keeps streaming the cancelled prompt for a moment: latched away.
	if ev := tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": " ...late tail"}})); len(ev) != 0 {
		t.Errorf("late chunk of an abandoned turn emitted %d events, want 0", len(ev))
	}

	// Send/onPromptDone hand the feed to the next turn.
	tr.clearDrop()
	tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "the real answer"}}))
	assertEvents(t, tr.endTurn(),
		`{"type":"assistant","message":{"role":"assistant","content":[
		  {"type":"text","text":"the real answer"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}`)
}
