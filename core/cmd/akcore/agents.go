package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/kimi"
	"agentkate/internal/safe"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// startAgentThread creates the thread's worktree, writes its MCP config and
// launches the headless agent. Failures are reported as lifecycle events
// rather than as an error reply, since the agent.start reply has already been
// sent by the time this runs.
func startAgentThread(d handlerDeps, threadID, sessionID string, p agentStartParams) {
	if p.Backend == session.BackendKimi {
		startKimiThread(d, threadID, p)
		return
	}
	wt, err := worktree.Create(p.WorkspacePath, threadID, p.Isolation)
	if err != nil {
		d.log.Error("worktree create failed", "thread", threadID, "err", err)
		emitLifecycle(d.srv, threadID, "error", "worktree: "+err.Error(), nil)
		return
	}
	wt.Number = d.sessions.NextNumber(p.WorkspacePath)
	d.threads.put(threadID, wt)
	d.gitCache.Register(wt)
	d.gitCache.Activate(threadID) // an agent is about to run here — watch it live

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, threadID, wt.Path, p.CoworkEnabled)
	if err != nil {
		emitLifecycle(d.srv, threadID, "error", "mcp config: "+err.Error(), &wt)
		return
	}

	// The UI sends a tier token ("opus"/"sonnet"/"haiku"/"fable"/""); resolve
	// it to a concrete --model id once here and persist that id, so a later
	// resume replays the exact same model.
	model := resolveModel(p.Model)

	if _, err := d.sup.Start(agent.StartOptions{
		ID:             threadID,
		WorkDir:        wt.Path,
		Prompt:         p.Prompt,
		MCPConfig:      mcpConfig,
		PermissionMode: p.PermissionMode,
		Effort:         p.Effort,
		Model:          model,
		Attachments:    p.Attachments,
		SessionID:      sessionID,
		CoworkEnabled:  p.CoworkEnabled,
		Provider:       p.Provider,
	}); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, threadID, "error", err.Error(), &wt)
		return
	}

	// Persist the thread so it survives a stop, a crash or an Agent Kate
	// restart, and can later be resumed on this same Claude Code session.
	permMode := p.PermissionMode
	if permMode == "" {
		permMode = "acceptEdits"
	}
	rec := session.Record{
		ThreadID:       threadID,
		SessionID:      sessionID,
		Project:        p.WorkspacePath,
		Worktree:       wt,
		PermissionMode: permMode,
		Effort:         p.Effort,
		Model:          model,
		Title:          summarizePrompt(p.Prompt),
		Created:        time.Now(),
		Status:         session.StatusRunning,
		CoworkEnabled:  p.CoworkEnabled,
	}
	applyProviderToRecord(&rec, p.Provider)
	if err := d.sessions.Put(rec); err != nil {
		d.log.Warn("could not persist thread record", "thread", threadID, "err", err)
	}

	mode := "directly in the workspace"
	if wt.Isolated {
		mode = "in an isolated worktree on " + wt.Branch
	}
	d.log.Info("agent thread started", "thread", threadID, "isolated", wt.Isolated, "dir", wt.Path)
	emitLifecycle(d.srv, threadID, "started", "running "+mode, &wt)
}

// startKimiThread is the kimi-backend counterpart of startAgentThread: same
// worktree/registry/record lifecycle, but the process is a `kimi acp` child
// driven over ACP. No MCP-config tempfile (the Cooperation bridge is passed
// to session/new as a stdio server) and no model tier resolution (kimi takes
// its own aliases); the session id is kimi-assigned during the handshake.
func startKimiThread(d handlerDeps, threadID string, p agentStartParams) {
	wt, err := worktree.Create(p.WorkspacePath, threadID, p.Isolation)
	if err != nil {
		d.log.Error("worktree create failed", "thread", threadID, "err", err)
		emitLifecycle(d.srv, threadID, "error", "worktree: "+err.Error(), nil)
		return
	}
	wt.Number = d.sessions.NextNumber(p.WorkspacePath)
	d.threads.put(threadID, wt)
	d.gitCache.Register(wt)
	d.gitCache.Activate(threadID) // an agent is about to run here — watch it live

	th, err := d.ksup.Start(kimi.StartOptions{
		ID:          threadID,
		WorkDir:     wt.Path,
		Prompt:      p.Prompt,
		Attachments: p.Attachments,
		Model:       p.Model,
		MCPServers:  []kimi.MCPServer{coopMCPServer(d.exePath, d.socketPath, threadID, wt.Path)},
	})
	if err != nil {
		emitLifecycle(d.srv, threadID, "error", err.Error(), &wt)
		return
	}

	// Kimi assigns its own session id during the ACP handshake; take it from
	// the thread directly. (The run loop's session_id capture can't do it:
	// the supervisor emits the init event synchronously inside Start, before
	// this record exists.)
	rec := session.Record{
		ThreadID:  threadID,
		SessionID: th.SessionID(),
		Project:   p.WorkspacePath,
		Worktree:  wt,
		Backend:   session.BackendKimi,
		Model:     p.Model,
		Title:     summarizePrompt(p.Prompt),
		Created:   time.Now(),
		Status:    session.StatusRunning,
	}
	if err := d.sessions.Put(rec); err != nil {
		d.log.Warn("could not persist thread record", "thread", threadID, "err", err)
	}

	mode := "directly in the workspace"
	if wt.Isolated {
		mode = "in an isolated worktree on " + wt.Branch
	}
	d.log.Info("kimi thread started", "thread", threadID, "isolated", wt.Isolated, "dir", wt.Path)
	emitLifecycle(d.srv, threadID, "started", "running "+mode, &wt)
}

// resumeKimiThread re-launches a dormant kimi thread on its persisted kimi
// session, in the same worktree it ran in before. Compaction does not exist
// for kimi, so resume always re-attaches the original session (ACP
// session/resume) — never a summary-seeded fresh one.
func resumeKimiThread(d handlerDeps, rec session.Record) {
	if _, err := os.Stat(rec.Worktree.Path); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error",
			"worktree no longer exists: "+rec.Worktree.Path, nil)
		return
	}
	d.threads.put(rec.ThreadID, rec.Worktree)
	d.gitCache.Register(rec.Worktree)
	d.gitCache.Activate(rec.ThreadID) // resuming — this thread is active again, watch it

	if _, err := d.ksup.Start(kimi.StartOptions{
		ID:         rec.ThreadID,
		WorkDir:    rec.Worktree.Path,
		SessionID:  rec.SessionID,
		Resume:     true,
		Model:      rec.Model,
		MCPServers: []kimi.MCPServer{coopMCPServer(d.exePath, d.socketPath, rec.ThreadID, rec.Worktree.Path)},
	}); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", err.Error(), &rec.Worktree)
		return
	}

	_ = d.sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) {
		r.Status = session.StatusRunning
	})
	d.log.Info("kimi thread resumed", "thread", rec.ThreadID, "session", rec.SessionID)
	emitLifecycle(d.srv, rec.ThreadID, "resumed", "resumed Kimi Code session", &rec.Worktree)
}

// resumeAgentThread re-launches a dormant thread. If a current compacted
// summary exists, a fresh Claude Code session is started seeded with that
// summary instead of replaying the full transcript — that is where the
// compaction savings actually land. Without a current summary the thread
// resumes on its original session via --resume as before.
func resumeAgentThread(d handlerDeps, rec session.Record, provOverride *agent.Provider) {
	if _, err := os.Stat(rec.Worktree.Path); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error",
			"worktree no longer exists: "+rec.Worktree.Path, nil)
		return
	}
	d.threads.put(rec.ThreadID, rec.Worktree)
	d.gitCache.Register(rec.Worktree)
	d.gitCache.Activate(rec.ThreadID) // resuming — this thread is active again, watch it

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, rec.ThreadID, rec.Worktree.Path, rec.CoworkEnabled)
	if err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", "mcp config: "+err.Error(), &rec.Worktree)
		return
	}

	// Pick a path: seed-from-summary if a current summary is on disk, else
	// classic --resume. A summary is "current" when it has been refreshed
	// after the last user/agent turn.
	sum, _ := d.summaries.Get(rec.ThreadID)
	current := sum != nil &&
		(rec.LastTurnAt.IsZero() || !rec.LastTurnAt.After(rec.SummaryUpdatedAt))

	opts := agent.StartOptions{
		ID:             rec.ThreadID,
		WorkDir:        rec.Worktree.Path,
		MCPConfig:      mcpConfig,
		PermissionMode: rec.PermissionMode,
		Effort:         rec.Effort,
		Model:          rec.Model,
		CoworkEnabled:  rec.CoworkEnabled,
		Provider:       providerFromRecord(rec),
	}
	// A fresh override (the UI re-supplying a KWallet-held token the Record never
	// stores) takes precedence over the env-var resolution baked into the snapshot.
	if provOverride.Routed() {
		opts.Provider = provOverride
	}
	var sessionIDForRecord string
	var detail string
	if current {
		// Fresh session seeded with the summary text. The instruction line
		// at the bottom asks the agent to acknowledge briefly so its first
		// turn is short — minimising the prefix that gets cached.
		newID := session.NewID()
		opts.SessionID = newID
		opts.Resume = false
		opts.Prompt = sum.Body +
			"\n\n---\n\nThe above is prior context from a session that has " +
			"been compacted. Acknowledge in one sentence that you have read " +
			"it, then wait for the user's next instruction."
		sessionIDForRecord = newID
		detail = "resumed from compacted summary (new session)"
	} else {
		opts.SessionID = rec.SessionID
		opts.Resume = true
		sessionIDForRecord = rec.SessionID
		detail = "resumed Claude Code session"
	}

	if _, err := d.sup.Start(opts); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, rec.ThreadID, "error", err.Error(), &rec.Worktree)
		return
	}

	_ = d.sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) {
		r.Status = session.StatusRunning
		if current {
			// The summary is now baked into the new session; clear our
			// staleness signals so the next compact cycle starts fresh.
			r.SessionID = sessionIDForRecord
			r.SummaryUpdatedAt = time.Time{}
			r.LastTurnAt = time.Time{}
		}
	})
	if current {
		// The previous summary belonged to the old session; drop it so a
		// missed exit-compact on the new session does not silently reuse
		// stale content.
		_ = d.summaries.Remove(rec.ThreadID)
	}
	d.log.Info("agent thread resumed",
		"thread", rec.ThreadID, "session", sessionIDForRecord, "seeded", current)
	emitLifecycle(d.srv, rec.ThreadID, "resumed", detail, &rec.Worktree)
}

// forkAgentThread branches a source thread's conversation into a brand-new
// thread that can run on a different model or effort while keeping the full
// context. It creates a fresh isolated worktree from the source worktree's HEAD
// (committed state only — uncommitted changes are NOT copied), then starts the
// new thread with `--resume <sourceSessionID> --fork-session`, which replays the
// source context but mints a new Claude Code session for the fork's own turns.
// The source thread is never touched. The new session id is captured from the
// CLI init event by the run loop (see run.go's lastSessionID wiring), so the
// fork's Record starts with an empty SessionID and is filled in on first event.
func forkAgentThread(d handlerDeps, src session.Record, newThreadID, model, effort, title string) {
	// Branch the fork's worktree from the source worktree's HEAD so it starts
	// from exactly the committed state the conversation was continuing from.
	base, ok := worktree.Head(src.Worktree.Path)
	if !ok {
		emitLifecycle(d.srv, newThreadID, "error",
			"fork: cannot read the source worktree's HEAD (no commits to branch from)", nil)
		return
	}
	wt, err := worktree.CreateFrom(src.Project, newThreadID, base)
	if err != nil {
		d.log.Error("fork worktree create failed", "thread", newThreadID, "err", err)
		emitLifecycle(d.srv, newThreadID, "error", "fork worktree: "+err.Error(), nil)
		return
	}
	wt.Number = d.sessions.NextNumber(src.Project)
	d.threads.put(newThreadID, wt)
	d.gitCache.Register(wt)
	d.gitCache.Activate(newThreadID)

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, newThreadID, wt.Path, src.CoworkEnabled)
	if err != nil {
		emitLifecycle(d.srv, newThreadID, "error", "mcp config: "+err.Error(), &wt)
		return
	}

	// Model/effort default to the source's when the fork didn't override them —
	// a fork that changes only the effort keeps the source model, and vice versa.
	resolvedModel := src.Model
	if strings.TrimSpace(model) != "" {
		resolvedModel = resolveModel(model)
	}
	resolvedEffort := src.Effort
	if strings.TrimSpace(effort) != "" {
		resolvedEffort = effort
	}

	if _, err := d.sup.Start(agent.StartOptions{
		ID:             newThreadID,
		WorkDir:        wt.Path,
		MCPConfig:      mcpConfig,
		PermissionMode: src.PermissionMode,
		Effort:         resolvedEffort,
		Model:          resolvedModel,
		SessionID:      src.SessionID,
		Resume:         true,
		ForkSession:    true,
		CoworkEnabled:  src.CoworkEnabled,
		Provider:       providerFromRecord(src),
	}); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, newThreadID, "error", err.Error(), &wt)
		return
	}

	forkTitle := strings.TrimSpace(title)
	if forkTitle == "" {
		forkTitle = summarizePrompt("Fork of " + src.Title)
	}
	rec := session.Record{
		ThreadID: newThreadID,
		// SessionID is intentionally empty: --fork-session mints a new one that
		// the run loop captures from the init event into this record.
		SessionID:      "",
		Project:        src.Project,
		Worktree:       wt,
		PermissionMode: src.PermissionMode,
		Effort:         resolvedEffort,
		Model:          resolvedModel,
		Title:          forkTitle,
		Created:        time.Now(),
		Status:         session.StatusRunning,
		CoworkEnabled:  src.CoworkEnabled,
		// Carry the source's user-set policy so the fork behaves like its parent:
		// compaction strategy/strip and roster tags. SessionID, worktree, status,
		// created/updated and summary/turn timestamps are intentionally per-fork.
		CompactStrategy: src.CompactStrategy,
		CompactStrip:    src.CompactStrip,
		Tags:            append([]string(nil), src.Tags...),
	}
	applyProviderToRecord(&rec, providerFromRecord(src))
	if err := d.sessions.Put(rec); err != nil {
		d.log.Warn("could not persist fork record", "thread", newThreadID, "err", err)
	}

	d.log.Info("agent thread forked",
		"from", src.ThreadID, "thread", newThreadID, "branch", wt.Branch,
		"model", resolvedModel, "effort", resolvedEffort)
	emitLifecycle(d.srv, newThreadID, "started",
		"forked from #"+strconv.Itoa(src.Worktree.Number)+" on "+wt.Branch, &wt)
}

// promoteAgentThread upgrades a non-isolated thread to an isolated worktree: it
// stops the agent, moves the working tree and Claude Code session into a fresh
// worktree, then resumes the thread there.
func promoteAgentThread(d handlerDeps, rec session.Record) {
	// Stop any live process and wait for it to exit before touching git.
	_ = d.sup.Stop(rec.ThreadID)
	for i := 0; i < 60 && d.sup.Running(rec.ThreadID); i++ {
		time.Sleep(200 * time.Millisecond)
	}

	iso, err := worktree.Promote(rec.Worktree)
	if err != nil && !iso.Isolated {
		// Promote failed before it created the worktree — there is nothing to
		// adopt, so the record stays as-is and we just report the failure.
		emitLifecycle(d.srv, rec.ThreadID, "error", "promote: "+err.Error(), &rec.Worktree)
		return
	}
	// A non-nil err with a valid isolated worktree means the worktree WAS created
	// but re-applying the stash hit a conflict (its markers are on disk for the
	// user to resolve). Adopt the worktree regardless: dropping it here would leak
	// the worktree and branch and leave the record pointing at the stale
	// non-isolated path. We surface the conflict in the lifecycle detail instead.
	promoteWarning := ""
	if err != nil {
		promoteWarning = err.Error()
	}
	// Relocate the Claude Code session so `--resume` finds it in the worktree.
	if err := session.PromoteTranscript(rec.SessionID, rec.ThreadID); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", "promote: "+err.Error(), &iso)
		return
	}

	rec.Worktree = iso
	_ = d.sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) { r.Worktree = iso })
	d.threads.put(rec.ThreadID, iso)
	d.gitCache.Register(iso)
	d.gitCache.Activate(rec.ThreadID) // re-point the watch onto the new isolated worktree
	if promoteWarning != "" {
		d.log.Warn("agent thread promoted with conflict",
			"thread", rec.ThreadID, "branch", iso.Branch, "warn", promoteWarning)
		emitLifecycle(d.srv, rec.ThreadID, "promoted",
			"promoted to an isolated worktree on "+iso.Branch+" — "+promoteWarning, &iso)
	} else {
		d.log.Info("agent thread promoted", "thread", rec.ThreadID, "branch", iso.Branch)
		emitLifecycle(d.srv, rec.ThreadID, "promoted",
			"promoted to an isolated worktree on "+iso.Branch, &iso)
	}

	// Bring the thread back up, now inside its isolated worktree. The provider
	// (if any) is rebuilt from the Record; its token re-resolves from the env var.
	resumeAgentThread(d, rec, nil)
}

// runHotCompactIfConfigured runs a Hot-Opus compaction on the live thread
// (if its strategy calls for one) before the caller terminates the process.
// Synchronous on purpose: the supervisor must wait for the compact to land
// before reap, so the assistant's final turn — the summary itself — is
// captured. No-op when the strategy is something else or the thread is
// already dead.
func runHotCompactIfConfigured(d handlerDeps, threadID string) {
	rec, ok := d.sessions.Get(threadID)
	if !ok {
		return
	}
	if rec.Backend == session.BackendKimi {
		return // kimi threads have no compaction support
	}
	if compact.Strategy(rec.CompactStrategy).Resolve() != compact.ExitOpusHot {
		return
	}
	if !d.sup.Running(threadID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	text, err := d.sup.Compact(ctx, threadID, compact.CompactPrompt)
	if err != nil {
		d.log.Warn("hot-opus compact failed", "thread", threadID, "err", err)
		return
	}
	if strings.TrimSpace(text) == "" {
		d.log.Warn("hot-opus compact returned empty body", "thread", threadID)
		return
	}
	sum := compact.Summary{
		ThreadID:  threadID,
		SessionID: rec.SessionID,
		Strategy:  compact.ExitOpusHot,
		Stripped:  rec.CompactStrip,
		Created:   time.Now().UTC(),
		Body:      text,
	}
	if err := d.summaries.Put(sum); err != nil {
		d.log.Warn("could not store hot summary", "thread", threadID, "err", err)
		return
	}
	_ = d.sessions.UpdateQuiet(threadID, func(r *session.Record) {
		r.SummaryUpdatedAt = sum.Created
	})
	d.log.Info("hot-opus compact complete",
		"thread", threadID, "body_bytes", len(sum.Body))
}

// runHotCompactsAtShutdown fires Hot-Opus in parallel for every running
// thread configured for it, before the supervisor terminates them all.
// Each compact has its own 2-minute timeout, so a stuck thread cannot
// hold up shutdown of the others.
func runHotCompactsAtShutdown(d handlerDeps, progress shutdownProgressFn) {
	var targets []session.Record
	for _, rec := range d.sessions.List("") {
		if rec.Backend == session.BackendKimi {
			continue // kimi threads have no compaction support
		}
		if !d.sup.Running(rec.ThreadID) {
			continue
		}
		if compact.Strategy(rec.CompactStrategy).Resolve() != compact.ExitOpusHot {
			continue
		}
		targets = append(targets, rec)
	}
	total := len(targets)
	if total == 0 {
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for _, rec := range targets {
		wg.Add(1)
		rec := rec
		safe.Go("shutdown.hotCompact", func() {
			defer wg.Done()
			runHotCompactIfConfigured(d, rec.ThreadID)
			// Report completion as each finishes so the shutdown dialog can show
			// "Compacting context for resume (i of N)".
			mu.Lock()
			done++
			progress("compacting", rec.Title, done, total)
			mu.Unlock()
		})
	}
	wg.Wait()
}

// shutdownProgressFn reports graceful-shutdown progress. phase is a stable code
// (preparing|compacting|stopping|draining|watchers|done) the UI maps to text;
// detail is an optional agent title; index/total drive the "(i of N)" counter.
type shutdownProgressFn func(phase, detail string, index, total int)

// exitCompactCap bounds how long shutdown waits for cold-exit compactions to
// finish. A cold `claude --resume` usually lands in a few seconds; the cap is
// a backstop so a hung one cannot wedge quit forever. Kept conservative so a
// dogfood quit never feels stuck — stragglers are cancelled and logged.
//
// IMPORTANT: the UI SIGKILLs the core if it doesn't exit within its own grace
// window after SIGTERM (CoreClient::~CoreClient, waitForFinished). That grace
// MUST stay larger than this cap, or the core is killed mid-drain at app quit
// and the summary goes missing — the very bug this fixes. Keep them in sync.
const exitCompactCap = 15 * time.Second

// exitCompactTracker owns the in-flight cold-exit compactions so the shutdown
// path can drain them before the process exits. ctx is shared by every spawned
// compaction; cancelling it (at the shutdown deadline) kills stragglers.
type exitCompactTracker struct {
	wg        sync.WaitGroup
	ctx       context.Context
	claudeBin string // injectable for tests; "" resolves to "claude" via PATH
}

// spawn launches a cold-exit compaction for rec as a tracked background
// goroutine and returns immediately. The goroutine is registered with the
// WaitGroup before it starts, so a later drain() reliably blocks on it.
func (e *exitCompactTracker) spawn(log *slog.Logger, sessions *session.Store,
	summaries *compact.Store, rec session.Record, strategy compact.Strategy) {
	e.wg.Add(1)
	safe.Go("shutdown.exitCompact", func() {
		defer e.wg.Done()
		runExitCompact(e.ctx, e.claudeBin, log, sessions, summaries, rec, strategy)
	})
}

// drain waits for all spawned cold-exit compactions to finish, bounded by cap.
// On timeout it cancels stragglers (killing any hung claude) and proceeds, so
// a wedged compaction can never block quit indefinitely.
func (e *exitCompactTracker) drain(cancel context.CancelFunc, cap time.Duration, log *slog.Logger) {
	if waitWithDeadline(&e.wg, cap) {
		return
	}
	log.Warn("exit compactions exceeded shutdown cap; cancelling stragglers", "cap", cap)
	cancel()
	// Give cancelled compactions a brief grace period to unwind, but never
	// block quit on one that ignores cancellation.
	if !waitWithDeadline(&e.wg, 2*time.Second) {
		log.Warn("exit compactions still running after cancel; exiting anyway")
	}
}

// waitWithDeadline blocks until wg drains or d elapses. Returns true if the
// WaitGroup drained in time, false on timeout.
func waitWithDeadline(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	safe.Go("shutdown.waitWithDeadline", func() {
		wg.Wait()
		close(done)
	})
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// runExitCompact performs a cold compaction after an agent exits. Errors are
// logged but do not block anything — the next resume will either find a usable
// summary or trigger the recovery dialog in the UI. ctx is the shutdown-scoped
// context: its cancellation (at the shutdown deadline) aborts a straggling
// claude rather than orphaning it. An empty claudeBin resolves to "claude".
func runExitCompact(ctx context.Context, claudeBin string, log *slog.Logger,
	sessions *session.Store, summaries *compact.Store,
	rec session.Record, strategy compact.Strategy) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	sum, err := compact.RunLLM(ctx, rec.ThreadID, strategy, compact.LLMOptions{
		ClaudeBin: claudeBin,
		WorkDir:   rec.Worktree.Path,
		SessionID: rec.SessionID,
		Model:     strategy.Model(),
		Timeout:   5 * time.Minute,
	})
	if err != nil {
		log.Warn("exit compaction failed",
			"thread", rec.ThreadID, "strategy", strategy, "err", err)
		return
	}
	if err := summaries.Put(sum); err != nil {
		log.Warn("could not store summary", "thread", rec.ThreadID, "err", err)
		return
	}
	_ = sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) {
		r.SummaryUpdatedAt = sum.Created
	})
	log.Info("exit compaction complete",
		"thread", rec.ThreadID, "strategy", strategy,
		"turns", sum.Turns, "body_bytes", len(sum.Body))
}

// summarizePrompt makes a short, single-line title from an opening prompt.
func summarizePrompt(prompt string) string {
	s := strings.TrimSpace(prompt)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 70
	if len(s) > max {
		cut := max
		// Back up to a rune boundary so a multi-byte rune is never split.
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// resolveModel maps a UI-facing tier token ("opus", "sonnet", "haiku",
// "fable") to the concrete claude --model id we spawn or compact with. It is
// the single source of truth for the tier→id map, shared by the agent-spawn
// path (startAgentThread) and the compaction path (resolveCompactModel) so the
// two never disagree about what "opus" means. An empty or unrecognised token
// is returned trimmed and unchanged: "" leaves Claude Code on its configured
// default, and an already-full id (e.g. "claude-opus-4-8") passes through.
func resolveModel(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "opus":
		return "claude-opus-4-8"
	case "sonnet":
		return "claude-sonnet-4-6"
	case "haiku":
		return "claude-haiku-4-5-20251001"
	case "fable":
		return "claude-fable-5"
	default:
		return strings.TrimSpace(tier)
	}
}

// applyProviderToRecord copies a provider's NON-SECRET fields onto a session
// Record so a later resume can rebuild the routing. The API token is deliberately
// never persisted — it is re-resolved at launch from the provider's env var or
// re-supplied by the UI. A nil or Claude-direct provider clears the fields.
func applyProviderToRecord(rec *session.Record, p *agent.Provider) {
	if !p.Routed() {
		rec.ProviderID, rec.ProviderName, rec.ProviderBaseURL, rec.ProviderEnvVar, rec.ProviderModels = "", "", "", "", nil
		return
	}
	rec.ProviderID = p.ID
	rec.ProviderName = p.Name
	rec.ProviderBaseURL = p.BaseURL
	rec.ProviderEnvVar = p.EnvVar
	rec.ProviderModels = p.Models
}

// providerFromRecord rebuilds the routing provider from a Record's non-secret
// snapshot. AuthToken is left empty: buildEnv resolves it from EnvVar at launch,
// or the caller passes a fresher override. Returns nil for Claude direct.
func providerFromRecord(rec session.Record) *agent.Provider {
	if rec.ProviderBaseURL == "" {
		return nil
	}
	return &agent.Provider{
		ID:      rec.ProviderID,
		Name:    rec.ProviderName,
		BaseURL: rec.ProviderBaseURL,
		EnvVar:  rec.ProviderEnvVar,
		Models:  rec.ProviderModels,
	}
}

// resolveCompactModel maps the UI-facing model token from the recovery dialog
// ("opus", "sonnet", "haiku", "fable", "local") to the claude --model id we
// spawn with and the strategy stamp for the resulting summary. The id half is
// delegated to resolveModel so spawn and compaction share one map. Empty or
// unrecognised tokens fall through to the programmatic compactor, the safe
// free fallback.
func resolveCompactModel(token string) (modelID string, strategy compact.Strategy, isLocal bool) {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "opus":
		return resolveModel("opus"), compact.ResumeOpusCold, false
	case "sonnet":
		return resolveModel("sonnet"), compact.ResumeSonnetCold, false
	case "haiku":
		return resolveModel("haiku"), compact.ResumeHaikuCold, false
	case "fable":
		// No dedicated Fable strategy bucket; stamp the closest (Sonnet-cold).
		return resolveModel("fable"), compact.ResumeSonnetCold, false
	case "local", "":
		return "", compact.ResumeLocal, true
	default:
		// Treat as a literal claude --model id; stamp the closest bucket.
		return token, compact.ResumeSonnetCold, false
	}
}

// mcpBridgeArgs is the argv for one `akcore mcp` bridge process: the
// Cooperation server, or the opt-in Cowork desktop server when cowork is set.
// backend tags kimi threads so the bridge hides the Claude-only
// request_permission tool (kimi permissions flow over ACP, not MCP).
func mcpBridgeArgs(socketPath, threadID, workspace string, cowork bool, backend string) []string {
	args := []string{
		"mcp",
		"--socket", socketPath,
		"--thread", threadID,
		"--workspace", workspace,
	}
	if cowork {
		args = append(args, "--cowork")
	}
	if backend != "" {
		args = append(args, "--backend", backend)
	}
	return args
}

// coopMCPServer describes the Cooperation MCP bridge as an ACP stdio server
// for a kimi thread's session/new.
func coopMCPServer(exePath, socketPath, threadID, workspace string) kimi.MCPServer {
	return kimi.MCPServer{
		Name:    "cooperation",
		Command: exePath,
		Args:    mcpBridgeArgs(socketPath, threadID, workspace, false, session.BackendKimi),
		Env:     []kimi.MCPEnv{},
	}
}

// writeMCPConfig writes a per-thread --mcp-config file that points `claude` at
// this binary's Cooperation MCP bridge subcommand, plus the opt-in Cowork desktop
// bridge when the thread enabled it (a second `akcore mcp ... --cowork` server).
func writeMCPConfig(exePath, socketPath, threadID, workspace string, coworkEnabled bool) (string, error) {
	servers := map[string]any{
		"cooperation": map[string]any{
			"type":    "stdio",
			"command": exePath,
			"args":    mcpBridgeArgs(socketPath, threadID, workspace, false, ""),
		},
	}
	if coworkEnabled {
		servers["cowork"] = map[string]any{
			"type":    "stdio",
			"command": exePath,
			"args":    mcpBridgeArgs(socketPath, threadID, workspace, true, ""),
		}
	}
	cfg := map[string]any{"mcpServers": servers}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "agentkate-mcp-"+threadID+"-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	return f.Name(), nil
}
