package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContextEventAfterTurn is the round trip that matters for the context
// meter: a completed turn triggers get_context_usage against the live CLI over
// the already-open stdin, and the answer reaches the UI as a `_context` event
// carrying the authoritative fill plus the category breakdown. The panel's
// result-derived estimate is only the fallback for when this does not arrive.
func TestContextEventAfterTurn(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	if _, err := sup.Start(StartOptions{ID: "t-ctx1", WorkDir: t.TempDir(), Prompt: "hello"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "context event", func(ev map[string]any) bool {
		return ev["type"] == "_context"
	})

	var got map[string]any
	for _, raw := range col.snapshot() {
		var ev map[string]any
		if json.Unmarshal(raw, &ev) == nil && ev["type"] == "_context" {
			got = ev
		}
	}
	if got["usedTokens"].(float64) != 41000 {
		t.Errorf("usedTokens = %v, want 41000", got["usedTokens"])
	}
	if got["maxTokens"].(float64) != 200000 {
		t.Errorf("maxTokens = %v, want 200000", got["maxTokens"])
	}
	breakdown, _ := got["breakdown"].([]any)
	if len(breakdown) != 3 {
		t.Fatalf("breakdown has %d categories, want 3: %v", len(breakdown), breakdown)
	}
	// Largest category first, so a tooltip reads top-down.
	first := breakdown[0].(map[string]any)
	if first["label"] != "Messages" || first["tokens"].(float64) != 30000 {
		t.Errorf("first breakdown row = %v, want Messages/30000", first)
	}
	sup.StopAll()
}

// TestSetEffortLive pins the live-effort lever: claude has no set_effort
// control request, so the effort tier is translated to a thinking-token budget
// and sent as set_max_thinking_tokens on the running session. An unknown tier
// is refused locally rather than sent as a malformed request.
func TestSetEffortLive(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-eff1", WorkDir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "turn result", isResult)

	if err := sup.SetEffort(th.ID, "high"); err != nil {
		t.Errorf("SetEffort(high): %v", err)
	}
	if err := sup.SetEffort(th.ID, "sideways"); err == nil {
		t.Error("SetEffort(sideways) succeeded; an unknown tier must be refused")
	}
	if err := sup.ReloadSkills(th.ID); err != nil {
		t.Errorf("ReloadSkills: %v", err)
	}
	sup.StopAll()
}

// TestEffortThinkingTokensMonotonic keeps the tier -> budget mapping ordered:
// a higher effort must never buy a smaller thinking budget, and every tier in
// the harness's declared vocabulary must be expressible.
func TestEffortThinkingTokensMonotonic(t *testing.T) {
	prev := 0
	for _, tier := range []string{"low", "medium", "high", "xhigh", "max"} {
		n, ok := EffortThinkingTokens(tier)
		if !ok {
			t.Fatalf("effort %q has no thinking-token budget", tier)
		}
		if n <= prev {
			t.Errorf("effort %q budget %d is not above the previous tier's %d", tier, n, prev)
		}
		prev = n
	}
	if _, ok := EffortThinkingTokens("nope"); ok {
		t.Error("an unknown tier reported a budget")
	}
}

// TestListModelsFromRunningThread covers the replacement for the old `/model`
// prose scrape: discovery prefers a session the human already has open, and the
// structured answer carries per-model effort support the prose never did.
func TestListModelsFromRunningThread(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-lm1", WorkDir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "turn result", isResult)

	if id := sup.anyRunningThread(); id != th.ID {
		t.Fatalf("anyRunningThread = %q, want %q", id, th.ID)
	}
	models, err := sup.listModels(th.ID)
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(models) != 2 || models[0].Value != "opus" || models[0].Name != "Opus 5" {
		t.Fatalf("models = %+v", models)
	}
	if strings.Join(models[1].Efforts, ",") != "low,medium" {
		t.Errorf("haiku efforts = %v, want [low medium]", models[1].Efforts)
	}
	sup.StopAll()
}

// capturedContextUsageLine and capturedModelListLine are VERBATIM control
// responses captured off a live `claude` 2.1.220 over the stream-json control
// channel — not invented shapes. Both nest the answer under response.response;
// an earlier generation of these fixtures guessed a flat shape, which certified
// two parsers that could never match the real CLI. Do not "simplify" them: if
// they stop looking like what the binary sends, they stop being evidence.
// (The category list is abridged exactly where the capture was.)
const (
	capturedContextUsageLine = `{"type":"control_response","response":{"subtype":"success","request_id":"ak-get_context_usage-1","response":{"categories":[{"name":"System prompt","tokens":3231}],"totalTokens":18074,"maxTokens":1000000}}}`
	capturedModelListLine    = `{"type":"control_response","response":{"subtype":"success","request_id":"ak-list_models-1","response":{"models":[{"value":"opus[1m]","displayName":"Opus (1M context)","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},{"value":"haiku","displayName":"Haiku 4.5","supportsEffort":false}]}}}`
)

// controlPayload peels a captured control_response line down to the object the
// parsers are handed, exactly as observeControlResponse does at runtime.
func controlPayload(t *testing.T, line string) json.RawMessage {
	t.Helper()
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("captured line is not JSON: %v", err)
	}
	return env.Response
}

// TestParseContextUsageCaptured is the test the old suite should have been:
// the real 2.1.220 payload in, a usable reading out.
func TestParseContextUsageCaptured(t *testing.T) {
	got, ok := parseContextUsage(controlPayload(t, capturedContextUsageLine))
	if !ok {
		t.Fatal("the captured live payload was refused")
	}
	if got.UsedTokens != 18074 || got.MaxTokens != 1000000 {
		t.Errorf("used/max = %d/%d, want 18074/1000000", got.UsedTokens, got.MaxTokens)
	}
	if len(got.Breakdown) != 1 || got.Breakdown[0].Label != "System prompt" ||
		got.Breakdown[0].Tokens != 3231 {
		t.Errorf("breakdown = %+v, want [{System prompt 3231}]", got.Breakdown)
	}
	// The label arrives as prose already; prettifying it again would mangle
	// multi-word names like "MCP tools" into "M c p tools".
	ev := got.JSON()
	if ev["type"] != "_context" || ev["usedTokens"].(int64) != 18074 {
		t.Errorf("_context event = %v", ev)
	}
}

// TestParseContextUsageShapes exercises the normaliser against the payload
// spellings the undocumented control channel has used, plus the shapes it must
// refuse — a refused reading leaves the panel on its estimate, which is far
// better than a meter that silently reads zero. These are tolerance cases only;
// the captured shape is covered above.
func TestParseContextUsageShapes(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantUsed    int64
		wantMax     int64
		wantTopCat  string
		wantRefused bool
	}{
		{
			name:       "snake case, map breakdown",
			payload:    `{"used_tokens":100,"max_tokens":1000,"breakdown":{"messages":80,"system_prompt":20}}`,
			wantUsed:   100,
			wantMax:    1000,
			wantTopCat: "Messages",
		},
		{
			name:       "captured nesting, categories ordered largest first",
			payload:    `{"subtype":"success","response":{"totalTokens":41000,"maxTokens":200000,"categories":[{"name":"System prompt","tokens":3000},{"name":"Messages","tokens":30000}]}}`,
			wantUsed:   41000,
			wantMax:    200000,
			wantTopCat: "Messages",
		},
		{
			name:       "camel case, array breakdown of objects",
			payload:    `{"usedTokens":7,"contextWindow":70,"categories":[{"name":"mcpTools","tokens":5},{"name":"messages","tokens":2}]}`,
			wantUsed:   7,
			wantMax:    70,
			wantTopCat: "Mcp tools",
		},
		{
			name:     "nested under a wrapper",
			payload:  `{"subtype":"success","context_usage":{"total_tokens":12,"limit":24}}`,
			wantUsed: 12,
			wantMax:  24,
		},
		{
			name:        "no token figures at all",
			payload:     `{"subtype":"success"}`,
			wantRefused: true,
		},
		{
			name:        "zero fill is not a reading",
			payload:     `{"used_tokens":0,"max_tokens":1000}`,
			wantRefused: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseContextUsage(json.RawMessage(tc.payload))
			if tc.wantRefused {
				if ok {
					t.Fatalf("payload was accepted as %+v; it should be refused", got)
				}
				return
			}
			if !ok {
				t.Fatal("payload was refused")
			}
			if got.UsedTokens != tc.wantUsed || got.MaxTokens != tc.wantMax {
				t.Errorf("used/max = %d/%d, want %d/%d",
					got.UsedTokens, got.MaxTokens, tc.wantUsed, tc.wantMax)
			}
			if tc.wantTopCat != "" {
				if len(got.Breakdown) == 0 || got.Breakdown[0].Label != tc.wantTopCat {
					t.Errorf("top breakdown row = %+v, want %q", got.Breakdown, tc.wantTopCat)
				}
			}
		})
	}
}

// TestParseModelListCaptured runs the real 2.1.220 list_models payload through
// the normaliser: nested under response.response, entries keyed
// value/displayName/supportedEffortLevels.
func TestParseModelListCaptured(t *testing.T) {
	models := parseModelList(controlPayload(t, capturedModelListLine))
	if len(models) != 2 {
		t.Fatalf("got %d models from the captured payload: %+v", len(models), models)
	}
	if models[0].Value != "opus[1m]" || models[0].Name != "Opus (1M context)" {
		t.Errorf("first model = %+v", models[0])
	}
	if strings.Join(models[0].Efforts, ",") != "low,medium,high,xhigh,max" {
		t.Errorf("opus efforts = %v", models[0].Efforts)
	}
	// supportsEffort:false with no level list is "no claim", which callers read
	// as every tier — the contract has no way to say "none", and withholding
	// tiers would block a picker where offering unused ones only looks odd.
	if len(models[1].Efforts) != 0 {
		t.Errorf("haiku efforts = %v, want empty", models[1].Efforts)
	}
}

// TestParseModelListShapes pins the list_models normaliser, including the
// rule that matters most downstream: no effort field means "the CLI said
// nothing", which callers read as every tier allowed — never as none.
func TestParseModelListShapes(t *testing.T) {
	models := parseModelList(json.RawMessage(
		`{"models":[{"id":"opus","display_name":"Opus 5","efforts":["low","max"]},
		            {"id":"haiku"},
		            {"id":"opus"}]}`))
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 (the duplicate id must collapse): %+v", len(models), models)
	}
	if models[0].Value != "opus" || models[0].Name != "Opus 5" {
		t.Errorf("first model = %+v", models[0])
	}
	if len(models[1].Efforts) != 0 {
		t.Errorf("haiku efforts = %v, want empty (no claim, not an empty allow-list)", models[1].Efforts)
	}
	if models[1].Name != "Haiku" {
		t.Errorf("haiku display name = %q, want the prettified alias", models[1].Name)
	}

	// A bare string vocabulary is still a model list.
	bare := parseModelList(json.RawMessage(`{"data":["opus","sonnet"]}`))
	if len(bare) != 2 || bare[0].Value != "opus" {
		t.Errorf("bare list = %+v", bare)
	}

	// A support map instead of a list, and a shape with no models at all.
	sup := parseModelList(json.RawMessage(
		`{"models":[{"id":"x","supportedEfforts":{"low":true,"max":false,"high":true}}]}`))
	if len(sup) != 1 || strings.Join(sup[0].Efforts, ",") != "high,low" {
		t.Errorf("support-map efforts = %+v", sup)
	}
	if got := parseModelList(json.RawMessage(`{"ok":true}`)); len(got) != 0 {
		t.Errorf("empty payload yielded %+v", got)
	}
}
