package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/coop"
	"agentkate/internal/cowork"
	"agentkate/internal/extensions"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/modes"
	"agentkate/internal/permission"
	"agentkate/internal/safe"
	"agentkate/internal/schedule"
	"agentkate/internal/search"
	"agentkate/internal/session"
	"agentkate/internal/skills"
	"agentkate/internal/vsix"
	"agentkate/internal/worktree"
)

// --- IPC parameter / result types ------------------------------------------

// agentEventParams is the wire shape of an "agent.event" notification. Events
// are delivered as an ordered batch (the core coalesces the per-line
// stream-json flood); the UI iterates Events in order. A batch always carries
// at least one event.
type agentEventParams struct {
	ThreadID string            `json:"threadId"`
	Events   []json.RawMessage `json:"events"`
}

// skillReloader is implemented by harnesses whose CLI can re-read its skill
// directories mid-session (claude: the reload_skills control request). Without
// it a skill installed while a thread is running is invisible to that thread
// until it is relaunched. Harnesses without the mechanism need no stub.
type skillReloader interface {
	ReloadSkills(threadID string) error
}

type agentStartParams struct {
	WorkspacePath  string             `json:"workspacePath"`
	Prompt         string             `json:"prompt"`
	PermissionMode string             `json:"permissionMode"`
	SandboxMode    string             `json:"sandboxMode,omitempty"`
	Effort         string             `json:"effort"`    // claude --effort level; "" = default
	Model          string             `json:"model"`     // model id; "" = backend default
	Backend        string             `json:"backend"`   // "" / "claude" = Claude Code, "kimi" = Kimi Code
	Isolation      string             `json:"isolation"` // worktree.Mode*; "" = auto
	Attachments    []agent.Attachment `json:"attachments"`
	CoworkEnabled  bool               `json:"coworkEnabled"` // opt into the KDE Cowork desktop tools
	// ProviderID is an opaque profile identifier. The core resolves it into a
	// private runtime binding; URLs, model maps, and credentials are never RPC
	// DTO fields.
	ProviderID string          `json:"providerId,omitempty"`
	Provider   *agent.Provider `json:"-"` // internal/test launch binding only
	// SystemPrompt / Agents are the persona channels (plan 16 P3). Both are
	// capability-gated per harness and reported as applied-truth by Launch —
	// a harness without the channel names the request as unapplied rather
	// than emulating it.
	SystemPrompt string                 `json:"systemPrompt,omitempty"`
	Agents       []harness.AgentProfile `json:"agents,omitempty"`
	// Env overlays this thread's process environment (plan 16 P6). Reachable
	// from agent.start — a UI/human path — and deliberately NOT from
	// launch_agent: see harness.StartSpec.Env.
	Env map[string]string `json:"env,omitempty"`
	// The P6 launch-option sweep, each capability-gated per harness and
	// reported as applied-truth when a harness cannot express it.
	FallbackModels  []string `json:"fallbackModels,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	AddDirs         []string `json:"addDirs,omitempty"`
	// The control-channel sweep. StrictMCPConfig isolates the thread from the
	// human's globally-configured MCP servers; MaxBudgetUSD is a hard spend
	// ceiling the CLI enforces itself (0 = uncapped).
	StrictMCPConfig bool    `json:"strictMcpConfig,omitempty"`
	MaxBudgetUSD    float64 `json:"maxBudgetUsd,omitempty"`
}

type agentSendParams struct {
	ThreadID    string             `json:"threadId"`
	Text        string             `json:"text"`
	Attachments []agent.Attachment `json:"attachments"`
	// FromThreadID names the AGENT thread issuing the send (the Cooperation
	// bridge's send_agent). Empty for UI-driven sends. When set and the target
	// is outside the caller's own subtree, the human must approve once.
	FromThreadID string `json:"fromThreadId,omitempty"`
	// AwaitReply says this send is the first half of send_agent(wait:true) —
	// the caller will block on agent.wait for the target's reply as soon as
	// this returns. It exists so ONE human decision covers the whole operation
	// (audit F35 pass 3): without it the composite asked twice, and the second
	// ask came after the message had already been delivered. Advisory only; it
	// widens nothing on its own, since the wait it pre-authorises is a wait the
	// caller could ask for separately anyway.
	AwaitReply bool `json:"awaitReply,omitempty"`
}

type agentStopParams struct {
	ThreadID string `json:"threadId"`
	// FromThreadID names the agent thread issuing the stop (close_agent);
	// empty for UI-driven stops. Same cross-subtree approval rule as sends.
	FromThreadID string `json:"fromThreadId,omitempty"`
}

type agentDiffParams struct {
	ThreadID string `json:"threadId"`
}

type coopSetOpenFilesParams struct {
	Owner string   `json:"owner"`
	Files []string `json:"files"`
}

type coopPostNoteParams struct {
	Author string `json:"author"`
	Text   string `json:"text"`
}

// handlerDeps bundles everything registerHandlers needs.
type handlerDeps struct {
	srv *ipc.Server
	// Every per-thread action routes through harnesses — there is deliberately
	// no direct supervisor handle here. The last one (the Claude supervisor,
	// kept for hot compaction) went away with plan 16 P6: compaction is a
	// Harness method now, and the running/stop checks that used it silently
	// reported a live kimi thread as not running.
	harnesses *harness.Registry
	turns     *agent.TurnTracker // backend-agnostic idle/busy mirror (agent.wait)
	// rateWakes arms the automatic resume that unparks a thread when the
	// account's usage window reopens (plan 28 §Phase 2, ratewake.go). Nil in
	// tests that do not exercise it — every call on it is nil-safe.
	rateWakes  *schedule.RateWaker
	orchGrants *orchGrants // one-shot human approvals for cross-subtree agent control
	// workerSlots reserves a worker slot for the whole duration of a launch, so
	// the fan-out caps hold against CONCURRENT launch_agent calls — the persisted
	// records they otherwise count only appear after the launch (authority.go).
	workerSlots *workerSlots
	// bridgeSecrets authenticates each thread's MCP bridges to the core: akcore
	// mints one per launch and passes it to that launch's bridges in their
	// environment, and bridge.identify demands it back (audit F13,
	// bridgeauth.go). Absent, no bridge can identify — which is the fail-closed
	// direction, not a bypass.
	bridgeSecrets *bridgeSecrets
	coop          *coop.State
	threads       *threadRegistry
	broker        *permission.Broker
	extensions    *vsix.Manager
	sessions      *session.Store
	attachSide    *session.AttachmentStore // per-thread attachment metadata sidecars
	questionSide  *session.QuestionStore   // completed AskUserQuestion history for replay
	summaries     *compact.Store
	modes         *modes.Store // user-editable ensembles (plan 16 P4)
	skills        *skills.Catalog
	// claudePlugins is a CLI boundary for the extension catalogue. Keeping it
	// separate from VSIX extensions prevents two unrelated extension systems
	// from sharing an install path or authority boundary.
	claudePlugins *extensions.ClaudePlugins
	gitCache      *gitstatus.Cache
	cowork        *cowork.Service // nil if KDE/consent init failed; handlers guard
	// remote is an authenticated HTTPS human surface, not an IPC UI role.
	remote     *remoteControl
	humanQueue *humanSendQueue
	socketPath string
	exePath    string
	log        *slog.Logger
	// agentExitWait overrides how long the destructive paths wait for a stopped
	// process to exit before refusing (waitAgentExit); zero means the default.
	// Set by tests only — a fake harness has no real backstops to wait out.
	agentExitWait time.Duration
}

// --- harness routing -------------------------------------------------------
// Per-thread operations route to the harness that owns the thread. The live
// harnesses answer first (a record may not be persisted yet); the session
// store's Backend answers for dormant threads; unknown/empty falls back to
// the default harness.

func (d handlerDeps) harnessFor(threadID string) harness.Harness {
	for _, h := range d.harnesses.All() {
		if h.Running(threadID) {
			return h
		}
	}
	if rec, ok := d.sessions.Get(threadID); ok {
		if h, ok := d.harnesses.Get(rec.Backend); ok {
			return h
		}
	}
	h, _ := d.harnesses.Get("") // the default harness always exists
	return h
}

func (d handlerDeps) descriptorFor(threadID string) harness.HarnessDescriptor {
	return d.harnessFor(threadID).Descriptor()
}

func (d handlerDeps) agentRunning(threadID string) bool {
	for _, h := range d.harnesses.All() {
		if h.Running(threadID) {
			return true
		}
	}
	return false
}

func (d handlerDeps) agentSend(threadID, text string, atts []agent.Attachment) error {
	return d.harnessFor(threadID).Send(threadID, text, atts)
}

func (d handlerDeps) agentStop(threadID string) error {
	return d.harnessFor(threadID).Stop(threadID)
}

func (d handlerDeps) agentInterrupt(threadID string) error {
	return d.harnessFor(threadID).Interrupt(threadID)
}

// defaultAgentExitWait bounds waitAgentExit. Stop already escalates to a
// process-group SIGKILL through the supervisor's own backstops (the interrupt
// backstop and closeStdin's kill timer), whose worst case on a busy thread is
// ~11 s — so a thread still running past this window survived even SIGKILL
// and cannot be waited out.
const defaultAgentExitWait = 15 * time.Second

// waitAgentExit polls until the thread's process is gone, or the deadline.
// Every worktree-destroying path must call it between agentStop and
// worktree.Remove: Stop returns immediately and finishes on a background
// goroutine, so removing straight away deletes a live process's cwd and
// leaves it running — with an authenticated bridge that can keep messaging
// siblings — after the human believes it destroyed (audit F54). Returns false
// when the process is still alive; destructive callers must refuse rather
// than proceed.
func waitAgentExit(d handlerDeps, threadID string) bool {
	wait := d.agentExitWait
	if wait <= 0 {
		wait = defaultAgentExitWait
	}
	deadline := time.Now().Add(wait)
	for d.agentRunning(threadID) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// unsupportedDetail is THE capability-gate wording: one message shape for
// every feature a harness lacks, so it never drifts per call site. Used both
// by the hard gate below and by the applied-truth reports, where a missing
// capability is a downgrade to name rather than a request to refuse.
func unsupportedDetail(feature string, descriptor harness.HarnessDescriptor) string {
	return feature + " is not supported by " + descriptor.DisplayName + " agents"
}

// unsupported is the capability-gate error: an optional RPC a harness cannot
// serve is rejected outright with the shared wording.
func unsupported(feature string, descriptor harness.HarnessDescriptor) error {
	return ipc.Errorf(ipc.CodeInvalidParams, unsupportedDetail(feature, descriptor))
}

// recordAttachments appends a compact, body-free attachment sidecar entry for a
// sent message so the UI can redraw named/clickable chips after a resume. A turn
// with no attachments is a no-op; a write failure is logged, never fatal — chips
// simply degrade to absent on replay.
func recordAttachments(d handlerDeps, threadID, text string, atts []agent.Attachment) {
	if d.attachSide == nil || len(atts) == 0 {
		return
	}
	metas := make([]session.AttachmentMeta, 0, len(atts))
	for _, a := range atts {
		metas = append(metas, session.AttachmentMeta{
			Name:      a.Name,
			Kind:      a.Kind,
			Path:      a.Path,
			MediaType: a.MediaType,
			Outside:   a.Outside,
			CachePath: a.CachePath,
		})
	}
	if err := d.attachSide.Append(threadID, session.AttachmentTurn{Text: text, Attachments: metas}); err != nil {
		d.log.Warn("could not record attachment sidecar", "thread", threadID, "err", err)
	}
}

// codeUIOnly is returned when an RPC that only the human's own window may drive
// is called by anything else.
//
// SECURITY (audit F34/F36 pass 3): these refusals used to be reported as
// codeCoworkDenied (-32010), which is the Cowork consent code. Twenty-six
// handlers with nothing to do with the desktop — the git mutations, the
// destructive removals, the transcript reads, the installers — were therefore
// telling clients "Cowork denied this". The error code is part of the wire
// contract, and a client that one day branches on it (today ui/src only knows
// -32000) would branch on a lie. The human-readable text is UNCHANGED, so
// nothing that matches on the message moves.
//
// The Cowork family owns the codes immediately below it and they must stay
// distinct — codeCoworkDenied (-32010, a consent refusal) and codeCoworkBusy
// (-32011, transient contention on the shared cursor, retry the same call), both
// in cowork.go. This code is neither: it is a PERMANENT refusal of an RPC that
// only the human's window may drive, and a client must not conflate it with a
// retryable one. Keep new codes in this space unique across cmd/akcore.
const codeUIOnly = -32012

// uiOnlyRefusal is the one sentence every UI-only gate answers with. Kept as a
// constant because tests, and the UI, match on it.
const uiOnlyRefusal = "this action may only be performed from the Agent Kate window"

// requireUIWindow enforces that the caller is the human's own window.
//
// It is the same check as Cowork's requireUI (cowork.go) — RequireUI on the
// connection identity, failing closed for a connection that never identified as
// anything — under the error code that actually describes it. New UI-only
// handlers outside the Cowork family should use THIS one; see the inventory
// test in handlers_inventory_test.go, which fails the build for any registered
// method that neither carries a gate nor appears in the reviewed
// agent-reachable set.
func requireUIWindow(srv *ipc.Server, ctx context.Context) error {
	if srv.RequireUI(ctx) {
		return nil
	}
	return ipc.Errorf(codeUIOnly, uiOnlyRefusal)
}

// sessionRecordWire is the compatibility boundary for the UI-facing session
// roster. The persistence refactor moved identity and requested/effective
// settings into the neutral linkage DTOs, which is the correct on-disk shape.
// Existing UI consumers still read the small, flat roster fields, though, and
// returning the raw Record makes every restored thread look like it has no id.
// Keep the linkage DTOs in the reply while projecting the old display fields;
// this is a wire compatibility shim, not a second persisted representation.
func sessionRecordWire(r session.Record) map[string]any {
	b, err := json.Marshal(r)
	if err != nil {
		// Record contains only JSON-compatible fields; keep a useful minimal
		// response even if a future field makes the full marshal fail.
		return map[string]any{
			"threadId": r.ThreadID,
			"project":  r.Project,
			"title":    r.Title,
			"status":   r.Status,
		}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{
			"threadId": r.ThreadID,
			"project":  r.Project,
			"title":    r.Title,
			"status":   r.Status,
		}
	}
	// These are display/control identifiers, not the persisted schema. Keep
	// them top-level for clients released before the linkage cutover.
	out["threadId"] = r.ThreadID
	out["sessionId"] = r.SessionID
	out["backend"] = r.Backend
	out["providerId"] = r.ProviderID
	out["model"] = r.Model
	out["effort"] = r.Effort
	out["permissionMode"] = r.PermissionMode
	// Cleanup's existing table also reads these worktree display fields.
	out["branch"] = r.Worktree.Branch
	out["path"] = r.Worktree.Path
	out["isolated"] = r.Worktree.Isolated
	out["number"] = r.Worktree.Number
	return out
}

func archiveRecordWire(r session.ArchiveRecord) map[string]any {
	out := sessionRecordWire(r.Record)
	out["archivedAt"] = r.ArchivedAt
	out["reason"] = r.Reason
	return out
}

// registerHandlers wires the JSON-RPC methods the core serves.
func registerHandlers(d handlerDeps) {
	d.srv.Handle("handshake", func(ctx context.Context, _ json.RawMessage) (any, error) {
		// Tag this connection as the UI (the first UI becomes the primary that runs
		// Cowork portal sessions) — the Cowork keystone (08 §C).
		//
		// A refusal is an ERROR, not a quiet no-op (audit F13): the caller would
		// otherwise believe it is the UI and discover otherwise one rejected
		// UI-only call at a time, with panels silently empty. The two refusals
		// are a bridge connection re-identifying, and a second client asking for
		// a role another live connection already holds.
		if !d.srv.MarkUI(ctx) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest,
				"the UI role is already held by another connection (or this "+
					"connection is an agent bridge); this client has no UI authority")
		}
		d.log.Info("handshake received")
		return map[string]any{
			"name":    "akcore",
			"version": version,
			"pid":     os.Getpid(),
			"role":    "ui",
		}, nil
	})

	// Cowork (KDE desktop see/control) RPCs. No-op if the service is unavailable.
	registerCoworkHandlers(d)

	// The opt-in switch itself, plus the OS-permission preflight. Registered
	// even when the Cowork service is unavailable: every thread's bridge asks
	// for its state on each tools/list and needs a real "not enabled" answer.
	registerCoworkEnableHandlers(d)

	// Orchestration RPCs (agent.wait / agent.launchWorker) — the core side of
	// the Cooperation bridge's launch/send/wait/close tools (plan 16 P1).
	registerOrchestrationHandlers(d)

	// Bridge identity + the `mcp.activity` feed the UI watches (plan 16 P2).
	registerMCPActivity(d)

	// Ensembles: the user-editable controller/worker recipes and the one-thread
	// apply that briefs a controller (plan 16 P4).
	registerModeHandlers(d)

	// Engine-level services: preflight health + the kimi provider registry (plan 26).
	registerEngineServiceHandlers(d)

	// Extension catalogue (plan 22): plugins widen, but do not replace, Skills.
	registerExtensionHandlers(d)

	// Per-subagent transcripts, discovered by the thread's own harness (P6).
	registerSubagentHandlers(d)

	// --- agent threads -----------------------------------------------------

	// harness.list is the complete descriptor surface. The UI is deliberately
	// not given a fallback list: no descriptor means launch controls stay
	// disabled until the core has answered.
	d.srv.Handle("harness.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		list := make([]harness.HarnessDescriptor, 0, len(d.harnesses.All()))
		for _, h := range d.harnesses.All() {
			descriptor := h.Descriptor()
			if err := harness.ValidateDescriptor(descriptor); err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			list = append(list, descriptor)
		}
		return map[string]any{"contractVersion": harness.ContractVersion, "harnesses": list}, nil
	})

	// harness.catalog returns one revisioned provider scope. Its request carries
	// identities only; it never accepts a provider URL, credential, environment
	// overlay or native config blob from the UI.
	d.srv.Handle("harness.catalog", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var scope harness.CatalogueScope
		if err := json.Unmarshal(raw, &scope); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if scope.HarnessID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "harnessId is required")
		}
		h, ok := d.harnesses.Get(scope.HarnessID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown harness "+scope.HarnessID)
		}
		snapshot, err := h.Catalogue(ctx, scope)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if err := harness.ValidateCatalogue(snapshot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return snapshot, nil
	})

	// agent.start is the HUMAN's start path, and every one of its parameters is
	// authority the agent-facing launch_agent deliberately cannot reach (audit
	// F5): an arbitrary WorkspacePath (any directory on the machine, not the
	// parent's project), CoworkEnabled (desktop control), a ProviderID selection,
	// and an Env overlay that is applied AFTER the provider credential scrub
	// (core/internal/agent/agent.go) — so it can rewrite ANTHROPIC_BASE_URL and
	// redirect the injected provider token to an endpoint of the caller's
	// choosing. UI-only, therefore, and not a subset of that: the whole handler.
	d.srv.Handle("agent.start", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p agentStartParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.WorkspacePath == "" || p.Prompt == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "workspacePath and prompt are required")
		}
		if p.Provider == nil {
			provider, err := resolveProviderBinding(p.ProviderID)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
			}
			p.Provider = provider
		}
		h, ok := d.harnesses.Get(p.Backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+p.Backend)
		}
		descriptor := h.Descriptor()
		if p.Provider.Routed() && !descriptor.Supports(harness.OperationProviderRouting) {
			return nil, unsupported("Provider routing", descriptor)
		}
		if p.CoworkEnabled && !descriptor.Supports(harness.OperationCowork) {
			return nil, unsupported("Cowork", descriptor)
		}
		if err := authorizeCoworkAtStart(d, ctx, p.CoworkEnabled,
			summarizePrompt(p.Prompt), firstLine(p.Prompt)); err != nil {
			return nil, err
		}
		// Start asynchronously so this reply — which carries the threadId —
		// always reaches the UI before any streamed event for the thread. The
		// session id is pre-minted only for harnesses launched onto an id we
		// choose; otherwise the CLI assigns its own during the handshake and
		// the UI captures it from the init event.
		threadID := agent.NewThreadID()
		sessionID := ""
		if descriptor.Supports(harness.OperationMintSessionID) {
			sessionID = session.NewID()
		}
		// The opening prompt is a turn: mark it queued BEFORE the async start so
		// an agent.wait racing the launch never sees a false idle. A launch
		// failure emits an "error" lifecycle, which clears it.
		d.turns.TurnQueued(threadID)
		safe.Go("agent.startThread", func() { startThread(d, h, threadID, sessionID, p) })
		// The opening prompt's attachments are recorded against the new thread id
		// (the record is created asynchronously above, but the sidecar is keyed by
		// thread id and needs no record to exist).
		recordAttachments(d, threadID, p.Prompt, p.Attachments)
		return map[string]any{
			"threadId":  threadID,
			"sessionId": sessionID,
			"backend":   descriptor.ID,
		}, nil
	})

	// agent.resume re-launches a dormant thread on its persisted Claude Code
	// session, in the same worktree it ran in before.
	//
	// UI-only (audit F5): resume replays the record's persisted authority — its
	// permission mode, its CoworkEnabled flag, its Env overlay — and accepts a
	// caller-supplied ProviderID and Model. An agent bridge
	// waking a dormant thread would be re-arming authority the human granted to
	// a thread they believed was finished.
	d.srv.Handle("agent.resume", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			// Optional: replace the persisted model on resume — the UI sends this
			// when the record's model is no longer offered and the user picked a
			// live replacement, so the chat continues instead of failing on a
			// retired id.
			Model string `json:"model,omitempty"`
			// Optional opaque profile selection. The core resolves the current
			// runtime binding rather than accepting a provider configuration blob.
			ProviderID string `json:"providerId,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if m := strings.TrimSpace(p.Model); m != "" && m != rec.Model {
			rec.Model = m                                                                // resume the local copy on the new model…
			_ = d.sessions.Update(rec.ThreadID, func(r *session.Record) { r.Model = m }) // …and persist it
		}
		// A double resume (say, a double-clicked Resume button) would spawn a
		// second process on the same thread id and corrupt the supervisor's
		// thread registry — the first process's reap would deregister the
		// second. One live process per thread, both backends.
		if d.agentRunning(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"thread "+p.ThreadID+" is already running")
		}
		h, ok := d.harnesses.Get(rec.Backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+rec.Backend)
		}
		if rec.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"thread has no "+h.Descriptor().DisplayName+" session to resume")
		}
		// The human got here first: drop any usage-window auto-resume armed for
		// this thread, so the schedule stops promising something that has
		// already happened (plan 28 §Phase 2).
		d.rateWakes.Cancel(p.ThreadID, "you resumed it yourself")
		providerID := p.ProviderID
		if providerID == "" {
			providerID = rec.ProviderID
		}
		provider, err := resolveProviderBinding(providerID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		safe.Go("agent.resumeThread", func() { resumeThread(d, h, rec, provider) })
		return map[string]any{"threadId": rec.ThreadID, "sessionId": rec.SessionID}, nil
	})

	// agent.transcript returns the Claude Code session transcript for a
	// thread, as the raw JSONL events. The UI replays these to rebuild the
	// conversation feed when reopening a dormant thread.
	//
	// SECURITY (audit F34): UI-only, and it is the same rule as the push side.
	// F6 made the live event channels UI-only; this is the PULL twin — the
	// whole raw transcript of ANY thread, tool inputs, the human's own
	// messages and whatever a tool printed into its output included. The UI
	// always handshakes before it asks, so the check costs it nothing, while a
	// bridge or any other local process reading another thread's conversation
	// off the socket now gets a refusal instead of the file.
	d.srv.Handle("agent.transcript", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		// The attachment sidecar (compact per-thread metadata) lets the UI redraw
		// the named/clickable chips on replayed "You" cards — the transcript keeps
		// only the inlined attachment content, not the origin name/path. Returned
		// as an ordered list the UI pairs with the user turns it replays.
		var attachTurns []session.AttachmentTurn
		if d.attachSide != nil {
			if t, err := d.attachSide.Load(p.ThreadID); err == nil {
				attachTurns = t
			}
		}
		if attachTurns == nil {
			attachTurns = []session.AttachmentTurn{}
		}
		// Each harness serves its own transcript source: claude reads the CLI's
		// session file (by session id), kimi its core-side translated-event log.
		events, err := d.harnessFor(p.ThreadID).ReadTranscript(p.ThreadID, rec.SessionID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if events == nil {
			events = []json.RawMessage{}
		}
		// Bound the reply (audit F10). The whole transcript travels as ONE
		// JSON-RPC result, and both sides cap an inbound frame at 16 MiB — a
		// months-old thread would otherwise be an undeliverable frame that cost
		// hundreds of MB to build on the way to failing. The tail is what the UI
		// keeps anyway (its row ring), and CapTranscript prepends a visible
		// notice when it drops anything, so a shortened history is never silent.
		// CLI transcripts do not reliably retain the completed answer to an
		// AskUserQuestion (and Kimi's ACP bridge has no native transcript for
		// it).  Append our private, completed-history sidecar only on this
		// UI-authorised pull path; no bridge ever receives another thread's
		// question text or answer.
		if d.questionSide != nil {
			questions, qerr := d.questionSide.Events(p.ThreadID)
			if qerr != nil {
				d.log.Warn("could not load question history", "thread", p.ThreadID, "err", qerr)
			} else {
				events = append(events, questions...)
			}
		}
		events = harness.CapTranscript(events)
		return map[string]any{"events": events, "attachments": attachTurns}, nil
	})

	// agent.fork branches a thread's conversation into a new thread so it can
	// continue on a different model or effort while keeping the full context.
	// The source thread is left running and untouched: the fork gets its own
	// isolated worktree (branched from the source worktree's HEAD; uncommitted
	// changes are not copied) and its own Claude Code session via --fork-session.
	//
	// UI-only (audit F5): a fork CREATES A THREAD, and it creates one carrying
	// the source's whole authority — its permission mode, its Env overlay with
	// the source's provider routing, its persona, and its CoworkEnabled flag.
	// An agent that could fork would have a way to duplicate any thread's
	// authority (including a cowork-enabled one) with no human anywhere in the
	// chain, which is the same escalation F1 closed on launch_agent. The MCP
	// bridge never calls this; the New Agent / roster UI does.
	d.srv.Handle("agent.fork", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Model    string `json:"model"`  // tier token ("opus"…); "" keeps the source's model
			Effort   string `json:"effort"` // "" keeps the source's effort
			Title    string `json:"title"`  // "" defaults to "Fork of <source title>"
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		src, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		h := d.harnessFor(p.ThreadID)
		if !h.Descriptor().Supports(harness.OperationFork) {
			return nil, unsupported("Forking", h.Descriptor())
		}
		if src.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this agent has no conversation yet to fork")
		}
		// A fork inherits the source's desktop access, so it goes through the
		// same gate every other thread-creating handler uses — the contract
		// authorizeCoworkAtStart states for itself. A UI caller passes on the
		// human's own click (the flag came from the source thread the human
		// enabled); any other caller has to ask, and cannot get here anyway
		// while requireUI stands above.
		if err := authorizeCoworkAtStart(d, ctx, src.CoworkEnabled,
			"Fork of "+src.Title, "forking thread "+src.ThreadID); err != nil {
			return nil, err
		}
		newThreadID := agent.NewThreadID()
		safe.Go("agent.forkThread", func() {
			forkAgentThread(d, h, src, newThreadID, p.Model, p.Effort, p.Title)
		})
		return map[string]any{"threadId": newThreadID}, nil
	})

	// agent.promote upgrades a non-isolated thread into a dedicated git
	// worktree, carrying its working-tree changes and Claude Code session over.
	//
	// UI-only (audit F5): it STOPS the agent, moves the working tree and the
	// session onto a new branch, and relaunches — a reconfiguration of where a
	// running thread's authority is pointed, and one that touches the human's
	// checkout. Not something another connection gets to do to a thread.
	d.srv.Handle("agent.promote", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		h := d.harnessFor(p.ThreadID)
		if !h.Descriptor().Supports(harness.OperationPromote) {
			return nil, unsupported("Promoting", h.Descriptor())
		}
		if rec.Worktree.Isolated {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this thread already runs in an isolated worktree")
		}
		safe.Go("agent.promoteThread", func() { promoteAgentThread(d, h, rec) })
		return map[string]any{"threadId": rec.ThreadID}, nil
	})

	// session.listThreads returns persisted threads (running and dormant),
	// optionally filtered to one project, so the UI can offer to resume them.
	//
	// SECURITY (audit F34 pass 3, reason corrected pass 4): UI-only, and the
	// reason is NOT the roster metadata the first version of this comment
	// claimed. agent.list already hands a bridge the project path, the worktree
	// path and branch, the title, the parent linkage and the role, so gating
	// those here would protect nothing — a comment that names the wrong reason
	// is the same defect as a label that names the wrong mechanism, and it is
	// how a control survives the refactor that makes it pointless.
	//
	// What this gate actually withholds is the rest of the RECORD, which
	// agent.list deliberately never projects: the thread's SystemPrompt (its
	// persona, verbatim), its Env overlay (variable names, and the values of
	// anything not caught by redaction), its provider routing (base URL, the
	// env var the API token is read from), its DisallowedTools / AddDirs /
	// StrictMCPConfig / MaxBudgetUSD restriction set — i.e. the exact map of
	// what each thread is allowed to do and where its credentials come from —
	// and its SessionID, which session.attach turns into a live thread.
	d.srv.Handle("session.listThreads", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p) // project is optional
		records := d.sessions.List(p.Project)
		out := make([]map[string]any, 0, len(records))
		for _, record := range records {
			out = append(out, sessionRecordWire(record))
		}
		return map[string]any{"threads": out}, nil
	})

	// session.browse merges every browse-capable harness's discoverable past
	// sessions (Claude Code transcripts on disk, kimi's session store), so the
	// user can attach any past conversation — even ones Agent Kate did not
	// start. A harness whose listing fails is skipped, never fatal: the others
	// still return.
	//
	// SECURITY (audit F34 pass 3): UI-only, same class and same reasoning as
	// session.preview, which F34 gated. It enumerates EVERY CLI session on the
	// machine — including conversations Agent Kate never started — and hands
	// back each one's session id, project path and title. The session id is not
	// merely descriptive: session.attach turns one into a live thread, so the
	// listing is also the discovery half of a resume. Its only caller is
	// ui/src/SessionBrowserDialog.cpp.
	d.srv.Handle("session.browse", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var found []harness.BrowsableSession
		for _, h := range d.harnesses.All() {
			descriptor := h.Descriptor()
			if !descriptor.Supports(harness.OperationSessionBrowse) {
				continue
			}
			list, err := h.BrowseSessions()
			if err != nil {
				d.log.Warn("session browse failed; skipping harness",
					"harness", descriptor.ID, "err", err)
				continue
			}
			found = append(found, list...)
		}
		known := map[string]bool{}
		for _, r := range d.sessions.List("") {
			known[r.SessionID] = true
		}
		for i := range found {
			found[i].Attached = known[found[i].SessionID]
		}
		// Newest first across harnesses. Every adapter emits second-precision
		// UTC RFC3339, so string order is time order.
		sort.SliceStable(found, func(i, j int) bool {
			return found[i].Updated > found[j].Updated
		})
		if len(found) > 500 {
			found = found[:500]
		}
		if found == nil {
			found = []harness.BrowsableSession{}
		}
		return map[string]any{"sessions": found}, nil
	})

	// session.attach turns a discovered past session into a dormant Agent Kate
	// thread, which the UI then resumes like any other. backend names the
	// owning harness (empty = the default).
	//
	// UI-only, same class as agent.start (audit F5): it creates a thread rooted
	// at an ARBITRARY project path from a caller-supplied session id, which
	// agent.resume then launches. Gating start while leaving its two-step
	// equivalent open would be no gate at all.
	d.srv.Handle("session.attach", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			SessionID string `json:"sessionId"`
			Project   string `json:"project"`
			Title     string `json:"title"`
			Backend   string `json:"backend"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" || p.Project == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"sessionId and project are required")
		}
		h, ok := d.harnesses.Get(p.Backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+p.Backend)
		}
		// Never attach the same session twice.
		if rec, ok := d.sessions.GetBySession(p.SessionID); ok {
			return map[string]any{"threadId": rec.ThreadID, "alreadyAttached": true}, nil
		}
		// A harness with a static mode vocabulary gets the conservative
		// apply-edits default the attach flow always used; a discovered
		// vocabulary stays empty — the CLI's own default applies at resume.
		mode := ""
		catalogue, err := h.Catalogue(ctx, harness.CatalogueScope{HarnessID: h.Descriptor().ID})
		if err == nil {
			for _, setting := range catalogue.Settings {
				if setting.Key == harness.SettingPermissionMode {
					mode = setting.DefaultValue
					break
				}
			}
		}
		threadID := agent.NewThreadID()
		rec := session.Record{
			ThreadID:  threadID,
			SessionID: p.SessionID,
			Project:   p.Project,
			Backend:   p.Backend,
			Worktree: worktree.Worktree{
				ThreadID: threadID,
				RepoRoot: p.Project,
				Path:     p.Project,
				Isolated: false, // resume in the conversation's own directory
			},
			PermissionMode: mode,
			Title:          summarizePrompt(p.Title),
			Created:        time.Now(),
			Status:         session.StatusDormant,
		}
		if err := d.sessions.Put(rec); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.log.Info("session attached", "thread", threadID, "session", p.SessionID)
		return map[string]any{"threadId": threadID, "alreadyAttached": false}, nil
	})

	// session.preview streams the last few turns of a discovered session's
	// transcript so the user can confirm what they are about to resume without
	// attaching it first. The whole file is never read into the reply.
	//
	// SECURITY (audit F34): UI-only, same data class as agent.transcript — and
	// wider in reach, because it addresses any session the CLI ever wrote on
	// this machine, not just threads Agent Kate knows about.
	d.srv.Handle("session.preview", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			SessionID   string `json:"sessionId"`
			MaxMessages int    `json:"maxMessages"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "sessionId is required")
		}
		msgs, truncated, err := session.PreviewTranscript(p.SessionID, p.MaxMessages)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if msgs == nil {
			msgs = []session.PreviewMessage{}
		}
		return map[string]any{"messages": msgs, "truncated": truncated}, nil
	})

	// session.forget deletes a discovered session's transcript from disk. It
	// refuses to act on a session that is attached as an Agent Kate thread —
	// the user must remove that agent first, so the thread never dangles.
	//
	// SECURITY (audit F36): UI-only. Deleting the human's conversation history
	// off disk is a decision for the human at the Sessions browser, which is
	// the only thing that calls it.
	d.srv.Handle("session.forget", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "sessionId is required")
		}
		if _, ok := d.sessions.GetBySession(p.SessionID); ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this session is attached as an agent; remove the agent first")
		}
		if err := session.DeleteTranscript(p.SessionID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("session forgotten", "session", p.SessionID)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("agent.send", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p agentSendParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// WHO is sending, before WHOM they may send to: a bridge may only speak
		// for its own thread, and a bridge that omitted fromThreadId used to be
		// read as the UI and skip the approval below entirely.
		if err := requireCallerThread(d, ctx, p.FromThreadID); err != nil {
			return nil, err
		}
		// Agent-to-agent sends outside the caller's own subtree need one human
		// approval (UI sends carry no FromThreadID and are never gated). When
		// the caller is going to wait for the reply, the SAME decision covers
		// the wait: the human is shown one prompt describing the exchange, and
		// it is shown BEFORE delivery, so declining means nothing was sent
		// rather than "sent, and the reply discarded" (audit F35 pass 3).
		var alsoGrant []string
		if p.AwaitReply {
			alsoGrant = append(alsoGrant, "wait_agent")
		}
		if err := d.authorizeAgentTarget(p.FromThreadID, p.ThreadID, "send_agent",
			map[string]any{"message": p.Text}, alsoGrant...); err != nil {
			return nil, err
		}
		var sendErr error
		if p.FromThreadID == "" {
			// requireCallerThread above proved this is the exclusive desktop UI.
			_, sendErr = d.humanSend(desktopPrincipal(), p.ThreadID, p.Text, p.Attachments)
		} else {
			// A bridge reached this point only after caller binding and any
			// cross-subtree grant. It shares delivery mechanics, not human
			// authority or the remote-human principal.
			sendErr = d.deliverAcceptedSend(p.ThreadID, p.Text, p.Attachments)
		}
		if sendErr != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, sendErr.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// SECURITY (audit F36): UI-only, and the odd one out until now — its
	// terminal sibling agent.stopClose got the full caller binding while the
	// plain stop, which ends any thread's process from any connection, got
	// none. There is no agent-facing tool for it (close_agent routes through
	// stopClose), so the human's window is its only legitimate caller.
	d.srv.Handle("agent.stop", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.humanStop(desktopPrincipal(), p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// agent.stopClose is the terminal "Stop & close" action: it runs the
	// configured hot-compaction (so the conversation is summarised while the KV
	// cache is warm), stops the process, then ARCHIVES the thread's record —
	// moving it out of the live roster while keeping it (and its worktree and
	// transcript) fully recoverable via the Sessions browser. Unlike agent.stop,
	// which leaves the thread dormant-and-resumable, this clears the roster entry.
	// The worktree is deliberately NOT removed here (that is cleanup's job), so a
	// later Restore is lossless.
	d.srv.Handle("agent.stopClose", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// WHO is closing, before WHOM they may close. Without this the
		// self-close refusal and the subtree gate below both ran against a
		// thread id the caller simply asserted — and a bridge that sent no
		// fromThreadId passed as the UI and closed anything in the arena.
		if err := requireCallerThread(d, ctx, p.FromThreadID); err != nil {
			return nil, err
		}
		// close_agent (agent-driven): refuse self-close — a thread stopping
		// itself mid-turn wedges its own tool call — and gate targets outside
		// the caller's subtree behind one human approval.
		if p.FromThreadID != "" {
			if p.FromThreadID == p.ThreadID {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"an agent cannot close itself — ask the human to close this thread")
			}
			if err := d.authorizeAgentTarget(p.FromThreadID, p.ThreadID,
				"close_agent", nil); err != nil {
				return nil, err
			}
		}
		// Hot-compaction must run against the live session, before termination.
		// It is a no-op for a dormant thread (it checks agentRunning first).
		runHotCompactIfConfigured(d, p.ThreadID)
		// Stop the process and wait for it to exit so its cooperation locks and
		// git watch are torn down by the reap/lifecycle path before we archive.
		// A dormant thread has already been reaped, so the supervisor no longer
		// tracks it and Stop reports "unknown thread" — that is the normal
		// dormant state, not an error, so ignore it and proceed to archive.
		if d.agentRunning(p.ThreadID) {
			if err := d.agentStop(p.ThreadID); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
			}
		}
		for i := 0; i < 60 && d.agentRunning(p.ThreadID); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		// Archive the record (reversible). A dormant thread with no live process
		// still has a record to archive; a thread that was never started has none.
		if _, ok := d.sessions.Get(p.ThreadID); ok {
			if err := d.sessions.Archive(p.ThreadID, "stop & close"); err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, "archive failed: "+err.Error())
			}
		}
		// Drop the live-thread bookkeeping; the worktree on disk is left intact.
		d.gitCache.Forget(p.ThreadID)
		d.turns.Forget(p.ThreadID)
		d.threads.remove(p.ThreadID)
		// An archived thread must not be woken by a usage-window resume armed
		// while it was still live.
		d.rateWakes.Cancel(p.ThreadID, "the agent was closed")
		d.log.Info("agent stopped & closed (archived)", "thread", p.ThreadID)
		return map[string]any{"ok": true}, nil
	})

	// agent.updateSettings applies a complete typed requested settings object.
	// The response says what native state actually took effect and when; a
	// client must not treat a request as fact merely because it was accepted.
	//
	// UI-only (audit F5), and this is the sharpest of the three: `option:
	// "permissionMode"` RE-ARMS A LIVE THREAD'S AUTHORITY. Without the check, a
	// prompt-injected agent could raise ITS OWN mode to bypassPermissions
	// mid-turn and skip every gate — a shorter path than launching a worker,
	// and one the launch gate would never see. The change is persisted too, so
	// it would survive the resume. Only the human's own pickers may move it.
	d.srv.Handle("agent.updateSettings", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			AgentRef  harness.AgentRef      `json:"agentRef"`
			Requested harness.AgentSettings `json:"requested"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.AgentRef.ThreadID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "agentRef.threadId is required")
		}
		if !d.agentRunning(p.AgentRef.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"the agent is not running — options apply at the next start instead")
		}
		record, _ := d.sessions.Get(p.AgentRef.ThreadID)
		if p.AgentRef.HarnessID == "" {
			p.AgentRef.HarnessID = record.Backend
		}
		if p.AgentRef.NativeSessionID == "" {
			p.AgentRef.NativeSessionID = record.SessionID
		}
		applied, err := d.harnessFor(p.AgentRef.ThreadID).UpdateSettings(ctx, p.AgentRef, p.Requested)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Persist so a resume replays the latest choice, not the start-time one.
		_ = d.sessions.UpdateQuiet(p.AgentRef.ThreadID, func(r *session.Record) {
			if applied.Effective.Model != "" {
				r.Model = applied.Effective.Model
			}
			if applied.Effective.ReasoningEffort != "" {
				r.Effort = applied.Effective.ReasoningEffort
			}
			if applied.Effective.PermissionMode != "" {
				r.PermissionMode = applied.Effective.PermissionMode
			}
		})
		d.log.Info("agent settings updated", "thread", p.AgentRef.ThreadID,
			"timing", applied.Timing)
		return applied, nil
	})

	// agent.interrupt cancels the in-flight turn immediately (no further tokens
	// billed) while keeping the process resident and the session hot: the next
	// agent.send goes down the same stdin with no resume cost. The supervisor
	// emits a `turn_aborted` lifecycle event once the aborted turn's result lands
	// (or, for a hung tool the CLI can't cancel in-band, escalates to signals and
	// the thread goes dormant). No hot-compaction here — interrupt is meant to be
	// instantaneous, and spending a summary turn would defeat the purpose.
	//
	// SECURITY (audit F36): UI-only — it is the Esc key, and only the human
	// presses it. No agent-facing tool interrupts another thread's turn.
	d.srv.Handle("agent.interrupt", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.humanInterrupt(desktopPrincipal(), p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// SECURITY (audit F34 pass 4): UI-only. This is FILE CONTENT — the whole
	// working diff of any thread named on the wire, its worktree's source lines
	// verbatim — and it was left ungated through the round that gated
	// search.code and session.browse for exactly the same data class. "Read
	// another agent's file content" was one RPC name away from where F34 found
	// it. Its only caller is ui/src/AgentPanel.cpp; an agent that wants a diff
	// of its OWN worktree runs git in its own shell.
	d.srv.Handle("agent.diff", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p agentDiffParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		diff, err := worktree.Diff(wt)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{
			"diff":     diff,
			"isolated": wt.Isolated,
			"branch":   wt.Branch,
		}, nil
	})

	// SECURITY (audit F36): the git mutations below (commit / openPR / land, and
	// their git.* twins further down) are UI-only. Each writes to a thread's
	// worktree or branch — someone else's worktree, from any connection that
	// names the id — and openPR reaches OUTWARD, pushing a branch and opening a
	// pull request through `gh`. All of them are review actions the human takes
	// after reading a diff; nothing agent-facing calls them.
	d.srv.Handle("agent.commit", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.Commit(wt, p.Message); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.Invalidate(p.ThreadID)
		return map[string]any{"ok": true, "branch": wt.Branch}, nil
	})

	d.srv.Handle("agent.openPR", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		url, err := worktree.OpenPR(wt, p.Title)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"url": url}, nil
	})

	// agent.land merges a thread's branch into whatever branch the workspace is
	// checked out on right now (`git branch --show-current`) — NOT necessarily
	// "main". A local integration, separate from agent.openPR which targets
	// GitHub. The UI's land label says the same thing (WorktreeReviewCopy.h).
	d.srv.Handle("agent.land", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		target, err := worktree.Land(rec.Worktree)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The land touched both the worktree and the main repo — invalidate
		// every entry so the dashboard reflects the new ahead/behind.
		d.gitCache.InvalidateAll()
		d.log.Info("agent thread landed", "thread", p.ThreadID,
			"branch", rec.Worktree.Branch, "into", target)
		return map[string]any{"branch": rec.Worktree.Branch, "into": target}, nil
	})

	// agent.list is the roster projection the orchestration tools run on, and
	// the ONE roster read an agent's own bridge may make (list_agents).
	//
	// SECURITY (audit F34 pass 4): the projection IS the boundary, so it is
	// built field by field from the record rather than by marshalling the
	// record — and it is kept to what the bridge in mcp.go actually consumes.
	// The gate on session.listThreads is only worth something while this list
	// stays short: every field added here is a field an agent can read about
	// every thread on the machine, its human's own private threads included.
	// Adding one is a decision, not a convenience; TestAgentListProjectionIsNarrow
	// fails when the set changes so that the decision has to be made on purpose.
	//
	// Deliberately absent, and why: `tags` (the human's own filing labels),
	// `updated`, `backend` and the resolved `harness` capability struct (which
	// engine each thread runs — harness.list already answers that
	// per-engine, without saying who is running what).
	d.srv.Handle("agent.list", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p)
		recs := d.sessions.List(p.Project)
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			out = append(out, map[string]any{
				"threadId": r.ThreadID,
				"project":  r.Project,
				"title":    r.Title,
				"status":   r.Status,
				"branch":   r.Worktree.Branch,
				"path":     r.Worktree.Path,
				"isolated": r.Worktree.Isolated,
				"number":   r.Worktree.Number,
				"created":  r.Created,
				"lastTurn": r.LastTurnAt,
				"model":    r.Model,
				// Orchestration linkage: which thread launched this one (empty
				// for human-launched threads) and its controller/worker role.
				"parentThreadId": r.ParentThreadID,
				"role":           r.Role,
			})
		}
		return map[string]any{"threads": out}, nil
	})

	// agent.rename persists a user-chosen title for a thread. No worktree or
	// process is touched — only the session record's Title field is updated, so
	// the new name survives restart (session.listThreads reads it back).
	//
	// SECURITY (audit F36): UI-only. The title is roster-facing text the human
	// reads to tell threads apart; a thread that could retitle its neighbours
	// could disguise them.
	d.srv.Handle("agent.rename", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" || p.Title == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId and title are required")
		}
		if _, ok := d.sessions.Get(p.ThreadID); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Title = p.Title
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// --- tags --------------------------------------------------------------
	// Roster organization labels. The store is a dumb setter; normalization
	// (trim, dedupe, cap length/count) happens here so threads.json never
	// holds malformed tags. Every successful mutation broadcasts
	// agent.tagsChanged so all roster clients converge.

	// agent.setTags replaces a thread's full tag set.
	//
	// SECURITY (audit F36): UI-only, and so are addTag/removeTag below. The
	// audit named setTags; gating it alone would be theatre, since addTag and
	// removeTag reach the same field with the same authority.
	d.srv.Handle("agent.setTags", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string   `json:"threadId"`
			Tags     []string `json:"tags"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if _, ok := d.sessions.Get(p.ThreadID); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		tags := normalizeTags(p.Tags)
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Tags = tags
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.srv.Notify("agent.tagsChanged", map[string]any{
			"threadId": p.ThreadID, "tags": tags})
		return map[string]any{"ok": true, "tags": tags}, nil
	})

	// agent.addTag adds one tag to a thread, returning the full normalized set.
	d.srv.Handle("agent.addTag", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Tag      string `json:"tag"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		tags := normalizeTags(append(append([]string{}, rec.Tags...), p.Tag))
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Tags = tags
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.srv.Notify("agent.tagsChanged", map[string]any{
			"threadId": p.ThreadID, "tags": tags})
		return map[string]any{"ok": true, "tags": tags}, nil
	})

	// agent.removeTag drops one tag (case-insensitive) from a thread.
	d.srv.Handle("agent.removeTag", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Tag      string `json:"tag"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		drop := strings.ToLower(strings.TrimSpace(p.Tag))
		kept := make([]string, 0, len(rec.Tags))
		for _, t := range rec.Tags {
			if strings.ToLower(strings.TrimSpace(t)) != drop {
				kept = append(kept, t)
			}
		}
		tags := normalizeTags(kept)
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Tags = tags
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.srv.Notify("agent.tagsChanged", map[string]any{
			"threadId": p.ThreadID, "tags": tags})
		return map[string]any{"ok": true, "tags": tags}, nil
	})

	// agent.suggestTags runs a one-shot Sonnet pass over a project's threads
	// and returns proposed tag assignments. It is read-only — it applies
	// nothing; the UI previews the proposals and applies them via setTags.
	//
	// SECURITY (audit F34 pass 4): UI-only, for two reasons. It reads every
	// thread's record to describe it to a model, so the answer is a digest of
	// the roster an agent is not otherwise given; and it SPENDS the human's
	// money on an API turn per call, from a caller who was never asked. Its
	// only caller is ui/src/AgentDock.cpp.
	d.srv.Handle("agent.suggestTags", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p) // project optional → all threads
		recs := d.sessions.List(p.Project)
		if len(recs) == 0 {
			return map[string]any{"proposals": []any{}}, nil
		}
		sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		proposals, err := session.SuggestTagOrganization(sctx, recs, "", "claude-sonnet-4-6")
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		out := make([]map[string]any, 0, len(proposals))
		for _, pr := range proposals {
			out = append(out, map[string]any{"threadId": pr.ThreadID, "tags": pr.Tags})
		}
		return map[string]any{"proposals": out}, nil
	})

	d.srv.Handle("agent.discard", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			// FromThreadID names the agent thread issuing the discard (the
			// bridge's discard_agent); empty for UI-driven discards. Same
			// cross-subtree approval rule as sends/closes — without it any
			// agent could destroy any thread's worktree, its controller's
			// included.
			FromThreadID string `json:"fromThreadId,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// WHO is discarding, before WHOM they may discard. This is the most
		// destructive of the three (it removes the worktree), and the id it
		// gates on was, until now, whatever the caller typed.
		if err := requireCallerThread(d, ctx, p.FromThreadID); err != nil {
			return nil, err
		}
		if err := d.authorizeAgentTarget(p.FromThreadID, p.ThreadID,
			"discard_agent", nil); err != nil {
			return nil, err
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		// Resolved BEFORE the record is dropped below — harnessFor needs it.
		h := d.harnessFor(p.ThreadID)
		_ = d.agentStop(p.ThreadID)
		// Stop returns immediately — a graceful busy-stop takes up to ~11 s —
		// and the next step deletes the process's cwd, so wait for the exit
		// first and refuse when it never comes (audit F54).
		if !waitAgentExit(d, p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInternalError,
				"the agent process did not exit; nothing was removed — try again")
		}
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The worktree is gone, so the thread can never be resumed — forget it,
		// including the core-owned transcript nothing used to delete (audit F10).
		deleteThreadTranscript(h, p.ThreadID, d.log)
		_ = d.sessions.Remove(p.ThreadID)
		_ = d.summaries.Remove(p.ThreadID)
		if d.attachSide != nil {
			_ = d.attachSide.Remove(p.ThreadID)
		}
		if d.questionSide != nil {
			_ = d.questionSide.Remove(p.ThreadID)
		}
		d.gitCache.Forget(p.ThreadID)
		d.turns.Forget(p.ThreadID)
		// A discarded thread has no worktree left to resume into, so any
		// usage-window wake armed for it is disarmed here rather than left to
		// fail at 3am.
		d.rateWakes.Cancel(p.ThreadID, "the agent was discarded")
		// Approval grants that named this thread (as granter or target) are
		// meaningless now — and must not silently cover a future thread that
		// happens to reuse the id.
		d.orchGrants.forgetThread(p.ThreadID)
		// ...and so are its bridge secrets, for the same reason: a reused thread
		// id must not be identifiable with a dead thread's secret (F13).
		d.bridgeSecrets.forget(p.ThreadID)
		// Tell every UI client to drop this thread from its roster.
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		return map[string]any{"ok": true}, nil
	})

	// --- compaction --------------------------------------------------------
	// Reduces prefix re-cache cost on resume. See package compact.
	//
	// SECURITY (audit F36 pass 3): UI-only, like every other per-thread setting
	// (agent.rename, agent.setTags, agent.updateSettings). It writes to ANY thread's
	// record with no caller binding, and the setting it writes is not cosmetic:
	// a strategy arms a pass that rewrites the thread's stored context at exit
	// and reseeds its next resume from the result, so an agent could quietly
	// arrange for another agent's history to be replaced by a summary. Only the
	// human's window changes how a thread remembers itself.
	d.srv.Handle("agent.setCompactStrategy", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Strategy string `json:"strategy"`
			Strip    bool   `json:"strip"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		s := compact.Strategy(p.Strategy)
		if p.Strategy != "" && !s.Valid() {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown strategy "+p.Strategy)
		}
		descriptor := d.descriptorFor(p.ThreadID)
		if !descriptor.Supports(harness.OperationCompaction) {
			return nil, unsupported("Compaction", descriptor)
		}
		// Every strategy but the hot one runs after the process is gone, off a
		// transcript the engine may not write. Storing one on an engine with no
		// cold pass would arm a compaction that can never fire (or, worse, one
		// that summarises nothing and reseeds the next resume from it).
		if !descriptor.Supports(harness.OperationColdCompaction) && s.Resolve() != compact.ExitOpusHot {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				descriptor.DisplayName+" agents compact only inside a live session; "+
					"the only strategy they support is "+string(compact.ExitOpusHot))
		}
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.CompactStrategy = p.Strategy
			r.CompactStrip = p.Strip
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// agent.summaryStatus reports whether a thread has a current compacted
	// summary on disk, used by the UI to drive the recovery dialog on resume.
	//
	// SECURITY (audit F36 pass 4): UI-only, the read half of the pair whose
	// write half (agent.setCompactStrategy) is gated. It answers for ANY thread
	// named on the wire — whether it has a summary, how many turns went into
	// it, when it was last compacted and when it last spoke — which is a
	// per-thread activity trace for the human's own private threads. Its only
	// caller is ui/src/AgentPanel.cpp.
	d.srv.Handle("agent.summaryStatus", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if descriptor := d.descriptorFor(p.ThreadID); !descriptor.Supports(harness.OperationCompaction) {
			return nil, unsupported("Compaction", descriptor)
		}
		sum, err := d.summaries.Get(p.ThreadID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		out := map[string]any{
			"hasSummary": sum != nil,
			"strategy":   rec.CompactStrategy,
			"strip":      rec.CompactStrip,
			"lastTurnAt": rec.LastTurnAt,
			"updatedAt":  rec.SummaryUpdatedAt,
		}
		if sum != nil {
			out["summaryTurns"] = sum.Turns
			out["summaryCreated"] = sum.Created
		}
		// Stale when there is no summary at all, or the latest user/assistant
		// turn happened after the last compaction.
		out["stale"] = sum == nil ||
			(!rec.LastTurnAt.IsZero() && rec.LastTurnAt.After(rec.SummaryUpdatedAt))
		return out, nil
	})

	// agent.compactNow runs a compaction synchronously with the given model.
	// Used by the resume-time recovery dialog and the explicit "Compact now"
	// UI action. model accepts: "hot" / "opus_hot" (inline on the live
	// thread), "opus", "sonnet", "haiku", "local" (case-insensitive), or a
	// full claude --model id like "claude-sonnet-4-6".
	//
	// SECURITY (audit F36 pass 4): UI-only, and it needed this gate MORE than
	// agent.setCompactStrategy, which got one a round earlier. The strategy
	// setting ARMS a rewrite of another thread's stored context; this PERFORMS
	// one, at once, on any thread named on the wire — it injects a turn into a
	// live session (the hot path), replaces that thread's history with a
	// summary the next resume is seeded from, and spends the human's money on
	// an Opus/Sonnet pass doing it. Gating the setting and not the act was
	// exactly backwards. Its only caller is ui/src/AgentPanel.cpp.
	d.srv.Handle("agent.compactNow", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if descriptor := d.descriptorFor(p.ThreadID); !descriptor.Supports(harness.OperationCompaction) {
			return nil, unsupported("Compaction", descriptor)
		}
		if rec.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"thread has no "+d.descriptorFor(p.ThreadID).DisplayName+" session yet")
		}

		var sum compact.Summary
		token := strings.ToLower(strings.TrimSpace(p.Model))
		if token == "hot" || token == "opus_hot" || token == "hot_opus" {
			// Hot path: send the compact prompt into the live thread and use
			// its assistant reply as the summary. Requires a running thread —
			// the harness enforces that, since only it knows.
			if !d.agentRunning(p.ThreadID) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"Hot Opus compaction requires a running thread; resume it first")
			}
			hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			// The compact prompt is a turn; track it so waiters see the
			// thread busy until the summary's result lands (same bracketing
			// as runHotCompactIfConfigured).
			d.turns.TurnQueued(p.ThreadID)
			text, herr := d.harnessFor(p.ThreadID).Compact(hctx, harness.CompactSpec{
				ThreadID: p.ThreadID,
				Prompt:   compact.CompactPrompt,
				Hot:      true,
			})
			if herr != nil {
				// However it ended, no result event is coming for a turn that
				// never ran: release it, or the thread stays busy forever for
				// agent.wait and the Jobs panel. (The in-place sentinel is a
				// SUCCESS — its turn really ran and its result will land.)
				if !errors.Is(herr, ErrCompactedInPlace) {
					d.turns.TurnFailed(p.ThreadID)
				}
				// Not every engine's hot compaction yields text. One that
				// rewrites its own context in place has already succeeded —
				// there is simply nothing to store and nothing to reseed from,
				// so report success rather than turning a win into an error.
				if errors.Is(herr, ErrCompactedInPlace) {
					d.log.Info("compacted in place; no summary stored",
						"thread", p.ThreadID)
					return map[string]any{
						"ok":               true,
						"strategy":         string(compact.ExitOpusHot),
						"compactedInPlace": true,
						"detail":           herr.Error(),
					}, nil
				}
				return nil, ipc.Errorf(ipc.CodeInternalError, herr.Error())
			}
			if strings.TrimSpace(text) == "" {
				d.turns.TurnFailed(p.ThreadID)
				return nil, ipc.Errorf(ipc.CodeInternalError, "hot compaction returned empty body")
			}
			sum = compact.Summary{
				ThreadID:  p.ThreadID,
				SessionID: rec.SessionID,
				Strategy:  compact.ExitOpusHot,
				Stripped:  rec.CompactStrip,
				Created:   time.Now().UTC(),
				Body:      text,
			}
		} else {
			// The cold mechanisms read the session back from disk (local) or
			// re-run the CLI over it (model pass). A harness without
			// ColdCompact has neither, and running them anyway would store a
			// summary of nothing — which the next resume would seed a fresh
			// session from, throwing the real history away.
			if descriptor := d.descriptorFor(p.ThreadID); !descriptor.Supports(harness.OperationColdCompaction) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					descriptor.DisplayName+" agents compact only inside a live session; "+
						"resume the thread and compact it hot instead")
			}
			modelID, strategy, isLocal := resolveCompactModel(p.Model)
			if isLocal {
				// Bounded (audit F10): a very long transcript is summarised
				// from its recent tail, with the truncation notice carried in
				// the events — Programmatic ignores that event type, so the
				// summary is simply of the tail. Deliberate: reading hundreds
				// of MB into the core to summarise it is the freeze the cap
				// exists to prevent, and the recent turns are what a resume
				// needs.
				events, rerr := session.ReadTranscript(rec.SessionID)
				if rerr != nil {
					return nil, ipc.Errorf(ipc.CodeInternalError, rerr.Error())
				}
				sum = compact.Programmatic(p.ThreadID, rec.SessionID, events)
			} else {
				body, err := d.harnessFor(p.ThreadID).Compact(ctx, harness.CompactSpec{
					ThreadID:  p.ThreadID,
					SessionID: rec.SessionID,
					WorkDir:   rec.Worktree.Path,
					Model:     modelID,
					Prompt:    compact.CompactPrompt,
					Timeout:   5 * time.Minute,
				})
				if err != nil {
					return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
				}
				sum = compact.Summary{
					ThreadID:  p.ThreadID,
					SessionID: rec.SessionID,
					Strategy:  strategy,
					Stripped:  rec.CompactStrip,
					Created:   time.Now().UTC(),
					Body:      body,
				}
			}
		}
		if err := d.summaries.Put(sum); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		_ = d.sessions.UpdateQuiet(p.ThreadID, func(r *session.Record) {
			r.SummaryUpdatedAt = sum.Created
		})
		d.log.Info("compaction complete",
			"thread", p.ThreadID, "strategy", sum.Strategy,
			"turns", sum.Turns, "body_bytes", len(sum.Body))
		return map[string]any{
			"ok":        true,
			"strategy":  string(sum.Strategy),
			"turns":     sum.Turns,
			"bodyBytes": len(sum.Body),
		}, nil
	})

	// --- cooperation state (shared with the Cooperation MCP) ---------------
	d.srv.Handle("coop.setOpenFiles", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p coopSetOpenFilesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		owner, err := coopCallerIdentity(ctx, p.Owner)
		if err != nil {
			return nil, err
		}
		d.coop.SetOpenFiles(owner, p.Files)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.listOpenFiles", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"files": d.coop.ListOpenFiles()}, nil
	})

	d.srv.Handle("coop.postNote", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p coopPostNoteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		author, err := coopCallerIdentity(ctx, p.Author)
		if err != nil {
			return nil, err
		}
		note := d.coop.PostNote(author, p.Text)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return note, nil
	})

	d.srv.Handle("coop.readNotes", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"notes": d.coop.ReadNotes()}, nil
	})

	d.srv.Handle("coop.setPresence", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner       string `json:"owner"`
			FocusedFile string `json:"focusedFile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		owner, err := coopCallerIdentity(ctx, p.Owner)
		if err != nil {
			return nil, err
		}
		d.coop.SetPresence(owner, p.FocusedFile)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.getPresence", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{
			"presence":  d.coop.ListPresence(),
			"claims":    d.coop.ListClaims(),
			"openFiles": d.coop.ListOpenFiles(),
		}, nil
	})

	d.srv.Handle("coop.claimFile", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		owner, err := coopCallerIdentity(ctx, p.Owner)
		if err != nil {
			return nil, err
		}
		ok, holder := d.coop.ClaimFile(p.Path, owner)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": ok, "holder": holder}, nil
	})

	d.srv.Handle("coop.releaseFile", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		owner, err := coopCallerIdentity(ctx, p.Owner)
		if err != nil {
			return nil, err
		}
		d.coop.ReleaseFile(p.Path, owner)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.requestReview", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Thread  string `json:"thread"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// The reviewed thread is the CALLER's for a bridge — a review filed
		// under somebody else's name would point the human at the wrong diff.
		thread, err := coopCallerIdentity(ctx, p.Thread)
		if err != nil {
			return nil, err
		}
		rev := d.coop.AddReview(thread, p.Summary)
		d.srv.Notify("agent.reviewRequested", map[string]any{
			"threadId": thread,
			"summary":  p.Summary,
			"id":       rev.ID,
		})
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"id": rev.ID}, nil
	})

	d.srv.Handle("coop.listReviews", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"reviews": d.coop.ListReviews()}, nil
	})

	// coop.getState is the UI's one-shot read of the whole cooperation board for
	// the Cooperation panel: presence, soft-lock claims, open files, notes and the
	// review backlog. The panel refreshes it whenever a coop.changed notification
	// fires (any mutation, agent- or human-driven).
	d.srv.Handle("coop.getState", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{
			"presence":  d.coop.ListPresence(),
			"claims":    d.coop.ListClaims(),
			"openFiles": d.coop.ListOpenFiles(),
			"notes":     d.coop.ReadNotes(),
			"reviews":   d.coop.ListReviews(),
		}, nil
	})

	// --- per-tool approval -------------------------------------------------
	// permission.request comes from an agent's MCP bridge and blocks until the
	// human answers via permission.respond (or an 8-minute safety timeout).
	//
	// SECURITY (audit F36): the threadId is the caller's own, not a parameter
	// it may choose. It decides which panel the dialog opens in and which
	// thread the human believes is asking, so an unbound id let one thread
	// raise a prompt in another thread's panel — the human approving a Bash
	// line they think belongs to the agent they are watching — and let it park
	// requests against a thread that will never answer. RequireBridge is the
	// same door bridge.identify authenticated; the UI never calls this.
	d.srv.Handle("permission.request", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string          `json:"threadId"`
			ToolName string          `json:"toolName"`
			Input    json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if ok, reason := d.srv.RequireBridge(ctx, p.ThreadID); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this connection may not request permission for thread "+
					p.ThreadID+": "+reason)
		}
		dec, ok := askHumanPermission(d.srv, d.broker, p.ThreadID, p.ToolName, p.Input)
		if !ok {
			// Two ways to get here, and they are worth telling apart in the
			// log: the human let the 8-minute window lapse, or there was no
			// window to ask at all (audit F35 pass 3). Both deny — the wire
			// answer is unchanged — but a headless core denying instantly must
			// not read as a human who said no.
			d.log.Warn("tool permission denied without a human answer",
				"thread", p.ThreadID, "tool", p.ToolName, "uiConnected", d.srv.HasUI())
			return map[string]any{"allow": false}, nil
		}
		res := map[string]any{"allow": dec.Allow}
		if len(dec.UpdatedInput) > 0 {
			res["updatedInput"] = dec.UpdatedInput
		}
		return res, nil
	})

	// permission.respond carries the HUMAN's answer, so it is UI-only (audit
	// F6). Without this, any connection could resolve any open request id and
	// answer an agent's tool prompt on the human's behalf — racing the human to
	// "Allow" on the primary approval flow of both backends. Same rule as its
	// Cowork siblings (cowork.respondGrant, cowork.setEnabled).
	d.srv.Handle("permission.respond", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			RequestID    string          `json:"requestId"`
			Allow        bool            `json:"allow"`
			UpdatedInput json.RawMessage `json:"updatedInput"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Resolve reports whether the decision was actually delivered to a waiting
		// request; surface it so the UI can tell a real answer from one that hit an
		// already-timed-out or unknown request (a stale dialog) instead of always
		// claiming success.
		delivered := d.humanRespondPermission(desktopPrincipal(), p.RequestID, p.Allow, p.UpdatedInput)
		return map[string]any{"ok": delivered}, nil
	})

	// --- VS Code extension reuse -------------------------------------------
	// vsix.install downloads an extension from Open VSX, unpacks it and
	// detects the language server it bundles. It blocks for the duration of
	// the download; the IPC server dispatches each request on its own
	// goroutine, so other traffic is unaffected.
	//
	// SECURITY (audit F36): UI-only, install and uninstall both. Installing an
	// extension fetches a third-party archive off the network and unpacks
	// executable content into the user's Agent Kate directory — a choice for
	// the human at the Extensions dialog, which is its only caller.
	d.srv.Handle("vsix.install", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ExtensionID string `json:"extensionId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ExtensionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "extensionId is required")
		}
		// Throttle progress so a fast download cannot flood the socket: emit at
		// most every 250ms or whenever the fraction advances by >= 1%.
		var lastEmit time.Time
		var lastFrac float64
		ext, err := d.extensions.InstallProgress(ctx, p.ExtensionID, func(done, total int64) {
			var frac float64
			if total > 0 {
				frac = float64(done) / float64(total)
			}
			now := time.Now()
			if now.Sub(lastEmit) < 250*time.Millisecond && frac-lastFrac < 0.01 && frac < 1.0 {
				return
			}
			lastEmit = now
			lastFrac = frac
			d.srv.Notify("vsix.installProgress", map[string]any{
				"extensionId":   p.ExtensionID,
				"fraction":      frac,
				"indeterminate": total == 0,
			})
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.log.Info("extension installed", "id", ext.ID, "version", ext.Version,
			"hasServer", ext.Server != nil)
		return ext, nil
	})

	// vsix.uninstall deletes an installed extension from the cache. The id is
	// validated and resolved under the cache dir inside Manager.Remove, so a
	// crafted id can never delete anything outside it.
	d.srv.Handle("vsix.uninstall", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ExtensionID string `json:"extensionId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ExtensionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "extensionId is required")
		}
		if err := d.extensions.Remove(p.ExtensionID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("extension uninstalled", "id", p.ExtensionID)
		return map[string]any{"ok": true}, nil
	})

	// UI-only (audit F36 pass 5). "Installed extension ids" was an honest
	// description of the list and a misleading one of the CALL: every
	// invocation fans out one marketplace request per installed extension, so a
	// bridge that could reach it held a parameterless outbound amplifier — and
	// nothing on the agent side has ever wanted the human's extension
	// inventory (the only caller is ui/src/ExtensionsDialog.cpp).
	d.srv.Handle("vsix.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		exts, err := d.extensions.List()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if exts == nil {
			exts = []*vsix.Extension{}
		}
		// Enrich each entry with the latest published version so the UI can
		// flag updates. This is best effort and concurrency-bounded: any
		// lookup error (offline, removed upstream) simply omits the field and
		// never fails the list. A short timeout keeps the dialog responsive.
		out := make([]map[string]any, len(exts))
		latest := make([]string, len(exts))
		lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for i, e := range exts {
			wg.Add(1)
			i, id := i, e.ID
			safe.Go("vsix.latestVersion", func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if v, err := d.extensions.LatestVersion(lctx, id); err == nil {
					latest[i] = v
				}
			})
		}
		wg.Wait()
		for i, e := range exts {
			m := map[string]any{
				"id":         e.ID,
				"name":       e.Name,
				"version":    e.Version,
				"dir":        e.Dir,
				"server":     e.Server,
				"serverHint": e.ServerHint,
			}
			if latest[i] != "" {
				m["latest"] = latest[i]
				m["updateAvailable"] = latest[i] != e.Version
			}
			out[i] = m
		}
		return map[string]any{"extensions": out}, nil
	})

	// vsix.search queries the Open VSX registry. It is network-dependent and
	// best effort — a failure returns an error the UI surfaces inline rather
	// than blocking the dialog. Hits already installed are tagged like the
	// curated catalog so the UI can disable their Install button.
	//
	// UI-only (audit F36 pass 5): "no local state" was true and beside the
	// point — the caller-supplied query is put on the wire, so an agent bridge
	// held an outbound channel with attacker-chosen content. The install half
	// was already gated; searching is the same reach with a smaller payload.
	d.srv.Handle("vsix.search", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		entries, err := d.extensions.Search(sctx, p.Query)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		installed := map[string]bool{}
		if list, err := d.extensions.List(); err == nil {
			for _, e := range list {
				installed[e.ID] = true
			}
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"id":          e.ID,
				"displayName": e.DisplayName,
				"summary":     e.Summary,
				"category":    e.Category,
				"installed":   installed[e.ID],
			})
		}
		return map[string]any{"entries": out}, nil
	})

	// vsix.catalog returns the curated list of popular extensions, each
	// tagged with whether the user already has it installed so the UI can
	// hide its Install button.
	d.srv.Handle("vsix.catalog", func(_ context.Context, _ json.RawMessage) (any, error) {
		installed := map[string]bool{}
		if list, err := d.extensions.List(); err == nil {
			for _, e := range list {
				installed[e.ID] = true
			}
		}
		entries := vsix.Catalog()
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"id":          e.ID,
				"displayName": e.DisplayName,
				"summary":     e.Summary,
				"category":    e.Category,
				"installed":   installed[e.ID],
			})
		}
		return map[string]any{"entries": out}, nil
	})

	// --- Claude Code skills ------------------------------------------------
	// skills.listCatalog returns every skill in the central Agent Kate catalog
	// (XDG_DATA_HOME/agentkate/skills). An empty catalog is fine — the UI
	// reveals the catalog directory so the user can drop skills into it.
	d.srv.Handle("skills.listCatalog", func(_ context.Context, _ json.RawMessage) (any, error) {
		if err := d.skills.EnsureDir(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		list, err := d.skills.List()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"skills": list, "catalogDir": d.skills.Dir()}, nil
	})

	// skills.listInstalled enumerates target/.claude/skills, flagging the
	// entries the catalog owns so the UI can show their install state.
	//
	// UI-only (audit F36 pass 5). `target` is a caller-supplied DIRECTORY, so
	// "installed skill names" described the reply while hiding the parameter:
	// an agent bridge could walk it across the filesystem and read back which
	// paths carry a .claude/skills tree. Its three mutating siblings were gated
	// in pass 4; the read that names the same arbitrary path was not.
	d.srv.Handle("skills.listInstalled", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		list, err := d.skills.ListInstalled(p.Target)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"installed": list}, nil
	})

	// SECURITY (audit F36): the three mutating skill handlers (install,
	// uninstall, create) are UI-only. A skill is instruction text every agent
	// in the target directory loads, so writing one is writing into every
	// future agent's prompt — the one thing a prompt-injected agent would most
	// like to do, and it hot-reloads into live threads (reloadSkillsEverywhere)
	// without anyone restarting anything. create is gated alongside the two the
	// audit named because it reaches the same directory by another door.
	d.srv.Handle("skills.install", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		path, err := d.skills.Install(p.Name, p.Target)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("skill installed", "name", p.Name, "target", p.Target, "path", path)
		reloaded := reloadSkillsEverywhere(d, "installed the "+p.Name+" skill")
		return map[string]any{"path": path, "reloaded": reloaded}, nil
	})

	d.srv.Handle("skills.uninstall", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.skills.Uninstall(p.Name, p.Target); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("skill uninstalled", "name", p.Name, "target", p.Target)
		reloaded := reloadSkillsEverywhere(d, "removed the "+p.Name+" skill")
		return map[string]any{"ok": true, "reloaded": reloaded}, nil
	})

	// skills.read returns the full markdown of a catalog skill for the detail
	// pane. Content is capped inside the catalog so a huge file cannot bloat
	// the reply.
	//
	// SECURITY (audit F34 pass 4): UI-only. It is a FILE-CONTENT read keyed by
	// a caller-chosen name — the same shape as search.code, which was gated a
	// round earlier for the same reason. An agent that is meant to have a skill
	// already has it on disk in its own session; this endpoint is the human's
	// skill browser (ui/src/SkillsDialog.cpp), and skills.listCatalog stays
	// open for the listing.
	d.srv.Handle("skills.read", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		if err := d.skills.EnsureDir(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		content, err := d.skills.ReadContent(p.Name)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"content": content}, nil
	})

	// skills.create scaffolds a new directory skill (SKILL.md + frontmatter) in
	// the catalog. Invalid or duplicate names are rejected by the catalog.
	d.srv.Handle("skills.create", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		if err := d.skills.EnsureDir(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		var p struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		skill, err := d.skills.Create(p.Name, p.Description)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("skill created", "name", skill.Name, "path", skill.Path)
		return skill, nil
	})

	// --- git status (read-only) -------------------------------------------
	//
	// SECURITY (audit F34 pass 4): the whole git READ family below is UI-only,
	// and this is the block decision the DEFERRED list in
	// handlers_inventory_test.go stood in for. "Read-only" was doing the work
	// of "harmless" here, and the two are not the same thing: git.file,
	// git.diff, git.blame and git.commit.diff return FILE CONTENT, at a
	// caller-chosen path or revision, out of a caller-chosen repo root —
	// precisely the data class F34 gated agent.transcript and search.code for,
	// reachable by a different verb. git.log, git.branches, git.commit.detail
	// and git.snapshot are the metadata around it (who committed what, when,
	// which branch every thread in the arena sits on, which files are dirty).
	// git.prDraft and git.suggestCommitMessage derive text from another
	// thread's branch, and the latter spends the human's money doing it.
	//
	// Nothing agent-facing calls any of them: every caller is a widget
	// (ui/src/git/*, WorktreeDashboard, ProjectTree, AgentDock). An agent that
	// wants its own repo's history runs git in its own shell, inside its own
	// worktree, under its own tool permissions — which is the boundary the
	// human actually set for it. Each handler carries the gate itself rather
	// than a shared wrapper so that a new sibling registered into this block
	// does not inherit an approval by proximity.
	//
	// git.snapshot returns every registered thread's worktree status, drawn
	// from the cache. The UI polls this at ~1 Hz to drive the worktree
	// dashboard.
	d.srv.Handle("git.snapshot", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		// The fs watcher keeps the cache honest, so polling just reads —
		// only stale entries pay the recompute cost.
		snaps := d.gitCache.Snapshots()
		if snaps == nil {
			snaps = []*gitstatus.Snapshot{}
		}
		// Surface each distinct workspace alongside the threads so the log
		// viewer can offer "view main" as a picker entry without needing the
		// user to have started an agent on the workspace.
		seen := make(map[string]bool)
		workspaces := []map[string]any{}
		for _, s := range snaps {
			if s.RepoRoot == "" || seen[s.RepoRoot] {
				continue
			}
			seen[s.RepoRoot] = true
			workspaces = append(workspaces, map[string]any{
				"repoRoot": s.RepoRoot,
				"branch":   gitstatus.WorkspaceHeadBranch(s.RepoRoot),
			})
		}
		return map[string]any{"threads": snaps, "workspaces": workspaces}, nil
	})

	// git.prDraft returns a suggested PR title and body for the thread's
	// branch, drawn from its commit history since the fork point. Pure
	// read; no network. The UI uses it to prefill the PR dialog.
	d.srv.Handle("git.prDraft", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		title, body, err := worktree.PRDraft(rec.Worktree)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"title": title, "body": body}, nil
	})

	// git.openPR pushes the thread's branch and opens a GitHub pull
	// request, accepting an edited title / body (and a draft flag) from
	// the UI. agent.openPR remains as the title-only shortcut.
	//
	// SECURITY (audit F36): UI-only, and so is every mutating git.* handler
	// from here down (openPR, land, discardChanges, removeWorktree,
	// abortMerge, finalizeMerge, openConflictTool, commit). The audit named
	// discardChanges and removeWorktree; the rest are the same authority
	// through a different door — this one is the full-form twin of the gated
	// agent.openPR, and gating one while the other stayed open would be a
	// boundary in name only. The read-only git.* queries below (diff, log,
	// blame, file, branches, commit detail) are deliberately NOT gated.
	d.srv.Handle("git.openPR", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			Draft    bool   `json:"draft"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		url, err := worktree.OpenPRWithOptions(rec.Worktree, worktree.PROptions{
			Title: p.Title,
			Body:  p.Body,
			Draft: p.Draft,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"url": url}, nil
	})

	// git.land merges the thread's branch into the workspace's current
	// branch. Always takes keepConflicts: the UI passes true so conflicts
	// surface as a banner instead of silently rolling back. agent.land
	// remains the always-rollback shortcut for callers that want that.
	d.srv.Handle("git.land", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID      string `json:"threadId"`
			KeepConflicts bool   `json:"keepConflicts"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		res, err := worktree.LandWithOptions(rec.Worktree, p.KeepConflicts)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// Whether clean or conflicting, both worktrees' state moved.
		d.gitCache.InvalidateAll()
		if len(res.Conflicts) == 0 {
			d.log.Info("git.land complete", "thread", p.ThreadID,
				"branch", res.Branch, "into", res.Into)
		} else {
			d.log.Info("git.land left conflicts", "thread", p.ThreadID,
				"branch", res.Branch, "into", res.Into,
				"conflicts", len(res.Conflicts))
		}
		return res, nil
	})

	// git.discardChanges throws away every uncommitted change in a thread's
	// worktree (git reset --hard HEAD + git clean -fd). DESTRUCTIVE: the UI
	// gates this behind an explicit confirmation. Guard: refuse while the
	// agent is live, so we never yank files out from under a running session.
	d.srv.Handle("git.discardChanges", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if d.agentRunning(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"cannot discard changes while the agent is running — stop it first")
		}
		if err := worktree.DiscardChanges(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.Invalidate(p.ThreadID)
		d.log.Info("git.discardChanges complete", "thread", p.ThreadID, "path", wt.Path)
		return map[string]any{"ok": true}, nil
	})

	// git.removeWorktree deletes an isolated agent worktree and its branch.
	// DESTRUCTIVE: the UI confirms first. Guards: only isolated worktrees can
	// be removed (never the shared workspace), and never while the agent is
	// live.
	d.srv.Handle("git.removeWorktree", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if !wt.Isolated {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this thread runs directly in the workspace and has no worktree to remove")
		}
		if d.agentRunning(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"cannot remove the worktree while the agent is running — stop it first")
		}
		// Resolved BEFORE the record is dropped below — harnessFor needs it.
		h := d.harnessFor(p.ThreadID)
		// Defensive stop — "running" was refused above, but a process can start
		// between that check and the removal (the same race
		// cleanup.archiveAndRemove guards against), and Remove deletes its cwd
		// (audit F54).
		_ = d.agentStop(p.ThreadID)
		if !waitAgentExit(d, p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInternalError,
				"the agent process did not exit; nothing was removed — try again")
		}
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The worktree is gone; forget its session + cache entry just like
		// agent.discard does — the core-owned transcript included (audit F10) —
		// and tell every UI client to refresh.
		deleteThreadTranscript(h, p.ThreadID, d.log)
		_ = d.sessions.Remove(p.ThreadID)
		_ = d.summaries.Remove(p.ThreadID)
		if d.attachSide != nil {
			_ = d.attachSide.Remove(p.ThreadID)
		}
		if d.questionSide != nil {
			_ = d.questionSide.Remove(p.ThreadID)
		}
		d.gitCache.Forget(p.ThreadID)
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		d.srv.Notify("git.invalidated", map[string]any{"threadIds": []string{p.ThreadID}})
		d.log.Info("git.removeWorktree complete", "thread", p.ThreadID, "path", wt.Path)
		return map[string]any{"ok": true}, nil
	})

	// git.abortMerge rolls back an in-progress merge in the thread's
	// workspace, restoring it to pre-merge state.
	d.srv.Handle("git.abortMerge", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.AbortMerge(rec.Worktree.RepoRoot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.InvalidateAll()
		return map[string]any{"ok": true}, nil
	})

	// git.finalizeMerge commits the in-progress merge in the thread's
	// workspace using git's default merge message. Fails if any conflict
	// markers are still present.
	d.srv.Handle("git.finalizeMerge", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.FinalizeMerge(rec.Worktree.RepoRoot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.InvalidateAll()
		return map[string]any{"ok": true}, nil
	})

	// git.openConflictTool spawns KDiff3 (via git mergetool) for every
	// unmerged path in the thread's workspace, detached so akcore doesn't
	// wait. The fs watcher catches each save and keeps the dashboard fresh.
	d.srv.Handle("git.openConflictTool", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.OpenConflictTool(rec.Worktree.RepoRoot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// git.workspaceMergeStatus tells the UI whether the workspace is mid-
	// merge and which paths are still unresolved. Polled by the conflict
	// banner so it can dismiss itself once the human finishes in KDiff3.
	d.srv.Handle("git.workspaceMergeStatus", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		return worktree.WorkspaceMergeStatus(rec.Worktree.RepoRoot), nil
	})

	// git.commit stages a subset of paths (or everything when paths is
	// empty) and commits them to the thread's branch. agent.commit is kept
	// as the "commit everything with this message" shortcut.
	d.srv.Handle("git.commit", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string   `json:"threadId"`
			Message  string   `json:"message"`
			Paths    []string `json:"paths"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.CommitPaths(wt, p.Message, p.Paths); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.Invalidate(p.ThreadID)
		return map[string]any{"ok": true, "branch": wt.Branch}, nil
	})

	// git.suggestCommitMessage asks Sonnet to draft a commit message for the
	// worktree's current diff. Used by the Commit dialog's "Suggest" button.
	// Long-running (one Claude turn); the IPC dispatcher already runs each
	// handler on its own goroutine so this does not block the bus.
	d.srv.Handle("git.suggestCommitMessage", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		msg, err := gitstatus.SuggestCommitMessage(ctx, wt, "", p.Model)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"message": msg}, nil
	})

	// git.diff returns a unified patch of the worktree vs HEAD, scoped to a
	// single path when one is given. Untracked files are folded in as full
	// new-file diffs, and the index is never touched.
	d.srv.Handle("git.diff", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		patch, err := gitstatus.UnifiedDiff(wt, p.Path)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{
			"patch":  patch,
			"branch": wt.Branch,
		}, nil
	})

	// git.blame returns one BlameLine per source line of an absolute file
	// path. The path is resolved against registered worktrees the same way
	// git.file does it, so an open editor file maps to its owning agent's
	// branch without the UI having to know the thread id.
	d.srv.Handle("git.blame", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Path     string `json:"path"`
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Path == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "path is required")
		}
		// Resolve owning worktree + thread + relative path, mirroring git.file.
		var wt worktree.Worktree
		var threadID, relPath string
		if p.ThreadID != "" {
			if rec, ok := d.threads.get(p.ThreadID); ok {
				if rel, err := filepath.Rel(rec.Path, p.Path); err == nil &&
					!strings.HasPrefix(rel, "..") {
					wt = rec
					threadID = p.ThreadID
					relPath = filepath.ToSlash(rel)
				}
			}
		}
		if wt.Path == "" {
			s, rel, ok := d.gitCache.FindByPath(p.Path)
			if !ok {
				return map[string]any{"lines": []gitstatus.BlameLine{}}, nil
			}
			// FindByPath returns a snapshot; we just need the worktree behind
			// it. Look it up via the thread registry / sessions store.
			if rec, ok := d.threads.get(s.ThreadID); ok {
				wt = rec
			} else if rec, ok := d.sessions.Get(s.ThreadID); ok {
				wt = rec.Worktree
			}
			threadID = s.ThreadID
			relPath = rel
		}
		if wt.Path == "" {
			return map[string]any{"lines": []gitstatus.BlameLine{}}, nil
		}
		// Prefer the cache (keyed on the snapshot generation, busted on HEAD
		// move or save) so a repeated blame for an unchanged file never re-shells
		// `git blame`. Fall back to a direct compute only when the thread is not
		// cache-registered.
		var lines []gitstatus.BlameLine
		if cached, ok, err := d.gitCache.BlameFor(threadID, relPath); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		} else if ok {
			lines = cached
		} else {
			l, err := gitstatus.Blame(wt, relPath)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			lines = l
		}
		if lines == nil {
			lines = []gitstatus.BlameLine{}
		}
		return map[string]any{"lines": lines}, nil
	})

	// git.file returns line-level hunks for one absolute file path vs the
	// owning worktree's HEAD. The UI's gutter polls this per open buffer.
	d.srv.Handle("git.file", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Path     string `json:"path"`
			ThreadID string `json:"threadId"` // optional hint
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Path == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "path is required")
		}
		// Locate the owning worktree. The thread hint short-cuts the search;
		// otherwise FindByPath picks the most specific worktree containing
		// the file (so files in .agentkate/worktrees/<id>/ map to that thread,
		// not the parent workspace).
		var snap *gitstatus.Snapshot
		var relPath string
		if p.ThreadID != "" {
			if s, ok := d.gitCache.SnapshotFor(p.ThreadID); ok {
				if rec, ok := d.threads.get(p.ThreadID); ok {
					if rel, err := filepath.Rel(rec.Path, p.Path); err == nil &&
						!strings.HasPrefix(rel, "..") {
						snap = s
						relPath = filepath.ToSlash(rel)
					}
				}
			}
		}
		if snap == nil {
			s, rel, ok := d.gitCache.FindByPath(p.Path)
			if !ok {
				return map[string]any{"status": gitstatus.StatusClean,
					"hunks": []gitstatus.Hunk{}}, nil
			}
			snap, relPath = s, rel
		}
		fileStatus := gitstatus.StatusClean
		for _, f := range snap.Files {
			if f.Path == relPath {
				fileStatus = f.Status
				break
			}
		}
		hunks, generation, _, err := d.gitCache.HunksFor(snap.ThreadID, relPath)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if hunks == nil {
			hunks = []gitstatus.Hunk{}
		}
		return map[string]any{
			"threadId":   snap.ThreadID,
			"branch":     snap.Branch,
			"status":     fileStatus,
			"hunks":      hunks,
			"headSha":    snap.HeadSHA,
			"generation": generation,
		}, nil
	})

	// git.log returns one page of commits for the thread's worktree, with
	// lane/edge metadata so the UI can render a graph rail. Skip drives
	// pagination; Path narrows the view to a file's history (and disables the
	// graph, since the parent edges between filtered commits would be
	// synthetic).
	d.srv.Handle("git.log", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
			Skip     int    `json:"skip"`
			Limit    int    `json:"limit"`
			Path     string `json:"path"`
			Branch   string `json:"branch"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		var wt worktree.Worktree
		if p.ThreadID != "" {
			w, ok := d.threads.get(p.ThreadID)
			if !ok {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
			}
			wt = w
		} else if p.RepoRoot != "" {
			// Workspace-level log: synthesize a Worktree pointing at the
			// repo root so gitstatus.Log can resolve the requested branch.
			wt = worktree.Worktree{Path: p.RepoRoot, RepoRoot: p.RepoRoot}
		} else {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "git.log requires threadId or repoRoot")
		}
		opts := gitstatus.LogOptions{
			Skip:   p.Skip,
			Limit:  p.Limit,
			Path:   p.Path,
			Branch: p.Branch,
		}
		// The unfiltered HEAD graph for a registered thread goes through the
		// cache, which keeps one history walk per (thread, HEAD) so deep scroll
		// pages slice a precomputed array instead of re-walking git each time.
		// Path / branch-scoped views and the workspace (repoRoot) view fall
		// through to the bare walk — identical results either way.
		var entries []gitstatus.LogEntry
		if cached, ok, err := d.gitCache.LogPageFor(p.ThreadID, opts); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		} else if ok {
			entries = cached
		} else {
			e, err := gitstatus.Log(wt, opts)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			entries = e
		}
		if entries == nil {
			entries = []gitstatus.LogEntry{}
		}
		return map[string]any{"entries": entries}, nil
	})

	// git.branches lists the repo's local + remote-tracking branches so the log
	// viewer's branch selector can scope the history to any of them. Read-only:
	// it never checks anything out. Resolves the source (thread worktree or
	// workspace repo root) exactly like git.log.
	d.srv.Handle("git.branches", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, err := resolveLogSource(d, p.ThreadID, p.RepoRoot)
		if err != nil {
			return nil, err
		}
		branches, err := gitstatus.Branches(wt)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if branches == nil {
			branches = []gitstatus.BranchRef{}
		}
		return map[string]any{"branches": branches}, nil
	})

	// git.commit.detail returns one commit's metadata + per-file change list
	// for the right-hand pane of the log viewer.
	d.srv.Handle("git.commit.detail", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
			SHA      string `json:"sha"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, err := resolveLogSource(d, p.ThreadID, p.RepoRoot)
		if err != nil {
			return nil, err
		}
		detail, err := gitstatus.CommitDetailFn(wt, p.SHA)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return detail, nil
	})

	// git.commit.diff returns the unified diff for one commit, optionally
	// scoped to a single file.
	d.srv.Handle("git.commit.diff", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
			SHA      string `json:"sha"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, err := resolveLogSource(d, p.ThreadID, p.RepoRoot)
		if err != nil {
			return nil, err
		}
		patch, err := gitstatus.CommitDiff(wt, p.SHA, p.Path)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"patch": patch}, nil
	})

	// --- worktree cleanup -----------------------------------------------------
	// SAFETY-CRITICAL. cleanup.analyze is a pure read that classifies every
	// worktree in a project as safe / review / blocked / orphaned. The verdict
	// is advisory to the UI; the server RE-DERIVES it in cleanup.archiveAndRemove
	// before anything destructive happens, so a stale client can never bypass
	// the gate.
	//
	// SECURITY (audit F36 pass 4): UI-only, like the two verbs it feeds
	// (cleanup.archiveAndRemove, cleanup.restore). It enumerates every worktree
	// in a caller-chosen project with its path, branch, dirty state and
	// provenance verdict — the map of what is destroyable and what is not —
	// and with `advise` set it also spends an API turn per call. Its only
	// caller is ui/src/CleanupDialog.cpp.
	d.srv.Handle("cleanup.analyze", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Project string `json:"project"`
			Advise  bool   `json:"advise"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}

		cands := analyzeCleanupCandidates(d, p.Project)

		// Phase 2: optional Sonnet advisory. ADVISORY ONLY — AdviseCleanup
		// never changes State/Removable; on any LLM error it returns the
		// candidates unchanged.
		if p.Advise {
			cands = gitstatus.AdviseCleanup(ctx, "", "", cands)
		}
		if cands == nil {
			cands = []gitstatus.CleanupCandidate{}
		}
		return map[string]any{"candidates": cands}, nil
	})

	// cleanup.archiveAndRemove is THE destructive path. It NEVER trusts the
	// client: it re-resolves the worktree, re-runs AnalyzeCandidate, refuses on
	// any blocker, and refuses on warnings unless confirmDestroy is set. The
	// record is archived (reversibly, transcript left on disk) BEFORE git is
	// touched, so a failed Remove still preserves the record.
	//
	// SECURITY (audit F36): and it is UI-only. The server-side re-analysis
	// above says WHETHER a removal is safe; it never said WHO asked. Only the
	// human at the Cleanup dialog does.
	d.srv.Handle("cleanup.archiveAndRemove", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID       string `json:"threadId"`
			ConfirmDestroy bool   `json:"confirmDestroy"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
		}

		// 1. Resolve the worktree. Prefer the live registry; fall back to the
		//    session record (an orphaned thread is not in the registry).
		wt, ok := d.threads.get(p.ThreadID)
		rec, recOK := d.sessions.Get(p.ThreadID)
		if !ok {
			if !recOK {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
			}
			wt = rec.Worktree
		}
		// Resolved here, while the record still exists: step 6 archives it, and
		// after that harnessFor answers with the DEFAULT harness for every id.
		h := d.harnessFor(p.ThreadID)

		// 2. RE-RUN the analysis NOW, server-side. The snapshot may be stale or
		//    absent (orphaned); AnalyzeCandidate handles a nil snapshot.
		snap, _ := d.gitCache.SnapshotFor(p.ThreadID)
		running := d.agentRunning(p.ThreadID)
		title := ""
		if recOK {
			title = rec.Title
		}
		c := gitstatus.AnalyzeCandidate(wt, snap, running, title, time.Time{})

		// 3. Refuse on ANY blocker — never remove running / non-isolated /
		//    detached / broken worktrees.
		if !c.Removable {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"refusing to remove: "+cleanupBlockerReason(c.Blockers))
		}
		// 4. Refuse on warnings unless the client explicitly confirmed the loss.
		if len(c.Warnings) > 0 && !p.ConfirmDestroy {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"unmerged/uncommitted work present; confirmDestroy required")
		}

		// 5. Defensive stop — even though "running" is a blocker, guard against
		//    a process that started between analysis and now — and wait for the
		//    exit before anything destructive: step 7 deletes the process's cwd
		//    (audit F54). Refusing here leaves the record un-archived and the
		//    worktree intact.
		_ = d.agentStop(p.ThreadID)
		if !waitAgentExit(d, p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInternalError,
				"the agent process did not exit; nothing was removed — try again")
		}

		// 6. Archive the record BEFORE touching git, so a failed Remove leaves
		//    the record (and transcript) intact and recoverable.
		if recOK {
			if err := d.sessions.Archive(p.ThreadID, "cleanup: "+string(c.State)); err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, "archive failed: "+err.Error())
			}
		}

		// 7. Remove the worktree (orphaned → Remove falls back to prune + branch -D).
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}

		// 8. Drop the rest of the thread's state and notify, mirroring
		//    agent.discard's teardown. This is a permanent removal (the worktree
		//    is gone), so the attachment sidecar goes too — unlike the reversible
		//    agent.stopClose archive, where chips must survive un-archive. The
		//    core-owned transcript goes with them for the same reason (audit
		//    F10); the CLI's own transcript is deliberately left, which is what
		//    keeps the archived record recoverable.
		deleteThreadTranscript(h, p.ThreadID, d.log)
		_ = d.summaries.Remove(p.ThreadID)
		if d.attachSide != nil {
			_ = d.attachSide.Remove(p.ThreadID)
		}
		if d.questionSide != nil {
			_ = d.questionSide.Remove(p.ThreadID)
		}
		d.gitCache.Forget(p.ThreadID)
		d.threads.remove(p.ThreadID)
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		d.srv.Notify("git.invalidated", map[string]any{"threadId": p.ThreadID})

		d.log.Info("cleanup.archiveAndRemove complete", "thread", p.ThreadID,
			"state", c.State, "confirmDestroy", p.ConfirmDestroy)
		return map[string]any{"ok": true, "archived": recOK}, nil
	})

	// cleanup.listArchived returns archived records newest-first.
	//
	// SECURITY (audit F34 pass 4): UI-only. An ArchiveRecord embeds the whole
	// session.Record, so this is session.listThreads for retired threads —
	// system prompts, env overlays, provider routing and all — under a
	// different verb. Its only caller is ui/src/CleanupDialog.cpp.
	d.srv.Handle("cleanup.listArchived", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		arch := d.sessions.ListArchived()
		out := make([]map[string]any, 0, len(arch))
		for _, record := range arch {
			out = append(out, archiveRecordWire(record))
		}
		return map[string]any{"archived": out}, nil
	})

	// cleanup.restore moves an archived record back as a dormant, non-isolated
	// thread. Its worktree is gone, so it can only resume in the workspace.
	//
	// SECURITY (audit F36 pass 3): UI-only, the twin of the gated
	// cleanup.archiveAndRemove — the roster is the human's, in both directions.
	// Restoring puts a thread the human retired back where agent.resume can
	// reach it, IN THE WORKSPACE rather than the isolated worktree it was
	// archived from, which is a quiet widening of where that thread's next
	// session may write. Its only caller is ui/src/CleanupDialog.cpp.
	d.srv.Handle("cleanup.restore", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.sessions.Restore(p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// --- code search ----------------------------------------------------------
	// search.code runs a filtered ripgrep across a project root. The UI calls
	// it from its global search panel (ui/src/SearchPanel.cpp), and nothing
	// else does.
	//
	// SECURITY (audit F34 pass 3): UI-only, because this is the SAME data class
	// F34 gated on agent.transcript reached by a different verb. `root` is
	// caller-chosen and unconstrained, and a match returns the matching LINES —
	// so an agent bridge could ask for the human's ~/.ssh, ~/.config or another
	// project's worktree and read file CONTENT out of it, with no thread
	// binding, no approval and no trace in anyone's panel. Gating the read that
	// is named "transcript" while leaving the read that is named "search" open
	// is not a boundary.
	d.srv.Handle("search.code", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Query         string   `json:"query"`
			Root          string   `json:"root"`
			Regex         bool     `json:"regex"`
			CaseSensitive bool     `json:"caseSensitive"`
			WholeWord     bool     `json:"wholeWord"`
			Includes      []string `json:"includes"`
			Excludes      []string `json:"excludes"`
			MaxResults    int      `json:"maxResults"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Root == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "root required")
		}
		res, err := search.Run(ctx, search.Options{
			Query:         p.Query,
			Root:          p.Root,
			Regex:         p.Regex,
			CaseSensitive: p.CaseSensitive,
			WholeWord:     p.WholeWord,
			Includes:      p.Includes,
			Excludes:      p.Excludes,
			MaxResults:    p.MaxResults,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return res, nil
	})
}

// coopCallerIdentity resolves the identity a coop.* write is attributed to
// (audit F63). The cooperation board stays open — any agent may write to it,
// which is the shared-board basis handlers_inventory_test.go checks — but WHO
// wrote is not the payload's to choose: a bridge connection is attributed as
// its bound thread, whatever owner/author/thread it supplied, so a
// prompt-injected agent can neither plant notes authored "human" nor release
// the human's file claims. "human" stays reserved for the UI role, the only
// connection that speaks for the human (F13's binding, applied to
// attribution). A connection that is neither is refused — the same fail-closed
// direction as requireCallerThread.
func coopCallerIdentity(ctx context.Context, supplied string) (string, error) {
	ref := ipc.ConnFromContext(ctx)
	if ref == nil {
		return "", ipc.Errorf(ipc.CodeInvalidParams,
			"coop: caller has no connection identity")
	}
	switch ref.Role() {
	case "ui":
		if supplied == "" {
			return "human", nil
		}
		return supplied, nil
	case "bridge":
		if tid := ref.ThreadID(); tid != "" {
			return tid, nil
		}
		return "", ipc.Errorf(ipc.CodeInvalidParams,
			"coop: bridge connection has no bound thread")
	}
	return "", ipc.Errorf(ipc.CodeInvalidParams,
		"coop: connection has not identified (handshake or bridge.identify first)")
}

// resolveLogSource maps the (threadId, repoRoot) pair the log-viewer RPCs
// accept to a Worktree go-git can read. Workspace-branch sources synthesize a
// Worktree pointing at the repo root since the workspace itself is a working
// repo.
func resolveLogSource(d handlerDeps, threadID, repoRoot string) (worktree.Worktree, error) {
	if threadID != "" {
		w, ok := d.threads.get(threadID)
		if !ok {
			return worktree.Worktree{}, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+threadID)
		}
		return w, nil
	}
	if repoRoot != "" {
		return worktree.Worktree{Path: repoRoot, RepoRoot: repoRoot}, nil
	}
	return worktree.Worktree{}, ipc.Errorf(ipc.CodeInvalidParams, "requires threadId or repoRoot")
}

// analyzeCleanupCandidates classifies every worktree in a project as a removal
// candidate. It analyses each live snapshot (filtered to the project's repo
// root) plus synthesises orphaned candidates from session records whose
// worktree directory no longer exists on disk and which the cache no longer
// tracks. Pure read.
func analyzeCleanupCandidates(d handlerDeps, project string) []gitstatus.CleanupCandidate {
	cands := make([]gitstatus.CleanupCandidate, 0)
	seen := make(map[string]bool)

	for _, snap := range d.gitCache.Snapshots() {
		if project != "" && snap.RepoRoot != project {
			continue
		}
		seen[snap.ThreadID] = true
		wt, ok := d.threads.get(snap.ThreadID)
		if !ok {
			// Reconstruct a worktree from the snapshot when the registry has no
			// entry (e.g. a record re-registered on restart).
			wt = worktree.Worktree{
				ThreadID: snap.ThreadID,
				RepoRoot: snap.RepoRoot,
				Path:     snap.Path,
				Branch:   snap.Branch,
				Base:     snap.Base,
				Isolated: snap.Isolated,
				Number:   snap.Number,
			}
		}
		running := d.agentRunning(snap.ThreadID)
		title, last := "", time.Time{}
		if rec, ok := d.sessions.Get(snap.ThreadID); ok {
			title, last = rec.Title, rec.Updated
		}
		cands = append(cands, gitstatus.AnalyzeCandidate(wt, snap, running, title, last))
	}

	// Session records the live cache no longer tracks. Two kinds surface here:
	// orphaned isolated worktrees (dir gone — removal prunes git bookkeeping),
	// and dormant direct-workspace agents (no live snapshot this session).
	// AnalyzeCandidate classifies each: the former orphaned, the latter
	// record-only. Both are removable; the direct ones only archive the session.
	for _, rec := range d.sessions.List(project) {
		if seen[rec.ThreadID] {
			continue
		}
		running := d.agentRunning(rec.ThreadID)
		cands = append(cands,
			gitstatus.AnalyzeCandidate(rec.Worktree, nil, running, rec.Title, rec.Updated))
	}
	return cands
}

// cleanupBlockerReason turns the first blocker code into a human-readable
// refusal message for the destructive handler.
func cleanupBlockerReason(blockers []string) string {
	if len(blockers) == 0 {
		return "worktree is not removable"
	}
	switch blockers[0] {
	case gitstatus.BlockerRunning:
		return "the agent is still running — stop it first"
	case gitstatus.BlockerNotIsolated:
		return "this is the main workspace, not an isolated worktree"
	case gitstatus.BlockerDetached:
		return "the worktree is detached or has no branch"
	case gitstatus.BlockerSnapshot:
		return "could not read the worktree's git state"
	default:
		return "worktree is not removable (" + blockers[0] + ")"
	}
}

// normalizeTags cleans a raw tag slice for persistence: trims whitespace,
// drops empties, dedupes case-insensitively (keeping the first-seen casing),
// caps each tag at 32 characters and the whole set at 12 tags. Order of the
// surviving tags is preserved. Always returns a non-nil slice when there is at
// least one valid tag; an all-empty input yields nil so omitempty stays clean.
func normalizeTags(in []string) []string {
	const maxLen = 32
	const maxTags = 12
	var out []string
	seen := make(map[string]bool)
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len([]rune(t)) > maxLen {
			t = string([]rune(t)[:maxLen])
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// permissionTimeout bounds how long a permission request may wait for the
// human. Advertised in the permission.requested notification so the UI's
// countdown always matches the broker's actual deadline.
const permissionTimeout = 8 * time.Minute

// askHumanPermission opens a broker request, pushes the permission.requested
// notification to the UI, and blocks until the human answers via
// permission.respond or the 8-minute safety timeout fires. It is shared by
// the Claude MCP bridge's permission.request RPC and the kimi ACP permission
// bridge, so both backends present the identical UI approval flow.
// NotifyUI, not Notify (audit F6): the notification carries the request id AND
// THE RAW TOOL INPUT — bash command lines, file contents, whatever the model
// passed — so broadcasting it handed every other agent's bridge a live feed of
// one agent's tool arguments plus the id needed to answer the prompt. The
// human's question goes to the human's client and nowhere else.
//
// NOBODY TO ASK (audit F35 pass 3). The old version pushed the notification and
// then parked on the 8-minute timer regardless of whether any window received
// it. With no UI connected that is eight minutes of a bridge connection held
// open on a question nothing will ever display, ending in the same refusal it
// could have given immediately — a hang where a one-second "no" is both correct
// and far more honest. NotifyUI's delivery count is the authority here, not a
// separate HasUI() check: taken from the same snapshot as the send, it cannot
// race a UI that disconnects in between. Still FAIL CLOSED — no window means no
// approval — only promptly.
func askHumanPermission(srv *ipc.Server, broker *permission.Broker, threadID, toolName string, input json.RawMessage, questionStores ...*session.QuestionStore) (permission.Decision, bool) {
	return askHumanPermissionForSurfaces(srv, nil, broker, threadID, toolName, input, questionStores...)
}

// askHumanPermissionForSurfaces keeps raw input on the desktop-only IPC
// notification while allowing a running paired-device surface to count as a
// real human who can answer the broker's redacted prompt. A paired device is
// never promoted to a UI connection: it only receives the broker DTO via its
// own HTTPS/SSE sink.
func askHumanPermissionForSurfaces(srv *ipc.Server, remoteAccess *remoteControl, broker *permission.Broker, threadID, toolName string, input json.RawMessage, questionStores ...*session.QuestionStore) (permission.Decision, bool) {
	var questions *session.QuestionStore
	if len(questionStores) != 0 {
		questions = questionStores[0]
	}
	// Persist every terminal outcome of an actual question.  "answered" is
	// deliberately strict: a permission allow without a usable question answer
	// remains a dismissal, never a guessed first option.
	recordQuestion := func(dec permission.Decision) {
		if questions == nil || toolName != "AskUserQuestion" {
			return
		}
		answered := dec.Allow && len(dec.UpdatedInput) != 0 && json.Valid(dec.UpdatedInput)
		if err := questions.Append(threadID, session.QuestionTurn{
			Input: input, Answer: dec.UpdatedInput, Answered: answered,
		}); err != nil {
			// The interactive answer already happened on the engine wire.  Do
			// not turn a history-write failure into a different decision, and
			// never invent an approval to recover it.
			slog.Warn("could not persist question history", "thread", threadID, "err", err)
		}
	}
	req, ch := broker.OpenWithDetail(threadID, toolName, permission.Summary(toolName),
		permission.RenderableDetail(toolName, input), permissionTimeout)
	desktopDelivered := srv.NotifyUI("permission.requested", map[string]any{
		"threadId":  threadID,
		"requestId": req.ID,
		"toolName":  toolName,
		"input":     input,
		// Summary and deadline are safe to mirror to a remote human surface;
		// input remains NotifyUI-only and never enters the broker.
		"summary":        req.Summary,
		"deadline":       req.Deadline.UTC().Format(time.RFC3339),
		"timeoutSeconds": int(permissionTimeout / time.Second),
	})
	if desktopDelivered == 0 && (remoteAccess == nil || !remoteAccess.canAnswerPermissions()) {
		broker.Close(req.ID, permission.NoHuman)
		recordQuestion(permission.Decision{})
		return permission.Decision{}, false
	}
	select {
	case dec := <-ch:
		recordQuestion(dec)
		return dec, true
	case <-time.After(permissionTimeout):
		broker.Close(req.ID, permission.TimedOut)
		recordQuestion(permission.Decision{})
		return permission.Decision{}, false
	}
}

// skillReloadFanout bounds how many threads are asked to reload at once. The
// request is a single line on an already-open stdin, so the ceiling is only
// there to keep a large fleet from spawning one goroutine per thread at once.
const skillReloadFanout = 16

// reloadSkillsEverywhere tells every RUNNING thread whose harness can do it to
// re-read its skill directories, and drops a note in each affected panel. It
// returns the thread ids that took the reload.
//
// The broadcast is deliberately unscoped by directory. Skills resolve from
// several roots at once (the thread's worktree AND the user-level catalogue),
// so a containment test against the install target would silently miss every
// thread that a user-level install actually affects. The request is cheap and
// idempotent — a thread with nothing new to find re-reads and moves on — so
// over-sending is strictly safer than under-sending here.
//
// A running thread whose harness CANNOT reload (its descriptor lacks SkillReload) is
// false — kimi, whose CLI resolves skills once at session/new) gets a notice
// too (audit F50). It used to get nothing at all: it was dropped while the
// target list was built, and the notice loop only ran over threads whose reload
// succeeded. The human installed a skill, saw it confirmed, and had no way to
// learn that the agent they were about to give the work to would never see it.
// A silent capability gap is worse than an unsupported one, because the human
// makes a decision on it.
func reloadSkillsEverywhere(d handlerDeps, reason string) []string {
	type target struct {
		threadID string
		reloader skillReloader
	}
	var targets []target
	var skipped []string
	for _, rec := range d.sessions.List("") {
		if !d.agentRunning(rec.ThreadID) {
			continue
		}
		// BOTH conditions, and the capability is the one that speaks for the
		// engine: the interface assertion is how the reload is CALLED, the
		// capability is what the harness DECLARES (and what the UI's traits
		// table mirrors). A harness that grew the method without declaring it
		// would otherwise reload while the UI said it could not, and one that
		// declares it without the method would be dropped without a word —
		// the same silence F50 is about.
		r, hasMethod := d.harnessFor(rec.ThreadID).(skillReloader)
		declared := d.descriptorFor(rec.ThreadID).Supports(harness.OperationSkillReload)
		if hasMethod && declared {
			targets = append(targets, target{rec.ThreadID, r})
			continue
		}
		// A HALF-wired harness is a bug in this tree, not a property of the
		// engine, and it fails closed into the same "restart this agent" notice
		// as an engine that genuinely cannot reload — which is the right answer
		// for the human and a silent one for us. Say it in the log, once per
		// affected thread, so the drift is findable (audit F50 pass 4).
		if hasMethod != declared {
			d.log.Warn("harness disagrees with itself about skill reload",
				"thread", rec.ThreadID, "harness", d.descriptorFor(rec.ThreadID).ID,
				"hasReloadSkillsMethod", hasMethod, "declaresSkillReload", declared)
		}
		skipped = append(skipped, rec.ThreadID)
	}

	// Fan out. Each ReloadSkills is a control request that blocks until the CLI
	// answers or the control channel times out, so serial sends would stall this
	// RPC by that timeout PER wedged thread; concurrent sends bound the whole
	// broadcast by roughly one timeout no matter how many threads there are.
	// The semaphore keeps a large fleet from opening an unbounded burst.
	ok := make([]bool, len(targets))
	sem := make(chan struct{}, skillReloadFanout)
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		sem <- struct{}{}
		safe.Go("skills.reload", func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := tg.reloader.ReloadSkills(tg.threadID); err != nil {
				d.log.Debug("skill reload failed", "thread", tg.threadID, "err", err)
				return
			}
			ok[i] = true
		})
	}
	wg.Wait()

	// Notices are emitted here, in session order, so a fleet-wide install reads
	// the same way in every panel regardless of which reload answered first.
	var reloaded []string
	for i, tg := range targets {
		if !ok[i] {
			continue
		}
		reloaded = append(reloaded, tg.threadID)
		emitLifecycle(d, tg.threadID, "notice",
			"skills reloaded — you "+reason+" while this agent was running", nil)
	}
	// The engines that cannot reload say so, in their own panel, in the same
	// place the reloaded threads say they did (audit F50). Same wording shape,
	// so a fleet-wide install reads as one event with two outcomes rather than
	// as a list with holes in it.
	for _, threadID := range skipped {
		emitLifecycle(d, threadID, "notice",
			"you "+reason+" while this agent was running, but "+
				d.descriptorFor(threadID).DisplayName+" agents read their skills only "+
				"at start — restart this agent to pick them up", nil)
	}
	if len(reloaded) > 0 || len(skipped) > 0 {
		d.log.Info("skill reload broadcast", "reloaded", len(reloaded),
			"skipped", len(skipped))
	}
	return reloaded
}

// emitLifecycle pushes a synthetic _lifecycle agent event to the UI, and
// mirrors the phase into the turn tracker — the orchestration-layer phases
// (started/resumed and, crucially, launch "error") never cross the supervisor
// relay, and an unobserved launch error would leave agent.wait blocked on a
// turn whose thread never came up.
func emitLifecycle(d handlerDeps, threadID, phase, detail string, wt *worktree.Worktree) {
	if d.turns != nil {
		d.turns.ObserveLifecycle(threadID, phase)
	}
	if phase == "resumed" {
		// A dormant remote send is queued before the persisted session is
		// re-launched. Deliver it only after the resume path has made the thread
		// live; a summary-seeded resume stays busy and drains at its normal turn
		// boundary instead.
		drainHumanSendAfterResume(d, threadID)
	}
	ev := map[string]any{"type": "_lifecycle", "phase": phase, "detail": detail}
	if wt != nil {
		ev["isolated"] = wt.Isolated
		ev["branch"] = wt.Branch
		ev["workdir"] = wt.Path
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// Single lifecycle event, but sent in the same batch shape as the supervisor
	// relay so the UI has exactly one wire contract to parse. NotifyUI for the
	// same reason the relay uses it (audit F6): a thread's events are that
	// thread's business and the human's, never every other agent's.
	d.srv.NotifyUI("agent.event", agentEventParams{
		ThreadID: threadID,
		Events:   []json.RawMessage{b},
	})
}
