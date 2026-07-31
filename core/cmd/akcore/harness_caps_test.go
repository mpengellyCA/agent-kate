package main

import (
	"testing"

	"agentkate/internal/harness"
)

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
		"Promote":           claude.Promote,
		"ProviderRouting":   claude.ProviderRouting,
		"Cowork":            claude.Cowork,
		"UsageReporting":    claude.UsageReporting,
		"SessionBrowse":     claude.SessionBrowse,
		"TranscriptPreview": claude.TranscriptPreview,
		"MintsSessionID":    claude.MintsSessionID,
	} {
		if !got {
			t.Errorf("claude capability %s = false, want true", name)
		}
	}
	// Verified against claude 2.1.220: no set_effort control request exists.
	if claude.EffortLive {
		t.Error("claude EffortLive = true; the CLI has no mid-session effort")
	}
	// Claude's models are discovered live (`claude -p /model` for direct, the
	// provider's /v1/models for routed) — no longer a fixed tier vocabulary.
	if claude.ModelPicker != harness.ModelPickerDiscovered {
		t.Errorf("claude ModelPicker = %q, want discovered", claude.ModelPicker)
	}
	if len(claude.PermissionModes) == 0 || len(claude.Efforts) == 0 {
		t.Error("claude static mode/effort vocabularies must be non-empty")
	}

	kimi := newKimiHarness(nil, "", "").Capabilities()
	if kimi.ID != "kimi" || kimi.DisplayName != "Kimi Code" || kimi.Badge != "Kimi" {
		t.Errorf("kimi identity = %q/%q/%q", kimi.ID, kimi.DisplayName, kimi.Badge)
	}
	for name, got := range map[string]bool{
		"Fork":              kimi.Fork,
		"Compaction":        kimi.Compaction,
		"Promote":           kimi.Promote,
		"ProviderRouting":   kimi.ProviderRouting,
		"Cowork":            kimi.Cowork,
		"UsageReporting":    kimi.UsageReporting,
		"TranscriptPreview": kimi.TranscriptPreview,
		"MintsSessionID":    kimi.MintsSessionID,
	} {
		if got {
			t.Errorf("kimi capability %s = true, want false (honestly gated, not emulated)", name)
		}
	}
	if !kimi.EffortLive {
		t.Error("kimi EffortLive = false; session/set_config_option works mid-session")
	}
	if !kimi.SessionBrowse {
		t.Error("kimi SessionBrowse = false; session/list works via a one-shot probe")
	}
	if kimi.ModelPicker != harness.ModelPickerDiscovered {
		t.Errorf("kimi ModelPicker = %q, want discovered", kimi.ModelPicker)
	}
	if len(kimi.PermissionModes) != 0 || len(kimi.Efforts) != 0 {
		t.Error("kimi vocabularies must be empty (discovered per session from configOptions)")
	}
}

// TestHarnessRegistryWiring mirrors runCore's registration: the default id
// resolves the legacy empty Backend, and picker order is claude-first.
func TestHarnessRegistryWiring(t *testing.T) {
	r := harness.NewRegistry("claude")
	r.Register(newClaudeHarness(nil, "", ""))
	r.Register(newKimiHarness(nil, "", ""))

	if h, ok := r.Get(""); !ok || h.Capabilities().ID != "claude" {
		t.Fatal(`legacy empty Backend must resolve to the claude harness`)
	}
	if h, ok := r.Get("kimi"); !ok || h.Capabilities().ID != "kimi" {
		t.Fatal(`Get("kimi") must resolve to the kimi harness`)
	}
	all := r.All()
	if len(all) != 2 || all[0].Capabilities().ID != "claude" {
		t.Fatalf("engine order wrong: %d entries, first %q",
			len(all), all[0].Capabilities().ID)
	}
}
