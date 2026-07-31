package main

// Orchestration RPCs (plan 16 P1): the core side of the Cooperation bridge's
// launch_agent / send_agent / wait_agent / close_agent tools. Workers are real
// Agent Kate threads — full worktree handling, roster visibility, archive
// semantics and the shared permission flow — and every bit of orchestration
// state (parent linkage, turn tracking, cross-subtree grants) lives here, not
// in the bridge.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// agentWait bounds. The bridge passes the caller's timeoutSec through; the
// clamp keeps a mistyped value from parking an IPC handler for hours.
const (
	waitDefaultTimeout = 5 * time.Minute
	waitMaxTimeout     = time.Hour
)

// orchGrants remembers which cross-subtree (caller → target → action) triples
// the human has already approved, so each pairing asks exactly once per core
// run rather than on every send.
type orchGrants struct {
	mu      sync.Mutex
	granted map[string]bool
}

func newOrchGrants() *orchGrants {
	return &orchGrants{granted: make(map[string]bool)}
}

func (g *orchGrants) key(from, target, action string) string {
	return from + "\x00" + target + "\x00" + action
}

func (g *orchGrants) has(from, target, action string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.granted[g.key(from, target, action)]
}

func (g *orchGrants) grant(from, target, action string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.granted[g.key(from, target, action)] = true
}

// forgetThread drops every grant that names the thread — as granter or as
// target. A discarded thread's approvals must not linger to silently cover a
// future thread that happens to reuse the id.
func (g *orchGrants) forgetThread(threadID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.granted {
		parts := strings.SplitN(k, "\x00", 3)
		if len(parts) == 3 && (parts[0] == threadID || parts[1] == threadID) {
			delete(g.granted, k)
		}
	}
}

// inSubtree reports whether target lies in caller's own subtree: the caller
// itself, or a thread whose ParentThreadID chain reaches the caller. The
// depth cap guards against a cyclic chain in a hand-edited threads.json.
func (d handlerDeps) inSubtree(callerID, targetID string) bool {
	id := targetID
	for depth := 0; depth < 32; depth++ {
		if id == callerID {
			return true
		}
		rec, ok := d.sessions.Get(id)
		if !ok || rec.ParentThreadID == "" {
			return false
		}
		id = rec.ParentThreadID
	}
	return false
}

// authorizeAgentTarget gates one agent acting on another thread. UI-driven
// calls (empty fromID) are never gated; a target inside the caller's own
// subtree is always allowed; anything else needs one human approval per
// (caller, target, action), through the same permission flow every gated tool
// uses — so the ask shows up in the caller's panel like any other approval.
func (d handlerDeps) authorizeAgentTarget(fromID, targetID, action string, detail map[string]any) error {
	if fromID == "" {
		return nil
	}
	if d.inSubtree(fromID, targetID) {
		return nil
	}
	if d.orchGrants.has(fromID, targetID, action) {
		return nil
	}
	input := map[string]any{"targetThreadId": targetID}
	for k, v := range detail {
		input[k] = v
	}
	rawInput, _ := json.Marshal(input)
	dec, ok := askHumanPermission(d.srv, d.broker, fromID,
		"mcp__cooperation__"+action, rawInput)
	if !ok || !dec.Allow {
		return ipc.Errorf(ipc.CodeInvalidParams,
			action+" on thread "+targetID+" was not approved by the human "+
				"(the target is outside your own worker subtree)")
	}
	d.orchGrants.grant(fromID, targetID, action)
	return nil
}

// unappliedOptions compares what a launch requested against what the harness
// reports it applied, for the applied-truth half of launch_agent's contract:
// anything requested but not applied is named, never silently dropped.
func unappliedOptions(requested map[string]string, launched harness.Launched) []map[string]string {
	applied := map[string]string{
		"model":          launched.Model,
		"effort":         launched.Effort,
		"permissionMode": launched.PermissionMode,
	}
	// Deterministic order for tests and readable tool output.
	var out []map[string]string
	for _, opt := range []string{"model", "effort", "permissionMode"} {
		want := strings.TrimSpace(requested[opt])
		if want == "" || want == applied[opt] {
			continue
		}
		out = append(out, map[string]string{
			"option": opt, "requested": want, "applied": applied[opt],
		})
	}
	return out
}

func registerOrchestrationHandlers(d handlerDeps) {
	// agent.wait blocks until the thread is idle (no turn in flight, or the
	// process ended) or the timeout fires, and returns the thread's last
	// assistant text. Backed by the turn tracker's broadcast wait — the IPC
	// handler simply blocks; no polling anywhere.
	d.srv.Handle("agent.wait", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID   string `json:"threadId"`
			TimeoutSec int    `json:"timeoutSec"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
		}
		if _, ok := d.sessions.Get(p.ThreadID); !ok && !d.agentRunning(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		timeout := waitDefaultTimeout
		if p.TimeoutSec > 0 {
			timeout = time.Duration(p.TimeoutSec) * time.Second
			if timeout > waitMaxTimeout {
				timeout = waitMaxTimeout
			}
		}
		lastText, timedOut := d.turns.Wait(ctx, p.ThreadID, timeout)
		// A cancelled context means the caller (a bridge whose agent died, a
		// disconnecting client) is gone — report that rather than a timeout.
		if err := ctx.Err(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, "wait cancelled: "+err.Error())
		}
		status := "idle"
		switch {
		case timedOut:
			status = "timeout"
		case !d.agentRunning(p.ThreadID):
			// Idle because the process is gone (finished and stopped, crashed,
			// or launch failed) — the thread is dormant/ended, not waiting.
			status = "exited"
		}
		return map[string]any{"status": status, "lastText": lastText}, nil
	})

	// agent.launchWorker starts a real Agent Kate thread on behalf of another
	// thread (the Cooperation bridge's launch_agent), synchronously — the
	// caller gets applied-truth back, not a promise. The worker is parented to
	// the launcher and appears in the roster like any human-started agent.
	d.srv.Handle("agent.launchWorker", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ParentThreadID string `json:"parentThreadId"`
			Backend        string `json:"backend"`
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			Title          string `json:"title"`
			Isolation      string `json:"isolation"`
			PermissionMode string `json:"permissionMode"`
			Effort         string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ParentThreadID == "" || strings.TrimSpace(p.Prompt) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"parentThreadId and prompt are required")
		}
		parent, ok := d.sessions.Get(p.ParentThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"unknown launching thread "+p.ParentThreadID)
		}
		// Empty backend = the caller's own harness (not the registry default:
		// a kimi controller's unqualified worker is another kimi).
		backend := p.Backend
		if backend == "" {
			backend = parent.Backend
		}
		h, ok := d.harnesses.Get(backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+backend)
		}
		caps := h.Capabilities()
		switch p.Isolation {
		case "", worktree.ModeAuto, worktree.ModeIsolated, worktree.ModeWorkspace:
		default:
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"isolation must be auto, isolated or workspace")
		}

		threadID := agent.NewThreadID()
		sessionID := ""
		if caps.MintsSessionID {
			sessionID = session.NewID()
		}
		// The opening prompt is a turn; queue it before the launch so a
		// wait_agent racing the start never sees a false idle. A failed launch
		// emits the "error" lifecycle, which clears it.
		d.turns.TurnQueued(threadID)
		launched, wt, err := launchThread(d, h, threadID, sessionID, agentStartParams{
			// Workers root in the PARENT'S PROJECT (never its worktree), so an
			// isolated worker gets a sibling worktree of its controller's.
			WorkspacePath:  parent.Project,
			Prompt:         p.Prompt,
			PermissionMode: p.PermissionMode,
			Effort:         p.Effort,
			Model:          p.Model,
			Backend:        caps.ID,
			Isolation:      p.Isolation,
		}, launchMeta{ParentThreadID: p.ParentThreadID, Title: p.Title})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// First worker promotes the launcher to controller; a worker that
		// launches sub-workers keeps its own worker role (the parent chain
		// carries the structure).
		if parent.Role == "" {
			_ = d.sessions.UpdateQuiet(p.ParentThreadID, func(r *session.Record) {
				if r.Role == "" {
					r.Role = session.RoleController
				}
			})
		}
		return map[string]any{
			"threadId":  threadID,
			"sessionId": launched.SessionID,
			"backend":   caps.ID,
			"isolated":  wt.Isolated,
			"branch":    wt.Branch,
			"applied": map[string]string{
				"model":          launched.Model,
				"effort":         launched.Effort,
				"permissionMode": launched.PermissionMode,
			},
			"unapplied": unappliedOptions(map[string]string{
				"model":          p.Model,
				"effort":         p.Effort,
				"permissionMode": p.PermissionMode,
			}, launched),
		}, nil
	})
}
