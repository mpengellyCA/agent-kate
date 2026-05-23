package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func newTestUsageMeter(t *testing.T) (*usageMeter, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.Writer(&buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	return newUsageMeter(log, "t-test"), &buf
}

func TestUsageMeterAssistantTurn(t *testing.T) {
	m, buf := newTestUsageMeter(t)

	// A realistic assistant event with cache hits: prompt was 1010 tokens,
	// of which 900 came from cache — cache_hit_pct should be 89.
	ev := mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":    "msg_01",
			"model": "claude-opus-4-7",
			"role":  "assistant",
			"usage": map[string]any{
				"input_tokens":                100,
				"cache_creation_input_tokens": 10,
				"cache_read_input_tokens":     900,
				"output_tokens":               42,
			},
		},
	})
	m.Observe(ev)

	got := buf.String()
	if !strings.Contains(got, "turn_usage") {
		t.Errorf("missing turn_usage record; got:\n%s", got)
	}
	for _, want := range []string{
		`model=claude-opus-4-7`,
		`in=100`, `out=42`,
		`cache_read=900`, `cache_create=10`,
		`prompt_total=1010`, `cache_hit_pct=89`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("turn_usage missing %s; got:\n%s", want, got)
		}
	}
}

func TestUsageMeterEmptyUsageSkipped(t *testing.T) {
	m, buf := newTestUsageMeter(t)

	// Assistant message with no usage block at all — should not be logged
	// (otherwise the log fills with empty turns).
	ev := mustMarshal(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":    "msg_empty",
			"model": "claude-opus-4-7",
			"role":  "assistant",
		},
	})
	m.Observe(ev)
	if strings.Contains(buf.String(), "turn_usage") {
		t.Errorf("expected empty-usage assistant to be skipped; got:\n%s", buf.String())
	}
}

// TestUsageMeterDedupesByMessageID covers the real-world case observed in
// session logs: claude's `--verbose` stream emits the same assistant message
// multiple times as it streams, each with its own usage block. We must
// dedupe by message id, otherwise usage_total over-counts dramatically.
func TestUsageMeterDedupesByMessageID(t *testing.T) {
	m, buf := newTestUsageMeter(t)

	emit := func(id string, in, out, cacheRead int) {
		ev := mustMarshal(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":    id,
				"model": "claude-opus-4-7",
				"usage": map[string]any{
					"input_tokens":            in,
					"output_tokens":           out,
					"cache_read_input_tokens": cacheRead,
				},
			},
		})
		m.Observe(ev)
	}

	// One logical turn, emitted three times as streaming progresses; only
	// the third event has the final output_tokens.
	emit("msg_dup", 5, 1, 100)
	emit("msg_dup", 5, 20, 100)
	emit("msg_dup", 5, 46, 100)
	// A second, distinct turn.
	emit("msg_next", 1, 30, 110)

	// Only the first observation of each id should produce a turn_usage log.
	if got := strings.Count(buf.String(), "turn_usage"); got != 2 {
		t.Errorf("turn_usage logs = %d, want 2 (one per unique msg id); got:\n%s", got, buf.String())
	}

	// Summary must reflect the FINAL usage per id, not the sum of partials.
	buf.Reset()
	m.Summary()
	sum := buf.String()
	// Expected totals: in=5+1=6, out=46+30=76, cache_read=100+110=210.
	for _, want := range []string{
		`turns=2`, `in=6`, `out=76`, `cache_read=210`,
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("usage_total missing %s; got:\n%s", want, sum)
		}
	}
}

func TestUsageMeterSessionResultAndSummary(t *testing.T) {
	m, buf := newTestUsageMeter(t)

	// Two turns, then the session result event from claude.
	turn := func(in, out, cacheRead, cacheCreate int) json.RawMessage {
		return mustMarshal(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model": "claude-opus-4-7",
				"usage": map[string]any{
					"input_tokens":                in,
					"output_tokens":               out,
					"cache_read_input_tokens":     cacheRead,
					"cache_creation_input_tokens": cacheCreate,
				},
			},
		})
	}
	m.Observe(turn(50, 20, 400, 0))
	m.Observe(turn(30, 18, 800, 0))

	result := mustMarshal(t, map[string]any{
		"type":           "result",
		"subtype":        "success",
		"num_turns":      2,
		"duration_ms":    12345,
		"total_cost_usd": 0.0237,
		"usage": map[string]any{
			"input_tokens":            80,
			"output_tokens":           38,
			"cache_read_input_tokens": 1200,
		},
	})
	m.Observe(result)

	got := buf.String()
	if !strings.Contains(got, `session_usage`) {
		t.Errorf("missing session_usage record; got:\n%s", got)
	}
	for _, want := range []string{
		`num_turns=2`, `cost_usd=0.0237`, `duration_ms=12345`,
		`in=80`, `out=38`, `cache_read=1200`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("session_usage missing %s; got:\n%s", want, got)
		}
	}

	// Summary should match what we accumulated turn by turn.
	buf.Reset()
	m.Summary()
	sum := buf.String()
	for _, want := range []string{
		`usage_total`, `turns=2`,
		`in=80`, `out=38`,
		`cache_read=1200`,
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("usage_total summary missing %s; got:\n%s", want, sum)
		}
	}
}
