package compact

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// transcript builds a small two-turn transcript: a user message, an assistant
// turn with a Read tool_use, the matching tool_result with a fat body, another
// user message, and a final assistant text turn. The fat body is the kind of
// content we expect to be stripped.
func transcript(t *testing.T) []json.RawMessage {
	t.Helper()
	fatBody := strings.Repeat("line of source code\n", 500)
	return []json.RawMessage{
		// Noise we expect to skip.
		mustMarshal(t, map[string]any{"type": "system", "subtype": "init", "model": "claude-opus-4-7"}),

		// Turn 1.
		mustMarshal(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "find the auth bug"}},
			},
		}),
		mustMarshal(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "I'll start by reading the login handler."},
					map[string]any{
						"type": "tool_use", "id": "toolu_1", "name": "Read",
						"input": map[string]any{"file_path": "/repo/auth/login.go"},
					},
				},
			},
		}),
		mustMarshal(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_1",
					"content": []any{map[string]any{"type": "text", "text": fatBody}},
				}},
			},
		}),

		// Turn 2.
		mustMarshal(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "now run the tests"}},
			},
		}),
		mustMarshal(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "Done — 3 failures."},
					map[string]any{
						"type": "tool_use", "id": "toolu_2", "name": "Bash",
						"input": map[string]any{"command": "go test ./..."},
					},
				},
			},
		}),

		// Noise we expect to skip.
		mustMarshal(t, map[string]any{"type": "result", "subtype": "success", "result": "ok"}),
	}
}

func TestProgrammaticKeepsUsersAndDecisions(t *testing.T) {
	sum := Programmatic("t-1", "sess-1", transcript(t))

	if !strings.Contains(sum.Body, "find the auth bug") {
		t.Errorf("body missing first user message:\n%s", sum.Body)
	}
	if !strings.Contains(sum.Body, "now run the tests") {
		t.Errorf("body missing second user message:\n%s", sum.Body)
	}
	if !strings.Contains(sum.Body, "I'll start by reading the login handler.") {
		t.Errorf("body missing assistant text:\n%s", sum.Body)
	}
	if !strings.Contains(sum.Body, "Done — 3 failures.") {
		t.Errorf("body missing assistant text:\n%s", sum.Body)
	}
}

func TestProgrammaticStripsToolResultBodies(t *testing.T) {
	sum := Programmatic("t-1", "sess-1", transcript(t))

	if strings.Contains(sum.Body, "line of source code") {
		t.Errorf("body should not contain the fat tool_result content:\n%s", sum.Body)
	}
	// The tool_use args still survive so the LLM knows the call happened.
	if !strings.Contains(sum.Body, "Read: /repo/auth/login.go [full]") {
		t.Errorf("body missing Read tool_use line:\n%s", sum.Body)
	}
	if !strings.Contains(sum.Body, "Bash: go test ./...") {
		t.Errorf("body missing Bash tool_use line:\n%s", sum.Body)
	}
}

func TestProgrammaticDropsSystemAndResultEvents(t *testing.T) {
	sum := Programmatic("t-1", "sess-1", transcript(t))

	if strings.Contains(sum.Body, "claude-opus-4-7") {
		t.Errorf("body should not echo system event payload:\n%s", sum.Body)
	}
	if strings.Contains(strings.ToLower(sum.Body), "subtype") {
		t.Errorf("body should not include result event metadata:\n%s", sum.Body)
	}
}

func TestProgrammaticRecordsLastUserRequest(t *testing.T) {
	sum := Programmatic("t-1", "sess-1", transcript(t))

	if !strings.Contains(sum.Body, "Last user request") {
		t.Errorf("body missing 'Last user request' section:\n%s", sum.Body)
	}
	// Verify the LAST one is what is highlighted, not the first.
	last := sum.Body[strings.Index(sum.Body, "Last user request"):]
	if !strings.Contains(last, "now run the tests") {
		t.Errorf("highlighted last user request should be the most recent one:\n%s", last)
	}
}

func TestProgrammaticTurnCount(t *testing.T) {
	sum := Programmatic("t-1", "sess-1", transcript(t))
	if sum.Turns != 2 {
		t.Errorf("expected 2 user turns, got %d", sum.Turns)
	}
}

func TestProgrammaticStampsMetadata(t *testing.T) {
	sum := Programmatic("t-zz", "sess-zz", transcript(t))
	if sum.ThreadID != "t-zz" || sum.SessionID != "sess-zz" {
		t.Errorf("metadata not stamped: %#v", sum)
	}
	if sum.Strategy != ResumeLocal {
		t.Errorf("programmatic summary should claim ResumeLocal strategy, got %q", sum.Strategy)
	}
	if sum.Created.IsZero() {
		t.Error("Created should be set")
	}
}

func TestProgrammaticHandlesEmptyTranscript(t *testing.T) {
	sum := Programmatic("t-empty", "", nil)
	if sum.Turns != 0 {
		t.Errorf("empty transcript should have 0 turns, got %d", sum.Turns)
	}
	if !strings.Contains(sum.Body, "Prior session context") {
		t.Errorf("body should still include header:\n%s", sum.Body)
	}
	if strings.Contains(sum.Body, "Last user request") {
		t.Errorf("body should not include 'Last user request' when there are no user turns:\n%s", sum.Body)
	}
}

func TestProgrammaticSkipsToolResultOnlyUserMessages(t *testing.T) {
	// A user message that is purely a tool_result should not be counted as a
	// human turn — it is the response to a previous tool_use.
	events := []json.RawMessage{
		mustMarshal(t, map[string]any{
			"type": "user",
			"message": map[string]any{
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_x", "content": "result",
				}},
			},
		}),
	}
	sum := Programmatic("t-1", "", events)
	if sum.Turns != 0 {
		t.Errorf("pure tool_result message should not count as a user turn, got %d", sum.Turns)
	}
}
