package main

import (
	"context"
	"encoding/json"
	"time"

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

func (h *codexHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{
		ID:          "codex",
		DisplayName: "Codex CLI",
		Badge:       "Codex",
		// The app-server owns persistent threads and exposes thread/fork. The
		// child supervisor reports the newly-minted id back from that request.
		Fork:           true,
		MintsSessionID: false,
		// Codex has a run-time model catalogue. The first implementation keeps
		// its option vocabulary discovered rather than freezing model or effort
		// tokens in Agent Kate.
		ModelPicker: harness.ModelPickerDiscovered,
		// Cowork, persona injection, provider routing, transcript preview and
		// every advanced launch option remain off until they have an end-to-end
		// app-server probe. Do not emulate a missing capability here.
	}
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

func (h *codexHarness) Launch(spec harness.StartSpec) (harness.Launched, error) {
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

func (h *codexHarness) SetOption(threadID, option, value string) (string, error) {
	return h.sup.SetOption(threadID, option, value)
}

// Health is intentionally small and non-invasive: a version probe tells the
// user whether the same `codex` binary the supervisor will execute exists.
// Auth/model checks need a live app-server handshake and will be added with
// that tested probe rather than guessing from local config files.
func (h *codexHarness) Health(ctx context.Context) (harness.Health, error) {
	binary, version := versionCheck(ctx, "codex")
	return harness.Health{
		EngineID: h.Capabilities().ID,
		State:    binary.State,
		Version:  version,
		Checks:   []harness.Check{binary},
	}, nil
}

// Codex model and reasoning vocabularies are app-server supplied, not fixed
// CLI flags. Until the supervisor's catalogue handshake is promoted into the
// public adapter API, a nil result keeps the UI's free-text discovered picker
// available without claiming a stale static list.
func (h *codexHarness) DiscoverOptions() ([]harness.DiscoveredOption, error) {
	return nil, nil
}

// DiscoverModels is the optional modelDiscoverer interface used by the shared
// engine catalogue RPC. Codex provider routing is not implemented, so a
// routed request reports no catalogue instead of borrowing the direct one.
func (h *codexHarness) DiscoverModels(p *agent.Provider) ([]harness.DiscoveredOptionValue, error) {
	if p != nil && p.Routed() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	found, err := h.sup.DiscoverModels(ctx)
	if err != nil {
		return nil, nil // discovery is best-effort, matching the Claude adapter
	}
	out := make([]harness.DiscoveredOptionValue, 0, len(found))
	for _, m := range found {
		out = append(out, harness.DiscoveredOptionValue{
			Value: m.ID, Name: m.Name, Efforts: m.Efforts,
		})
	}
	return out, nil
}

// Session browsing is not exposed until the supervisor can safely enumerate
// only this harness's persisted threads; reporting an empty list is the
// required Harness method shape while Capabilities.SessionBrowse remains false.
func (h *codexHarness) BrowseSessions() ([]harness.BrowsableSession, error) {
	return nil, nil
}

func (h *codexHarness) Compact(_ context.Context, _ harness.CompactSpec) (string, error) {
	return "", harness.Unsupported("Compaction", h.Capabilities())
}
