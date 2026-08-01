package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/kimi"
	"agentkate/internal/session"
)

// kimiHarness adapts the Kimi Code supervisor (`kimi acp` over ACP) to the
// harness interface. Kimi has no session-fork, summary or promote primitive
// and no provider routing — those stay honestly capability-gated rather than
// emulated. Its effort analogue ("thinking") and approval modes are discovered
// per session from the CLI's configOptions.
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
		// One wire log per subagent under <session-dir>/agents/<id>/, probed
		// on 0.30.0 — the viewer translates its event shapes.
		SubagentTranscripts: true,
		// Kimi forwards stdio MCP servers natively, so the Cowork desktop
		// bridge works here exactly as it does for claude — every desktop
		// action is still gated by the core's consent authority, which is
		// backend-agnostic.
		Cowork: true,
		// ...but kimi 0.30 lists each server's tools once, at session/new, and
		// ignores notifications/tools/list_changed (probed: the notification
		// was sent, no re-list followed, and the revealed tool stayed invisible
		// for the rest of the session). Switching Cowork on mid-session
		// therefore re-attaches the thread — session/resume keeps the
		// conversation and accepts a fresh mcpServers list (also probed).
		LiveToolReveal: false,
		ModelPicker:    harness.ModelPickerDiscovered,
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

// The plan 16 P6 sweep, per option, probed against kimi 0.30.0: `kimi acp`
// takes no harness-shaping flags at all, and ACP's session/new carries a
// single `cwd` plus `mcpServers` — no model-fallback chain, no tool deny-list,
// no additional roots. The documented per-agent `disallowedTools` frontmatter
// belongs to an agent FILE, and P3 proved those files are unreachable over ACP
// (v2 engine only), so there is nowhere honest to put it.
const (
	kimiNoFallbackModels = "Kimi Code has no model-fallback chain over ACP; " +
		"the session runs on one model"
	kimiNoDisallowedTools = "Kimi Code has no per-session tool deny-list over ACP " +
		"(its agent-file frontmatter is read only by the v2 engine, which ACP never runs)"
	kimiNoAddDirs = "Kimi Code sessions reach one working directory over ACP; " +
		"extra roots are not expressible"
)

// unappliedSweep reports every list-valued sweep option the caller asked for,
// since kimi can express none of them. A request that vanished silently would
// be worse than one that is refused with a reason.
func unappliedSweep(spec harness.StartSpec) []harness.UnappliedOption {
	var out []harness.UnappliedOption
	for _, o := range []struct {
		name   string
		want   []string
		reason string
	}{
		{"fallbackModels", spec.FallbackModels, kimiNoFallbackModels},
		{"disallowedTools", spec.DisallowedTools, kimiNoDisallowedTools},
		{"addDirs", spec.AddDirs, kimiNoAddDirs},
	} {
		if len(o.want) > 0 {
			out = append(out, harness.UnappliedOption{Option: o.name, Reason: o.reason})
		}
	}
	return out
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
		Env:         spec.Env,
		MCPServers: []kimi.MCPServer{
			coopMCPServer(h.exePath, h.socketPath, spec.ThreadID, spec.WorkDir),
			coworkMCPServer(h.exePath, h.socketPath, spec.ThreadID, spec.WorkDir),
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
		UnappliedOptions:    unappliedSweep(spec),
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

// SubagentTranscripts lists this thread's per-subagent wire logs. kimi keeps
// one directory per subagent under the session dir — <session>/agents/<id>/
// wire.jsonl, ids "main" (the thread itself) plus agent-0, agent-1, … —
// carrying its own wire protocol rather than a Claude-shaped transcript; the
// viewer translates. Verified on kimi 0.30.0 against real session dirs.
func (h *kimiHarness) SubagentTranscripts(_, sessionID string) ([]harness.SubagentTranscript, error) {
	dir := kimi.SessionDir(sessionID)
	if dir == "" {
		return nil, nil
	}
	list := scanSubagentDir(filepath.Join(dir, "agents"), true)
	for i := range list {
		list[i].Label = kimiSubagentLabel(list[i].Path)
	}
	return list, nil
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
