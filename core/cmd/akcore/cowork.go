package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentkate/internal/cowork"
	"agentkate/internal/ipc"
	"agentkate/internal/kde"
	"agentkate/internal/session"
)

// codeCoworkDenied is returned when a Cowork action is refused (no consent, wrong
// origin, not enabled, kill-switch). The MCP bridge surfaces the message to the agent.
const codeCoworkDenied = -32010

// coworkNotifier adapts ipc.Server.Notify to the cowork.Notifier interface so the
// consent authority can push grant prompts / changes to the UI.
type coworkNotifier struct{ srv *ipc.Server }

func (n coworkNotifier) Notify(method string, params any) { n.srv.Notify(method, params) }

// screenshotMaxDim caps the longest edge of a returned still (plan 02 §3): keeps the
// base64 image well under the 16 MiB IPC frame cap and within model vision limits.
const screenshotMaxDim = 1568

// registerCoworkHandlers wires the v1 cowork.* RPCs. It is a no-op if the service
// failed to initialize (e.g. no session bus) — the tools then report unavailable.
func registerCoworkHandlers(d handlerDeps) {
	cw := d.cowork
	if cw == nil {
		return
	}

	// Per-thread pointer profiles + last-position mirror + the user's speed/accuracy
	// bounds, shared by the positioned-pointer handlers below (plan 09 §4).
	pstate := newPointerState()

	// --- capability RPCs (agent bridge → core; consent-gated) ------------------

	d.srv.Handle("cowork.listWindows", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapWindowList,
			Target: cowork.Target{Kind: cowork.TargetAny}, SuggestedScope: cowork.ScopeSession,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		wins, err := cw.KDE().ListWindows(8 * time.Second)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		cw.AuditCapture(p.ThreadID, cowork.CapWindowList, cowork.Target{Kind: cowork.TargetAny}, dec.GrantID, hashJSON(wins))
		return map[string]any{"windows": wins, "grantId": dec.GrantID}, nil
	})

	d.srv.Handle("cowork.screenshot", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID    string        `json:"threadId"`
			Target      cowork.Target `json:"target"`
			MaxDim      int           `json:"maxDim"`
			Format      string        `json:"format"`
			Interactive bool          `json:"interactive"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if p.Target.Kind == "" {
			p.Target = cowork.Target{Kind: cowork.TargetScreen, Label: "active screen"}
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapScreenshot,
			Target: p.Target, SuggestedScope: cowork.ScopeOnce,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		maxDim := p.MaxDim
		if maxDim <= 0 || maxDim > screenshotMaxDim {
			maxDim = screenshotMaxDim
		}
		format := p.Format
		if format != "jpeg" {
			format = "png"
		}
		res, err := runPortal(d, ctx, "screenshot", map[string]any{
			"threadId":    p.ThreadID,
			"target":      p.Target,
			"maxDim":      maxDim,
			"format":      format,
			"interactive": p.Interactive,
		}, 125*time.Second)
		if err != nil {
			return nil, err
		}
		cw.AuditCapture(p.ThreadID, cowork.CapScreenshot, p.Target, dec.GrantID, hashString(res.PNGB64))
		return map[string]any{
			"pngB64": res.PNGB64, "mime": res.Mime,
			"width": res.Width, "height": res.Height, "grantId": dec.GrantID,
		}, nil
	})

	// --- UI-only RPCs (the agent can never invoke these — origin checked) -------

	d.srv.Handle("cowork.respondGrant", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			RequestID    string `json:"requestId"`
			Allow        bool   `json:"allow"`
			Scope        string `json:"scope"`
			ExpiresInSec int    `json:"expiresInSec"`
			Redact       bool   `json:"redact"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		ok := cw.Respond(p.RequestID, p.Allow, cowork.Scope(p.Scope), p.ExpiresInSec, p.Redact)
		return map[string]any{"ok": ok}, nil
	})

	d.srv.Handle("cowork.requestGrant", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID     string        `json:"threadId"`
			Capability   string        `json:"capability"`
			Target       cowork.Target `json:"target"`
			Scope        string        `json:"scope"`
			ExpiresInSec int           `json:"expiresInSec"`
			Redact       bool          `json:"redact"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		cap := cowork.Capability(p.Capability)
		if !cap.Valid() {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown capability")
		}
		g, err := cw.GrantDirect(p.ThreadID, cap, p.Target, cowork.Scope(p.Scope), p.ExpiresInSec, p.Redact)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"grant": g}, nil
	})

	d.srv.Handle("cowork.listGrants", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &p)
		grants, killed := cw.ListGrants(p.ThreadID)
		return map[string]any{"grants": grants, "killed": killed}, nil
	})

	d.srv.Handle("cowork.revokeGrant", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		reason := p.Reason
		if reason == "" {
			reason = "revoked by user"
		}
		g := cw.RevokeGrant(p.ID, reason)
		return map[string]any{"ok": g != nil, "grant": g}, nil
	})

	d.srv.Handle("cowork.killSwitch", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			On     bool   `json:"on"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		var revoked []string
		if p.On {
			revoked = cw.Kill(orDefault(p.Reason, "user pressed the kill-switch"))
		} else {
			cw.Rearm(orDefault(p.Reason, "user re-armed access"))
		}
		return map[string]any{"ok": true, "revoked": revoked}, nil
	})

	d.srv.Handle("cowork.listAudit", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			ThreadID string `json:"threadId"`
			SinceSeq int64  `json:"sinceSeq"`
			Limit    int    `json:"limit"`
		}
		_ = json.Unmarshal(raw, &p)
		entries, next, err := cw.ListAudit(p.ThreadID, p.SinceSeq, p.Limit)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"entries": entries, "nextSeq": next}, nil
	})

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
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) { r.CoworkEnabled = p.Enabled }); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("cowork.portalResult", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var r kde.PortalResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		cw.Portal().Resolve(r.CorrID, r)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("cowork.status", func(ctx context.Context, raw json.RawMessage) (any, error) {
		// Readable by the UI; reports the capability probe + kill state.
		_, killed := cw.ListGrants("")
		return map[string]any{"available": cw.Available(), "killed": killed, "tampered": cw.Tampered()}, nil
	})

	// --- global capability policy (the toggle switchboard, Phase 2) — UI-only -----

	d.srv.Handle("cowork.getPolicy", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		enabled := cw.PolicyList()
		caps := make([]map[string]any, 0)
		for _, c := range cowork.AllToggleable() {
			caps = append(caps, map[string]any{
				"key":     string(c),
				"tier":    string(cowork.TierOf(c)),
				"enabled": enabled[c],
			})
		}
		return map[string]any{"capabilities": caps}, nil
	})

	d.srv.Handle("cowork.setPolicy", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Capability string `json:"capability"`
			Enabled    bool   `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		cap := cowork.Capability(p.Capability)
		if !cap.Valid() {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown capability")
		}
		if err := cw.SetPolicy(cap, p.Enabled); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// --- control: keyboard/pointer injection (R2) — agent bridge → core ----------

	d.srv.Handle("cowork.injectInput", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID       string        `json:"threadId"`
			TargetWindowID string        `json:"targetWindowId"`
			Events         []injectEvent `json:"events"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}

		ops, desc, err := buildInjectOps(p.Events)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}

		// Anti-escalation: this low-level path fires buttons wherever the pointer already
		// sits, so it cannot self-target by coordinate the way the positioned tools do. If
		// a prior pointer_control move parked the cursor on Agent Kate's own UI, a bare
		// click here would hit our consent/kill-switch controls — the geometric guard's
		// blind spot. Fail CLOSED: a bare click is allowed only when the last commanded
		// pointer position is known AND verified outside every Agent Kate window. If the
		// position is unknown (no positioned move this session), refuse and steer the agent
		// to the guarded desktop_click(x,y).
		if injectHasButton(p.Events) {
			last, ok := pstate.last(p.ThreadID)
			if !ok {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
					cowork.Target{Kind: cowork.TargetScreen, Label: "bare click at an unverified pointer position"},
					"refused: cannot verify where a bare click would land")
				return nil, ipc.Errorf(codeCoworkDenied,
					"refused: a bare click fires at the cursor's current position, which can't be verified safe — "+
						"use desktop_click(x,y) (it targets and guards an exact point), or move the pointer first with desktop_move_pointer")
			}
			if err := guardPointerTargets(cw, []point{last}); err != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
					cowork.Target{Kind: cowork.TargetScreen, Label: "bare click at the current pointer"}, err.Error())
				return nil, ipc.Errorf(codeCoworkDenied, err.Error())
			}
		}

		// Resolve the target window's class/title so the self-target guard can refuse
		// Agent Kate's own UI and the consent prompt can name the window.
		target := cowork.Target{Kind: cowork.TargetWindow, WindowID: p.TargetWindowID, Label: "the focused window"}
		if p.TargetWindowID != "" {
			if wins, err := cw.KDE().ListWindows(4 * time.Second); err == nil {
				for _, w := range wins {
					if w.InternalID == p.TargetWindowID {
						target.ResourceClass = w.ResourceClass
						if w.Caption != "" {
							target.Label = w.Caption
						}
						break
					}
				}
			}
		}

		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID:       p.ThreadID,
			Capability:     cowork.CapInputInject,
			Target:         target,
			SuggestedScope: cowork.ScopeOnce,
			ActionPreview: &cowork.ActionDescriptor{
				Mechanism:   "input_inject",
				WindowTitle: target.Label,
				Detail:      desc,
			},
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}

		// Focus the target window first so the keystrokes land on it.
		if p.TargetWindowID != "" {
			if err := cw.KDE().ActivateWindow(p.TargetWindowID, 4*time.Second); err != nil {
				d.log.Warn("cowork: could not focus target window", "id", p.TargetWindowID, "err", err)
			}
		}

		if _, err := runPortal(d, ctx, "inject", map[string]any{
			"threadId": p.ThreadID,
			"ops":      ops,
		}, 35*time.Second); err != nil {
			return nil, err
		}
		cw.AuditCapture(p.ThreadID, cowork.CapInputInject, target, dec.GrantID, hashString(desc))
		return map[string]any{"ok": true, "actions": desc}, nil
	})

	// --- control: choreographed input timeline (R2) — agent bridge → core ---------
	// Compiles an ordered, time-pinned score of keyboard + pointer events (cowork plan
	// 10) into one delayMs-bearing op stream the UI replays. It rides the same consent/
	// guard/audit spine as injectInput + the positioned-pointer tools: a keyboard event
	// needs input_inject, a pointer event needs pointer_control, a mixed script needs
	// BOTH (so with both toggles off the user may see up to two prompts — the common
	// toggles-on path prompts for neither).

	d.srv.Handle("cowork.playInput", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID       string               `json:"threadId"`
			TargetWindowID string               `json:"targetWindowId"`
			FPS            float64              `json:"fps"`
			Profile        *pointerProfilePatch `json:"profile"`
			Events         []timelineEvent      `json:"events"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}

		// Compile. Seed the path expansion from the thread's last commanded position so the
		// first move draws a real path and bare buttons/scrolls can be verified. The
		// per-event patch wins; otherwise the call-level default; both fold onto the
		// thread's standing profile, clamped to the user's bounds (reuse pstate.resolve).
		start, haveStart := pstate.last(p.ThreadID)
		resolveProfile := func(evPatch *pointerProfilePatch) PointerProfile {
			patch := evPatch
			if patch == nil {
				patch = p.Profile
			}
			return pstate.resolve(p.ThreadID, patch)
		}
		plan, err := buildTimelineOps(timelineScript{Events: p.Events, FPS: p.FPS}, start, haveStart, resolveProfile, newPointerRNG())
		if err != nil {
			// Script-construction / fail-closed errors (bad schedule, unreleased hold,
			// span overrun, unverifiable bare click) are the caller's mistake.
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}

		// Resolve the target window's class/title so the self-target guard can refuse
		// Agent Kate's own UI and the consent prompt(s) can name the window.
		target := cowork.Target{Kind: cowork.TargetWindow, WindowID: p.TargetWindowID, Label: "the focused window"}
		if p.TargetWindowID != "" {
			if wins, err := cw.KDE().ListWindows(4 * time.Second); err == nil {
				for _, w := range wins {
					if w.InternalID == p.TargetWindowID {
						target.ResourceClass = w.ResourceClass
						if w.Caption != "" {
							target.Label = w.Caption
						}
						break
					}
				}
			}
		}

		// Authorize each capability the compiled script actually exercises, surfacing the
		// compact, literal description (never an opaque "play script") as the prompt detail.
		// Track the primary capability + grant for the audit record below.
		primaryCap := cowork.CapInputInject
		var primaryGrant string
		if plan.HasKey {
			dec, err := cw.Authorize(ctx, cowork.AuthRequest{
				ThreadID:       p.ThreadID,
				Capability:     cowork.CapInputInject,
				Target:         target,
				SuggestedScope: cowork.ScopeOnce,
				ActionPreview: &cowork.ActionDescriptor{
					Mechanism:   "input_inject",
					WindowTitle: target.Label,
					Detail:      plan.Desc,
				},
			})
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			if !dec.Allow {
				return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
			}
			primaryGrant = dec.GrantID
		}
		if plan.HasPointer {
			dec, err := cw.Authorize(ctx, cowork.AuthRequest{
				ThreadID:       p.ThreadID,
				Capability:     cowork.CapPointerControl,
				Target:         target,
				SuggestedScope: cowork.ScopeOnce,
				ActionPreview: &cowork.ActionDescriptor{
					Mechanism:   "pointer_control",
					WindowTitle: target.Label,
					Detail:      plan.Desc,
				},
			})
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			if !dec.Allow {
				return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
			}
			primaryCap = cowork.CapPointerControl
			primaryGrant = dec.GrantID
		}

		// Submit-time self-target guard over every click/scroll point. Per plan §3 this is
		// a submit-time check: we rely on the bounded (≤30s) span rather than re-checking
		// each op at its fire-time.
		if len(plan.GuardPts) > 0 {
			if err := guardPointerTargets(cw, plan.GuardPts); err != nil {
				cw.AuditRefusal(p.ThreadID, primaryCap, target, err.Error())
				return nil, ipc.Errorf(codeCoworkDenied, err.Error())
			}
		}

		// Focus the target window first so the keystrokes/clicks land on it.
		if p.TargetWindowID != "" {
			if err := cw.KDE().ActivateWindow(p.TargetWindowID, 4*time.Second); err != nil {
				d.log.Warn("cowork: could not focus target window", "id", p.TargetWindowID, "err", err)
			}
		}

		// 60s: a timeline may legitimately span up to 30s of playback and the UI replies
		// only once the whole stream has drained.
		if _, err := runPortal(d, ctx, "inject", map[string]any{
			"threadId": p.ThreadID,
			"ops":      plan.Ops,
		}, 60*time.Second); err != nil {
			return nil, err
		}
		// Commit the pointer mirror only after a successful play (mirror move/click), so a
		// later bare click in another call can be verified against where we left the cursor.
		if plan.HaveFinal {
			pstate.setLast(p.ThreadID, plan.FinalPos)
		}
		cw.AuditCapture(p.ThreadID, primaryCap, target, primaryGrant, hashString(plan.Desc))
		return map[string]any{"ok": true, "actions": plan.Desc}, nil
	})

	// --- control: positioned pointer (move/click/scroll/drag) (R2) ----------------
	// All of these gate on the single pointer_control capability and route through the
	// same move+notify ops the UI plays. Coordinates are absolute desktop pixels.

	d.srv.Handle("cowork.movePointer", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID   string               `json:"threadId"`
			X          int                  `json:"x"`
			Y          int                  `json:"y"`
			RelativeTo *pointerRef          `json:"relativeTo"`
			Profile    *pointerProfilePatch `json:"profile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requirePointerControl(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		pt, frame, err := resolveGlobalPoint(cw, p.X, p.Y, p.RelativeTo)
		if err != nil {
			return nil, err
		}
		prof := pstate.resolve(p.ThreadID, p.Profile)
		ops := pstate.moveOps(p.ThreadID, pt.X, pt.Y, prof, newPointerRNG())
		desc := fmt.Sprintf("move pointer to (%d,%d)%s", pt.X, pt.Y, frame)
		// Move-only may pass over Agent Kate's windows (motion has no side effect); only
		// a click/scroll on them is refused — so no geometric guard here.
		res, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, nil)
		if err == nil {
			pstate.setLast(p.ThreadID, pt)
		}
		return res, err
	})

	// Relative pointer motion: raw dx/dy deltas, the input a pointer-grabbing game (mouse-
	// look) reads. Distinct from cowork.movePointer (absolute targeting): no screencast
	// stream, no self-target guard (no landing point), and the position mirror is left
	// unchanged — a grab makes the true cursor position unknowable. steps>1 splits the
	// delta into smooth, timed sub-nudges.
	d.srv.Handle("cowork.movePointerRelative", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string  `json:"threadId"`
			DX       float64 `json:"dx"`
			DY       float64 `json:"dy"`
			Steps    int     `json:"steps"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requirePointerControl(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		dx, dy := clampRelDelta(p.DX), clampRelDelta(p.DY)
		ops := relMoveOps(dx, dy, p.Steps)
		desc := fmt.Sprintf("relative pointer move (%+.0f,%+.0f)", dx, dy)
		// No guard (no absolute target); mirror deliberately NOT committed (see relMoveOp).
		return runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, nil)
	})

	d.srv.Handle("cowork.pointerClick", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID   string               `json:"threadId"`
			X          int                  `json:"x"`
			Y          int                  `json:"y"`
			Button     string               `json:"button"`
			Count      int                  `json:"count"`
			RelativeTo *pointerRef          `json:"relativeTo"`
			Profile    *pointerProfilePatch `json:"profile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requirePointerControl(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		code, err := buttonCodeFor(p.Button)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		pt, frame, err := resolveGlobalPoint(cw, p.X, p.Y, p.RelativeTo)
		if err != nil {
			return nil, err
		}
		count := p.Count
		if count < 1 {
			count = 1
		}
		if count > 3 {
			count = 3
		}
		prof := pstate.resolve(p.ThreadID, p.Profile)
		ops := clickOps(pstate.moveOps(p.ThreadID, pt.X, pt.Y, prof, newPointerRNG()), code, count, prof.SettleMs)
		click := buttonName(code) + "-click"
		if count == 2 {
			click = "double " + click
		} else if count > 2 {
			click = fmt.Sprintf("%d× %s", count, click)
		}
		desc := fmt.Sprintf("%s at (%d,%d)%s", click, pt.X, pt.Y, frame)
		res, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, []point{pt})
		if err == nil {
			pstate.setLast(p.ThreadID, pt)
		}
		return res, err
	})

	d.srv.Handle("cowork.pointerClickElement", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID  string               `json:"threadId"`
			ElementID string               `json:"elementId"`
			Button    string               `json:"button"`
			Anchor    string               `json:"anchor"` // center (default), corners, edge midpoints
			Dx        int                  `json:"dx"`     // pixel nudge from the anchor
			Dy        int                  `json:"dy"`
			Profile   *pointerProfilePatch `json:"profile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requirePointerControl(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if p.ElementID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "elementId is required (from desktop_list_elements)")
		}
		code, err := buttonCodeFor(p.Button)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// TOCTOU re-check: re-resolve the element's live bounds (it may have moved/gone).
		info, rect, ok, err := cw.KDE().ElementBounds(p.ElementID, 8*time.Second)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "the element has no on-screen bounds to click (re-list elements)")
		}
		// Resolve the owning window (for the consent prompt's class/title AND to translate
		// AT-SPI's surface-relative bounds into the global pixels the pointer uses).
		target, elemLabel, ownWin, haveWin := elementTargetWindow(cw, info)
		// Click point = the chosen anchor on the element + a pixel nudge, all translated to
		// global. anchor=center+(0,0) reproduces the old "click the middle" behaviour.
		ax, ay := anchorPoint(rect, p.Anchor)
		ox, oy := elementGlobalOffset(rect, ownWin, haveWin)
		cx, cy := ax+ox+p.Dx, ay+oy+p.Dy
		prof := pstate.resolve(p.ThreadID, p.Profile)
		ops := clickOps(pstate.moveOps(p.ThreadID, cx, cy, prof, newPointerRNG()), code, 1, prof.SettleMs)
		desc := fmt.Sprintf("%s-click %s at (%d,%d)", buttonName(code), elemLabel, cx, cy)
		target.Label = orDefault(target.Label, "the focused window")
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapPointerControl,
			Target: target, SuggestedScope: cowork.ScopeOnce,
			ActionPreview: &cowork.ActionDescriptor{
				Mechanism: "pointer_control", AppName: target.ResourceClass,
				WindowTitle: target.Label, Element: elemLabel, Detail: desc,
			},
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		if err := guardPointerTargets(cw, []point{{cx, cy}}); err != nil {
			cw.AuditRefusal(p.ThreadID, cowork.CapPointerControl, target, err.Error())
			return nil, ipc.Errorf(codeCoworkDenied, err.Error())
		}
		if _, err := runPortal(d, ctx, "inject", map[string]any{"threadId": p.ThreadID, "ops": ops}, 45*time.Second); err != nil {
			return nil, err
		}
		pstate.setLast(p.ThreadID, point{cx, cy})
		cw.AuditCapture(p.ThreadID, cowork.CapPointerControl, target, dec.GrantID, hashString(desc))
		return map[string]any{"ok": true, "action": desc, "element": elemLabel}, nil
	})

	d.srv.Handle("cowork.scroll", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID   string      `json:"threadId"`
			DX         int         `json:"dx"` // horizontal wheel notches (sign = direction)
			DY         int         `json:"dy"` // vertical wheel notches (sign = direction)
			X          *int        `json:"x"`  // optional: move here first
			Y          *int        `json:"y"`
			RelativeTo *pointerRef `json:"relativeTo"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requirePointerControl(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if p.DX == 0 && p.DY == 0 {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "dx and dy are both zero — nothing to scroll")
		}
		var ops []map[string]any
		var at point
		switch {
		case p.X != nil && p.Y != nil:
			pt, _, err := resolveGlobalPoint(cw, *p.X, *p.Y, p.RelativeTo)
			if err != nil {
				return nil, err
			}
			at = pt
			prof := pstate.resolve(p.ThreadID, nil)
			ops = append(ops, pstate.moveOps(p.ThreadID, at.X, at.Y, prof, newPointerRNG())...)
		default:
			// Scroll lands at the current pointer position; we must know it to run the
			// geometric guard (a scroll on Agent Kate's UI is refused). Fail closed.
			last, ok := pstate.last(p.ThreadID)
			if !ok {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"pass x,y (or move the pointer first) so the scroll location can be verified")
			}
			at = last
		}
		if p.DY != 0 {
			ops = append(ops, scrollOp(0, p.DY))
		}
		if p.DX != 0 {
			ops = append(ops, scrollOp(1, p.DX))
		}
		desc := fmt.Sprintf("scroll vertical %d / horizontal %d notches at (%d,%d)", p.DY, p.DX, at.X, at.Y)
		res, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, []point{at})
		if err == nil {
			pstate.setLast(p.ThreadID, at)
		}
		return res, err
	})

	d.srv.Handle("cowork.pointerDrag", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID   string               `json:"threadId"`
			FromX      int                  `json:"fromX"`
			FromY      int                  `json:"fromY"`
			ToX        int                  `json:"toX"`
			ToY        int                  `json:"toY"`
			RelativeTo *pointerRef          `json:"relativeTo"` // shared frame for both endpoints
			Profile    *pointerProfilePatch `json:"profile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := requirePointerControl(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		from, _, err := resolveGlobalPoint(cw, p.FromX, p.FromY, p.RelativeTo)
		if err != nil {
			return nil, err
		}
		to, frame, err := resolveGlobalPoint(cw, p.ToX, p.ToY, p.RelativeTo)
		if err != nil {
			return nil, err
		}
		prof := pstate.resolve(p.ThreadID, p.Profile)
		ops := pstate.dragOps(p.ThreadID, from, to, prof, newPointerRNG())
		desc := fmt.Sprintf("drag from (%d,%d) to (%d,%d)%s", from.X, from.Y, to.X, to.Y, frame)
		res, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, []point{from, to})
		if err == nil {
			pstate.setLast(p.ThreadID, to)
		}
		return res, err
	})

	d.srv.Handle("cowork.setPointerProfile", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			pointerProfilePatch
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Setting a session preference performs no desktop action, so it only needs the
		// bridge gate (no consent prompt), but the thread must have Cowork enabled.
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		eff := pstate.setThreadProfile(p.ThreadID, &p.pointerProfilePatch)
		return map[string]any{"ok": true, "profile": eff}, nil
	})

	d.srv.Handle("cowork.setPointerBounds", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUI(d, ctx); err != nil {
			return nil, err
		}
		var patch pointerProfilePatch
		if err := json.Unmarshal(raw, &patch); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		eff := pstate.setBounds(&patch)
		return map[string]any{"ok": true, "bounds": eff}, nil
	})

	// --- AT-SPI element index + cursor-free activation (R1 read / R2 act) ---------

	d.srv.Handle("cowork.listElements", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID       string `json:"threadId"`
			TargetWindowID string `json:"targetWindowId"`
			Max            int    `json:"max"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		win, ok := resolveTargetWindow(cw, p.TargetWindowID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no target window: pass targetWindowId (from desktop_list_windows), or focus a window first")
		}
		target := cowork.Target{
			Kind: cowork.TargetWindow, WindowID: win.InternalID,
			ResourceClass: win.ResourceClass, Label: orDefault(win.Caption, win.ResourceClass),
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapA11yRead,
			Target: target, SuggestedScope: cowork.ScopeSession,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		elems, truncated, err := cw.KDE().ListElements(win.PID,
			kde.Rect{X: win.X, Y: win.Y, W: win.Width, H: win.Height}, p.Max, 30*time.Second)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		cw.AuditCapture(p.ThreadID, cowork.CapA11yRead, target, dec.GrantID, hashJSON(elems))
		return map[string]any{"elements": elems, "truncated": truncated, "grantId": dec.GrantID}, nil
	})

	d.srv.Handle("cowork.readText", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID       string `json:"threadId"`
			TargetWindowID string `json:"targetWindowId"`
			MaxChars       int    `json:"maxChars"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		win, ok := resolveTargetWindow(cw, p.TargetWindowID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no target window: pass targetWindowId (from desktop_list_windows), or focus a window first")
		}
		target := cowork.Target{
			Kind: cowork.TargetWindow, WindowID: win.InternalID,
			ResourceClass: win.ResourceClass, Label: orDefault(win.Caption, win.ResourceClass),
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapA11yRead,
			Target: target, SuggestedScope: cowork.ScopeSession,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		text, truncated, err := cw.KDE().ReadText(win.PID,
			kde.Rect{X: win.X, Y: win.Y, W: win.Width, H: win.Height}, p.MaxChars, 30*time.Second)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		cw.AuditCapture(p.ThreadID, cowork.CapA11yRead, target, dec.GrantID, hashString(text))
		return map[string]any{"text": text, "truncated": truncated, "grantId": dec.GrantID}, nil
	})

	d.srv.Handle("cowork.activateElement", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID  string `json:"threadId"`
			ElementID string `json:"elementId"`
			Action    string `json:"action"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		if p.ElementID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "elementId is required (from desktop_list_elements)")
		}
		info, err := cw.KDE().ElementInfo(p.ElementID, 8*time.Second)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		target, elemLabel := elementTarget(cw, info)
		act := p.Action
		if act == "" {
			act = "activate (default action)"
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapA11yAction,
			Target: target, SuggestedScope: cowork.ScopeOnce,
			ActionPreview: &cowork.ActionDescriptor{
				Mechanism: "a11y_action", AppName: target.ResourceClass,
				WindowTitle: target.Label, Element: elemLabel, Detail: act,
			},
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		if err := cw.KDE().ActivateElement(p.ElementID, p.Action, 10*time.Second); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		cw.AuditCapture(p.ThreadID, cowork.CapA11yAction, target, dec.GrantID, hashString(elemLabel+"|"+act))
		return map[string]any{"ok": true, "element": elemLabel, "action": act}, nil
	})

	d.srv.Handle("cowork.setElementText", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID  string `json:"threadId"`
			ElementID string `json:"elementId"`
			Text      string `json:"text"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		if p.ElementID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "elementId is required (from desktop_list_elements)")
		}
		info, err := cw.KDE().ElementInfo(p.ElementID, 8*time.Second)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		target, elemLabel := elementTarget(cw, info)
		detail := fmt.Sprintf("set text (%d chars)", len([]rune(p.Text)))
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapA11yAction,
			Target: target, SuggestedScope: cowork.ScopeOnce,
			ActionPreview: &cowork.ActionDescriptor{
				Mechanism: "a11y_action", AppName: target.ResourceClass,
				WindowTitle: target.Label, Element: elemLabel, Detail: detail,
			},
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		if err := cw.KDE().SetElementText(p.ElementID, p.Text, 10*time.Second); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// Audit the action + a hash of the text (never the plaintext itself).
		cw.AuditCapture(p.ThreadID, cowork.CapA11yAction, target, dec.GrantID, hashString(elemLabel+"|"+p.Text))
		return map[string]any{"ok": true, "element": elemLabel}, nil
	})

	// --- launch a user-configured browser with its a11y tree enabled (R1) ---------

	d.srv.Handle("cowork.launchBrowser", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Name     string `json:"name"` // optional: pick one of the user's configured browsers
		}
		_ = json.Unmarshal(raw, &p)
		if err := requireCoworkBridge(d, ctx, p.ThreadID); err != nil {
			return nil, err
		}
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		label := "open a web browser"
		if p.Name != "" {
			label = "open " + p.Name
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapLaunchBrowser,
			Target: cowork.Target{Kind: cowork.TargetAny, Label: label}, SuggestedScope: cowork.ScopeSession,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if !dec.Allow {
			return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
		}
		// The UI owns the browser list + launching (KConfig + the right a11y flag/env);
		// it resolves p.Name against the user's configured browsers — the agent can
		// never name an arbitrary executable.
		res, err := runPortal(d, ctx, "launchBrowser", map[string]any{
			"threadId": p.ThreadID,
			"name":     p.Name,
		}, 15*time.Second)
		if err != nil {
			return nil, err
		}
		cw.AuditCapture(p.ThreadID, cowork.CapLaunchBrowser,
			cowork.Target{Kind: cowork.TargetAny, Label: "browser: " + res.Browser}, dec.GrantID, hashString(res.Browser))
		return map[string]any{"ok": true, "browser": res.Browser, "browsers": res.Browsers, "grantId": dec.GrantID}, nil
	})
}

// resolveTargetWindow returns the KWin window the agent wants to inspect: the one
// matching windowID, or the active window when windowID is empty. ok is false if no
// such window can be found (so the caller can ask for an explicit target).
func resolveTargetWindow(cw *cowork.Service, windowID string) (kde.Window, bool) {
	wins, err := cw.KDE().ListWindows(5 * time.Second)
	if err != nil {
		return kde.Window{}, false
	}
	if windowID != "" {
		for _, w := range wins {
			if w.InternalID == windowID {
				return w, true
			}
		}
		return kde.Window{}, false
	}
	for _, w := range wins {
		if w.Active && !w.Minimized {
			return w, true
		}
	}
	return kde.Window{}, false
}

// elementTarget maps an AT-SPI element's owning pid back to a KWin window so the
// self-target guard (refuse Agent Kate's own UI) and the R2 consent prompt have a
// resourceClass + title. Returns the consent Target and a human label for the element.
func elementTarget(cw *cowork.Service, info kde.ElementContext) (cowork.Target, string) {
	t, label, _, _ := elementTargetWindow(cw, info)
	return t, label
}

// elementTargetWindow is elementTarget plus the resolved owning KWin window (and whether
// one was found) — the pointer-click path needs the window's global origin to translate
// AT-SPI's surface-relative element bounds into global desktop pixels.
func elementTargetWindow(cw *cowork.Service, info kde.ElementContext) (cowork.Target, string, kde.Window, bool) {
	t := cowork.Target{Kind: cowork.TargetApp, Label: "the focused window"}
	var win kde.Window
	found := false
	if info.PID > 0 {
		if wins, err := cw.KDE().ListWindows(4 * time.Second); err == nil {
			for _, w := range wins {
				if w.PID == info.PID {
					t.ResourceClass = w.ResourceClass
					if w.Caption != "" {
						t.Label = w.Caption
					}
					win, found = w, true
					break
				}
			}
		}
	}
	label := strings.TrimSpace(info.Role)
	if info.Name != "" {
		label = strings.TrimSpace(info.Role + " “" + info.Name + "”")
	}
	if label == "" {
		label = "element"
	}
	return t, label, win, found
}

// elementGlobalOffset returns the (ox,oy) to add to an element's AT-SPI coords to get
// global desktop pixels. On Wayland a client cannot know its own global position, so
// AT-SPI reports element bounds relative to the owning window's surface; when the
// element's center falls OUTSIDE the owning window's global rect it is surface-relative
// and we add the window origin. On X11 (already global, center inside the window) the
// offset is zero.
func elementGlobalOffset(rect kde.Rect, win kde.Window, haveWin bool) (int, int) {
	if !haveWin || win.Width <= 0 || win.Height <= 0 {
		return 0, 0
	}
	cx, cy := rect.X+rect.W/2, rect.Y+rect.H/2
	inside := cx >= win.X && cy >= win.Y && cx < win.X+win.Width && cy < win.Y+win.Height
	if inside {
		return 0, 0
	}
	return win.X, win.Y
}

// elementCenterGlobal returns the element's center as a global-desktop-pixel point.
func elementCenterGlobal(rect kde.Rect, win kde.Window, haveWin bool) (int, int) {
	ox, oy := elementGlobalOffset(rect, win, haveWin)
	return rect.X + rect.W/2 + ox, rect.Y + rect.H/2 + oy
}

// anchorPoint returns a point on the element's bounds (in the element's own AT-SPI coord
// space) for the named anchor — center (default), the four corners, the four edge
// midpoints. Combined with elementGlobalOffset + a dx/dy nudge this lets the agent click
// a sub-region (a dropdown arrow at the right edge, just below a label, a drag handle).
func anchorPoint(rect kde.Rect, anchor string) (int, int) {
	cx, cy := rect.X+rect.W/2, rect.Y+rect.H/2
	switch strings.ToLower(strings.TrimSpace(anchor)) {
	case "topleft":
		return rect.X, rect.Y
	case "top":
		return cx, rect.Y
	case "topright":
		return rect.X + rect.W, rect.Y
	case "left":
		return rect.X, cy
	case "right":
		return rect.X + rect.W, cy
	case "bottomleft":
		return rect.X, rect.Y + rect.H
	case "bottom":
		return cx, rect.Y + rect.H
	case "bottomright":
		return rect.X + rect.W, rect.Y + rect.H
	}
	return cx, cy // "center" / default
}

// pointerRef is an optional reference frame for a pointer coordinate: relative to a
// window's top-left, or offset from an element's center. Empty → the coordinate is
// already a global desktop pixel. This lets the agent target points in the frame it
// actually perceives (a window it is looking at, or an element from desktop_list_elements)
// and have the core translate to global — including spots with no element of their own
// (a canvas, a map, a video scrubber).
type pointerRef struct {
	Window  string `json:"window"`
	Element string `json:"element"`
}

// resolveGlobalPoint maps (x,y) in ref's frame to a global desktop point, plus a short
// human description of the frame (for the audit log + consent prompt). A nil/empty ref
// means (x,y) is already global.
func resolveGlobalPoint(cw *cowork.Service, x, y int, ref *pointerRef) (point, string, error) {
	if ref == nil || (ref.Window == "" && ref.Element == "") {
		return point{x, y}, "", nil
	}
	if ref.Element != "" {
		info, rect, ok, err := cw.KDE().ElementBounds(ref.Element, 8*time.Second)
		if err != nil {
			return point{}, "", ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if !ok {
			return point{}, "", ipc.Errorf(ipc.CodeInvalidParams, "the reference element has no on-screen bounds")
		}
		_, _, win, haveWin := elementTargetWindow(cw, info)
		ex, ey := elementCenterGlobal(rect, win, haveWin)
		label := strings.TrimSpace(strings.TrimSpace(info.Role) + " " + info.Name)
		if label == "" {
			label = "element"
		}
		return point{ex + x, ey + y}, fmt.Sprintf(" (rel. to %s)", label), nil
	}
	w, ok := resolveTargetWindow(cw, ref.Window)
	if !ok {
		return point{}, "", ipc.Errorf(ipc.CodeInvalidParams,
			"the reference window was not found (use a windowId from desktop_list_windows)")
	}
	return point{w.X + x, w.Y + y}, " (rel. to window)", nil
}

// requireCoworkBridge enforces: the caller is an agent bridge (not the UI), bound to
// this thread, and the thread has opted into Cowork (08 §B/§C).
func requireCoworkBridge(d handlerDeps, ctx context.Context, threadID string) error {
	if threadID == "" {
		return ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
	}
	if ok, reason := d.srv.BindBridge(ctx, threadID); !ok {
		return ipc.Errorf(codeCoworkDenied, reason)
	}
	rec, ok := d.sessions.Get(threadID)
	if !ok || !rec.CoworkEnabled {
		return ipc.Errorf(codeCoworkDenied, "Cowork is not enabled for this agent thread")
	}
	return nil
}

// requireUI enforces that the caller is the UI (grant responses, kill-switch, etc.).
func requireUI(d handlerDeps, ctx context.Context) error {
	if !d.srv.RequireUI(ctx) {
		return ipc.Errorf(codeCoworkDenied, "this action may only be performed from the Agent Kate window")
	}
	return nil
}

// requirePointerControl is the common front gate for the positioned-pointer handlers:
// the caller is the agent bridge for an opted-in thread and the desktop is reachable.
func requirePointerControl(d handlerDeps, ctx context.Context, threadID string) error {
	if err := requireCoworkBridge(d, ctx, threadID); err != nil {
		return err
	}
	if !d.cowork.Available() {
		return ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
	}
	return nil
}

// pointerTarget builds the consent/audit Target for a coordinate-addressed action. The
// geometric self-target guard (guardPointerTargets), not the Target's class, is what
// protects Agent Kate's own UI here — raw coordinates carry no resourceClass.
func pointerTarget(label string) cowork.Target {
	return cowork.Target{Kind: cowork.TargetScreen, Label: label}
}

// runPointerAction is the shared tail for the positioned-pointer handlers: it gates the
// action under the R2 pointer_control capability, enforces the geometric self-target
// guard against LIVE geometry for each action point, plays the ops through the UI portal,
// and audits the literal target. guardPts is nil for a pure move (which may pass over
// Agent Kate's windows — only a click/scroll on them is dangerous).
func runPointerAction(d handlerDeps, ctx context.Context, threadID string, target cowork.Target, desc string, ops []map[string]any, guardPts []point) (any, error) {
	cw := d.cowork
	dec, err := cw.Authorize(ctx, cowork.AuthRequest{
		ThreadID:       threadID,
		Capability:     cowork.CapPointerControl,
		Target:         target,
		SuggestedScope: cowork.ScopeOnce,
		ActionPreview: &cowork.ActionDescriptor{
			Mechanism:   "pointer_control",
			WindowTitle: target.Label,
			Detail:      desc,
		},
	})
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
	}
	if !dec.Allow {
		return nil, ipc.Errorf(codeCoworkDenied, dec.Reason)
	}
	if len(guardPts) > 0 {
		if err := guardPointerTargets(cw, guardPts); err != nil {
			cw.AuditRefusal(threadID, cowork.CapPointerControl, target, err.Error())
			return nil, ipc.Errorf(codeCoworkDenied, err.Error())
		}
	}
	if _, err := runPortal(d, ctx, "inject", map[string]any{"threadId": threadID, "ops": ops}, 45*time.Second); err != nil {
		return nil, err
	}
	cw.AuditCapture(threadID, cowork.CapPointerControl, target, dec.GrantID, hashString(desc))
	return map[string]any{"ok": true, "action": desc}, nil
}

// runPortal runs the core↔UI portal round-trip and returns the UI's result.
func runPortal(d handlerDeps, ctx context.Context, kind string, payload map[string]any, timeout time.Duration) (kde.PortalResult, error) {
	pb := d.cowork.Portal()
	corrID, ch := pb.Open()
	defer pb.Close(corrID)
	payload["corrId"] = corrID
	payload["kind"] = kind
	if !d.srv.NotifyPrimaryUI("cowork.portalRequest", payload) {
		return kde.PortalResult{}, ipc.Errorf(codeCoworkDenied, "no Agent Kate window is available to run the desktop portal")
	}
	select {
	case r := <-ch:
		if !r.OK {
			return r, ipc.Errorf(ipc.CodeInternalError, "desktop portal failed: "+r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return kde.PortalResult{}, ctx.Err()
	case <-time.After(timeout):
		return kde.PortalResult{}, ipc.Errorf(ipc.CodeInternalError, "desktop portal timed out")
	}
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	return hashBytes(b)
}
func hashString(s string) string { return hashBytes([]byte(s)) }
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
