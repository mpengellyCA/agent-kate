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
	"agentkate/internal/safe"
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

// unappliedPersona is the same applied-truth pass for the persona channels
// (plan 16 P3). These entries carry a "reason" instead of a requested/applied
// pair: there is no downgraded value to report, only what was lost and why.
func unappliedPersona(systemPrompt string, profiles []harness.AgentProfile,
	launched harness.Launched, caps harness.Capabilities) []map[string]string {
	var out []map[string]string
	if strings.TrimSpace(systemPrompt) != "" && !launched.SystemPromptApplied {
		// The adapter's own reason wins when it has one (an oversize prompt is
		// not a missing capability); otherwise the channel is simply absent.
		reason := launched.SystemPromptUnapplied
		if reason == "" {
			reason = unsupportedDetail("a custom system prompt", caps) +
				"; put the persona in the worker's opening prompt instead"
		}
		out = append(out, map[string]string{
			"option": "system_prompt",
			"reason": reason,
		})
	}
	for _, a := range launched.Agents {
		if a.Applied && len(a.Unapplied) == 0 {
			continue
		}
		reason := strings.Join(a.Unapplied, "; ")
		if reason == "" {
			// A verdict with no explanation. Guessing "unsupported" would be
			// wrong for a harness that HAS the capability and lost the profile
			// for some other reason, so say only what is known.
			reason = "not applied; the harness gave no reason"
		}
		out = append(out, map[string]string{
			"option": "agents[" + profileLabel(a.Name) + "]",
			"reason": reason,
		})
	}
	// Backstop: an adapter that reports NOTHING for a requested profile would
	// otherwise drop it silently, which is exactly what applied-truth forbids.
	// Launched.Agents carries one entry per request, so anything past its end
	// is unaccounted for.
	for i := len(launched.Agents); i < len(profiles); i++ {
		out = append(out, map[string]string{
			"option": "agents[" + profileLabel(profiles[i].Name) + "]",
			"reason": caps.DisplayName + " did not report whether this subagent " +
				"profile was applied; assume it was not",
		})
	}
	return out
}

// unappliedSweepReport renders the harness's own UnappliedOptions (the plan 16
// P6 list-valued launch options) into the same reason-carrying shape. The
// adapter owns the wording, because only it knows why its CLI cannot express
// the option.
func unappliedSweepReport(launched harness.Launched) []map[string]string {
	var out []map[string]string
	for _, u := range launched.UnappliedOptions {
		out = append(out, map[string]string{"option": u.Option, "reason": u.Reason})
	}
	return out
}

// profileLabel names a profile in applied-truth output, standing in for the
// nameless (which no harness can register anyway).
func profileLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
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
			ParentThreadID string                 `json:"parentThreadId"`
			Backend        string                 `json:"backend"`
			Model          string                 `json:"model"`
			Prompt         string                 `json:"prompt"`
			Title          string                 `json:"title"`
			Isolation      string                 `json:"isolation"`
			PermissionMode string                 `json:"permissionMode"`
			Effort         string                 `json:"effort"`
			SystemPrompt   string                 `json:"systemPrompt"`
			Agents         []harness.AgentProfile `json:"agents"`
			Cowork         bool                   `json:"cowork"`
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

		// Desktop access for a worker follows the same rule as enable_cowork:
		// an agent may ask, the human decides. Asked BEFORE the launch, so a
		// refusal costs nothing and the worker simply comes up without it —
		// reported NOT APPLIED rather than silently dropped.
		cowork := false
		coworkWhyNot := ""
		switch {
		case !p.Cowork:
		case !caps.Cowork:
			coworkWhyNot = unsupportedDetail("desktop cowork", caps)
		case askCoworkEnable(d, p.ParentThreadID, "", orDefault(p.Title, "a new worker"),
			capText(firstLine(p.Prompt))):
			cowork = true
		default:
			coworkWhyNot = "the human did not approve desktop access for this worker"
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
			SystemPrompt:   p.SystemPrompt,
			Agents:         p.Agents,
			CoworkEnabled:  cowork,
		}, launchMeta{ParentThreadID: p.ParentThreadID, Title: p.Title})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// An approved desktop worker asks for the OS-level permission right
		// away, so its first desktop action does not stall on a dialog.
		if cowork && d.cowork != nil && d.cowork.Available() {
			safe.Go("cowork.preflight", func() {
				_, _ = coworkPreflight(context.Background(), d, threadID, true)
			})
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
		// Applied-truth: the downgraded options first, then the persona
		// channels — one flat list the bridge renders verbatim.
		unapplied := unappliedOptions(map[string]string{
			"model":          p.Model,
			"effort":         p.Effort,
			"permissionMode": p.PermissionMode,
		}, launched)
		unapplied = append(unapplied,
			unappliedPersona(p.SystemPrompt, p.Agents, launched, caps)...)
		unapplied = append(unapplied, unappliedSweepReport(launched)...)
		if coworkWhyNot != "" {
			unapplied = append(unapplied, map[string]string{
				"option": "cowork", "requested": "true", "reason": coworkWhyNot,
			})
		}
		var appliedAgents []string
		for _, a := range launched.Agents {
			if a.Applied {
				appliedAgents = append(appliedAgents, a.Name)
			}
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
			"systemPromptApplied": launched.SystemPromptApplied,
			"appliedAgents":       appliedAgents,
			"unapplied":           unapplied,
		}, nil
	})
}
