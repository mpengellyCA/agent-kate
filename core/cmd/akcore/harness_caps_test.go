package main

import (
	"testing"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
)

// registryWith builds a one-harness registry, so the positional
// permissive-mode rule can be exercised without a whole core.
func registryWith(h harness.Harness) *harness.Registry {
	reg := harness.NewRegistry("claude")
	reg.Register(h)
	return reg
}

// TestHarnessCapabilities freezes each adapter's capability set. A flipped
// flag here silently enables (or hides) whole UI affordances and RPC families
// — provider routing, compaction, forking — so any change must be deliberate.
func TestHarnessCapabilities(t *testing.T) {
	claude := newClaudeHarness(nil, "", "").Capabilities()
	if claude.ID != "claude" || claude.DisplayName != "Claude Code" || claude.Badge != "" {
		t.Errorf("claude identity = %q/%q/%q", claude.ID, claude.DisplayName, claude.Badge)
	}
	for name, got := range map[string]bool{
		"Fork":              claude.Fork,
		"Compaction":        claude.Compaction,
		"ColdCompact":       claude.ColdCompact,
		"Promote":           claude.Promote,
		"ProviderRouting":   claude.ProviderRouting,
		"Cowork":            claude.Cowork,
		"UsageReporting":    claude.UsageReporting,
		"SessionBrowse":     claude.SessionBrowse,
		"TranscriptPreview": claude.TranscriptPreview,
		"MintsSessionID":    claude.MintsSessionID,
		// Verified against claude 2.1.220 in print mode: the persona reaches
		// the model via --append-system-prompt, and --agents registers custom
		// subagents whose tools and model are both honored.
		"SystemPrompt":    claude.SystemPrompt,
		"CustomSubagents": claude.CustomSubagents,
		// The plan 16 P6 sweep, all three verified present on claude 2.1.220:
		// --fallback-model (one comma-separated value), --disallowedTools and
		// --add-dir (both variadic).
		"FallbackModels":  claude.FallbackModels,
		"DisallowedTools": claude.DisallowedTools,
		"AddDirs":         claude.AddDirs,
		// subagents/agent-<id>.jsonl beside the session transcript.
		"SubagentTranscripts": claude.SubagentTranscripts,
		// The control-channel sweep, both verified in `claude -p --help` on
		// 2.1.220: --strict-mcp-config and --max-budget-usd.
		"StrictMCPConfig": claude.StrictMCPConfig,
		"CostBudget":      claude.CostBudget,
		// No set_effort control request exists, but set_max_thinking_tokens is
		// accepted mid-session and the effort tiers ARE thinking-token budgets
		// underneath — so the lever is real. MUST stay in lockstep with
		// ui/src/state/HarnessTraits.cpp claudeDefaults().
		"EffortLive": claude.EffortLive,
	} {
		if !got {
			t.Errorf("claude capability %s = false, want true", name)
		}
	}
	// Every declared effort tier must be expressible as a thinking-token
	// budget, or SetOption("effort") would refuse a value the picker offered.
	for _, tier := range claude.Efforts {
		if _, ok := agent.EffortThinkingTokens(tier); !ok {
			t.Errorf("claude effort %q has no thinking-token budget", tier)
		}
	}
	// Claude's models are discovered live (`claude -p /model` for direct, the
	// provider's /v1/models for routed) — no longer a fixed tier vocabulary.
	if claude.ModelPicker != harness.ModelPickerDiscovered {
		t.Errorf("claude ModelPicker = %q, want discovered", claude.ModelPicker)
	}
	if len(claude.PermissionModes) == 0 || len(claude.Efforts) == 0 {
		t.Error("claude static mode/effort vocabularies must be non-empty")
	}
	// The modes claude 2.1.220 accepts AND HONORS in print mode. ORDER IS
	// LOAD-BEARING: permissiveModes() reads the LAST entry as the unattended
	// end, which the ensemble master prompt hands to workers.
	//
	// "manual" is deliberately absent. `--permission-mode manual` is a valid
	// CLI choice, but a print-mode session launched with it reports
	// "permissionMode":"default" in its init event — accepted, then silently
	// downgraded. Offering it would promise supervision the session does not
	// have, so it must not come back without a fresh probe showing the CLI
	// actually honors it.
	wantModes := []string{
		"acceptEdits", "default", "plan", "auto",
		"dontAsk", "bypassPermissions",
	}
	for _, m := range claude.PermissionModes {
		if m == "manual" {
			t.Error(`"manual" is back in claude's vocabulary: the CLI accepts ` +
				`the flag but downgrades the session to "default" in print mode`)
		}
	}
	if len(claude.PermissionModes) != len(wantModes) {
		t.Fatalf("claude PermissionModes = %v, want %v", claude.PermissionModes, wantModes)
	}
	for i, want := range wantModes {
		if claude.PermissionModes[i] != want {
			t.Errorf("claude PermissionModes[%d] = %q, want %q",
				i, claude.PermissionModes[i], want)
		}
	}
	if got := permissiveModes(registryWith(newClaudeHarness(nil, "", "")))["claude"]; got != "bypassPermissions" {
		t.Errorf("permissive mode for claude = %q, want bypassPermissions", got)
	}

	kimi := newKimiHarness(nil, "", "").Capabilities()
	if kimi.ID != "kimi" || kimi.DisplayName != "Kimi Code" || kimi.Badge != "Kimi" {
		t.Errorf("kimi identity = %q/%q/%q", kimi.ID, kimi.DisplayName, kimi.Badge)
	}
	for name, got := range map[string]bool{
		"Fork": kimi.Fork,
		// Hot-only. There is no `kimi --resume --print` and no claude-shaped
		// transcript on disk, so the cold paths (compactNow's local/model
		// branch, the exit-compaction drain) must never run here: they would
		// store a summary of nothing, and the next resume seeds a NEW session
		// from a stored summary — wiping the thread's continuity.
		"ColdCompact":       kimi.ColdCompact,
		"Promote":           kimi.Promote,
		"ProviderRouting":   kimi.ProviderRouting,
		"UsageReporting":    kimi.UsageReporting,
		"TranscriptPreview": kimi.TranscriptPreview,
		"MintsSessionID":    kimi.MintsSessionID,
		// `kimi acp` exposes no system-prompt channel, and its v1 engine
		// resolves subagents from a COMPILED-IN table (coder/explore/plan) —
		// the .agents/agents/*.md catalogue belongs to the v2 engine, which is
		// reachable only through `kimi -p` with KIMI_CODE_EXPERIMENTAL_FLAG=1.
		// Probed live on kimi 0.30.0; see harness_kimi.go.
		"SystemPrompt":    kimi.SystemPrompt,
		"CustomSubagents": kimi.CustomSubagents,
		// `kimi acp` takes no harness-shaping flags, and ACP session/new
		// carries one cwd plus mcpServers — no fallback chain, no tool
		// deny-list, no extra roots. The documented per-agent disallowedTools
		// frontmatter lives in an agent FILE, which ACP never reads.
		"FallbackModels":  kimi.FallbackModels,
		"DisallowedTools": kimi.DisallowedTools,
		"AddDirs":         kimi.AddDirs,
	} {
		if got {
			t.Errorf("kimi capability %s = true, want false (honestly gated, not emulated)", name)
		}
	}
	if !kimi.EffortLive {
		t.Error("kimi EffortLive = false; session/set_config_option works mid-session")
	}
	// Cowork rides on a stdio MCP server, which kimi forwards natively — every
	// desktop action is still gated by the backend-agnostic consent authority.
	// What kimi cannot do is learn about a server's NEW tools mid-session: it
	// ignores notifications/tools/list_changed (probed on 0.30.0), so enabling
	// Cowork there re-attaches the session instead of revealing tools in place.
	if !kimi.Cowork {
		t.Error("kimi Cowork = false; kimi forwards the Cowork stdio MCP server natively")
	}
	if kimi.LiveToolReveal {
		t.Error("kimi LiveToolReveal = true; kimi 0.30 ignores tools/list_changed")
	}
	if !claude.LiveToolReveal {
		t.Error("claude LiveToolReveal = false; claude 2.1.220 honours tools/list_changed")
	}
	if !kimi.SessionBrowse {
		t.Error("kimi SessionBrowse = false; session/list works via a one-shot probe")
	}
	// Hot-only, and it returns no summary text: `/compact` sent as prompt text
	// is intercepted by the CLI, which rewrites its own context and keeps it.
	// See kimiHarness.Compact / ErrCompactedInPlace.
	if !kimi.Compaction {
		t.Error("kimi Compaction = false; `/compact` compacts the live session in place")
	}
	// Unlike the persona channels, this one IS reachable: kimi writes a wire
	// log per subagent under <session-dir>/agents/<id>/ (probed on 0.30.0).
	if !kimi.SubagentTranscripts {
		t.Error("kimi SubagentTranscripts = false; the per-subagent wire logs exist")
	}
	if kimi.ModelPicker != harness.ModelPickerDiscovered {
		t.Errorf("kimi ModelPicker = %q, want discovered", kimi.ModelPicker)
	}
	if len(kimi.PermissionModes) != 0 || len(kimi.Efforts) != 0 {
		t.Error("kimi vocabularies must be empty (discovered per session from configOptions)")
	}

	codex := newCodexHarness(nil).Capabilities()
	if codex.ID != "codex" || codex.DisplayName != "Codex CLI" || codex.Badge != "Codex" {
		t.Errorf("codex identity = %q/%q/%q", codex.ID, codex.DisplayName, codex.Badge)
	}
	if !codex.Fork {
		t.Error("codex Fork = false; app-server exposes thread/fork")
	}
	if codex.MintsSessionID {
		t.Error("codex MintsSessionID = true; app-server assigns the persistent thread id")
	}
	if codex.ModelPicker != harness.ModelPickerDiscovered {
		t.Errorf("codex ModelPicker = %q, want discovered", codex.ModelPicker)
	}
	// Keep the first adapter deliberately narrow. The app-server protocol has
	// endpoints that resemble several of these features, but none may appear in
	// Agent Kate before its end-to-end semantic mapping is tested.
	for name, got := range map[string]bool{
		"Compaction":          codex.Compaction,
		"ColdCompact":         codex.ColdCompact,
		"Promote":             codex.Promote,
		"ProviderRouting":     codex.ProviderRouting,
		"ProviderRegistry":    codex.ProviderRegistry,
		"Cowork":              codex.Cowork,
		"LiveToolReveal":      codex.LiveToolReveal,
		"EffortLive":          codex.EffortLive,
		"UsageReporting":      codex.UsageReporting,
		"SessionBrowse":       codex.SessionBrowse,
		"TranscriptPreview":   codex.TranscriptPreview,
		"SystemPrompt":        codex.SystemPrompt,
		"CustomSubagents":     codex.CustomSubagents,
		"FallbackModels":      codex.FallbackModels,
		"DisallowedTools":     codex.DisallowedTools,
		"AddDirs":             codex.AddDirs,
		"StrictMCPConfig":     codex.StrictMCPConfig,
		"CostBudget":          codex.CostBudget,
		"SubagentTranscripts": codex.SubagentTranscripts,
		"SkillReload":         codex.SkillReload,
	} {
		if got {
			t.Errorf("codex capability %s = true, want false until its complete lifecycle is wired", name)
		}
	}
	if len(codex.PermissionModes) != 0 || len(codex.Efforts) != 0 {
		t.Error("codex vocabularies must remain discovered until app-server option mapping is verified")
	}
}

// TestHarnessRegistryWiring mirrors runCore's registration: the default id
// resolves the legacy empty Backend, and picker order is claude-first.
func TestHarnessRegistryWiring(t *testing.T) {
	r := harness.NewRegistry("claude")
	r.Register(newClaudeHarness(nil, "", ""))
	r.Register(newKimiHarness(nil, "", ""))
	r.Register(newCodexHarness(nil))

	if h, ok := r.Get(""); !ok || h.Capabilities().ID != "claude" {
		t.Fatal(`legacy empty Backend must resolve to the claude harness`)
	}
	if h, ok := r.Get("kimi"); !ok || h.Capabilities().ID != "kimi" {
		t.Fatal(`Get("kimi") must resolve to the kimi harness`)
	}
	if h, ok := r.Get("codex"); !ok || h.Capabilities().ID != "codex" {
		t.Fatal(`Get("codex") must resolve to the codex harness`)
	}
	all := r.All()
	if len(all) != 3 || all[0].Capabilities().ID != "claude" || all[2].Capabilities().ID != "codex" {
		t.Fatalf("engine order wrong: %d entries, first %q",
			len(all), all[0].Capabilities().ID)
	}
}
