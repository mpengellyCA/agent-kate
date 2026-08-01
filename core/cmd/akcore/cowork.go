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
	"agentkate/internal/safe"
)

// codeCoworkDenied is returned when a Cowork action is refused (no consent, wrong
// origin, not enabled, kill-switch). The MCP bridge surfaces the message to the agent.
const codeCoworkDenied = -32010

// coworkNotifier adapts ipc.Server to the cowork.Notifier interface so the
// consent authority can push grant prompts / changes to the UI. Both directions
// are exposed because the authority decides per event whether it may be
// broadcast at all — see cowork.Notifier.
type coworkNotifier struct{ srv *ipc.Server }

func (n coworkNotifier) Notify(method string, params any)   { n.srv.Notify(method, params) }
func (n coworkNotifier) NotifyUI(method string, params any) { n.srv.NotifyUI(method, params) }

// screenshotMaxDim caps the longest edge of a returned still (plan 02 §3): keeps the
// base64 image well under the 16 MiB IPC frame cap and within model vision limits.
const screenshotMaxDim = 1568

// registerCoworkHandlers wires the v1 cowork.* RPCs. It is a no-op if the service
// failed to initialize (e.g. no session bus) — the tools then report unavailable.
func registerCoworkHandlers(d handlerDeps) {
	cw := d.cowork
	if cw == nil {
		// The service failed to initialise (e.g. consent-store load error). Register
		// a stand-in cowork.status so the UI's capability probe still gets a reply
		// and can reach its "desktop integration unavailable" state — without this
		// the probe returns method-not-found and the panel stays stuck on its
		// default "checking…" text, silently masking that desktop access is off.
		d.srv.Handle("cowork.status", func(_ context.Context, _ json.RawMessage) (any, error) {
			return map[string]any{"available": false, "killed": false, "tampered": false}, nil
		})
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

	// cowork.setEnabled / cowork.threadState / cowork.preflight live in
	// cowork_enable.go and are registered unconditionally (a bridge must be able
	// to ask for its state even when this service failed to start).

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
				return nil, ipc.Errorf(codeCoworkDenied, bareClickRefusal(pstate.mirrorLoss(p.ThreadID)))
			}
			if err := guardPointerTargets(cw, []point{last}); err != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
					cowork.Target{Kind: cowork.TargetScreen, Label: "bare click at the current pointer"}, err.Error())
				return nil, ipc.Errorf(codeCoworkDenied, err.Error())
			}
		}

		// Resolve — and VERIFY — the window the keystrokes will land on. This is the
		// keyboard analogue of guardPointerTargets and it fails CLOSED: with no
		// targetWindowId the events go to whatever is focused, which may be Agent Kate's
		// own typed-phrase consent dialog, so "the focused window" is resolved for real
		// and refused when it is ours or cannot be identified.
		target, gerr := resolveInjectTarget(cw, p.TargetWindowID)
		if gerr != nil {
			cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
				cowork.Target{Kind: cowork.TargetWindow, WindowID: p.TargetWindowID, Label: "the focused window"},
				gerr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
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

		// Re-assert focus on the VERIFIED window (target.WindowID, which resolveInjectTarget
		// filled in even when the caller passed none) AFTER the consent wait, and refuse the
		// batch outright if that cannot be proven — the authorize above may have blocked on a
		// human for minutes, and typing into "whatever is focused now" is the whole attack.
		// A batch that types nothing keeps the best-effort focus hint: its real guard is the
		// bare-click check above, which is geometric and does not depend on focus at all.
		typing := opsHaveKey(ops)
		if typing {
			if err := focusVerifiedInjectTarget(cw, target); err != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject, target, err.Error())
				return nil, ipc.Errorf(codeCoworkDenied, err.Error())
			}
		} else if target.WindowID != "" {
			if err := cw.KDE().ActivateWindow(target.WindowID, 4*time.Second); err != nil {
				d.log.Warn("cowork: could not focus target window", "id", target.WindowID, "err", err)
			}
		}

		// A batch with delays plays over wall-clock time, so the point-in-time checks above
		// do not cover it: supervise the focused window for the whole span and abort the
		// remainder the moment it changes (audit F3). Establishing the watch is a
		// precondition — if KWin cannot be watched, the timed batch is refused.
		var abort <-chan string
		if typing && opsSpanMs(ops) > 0 {
			watch, ch, werr := startInjectFocusWatch(cw, target)
			if werr != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject, target, werr.Error())
				return nil, ipc.Errorf(codeCoworkDenied, werr.Error())
			}
			defer watch.Stop()
			abort = ch
		}

		// No pointer-mirror bookkeeping here, deliberately: buildInjectOps emits only key
		// and button events (this path fires at the CURRENT cursor by definition), so
		// nothing in this batch — landed, dropped, or abandoned half-way — can move the
		// pointer. The mirror still describes the cursor afterwards.
		if _, err := runPortalAbortable(d, ctx, "inject", map[string]any{
			"threadId": p.ThreadID,
			"ops":      ops,
		}, 35*time.Second, abort); err != nil {
			if abort != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject, target, err.Error())
			}
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
		// The desktop box the compositor clamps the pointer into, so a move_rel event can
		// carry the mirror instead of stranding it (audit F3). Unknown bounds are not a
		// guess: the compiler invalidates the position and any bare button/scroll after a
		// nudge is refused.
		script := timelineScript{Events: p.Events, FPS: p.FPS, Bounds: pstate.desktopBounds(cw)}
		plan, err := buildTimelineOps(script, start, haveStart, resolveProfile, newPointerRNG())
		if err != nil {
			// Script-construction / fail-closed errors (bad schedule, unreleased hold,
			// span overrun, unverifiable bare click) are the caller's mistake.
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}

		// Resolve the target window's class/title so the self-target guard can refuse
		// Agent Kate's own UI and the consent prompt(s) can name the window.
		//
		// A script that types anything gets the same fail-closed keyboard guard as
		// injectInput: keystrokes follow focus, so the focused window must be identified
		// and cleared before any of it is scheduled. A pointer-only script keeps the
		// best-effort lookup — every one of its action points is checked geometrically by
		// guardPointerTargets below, which is the stronger check for that case and does
		// not need a focused window at all.
		var target cowork.Target
		if plan.HasKey {
			var gerr error
			target, gerr = resolveInjectTarget(cw, p.TargetWindowID)
			if gerr != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
					cowork.Target{Kind: cowork.TargetWindow, WindowID: p.TargetWindowID, Label: "the focused window"},
					gerr.Error())
				return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
			}
		} else {
			target = bestEffortWindowTarget(cw, p.TargetWindowID)
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

		// Focus the VERIFIED window first so the keystrokes/clicks land on it.
		//
		// A typing script gets the fail-closed re-verification: target.WindowID is the
		// window resolveInjectTarget cleared, the authorize above may have blocked on a
		// human for minutes, and a focus change since then would send the keys elsewhere —
		// so an unprovable focus refuses the batch rather than warning. A pointer-only
		// script has no keyboard target (target.WindowID may be empty, and every action
		// point is geometrically guarded), so its focus hint stays best-effort.
		if plan.HasKey {
			if err := focusVerifiedInjectTarget(cw, target); err != nil {
				cw.AuditRefusal(p.ThreadID, primaryCap, target, err.Error())
				return nil, ipc.Errorf(codeCoworkDenied, err.Error())
			}
		} else if target.WindowID != "" {
			if err := cw.KDE().ActivateWindow(target.WindowID, 4*time.Second); err != nil {
				d.log.Warn("cowork: could not focus target window", "id", target.WindowID, "err", err)
			}
		}

		// Supervise the focused window for the whole span of a timed TYPING script and
		// abort the remainder on any change (audit F3). A timeline is exactly the case the
		// submit-time checks cannot cover: up to 30s of wall clock with RemoteDesktop
		// injecting throughout. Establishing the watch is a precondition — refuse if KWin
		// cannot be watched.
		var abort <-chan string
		if plan.HasKey && opsSpanMs(plan.Ops) > 0 {
			watch, ch, werr := startInjectFocusWatch(cw, target)
			if werr != nil {
				cw.AuditRefusal(p.ThreadID, primaryCap, target, werr.Error())
				return nil, ipc.Errorf(codeCoworkDenied, werr.Error())
			}
			defer watch.Stop()
			abort = ch
		}

		// 60s: a timeline may legitimately span up to 30s of playback and the UI replies
		// only once the whole stream has drained.
		res, err := runPortalAbortable(d, ctx, "inject", map[string]any{
			"threadId": p.ThreadID,
			"ops":      plan.Ops,
		}, 60*time.Second, abort)
		if err != nil {
			if abort != nil {
				cw.AuditRefusal(p.ThreadID, primaryCap, target, err.Error())
			}
			// The ops reached the portal, so a pointer script may have played in PART: an
			// aborted or failed timeline strands the cursor mid-path (and an interpolated
			// path is allowed to cross Agent Kate's windows). Leaving the pre-script mirror
			// standing is the same bypass as a stale relative nudge — destroy it.
			if plan.HasPointer {
				pstate.invalidate(p.ThreadID, mirrorLostUnproven)
			}
			return nil, err
		}
		// Commit the pointer mirror only when the UI PROVED the batch's last absolute move
		// landed where it was aimed, so a later bare click in another call is verified
		// against where the cursor really is. A script whose relative nudges outran what we
		// can account for — or whose absolute move the desktop could not apply — DESTROYS
		// the mirror instead: leaving the pre-script position standing is the F3 bypass.
		landed := opsLandedAsAimed(plan.Ops, res)
		switch {
		case plan.HaveFinal && landed:
			pstate.setLast(p.ThreadID, plan.FinalPos)
		case plan.HaveFinal:
			pstate.invalidate(p.ThreadID, mirrorLostUnproven)
		case plan.RelLost:
			pstate.invalidate(p.ThreadID, mirrorLostRelative)
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
		res, play, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, nil)
		// The mirror records where the cursor PROVABLY is, never where we asked it to go: a
		// move to a point no captured screen contains is dropped by the UI, and a failed
		// play can strand the cursor mid-path — both leave it somewhere unverified, which
		// must refuse the next bare click rather than clear it against a fiction.
		pstate.commitPointer(p.ThreadID, play, pt, true)
		return res, err
	})

	// Relative pointer motion: raw dx/dy deltas, the input a pointer-grabbing game (mouse-
	// look) reads. Distinct from cowork.movePointer (absolute targeting): no screencast
	// stream and no landing point to self-target-guard. steps>1 splits the delta into
	// smooth, timed sub-nudges.
	//
	// SECURITY (audit F3): the mirror is carried ACROSS the move (applyRelative) rather
	// than left standing. A stale mirror let an agent walk the real cursor onto Agent
	// Kate's own window with move_rel and then fire a bare click the geometric guard
	// cleared against where the cursor used to be. Deltas that cannot be accounted for
	// (unknown desktop bounds, or a walk into the compositor's clamp at a screen edge)
	// destroy the mirror instead, and every guarded bare action then refuses.
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
		// No guard point (there is no absolute landing point to check), but the mirror MUST
		// move with the cursor — see applyRelative. Read the desktop bounds BEFORE playing,
		// so the fail-closed decision does not depend on a KWin round trip taken after the
		// pointer has already moved.
		bounds := pstate.desktopBounds(cw)
		res, play, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, nil)
		if err != nil {
			// A refusal BEFORE the portal ran moved nothing, so the mirror still describes
			// the cursor; once the ops are in the UI's hands a failure may have played part
			// of the stream, and we cannot tell how far it got — so the mirror goes.
			if play.played {
				pstate.invalidate(p.ThreadID, mirrorLostRelative)
			}
			return nil, err
		}
		if !play.landed {
			// The UI could not apply every op, so the delta that actually reached the
			// compositor is not the one we are about to account for. Fail closed.
			pstate.invalidate(p.ThreadID, mirrorLostRelative)
			return res, nil
		}
		if _, known := pstate.applyRelative(p.ThreadID, dx, dy, bounds); !known {
			d.log.Debug("cowork: relative move left the pointer position unverifiable",
				"thread", p.ThreadID, "boundsKnown", bounds.Valid())
		}
		return res, nil
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
		res, play, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, []point{pt})
		pstate.commitPointer(p.ThreadID, play, pt, true)
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
		res, err := runPortal(d, ctx, "inject", map[string]any{"threadId": p.ThreadID, "ops": ops}, 45*time.Second)
		if err != nil {
			// Played (or half-played) and failed: the cursor is somewhere we cannot name.
			pstate.invalidate(p.ThreadID, mirrorLostUnproven)
			return nil, err
		}
		// Same rule as every other absolute action: commit only what the UI proved landed.
		pstate.commitPointer(p.ThreadID, pointerPlay{played: true, landed: opsLandedAsAimed(ops, res)},
			point{cx, cy}, true)
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
			// geometric guard (a scroll on Agent Kate's UI is refused). Fail closed —
			// including when a relative nudge, or an absolute move the desktop could not
			// apply, is what made the position unverifiable.
			last, ok := pstate.last(p.ThreadID)
			if !ok {
				if why := pstate.mirrorLoss(p.ThreadID); why != "" {
					return nil, ipc.Errorf(ipc.CodeInvalidParams, bareClickRefusal(why))
				}
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
		res, play, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, []point{at})
		pstate.commitPointer(p.ThreadID, play, at, true)
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
		res, play, err := runPointerAction(d, ctx, p.ThreadID, pointerTarget(desc), desc, ops, []point{from, to})
		// A drag that fails mid-play is the worst case for a stale mirror: the button may be
		// down and the cursor anywhere between the endpoints. commitPointer destroys it.
		pstate.commitPointer(p.ThreadID, play, to, true)
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
		target, elemLabel, ownWin, haveWin, listErr := elementTargetWindowErr(cw, info)
		// Fail-closed self-target guard BEFORE the prompt: never even offer the human a
		// consent dialog for an action aimed at Agent Kate's own controls.
		if gerr := guardA11yTarget(cw.Authority, info, ownWin, haveWin, listErr); gerr != nil {
			cw.AuditRefusal(p.ThreadID, cowork.CapA11yAction, target, gerr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
		}
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
		target, elemLabel, ownWin, haveWin, listErr := elementTargetWindowErr(cw, info)
		// Same fail-closed self-target guard as activateElement: setting text into Agent
		// Kate's own typed-phrase consent field is the whole attack this refuses.
		if gerr := guardA11yTarget(cw.Authority, info, ownWin, haveWin, listErr); gerr != nil {
			cw.AuditRefusal(p.ThreadID, cowork.CapA11yAction, target, gerr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
		}
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

// bestEffortWindowTarget builds the consent Target for a named window, filling in the
// class/title when KWin can be read. It is NOT a guard: an unreadable window list or an
// unknown id yields a bare, unverified Target. Only paths whose real safety check is
// somewhere else (the geometric guardPointerTargets) may use it; anything that follows
// keyboard focus must use resolveInjectTarget.
func bestEffortWindowTarget(cw *cowork.Service, windowID string) cowork.Target {
	t := cowork.Target{Kind: cowork.TargetWindow, WindowID: windowID, Label: "the focused window"}
	if windowID == "" {
		return t
	}
	wins, err := cw.KDE().ListWindows(4 * time.Second)
	if err != nil {
		return t
	}
	for _, w := range wins {
		if w.InternalID == windowID {
			t.ResourceClass = w.ResourceClass
			if w.Caption != "" {
				t.Label = w.Caption
			}
			break
		}
	}
	return t
}

// focusVerifiedInjectTarget re-establishes — and PROVES — keyboard focus on the window a
// batch was authorized against, immediately before the ops are handed to the UI.
//
// SECURITY (audit F3, residual): resolveInjectTarget runs BEFORE cw.Authorize, which may
// block for up to three minutes waiting on a human. Anything can take focus in that
// window, so the verified target has to be re-asserted afterwards — and a failure to
// re-assert it is FATAL, not a warning. Warning and injecting anyway is precisely the
// "types into whatever is actually focused" hole the guard exists to close.
//
// ActivateWindow only reports that KWin accepted the request, so the focus is then read
// BACK: the active window must be the approved one, and resolveInjectTarget re-runs the
// full self-target check on it. Every failure mode refuses.
func focusVerifiedInjectTarget(cw *cowork.Service, target cowork.Target) error {
	if target.WindowID == "" {
		return fmt.Errorf("refused: the keyboard target could not be identified, so focus cannot be verified before typing")
	}
	if err := cw.KDE().ActivateWindow(target.WindowID, 4*time.Second); err != nil {
		return fmt.Errorf("refused: the approved target window could not be focused (%v) — nothing was typed, because the keystrokes would have gone to whatever is focused instead", err)
	}
	now, err := resolveInjectTarget(cw, "")
	if err != nil {
		return err
	}
	if now.WindowID != target.WindowID {
		return fmt.Errorf("refused: focus is on %q, not the window this action was approved for — nothing was typed", now.Label)
	}
	return nil
}

// startInjectFocusWatch subscribes to KWin's window-activation signal for the duration of
// a TIMED injection and returns a channel carrying the reason the batch must be aborted.
//
// SECURITY (audit F3, main): resolveInjectTarget + focusVerifiedInjectTarget are still
// point-in-time checks, and a script may span up to 30 s (injectMaxSpanMs /
// timelineMaxSpanMs) with RemoteDesktop injecting the whole way. Without this, an agent
// holding the input_inject toggle schedules a delayed phrase + activation, raises a
// consent prompt in parallel, and the playback types into the prompt that stole focus.
//
// It FAILS CLOSED in both directions: the watch must be established before the ops are
// released (the caller denies if this returns an error), and any activation that is not
// the granted window — most sharply, an Agent Kate window — aborts the remainder.
func startInjectFocusWatch(cw *cowork.Service, target cowork.Target) (*kde.ActiveWindowWatch, <-chan string, error) {
	if cw.Authority == nil {
		return nil, nil, fmt.Errorf("refused: the self-target guard is unavailable, so a timed script cannot be supervised")
	}
	w, err := cw.KDE().WatchActiveWindow(4 * time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("refused: the focused window cannot be watched for the duration of this timed script (%v) — send it without delays, or retry", err)
	}
	auth := cw.Authority
	abort := make(chan string, 1)
	safe.Go("cowork.injectFocusWatch", func() {
		for ev := range w.C {
			reason := injectFocusAbortReason(auth, target, ev)
			if reason == "" {
				continue
			}
			select {
			case abort <- reason:
			default:
			}
			return
		}
	})
	return w, abort, nil
}

// injectFocusAbortReason decides whether one activation event invalidates a running
// script. Pure, so the fail-closed matrix is testable without KWin. "" means carry on.
func injectFocusAbortReason(auth *cowork.Authority, target cowork.Target, ev kde.ActiveWindowEvent) string {
	if auth == nil {
		return "aborted: the self-target guard became unavailable while a timed script was playing"
	}
	if ev.Error != "" {
		return "aborted: the focused window could no longer be verified while a timed script was playing"
	}
	// The sharpest case first, and with its own message: our own window taking focus is
	// the self-approval attack, not an ordinary focus change.
	if auth.IsSelfWindow(ev.PID, ev.ResourceClass) {
		return "aborted: an Agent Kate window took focus while a timed script was playing — the agent may not type into its own interface, including its consent prompts"
	}
	if ev.InternalID == "" {
		return "aborted: focus moved to a window that could not be identified while a timed script was playing"
	}
	if ev.InternalID != target.WindowID {
		who := ev.Caption
		if who == "" {
			who = ev.ResourceClass
		}
		if who == "" {
			who = "another window"
		}
		return fmt.Sprintf("aborted: focus moved to %q while a timed script was playing — this action was approved for a different window", who)
	}
	return ""
}

// opsSpanMs is the wall-clock the UI will take to play an op list. op[0]'s delay is
// excluded because the player ignores it (its timer starts at 0). A span of 0 means the
// UI runs the batch synchronously, leaving no window in which focus can move — which is
// why only a non-zero span needs the activation watch.
func opsSpanMs(ops []map[string]any) int {
	total := 0
	for i, op := range ops {
		if i == 0 {
			continue
		}
		total += intOf(op["delayMs"])
	}
	return total
}

// opsHaveKey reports whether an op list types anything. Keystrokes follow focus; pointer
// ops are pinned to coordinates and are covered by guardPointerTargets instead.
func opsHaveKey(ops []map[string]any) bool {
	for _, op := range ops {
		if s, _ := op["t"].(string); s == "key" {
			return true
		}
	}
	return false
}

// resolveInjectTarget is the keyboard analogue of guardPointerTargets: it identifies the
// window synthetic key events will actually reach and refuses when that window is Agent
// Kate's own UI. It exists because keystrokes follow FOCUS, not coordinates — with the
// input_inject toggle pre-authorized, an agent that could type into "whatever is focused"
// could type the typed-phrase consent dialog's phrase and approve its own R2 request.
func resolveInjectTarget(cw *cowork.Service, windowID string) (cowork.Target, error) {
	wins, err := cw.KDE().ListWindows(4 * time.Second)
	return resolveInjectTargetFrom(cw.Authority, wins, err, windowID)
}

// resolveInjectTargetFrom is resolveInjectTarget's pure core (testable without KWin).
// Every failure mode returns an error: this guard FAILS CLOSED, because "we could not
// tell whose window this is" and "it is Agent Kate's consent dialog" are indistinguishable
// from here, and typing blind is precisely the escalation being defended against.
func resolveInjectTargetFrom(auth *cowork.Authority, wins []kde.Window, listErr error, windowID string) (cowork.Target, error) {
	// FAIL CLOSED: no authority object means no self-identity to compare against.
	if auth == nil {
		return cowork.Target{}, fmt.Errorf("refused: the self-target guard is unavailable, so the keyboard target cannot be verified")
	}
	// FAIL CLOSED: without the live window list we cannot tell whether the keystrokes
	// would land in Agent Kate's own consent prompt.
	if listErr != nil {
		return cowork.Target{}, fmt.Errorf("refused: cannot read the window list to verify the keyboard target is not Agent Kate's own UI")
	}
	var win kde.Window
	found := false
	if windowID != "" {
		for _, w := range wins {
			if w.InternalID == windowID {
				win, found = w, true
				break
			}
		}
		if !found {
			return cowork.Target{}, fmt.Errorf("refused: the target window no longer exists — re-list windows with desktop_list_windows")
		}
	} else {
		for _, w := range wins {
			if w.Active && !w.Minimized {
				win, found = w, true
				break
			}
		}
		// FAIL CLOSED: "type into whatever is focused" with no identifiable focused window
		// is exactly the case where a just-raised Agent Kate dialog could swallow the keys.
		if !found {
			return cowork.Target{}, fmt.Errorf("refused: no active window could be identified, so the keyboard target cannot be verified — pass targetWindowId from desktop_list_windows")
		}
	}
	t := cowork.Target{
		Kind:          cowork.TargetWindow,
		WindowID:      win.InternalID,
		ResourceClass: win.ResourceClass,
		Label:         orDefault(win.Caption, "the focused window"),
	}
	// Both kinds of evidence: the PID (decisive even when KWin reports no class) and the
	// class/label the central Authorize gate would use.
	if auth.IsSelfWindow(win.PID, win.ResourceClass) || auth.IsSelfTarget(t) {
		return cowork.Target{}, fmt.Errorf("refused: the keyboard target is an Agent Kate window — the agent may not type into its own interface, including its consent prompts")
	}
	// FAIL CLOSED on NO evidence (audit F3, residual). IsSelfWindow returning false means
	// "no evidence of self", not "verified other" — it is documented that way in
	// consent.go, and guardA11yTarget already refuses its equivalent case. A window KWin
	// reports with neither an owning pid nor a resource class is exactly the shape an
	// unidentifiable Agent Kate dialog would take, so it is not a keyboard target.
	if win.PID <= 0 && win.ResourceClass == "" {
		return cowork.Target{}, fmt.Errorf("refused: the keyboard target window reports neither an owning process nor an application class, so it cannot be verified as not Agent Kate's own UI — re-list windows with desktop_list_windows and name one")
	}
	// A window id is what focusVerifiedInjectTarget re-asserts and proves focus against
	// after consent; without one there is nothing to prove, so refuse here rather than
	// letting the keystrokes fall through to "whatever is focused".
	if t.WindowID == "" {
		return cowork.Target{}, fmt.Errorf("refused: the keyboard target has no window id, so focus cannot be verified before typing — re-list windows with desktop_list_windows")
	}
	return t, nil
}

// guardA11yTarget is the AT-SPI analogue of guardPointerTargets, for the R2 actions
// (activate an element, set its text). Those act on an element id with no coordinate and
// no focus, so neither existing guard covers them: the only identity available is the
// element's owning process plus the KWin window that process owns.
//
// It FAILS CLOSED. Previously the owning class was learned only via a secondary KWin
// lookup, and any failure there left ResourceClass empty — IsSelfTarget then returned
// false and the action proceeded against an unidentified window. Here an unresolvable
// owner is a refusal, and a PID match against Agent Kate's own processes is decisive
// before KWin is consulted at all.
func guardA11yTarget(auth *cowork.Authority, info kde.ElementContext, win kde.Window, haveWin bool, listErr error) error {
	const selfMsg = "refused: that element belongs to Agent Kate — the agent may not drive its own interface, including its consent prompts"
	if auth == nil {
		return fmt.Errorf("refused: the self-target guard is unavailable, so the element's owner cannot be verified")
	}
	// PID first: it comes straight from the AT-SPI context and survives a KWin failure.
	if auth.IsSelfPID(info.PID) {
		return fmt.Errorf("%s", selfMsg)
	}
	if info.PID <= 0 {
		return fmt.Errorf("refused: that element reports no owning process, so it cannot be verified as not part of Agent Kate's own UI")
	}
	if listErr != nil {
		return fmt.Errorf("refused: cannot read the window list to verify that element is not part of Agent Kate's own UI")
	}
	if !haveWin {
		return fmt.Errorf("refused: the window owning that element could not be identified, so it cannot be verified as not Agent Kate's own UI")
	}
	if auth.IsSelfWindow(win.PID, win.ResourceClass) {
		return fmt.Errorf("%s", selfMsg)
	}
	return nil
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
//
// It is deliberately lossy about failure (a missing window just means "no offset"), so it
// is NOT a guard. R2 callers must use elementTargetWindowErr + guardA11yTarget, which can
// tell "no such window" apart from "KWin unreadable" and refuse both.
func elementTargetWindow(cw *cowork.Service, info kde.ElementContext) (cowork.Target, string, kde.Window, bool) {
	t, label, win, found, _ := elementTargetWindowErr(cw, info)
	return t, label, win, found
}

// elementTargetWindowErr is elementTargetWindow that also reports why the owning window
// could not be resolved: a nil error with found==false means "KWin answered and no window
// owns that pid", a non-nil error means "KWin could not be read at all". The R2 a11y guard
// refuses on either, but the distinction belongs in the refusal message.
func elementTargetWindowErr(cw *cowork.Service, info kde.ElementContext) (cowork.Target, string, kde.Window, bool, error) {
	t := cowork.Target{Kind: cowork.TargetApp, Label: "the focused window"}
	var win kde.Window
	found := false
	var listErr error
	if info.PID > 0 {
		var wins []kde.Window
		wins, listErr = cw.KDE().ListWindows(4 * time.Second)
		if listErr == nil {
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
	return t, label, win, found, listErr
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

// requireCoworkBridge enforces: the caller is an agent bridge (not the UI) that has
// already identified for this thread, and the thread has opted into Cowork (08 §B/§C).
// The identity must pre-exist — a desktop call is not a place to acquire one (F13).
func requireCoworkBridge(d handlerDeps, ctx context.Context, threadID string) error {
	if threadID == "" {
		return ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
	}
	if ok, reason := d.srv.RequireBridge(ctx, threadID); !ok {
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
//
// It also reports what became of the ops (pointerPlay), which the caller feeds to
// commitPointer: a refusal before the portal ran leaves the mirror alone, a play that did
// not provably land destroys it, and only a proven landing commits it. The distinction is
// load-bearing — see the "playback evidence" block in cowork_pointer.go.
func runPointerAction(d handlerDeps, ctx context.Context, threadID string, target cowork.Target, desc string, ops []map[string]any, guardPts []point) (any, pointerPlay, error) {
	var play pointerPlay
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
		return nil, play, ipc.Errorf(ipc.CodeInternalError, err.Error())
	}
	if !dec.Allow {
		return nil, play, ipc.Errorf(codeCoworkDenied, dec.Reason)
	}
	if len(guardPts) > 0 {
		if err := guardPointerTargets(cw, guardPts); err != nil {
			cw.AuditRefusal(threadID, cowork.CapPointerControl, target, err.Error())
			return nil, play, ipc.Errorf(codeCoworkDenied, err.Error())
		}
	}
	// From here the ops are in the UI's hands: whatever comes back, the cursor may have
	// moved (a failure can strand it part-way along an interpolated path).
	play.played = true
	res, err := runPortal(d, ctx, "inject", map[string]any{"threadId": threadID, "ops": ops}, 45*time.Second)
	if err != nil {
		return nil, play, err
	}
	play.landed = opsLandedAsAimed(ops, res)
	cw.AuditCapture(threadID, cowork.CapPointerControl, target, dec.GrantID, hashString(desc))
	return map[string]any{"ok": true, "action": desc}, play, nil
}

// runPortal runs the core↔UI portal round-trip and returns the UI's result.
func runPortal(d handlerDeps, ctx context.Context, kind string, payload map[string]any, timeout time.Duration) (kde.PortalResult, error) {
	return runPortalAbortable(d, ctx, kind, payload, timeout, nil)
}

// runPortalAbortable is runPortal plus a supervisor channel. When abort fires, the UI is
// told to cancel the running injection (releasing anything held down and failing every
// batch queued behind it) and the call returns a REFUSAL immediately — it does not wait
// for the UI to confirm, because "we could not reach the player" and "the player kept
// typing" must resolve the same way. Used by the timed-injection focus watch (audit F3).
func runPortalAbortable(d handlerDeps, ctx context.Context, kind string, payload map[string]any,
	timeout time.Duration, abort <-chan string) (kde.PortalResult, error) {
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
	case reason := <-abort:
		d.srv.NotifyPrimaryUI("cowork.portalRequest", map[string]any{
			"kind":   "abortInject",
			"corrId": "",
			"reason": reason,
		})
		return kde.PortalResult{}, ipc.Errorf(codeCoworkDenied, reason)
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
