package kimi

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
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

	// usage is the latest context/token readout, refreshed by the supervisor's
	// `/usage` probe after each turn. It rides on the NEXT result event (see
	// endTurn) because the probe can only run once the turn it measures has
	// finished — the UI's context meter therefore trails by one turn.
	usage usageInfo

	// capture, when non-nil, accumulates the assistant text of an
	// Agent-Kate-internal turn (`/compact`, `/usage`) so the supervisor can
	// read the CLI's answer. silent additionally suppresses every event of
	// that turn: a bookkeeping turn the human never asked for must not leave
	// cards in the transcript.
	capture *strings.Builder
	silent  bool

	// dropUntil latches away every event of a silent turn Agent Kate abandoned
	// but the CLI is still streaming — see abandonCapture. Zero means "not
	// dropping"; the deadline is only a backstop for a cancelled prompt whose
	// reply never arrives, since that reply is what normally clears it.
	dropUntil time.Time
	// dropOwner names the abandoned turn the latch was armed for. The latch is
	// thread-global but two abandoned turns can be unwinding at once (a probe
	// pre-empted by a send, then that send's own compact abandoned), and the
	// FIRST one's late reply must not lift the SECOND one's latch — that would
	// spill the still-streaming turn's chunks into the transcript. Zero means
	// "armed by nobody in particular"; clearDropFor only lifts its own.
	dropOwner uint64
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

// setSession fills in the model and config-option set a session/load handshake
// only learns AFTER the translator had to exist (the replayed history streams
// as notifications during the load call itself).
func (t *translator) setSession(model string, configOptions []ConfigOption) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if model != "" {
		t.model = model
	}
	if len(configOptions) > 0 {
		t.configOptions = configOptions
	}
}

// configValue returns an option's current value as the translator last saw it
// — the handshake's value, then whatever config_option_update or a
// set_config_option response revised it to.
func (t *translator) configValue(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, co := range t.configOptions {
		if co.ID == id {
			return co.CurrentValue
		}
	}
	return ""
}

// applyConfigOptions folds an authoritative config snapshot (a
// config_option_update notification, or a session/set_config_option response)
// into the session's option set and returns the `_options` event carrying the
// merged result. Returns nil when the snapshot said nothing.
//
// The event repeats the FULL set, in the same `configOptions` shape the init
// event uses, so a consumer needs one parser for both: the enumerations are
// the picker's vocabulary and each currentValue is what the CLI is actually
// running right now — which is the point, since kimi changes mode on its own
// (its ExitPlanMode transition) and never asked us first.
func (t *translator) applyConfigOptions(in []ConfigOption) json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.applyConfigOptionsLocked(in)
}

// applyConfigOptionsLocked is applyConfigOptions with t.mu already held.
func (t *translator) applyConfigOptionsLocked(in []ConfigOption) json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	changed := make([]string, 0, len(in))
	for _, up := range in {
		if up.ID == "" {
			continue
		}
		changed = append(changed, up.ID)
		found := false
		for i := range t.configOptions {
			if t.configOptions[i].ID != up.ID {
				continue
			}
			found = true
			t.configOptions[i].CurrentValue = up.CurrentValue
			// A notification may carry only the new value; keep the
			// enumeration we already have rather than blanking the picker.
			if up.Name != "" {
				t.configOptions[i].Name = up.Name
			}
			if up.Category != "" {
				t.configOptions[i].Category = up.Category
			}
			if len(up.Options) > 0 {
				t.configOptions[i].Options = up.Options
			}
		}
		if !found {
			t.configOptions = append(t.configOptions, up)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return marshalEvent(map[string]any{
		"type":          "_options",
		"session_id":    t.sessionID,
		"changed":       changed,
		"configOptions": t.configOptions,
	})
}

// decodeConfigOptions reads a config snapshot out of any of the shapes kimi
// uses for one: the update object of a config_option_update notification, or
// the result of session/set_config_option. Both spell the payload differently
// depending on whether one option or the whole set changed, so every spelling
// is accepted and the unrecognised ones degrade to nil rather than to a wrong
// value.
func decodeConfigOptions(raw json.RawMessage) []ConfigOption {
	if len(raw) == 0 {
		return nil
	}
	var envelope struct {
		ConfigOptions []ConfigOption `json:"configOptions"`
		ConfigOption  *ConfigOption  `json:"configOption"`
		// The flat spelling: the option's id and its new value side by side.
		ConfigID     string `json:"configId"`
		ID           string `json:"id"`
		Value        string `json:"value"`
		CurrentValue string `json:"currentValue"`
		// Some updates nest the whole set under "config".
		Config []ConfigOption `json:"config"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	if len(envelope.ConfigOptions) > 0 {
		return envelope.ConfigOptions
	}
	if len(envelope.Config) > 0 {
		return envelope.Config
	}
	if envelope.ConfigOption != nil && envelope.ConfigOption.ID != "" {
		return []ConfigOption{*envelope.ConfigOption}
	}
	id := envelope.ConfigID
	if id == "" {
		id = envelope.ID
	}
	value := envelope.CurrentValue
	if value == "" {
		value = envelope.Value
	}
	if id != "" && value != "" {
		return []ConfigOption{{ID: id, CurrentValue: value}}
	}
	return nil
}

// usageInfo is one context/token readout, as `/usage` reports it.
type usageInfo struct {
	PromptTokens  int64 // what the context currently holds
	OutputTokens  int64 // 0 when the CLI reports no output split
	ContextWindow int64 // the model's window; 0 when unreported
}

func (u usageInfo) known() bool { return u.PromptTokens > 0 || u.ContextWindow > 0 }

// setUsage records the latest context readout and returns the event announcing
// it immediately. Returns nil when nothing was parsed.
//
// The event is `_context` — the SAME shape the claude control channel ships
// from get_context_usage (see agent.ContextUsage.JSON), because that is the one
// the UI's context meter already consumes. Emitting a kimi-only `_usage` shape
// was inert: no UI path read it, so the readout only ever reached the panel
// folded into the NEXT turn's result event, a full turn late. Kimi's `/usage`
// is the CLI reporting its own window, so like claude's it outranks the UI's
// prompt-token estimate; there is no per-category breakdown to send.
func (t *translator) setUsage(u usageInfo) json.RawMessage {
	if !u.known() {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usage = u
	return marshalEvent(map[string]any{
		"type":       "_context",
		"usedTokens": u.PromptTokens,
		"maxTokens":  u.ContextWindow,
	})
}

// contextFieldsLocked renders the stored readout as the ONLY usage-shaped
// fields a kimi result event may carry: `modelUsage.<model>.contextWindow`,
// which is the model's window size — a property of the model, not a bill.
// Caller holds t.mu.
//
// It deliberately does NOT emit `usage.input_tokens` / `output_tokens` (audit
// F19b). Kimi's readout comes from `/usage`, which reports what the context
// CURRENTLY HOLDS — a cumulative snapshot, not this turn's tokens. Claude's
// result event carries per-turn deltas, and the UI accumulates whatever it
// finds there into the session total, so shipping the snapshot in those field
// names made a thread holding 4k/8k/12k over three turns report "24k in" and a
// per-turn cost line for spend that never happened. The honest wire has kimi
// send its context fill exactly once, as the `_context` event setUsage emits,
// and claim no per-turn usage at all — which is also what the kimi harness
// advertises (UsageReporting is absent from its descriptor; LOCKSTEP with
// ui/src/state/HarnessTraits.cpp's usageReporting for "kimi").
func (t *translator) contextFieldsLocked() map[string]any {
	if !t.usage.known() || t.usage.ContextWindow <= 0 {
		return nil
	}
	model := t.model
	if model == "" {
		model = "kimi"
	}
	return map[string]any{
		"modelUsage": map[string]any{
			model: map[string]any{"contextWindow": t.usage.ContextWindow},
		},
	}
}

// beginCapture opens an Agent-Kate-internal turn: its assistant text is
// captured for the caller, and when silent, none of its events reach the
// transcript.
func (t *translator) beginCapture(silent bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.capture = &strings.Builder{}
	t.silent = silent
	// A new turn owns the feed from here, whoever armed the latch.
	t.dropUntil, t.dropOwner = time.Time{}, 0
}

// endCapture closes the internal turn and returns the text it produced.
func (t *translator) endCapture() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endCaptureLocked()
}

func (t *translator) endCaptureLocked() string {
	text := ""
	if t.capture != nil {
		text = t.capture.String()
	}
	t.capture = nil
	t.silent = false
	// Drop whatever the turn had buffered but not shipped. On the happy path
	// endTurn has already flushed (or, for a silent turn, reset) both builders,
	// so this is a no-op. It is NOT a no-op when the turn errored or was
	// abandoned: those paths skip endTurn, and a non-silent internal turn
	// (Compact) would leave its partial reply sitting in these builders for the
	// next human turn's endTurn to flush into that turn's transcript.
	t.text.Reset()
	t.thought.Reset()
	return text
}

// abandonCapture closes an internal turn whose reply nobody is waiting for any
// more and latches the rest of its output away. The CLI keeps streaming a
// cancelled prompt for a moment, and those late chunks are no longer covered
// by `silent` — nor, once endCaptureLocked has dropped the buffers, by
// anything else: they would re-buffer and flush into whatever turn comes next.
// A silent probe's would surface as an assistant card in a transcript the
// human never asked a question in; an abandoned Compact's would be the tail of
// a partial reply whose head has just been discarded. Neither belongs to the
// next turn, so the latch covers both.
//
// It is lifted by whichever comes first: the abandoned prompt's own reply (see
// onPromptDone, which lifts only the latch IT armed — owner), the next prompt
// Agent Kate writes (Send clears it explicitly, beginCapture resets it), or
// abandonDropGrace. All three are deliberate — once another turn can be
// streaming, dropping its output would be far worse than one stray card.
//
// owner is the abandoned turn's id, so a later abandon's latch cannot be lifted
// by an earlier abandon's straggling reply.
func (t *translator) abandonCapture(owner uint64) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropUntil = time.Now().Add(abandonDropGrace)
	t.dropOwner = owner
	return t.endCaptureLocked()
}

// clearDrop lifts the abandon latch unconditionally. For the callers that own
// the feed outright — a human send about to write its own prompt.
func (t *translator) clearDrop() {
	t.mu.Lock()
	t.dropUntil, t.dropOwner = time.Time{}, 0
	t.mu.Unlock()
}

// clearDropFor lifts the abandon latch only if `owner` is the turn that armed
// it. Reports whether it did. A straggling reply from an earlier abandoned turn
// must leave a later one's latch standing.
func (t *translator) clearDropFor(owner uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropUntil.IsZero() || t.dropOwner != owner {
		return false
	}
	t.dropUntil, t.dropOwner = time.Time{}, 0
	return true
}

// dropping reports whether the abandon latch is still in force. Caller holds
// t.mu.
func (t *translator) droppingLocked() bool {
	return !t.dropUntil.IsZero() && time.Now().Before(t.dropUntil)
}

// abandonDropGrace bounds the abandon latch: long enough for a cancelled
// prompt to finish unwinding, short enough that a CLI which never answers it
// cannot mute the thread.
const abandonDropGrace = 15 * time.Second

// flush ships whatever assistant thought and text are buffered, without ending
// a turn — the session/load replay has no prompt response to close it.
func (t *translator) flush() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.flushPendingLocked()
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
			SessionUpdate     string          `json:"sessionUpdate"`
			Content           json.RawMessage `json:"content"`
			ToolCallID        string          `json:"toolCallId"`
			Title             string          `json:"title"`
			Kind              string          `json:"kind"`
			Status            string          `json:"status"`
			RawInput          json.RawMessage `json:"rawInput"`
			RawOutput         json.RawMessage `json:"rawOutput"`
			Entries           []planEntry     `json:"entries"`           // plan updates
			AvailableCommands []acpCommand    `json:"availableCommands"` // available_commands_update
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	u := &p.Update

	t.mu.Lock()
	defer t.mu.Unlock()

	// The tail of an abandoned silent turn: still the CLI's bookkeeping reply,
	// no longer anything anyone waits for.
	if t.droppingLocked() {
		return nil
	}

	// A silent internal turn (`/usage`) contributes nothing but its text: the
	// human never asked for it, so none of its updates may reach the feed.
	if t.silent && u.SessionUpdate != "agent_message_chunk" {
		return nil
	}

	switch u.SessionUpdate {
	case "agent_message_chunk":
		txt := contentText(decodeContent(u.Content))
		if t.capture != nil {
			t.capture.WriteString(txt)
		}
		if t.silent {
			return nil
		}
		// Buffer the delta; it ships when a tool_call or the turn end flushes
		// it. The first message chunk after thought chunks marks the thought
		// as finished — ship it now so the cards read in order.
		events := t.flushThoughtLocked()
		t.text.WriteString(txt)
		return events

	case "user_message_chunk":
		// Only session/load replay produces these — a live turn's user message
		// is one Agent Kate sent itself. Shaped as the Claude-side user event
		// so a session started outside Agent Kate replays with both halves of
		// the conversation, not just the agent's.
		txt := contentText(decodeContent(u.Content))
		if txt == "" {
			return nil
		}
		events := t.flushPendingLocked()
		return append(events, marshalEvent(map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": txt}},
			},
		}))

	case "config_option_update":
		// Kimi announces every mid-session model / thinking / mode change here
		// — including the ones it makes on its own initiative, like the mode
		// flip its ExitPlanMode performs. Dropping it is what left the UI
		// showing a mode the session had already left.
		var raw struct {
			Update json.RawMessage `json:"update"`
		}
		if json.Unmarshal(params, &raw) != nil {
			return nil
		}
		ev := t.applyConfigOptionsLocked(decodeConfigOptions(raw.Update))
		if ev == nil {
			return nil
		}
		return []json.RawMessage{ev}

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
		// The argument hint rides along as `hint` — the placeholder the CLI
		// shows after the command name (`/compact <instructions>`). Claude's
		// own init event lists slash commands as bare names with no hint
		// channel at all, so there is no existing field name to match.
		cmds := make([]map[string]any, 0, len(u.AvailableCommands))
		for _, c := range u.AvailableCommands {
			cmd := map[string]any{
				"name":        c.Name,
				"description": c.Description,
			}
			if hint := c.hint(); hint != "" {
				cmd["hint"] = hint
			}
			cmds = append(cmds, cmd)
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

// commandNames extracts the slash-command names an available_commands_update
// announces. An announcement that lists none returns an empty (non-nil) slice:
// "the CLI told us it has no commands" is a different fact from "the CLI has
// not told us anything", and the thread's gate distinguishes them.
func commandNames(params json.RawMessage) []string {
	var p struct {
		Update struct {
			AvailableCommands []acpCommand `json:"availableCommands"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	out := make([]string, 0, len(p.Update.AvailableCommands))
	for _, c := range p.Update.AvailableCommands {
		out = append(out, c.Name)
	}
	return out
}

// acpCommand is one entry of an available_commands_update. The argument hint
// is optional and kimi spells it two ways depending on the command — a bare
// string, or an object under `hint` — so Input stays raw and hint() decides.
type acpCommand struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
}

// hint returns the command's argument placeholder, or "" when it takes none.
func (c acpCommand) hint() string {
	if len(c.Input) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(c.Input, &s) == nil {
		return s
	}
	var obj struct {
		Hint string `json:"hint"`
	}
	if json.Unmarshal(c.Input, &obj) == nil {
		return obj.Hint
	}
	return ""
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
// turn's result event. A session/prompt response carries only stopReason — no
// token accounting of any kind — so the result event carries no per-turn usage
// either; the context fill travels on its own `_context` event (see
// contextFieldsLocked for why inventing per-turn numbers here was a billing
// lie). Only the model's context-window size rides along.
func (t *translator) endTurn() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.silent {
		// A silent internal turn leaves no trace: no flush, no result.
		t.text.Reset()
		t.thought.Reset()
		return nil
	}
	events := t.flushPendingLocked()
	ev := map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"session_id": t.sessionID,
	}
	for k, v := range t.contextFieldsLocked() {
		ev[k] = v
	}
	return append(events, marshalEvent(ev))
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
