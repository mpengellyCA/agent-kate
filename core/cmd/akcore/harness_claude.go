package main

import (
	"encoding/json"
	"fmt"
	"os"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/session"
)

// claudeHarness adapts the Claude Code supervisor (`claude -p` over
// stream-json) to the harness interface. It is the default engine and the
// most capable one: forking, compaction, promotion, provider routing and the
// Cowork desktop server are all Claude-only today.
type claudeHarness struct {
	sup        *agent.Supervisor
	exePath    string
	socketPath string
}

func newClaudeHarness(sup *agent.Supervisor, exePath, socketPath string) *claudeHarness {
	return &claudeHarness{sup: sup, exePath: exePath, socketPath: socketPath}
}

func (h *claudeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{
		// The canonical id. Persisted records historically use "" for the
		// default backend (session.BackendClaude); the registry resolves ""
		// here, and records written from now on carry the explicit id.
		ID:              "claude",
		DisplayName:     "Claude Code",
		Badge:           "", // the default engine stays unmarked in the roster
		Fork:            true,
		Compaction:      true,
		Promote:         true,
		ProviderRouting: true,
		Cowork:          true,
		// Verified against claude 2.1.220: set_model / set_permission_mode
		// exist mid-session, set_effort does not — effort is start-time only.
		EffortLive:     false,
		UsageReporting: true,
		SessionBrowse:  true,
		MintsSessionID: true,
		ModelPicker:    harness.ModelPickerTiers,
		PermissionModes: []string{
			"acceptEdits", "default", "plan", "auto", "bypassPermissions",
		},
		Efforts: []string{"low", "medium", "high", "xhigh", "max"},
	}
}

func (h *claudeHarness) Launch(spec harness.StartSpec) (harness.Launched, error) {
	mcpConfig, err := writeMCPConfig(h.exePath, h.socketPath, spec.ThreadID,
		spec.WorkDir, spec.Cowork)
	if err != nil {
		return harness.Launched{}, fmt.Errorf("mcp config: %w", err)
	}
	model := resolveModel(spec.Model)
	if _, err := h.sup.Start(agent.StartOptions{
		ID:             spec.ThreadID,
		WorkDir:        spec.WorkDir,
		Prompt:         spec.Prompt,
		MCPConfig:      mcpConfig,
		PermissionMode: spec.PermissionMode,
		Effort:         spec.Effort,
		Model:          model,
		Attachments:    spec.Attachments,
		SessionID:      spec.SessionID,
		Resume:         spec.Resume,
		ForkSession:    spec.ForkSession,
		CoworkEnabled:  spec.Cowork,
		Provider:       spec.Provider,
	}); err != nil {
		os.Remove(mcpConfig)
		return harness.Launched{}, err
	}
	mode := spec.PermissionMode
	if mode == "" {
		mode = "acceptEdits" // the supervisor's own default — persist reality
	}
	sessionID := spec.SessionID
	if spec.ForkSession {
		// A fork mints a fresh session id; the run loop captures it from the
		// init event into the record.
		sessionID = ""
	}
	return harness.Launched{
		SessionID:      sessionID,
		Model:          model,
		Effort:         spec.Effort,
		PermissionMode: mode,
	}, nil
}

func (h *claudeHarness) Send(threadID, text string, atts []agent.Attachment) error {
	return h.sup.Send(threadID, text, atts)
}

func (h *claudeHarness) Interrupt(threadID string) error { return h.sup.Interrupt(threadID) }
func (h *claudeHarness) Stop(threadID string) error      { return h.sup.Stop(threadID) }
func (h *claudeHarness) Running(threadID string) bool    { return h.sup.Running(threadID) }
func (h *claudeHarness) StopAll()                        { h.sup.StopAll() }

func (h *claudeHarness) ReadTranscript(threadID, sessionID string) ([]json.RawMessage, error) {
	if sessionID == "" {
		return nil, nil // no session yet — nothing to replay
	}
	return session.ReadTranscript(sessionID)
}

func (h *claudeHarness) SetOption(threadID, option, value string) (string, error) {
	switch option {
	case "model":
		applied := resolveModel(value)
		return applied, h.sup.SetModel(threadID, applied)
	case "permissionMode":
		return value, h.sup.SetPermissionMode(threadID, value)
	case "effort":
		return "", fmt.Errorf(
			"Claude Code does not support changing the thinking effort mid-session")
	default:
		return "", fmt.Errorf("unknown option %q", option)
	}
}
