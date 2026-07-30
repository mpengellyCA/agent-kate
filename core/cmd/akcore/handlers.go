package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/coop"
	"agentkate/internal/cowork"
	"agentkate/internal/gitstatus"
	"agentkate/internal/ipc"
	"agentkate/internal/harness"
	"agentkate/internal/permission"
	"agentkate/internal/safe"
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

type agentStartParams struct {
	WorkspacePath  string             `json:"workspacePath"`
	Prompt         string             `json:"prompt"`
	PermissionMode string             `json:"permissionMode"`
	Effort         string             `json:"effort"`    // claude --effort level; "" = default
	Model          string             `json:"model"`     // model id; "" = backend default
	Backend        string             `json:"backend"`   // "" / "claude" = Claude Code, "kimi" = Kimi Code
	Isolation      string             `json:"isolation"` // worktree.Mode*; "" = auto
	Attachments    []agent.Attachment `json:"attachments"`
	CoworkEnabled  bool               `json:"coworkEnabled"`      // opt into the KDE Cowork desktop tools
	Provider       *agent.Provider    `json:"provider,omitempty"` // third-party API routing; nil = Claude direct
}

type agentSendParams struct {
	ThreadID    string             `json:"threadId"`
	Text        string             `json:"text"`
	Attachments []agent.Attachment `json:"attachments"`
}

type agentStopParams struct {
	ThreadID string `json:"threadId"`
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
	// sup is the Claude supervisor, reachable directly for the capabilities
	// only it has today (hot compaction via Compact). All per-thread routing
	// goes through harnesses.
	sup        *agent.Supervisor
	harnesses  *harness.Registry
	coop       *coop.State
	threads    *threadRegistry
	broker     *permission.Broker
	extensions *vsix.Manager
	sessions   *session.Store
	attachSide *session.AttachmentStore // per-thread attachment metadata sidecars
	summaries  *compact.Store
	skills     *skills.Catalog
	gitCache   *gitstatus.Cache
	cowork     *cowork.Service // nil if KDE/consent init failed; handlers guard
	socketPath string
	exePath    string
	log        *slog.Logger
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

func (d handlerDeps) capsFor(threadID string) harness.Capabilities {
	return d.harnessFor(threadID).Capabilities()
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

// unsupported is THE capability-gate error: every optional feature a harness
// lacks is rejected with this one message shape, so the wording never drifts
// per call site.
func unsupported(feature string, caps harness.Capabilities) error {
	return ipc.Errorf(ipc.CodeInvalidParams,
		feature+" is not supported by "+caps.DisplayName+" agents")
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
		})
	}
	if err := d.attachSide.Append(threadID, session.AttachmentTurn{Text: text, Attachments: metas}); err != nil {
		d.log.Warn("could not record attachment sidecar", "thread", threadID, "err", err)
	}
}

// registerHandlers wires the JSON-RPC methods the core serves.
func registerHandlers(d handlerDeps) {
	d.srv.Handle("handshake", func(ctx context.Context, _ json.RawMessage) (any, error) {
		// Tag this connection as the UI (the first UI becomes the primary that runs
		// Cowork portal sessions) — the Cowork keystone (08 §C).
		d.srv.MarkUI(ctx)
		d.log.Info("handshake received")
		return map[string]any{
			"name":    "akcore",
			"version": version,
			"pid":     os.Getpid(),
		}, nil
	})

	// Cowork (KDE desktop see/control) RPCs. No-op if the service is unavailable.
	registerCoworkHandlers(d)

	// --- agent threads -----------------------------------------------------

	// agent.capabilities lists the registered harnesses with their capability
	// sets, in engine-picker order. The UI fetches this once per connection
	// and derives every backend-specific affordance from it — no harness
	// knowledge is hardcoded client-side.
	d.srv.Handle("agent.capabilities", func(_ context.Context, _ json.RawMessage) (any, error) {
		list := make([]harness.Capabilities, 0, len(d.harnesses.All()))
		for _, h := range d.harnesses.All() {
			list = append(list, h.Capabilities())
		}
		return map[string]any{"harnesses": list}, nil
	})

	d.srv.Handle("agent.start", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStartParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.WorkspacePath == "" || p.Prompt == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "workspacePath and prompt are required")
		}
		h, ok := d.harnesses.Get(p.Backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+p.Backend)
		}
		caps := h.Capabilities()
		if p.Provider.Routed() && !caps.ProviderRouting {
			return nil, unsupported("Provider routing", caps)
		}
		if p.CoworkEnabled && !caps.Cowork {
			return nil, unsupported("Cowork", caps)
		}
		// Start asynchronously so this reply — which carries the threadId —
		// always reaches the UI before any streamed event for the thread. The
		// session id is pre-minted only for harnesses launched onto an id we
		// choose; otherwise the CLI assigns its own during the handshake and
		// the UI captures it from the init event.
		threadID := agent.NewThreadID()
		sessionID := ""
		if caps.MintsSessionID {
			sessionID = session.NewID()
		}
		safe.Go("agent.startThread", func() { startThread(d, h, threadID, sessionID, p) })
		// The opening prompt's attachments are recorded against the new thread id
		// (the record is created asynchronously above, but the sidecar is keyed by
		// thread id and needs no record to exist).
		recordAttachments(d, threadID, p.Prompt, p.Attachments)
		return map[string]any{
			"threadId":  threadID,
			"sessionId": sessionID,
			"backend":   caps.ID,
		}, nil
	})

	// agent.resume re-launches a dormant thread on its persisted Claude Code
	// session, in the same worktree it ran in before.
	d.srv.Handle("agent.resume", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			// Optional: re-supply the provider (with its API token) on resume.
			// Needed when the key lives in KWallet — the Record never persists it.
			Provider *agent.Provider `json:"provider,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
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
				"thread has no "+h.Capabilities().DisplayName+" session to resume")
		}
		safe.Go("agent.resumeThread", func() { resumeThread(d, h, rec, p.Provider) })
		return map[string]any{"threadId": rec.ThreadID, "sessionId": rec.SessionID}, nil
	})

	// agent.transcript returns the Claude Code session transcript for a
	// thread, as the raw JSONL events. The UI replays these to rebuild the
	// conversation feed when reopening a dormant thread.
	d.srv.Handle("agent.transcript", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		return map[string]any{"events": events, "attachments": attachTurns}, nil
	})

	// agent.fork branches a thread's conversation into a new thread so it can
	// continue on a different model or effort while keeping the full context.
	// The source thread is left running and untouched: the fork gets its own
	// isolated worktree (branched from the source worktree's HEAD; uncommitted
	// changes are not copied) and its own Claude Code session via --fork-session.
	d.srv.Handle("agent.fork", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		if !h.Capabilities().Fork {
			return nil, unsupported("Forking", h.Capabilities())
		}
		if src.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this agent has no conversation yet to fork")
		}
		newThreadID := agent.NewThreadID()
		safe.Go("agent.forkThread", func() {
			forkAgentThread(d, h, src, newThreadID, p.Model, p.Effort, p.Title)
		})
		return map[string]any{"threadId": newThreadID}, nil
	})

	// agent.promote upgrades a non-isolated thread into a dedicated git
	// worktree, carrying its working-tree changes and Claude Code session over.
	d.srv.Handle("agent.promote", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		if !h.Capabilities().Promote {
			return nil, unsupported("Promoting", h.Capabilities())
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
	d.srv.Handle("session.listThreads", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p) // project is optional
		records := d.sessions.List(p.Project)
		if records == nil {
			records = []session.Record{}
		}
		return map[string]any{"threads": records}, nil
	})

	// session.browse lists every Claude Code session transcript on disk, so the
	// user can attach any past conversation — even ones Agent Kate did not start.
	d.srv.Handle("session.browse", func(_ context.Context, _ json.RawMessage) (any, error) {
		found, err := session.Discover()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		known := map[string]bool{}
		for _, r := range d.sessions.List("") {
			known[r.SessionID] = true
		}
		for i := range found {
			found[i].Attached = known[found[i].SessionID]
		}
		if len(found) > 500 {
			found = found[:500] // Discover sorts newest-first
		}
		if found == nil {
			found = []session.Discovered{}
		}
		return map[string]any{"sessions": found}, nil
	})

	// session.attach turns a discovered Claude Code session into a dormant
	// Agent Kate thread, which the UI then resumes like any other.
	d.srv.Handle("session.attach", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"sessionId"`
			Project   string `json:"project"`
			Title     string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" || p.Project == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"sessionId and project are required")
		}
		// Never attach the same Claude Code session twice.
		if rec, ok := d.sessions.GetBySession(p.SessionID); ok {
			return map[string]any{"threadId": rec.ThreadID, "alreadyAttached": true}, nil
		}
		threadID := agent.NewThreadID()
		rec := session.Record{
			ThreadID:  threadID,
			SessionID: p.SessionID,
			Project:   p.Project,
			Worktree: worktree.Worktree{
				ThreadID: threadID,
				RepoRoot: p.Project,
				Path:     p.Project,
				Isolated: false, // resume in the conversation's own directory
			},
			PermissionMode: "acceptEdits",
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
	d.srv.Handle("session.preview", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("session.forget", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("agent.send", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentSendParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.agentSend(p.ThreadID, p.Text, p.Attachments); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Persist a compact attachment sidecar so the "You" card's chips survive
		// a resume; the transcript keeps only inlined content, not the origin.
		recordAttachments(d, p.ThreadID, p.Text, p.Attachments)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("agent.stop", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Hot-Opus runs against the live session so it has to happen BEFORE
		// the supervisor terminates the process; cold strategies fire from
		// the exit lifecycle handler instead.
		runHotCompactIfConfigured(d, p.ThreadID)
		if err := d.agentStop(p.ThreadID); err != nil {
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
	d.srv.Handle("agent.stopClose", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Hot-compaction must run against the live session, before termination.
		// It is a no-op for a dormant thread (guards on d.sup.Running internally).
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
		d.threads.remove(p.ThreadID)
		d.log.Info("agent stopped & closed (archived)", "thread", p.ThreadID)
		return map[string]any{"ok": true}, nil
	})

	// agent.setOption changes one session option — model, permissionMode or
	// effort — on a RUNNING thread, mid-session, and persists it to the record
	// so a later resume replays the same choice. Each harness maps the option
	// onto its own mechanism (claude: set_model / set_permission_mode control
	// requests, no mid-session effort; kimi: session/set_config_option). The
	// CLI's own rejection (unknown model id, bad mode) is passed through
	// verbatim so the UI can show it and revert the picker.
	d.srv.Handle("agent.setOption", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Option   string `json:"option"` // "model" | "permissionMode" | "effort"
			Value    string `json:"value"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" || p.Option == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId and option are required")
		}
		if !d.agentRunning(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"the agent is not running — options apply at the next start instead")
		}
		applied, err := d.harnessFor(p.ThreadID).SetOption(p.ThreadID, p.Option, p.Value)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Persist so a resume replays the latest choice, not the start-time one.
		_ = d.sessions.UpdateQuiet(p.ThreadID, func(r *session.Record) {
			switch p.Option {
			case "model":
				r.Model = applied
			case "effort":
				r.Effort = applied
			case "permissionMode":
				r.PermissionMode = applied
			}
		})
		d.log.Info("agent option changed", "thread", p.ThreadID,
			"option", p.Option, "value", applied)
		return map[string]any{"ok": true, "value": applied}, nil
	})

	// agent.interrupt cancels the in-flight turn immediately (no further tokens
	// billed) while keeping the process resident and the session hot: the next
	// agent.send goes down the same stdin with no resume cost. The supervisor
	// emits a `turn_aborted` lifecycle event once the aborted turn's result lands
	// (or, for a hung tool the CLI can't cancel in-band, escalates to signals and
	// the thread goes dormant). No hot-compaction here — interrupt is meant to be
	// instantaneous, and spending a summary turn would defeat the purpose.
	d.srv.Handle("agent.interrupt", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.agentInterrupt(p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("agent.diff", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("agent.commit", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("agent.openPR", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	// agent.land merges a thread's branch into the workspace's main branch — a
	// local integration, separate from agent.openPR which targets GitHub.
	d.srv.Handle("agent.land", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("agent.list", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p)
		recs := d.sessions.List(p.Project)
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			row := map[string]any{
				"threadId": r.ThreadID,
				"project":  r.Project,
				"title":    r.Title,
				"status":   r.Status,
				"backend":  r.Backend,
				"branch":   r.Worktree.Branch,
				"path":     r.Worktree.Path,
				"isolated": r.Worktree.Isolated,
				"number":   r.Worktree.Number,
				"created":  r.Created,
				"updated":  r.Updated,
				"lastTurn": r.LastTurnAt,
				"model":    r.Model,
				"tags":     r.Tags,
			}
			// The record's harness capabilities, resolved (a legacy "" backend
			// reports the default harness), so a consumer never re-derives them.
			if h, ok := d.harnesses.Get(r.Backend); ok {
				row["harness"] = h.Capabilities()
			}
			out = append(out, row)
		}
		return map[string]any{"threads": out}, nil
	})

	// agent.rename persists a user-chosen title for a thread. No worktree or
	// process is touched — only the session record's Title field is updated, so
	// the new name survives restart (session.listThreads reads it back).
	d.srv.Handle("agent.rename", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("agent.setTags", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("agent.addTag", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("agent.removeTag", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("agent.suggestTags", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("agent.discard", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		_ = d.agentStop(p.ThreadID)
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The worktree is gone, so the thread can never be resumed — forget it.
		_ = d.sessions.Remove(p.ThreadID)
		_ = d.summaries.Remove(p.ThreadID)
		if d.attachSide != nil {
			_ = d.attachSide.Remove(p.ThreadID)
		}
		d.gitCache.Forget(p.ThreadID)
		// Tell every UI client to drop this thread from its roster.
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		return map[string]any{"ok": true}, nil
	})

	// --- compaction --------------------------------------------------------
	// Reduces prefix re-cache cost on resume. See package compact.
	d.srv.Handle("agent.setCompactStrategy", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		if caps := d.capsFor(p.ThreadID); !caps.Compaction {
			return nil, unsupported("Compaction", caps)
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
	d.srv.Handle("agent.summaryStatus", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		if caps := d.capsFor(p.ThreadID); !caps.Compaction {
			return nil, unsupported("Compaction", caps)
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
	d.srv.Handle("agent.compactNow", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		if caps := d.capsFor(p.ThreadID); !caps.Compaction {
			return nil, unsupported("Compaction", caps)
		}
		if rec.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "thread has no Claude Code session yet")
		}

		var sum compact.Summary
		token := strings.ToLower(strings.TrimSpace(p.Model))
		if token == "hot" || token == "opus_hot" || token == "hot_opus" {
			// Hot path: send the compact prompt into the live thread and use
			// its assistant reply as the summary. Requires a running thread.
			if !d.sup.Running(p.ThreadID) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"Hot Opus compaction requires a running thread; resume it first")
			}
			hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			text, herr := d.sup.Compact(hctx, p.ThreadID, compact.CompactPrompt)
			if herr != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, herr.Error())
			}
			if strings.TrimSpace(text) == "" {
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
			modelID, strategy, isLocal := resolveCompactModel(p.Model)
			if isLocal {
				events, rerr := session.ReadTranscript(rec.SessionID)
				if rerr != nil {
					return nil, ipc.Errorf(ipc.CodeInternalError, rerr.Error())
				}
				sum = compact.Programmatic(p.ThreadID, rec.SessionID, events)
			} else {
				var err error
				sum, err = compact.RunLLM(ctx, p.ThreadID, strategy, compact.LLMOptions{
					WorkDir:   rec.Worktree.Path,
					SessionID: rec.SessionID,
					Model:     modelID,
					Timeout:   5 * time.Minute,
				})
				if err != nil {
					return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
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
	d.srv.Handle("coop.setOpenFiles", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p coopSetOpenFilesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		d.coop.SetOpenFiles(p.Owner, p.Files)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.listOpenFiles", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"files": d.coop.ListOpenFiles()}, nil
	})

	d.srv.Handle("coop.postNote", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p coopPostNoteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Author == "" {
			p.Author = "human"
		}
		note := d.coop.PostNote(p.Author, p.Text)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return note, nil
	})

	d.srv.Handle("coop.readNotes", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"notes": d.coop.ReadNotes()}, nil
	})

	d.srv.Handle("coop.setPresence", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner       string `json:"owner"`
			FocusedFile string `json:"focusedFile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		d.coop.SetPresence(p.Owner, p.FocusedFile)
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

	d.srv.Handle("coop.claimFile", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		ok, holder := d.coop.ClaimFile(p.Path, p.Owner)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": ok, "holder": holder}, nil
	})

	d.srv.Handle("coop.releaseFile", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		d.coop.ReleaseFile(p.Path, p.Owner)
		d.srv.NotifyPrimaryUI("coop.changed", map[string]any{})
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.requestReview", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Thread  string `json:"thread"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rev := d.coop.AddReview(p.Thread, p.Summary)
		d.srv.Notify("agent.reviewRequested", map[string]any{
			"threadId": p.Thread,
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
	d.srv.Handle("permission.request", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string          `json:"threadId"`
			ToolName string          `json:"toolName"`
			Input    json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		dec, ok := askHumanPermission(d.srv, d.broker, p.ThreadID, p.ToolName, p.Input)
		if !ok {
			return map[string]any{"allow": false}, nil
		}
		res := map[string]any{"allow": dec.Allow}
		if len(dec.UpdatedInput) > 0 {
			res["updatedInput"] = dec.UpdatedInput
		}
		return res, nil
	})

	d.srv.Handle("permission.respond", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		delivered := d.broker.Resolve(p.RequestID,
			permission.Decision{Allow: p.Allow, UpdatedInput: p.UpdatedInput})
		return map[string]any{"ok": delivered}, nil
	})

	// --- VS Code extension reuse -------------------------------------------
	// vsix.install downloads an extension from Open VSX, unpacks it and
	// detects the language server it bundles. It blocks for the duration of
	// the download; the IPC server dispatches each request on its own
	// goroutine, so other traffic is unaffected.
	d.srv.Handle("vsix.install", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("vsix.uninstall", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("vsix.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
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
	d.srv.Handle("vsix.search", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("skills.listInstalled", func(_ context.Context, raw json.RawMessage) (any, error) {
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

	d.srv.Handle("skills.install", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		return map[string]any{"path": path}, nil
	})

	d.srv.Handle("skills.uninstall", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		return map[string]any{"ok": true}, nil
	})

	// skills.read returns the full markdown of a catalog skill for the detail
	// pane. Content is capped inside the catalog so a huge file cannot bloat
	// the reply.
	d.srv.Handle("skills.read", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("skills.create", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	// git.snapshot returns every registered thread's worktree status, drawn
	// from the cache. The UI polls this at ~1 Hz to drive the worktree
	// dashboard.
	d.srv.Handle("git.snapshot", func(_ context.Context, _ json.RawMessage) (any, error) {
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
	d.srv.Handle("git.prDraft", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.openPR", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.land", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.discardChanges", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		if d.sup.Running(p.ThreadID) {
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
	d.srv.Handle("git.removeWorktree", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		if d.sup.Running(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"cannot remove the worktree while the agent is running — stop it first")
		}
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The worktree is gone; forget its session + cache entry just like
		// agent.discard does, and tell every UI client to refresh.
		_ = d.sessions.Remove(p.ThreadID)
		_ = d.summaries.Remove(p.ThreadID)
		if d.attachSide != nil {
			_ = d.attachSide.Remove(p.ThreadID)
		}
		d.gitCache.Forget(p.ThreadID)
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		d.srv.Notify("git.invalidated", map[string]any{"threadIds": []string{p.ThreadID}})
		d.log.Info("git.removeWorktree complete", "thread", p.ThreadID, "path", wt.Path)
		return map[string]any{"ok": true}, nil
	})

	// git.abortMerge rolls back an in-progress merge in the thread's
	// workspace, restoring it to pre-merge state.
	d.srv.Handle("git.abortMerge", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.finalizeMerge", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.openConflictTool", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.workspaceMergeStatus", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.commit", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.diff", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.blame", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.file", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.log", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.branches", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.commit.detail", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("git.commit.diff", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("cleanup.analyze", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
	d.srv.Handle("cleanup.archiveAndRemove", func(_ context.Context, raw json.RawMessage) (any, error) {
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

		// 2. RE-RUN the analysis NOW, server-side. The snapshot may be stale or
		//    absent (orphaned); AnalyzeCandidate handles a nil snapshot.
		snap, _ := d.gitCache.SnapshotFor(p.ThreadID)
		running := d.sup.Running(p.ThreadID)
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
		//    a process that started between analysis and now.
		_ = d.sup.Stop(p.ThreadID)

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
		//    agent.stopClose archive, where chips must survive un-archive.
		_ = d.summaries.Remove(p.ThreadID)
		if d.attachSide != nil {
			_ = d.attachSide.Remove(p.ThreadID)
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
	d.srv.Handle("cleanup.listArchived", func(_ context.Context, _ json.RawMessage) (any, error) {
		arch := d.sessions.ListArchived()
		if arch == nil {
			arch = []session.ArchiveRecord{}
		}
		return map[string]any{"archived": arch}, nil
	})

	// cleanup.restore moves an archived record back as a dormant, non-isolated
	// thread. Its worktree is gone, so it can only resume in the workspace.
	d.srv.Handle("cleanup.restore", func(_ context.Context, raw json.RawMessage) (any, error) {
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
	// it from its global search panel.
	d.srv.Handle("search.code", func(_ context.Context, raw json.RawMessage) (any, error) {
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
		res, err := search.Run(search.Options{
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
		running := d.sup.Running(snap.ThreadID)
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
		running := d.sup.Running(rec.ThreadID)
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

// askHumanPermission opens a broker request, pushes the permission.requested
// notification to the UI, and blocks until the human answers via
// permission.respond or the 8-minute safety timeout fires. It is shared by
// the Claude MCP bridge's permission.request RPC and the kimi ACP permission
// bridge, so both backends present the identical UI approval flow.
// permissionTimeout bounds how long a permission request may wait for the
// human. Advertised in the permission.requested notification so the UI's
// countdown always matches the broker's actual deadline.
const permissionTimeout = 8 * time.Minute

func askHumanPermission(srv *ipc.Server, broker *permission.Broker, threadID, toolName string, input json.RawMessage) (permission.Decision, bool) {
	id, ch := broker.Open()
	srv.Notify("permission.requested", map[string]any{
		"threadId":       threadID,
		"requestId":      id,
		"toolName":       toolName,
		"input":          input,
		"timeoutSeconds": int(permissionTimeout / time.Second),
	})
	select {
	case dec := <-ch:
		return dec, true
	case <-time.After(permissionTimeout):
		broker.Close(id)
		return permission.Decision{}, false
	}
}

// emitLifecycle pushes a synthetic _lifecycle agent event to the UI.
func emitLifecycle(srv *ipc.Server, threadID, phase, detail string, wt *worktree.Worktree) {
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
	// relay so the UI has exactly one wire contract to parse.
	srv.Notify("agent.event", agentEventParams{
		ThreadID: threadID,
		Events:   []json.RawMessage{b},
	})
}
