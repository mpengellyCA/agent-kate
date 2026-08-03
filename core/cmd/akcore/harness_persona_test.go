package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agentkate/internal/harness"
)

// TestBuildAgentsJSONShape pins the --agents payload against what claude
// 2.1.220 was probed to accept: the profile NAME is the object key (a "name"
// field is ignored), "tools" is a JSON ARRAY (a comma-separated string makes
// the CLI drop the whole entry), and "model" is carried through — a probe
// confirmed a profile's subagent turns really run on it. Every field of a
// well-formed profile is applied, so nothing is reported unapplied.
func TestBuildAgentsJSONShape(t *testing.T) {
	payload, applied := buildAgentsJSON([]harness.AgentProfile{{
		Name:        "reviewer",
		Description: "Reviews code",
		Prompt:      "You are a code reviewer.",
		Tools:       []string{"Read", "Glob"},
		Model:       "sonnet",
	}, {
		Name:        "scout",
		Description: "Finds things",
		Prompt:      "You are a scout.",
	}})

	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%s)", err, payload)
	}
	rev, ok := got["reviewer"]
	if !ok {
		t.Fatalf("profile not keyed by name: %s", payload)
	}
	if rev["description"] != "Reviews code" || rev["prompt"] != "You are a code reviewer." {
		t.Errorf("reviewer = %v", rev)
	}
	if rev["model"] != "sonnet" {
		t.Errorf("model = %v, want sonnet", rev["model"])
	}
	tools, isArray := rev["tools"].([]any)
	if !isArray || len(tools) != 2 || tools[0] != "Read" {
		t.Errorf("tools = %#v, want a JSON array [Read Glob]", rev["tools"])
	}
	if _, hasName := rev["name"]; hasName {
		t.Error("the entry carries a redundant name field; the key IS the name")
	}
	// A profile with no tool allow-list must omit the key entirely — an empty
	// array would read as "no tools at all".
	if _, hasTools := got["scout"]["tools"]; hasTools {
		t.Errorf("scout carries a tools key with none requested: %v", got["scout"])
	}

	if len(applied) != 2 {
		t.Fatalf("applied = %v, want one entry per requested profile", applied)
	}
	for _, a := range applied {
		if !a.Applied || len(a.Unapplied) != 0 {
			t.Errorf("%s reported as not fully applied: %+v", a.Name, a)
		}
	}
}

// TestBuildAgentsJSONRefusesSilentDrops covers the profiles claude would
// discard without a word: the CLI validates nothing, so a profile missing a
// description or prompt (or colliding on a name) would simply never exist. The
// adapter refuses those up front and names them instead.
func TestBuildAgentsJSONRefusesSilentDrops(t *testing.T) {
	payload, applied := buildAgentsJSON([]harness.AgentProfile{
		{Name: "", Description: "d", Prompt: "p"},
		{Name: "nodesc", Prompt: "p"},
		{Name: "noprompt", Description: "d"},
		{Name: "dup", Description: "d", Prompt: "p"},
		{Name: "dup", Description: "d2", Prompt: "p2"},
	})
	if len(applied) != 5 {
		t.Fatalf("applied = %v, want one entry per requested profile", applied)
	}
	for i, a := range applied {
		wantApplied := i == 3 // only the first "dup" survives
		if a.Applied != wantApplied {
			t.Errorf("profile %d (%q) Applied = %v, want %v", i, a.Name, a.Applied, wantApplied)
		}
		if !wantApplied && len(a.Unapplied) == 0 {
			t.Errorf("profile %d (%q) dropped without a reason", i, a.Name)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if len(got) != 1 || got["dup"] == nil {
		t.Errorf("payload = %s, want only the first dup entry", payload)
	}
	// Nothing requested at all means no flag at all.
	if p, a := buildAgentsJSON(nil); p != "" || a != nil {
		t.Errorf("empty request produced %q / %v", p, a)
	}
	// Every profile refused means no flag either — an empty object would still
	// be a --agents argument.
	if p, a := buildAgentsJSON([]harness.AgentProfile{{Name: "x"}}); p != "" || len(a) != 1 {
		t.Errorf("all-refused request produced %q / %v", p, a)
	}
}

// TestPersonaArgvLimits covers the guard against an opaque E2BIG: each persona
// flag is ONE argv element, capped by the kernel at MAX_ARG_STRLEN, so an
// oversize one is dropped with a reason naming the limit instead of failing
// the whole spawn. Blank text is treated as no request, the same rule
// buildStartArgs and unappliedPersona use.
func TestPersonaArgvLimits(t *testing.T) {
	for _, tc := range []struct {
		name           string
		in             string
		wantPass       bool
		wantReasonPart string
	}{
		{"ordinary", "You are the scout.", true, ""},
		{"empty", "", false, ""},
		{"blank", "   \n\t ", false, ""},
		{"at the limit", strings.Repeat("x", maxArgBytes), true, ""},
		{"over the limit", strings.Repeat("x", maxArgBytes+1), false, "MAX_ARG_STRLEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass, whyNot := personaSystemPrompt(tc.in)
			if (pass != "") != tc.wantPass {
				t.Errorf("passed %d bytes, want passed=%v", len(pass), tc.wantPass)
			}
			if tc.wantReasonPart == "" {
				if whyNot != "" {
					t.Errorf("unexpected reason %q", whyNot)
				}
			} else if !strings.Contains(whyNot, tc.wantReasonPart) ||
				!strings.Contains(whyNot, "131071") {
				t.Errorf("reason = %q, want it to name the limit", whyNot)
			}
		})
	}

	// The agents payload is the other single element: too big means NONE of
	// the profiles registered, so every one of them says so.
	huge := harness.AgentProfile{
		Name: "big", Description: "d", Prompt: strings.Repeat("p", maxArgBytes),
	}
	small := harness.AgentProfile{Name: "small", Description: "d", Prompt: "p"}
	payload, applied := buildAgentsJSON([]harness.AgentProfile{huge, small})
	if payload != "" {
		t.Errorf("oversize payload passed anyway (%d bytes)", len(payload))
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %+v", applied)
	}
	for _, a := range applied {
		if a.Applied || len(a.Unapplied) != 1 ||
			!strings.Contains(a.Unapplied[0], "MAX_ARG_STRLEN") {
			t.Errorf("%s = %+v, want refused with the size reason", a.Name, a)
		}
	}
}

// TestUnappliedPersonaUsesAdapterReason: an adapter that knows WHY (an
// oversize prompt) must not have its answer replaced by the generic
// "unsupported" wording — that would be a lie about a harness that has the
// capability.
func TestUnappliedPersonaUsesAdapterReason(t *testing.T) {
	descriptor := newClaudeHarness(nil, "", "").Descriptor()
	got := unappliedPersona("a very long persona", nil, harness.Launched{
		SystemPromptUnapplied: tooLongForArgv("the system prompt", 200000),
	}, descriptor)
	if len(got) != 1 || !strings.Contains(got[0]["reason"], "MAX_ARG_STRLEN") {
		t.Fatalf("unapplied = %v, want the adapter's own reason", got)
	}
	if strings.Contains(got[0]["reason"], "not supported") {
		t.Errorf("adapter reason overwritten with the capability wording: %v", got[0])
	}

	// A verdict with no explanation says only what is known — claiming
	// "not supported by Claude Code" would be wrong for a harness that has
	// the capability.
	got = unappliedPersona("", []harness.AgentProfile{{Name: "x"}}, harness.Launched{
		Agents: []harness.AppliedAgent{{Name: "x"}},
	}, descriptor)
	if len(got) != 1 || got[0]["reason"] != "not applied; the harness gave no reason" {
		t.Fatalf("fallback reason = %v", got)
	}
}

// TestKimiPersonaUnapplied pins the honest gate: `kimi acp` has no
// system-prompt channel and resolves subagents from a compiled-in set, so both
// capabilities are false and Launch reports every requested profile as not
// applied — with a reason, never silently and never emulated with files the
// running agent could not read.
func TestKimiPersonaUnapplied(t *testing.T) {
	descriptor := newKimiHarness(nil, "", "").Descriptor()
	if descriptor.Supports(harness.OperationSystemPrompt) || descriptor.Supports(harness.OperationCustomSubagents) {
		t.Fatalf("kimi descriptor incorrectly exposes persona operations")
	}
	agents := harness.UnappliedAgents([]harness.AgentProfile{
		{Name: "reviewer", Description: "d", Prompt: "p"},
		{Name: "scout", Description: "d", Prompt: "p"},
	}, kimiNoCustomSubagents)
	if len(agents) != 2 {
		t.Fatalf("agents = %v", agents)
	}
	for _, a := range agents {
		if a.Applied || len(a.Unapplied) != 1 || a.Unapplied[0] != kimiNoCustomSubagents {
			t.Errorf("%s = %+v", a.Name, a)
		}
	}

	// The launch_agent report the controller actually reads.
	unapplied := unappliedPersona("You are the scout.",
		[]harness.AgentProfile{{Name: "reviewer"}, {Name: "scout"}},
		harness.Launched{Agents: agents}, descriptor)
	if len(unapplied) != 3 {
		t.Fatalf("unapplied = %v, want the system prompt plus both profiles", unapplied)
	}
	if unapplied[0]["option"] != "system_prompt" ||
		!strings.Contains(unapplied[0]["reason"], "Kimi Code") {
		t.Errorf("system prompt entry = %v", unapplied[0])
	}
	if unapplied[1]["option"] != "agents[reviewer]" ||
		unapplied[2]["option"] != "agents[scout]" {
		t.Errorf("profile entries = %v, %v", unapplied[1], unapplied[2])
	}
}

// TestUnappliedPersonaAppliedIsSilent covers the claude side of the same
// report: a fully applied persona names nothing, so the controller only ever
// reads about real downgrades.
func TestUnappliedPersonaAppliedIsSilent(t *testing.T) {
	descriptor := newClaudeHarness(nil, "", "").Descriptor()
	if !descriptor.Supports(harness.OperationSystemPrompt) || !descriptor.Supports(harness.OperationCustomSubagents) {
		t.Fatalf("claude descriptor does not expose persona operations")
	}
	profiles := []harness.AgentProfile{{Name: "reviewer", Description: "d", Prompt: "p"}}
	_, applied := buildAgentsJSON(profiles)
	got := unappliedPersona("You are the scout.", profiles,
		harness.Launched{SystemPromptApplied: true, Agents: applied}, descriptor)
	if len(got) != 0 {
		t.Fatalf("fully applied persona reported as unapplied: %v", got)
	}
	// Requesting nothing reports nothing, whatever the capabilities say.
	if got := unappliedPersona("", nil, harness.Launched{},
		newKimiHarness(nil, "", "").Descriptor()); len(got) != 0 {
		t.Fatalf("nothing requested reported as unapplied: %v", got)
	}
}

// TestUnappliedPersonaBackstop guards the applied-truth contract against a
// future adapter that ignores StartSpec.Agents without reporting anything:
// a profile with no verdict must surface as unapplied, never vanish.
func TestUnappliedPersonaBackstop(t *testing.T) {
	got := unappliedPersona("", []harness.AgentProfile{{Name: "ghost"}, {Name: ""}},
		harness.Launched{}, newKimiHarness(nil, "", "").Descriptor())
	if len(got) != 2 {
		t.Fatalf("unreported profiles = %v, want both named", got)
	}
	if got[0]["option"] != "agents[ghost]" || got[1]["option"] != "agents[(unnamed)]" {
		t.Errorf("entries = %v", got)
	}
	for _, e := range got {
		if e["reason"] == "" {
			t.Errorf("entry without a reason: %v", e)
		}
	}
}
