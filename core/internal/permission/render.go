package permission

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxPlanBytes      = 16 * 1024
	maxQuestionsBytes = 16 * 1024
)

// Summary returns a remote-safe description of a pending action. In contrast
// to the desktop's local transcript summary, it never incorporates tool
// arguments: a command line, path, URL, or text value can all be sensitive.
func Summary(toolName string) string {
	switch toolName {
	case "ExitPlanMode":
		return "Review and approve the agent plan"
	case "AskUserQuestion":
		return "Answer the agent's question"
	case "Bash":
		return "Approve a shell command"
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return "Approve a file change"
	default:
		if strings.TrimSpace(toolName) == "" {
			return "Approval required"
		}
		return "Approve " + clip(toolName, 128)
	}
}

// RenderableDetail extracts exactly the two fields a remote human must see to
// answer. All other input, including every ordinary tool argument, is dropped.
func RenderableDetail(toolName string, input json.RawMessage) Detail {
	switch toolName {
	case "ExitPlanMode":
		var payload struct {
			Plan string `json:"plan"`
		}
		if json.Unmarshal(input, &payload) != nil {
			return Detail{}
		}
		return Detail{Plan: clip(strings.TrimSpace(payload.Plan), maxPlanBytes)}
	case "AskUserQuestion":
		var payload struct {
			Questions json.RawMessage `json:"questions"`
		}
		if json.Unmarshal(input, &payload) != nil || len(payload.Questions) == 0 || len(payload.Questions) > maxQuestionsBytes || !json.Valid(payload.Questions) {
			return Detail{}
		}
		var questions []json.RawMessage
		if json.Unmarshal(payload.Questions, &questions) != nil {
			return Detail{}
		}
		return Detail{Questions: append(json.RawMessage(nil), payload.Questions...)}
	default:
		return Detail{}
	}
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
