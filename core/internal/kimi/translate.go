package kimi

import (
	"encoding/json"
	"strings"
	"sync"
)

// translator converts one kimi session's ACP traffic into the Claude-shaped
// stream-json events the UI already renders. All mutation happens on the ACP
// read-loop goroutine except toolForPermission, which the permission bridge
// calls from its own goroutine — hence the single mutex.
//
// The UI appends one card per assistant event (AgentPanel renderEvent), so
// agent_message_chunk deltas are buffered and flushed as ONE assistant event
// per completed message: when a tool_call interrupts the text, or when the
// turn ends. Partial snapshots are never emitted.
type translator struct {
	mu        sync.Mutex
	sessionID string
	model     string

	text strings.Builder // accumulated agent_message_chunk deltas

	// toolCalls tracks each in-flight tool call so its tool_use event can be
	// emitted exactly once, with the best input known at the time: kimi
	// streams the raw input JSON as content snapshots on tool_call_update
	// (status in_progress) rather than in the tool_call itself.
	toolCalls map[string]*toolCallState
}

type toolCallState struct {
	title   string
	kind    string
	input   string // latest raw-input JSON snapshot
	emitted bool   // the tool_use event has been emitted
}

func newTranslator(sessionID, model string) *translator {
	return &translator{
		sessionID: sessionID,
		model:     model,
		toolCalls: make(map[string]*toolCallState),
	}
}

// acpContent is an ACP content block. Kimi wraps streamed text in a
// {"type":"content","content":{...}} envelope, hence the recursion.
type acpContent struct {
	Type    string      `json:"type"`
	Text    string      `json:"text"`
	Content *acpContent `json:"content"`
}

// contentText concatenates the text found in a content block list.
func contentText(blocks []acpContent) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "content" && b.Content != nil {
			sb.WriteString(contentText([]acpContent{*b.Content}))
			continue
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// marshalEvent is the shared event constructor; a marshal failure on these
// static shapes is impossible, so the error is collapsed.
func marshalEvent(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// initEvent is the synthetic session-start event, shaped like the init system
// event `claude` emits. The run loop persists session_id from it; the UI shows
// the model line.
func (t *translator) initEvent() json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return marshalEvent(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": t.sessionID,
		"model":      t.model,
	})
}

// toolName maps an ACP tool kind/title onto the Claude tool name the UI knows
// how to summarise and icon.
func toolName(title, kind string) string {
	switch kind {
	case "execute":
		return "Bash"
	case "edit":
		return "Edit"
	case "read":
		return "Read"
	case "fetch":
		return "WebFetch"
	}
	if title != "" {
		return title
	}
	return "Tool"
}

// parseInput parses a raw-input JSON snapshot into a tool_use input object.
// An unparseable or non-object snapshot degrades to an empty object.
func parseInput(raw string) map[string]any {
	input := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &input)
	}
	return input
}

// decodeContent reads the flexible "content" field of an update:
// agent_message_chunk carries a single block object, tool_call* a block array.
func decodeContent(raw json.RawMessage) []acpContent {
	if len(raw) == 0 {
		return nil
	}
	var arr []acpContent
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var single acpContent
	if json.Unmarshal(raw, &single) == nil {
		return []acpContent{single}
	}
	return nil
}

// update translates one session/update notification into zero or more events.
func (t *translator) update(params json.RawMessage) []json.RawMessage {
	var p struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			RawOutput     json.RawMessage `json:"rawOutput"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	u := &p.Update

	t.mu.Lock()
	defer t.mu.Unlock()

	switch u.SessionUpdate {
	case "agent_message_chunk":
		// Buffer the delta; it ships when a tool_call or the turn end flushes it.
		t.text.WriteString(contentText(decodeContent(u.Content)))
		return nil

	case "tool_call":
		st := &toolCallState{title: u.Title, kind: u.Kind}
		if len(u.RawInput) > 0 {
			st.input = string(u.RawInput)
		} else if txt := contentText(decodeContent(u.Content)); txt != "" {
			st.input = txt
		}
		t.toolCalls[u.ToolCallID] = st
		events := t.flushTextLocked()
		// Emit the card immediately only when the input is already known in
		// full (a spec-style rawInput); otherwise wait for the streamed
		// snapshot to become parseable so the card shows the real command.
		if len(u.RawInput) > 0 {
			if ev := t.emitToolUseLocked(u.ToolCallID); ev != nil {
				events = append(events, ev)
			}
		}
		return events

	case "tool_call_update":
		st := t.toolCalls[u.ToolCallID]
		if st == nil {
			st = &toolCallState{}
			t.toolCalls[u.ToolCallID] = st
		}
		done := u.Status == "completed" || u.Status == "failed"
		// While the call runs, content carries the accumulating raw-input JSON
		// snapshot; once done it carries the tool's output instead.
		if !done {
			if txt := contentText(decodeContent(u.Content)); txt != "" {
				st.input = txt
			}
		}
		// Emit the tool card as soon as the input snapshot parses (typically
		// just before the permission request), or force it at completion.
		if !st.emitted && (parseableObject(st.input) || done) {
			// A tool card is an assistant event; flush any pending text first
			// so the conversation reads in order.
			events := t.flushTextLocked()
			if ev := t.emitToolUseLocked(u.ToolCallID); ev != nil {
				events = append(events, ev)
			}
			if !done {
				return events
			}
			return append(events, t.toolResultLocked(u.ToolCallID, u.Status, u.RawOutput, u.Content))
		}
		if done {
			return []json.RawMessage{t.toolResultLocked(u.ToolCallID, u.Status, u.RawOutput, u.Content)}
		}
		return nil
	}
	// agent_thought_chunk, plan, config_option_update, available_commands_update
	// and friends have no Claude-shaped counterpart the UI renders — dropped.
	return nil
}

// parseableObject reports whether raw parses as a JSON object.
func parseableObject(raw string) bool {
	if raw == "" {
		return false
	}
	var v map[string]any
	return json.Unmarshal([]byte(raw), &v) == nil
}

// flushTextLocked ships the buffered assistant text as a single assistant
// event. No-op when the buffer is empty. Caller holds t.mu.
func (t *translator) flushTextLocked() []json.RawMessage {
	if t.text.Len() == 0 {
		return nil
	}
	text := t.text.String()
	t.text.Reset()
	return []json.RawMessage{marshalEvent(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})}
}

// emitToolUseLocked emits the tool_use assistant event for a call exactly
// once. Caller holds t.mu.
func (t *translator) emitToolUseLocked(toolCallID string) json.RawMessage {
	st := t.toolCalls[toolCallID]
	if st == nil || st.emitted {
		return nil
	}
	st.emitted = true
	return marshalEvent(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{{
				"type":  "tool_use",
				"id":    toolCallID,
				"name":  toolName(st.title, st.kind),
				"input": parseInput(st.input),
			}},
		},
	})
}

// toolResultLocked builds the user/tool_result event for a finished call.
// Caller holds t.mu.
func (t *translator) toolResultLocked(toolCallID, status string, rawOutput, blocks json.RawMessage) json.RawMessage {
	content := ""
	if len(rawOutput) > 0 {
		// rawOutput may be a plain string or a structured object; the UI's
		// tool-result rendering expects text either way.
		if err := json.Unmarshal(rawOutput, &content); err != nil {
			content = string(rawOutput)
		}
	} else {
		content = contentText(decodeContent(blocks))
	}
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolCallID,
		"content":     content,
	}
	if status == "failed" {
		block["is_error"] = true
	}
	return marshalEvent(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{block},
		},
	})
}

// endTurn flushes any buffered assistant text and appends the turn's result
// event. ACP exposes no usage accounting, so the event carries none — the UI
// already defaults missing usage fields to zero.
func (t *translator) endTurn() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	events := t.flushTextLocked()
	return append(events, marshalEvent(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"session_id": t.sessionID,
	}))
}

// toolForPermission returns the display name and best-known raw input for a
// tool call, for the permission bridge's prompt to the human. Called off the
// read loop (the request handler's goroutine).
func (t *translator) toolForPermission(toolCallID string) (name string, input json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.toolCalls[toolCallID]
	if st == nil {
		return "", nil
	}
	input = json.RawMessage(st.input)
	if !parseableObject(st.input) {
		input = json.RawMessage("{}")
	}
	return toolName(st.title, st.kind), input
}
