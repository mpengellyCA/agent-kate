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
	"agentkate/internal/harness"
	"agentkate/internal/kimi"
	"agentkate/internal/safe"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// startThread creates the thread's worktree and launches the agent through
// its harness. Failures are reported as lifecycle events rather than as an
// error reply, since the agent.start reply has already been sent by the time
// this runs. sessionID is pre-minted when the harness's capabilities say so
// (claude --session-id), empty otherwise — the harness assigns its own during
// launch and Launch reports it back.
func startThread(d handlerDeps, h harness.Harness, threadID, sessionID string, p agentStartParams) {
	_, _, _ = launchThread(d, h, threadID, sessionID, p, launchMeta{})
}

// launchMeta carries the orchestration extras of a worker launch (plan 16):
// the launching thread's id (persisted as the worker's ParentThreadID), an
// optional roster title, and an explicit Role for a thread that is born into
// one (mode.apply's controller — it is a controller before it has launched
// anybody). Zero value = an ordinary human-launched thread.
type launchMeta struct {
	ParentThreadID string
	Title          string
	Role           string
}

// appliedPersona narrows a persona request down to what the harness CONFIRMED
// it applied, for the thread's record. Persisting the REQUEST instead would
// make every later resume replay a persona the agent never actually had —
// applied-truth has to survive the restart, not just the launch reply.
//
// Launched.Agents is one entry per requested profile, in request order (the
// harness contract), so the pairing is by index; a profile an adapter reported
// nothing about is simply not persisted, which is the honest reading of "we
// do not know it landed" (unappliedPersona names it at launch time).
func appliedPersona(systemPrompt string, requested []harness.AgentProfile,
	launched harness.Launched) (string, []harness.AgentProfile) {
	applied := ""
	if launched.SystemPromptApplied {
		applied = systemPrompt
	}
	var profiles []harness.AgentProfile
	for i, p := range requested {
		if i < len(launched.Agents) && launched.Agents[i].Applied {
			profiles = append(profiles, p)
		}
	}
	return applied, profiles
}

// launchThread is the shared start path behind agent.start (async, results
// ignored — failures surface as lifecycle events) and agent.launchWorker
// (synchronous — the bridge reports applied-truth back to the launching
// agent). Lifecycle events are emitted either way, so the UI narrative is
// identical for human- and agent-launched threads.
func launchThread(d handlerDeps, h harness.Harness, threadID, sessionID string,
	p agentStartParams, meta launchMeta) (harness.Launched, worktree.Worktree, error) {
	caps := h.Capabilities()
	wt, err := worktree.Create(p.WorkspacePath, threadID, p.Isolation)
	if err != nil {
		d.log.Error("worktree create failed", "thread", threadID, "err", err)
		emitLifecycle(d, threadID, "error", "worktree: "+err.Error(), nil)
		return harness.Launched{}, worktree.Worktree{}, err
	}
	wt.Number = d.sessions.NextNumber(p.WorkspacePath)
	d.threads.put(threadID, wt)
	d.gitCache.Register(wt)
	d.gitCache.Activate(threadID) // an agent is about to run here — watch it live

	launched, err := h.Launch(harness.StartSpec{
		ThreadID:       threadID,
		WorkDir:        wt.Path,
		Prompt:         p.Prompt,
		Attachments:    p.Attachments,
		Model:          p.Model,
		Effort:         p.Effort,
		PermissionMode: p.PermissionMode,
		SessionID:      sessionID,
		Cowork:         p.CoworkEnabled,
		Provider:       p.Provider,
		SystemPrompt:   p.SystemPrompt,
		Agents:         p.Agents,
	})
	if err != nil {
		emitLifecycle(d, threadID, "error", err.Error(), &wt)
		return harness.Launched{}, wt, err
	}

	// Persist the thread so it survives a stop, a crash or an Agent Kate
	// restart, and can later be resumed on the same session. The record holds
	// what the harness actually APPLIED (resolved model, defaulted mode, the
	// session id it assigned), so a later resume replays reality.
	title := meta.Title
	if strings.TrimSpace(title) == "" {
		title = p.Prompt
	}
	role := meta.Role
	if role == "" && meta.ParentThreadID != "" {
		role = session.RoleWorker
	}
	recPrompt, recAgents := appliedPersona(p.SystemPrompt, p.Agents, launched)
	rec := session.Record{
		ThreadID:       threadID,
		SessionID:      launched.SessionID,
		Project:        p.WorkspacePath,
		Worktree:       wt,
		Backend:        caps.ID,
		PermissionMode: launched.PermissionMode,
		Effort:         launched.Effort,
		Model:          launched.Model,
		Title:          summarizePrompt(title),
		ParentThreadID: meta.ParentThreadID,
		Role:           role,
		Created:        time.Now(),
		Status:         session.StatusRunning,
		CoworkEnabled:  p.CoworkEnabled,
		SystemPrompt:   recPrompt,
		Agents:         recAgents,
	}
	applyProviderToRecord(&rec, p.Provider)
	if err := d.sessions.Put(rec); err != nil {
		d.log.Warn("could not persist thread record", "thread", threadID, "err", err)
	}

	mode := "directly in the workspace"
	if wt.Isolated {
		mode = "in an isolated worktree on " + wt.Branch
	}
	d.log.Info("agent thread started", "thread", threadID, "harness", caps.ID,
		"isolated", wt.Isolated, "parent", meta.ParentThreadID, "dir", wt.Path)
	emitLifecycle(d, threadID, "started", "running "+mode, &wt)
	return launched, wt, nil
}

// resumeThread re-launches a dormant thread through its harness, in the same
// worktree it ran in before. For harnesses with compaction support, a current
// compacted summary seeds a FRESH session instead of replaying the full
// transcript — that is where the compaction savings actually land; without
// one (or without the capability) the original session is re-attached.
func resumeThread(d handlerDeps, h harness.Harness, rec session.Record, provOverride *agent.Provider) {
	caps := h.Capabilities()
	if _, err := os.Stat(rec.Worktree.Path); err != nil {
		emitLifecycle(d, rec.ThreadID, "error",
			"worktree no longer exists: "+rec.Worktree.Path, nil)
		return
	}
	d.threads.put(rec.ThreadID, rec.Worktree)
	d.gitCache.Register(rec.Worktree)
	d.gitCache.Activate(rec.ThreadID) // resuming — this thread is active again, watch it

	spec := harness.StartSpec{
		ThreadID:       rec.ThreadID,
		WorkDir:        rec.Worktree.Path,
		Model:          rec.Model,
		Effort:         rec.Effort,
		PermissionMode: rec.PermissionMode,
		Cowork:         rec.CoworkEnabled,
		Provider:       providerFromRecord(rec),
		// The persona is a launch-time flag on both paths below: a plain
		// --resume re-spawns the CLI, and a summary-seeded resume is a brand
		// new session. Re-passing what the record says was APPLIED keeps the
		// agent the same agent across a stop, a promote or a restart. Records
		// written before P3 carry none and resume exactly as they did before.
		SystemPrompt: rec.SystemPrompt,
		Agents:       rec.Agents,
	}
	// A fresh override (the UI re-supplying a KWallet-held token the Record never
	// stores) takes precedence over the env-var resolution baked into the snapshot.
	if provOverride.Routed() {
		spec.Provider = provOverride
	}

	// Seed-from-summary when the harness supports compaction and a current
	// summary is on disk. A summary is "current" when it has been refreshed
	// after the last user/agent turn.
	var sum *compact.Summary
	seeded := false
	if caps.Compaction {
		sum, _ = d.summaries.Get(rec.ThreadID)
		seeded = sum != nil &&
			(rec.LastTurnAt.IsZero() || !rec.LastTurnAt.After(rec.SummaryUpdatedAt))
	}
	detail := "resumed " + caps.DisplayName + " session"
	if seeded {
		// Fresh session seeded with the summary text. The instruction line
		// at the bottom asks the agent to acknowledge briefly so its first
		// turn is short — minimising the prefix that gets cached.
		spec.SessionID = session.NewID()
		spec.Prompt = sum.Body +
			"\n\n---\n\nThe above is prior context from a session that has " +
			"been compacted. Acknowledge in one sentence that you have read " +
			"it, then wait for the user's next instruction."
		detail = "resumed from compacted summary (new session)"
	} else {
		spec.SessionID = rec.SessionID
		spec.Resume = true
	}

	// The seeded summary prompt is a real turn: track it so a wait_agent
	// racing this resume never sees a false idle while the acknowledgement
	// turn runs. A failed launch emits the "error" lifecycle, which clears it.
	if seeded {
		d.turns.TurnQueued(rec.ThreadID)
	}
	launched, err := h.Launch(spec)
	if err != nil {
		emitLifecycle(d, rec.ThreadID, "error", err.Error(), &rec.Worktree)
		return
	}

	_ = d.sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) {
		r.Status = session.StatusRunning
		if seeded {
			// The summary is now baked into the new session; clear our
			// staleness signals so the next compact cycle starts fresh.
			r.SessionID = launched.SessionID
			r.SummaryUpdatedAt = time.Time{}
			r.LastTurnAt = time.Time{}
		}
	})
	if seeded {
		// The previous summary belonged to the old session; drop it so a
		// missed exit-compact on the new session does not silently reuse
		// stale content.
		_ = d.summaries.Remove(rec.ThreadID)
	}
	d.log.Info("agent thread resumed", "thread", rec.ThreadID, "harness", caps.ID,
		"session", launched.SessionID, "seeded", seeded)
	emitLifecycle(d, rec.ThreadID, "resumed", detail, &rec.Worktree)
}

// forkAgentThread branches a source thread's conversation into a brand-new
// thread that can run on a different model or effort while keeping the full
// context. It creates a fresh isolated worktree from the source worktree's HEAD
// (committed state only — uncommitted changes are NOT copied), then launches
// the new thread with the harness's fork semantics (claude: `--resume
// <sourceSessionID> --fork-session`), which replays the source context but
// mints a new session for the fork's own turns. The source thread is never
// touched. The new session id is captured from the init event by the run loop
// (see run.go's lastSessionID wiring), so the fork's Record starts with an
// empty SessionID and is filled in on first event. Callers gate on
// Capabilities().Fork before reaching here.
func forkAgentThread(d handlerDeps, h harness.Harness, src session.Record, newThreadID, model, effort, title string) {
	// Branch the fork's worktree from the source worktree's HEAD so it starts
	// from exactly the committed state the conversation was continuing from.
	base, ok := worktree.Head(src.Worktree.Path)
	if !ok {
		emitLifecycle(d, newThreadID, "error",
			"fork: cannot read the source worktree's HEAD (no commits to branch from)", nil)
		return
	}
	wt, err := worktree.CreateFrom(src.Project, newThreadID, base)
	if err != nil {
		d.log.Error("fork worktree create failed", "thread", newThreadID, "err", err)
		emitLifecycle(d, newThreadID, "error", "fork worktree: "+err.Error(), nil)
		return
	}
	wt.Number = d.sessions.NextNumber(src.Project)
	d.threads.put(newThreadID, wt)
	d.gitCache.Register(wt)
	d.gitCache.Activate(newThreadID)

	// Model/effort default to the source's when the fork didn't override them —
	// a fork that changes only the effort keeps the source model, and vice versa.
	// (The source record already holds resolved/applied values, which Launch
	// passes through unchanged.)
	forkModel := src.Model
	if strings.TrimSpace(model) != "" {
		forkModel = model
	}
	forkEffort := src.Effort
	if strings.TrimSpace(effort) != "" {
		forkEffort = effort
	}

	launched, err := h.Launch(harness.StartSpec{
		ThreadID:       newThreadID,
		WorkDir:        wt.Path,
		Model:          forkModel,
		Effort:         forkEffort,
		PermissionMode: src.PermissionMode,
		SessionID:      src.SessionID,
		Resume:         true,
		ForkSession:    true,
		Cowork:         src.CoworkEnabled,
		Provider:       providerFromRecord(src),
		// A fork continues the source's conversation, so it continues the
		// source's persona too — the record holds what was applied there.
		SystemPrompt: src.SystemPrompt,
		Agents:       src.Agents,
	})
	if err != nil {
		emitLifecycle(d, newThreadID, "error", err.Error(), &wt)
		return
	}
	forkPrompt, forkAgents := appliedPersona(src.SystemPrompt, src.Agents, launched)

	forkTitle := strings.TrimSpace(title)
	if forkTitle == "" {
		forkTitle = summarizePrompt("Fork of " + src.Title)
	}
	rec := session.Record{
		ThreadID: newThreadID,
		// Empty for a fork: the harness mints a new session whose id the run
		// loop captures from the init event into this record.
		SessionID:      launched.SessionID,
		Project:        src.Project,
		Worktree:       wt,
		Backend:        h.Capabilities().ID,
		PermissionMode: launched.PermissionMode,
		Effort:         launched.Effort,
		Model:          launched.Model,
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
		SystemPrompt:    forkPrompt,
		Agents:          forkAgents,
	}
	applyProviderToRecord(&rec, providerFromRecord(src))
	if err := d.sessions.Put(rec); err != nil {
		d.log.Warn("could not persist fork record", "thread", newThreadID, "err", err)
	}

	d.log.Info("agent thread forked",
		"from", src.ThreadID, "thread", newThreadID, "branch", wt.Branch,
		"model", launched.Model, "effort", launched.Effort)
	emitLifecycle(d, newThreadID, "started",
		"forked from #"+strconv.Itoa(src.Worktree.Number)+" on "+wt.Branch, &wt)
}

// promoteAgentThread upgrades a non-isolated thread to an isolated worktree: it
// stops the agent, moves the working tree and session into a fresh worktree,
// then resumes the thread there. Callers gate on Capabilities().Promote (the
// session relocation is claude-specific today).
func promoteAgentThread(d handlerDeps, h harness.Harness, rec session.Record) {
	// Stop any live process and wait for it to exit before touching git.
	_ = h.Stop(rec.ThreadID)
	for i := 0; i < 60 && h.Running(rec.ThreadID); i++ {
		time.Sleep(200 * time.Millisecond)
	}

	iso, err := worktree.Promote(rec.Worktree)
	if err != nil && !iso.Isolated {
		// Promote failed before it created the worktree — there is nothing to
		// adopt, so the record stays as-is and we just report the failure.
		emitLifecycle(d, rec.ThreadID, "error", "promote: "+err.Error(), &rec.Worktree)
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
		emitLifecycle(d, rec.ThreadID, "error", "promote: "+err.Error(), &iso)
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
		emitLifecycle(d, rec.ThreadID, "promoted",
			"promoted to an isolated worktree on "+iso.Branch+" — "+promoteWarning, &iso)
	} else {
		d.log.Info("agent thread promoted", "thread", rec.ThreadID, "branch", iso.Branch)
		emitLifecycle(d, rec.ThreadID, "promoted",
			"promoted to an isolated worktree on "+iso.Branch, &iso)
	}

	// Bring the thread back up, now inside its isolated worktree. The provider
	// (if any) is rebuilt from the Record; its token re-resolves from the env var.
	resumeThread(d, h, rec, nil)
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
	if h, ok := d.harnesses.Get(rec.Backend); !ok || !h.Capabilities().Compaction {
		return // this thread's harness has no compaction support
	}
	if compact.Strategy(rec.CompactStrategy).Resolve() != compact.ExitOpusHot {
		return
	}
	if !d.agentRunning(threadID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The compact prompt is a turn the supervisor sends internally; track it
	// so waiters see the thread busy until the summary's result lands. If the
	// summary never comes, the imminent exit lifecycle clears the count.
	if d.turns != nil {
		d.turns.TurnQueued(threadID)
	}
	text, err := d.harnessFor(threadID).Compact(ctx, harness.CompactSpec{
		ThreadID: threadID,
		Prompt:   compact.CompactPrompt,
		Hot:      true,
	})
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
		if h, ok := d.harnesses.Get(rec.Backend); !ok || !h.Capabilities().Compaction {
			continue // this thread's harness has no compaction support
		}
		if !d.agentRunning(rec.ThreadID) {
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
	wg  sync.WaitGroup
	ctx context.Context
	// harnesses resolves each record's own backend, so a cold compaction runs
	// through the thread's harness instead of assuming a claude subprocess
	// (plan 16 P6). A record whose harness has no Compaction capability never
	// reaches here — the exit hook checks first — but Compact refuses anyway.
	harnesses *harness.Registry
}

// spawn launches a cold-exit compaction for rec as a tracked background
// goroutine and returns immediately. The goroutine is registered with the
// WaitGroup before it starts, so a later drain() reliably blocks on it.
func (e *exitCompactTracker) spawn(log *slog.Logger, sessions *session.Store,
	summaries *compact.Store, rec session.Record, strategy compact.Strategy) {
	h, ok := e.harnesses.Get(rec.Backend)
	if !ok || !h.Capabilities().Compaction {
		return // nothing to run: this thread's engine cannot compact
	}
	e.wg.Add(1)
	safe.Go("shutdown.exitCompact", func() {
		defer e.wg.Done()
		runExitCompact(e.ctx, h, log, sessions, summaries, rec, strategy)
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
// straggler rather than orphaning it. The pass runs through the thread's own
// harness, which owns the mechanism.
func runExitCompact(ctx context.Context, h harness.Harness, log *slog.Logger,
	sessions *session.Store, summaries *compact.Store,
	rec session.Record, strategy compact.Strategy) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	body, err := h.Compact(ctx, harness.CompactSpec{
		ThreadID:  rec.ThreadID,
		SessionID: rec.SessionID,
		WorkDir:   rec.Worktree.Path,
		Model:     strategy.Model(),
		Prompt:    compact.CompactPrompt,
		Timeout:   5 * time.Minute,
	})
	if err != nil {
		log.Warn("exit compaction failed",
			"thread", rec.ThreadID, "strategy", strategy, "err", err)
		return
	}
	sum := compact.Summary{
		ThreadID:  rec.ThreadID,
		SessionID: rec.SessionID,
		Strategy:  strategy,
		Stripped:  rec.CompactStrip,
		Created:   time.Now().UTC(),
		Body:      body,
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

// resolveModel normalises a UI-facing model token before we hand it to
// `claude --model`. Aliases (opus/sonnet/haiku/fable/best/opusplan and their
// [1m] variants) are passed straight through: the CLI owns the alias→latest
// map, so "opus" resolves to the newest Opus rather than a version we froze
// here. Pinning aliases to concrete ids was the bug that kept "opus" on an old
// release. A full id (e.g. "claude-opus-5") and "" (the CLI default) also pass
// through unchanged. Routed providers never send aliases — their pickers carry
// concrete ids discovered from the provider's /v1/models.
func resolveModel(tier string) string {
	return strings.TrimSpace(tier)
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
// noPermissionTool hides the request_permission tool for harnesses whose
// permissions don't flow over MCP (kimi asks via ACP instead).
func mcpBridgeArgs(socketPath, threadID, workspace string, cowork, noPermissionTool bool) []string {
	args := []string{
		"mcp",
		"--socket", socketPath,
		"--thread", threadID,
		"--workspace", workspace,
	}
	if cowork {
		args = append(args, "--cowork")
	}
	if noPermissionTool {
		args = append(args, "--no-permission-tool")
	}
	return args
}

// coopMCPServer describes the Cooperation MCP bridge as an ACP stdio server
// for a kimi thread's session/new. The permission tool is hidden: kimi
// permissions flow over ACP (session/request_permission), not MCP.
func coopMCPServer(exePath, socketPath, threadID, workspace string) kimi.MCPServer {
	return kimi.MCPServer{
		Name:    "cooperation",
		Command: exePath,
		Args:    mcpBridgeArgs(socketPath, threadID, workspace, false, true),
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
			"args":    mcpBridgeArgs(socketPath, threadID, workspace, false, false),
		},
	}
	if coworkEnabled {
		servers["cowork"] = map[string]any{
			"type":    "stdio",
			"command": exePath,
			"args":    mcpBridgeArgs(socketPath, threadID, workspace, true, false),
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
