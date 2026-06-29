package compact

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Programmatic builds a compact summary of a Claude Code transcript with no
// LLM involvement. It is behaviourally lossless: every user message, every
// assistant text block, and every tool call (with its arguments) survives;
// only tool_result bodies — which dominate token cost on code-heavy sessions
// — are dropped. Lifecycle, system, summary and result events are also
// skipped as low-signal for resumption.
//
// The output is a single markdown blob suitable for seeding a fresh claude
// session as its opening prompt, in place of replaying the full transcript.
//
// events is the raw JSONL transcript from session.ReadTranscript. ThreadID
// and SessionID are stamped onto the returned Summary; both may be empty in
// tests.
func Programmatic(threadID, sessionID string, events []json.RawMessage) Summary {
	var b strings.Builder
	b.WriteString("# Prior session context (compressed)\n\n")
	b.WriteString("This is a compressed transcript of the previous session. " +
		"Tool result bodies have been omitted; tool calls and assistant " +
		"text are preserved. Resume from where this leaves off.\n\n")
	b.WriteString("## Conversation\n\n")

	var (
		turns       int
		userTurn    int
		lastUserMsg string
	)
	for _, raw := range events {
		var head struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &head) != nil {
			continue
		}
		switch head.Type {
		case "user":
			text, sawText := userText(head.Message)
			if !sawText {
				// Pure tool_result wrapper — skip; the assistant's tool_use
				// call already records what the call was for.
				continue
			}
			userTurn++
			turns = userTurn
			lastUserMsg = text
			fmt.Fprintf(&b, "**User (turn %d):** %s\n\n", userTurn, trimForDisplay(text))
		case "assistant":
			assistantText, toolCalls := assistantPieces(head.Message)
			if assistantText == "" && len(toolCalls) == 0 {
				continue
			}
			b.WriteString("**Assistant:** ")
			if assistantText != "" {
				b.WriteString(trimForDisplay(assistantText))
			}
			b.WriteString("\n")
			if len(toolCalls) > 0 {
				b.WriteString("Tool calls:\n")
				for _, c := range toolCalls {
					fmt.Fprintf(&b, "- %s: %s\n", c.name, c.args)
				}
			}
			b.WriteString("\n")
		}
		// Other event types (system, result, summary, meta) are noise for a
		// downstream resumer and are dropped.
	}

	if lastUserMsg != "" {
		b.WriteString("## Last user request (most recent)\n\n> ")
		b.WriteString(strings.ReplaceAll(strings.TrimSpace(lastUserMsg), "\n", "\n> "))
		b.WriteString("\n")
	}

	return Summary{
		ThreadID:  threadID,
		SessionID: sessionID,
		Strategy:  ResumeLocal,
		Stripped:  false, // strip flag is for LLM compactors; programmatic always strips
		Created:   time.Now().UTC(),
		Turns:     turns,
		Body:      b.String(),
	}
}

// toolCall is one tool_use block summarised for inclusion in the compact body.
type toolCall struct {
	name string
	args string // one-line argument summary
}

// userText extracts the user-visible text from a user message. Returns the
// second value false when the message carried only tool_result blocks (i.e.
// it was a tool response rather than a real human message).
func userText(msg json.RawMessage) (string, bool) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return "", false
	}
	// Content can be a bare string or an array of typed blocks.
	if len(m.Content) > 0 && m.Content[0] == '"' {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s, strings.TrimSpace(s) != ""
		}
		return "", false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return "", false
	}
	var sb strings.Builder
	sawText := false
	for _, blk := range blocks {
		if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(blk.Text)
			sawText = true
		}
	}
	return sb.String(), sawText
}

// assistantPieces extracts the assistant's text and a list of tool_use blocks
// from one assistant message. Other block kinds (thinking, etc.) are dropped.
func assistantPieces(msg json.RawMessage) (string, []toolCall) {
	var m struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return "", nil
	}
	var (
		text  strings.Builder
		calls []toolCall
	)
	for _, raw := range m.Content {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &head) != nil {
			continue
		}
		switch head.Type {
		case "text":
			var t struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &t) == nil && strings.TrimSpace(t.Text) != "" {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(t.Text)
			}
		case "tool_use":
			var u struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(raw, &u) != nil || u.Name == "" {
				continue
			}
			calls = append(calls, toolCall{
				name: u.Name,
				args: summarizeToolInput(u.Name, u.Input),
			})
		}
	}
	return text.String(), calls
}

// summarizeToolInput renders a one-line view of a tool's input. Mirrors the
// helper in package agent's toolmeter but cannot import it without inverting
// the dependency. Kept narrowly focused — extra detail belongs to telemetry,
// not compaction.
func summarizeToolInput(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch name {
	case "Read":
		var i struct {
			FilePath string `json:"file_path"`
			Offset   *int   `json:"offset"`
			Limit    *int   `json:"limit"`
		}
		if json.Unmarshal(raw, &i) == nil {
			window := "full"
			if i.Offset != nil || i.Limit != nil {
				off, lim := 0, 0
				if i.Offset != nil {
					off = *i.Offset
				}
				if i.Limit != nil {
					lim = *i.Limit
				}
				window = fmt.Sprintf("offset=%d limit=%d", off, lim)
			}
			return fmt.Sprintf("%s [%s]", i.FilePath, window)
		}
	case "Edit", "Write":
		var i struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(raw, &i) == nil && i.FilePath != "" {
			return i.FilePath
		}
	case "Bash":
		var i struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &i) == nil {
			return truncate(i.Command, 120)
		}
	case "Grep", "Glob":
		var i struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(raw, &i) == nil {
			if i.Path != "" {
				return fmt.Sprintf("%q in %s", i.Pattern, i.Path)
			}
			return fmt.Sprintf("%q", i.Pattern)
		}
	case "TaskCreate":
		var i struct {
			Subject string `json:"subject"`
		}
		if json.Unmarshal(raw, &i) == nil && i.Subject != "" {
			return i.Subject
		}
	case "TaskUpdate":
		var i struct {
			TaskID string `json:"taskId"`
			Status string `json:"status"`
		}
		if json.Unmarshal(raw, &i) == nil {
			return fmt.Sprintf("%s -> %s", i.TaskID, i.Status)
		}
	}
	return truncate(string(raw), 120)
}

// trimForDisplay keeps the body compact: collapses runs of blank lines and
// trims surrounding whitespace, but preserves single newlines so the LLM still
// sees paragraph structure.
func trimForDisplay(s string) string {
	s = strings.TrimSpace(s)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Back up to a rune boundary so a multi-byte rune is never split (which would
	// emit invalid UTF-8 into the summary body).
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}
