package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
		EffortLive: true,
		// session/list works via a one-shot probe (verified kimi 0.30), so
		// past kimi sessions show up in the "Resume a Session…" browser.
		SessionBrowse: true,
		ModelPicker:   harness.ModelPickerDiscovered,
		// SystemPrompt / CustomSubagents stay false — see below for what was
		// probed. PermissionModes/Efforts empty: the vocabularies come from
		// the session handshake's configOptions ("mode", "thinking").
	}
}

// Why the two persona channels are off for kimi, probed against kimi 0.30.0:
//
//   - `kimi acp` accepts no system-prompt channel. The only replace-the-prompt
//     lever ($KIMI_CODE_HOME/SYSTEM.md) is a whole-prompt override that would
//     hide the CLI's own tool and skill injections, so it is not a persona
//     channel; callers fold the persona into the opening message instead,
//     which behaves identically on every harness.
//   - `kimi acp` runs kimi's v1 engine, whose subagent resolver reads a
//     COMPILED-IN profile table (coder / explore / plan) and no filesystem
//     catalogue at all. The documented `.agents/agents/*.md` and
//     `.kimi-code/agents/*.md` directories are read only by the v2 engine,
//     which is reachable exclusively through `kimi -p` with
//     KIMI_CODE_EXPERIMENTAL_FLAG=1 — never over ACP. Verified live: with the
//     file in place, a session/prompt delegation answered `subagent error:
//     Subagent profile "akprobe" was not found` (from inside the worktree,
//     with and without the experimental flag), while
//     `KIMI_CODE_EXPERIMENTAL_FLAG=1 kimi --agent akprobe -p` found the very
//     same file. Writing agent files here would therefore leave dead files in
//     the user's worktree and report a capability the agent cannot use.
//
// Only the subagent side needs a constant: per-profile reasons are
// adapter-supplied, while a missing system-prompt channel is described by the
// shared unsupportedDetail wording, so a downgrade and a refusal read alike.
const kimiNoCustomSubagents = "Kimi Code agents cannot be given custom subagent " +
	"profiles over ACP (its subagent set is fixed: coder, explore, plan)"

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
		Env:         spec.Env,
		MCPServers: []kimi.MCPServer{
			coopMCPServer(h.exePath, h.socketPath, spec.ThreadID, spec.WorkDir),
		},
	})
	if err != nil {
		return harness.Launched{}, err
	}
	// Kimi assigns its own session id during the ACP handshake, and downgrades a
	// rejected model/thinking/mode to its default — report what the handshake
	// actually applied (not the request) so the record replays reality.
	// A requested persona or subagent profile is reported unapplied rather than
	// emulated — see the kimiNo* constants for the probe results behind that.
	return harness.Launched{
		SessionID:           th.SessionID(),
		Model:               th.Model(),
		Effort:              th.Thinking(),
		PermissionMode:      th.Mode(),
		SystemPromptApplied: false,
		Agents:              harness.UnappliedAgents(spec.Agents, kimiNoCustomSubagents),
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

// DiscoverOptions probes the CLI's live config-option vocabulary (model /
// thinking / mode enumerations) via a one-shot `kimi acp` handshake, cached by
// the supervisor for the process lifetime.
func (h *kimiHarness) DiscoverOptions() ([]harness.DiscoveredOption, error) {
	opts, err := h.ksup.DiscoverOptions()
	if err != nil {
		return nil, err
	}
	out := make([]harness.DiscoveredOption, 0, len(opts))
	for _, o := range opts {
		d := harness.DiscoveredOption{ID: o.ID, Name: o.Name}
		for _, v := range o.Options {
			d.Options = append(d.Options,
				harness.DiscoveredOptionValue{Value: v.Value, Name: v.Name})
		}
		out = append(out, d)
	}
	return out, nil
}

// BrowseSessions lists kimi's stored past sessions (session/list via a
// one-shot probe) in the neutral browse shape.
func (h *kimiHarness) BrowseSessions() ([]harness.BrowsableSession, error) {
	sessions, err := h.ksup.ListSessions("")
	if err != nil {
		return nil, err
	}
	out := make([]harness.BrowsableSession, 0, len(sessions))
	for _, s := range sessions {
		title := s.Title
		if title == "" {
			title = "(untitled session)"
		}
		// Normalise the timestamp to second-precision UTC so browse entries
		// from every harness sort correctly as plain strings.
		updated := s.UpdatedAt
		if ts, err := time.Parse(time.RFC3339, updated); err == nil {
			updated = ts.UTC().Format(time.RFC3339)
		}
		out = append(out, harness.BrowsableSession{
			SessionID: s.SessionID,
			Backend:   session.BackendKimi,
			Project:   s.Cwd,
			Title:     title,
			Updated:   updated,
		})
	}
	return out, nil
}

// Compact is the honest gate: ACP has no in-session compaction turn we can
// bracket, and there is no `kimi --resume --print` equivalent to run a cold
// pass with. Capabilities().Compaction is false, so the RPC layer rejects the
// request before it reaches here; this is the backstop for any caller that
// forgets to check.
func (h *kimiHarness) Compact(context.Context, harness.CompactSpec) (string, error) {
	return "", harness.Unsupported("Compaction", h.Capabilities())
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
