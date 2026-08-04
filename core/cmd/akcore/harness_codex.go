package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"agentkate/internal/agent"
	"agentkate/internal/codex"
	"agentkate/internal/harness"
	"agentkate/internal/skills"
)

// codexHarness adapts the Codex CLI app-server supervisor to the neutral
// harness contract. A feature is not exposed to Agent Kate until the
// supervisor translates and tests its complete lifecycle.
type codexHarness struct {
	sup        *codex.Supervisor
	exePath    string
	socketPath string
}

func newCodexHarness(sup *codex.Supervisor, bridge ...string) *codexHarness {
	h := &codexHarness{sup: sup}
	if len(bridge) > 0 {
		h.exePath = bridge[0]
	}
	if len(bridge) > 1 {
		h.socketPath = bridge[1]
	}
	return h
}

func (h *codexHarness) Descriptor() harness.HarnessDescriptor {
	return harness.HarnessDescriptor{
		ContractVersion: harness.ContractVersion,
		ID:              "codex",
		DisplayName:     "Codex CLI",
		Badge:           "Codex",
		Health:          harness.HealthUnknown,
		// Codex has a run-time model catalogue. The implementation keeps
		// its option vocabulary discovered rather than freezing model or effort
		// tokens in Agent Kate. Efforts are part of each model/list entry, and
		// a change is queued into the next turn/start request, which makes the
		// running picker truthful without relying on the removed
		// thread/settings/update endpoint.
		// Agent Kate's two MCP bridges are layered into Codex's native config at
		// process launch. Cowork re-attaches a live thread when its tool list
		// changes, matching the conservative Kimi path until Codex's re-list
		// notification is probed end-to-end.
		// `thread/compact/start` is Codex's native, in-place compaction; unlike
		// Claude's cold summary pass it needs the live thread and returns no
		// summary body. Compact translates its completion into the shared
		// success-without-summary path.
		//
		// The current app-server schema does not expose a slash-command catalogue.
		// Do not declare OperationCommands until it does: the composer may always
		// send text, but showing an invented command list would be misleading.
		Operations: harness.Operations(harness.OperationFork,
			harness.OperationCompaction, harness.OperationCowork,
			harness.OperationSystemPrompt),
		Interop: harness.InteroperabilityMatrix{
			Continuation: harness.InteropManaged,
			Plans:        harness.InteropNative,
			Questions:    harness.InteropNative,
		},
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
			Role:                      harness.DeriveModelRole(model.ID, model.Name),
			SupportedReasoningEfforts: model.Efforts})
	}
	snapshot := harness.CatalogueSnapshot{ContractVersion: harness.ContractVersion,
		HarnessID: h.Descriptor().ID, ProviderID: scope.ProviderID, Models: models,
		Settings: []harness.SettingDescriptor{
			{Key: harness.SettingModel, DisplayName: "Model", Timing: harness.TimingNextTurn},
			{Key: harness.SettingReasoningEffort, DisplayName: "Reasoning effort", Timing: harness.TimingNextTurn},
			{Key: harness.SettingPermissionMode, DisplayName: "Approval policy",
				Choices: choices("untrusted", "on-request", "never"), Timing: harness.TimingNextTurn},
			{Key: harness.SettingSandboxMode, DisplayName: "Sandbox mode",
				Choices: []harness.SettingChoice{
					{Value: "workspace-write", DisplayName: "Workspace write"},
					{Value: "danger-full-access", DisplayName: "Full access (dangerous)"},
				}, DefaultValue: "workspace-write", Timing: harness.TimingNextTurn},
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
		ID:                    spec.ThreadID,
		WorkDir:               spec.WorkDir,
		Prompt:                spec.Prompt,
		Attachments:           spec.Attachments,
		SessionID:             spec.SessionID,
		Resume:                spec.Resume,
		ForkSession:           spec.ForkSession,
		Model:                 spec.Model,
		Effort:                spec.Effort,
		ApprovalPolicy:        spec.PermissionMode,
		Sandbox:               firstNonEmpty(spec.SandboxMode, "workspace-write"),
		DeveloperInstructions: codexDeveloperInstructions(spec.SystemPrompt),
		SkillRoots:            codexSkillRoots(),
		MCPServers:            h.mcpServers(spec),
		Env:                   h.launchEnv(spec),
	})
	if err != nil {
		return harness.Launched{}, err
	}
	return harness.Launched{
		SessionID:           th.SessionID(),
		Model:               th.Model(),
		Effort:              th.Effort(),
		PermissionMode:      th.ApprovalPolicy(),
		SystemPromptApplied: spec.SystemPrompt != "",
		Agents:              harness.UnappliedAgents(spec.Agents, codexNoCustomSubagents),
		UnappliedOptions:    codexUnappliedOptions(spec),
	}, nil
}

const (
	codexCooperationSecretEnv = "AGENTKATE_CODEX_COOPERATION_SECRET"
	codexCoworkSecretEnv      = "AGENTKATE_CODEX_COWORK_SECRET"
)

// codexDeveloperInstructions is deliberately additive: it identifies Agent
// Kate's host facilities without restating or replacing Codex's own base
// instructions, global config, plugins, skills, or project AGENTS.md files.
func codexDeveloperInstructions(user string) string {
	base := "You are running inside Agent Kate. Preserve Codex's native tools, skills, plugins, and project guidance. " +
		"When available, use the Agent Kate Cooperation MCP tools to coordinate with other Agent Kate agents; Cowork tools are opt-in desktop controls and remain subject to user approval."
	if user == "" {
		return base
	}
	return base + "\n\nAdditional instructions from the Agent Kate session:\n" + user
}

func codexSkillRoots() []string {
	root := skills.DefaultDir()
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return []string{root}
	}
	return nil
}

func (h *codexHarness) mcpServers(spec harness.StartSpec) []codex.MCPServer {
	if h.exePath == "" || h.socketPath == "" {
		return nil // unit assembly with no running core has no bridge to attach
	}
	return []codex.MCPServer{
		{Name: "agentkate-cooperation", Command: h.exePath,
			Args:    append(mcpBridgeArgs(h.socketPath, spec.ThreadID, spec.WorkDir, false, true), "--secret-env", codexCooperationSecretEnv),
			EnvVars: []string{codexCooperationSecretEnv}},
		{Name: "agentkate-cowork", Command: h.exePath,
			Args:    append(mcpBridgeArgs(h.socketPath, spec.ThreadID, spec.WorkDir, true, true), "--secret-env", codexCoworkSecretEnv),
			EnvVars: []string{codexCoworkSecretEnv}},
	}
}

func (h *codexHarness) launchEnv(spec harness.StartSpec) map[string]string {
	env := make(map[string]string, len(spec.Env)+2)
	for key, value := range spec.Env {
		env[key] = value
	}
	if h.exePath != "" && h.socketPath != "" {
		env[codexCooperationSecretEnv] = spec.BridgeSecret
		env[codexCoworkSecretEnv] = spec.CoworkBridgeSecret
	}
	return env
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
		{"sandboxMode", requested.SandboxMode},
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

func (h *codexHarness) Compact(ctx context.Context, spec harness.CompactSpec) (string, error) {
	if !spec.Hot {
		return "", fmt.Errorf("Codex compacts only inside a live session; resume the thread and compact it natively")
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	if err := h.sup.Compact(ctx, spec.ThreadID); err != nil {
		return "", err
	}
	return "", ErrCompactedInPlace
}
