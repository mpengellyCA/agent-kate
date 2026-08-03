package main

import (
	"testing"

	"agentkate/internal/harness"
)

// registryWith builds a one-harness registry, so the positional
// permissive-mode rule can be exercised without a whole core.
func registryWith(h harness.Harness) *harness.Registry {
	reg := harness.NewRegistry("claude")
	reg.Register(h)
	return reg
}

// TestHarnessDescriptors freezes the typed operation surface. Missing entries
// mean unsupported; no adapter has a parallel boolean matrix to drift from it.
func TestHarnessDescriptors(t *testing.T) {
	claude := newClaudeHarness(nil, "", "").Descriptor()
	if err := harness.ValidateDescriptor(claude); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []harness.OperationKind{harness.OperationFork, harness.OperationCompaction,
		harness.OperationColdCompaction, harness.OperationPromote, harness.OperationCowork,
		harness.OperationSystemPrompt, harness.OperationCustomSubagents, harness.OperationSkillReload} {
		if !claude.Supports(operation) {
			t.Errorf("Claude descriptor does not support %q", operation)
		}
	}
	kimi := newKimiHarness(nil, "", "").Descriptor()
	if kimi.Supports(harness.OperationColdCompaction) || kimi.Supports(harness.OperationLiveToolReveal) {
		t.Error("Kimi must expose only its real hot compaction/Cowork semantics")
	}
	for _, operation := range []harness.OperationKind{harness.OperationCompaction, harness.OperationCowork,
		harness.OperationSessionBrowse, harness.OperationSubagentTranscripts, harness.OperationProviderRegistry} {
		if !kimi.Supports(operation) {
			t.Errorf("Kimi descriptor does not support %q", operation)
		}
	}
	codex := newCodexHarness(nil).Descriptor()
	if !codex.Supports(harness.OperationFork) || codex.Supports(harness.OperationCompaction) {
		t.Error("Codex operation descriptor is not conservative")
	}
}

// TestHarnessRegistryWiring mirrors runCore's registration: the default id
// resolves the legacy empty Backend, and picker order is claude-first.
func TestHarnessRegistryWiring(t *testing.T) {
	r := harness.NewRegistry("claude")
	r.Register(newClaudeHarness(nil, "", ""))
	r.Register(newKimiHarness(nil, "", ""))
	r.Register(newCodexHarness(nil))

	if h, ok := r.Get(""); !ok || h.Descriptor().ID != "claude" {
		t.Fatal(`legacy empty Backend must resolve to the claude harness`)
	}
	if h, ok := r.Get("kimi"); !ok || h.Descriptor().ID != "kimi" {
		t.Fatal(`Get("kimi") must resolve to the kimi harness`)
	}
	if h, ok := r.Get("codex"); !ok || h.Descriptor().ID != "codex" {
		t.Fatal(`Get("codex") must resolve to the codex harness`)
	}
	all := r.All()
	if len(all) != 3 || all[0].Descriptor().ID != "claude" || all[2].Descriptor().ID != "codex" {
		t.Fatalf("engine order wrong: %d entries, first %q",
			len(all), all[0].Descriptor().ID)
	}
}
