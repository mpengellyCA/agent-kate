package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// newTestMeter returns a meter whose log writes to the returned buffer, so
// tests can assert on the structured records it emits.
func newTestMeter(t *testing.T) (*toolMeter, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.Writer(&buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	return newToolMeter(log, "t-test"), &buf
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestToolMeterPairsUseAndResult(t *testing.T) {
	m, buf := newTestMeter(t)

	// Assistant emits a Read tool_use without a window — the kind of call we
	// most want to flag in the logs.
	use := mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "Read",
					"input": map[string]any{"file_path": "/repo/main.go"},
				},
			},
		},
	})
	// Tool result comes back as a typed-block list (Claude Code's usual shape).
	result := mustMarshal(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{"type": "text", "text": strings.Repeat("line\n", 50)},
					},
				},
			},
		},
	})

	m.Observe(use)
	m.Observe(result)

	out := buf.String()
	if !strings.Contains(out, `tool=Read`) {
		t.Errorf("log missing tool=Read; got:\n%s", out)
	}
	if !strings.Contains(out, `chars=250`) {
		t.Errorf("log missing chars=250; got:\n%s", out)
	}
	if !strings.Contains(out, `lines=50`) {
		t.Errorf("log missing lines=50; got:\n%s", out)
	}
	if !strings.Contains(out, `/repo/main.go [full]`) {
		t.Errorf("log missing full-window summary; got:\n%s", out)
	}
}

func TestToolMeterWindowedReadAndSummary(t *testing.T) {
	m, buf := newTestMeter(t)

	use := mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"id":   "toolu_2",
					"name": "Read",
					"input": map[string]any{
						"file_path": "/repo/x.go", "offset": 100, "limit": 40,
					},
				},
			},
		},
	})
	// Plain-string content form, also valid per the MCP spec.
	result := mustMarshal(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_2",
					"content": "short",
				},
			},
		},
	})
	m.Observe(use)
	m.Observe(result)

	if !strings.Contains(buf.String(), "offset=100 limit=40") {
		t.Errorf("log missing windowed summary; got:\n%s", buf.String())
	}

	// Summary should aggregate both Read calls (one from this test, one was
	// pre-recorded above — but each test gets a fresh meter, so it's just one).
	buf.Reset()
	m.Summary()
	got := buf.String()
	if !strings.Contains(got, "tool_total") || !strings.Contains(got, "calls=1") {
		t.Errorf("summary missing roll-up; got:\n%s", got)
	}
}

func TestToolMeterIgnoresOrphanResults(t *testing.T) {
	m, buf := newTestMeter(t)

	// A tool_result whose tool_use we never saw is still logged (with name
	// "(unknown)") so the cost is not invisible.
	orphan := mustMarshal(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_ghost",
					"content": "hi",
				},
			},
		},
	})
	m.Observe(orphan)
	if !strings.Contains(buf.String(), `tool=(unknown)`) {
		t.Errorf("orphan result not logged; got:\n%s", buf.String())
	}
}
