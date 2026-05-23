package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// toolMeter measures how many characters each tool returns to the model on a
// single agent thread, so we can tell where context tokens are going. It
// observes the same stream-json events the supervisor relays to the UI and
// pairs every tool_use with its tool_result; it never alters the stream.
//
// The estimate (chars/4) is rough but good enough to rank tools against each
// other — which is the question we are answering. Per-call records and a
// per-thread roll-up at exit go to the supervisor's slog, so a single session
// of real use is enough to see whether Read, Bash, Grep or something else is
// the dominant cost.
type toolMeter struct {
	log      *slog.Logger
	threadID string

	mu      sync.Mutex
	pending map[string]pendingTool
	totals  map[string]toolTotals
}

type pendingTool struct {
	name      string
	input     json.RawMessage
	startedAt time.Time
}

type toolTotals struct {
	calls       int
	outputChars int
	outputLines int
}

// pendingCap bounds the in-flight tool_use map. A misbehaving agent that
// produces tool_use blocks without ever receiving matching tool_results
// (e.g. crashes mid-turn) cannot grow the map unboundedly.
const pendingCap = 1024

func newToolMeter(log *slog.Logger, threadID string) *toolMeter {
	return &toolMeter{
		log:      log,
		threadID: threadID,
		pending:  make(map[string]pendingTool),
		totals:   make(map[string]toolTotals),
	}
}

// Observe inspects one stream-json event from `claude` and records any
// tool_use or tool_result it carries. Events of other shapes are ignored.
func (m *toolMeter) Observe(event json.RawMessage) {
	var head struct {
		Type    string `json:"type"`
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(event, &head) != nil {
		return
	}
	switch head.Type {
	case "assistant":
		for _, blk := range head.Message.Content {
			m.recordToolUse(blk)
		}
	case "user":
		for _, blk := range head.Message.Content {
			m.recordToolResult(blk)
		}
	}
}

func (m *toolMeter) recordToolUse(blk json.RawMessage) {
	var t struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(blk, &t) != nil || t.Type != "tool_use" || t.ID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) >= pendingCap {
		// Evict any one entry — we have already lost correlation for this id.
		for k := range m.pending {
			delete(m.pending, k)
			break
		}
	}
	m.pending[t.ID] = pendingTool{name: t.Name, input: t.Input, startedAt: time.Now()}
}

func (m *toolMeter) recordToolResult(blk json.RawMessage) {
	var t struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(blk, &t) != nil || t.Type != "tool_result" || t.ToolUseID == "" {
		return
	}
	text := toolResultText(t.Content)
	chars := len(text)
	lines := strings.Count(text, "\n")
	if chars > 0 && !strings.HasSuffix(text, "\n") {
		lines++
	}

	m.mu.Lock()
	p, ok := m.pending[t.ToolUseID]
	delete(m.pending, t.ToolUseID)
	name := "(unknown)"
	var input json.RawMessage
	var dur time.Duration
	if ok {
		name = p.name
		input = p.input
		dur = time.Since(p.startedAt)
	}
	tot := m.totals[name]
	tot.calls++
	tot.outputChars += chars
	tot.outputLines += lines
	m.totals[name] = tot
	m.mu.Unlock()

	m.log.Info("tool_result size",
		"thread", m.threadID,
		"tool", name,
		"input", summarizeInput(name, input),
		"chars", chars,
		"lines", lines,
		"est_tokens", chars/4,
		"is_error", t.IsError,
		"dur_ms", dur.Milliseconds(),
	)
}

// Summary logs a per-tool roll-up for the thread; called when the thread exits.
func (m *toolMeter) Summary() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.totals) == 0 {
		return
	}
	for name, t := range m.totals {
		m.log.Info("tool_total",
			"thread", m.threadID,
			"tool", name,
			"calls", t.calls,
			"chars", t.outputChars,
			"lines", t.outputLines,
			"est_tokens", t.outputChars/4,
		)
	}
}

// toolResultText extracts plain text from a tool_result content field, which
// MCP/Anthropic frame as either a bare string or a list of typed content
// blocks. Non-text blocks (images, etc.) contribute nothing to char count.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" {
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		}
	}
	return ""
}

// summarizeInput renders a one-line view of a tool's input, highlighting the
// fields that drive context cost. Read in particular surfaces whether the
// agent windowed (offset/limit) or slurped the whole file.
func summarizeInput(name string, raw json.RawMessage) string {
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
			Glob    string `json:"glob"`
		}
		if json.Unmarshal(raw, &i) == nil {
			return fmt.Sprintf("%q in %s (glob=%s)", i.Pattern, i.Path, i.Glob)
		}
	}
	return truncate(string(raw), 120)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
