package main

import (
	"context"
	"encoding/json"
	"fmt"

	"agentkate/internal/agent"
	"agentkate/internal/codex"
	"agentkate/internal/harness"
)

// codexHarness adapts the Codex CLI app-server supervisor to the neutral
// harness contract. Its deliberately modest capability set is important: the
// app-server has a large protocol surface, but a feature is not exposed to
// Agent Kate until the supervisor translates and tests its complete lifecycle.
type codexHarness struct {
	sup *codex.Supervisor
}

func newCodexHarness(sup *codex.Supervisor) *codexHarness {
	return &codexHarness{sup: sup}
}

func (h *codexHarness) Descriptor() harness.HarnessDescriptor {
	return harness.HarnessDescriptor{
		ContractVersion: harness.ContractVersion,
		ID:              "codex",
		DisplayName:     "Codex CLI",
		Badge:           "Codex",
		Health:          harness.HealthUnknown,
		// The app-server owns persistent threads and exposes thread/fork. The
		// child supervisor reports the newly-minted id back from that request.
		Operations: harness.Operations(harness.OperationFork),
		// Codex has a run-time model catalogue. The first implementation keeps
		// its option vocabulary discovered rather than freezing model or effort
		// tokens in Agent Kate. Efforts are part of each model/list entry, and
		// a change is queued into the next turn/start request, which makes the
		// running picker truthful without relying on the removed
		// thread/settings/update endpoint.
		// Cowork, persona injection, provider routing, transcript preview and
		// every advanced launch option remain off until they have an end-to-end
		// app-server probe. Do not emulate a missing capability here.
	}
}

// Catalogue maps app-server model/list into the linkage DTO.  Codex lists the
// valid reasoning efforts alongside each model, which is why the dependency is
// represented on ModelDescriptor rather than duplicated as native data in UI.
func (h *codexHarness) Catalogue(ctx context.Context, scope harness.CatalogueScope) (harness.CatalogueSnapshot, error) {
	if scope.ProviderID != "" && scope.ProviderID != "direct" {
		return harness.CatalogueSnapshot{}, fmt.Errorf("Codex CLI provider routing is not available")
	}
	found, err := h.sup.DiscoverModels(ctx)
	if err != nil {
		return harness.CatalogueSnapshot{}, err
	}
	models := make([]harness.ModelDescriptor, 0, len(found))
	for _, model := range found {
		models = append(models, harness.ModelDescriptor{ID: model.ID, DisplayName: model.Name,
			SupportedReasoningEfforts: model.Efforts})
	}
	snapshot := harness.CatalogueSnapshot{ContractVersion: harness.ContractVersion,
		HarnessID: h.Descriptor().ID, ProviderID: scope.ProviderID, Models: models,
		Settings: []harness.SettingDescriptor{
			{Key: harness.SettingModel, DisplayName: "Model", Timing: harness.TimingNextTurn},
			{Key: harness.SettingReasoningEffort, DisplayName: "Reasoning effort", Timing: harness.TimingNextTurn},
			{Key: harness.SettingPermissionMode, DisplayName: "Approval policy",
				Choices: choices("untrusted", "on-request", "never"), Timing: harness.TimingNextTurn},
		}}
	snapshot.Revision = harness.CatalogueRevision(snapshot)
	return snapshot, nil
}

const codexNoCustomSubagents = "Codex CLI custom subagent profiles are not wired into Agent Kate yet"

func codexUnappliedOptions(spec harness.StartSpec) []harness.UnappliedOption {
	var out []harness.UnappliedOption
	for _, option := range []struct {
		name   string
		asked  bool
		reason string
	}{
		{"fallbackModels", len(spec.FallbackModels) > 0,
			"Codex CLI model fallback chains are not wired into Agent Kate yet"},
		{"disallowedTools", len(spec.DisallowedTools) > 0,
			"Codex CLI per-session tool deny-lists are not wired into Agent Kate yet"},
		{"addDirs", len(spec.AddDirs) > 0,
			"Codex CLI additional workspace roots are not wired into Agent Kate yet"},
		{"strictMcpConfig", spec.StrictMCPConfig,
			"Codex CLI strict MCP configuration is not wired into Agent Kate yet"},
		{"maxBudgetUsd", spec.MaxBudgetUSD > 0,
			"Codex CLI session cost budgets are not wired into Agent Kate yet"},
	} {
		if option.asked {
			out = append(out, harness.UnappliedOption{Option: option.name, Reason: option.reason})
		}
	}
	return out
}

func (h *codexHarness) Launch(launch harness.AgentLaunch, runtime harness.StartSpec) (harness.Launched, error) {
	spec := runtime
	spec.ThreadID = launch.Ref.ThreadID
	spec.WorkDir = launch.WorkDir
	spec.Prompt = launch.Prompt
	spec.Attachments = launch.Attachments
	spec.Model = launch.Settings.Model
	spec.Effort = launch.Settings.ReasoningEffort
	spec.PermissionMode = launch.Settings.PermissionMode
	spec.SessionID = launch.Ref.NativeSessionID
	spec.Resume = launch.Resume
	spec.ForkSession = launch.ForkSession
	spec.Cowork = launch.Cowork
	spec.Title = launch.Title
	th, err := h.sup.Start(codex.StartOptions{
		ID:             spec.ThreadID,
		WorkDir:        spec.WorkDir,
		Prompt:         spec.Prompt,
		Attachments:    spec.Attachments,
		SessionID:      spec.SessionID,
		Resume:         spec.Resume,
		ForkSession:    spec.ForkSession,
		Model:          spec.Model,
		Effort:         spec.Effort,
		ApprovalPolicy: spec.PermissionMode,
		Sandbox:        "workspace-write",
		Env:            spec.Env,
	})
	if err != nil {
		return harness.Launched{}, err
	}
	return harness.Launched{
		SessionID:        th.SessionID(),
		Model:            th.Model(),
		Effort:           th.Effort(),
		PermissionMode:   th.ApprovalPolicy(),
		Agents:           harness.UnappliedAgents(spec.Agents, codexNoCustomSubagents),
		UnappliedOptions: codexUnappliedOptions(spec),
	}, nil
}

func (h *codexHarness) Send(threadID, text string, atts []agent.Attachment) error {
	return h.sup.Send(threadID, text, atts)
}

func (h *codexHarness) Interrupt(threadID string) error { return h.sup.Interrupt(threadID) }
func (h *codexHarness) Stop(threadID string) error      { return h.sup.Stop(threadID) }
func (h *codexHarness) Running(threadID string) bool    { return h.sup.Running(threadID) }
func (h *codexHarness) StopAll()                        { h.sup.StopAll() }

// ReadTranscript serves the supervisor's durable normalized-event log. Codex
// owns a different on-disk conversation format, so Agent Kate deliberately
// replays only the events it translated itself.
func (h *codexHarness) ReadTranscript(threadID, _ string) ([]json.RawMessage, error) {
	return h.sup.ReadTranscript(threadID)
}

// DeleteTranscript is intentionally limited to Agent Kate's translated log;
// Codex's own persisted thread remains available to its own resume tooling.
func (h *codexHarness) DeleteTranscript(threadID string) error {
	return h.sup.DeleteTranscript(threadID)
}

func (h *codexHarness) UpdateSettings(_ context.Context, ref harness.AgentRef, requested harness.AgentSettings) (harness.AppliedSettings, error) {
	effective := requested
	for _, update := range []struct{ option, value string }{
		{"model", requested.Model},
		{"effort", requested.ReasoningEffort},
		{"permissionMode", requested.PermissionMode},
	} {
		if update.value == "" {
			continue
		}
		if _, err := h.sup.SetOption(ref.ThreadID, update.option, update.value); err != nil {
			return harness.AppliedSettings{}, err
		}
	}
	return harness.AppliedSettings{Requested: requested, Effective: effective, Timing: harness.TimingNextTurn}, nil
}

// Health is intentionally small and non-invasive: a version probe tells the
// user whether the same `codex` binary the supervisor will execute exists.
// Auth/model checks need a live app-server handshake and will be added with
// that tested probe rather than guessing from local config files.
func (h *codexHarness) Health(ctx context.Context) (harness.Health, error) {
	binary, version := versionCheck(ctx, "codex")
	return harness.Health{
		EngineID: h.Descriptor().ID,
		State:    binary.State,
		Version:  version,
		Checks:   []harness.Check{binary},
	}, nil
}

// Session browsing is not exposed until the supervisor can safely enumerate
// only this harness's persisted threads; reporting an empty list is the
// required Harness method shape while SessionBrowse remains absent.
func (h *codexHarness) BrowseSessions() ([]harness.BrowsableSession, error) {
	return nil, nil
}

func (h *codexHarness) Compact(_ context.Context, _ harness.CompactSpec) (string, error) {
	return "", harness.Unsupported("Compaction", h.Descriptor())
}
