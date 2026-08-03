package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
		// SkillReload stays false: ACP has no reload-skills request and `kimi
		// acp` resolves its skill directories once, at session/new. A skill the
		// human installs while a kimi thread is running does not reach it until
		// that thread is restarted — a fact reloadSkillsEverywhere now SAYS in
		// the thread's own panel instead of skipping it in silence (audit F50).
		// LOCKSTEP with ui/src/state/HarnessTraits.cpp kimiDefaults().
		// `/compact` sent as ordinary prompt text performs a real in-session
		// compaction (probed on 0.30.0). It is a live-thread mechanism that
		// produces no summary text — see Compact for what that costs callers.
		Compaction: true,
		// ...but ONLY hot. ColdCompact stays false: there is no
		// `kimi --resume --print` to run a pass with, and kimi's on-disk store
		// is its own wire format, not the claude transcript session.ReadTranscript
		// parses. Letting the cold path run here would read nothing, store a
		// summary of nothing, and make the next resume seed a fresh session
		// from it — silently discarding the thread's history.
		ColdCompact: false,
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
		// ProviderRegistry: `kimi provider add/catalog/list/remove` manage a
		// persistent registry in the engine's home (plan 26) — a genuinely
		// different mechanism from claude's per-launch env routing, which is
		// why it is a SECOND flag and ProviderRouting stays false here. The
		// registry is edited by shelling out (providers/list is not
		// implemented over ACP — probed, -32601). LOCKSTEP with
		// ui/src/state/HarnessTraits.cpp kimiDefaults().
		ProviderRegistry: true,
		// UsageReporting stays false (the zero value, stated here because it is
		// load-bearing): the ACP protocol reports no per-turn token accounting,
		// and kimi's only token figure is `/usage`, a CUMULATIVE context
		// snapshot. The translator therefore ships that as a `_context` event
		// and puts no per-turn `usage` block on the result event (audit F19b) —
		// so the UI must not accumulate a session total for kimi threads.
		// LOCKSTEP with ui/src/state/HarnessTraits.cpp: `usageReporting` is set
		// for claude only.
		UsageReporting: false,
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
	// The control-channel sweep. ACP's session/new takes the mcpServers list
	// the core hands it and nothing else — there is no global-server set to
	// isolate from — and the protocol carries no spend ceiling.
	kimiNoStrictMCPConfig = "Kimi Code sessions already run with only the MCP " +
		"servers Agent Kate passes over ACP; there is no global set to isolate from"
	kimiNoCostBudget = "Kimi Code has no session spend ceiling over ACP"
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
	// The scalar half of the control-channel sweep. Title is deliberately not
	// reported: it is cosmetic, and ACP sessions carry no label to put it in.
	if spec.StrictMCPConfig {
		out = append(out, harness.UnappliedOption{
			Option: "strictMcpConfig", Reason: kimiNoStrictMCPConfig})
	}
	if spec.MaxBudgetUSD > 0 {
		out = append(out, harness.UnappliedOption{
			Option: "maxBudgetUsd", Reason: kimiNoCostBudget})
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
			// One secret per bridge process: a redeemed secret is spent while
			// its holder is connected, so sharing one between these two would
			// make whichever spawned second look like a replay (F13).
			coopMCPServer(h.exePath, h.socketPath, spec.ThreadID, spec.WorkDir,
				spec.BridgeSecret),
			coworkMCPServer(h.exePath, h.socketPath, spec.ThreadID, spec.WorkDir,
				spec.CoworkBridgeSecret),
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

// DeleteTranscript implements transcriptDeleter: the kimi event log is Agent
// Kate's OWN file, so a destroyed thread takes it with it. Claude deliberately
// does not implement this — its transcript belongs to the CLI and is kept so an
// archived thread stays recoverable.
func (h *kimiHarness) DeleteTranscript(threadID string) error {
	return h.ksup.DeleteTranscript(threadID)
}

// Health is kimi's preflight: `kimi --version` (binary), `kimi doctor
// config` (non-interactive config validation), the engine-level auth probe
// (initialize + session/new in a throwaway home-respecting probe — an
// unauthenticated kimi answers "Authentication required", and the remedy is
// taken VERBATIM from the authMethods' _meta.terminal-auth, never invented),
// and the discovered model catalogue's size. Best-effort throughout: a hung
// probe yields an Unknown check, never an error.
func (h *kimiHarness) Health(ctx context.Context) (harness.Health, error) {
	bin := h.ksup.Bin()
	var (
		binaryCheck, config harness.Check
		version             string
		auth                kimi.EngineAuth
		wg                  sync.WaitGroup
	)
	// The auth ACP handshake is one health CHECK, so it receives the same
	// deadline as --version and doctor. ProbeEngineAuth propagates this into
	// its child instead of retaining a private Background/20s timeout.
	authCtx, cancelAuth := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancelAuth()
	wg.Add(3)
	go func() {
		defer wg.Done()
		binaryCheck, version = versionCheck(ctx, bin)
	}()
	go func() {
		defer wg.Done()
		config = doctorCheck(ctx, "config", "", bin, "doctor", "config")
	}()
	go func() {
		defer wg.Done()
		// ProbeEngineAuth is best-effort by contract (state Unknown on any
		// probe failure) and honors the per-check deadline above.
		auth, _ = h.ksup.ProbeEngineAuth(authCtx)
	}()
	wg.Wait()
	authCheck := harness.Check{Name: "auth", State: auth.State, Detail: auth.Detail}
	if auth.State == "bad" {
		// The remedy travels only with a verdict it would fix.
		authCheck.Remedy = auth.Remedy
	}
	modelsCheck := harness.Check{Name: "models", State: harness.HealthOK,
		Detail: fmt.Sprintf("%d models discovered", auth.Models)}
	if auth.Models == 0 {
		modelsCheck = harness.Check{Name: "models", State: harness.HealthUnknown,
			Detail: "no model catalogue discovered"}
	}
	checks := []harness.Check{binaryCheck, config, authCheck, modelsCheck}
	return harness.Health{
		EngineID: h.Capabilities().ID,
		State:    harness.WorstState(checks),
		Version:  version,
		Checks:   checks,
		Models:   auth.Models,
	}, nil
}

// The provider-registry side-interface (engineservices.go providerRegistrar):
// thin delegation to internal/kimi, so the handlers reach the registry
// through the capability + type assertion and never a backend string compare.
func (h *kimiHarness) ListProviders(home string) ([]kimi.Provider, error) {
	return kimi.ListProviders(home)
}
func (h *kimiHarness) AddProvider(home, url string) error { return kimi.AddProvider(home, url) }
func (h *kimiHarness) ImportCatalog(home string) ([]kimi.Provider, error) {
	return kimi.ImportCatalog(home)
}
func (h *kimiHarness) RemoveProvider(home, id string) error { return kimi.RemoveProvider(home, id) }

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
		// CurrentValue travels with the enumeration: it is the value a fresh
		// `kimi acp` session reports for the option, i.e. the CLI's own default.
		// The launch authority gate reads the `mode` one (authority.go).
		d := harness.DiscoveredOption{ID: o.ID, Name: o.Name, Current: o.CurrentValue}
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

// ErrCompactedInPlace reports the one outcome the Harness.Compact signature
// cannot express: the compaction ran and succeeded, but produced no summary
// text to store. Callers that only want the context smaller can treat it as
// success; callers that need a body have nothing to write.
var ErrCompactedInPlace = errors.New(
	"Kimi Code compacted the session in place — its context is now smaller, but the " +
		"CLI returns no summary text, so no summary was stored (none is needed: the " +
		"compacted context lives in the kimi session and survives resume)")

// minKimiSummaryBytes is the length below which a `/compact` reply is treated
// as a status line ("Compacted 42 messages.") rather than a summary.
//
// This guard is load-bearing, not cosmetic. A stored summary makes resumeThread
// seed a BRAND NEW session with the summary text in place of the conversation —
// so storing a one-line status message would silently throw the whole thread's
// context away on the next resume. When in doubt, store nothing: kimi's own
// session already holds the compacted history.
const minKimiSummaryBytes = 400

// Compact runs kimi's in-session compaction. `/compact` sent as ordinary
// prompt text is intercepted by the CLI and really does compact the session
// (verified against kimi 0.30.0 by ACP probe) — there is no ACP method for it,
// and no `kimi --resume --print` to run a cold pass with, so the hot form is
// the only one and a cold request is refused.
//
// How this differs from claude, which the caller must tolerate: claude's hot
// compaction asks the model to WRITE a summary and hands that text back to be
// stored and replayed into a fresh session. Kimi rewrites its own context and
// keeps it; the reply is a status line, if anything. So unless the CLI returns
// something long enough to be a real summary, this reports ErrCompactedInPlace
// and stores nothing — resume then re-attaches to the already-compacted
// session, which is both cheaper and lossless.
func (h *kimiHarness) Compact(ctx context.Context, spec harness.CompactSpec) (string, error) {
	if !spec.Hot {
		return "", fmt.Errorf("Kimi Code compacts only inside a live session; " +
			"resume the thread and compact it hot (there is no cold pass to run)")
	}
	if !h.ksup.Running(spec.ThreadID) {
		return "", fmt.Errorf("compaction requires a running Kimi Code thread; resume it first")
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	text, err := h.ksup.Compact(ctx, spec.ThreadID)
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(text)) < minKimiSummaryBytes {
		return "", ErrCompactedInPlace
	}
	return text, nil
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
