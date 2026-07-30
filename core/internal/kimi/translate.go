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

	// configOptions is the session's config-option set from the handshake
	// (model / thinking / mode enumerations), embedded in the init event so
	// the UI can populate real pickers instead of free-text fields.
	configOptions []ConfigOption

	text    strings.Builder // accumulated agent_message_chunk deltas
	thought strings.Builder // accumulated agent_thought_chunk deltas

	// toolCalls tracks each in-flight tool call so its tool_use event can be
	// emitted exactly once, with the best input known at the time: kimi
	// streams the raw input JSON as content snapshots on tool_call_update
	// (status in_progress) rather than in the tool_call itself.
	toolCalls map[string]*toolCallState
}

// ConfigOption is one session config option from the ACP handshake, normalised
// for the init event. Kimi 0.30 exposes three: "model" (the model list),
// "thinking" (low/high/max — the effort analogue) and "mode"
// (default/plan/auto/yolo — the approval-mode analogue).
type ConfigOption struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Category     string              `json:"category,omitempty"`
	CurrentValue string              `json:"currentValue"`
	Options      []ConfigOptionValue `json:"options,omitempty"`
}

// ConfigOptionValue is one selectable value of a ConfigOption.
type ConfigOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type toolCallState struct {
	title   string
	kind    string
	input   string // latest raw-input JSON snapshot
	emitted bool   // the tool_use event has been emitted
}

func newTranslator(sessionID, model string, configOptions []ConfigOption) *translator {
	return &translator{
		sessionID:     sessionID,
		model:         model,
		configOptions: configOptions,
		toolCalls:     make(map[string]*toolCallState),
	}
}

// acpContent is an ACP content block. Kimi wraps streamed text in a
// {"type":"content","content":{...}} envelope, hence the recursion. Image
// blocks carry base64 data + mimeType.
type acpContent struct {
	Type     string      `json:"type"`
	Text     string      `json:"text"`
	Data     string      `json:"data"`
	MimeType string      `json:"mimeType"`
	Content  *acpContent `json:"content"`
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

// contentImages collects the image blocks from a content block list.
func contentImages(blocks []acpContent) []acpContent {
	var out []acpContent
	for _, b := range blocks {
		if b.Type == "content" && b.Content != nil {
			out = append(out, contentImages([]acpContent{*b.Content})...)
			continue
		}
		if b.Type == "image" && b.Data != "" {
			out = append(out, b)
		}
	}
	return out
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
// the model line and reads configOptions (a kimi-only extension field) to
// populate the model/thinking/mode pickers with what the CLI actually offers.
func (t *translator) initEvent() json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	ev := map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": t.sessionID,
		"model":      t.model,
	}
	if len(t.configOptions) > 0 {
		ev["configOptions"] = t.configOptions
	}
	return marshalEvent(ev)
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
			Entries       []planEntry     `json:"entries"` // plan updates
			AvailableCommands []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"availableCommands"` // available_commands_update
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
		// Buffer the delta; it ships when a tool_call or the turn end flushes
		// it. The first message chunk after thought chunks marks the thought
		// as finished — ship it now so the cards read in order.
		events := t.flushThoughtLocked()
		t.text.WriteString(contentText(decodeContent(u.Content)))
		return events

	case "agent_thought_chunk":
		// Buffer the reasoning delta; it ships as one thinking block when the
		// visible message resumes, a tool call lands, or the turn ends.
		t.thought.WriteString(contentText(decodeContent(u.Content)))
		return nil

	case "plan":
		// The agent's plan / todo list. Translate into the same TodoWrite
		// tool_use shape the Claude harness produces, so both backends feed
		// the one checklist card in the UI. ACP and TodoWrite share the
		// status vocabulary (pending / in_progress / completed).
		if len(u.Entries) == 0 {
			return nil
		}
		todos := make([]map[string]any, 0, len(u.Entries))
		for _, e := range u.Entries {
			todos = append(todos, map[string]any{
				"content": e.Content,
				"status":  e.Status,
			})
		}
		events := t.flushPendingLocked()
		return append(events, marshalEvent(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{{
					"type":  "tool_use",
					"id":    "acp-plan",
					"name":  "TodoWrite",
					"input": map[string]any{"todos": todos},
				}},
			},
		}))

	case "available_commands_update":
		// The CLI's slash-command list. There is no claude stream-json
		// counterpart event, so this ships as the synthetic `_commands` type
		// (underscore = Agent-Kate-injected, like _lifecycle) feeding the
		// composer's autocomplete.
		cmds := make([]map[string]any, 0, len(u.AvailableCommands))
		for _, c := range u.AvailableCommands {
			cmds = append(cmds, map[string]any{
				"name":        c.Name,
				"description": c.Description,
			})
		}
		return []json.RawMessage{marshalEvent(map[string]any{
			"type":     "_commands",
			"commands": cmds,
		})}

	case "tool_call":
		st := &toolCallState{title: u.Title, kind: u.Kind}
		if len(u.RawInput) > 0 {
			st.input = string(u.RawInput)
		} else if txt := contentText(decodeContent(u.Content)); txt != "" {
			st.input = txt
		}
		t.toolCalls[u.ToolCallID] = st
		events := t.flushPendingLocked()
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
		synthesized := false
		if st == nil {
			st = &toolCallState{}
			t.toolCalls[u.ToolCallID] = st
			synthesized = true
		}
		done := u.Status == "completed" || u.Status == "failed"
		// A completion (or failure) for a tool call we never saw announced —
		// no preceding tool_call, so the state was just synthesized here with no
		// title/kind/input — has no real tool_use to show. Emit only its result,
		// never a phantom "Tool" card with empty input.
		if synthesized && done {
			return []json.RawMessage{
				t.toolResultLocked(u.ToolCallID, u.Status, u.RawOutput, u.Content)}
		}
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
			// A tool card is an assistant event; flush any pending thought and
			// text first so the conversation reads in order.
			events := t.flushPendingLocked()
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
	// config_option_update and friends have no Claude-shaped counterpart the
	// UI renders — dropped.
	return nil
}

// planEntry is one item of an ACP plan update.
type planEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
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

// flushThoughtLocked ships the buffered reasoning as a single assistant event
// carrying a thinking block — the same shape the Claude harness emits, so the
// UI's thinking card renders both backends. No-op when empty. Caller holds t.mu.
func (t *translator) flushThoughtLocked() []json.RawMessage {
	if t.thought.Len() == 0 {
		return nil
	}
	thought := t.thought.String()
	t.thought.Reset()
	return []json.RawMessage{marshalEvent(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "thinking", "thinking": thought}},
		},
	})}
}

// flushPendingLocked ships buffered thought then text, preserving the order
// they streamed in (reasoning always precedes the visible message it led to).
// Caller holds t.mu.
func (t *translator) flushPendingLocked() []json.RawMessage {
	return append(t.flushThoughtLocked(), t.flushTextLocked()...)
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

// toolResultLocked builds the user/tool_result event for a finished call and
// drops the call's tracking state — a long thread makes thousands of tool
// calls, and the raw-input snapshots would otherwise accumulate for its whole
// life. Safe: the permission lookup always precedes completion. Caller holds
// t.mu.
func (t *translator) toolResultLocked(toolCallID, status string, rawOutput, blocks json.RawMessage) json.RawMessage {
	delete(t.toolCalls, toolCallID)
	text := ""
	if len(rawOutput) > 0 {
		// rawOutput may be a plain string or a structured object; the UI's
		// tool-result rendering expects text either way.
		if err := json.Unmarshal(rawOutput, &text); err != nil {
			text = string(rawOutput)
		}
	} else {
		text = contentText(decodeContent(blocks))
	}
	// Image blocks (e.g. a screenshot result) ride through as Claude-shaped
	// base64 image blocks so the UI's thumbnail chips render them; text-only
	// results keep the plain-string shape.
	var content any = text
	if images := contentImages(decodeContent(blocks)); len(images) > 0 {
		arr := []map[string]any{}
		if text != "" {
			arr = append(arr, map[string]any{"type": "text", "text": text})
		}
		for _, img := range images {
			arr = append(arr, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": img.MimeType,
					"data":       img.Data,
				},
			})
		}
		content = arr
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

// endTurn flushes any buffered assistant thought and text and appends the
// turn's result event. ACP exposes no usage accounting (verified against kimi
// 0.30: session/prompt responses carry only stopReason), so the event carries
// none — the UI already defaults missing usage fields to zero.
func (t *translator) endTurn() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	events := t.flushPendingLocked()
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
