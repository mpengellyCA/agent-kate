package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/safe"
	"agentkate/internal/session"
)

// Switching Cowork on mid-session.
//
// Neither CLI can be handed a new MCP server once it is running, so the Cowork
// bridge is wired into EVERY thread at launch and simply advertises nothing
// until the thread opts in (writeMCPConfig / coworkMCPServer). Flipping the
// opt-in then has to reach the running agent, and the two engines differ —
// probed, not assumed:
//
//   - claude 2.1.220 honours MCP notifications/tools/list_changed: the core
//     pushes cowork.enabledChanged down the bridge's IPC connection, the bridge
//     re-advertises, and the desktop tools are usable in the very next turn.
//     Nothing restarts (Capabilities.LiveToolReveal).
//   - kimi 0.30 lists each server's tools once, at session/new, and ignores the
//     notification. That thread is re-attached instead: session/resume keeps the
//     conversation and takes a fresh mcpServers list, so the agent keeps its
//     context and gains the tools.
//
// Enabling also fires the OS-permission preflight immediately (coworkPreflight),
// so the desktop portal's "allow remote control" dialog appears while the human
// is right there switching it on — instead of ambushing them, or silently never
// appearing, halfway through an agent's turn.
const (
	coworkApplyLive      = "live"      // tools revealed in the running session
	coworkApplyReattach  = "reattach"  // session re-attached to pick them up
	coworkApplyNextStart = "nextStart" // thread is not running; applies when it launches
)

// coworkPreflightTimeout bounds the wait on the portal round-trip. Generous: it
// spans a human reading and answering the KDE portal dialog.
const coworkPreflightTimeout = 3 * time.Minute

// coworkRevealAck bounds the wait for the agent's CLI to actually re-list the
// bridge's tools after a live enable. Claude re-lists within milliseconds; the
// ceiling only stops a wedged client from stalling the caller.
const coworkRevealAck = 3 * time.Second

// revealWaiters lets an enable wait for proof that the CLI re-listed, rather
// than assuming it. The bridge re-reads cowork.threadState on every tools/list,
// so that call — from THIS thread's bridge, after the change — is the ack.
//
// Without it there is a real race: switching Cowork on and immediately sending a
// message (a human typing fast, or an agent that just had enable_cowork
// approved) can start the next turn before the client has re-listed, and that
// turn still sees no desktop tools. Waiting makes "enabled" mean "usable now".
type revealWaiters struct {
	mu sync.Mutex
	ch map[string][]chan struct{}
}

var coworkReveal = &revealWaiters{ch: map[string][]chan struct{}{}}

func (w *revealWaiters) add(threadID string) chan struct{} {
	c := make(chan struct{}, 1)
	w.mu.Lock()
	w.ch[threadID] = append(w.ch[threadID], c)
	w.mu.Unlock()
	return c
}

// signal wakes (and drops) every waiter for a thread.
func (w *revealWaiters) signal(threadID string) {
	w.mu.Lock()
	waiting := w.ch[threadID]
	delete(w.ch, threadID)
	w.mu.Unlock()
	for _, c := range waiting {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// drop removes one waiter that gave up, so a timed-out enable leaves nothing behind.
func (w *revealWaiters) drop(threadID string, c chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rest := w.ch[threadID][:0]
	for _, other := range w.ch[threadID] {
		if other != c {
			rest = append(rest, other)
		}
	}
	if len(rest) == 0 {
		delete(w.ch, threadID)
		return
	}
	w.ch[threadID] = rest
}

// coworkReattachIdleWait bounds how long a re-attach waits for the thread's
// current turn to finish before it stops the process anyway. A mid-turn stop
// loses the rest of that turn, so waiting is worth a few minutes; hanging on it
// forever is not.
const coworkReattachIdleWait = 5 * time.Minute

// setCoworkEnabled flips one thread's Cowork opt-in and makes it real for the
// running agent. It is the single path behind cowork.setEnabled (the human) and
// enable_cowork (an agent asking, once a human has approved).
func setCoworkEnabled(d handlerDeps, threadID string, enabled bool) (map[string]any, error) {
	rec, ok := d.sessions.Get(threadID)
	if !ok {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+threadID)
	}
	h := d.harnessFor(threadID)
	caps := h.Capabilities()
	if enabled && !caps.Cowork {
		return nil, unsupported("desktop cowork", caps)
	}

	changed := rec.CoworkEnabled != enabled
	if changed {
		if err := d.sessions.Update(threadID, func(r *session.Record) { r.CoworkEnabled = enabled }); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
	}

	applied := coworkApplyNextStart
	if d.agentRunning(threadID) {
		applied = coworkApplyLive
		if !caps.LiveToolReveal {
			applied = coworkApplyReattach
		}
	}
	revealed := applied != coworkApplyLive // nothing to wait for on the other paths
	if changed {
		// Tell the thread's own bridge, so it can re-advertise. Harmless on a
		// harness that ignores the resulting MCP notification — that thread is
		// being re-attached anyway.
		var ack chan struct{}
		if applied == coworkApplyLive {
			ack = coworkReveal.add(threadID) // registered BEFORE the push, or we could miss it
		}
		d.srv.NotifyBridge(threadID, "cowork.enabledChanged", map[string]any{
			"threadId": threadID, "enabled": enabled,
		})
		if ack != nil {
			select {
			case <-ack:
				revealed = true
			case <-time.After(coworkRevealAck):
				coworkReveal.drop(threadID, ack)
				d.log.Warn("cowork: the agent's CLI did not re-list its tools in time",
					"thread", threadID)
			}
		}
		d.srv.NotifyUI("cowork.enabledChanged", map[string]any{
			"threadId": threadID, "enabled": enabled, "applied": applied,
		})
		if applied == coworkApplyReattach {
			safe.Go("cowork.reattach", func() { reattachForCowork(d, threadID) })
		}
	}

	// Ask for the OS-level permissions now, not on first use. Fire-and-forget:
	// the human answers the portal dialog on their own time and the result
	// lands as a cowork.preflightResult notification, so enabling never blocks
	// on it.
	if enabled && d.cowork != nil && d.cowork.Available() {
		safe.Go("cowork.preflight", func() {
			_, _ = coworkPreflight(context.Background(), d, threadID, true)
		})
	}
	// SECURITY / honesty (audit F8): the enable dialog promises the desktop-wide
	// accessibility flip lasts "until desktop access is turned off — then your
	// original setting is restored". Only the kill-switch and app exit used to
	// honour that, so switching the last agent off left the whole session
	// exporting its AT-SPI tree with the UI claiming otherwise. Turning the LAST
	// cowork thread off now restores the flags for real.
	if changed && !enabled && noCoworkThreadsLeft(d) {
		d.srv.NotifyUI("cowork.restoreDesktopFlags", map[string]any{
			"reason": "the last agent with desktop access was switched off",
		})
	}

	return map[string]any{
		"ok": true, "enabled": enabled, "changed": changed, "applied": applied,
		"harness": caps.ID, "liveToolReveal": caps.LiveToolReveal,
		// revealed: the CLI confirmed it re-listed, so the very next turn has
		// the tools. False after a live enable means it had not re-listed yet
		// when we gave up waiting — it still will, just perhaps a turn later.
		"revealed": revealed,
	}, nil
}

// noCoworkThreadsLeft reports whether NO thread still has Cowork enabled. It reads the
// store rather than a counter so a record changed by any path (start, resume, discard)
// is accounted for. On a read failure it returns false — leaving the desktop flags in
// place is the conservative answer, because restoring them mid-session would silently
// break a live agent's element reads.
func noCoworkThreadsLeft(d handlerDeps) bool {
	if d.sessions == nil {
		return false
	}
	for _, r := range d.sessions.List("") {
		if r.CoworkEnabled {
			return false
		}
	}
	return true
}

// reattachForCowork restarts a running thread on its own session so a harness
// that cannot reveal tools live picks up the new MCP catalogue. It waits for the
// current turn first — a mid-turn stop would throw away that turn's work.
func reattachForCowork(d handlerDeps, threadID string) {
	if _, timedOut := d.turns.Wait(context.Background(), threadID, coworkReattachIdleWait); timedOut {
		d.log.Warn("cowork re-attach: thread still busy, restarting anyway", "thread", threadID)
	}
	if !d.agentRunning(threadID) {
		return // stopped on its own meanwhile; the next launch reads the record
	}
	rec, ok := d.sessions.Get(threadID)
	if !ok {
		return
	}
	h := d.harnessFor(threadID)
	if err := d.agentStop(threadID); err != nil {
		d.log.Warn("cowork re-attach: stop failed", "thread", threadID, "err", err)
		return
	}
	// Resuming before the old process is reaped would leave two processes on one
	// thread id (the first one's reap deregisters the second).
	deadline := time.Now().Add(30 * time.Second)
	for d.agentRunning(threadID) {
		if time.Now().After(deadline) {
			d.log.Error("cowork re-attach: thread did not stop", "thread", threadID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rec.SessionID == "" {
		// Nothing to resume onto — the thread simply starts with the new tool
		// set the next time the human launches it.
		d.log.Warn("cowork re-attach: no session to resume", "thread", threadID)
		return
	}
	d.log.Info("cowork re-attach: resuming thread with the new tool set", "thread", threadID)
	resumeThread(d, h, rec, nil)
}

// coworkPreflight asks the primary UI to acquire the OS-level desktop
// permissions up front: the accessibility bus (for reading windows/elements)
// and the XDG RemoteDesktop + ScreenCast portal session (for pointer and
// keyboard control). KDE refuses to persist a remote-desktop grant, so this
// runs once per Agent Kate run — doing it at enable time means the dialog
// appears while the human is present, and every later action reuses the
// approved session with no prompt.
//
// announce=true publishes the outcome to the UI as cowork.preflightResult, for
// the fire-and-forget call on enable.
func coworkPreflight(ctx context.Context, d handlerDeps, threadID string, announce bool) (map[string]any, error) {
	if d.cowork == nil || !d.cowork.Available() {
		return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
	}
	res, err := runPortal(d, ctx, "preflight", map[string]any{"threadId": threadID},
		coworkPreflightTimeout)
	out := map[string]any{"threadId": threadID, "ok": err == nil}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["detail"] = res.Error // the UI reports partial success (e.g. a11y on, portal declined) here
	}
	if announce {
		d.srv.NotifyUI("cowork.preflightResult", out)
	}
	if err != nil {
		d.log.Warn("cowork preflight did not complete", "thread", threadID, "err", err)
	}
	return out, err
}

// askCoworkEnable puts one "an agent is asking for desktop access" question in
// front of the human and blocks until they answer. Cowork is the one capability
// that reaches outside the workspace — an agent may ASK for it, never grant it
// to itself — so this prompt is unconditional, even for a thread the caller
// already controls. It resolves through the same broker (and the same
// permission.respond RPC) as every other human decision.
func askCoworkEnable(d handlerDeps, fromThreadID, targetThreadID, targetTitle, reason string) bool {
	id, ch := d.broker.Open()
	d.srv.NotifyUI("cowork.enableRequested", map[string]any{
		"requestId":      id,
		"fromThreadId":   fromThreadID,
		"threadId":       targetThreadID,
		"title":          targetTitle,
		"reason":         reason,
		"self":           fromThreadID == targetThreadID,
		"timeoutSeconds": int(permissionTimeout / time.Second),
	})
	select {
	case dec := <-ch:
		return dec.Allow
	case <-time.After(permissionTimeout):
		d.broker.Close(id)
		return false
	}
}

// coworkEnableTitle is the human-readable name of a thread for the approval
// prompt — its roster title, or the id when it has none yet.
func coworkEnableTitle(d handlerDeps, threadID string) string {
	if rec, ok := d.sessions.Get(threadID); ok && rec.Title != "" {
		return rec.Title
	}
	return threadID
}

// registerCoworkEnableHandlers wires the RPCs that switch Cowork on and off and
// report a thread's state. They are registered even when the cowork service
// failed to initialise: the bridge asks for its thread state on every
// tools/list, and must get a clean "not enabled" rather than method-not-found.
func registerCoworkEnableHandlers(d handlerDeps) {
	// cowork.threadState answers the bridge's "do my desktop tools exist?" and
	// the UI's per-agent indicator.
	d.srv.Handle("cowork.threadState", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requireUIOrOwnBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		caps := d.harnessFor(p.ThreadID).Capabilities()
		return map[string]any{
			"enabled":        ok && rec.CoworkEnabled,
			"supported":      caps.Cowork,
			"liveToolReveal": caps.LiveToolReveal,
			"available":      d.cowork != nil && d.cowork.Available(),
		}, nil
	})

	// cowork.toolsListed is the Cowork bridge reporting that it has just answered
	// a tools/list — i.e. the CLI now HAS the current catalogue. A notification,
	// not a call: the bridge sends it after flushing its reply and never waits.
	d.srv.Handle("cowork.toolsListed", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if ok, _ := d.srv.RequireBridge(ctx, p.ThreadID); ok {
			coworkReveal.signal(p.ThreadID)
		}
		return map[string]any{"ok": true}, nil
	})

	// cowork.setEnabled is the human's switch — from the agent's settings or the
	// Cowork panel — and applies to the running agent, not just the record.
	d.srv.Handle("cowork.setEnabled", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Enabled  bool   `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return setCoworkEnabled(d, p.ThreadID, p.Enabled)
	})

	// cowork.requestEnable is the agent-facing door (the enable_cowork MCP
	// tool): an agent asks, the human decides, and on approval the desktop
	// tools appear in the target's running session and the OS permission
	// dialog is raised straight away.
	d.srv.Handle("cowork.requestEnable", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			FromThreadID string `json:"fromThreadId"`
			ThreadID     string `json:"threadId"`
			Reason       string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if ok, reason := d.srv.RequireBridge(ctx, p.FromThreadID); !ok {
			return nil, ipc.Errorf(codeCoworkDenied, reason)
		}
		target := p.ThreadID
		if target == "" {
			target = p.FromThreadID
		}
		if _, ok := d.sessions.Get(target); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+target)
		}
		caps := d.harnessFor(target).Capabilities()
		if !caps.Cowork {
			return nil, unsupported("desktop cowork", caps)
		}
		// Reaching a thread outside your own worker subtree needs the standard
		// orchestration approval too — asking for desktop access on someone
		// else's agent is at least as sensitive as messaging it.
		if target != p.FromThreadID {
			if err := d.authorizeAgentTarget(p.FromThreadID, target, "enable_cowork",
				map[string]any{"reason": p.Reason}); err != nil {
				return nil, err
			}
		}
		if rec, ok := d.sessions.Get(target); ok && rec.CoworkEnabled {
			return map[string]any{"ok": true, "enabled": true, "changed": false,
				"applied": coworkApplyLive, "alreadyEnabled": true}, nil
		}
		if !askCoworkEnable(d, p.FromThreadID, target, coworkEnableTitle(d, target), p.Reason) {
			return nil, ipc.Errorf(codeCoworkDenied,
				"the human did not approve desktop access for this agent")
		}
		return setCoworkEnabled(d, target, true)
	})

	// cowork.preflight lets the UI acquire the OS permissions on demand ("Grant
	// desktop access now"), and re-acquire them after a kill-switch teardown.
	d.srv.Handle("cowork.preflight", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &p)
		return coworkPreflight(ctx, d, p.ThreadID, false)
	})
}

// requireUIOrOwnBridge allows the UI, or the agent bridge already identified
// for this very thread. Used by the read-only state RPC a bridge needs before
// it is allowed to do anything else — it must work precisely when Cowork is
// still off.
//
// It asserts an existing identity rather than creating one (audit F13): a
// connection that never proved it is this thread's bridge is refused here, so
// this handler cannot be used as a way around bridge.identify's secret.
func requireUIOrOwnBridge(d handlerDeps, ctx context.Context, threadID string) error {
	if threadID == "" {
		return ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
	}
	if d.srv.RequireUI(ctx) {
		return nil
	}
	if ok, reason := d.srv.RequireBridge(ctx, threadID); !ok {
		return ipc.Errorf(codeCoworkDenied, reason)
	}
	return nil
}

// coworkCapableHarnesses lists the engines that can run desktop tools, for the
// "which agents can do this" messaging in the UI and in agent-facing errors.
func coworkCapableHarnesses(reg *harness.Registry) []string {
	var names []string
	for _, h := range reg.All() {
		if c := h.Capabilities(); c.Cowork {
			names = append(names, c.DisplayName)
		}
	}
	return names
}
