package main

import (
	"encoding/json"
	"fmt"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/kimi"
	"agentkate/internal/session"
)

// kimiHarness adapts the Kimi Code supervisor (`kimi acp` over ACP) to the
// harness interface. Kimi has no session-fork, summary or promote primitive,
// no provider routing and no Cowork — those stay honestly capability-gated
// rather than emulated. Its effort analogue ("thinking") and approval modes
// are discovered per session from the CLI's configOptions.
type kimiHarness struct {
	ksup       *kimi.Supervisor
	exePath    string
	socketPath string
}

func newKimiHarness(ksup *kimi.Supervisor, exePath, socketPath string) *kimiHarness {
	return &kimiHarness{ksup: ksup, exePath: exePath, socketPath: socketPath}
}

func (h *kimiHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{
		ID:          session.BackendKimi,
		DisplayName: "Kimi Code",
		Badge:       "Kimi",
		// session/set_config_option applies mid-session (verified kimi 0.30),
		// so the thinking level stays adjustable while the agent runs.
		EffortLive:  true,
		ModelPicker: harness.ModelPickerDiscovered,
		// PermissionModes/Efforts empty: the vocabularies come from the
		// session handshake's configOptions ("mode", "thinking").
	}
}

func (h *kimiHarness) Launch(spec harness.StartSpec) (harness.Launched, error) {
	th, err := h.ksup.Start(kimi.StartOptions{
		ID:          spec.ThreadID,
		WorkDir:     spec.WorkDir,
		Prompt:      spec.Prompt,
		Attachments: spec.Attachments,
		SessionID:   spec.SessionID,
		Resume:      spec.Resume,
		Model:       spec.Model,
		Thinking:    spec.Effort,
		Mode:        spec.PermissionMode,
		MCPServers: []kimi.MCPServer{
			coopMCPServer(h.exePath, h.socketPath, spec.ThreadID, spec.WorkDir),
		},
	})
	if err != nil {
		return harness.Launched{}, err
	}
	// Kimi assigns its own session id during the ACP handshake.
	return harness.Launched{
		SessionID:      th.SessionID(),
		Model:          spec.Model,
		Effort:         spec.Effort,
		PermissionMode: spec.PermissionMode,
	}, nil
}

func (h *kimiHarness) Send(threadID, text string, atts []agent.Attachment) error {
	return h.ksup.Send(threadID, text, atts)
}

func (h *kimiHarness) Interrupt(threadID string) error { return h.ksup.Interrupt(threadID) }
func (h *kimiHarness) Stop(threadID string) error      { return h.ksup.Stop(threadID) }
func (h *kimiHarness) Running(threadID string) bool    { return h.ksup.Running(threadID) }
func (h *kimiHarness) StopAll()                        { h.ksup.StopAll() }

// ReadTranscript serves the core-side translated-event log — kimi has no
// CLI-parseable transcript of its own, so the sessionID is irrelevant here.
func (h *kimiHarness) ReadTranscript(threadID, _ string) ([]json.RawMessage, error) {
	return h.ksup.ReadTranscript(threadID)
}

func (h *kimiHarness) SetOption(threadID, option, value string) (string, error) {
	configID := map[string]string{
		"model":          "model",
		"effort":         "thinking",
		"permissionMode": "mode",
	}[option]
	if configID == "" {
		return "", fmt.Errorf("unknown option %q", option)
	}
	return value, h.ksup.SetConfigOption(threadID, configID, value)
}
