package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agentkate/internal/cowork"
	"agentkate/internal/ipc"
	"agentkate/internal/kde"
	"agentkate/internal/safe"
)

// codeCoworkDenied is returned when a Cowork action is refused (no consent, wrong
// origin, not enabled, kill-switch). The MCP bridge surfaces the message to the agent.
const codeCoworkDenied = -32010

// codeCoworkBusy is returned when a Cowork action could not take its turn on the SHARED
// cursor within fireWaitMax. It is a distinct code from codeCoworkDenied on purpose: an
// agent that reads contention as a policy refusal changes its target or gives up, when the
// correct response is to retry the identical call (audit F26 / regression A1).
//
// Distinct from codeUIOnly (-32012, handlers.go), which is a PERMANENT refusal of
// a human-window-only RPC. These two were both landed as -32011 by concurrent
// work; the shared number space is cmd/akcore-wide, so keep new codes unique.
const codeCoworkBusy = -32011

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

// coworkRegistrar registers one cowork.* RPC — and registers it whether or not the desktop
// service exists.
//
// SECURITY (audit F34/F36 inventory, plan 29): registration used to return early when the
// service was absent (no KDE session bus, or a consent store that failed to load), so on
// every machine without a session bus — which is every CI runner and every test process —
// the whole cowork family was simply not in the registry. The handler-inventory test
// enumerates the registry, so it could not see one of them, and a new UNGATED cowork
// handler could not break the build: the one failure mode that inventory exists to catch.
//
// Now the method names are always there. Without a service they answer a clean
// "unavailable" — but only AFTER the same caller gate the live handler carries, so a UI-only
// RPC is still refused to an agent bridge with the UI-only refusal, and the inventory test
// checks the real gate rather than a placeholder.
type coworkRegistrar struct {
	d       handlerDeps
	present bool
}

// agent registers an RPC an agent's own bridge may call (its gate is the per-thread Cowork
// enablement plus a human grant, inside the handler).
func (r coworkRegistrar) agent(name string, h ipc.Handler) {
	if r.present {
		r.d.srv.Handle(name, h)
		return
	}
	r.d.srv.Handle(name, func(context.Context, json.RawMessage) (any, error) {
		return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
	})
}

// ui registers an RPC only the human's own window may drive. The gate is applied by the
// stand-in too, so "unavailable" can never be a way to learn whether a UI-only RPC exists.
func (r coworkRegistrar) ui(name string, h ipc.Handler) {
	if r.present {
		r.d.srv.Handle(name, h)
		return
	}
	r.d.srv.Handle(name, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUI(r.d, ctx); err != nil {
			return nil, err
		}
		return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
	})
}

// probe registers an RPC that must answer even with no service behind it — the UI's
// capability probe, which has to be able to report "unavailable" as a RESULT.
func (r coworkRegistrar) probe(name string, live, absent ipc.Handler) {
	if r.present {
		r.d.srv.Handle(name, live)
		return
	}
	r.d.srv.Handle(name, absent)
}

// registerCoworkHandlers wires the v1 cowork.* RPCs. Every method name is registered
// unconditionally (see coworkRegistrar); the handlers themselves only run when the service
// initialised.
func registerCoworkHandlers(d handlerDeps) {
	cw := d.cowork
	reg := coworkRegistrar{d: d, present: cw != nil}

	// Per-thread pointer profiles + last-position mirror + the user's speed/accuracy
	// bounds, shared by the positioned-pointer handlers below (plan 09 §4).
	pstate := newPointerState()

	// --- capability RPCs (agent bridge → core; consent-gated) ------------------

	reg.agent("cowork.listWindows", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.agent("cowork.screenshot", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		if !cw.Available() {
			return nil, ipc.Errorf(codeCoworkDenied, "desktop integration unavailable (no KDE session bus)")
		}
		if p.Target.Kind == "" {
			p.Target = cowork.Target{Kind: cowork.TargetScreen, Label: "active screen"}
		}
		// Resolve AND clear what is about to be captured, BEFORE the prompt — the same fused
		// decision the a11y read path uses (audit F35, plan 29). A Target carries no PID and
		// the agent writes its ResourceClass itself, so Authorize's IsSelfTarget could be
		// walked straight past by naming a window id with a blank or borrowed class. Here the
		// window id is resolved against LIVE KWin data and its owner checked.
		wins, listErr := cw.KDE().ListWindows(5 * time.Second)
		shot, gerr := resolveCaptureTarget(cw.Authority, wins, listErr, p.Target)
		if gerr != nil {
			if errors.Is(gerr, errNoCaptureTarget) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, gerr.Error())
			}
			cw.AuditRefusal(p.ThreadID, cowork.CapScreenshot, shot.Target, gerr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
		}
		dec, err := cw.Authorize(ctx, cowork.AuthRequest{
			ThreadID: p.ThreadID, Capability: cowork.CapScreenshot,
			Target: shot.Target, SuggestedScope: cowork.ScopeOnce,
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
			"target":      shot.Target,
			"maxDim":      maxDim,
			"format":      format,
			"interactive": p.Interactive,
			// The Agent Kate rectangles this frame must not show, in absolute desktop pixels.
			// See resolveCaptureTarget: the core cannot black them out itself (the frame only
			// ever exists in the UI process, and the core is not told the captured region's
			// origin or pre-scale size), so this is the query the capture pipeline needs and
			// the honest state of the gap is recorded there.
			"redactRects": shot.RedactRects,
		}, 125*time.Second)
		if err != nil {
			return nil, err
		}
		cw.AuditCapture(p.ThreadID, cowork.CapScreenshot, shot.Target, dec.GrantID, hashString(res.PNGB64))
		return map[string]any{
			"pngB64": res.PNGB64, "mime": res.Mime,
			"width": res.Width, "height": res.Height, "grantId": dec.GrantID,
		}, nil
	})

	// --- UI-only RPCs (the agent can never invoke these — origin checked) -------

	reg.ui("cowork.respondGrant", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.requestGrant", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.listGrants", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.revokeGrant", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.killSwitch", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.listAudit", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.portalResult", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	// cowork.status is the one member of the family whose stand-in answers with DATA rather
	// than a refusal: it is the UI's capability probe, and a method-not-found (or an error)
	// leaves the panel stuck on its default "checking…" text, silently masking that desktop
	// access is off.
	reg.probe("cowork.status", func(ctx context.Context, raw json.RawMessage) (any, error) {
		// Readable by the UI; reports the capability probe + kill state.
		_, killed := cw.ListGrants("")
		return map[string]any{"available": cw.Available(), "killed": killed, "tampered": cw.Tampered()}, nil
	}, func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"available": false, "killed": false, "tampered": false}, nil
	})

	// --- global capability policy (the toggle switchboard, Phase 2) — UI-only -----

	reg.ui("cowork.getPolicy", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.setPolicy", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.agent("cowork.injectInput", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		// position is unknown (no positioned move yet), refuse and steer the agent to the
		// guarded desktop_click(x,y).
		//
		// This early copy is ADVISORY and deliberately runs outside the cursor section: it
		// exists so the human is never prompted for an action that is going to be refused
		// anyway. The AUTHORITATIVE check is the one below, taken inside the guard→fire
		// section immediately before the ops are released (audit F26) — which is why it,
		// and not this one, is the caller of guardPointerTargets.
		bareClick := injectHasButton(p.Events)
		var bareAt point
		if bareClick {
			last, ok := pstate.last()
			if !ok {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
					cowork.Target{Kind: cowork.TargetScreen, Label: "bare click at an unverified pointer position"},
					"refused: cannot verify where a bare click would land")
				return nil, ipc.Errorf(codeCoworkDenied, bareClickRefusal(pstate.mirrorLoss()))
			}
			bareAt = last
			if err := pointerTargetsClear(coworkGeometry{cw}, []point{last}); err != nil {
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

		// A typing batch is supervised from the moment its ops are released until the portal
		// replies, and the remainder is aborted the moment focus changes (audit F3).
		//
		// SECURITY (audit F50): EVERY typing batch is supervised, span-0 ones included. A
		// span-0 batch is played synchronously by the UI, but on first use it can sit QUEUED
		// behind the RemoteDesktop handshake — by the time it plays, the focus
		// re-verification above is stale, which is the same hole the watch closes for timed
		// batches. startInjectSupervision picks the MECHANISM by span (a resident KWin
		// activation watch for a timed batch, polling for an immediate one); what is
		// supervised is decided by `typing` alone, never by the span.
		stop, abort, werr := startInjectSupervision(cw, target, typing, opsSpanMs(ops))
		if werr != nil {
			cw.AuditRefusal(p.ThreadID, cowork.CapInputInject, target, werr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, werr.Error())
		}
		defer stop()

		// No pointer-mirror bookkeeping on either branch, deliberately: buildInjectOps emits
		// only key and button events (this path fires at the CURRENT cursor by definition),
		// so nothing in this batch — landed, dropped, or abandoned half-way — can move the
		// pointer. The mirror still describes the cursor afterwards.
		fire := func(hold *cursorHold, ops []map[string]any) (bool, error) {
			var err error
			if bareClick {
				_, err = runCursorPortal(d, ctx, hold, p.ThreadID, ops, 35*time.Second, abort)
			} else {
				_, err = runPortalAbortable(d, ctx, "inject", map[string]any{
					"threadId": p.ThreadID,
					"ops":      ops,
				}, 35*time.Second, abort)
			}
			if err != nil && abort != nil {
				cw.AuditRefusal(p.ThreadID, cowork.CapInputInject, target, err.Error())
			}
			return false, err
		}

		// SECURITY (audit F26): the bare-click guard is re-run here, inside the guard→fire
		// section, against the LIVE global mirror — the advisory check above happened before
		// a consent wait that another thread's pointer action may have crossed. Nothing else
		// may touch the cursor between this check and the portal's reply. A keys-only batch
		// takes NO section: it cannot move the pointer and its guard does not read the
		// mirror, so making it hold the shared cursor would stall every other agent for the
		// length of the typing (regression A1).
		//
		// The section derives the guard point from `ops` themselves (a bare button acts at
		// the mirror), and bareAt — the position the advisory check above cleared and the
		// human was told about — is re-proven before it does. Neither is a closure this
		// handler could quietly stop passing: without ops there is nothing to fire, and
		// without the seed a stream that acts at the cursor is refused (audit F25/F26 wiring).
		if bareClick {
			if err := (cursorAction{
				Geometry: coworkGeometry{cw},
				Ops:      ops,
				Seed:     &bareAt,
				Fire:     fire,
				Refused: func(err error) {
					cw.AuditRefusal(p.ThreadID, cowork.CapInputInject,
						cowork.Target{Kind: cowork.TargetScreen, Label: "bare click at the current pointer"}, err.Error())
				},
			}).run(ctx, pstate, p.ThreadID); err != nil {
				return nil, cursorActionError(err)
			}
		} else if _, err := fire(nil, ops); err != nil {
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

	reg.agent("cowork.playInput", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		start, haveStart := pstate.last()
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

		// fire focuses the target, establishes the supervisor, releases the ops, and reports
		// whether the UI PROVED the batch's last absolute move landed where it was aimed. A
		// pointer-bearing script runs it inside the cursor section (below), which is what
		// makes runCursorPortal's hold available; a keyboard-only one runs it directly.
		fire := func(hold *cursorHold, ops []map[string]any) (bool, error) {
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
					return false, ipc.Errorf(codeCoworkDenied, err.Error())
				}
			} else if target.WindowID != "" {
				if err := cw.KDE().ActivateWindow(target.WindowID, 4*time.Second); err != nil {
					d.log.Warn("cowork: could not focus target window", "id", target.WindowID, "err", err)
				}
			}

			// Supervise the focused window for the whole span of a TYPING script and abort the
			// remainder on any change (audit F3). A timeline is exactly the case the submit-time
			// checks cannot cover: up to 30s of wall clock with RemoteDesktop injecting
			// throughout. As on the injectInput path a span-0 script is supervised too — it can
			// still wait behind the portal handshake before it plays (audit F50) — and the span
			// picks only the MECHANISM, never whether to supervise at all.
			stop, abort, werr := startInjectSupervision(cw, target, plan.HasKey, opsSpanMs(plan.Ops))
			if werr != nil {
				cw.AuditRefusal(p.ThreadID, primaryCap, target, werr.Error())
				return false, ipc.Errorf(codeCoworkDenied, werr.Error())
			}
			defer stop()

			// 60s: a timeline may legitimately span up to 30s of playback and the UI replies
			// only once the whole stream has drained.
			var res kde.PortalResult
			var err error
			if plan.HasPointer {
				res, err = runCursorPortal(d, ctx, hold, p.ThreadID, ops, 60*time.Second, abort)
			} else {
				res, err = runPortalAbortable(d, ctx, "inject", map[string]any{
					"threadId": p.ThreadID,
					"ops":      ops,
				}, 60*time.Second, abort)
			}
			if err != nil {
				if abort != nil {
					cw.AuditRefusal(p.ThreadID, primaryCap, target, err.Error())
				}
				return false, err
			}
			return opsLandedAsAimed(ops, res), nil
		}

		// SECURITY (audit F26): everything from the guard to the portal's reply — including
		// the mirror commit at the end — is one critical section over the shared cursor, so
		// no other thread can move the pointer between the check and the use. Entered AFTER
		// the Authorize calls above, which can wait minutes on a human, and with a bounded,
		// cancellable wait so a long script cannot park every other agent (regression A1).
		// A keyboard-only script takes NO section: it cannot move the cursor, so holding it
		// for the length of the typing would be pure collateral.
		if plan.HasPointer {
			// A script whose guard evidence is the mirror itself (a bare button/scroll before
			// it commanded any absolute motion) is only meaningful while the mirror still
			// holds the position it was compiled against — and the consent wait above is
			// exactly where another thread's pointer action lands.
			var seed *point
			if plan.UsedSeedPos {
				seed = &start
			}
			if err := (cursorAction{
				Geometry: coworkGeometry{cw},
				// Submit-time self-target guard over every point the compiled stream acts at,
				// derived from that stream. Per plan §3 this is a submit-time check: we rely on
				// the bounded (≤30s) span rather than re-checking each op at its fire-time — and
				// on the compiler's no-overlap invariant (audit F25), which is what makes each
				// derived point the place its own event really fires. The compiler cross-checks
				// its own GuardPts against the same derivation and refuses on disagreement.
				Ops:    plan.Ops,
				Seed:   seed,
				Bounds: script.Bounds,
				Fire:   fire,
				// Commit the pointer mirror only when the UI PROVED the batch's last absolute
				// move landed where it was aimed, so a later bare click in another call is
				// verified against where the cursor really is. A script whose relative nudges
				// outran what we can account for — or whose absolute move the desktop could not
				// apply — DESTROYS the mirror instead: leaving the pre-script position standing
				// is the F3 bypass. Still inside the section.
				Commit: func(play pointerPlay) {
					switch {
					case !play.played:
						// Refused before the portal ran: nothing moved, the mirror stands.
					case !play.landed:
						// The ops reached the portal, so the script may have played in PART: an
						// aborted or failed timeline strands the cursor mid-path (and an
						// interpolated path is allowed to cross Agent Kate's windows).
						_, lostWhy := playMirrorOutcome(plan, false)
						pstate.invalidate(p.ThreadID, orDefault(lostWhy, mirrorLostUnproven))
					default:
						if commit, lostWhy := playMirrorOutcome(plan, true); commit {
							pstate.setLast(p.ThreadID, plan.FinalPos)
						} else if lostWhy != "" {
							pstate.invalidate(p.ThreadID, lostWhy)
						}
					}
				},
				Refused: func(err error) { cw.AuditRefusal(p.ThreadID, primaryCap, target, err.Error()) },
			}).run(ctx, pstate, p.ThreadID); err != nil {
				return nil, cursorActionError(err)
			}
		} else if _, err := fire(nil, plan.Ops); err != nil {
			// A script that commanded no pointer at all writes nothing to the mirror (audit
			// F26): its FinalPos is merely the seed it compiled against.
			return nil, err
		}
		cw.AuditCapture(p.ThreadID, primaryCap, target, primaryGrant, hashString(plan.Desc))
		return map[string]any{"ok": true, "actions": plan.Desc}, nil
	})

	// --- control: positioned pointer (move/click/scroll/drag) (R2) ----------------
	// All of these gate on the single pointer_control capability and route through the
	// same move+notify ops the UI plays. Coordinates are absolute desktop pixels.

	reg.agent("cowork.movePointer", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		ops := pstate.moveOps(pt.X, pt.Y, prof, newPointerRNG())
		desc := fmt.Sprintf("move pointer to (%d,%d)%s", pt.X, pt.Y, frame)
		// Move-only may pass over Agent Kate's windows (motion has no side effect); only
		// a click/scroll on them is refused — so no geometric guard here.
		//
		// The mirror records where the cursor PROVABLY is, never where we asked it to go: a
		// move to a point no captured screen contains is dropped by the UI, and a failed
		// play can strand the cursor mid-path — both leave it somewhere unverified, which
		// must refuse the next bare click rather than clear it against a fiction.
		return runPointerAction(d, ctx, pstate, p.ThreadID, pointerTarget(desc), desc, ops, nil,
			func(play pointerPlay) { pstate.commitPointer(p.ThreadID, play, pt, true) })
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
	reg.agent("cowork.movePointerRelative", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		// The mirror update runs under the fire lock (audit F26), like every other pointer
		// action: a refusal BEFORE the portal ran moved nothing, so the mirror still
		// describes the cursor; once the ops are in the UI's hands a failure may have played
		// part of the stream and we cannot tell how far it got — so the mirror goes.
		return runPointerAction(d, ctx, pstate, p.ThreadID, pointerTarget(desc), desc, ops, nil,
			func(play pointerPlay) {
				switch {
				case !play.played:
					return
				case !play.landed:
					// The UI could not apply every op, so the delta that actually reached the
					// compositor is not the one we would be accounting for. Fail closed.
					pstate.invalidate(p.ThreadID, mirrorLostRelative)
				default:
					if _, known := pstate.applyRelative(p.ThreadID, dx, dy, bounds); !known {
						d.log.Debug("cowork: relative move left the pointer position unverifiable",
							"thread", p.ThreadID, "boundsKnown", bounds.Valid())
					}
				}
			})
	})

	reg.agent("cowork.pointerClick", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		ops := clickOps(pstate.moveOps(pt.X, pt.Y, prof, newPointerRNG()), code, count, prof.SettleMs)
		click := buttonName(code) + "-click"
		if count == 2 {
			click = "double " + click
		} else if count > 2 {
			click = fmt.Sprintf("%d× %s", count, click)
		}
		desc := fmt.Sprintf("%s at (%d,%d)%s", click, pt.X, pt.Y, frame)
		return runPointerAction(d, ctx, pstate, p.ThreadID, pointerTarget(desc), desc, ops, nil,
			func(play pointerPlay) { pstate.commitPointer(p.ThreadID, play, pt, true) })
	})

	reg.agent("cowork.pointerClickElement", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		ops := clickOps(pstate.moveOps(cx, cy, prof, newPointerRNG()), code, 1, prof.SettleMs)
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
		// SECURITY (audit F26): guard → fire is one critical section over the shared cursor,
		// entered with a bounded, cancellable wait (regression A1).
		if err := (cursorAction{
			Geometry: coworkGeometry{cw},
			Ops:      ops,
			Fire: func(hold *cursorHold, ops []map[string]any) (bool, error) {
				res, err := runCursorPortal(d, ctx, hold, p.ThreadID, ops, 45*time.Second, nil)
				if err != nil {
					return false, err
				}
				return opsLandedAsAimed(ops, res), nil
			},
			// Same rule as every other absolute action: commit only what the UI proved
			// landed; a play that failed part-way leaves the cursor somewhere unnameable.
			Commit:  func(play pointerPlay) { pstate.commitPointer(p.ThreadID, play, point{cx, cy}, true) },
			Refused: func(err error) { cw.AuditRefusal(p.ThreadID, cowork.CapPointerControl, target, err.Error()) },
		}).run(ctx, pstate, p.ThreadID); err != nil {
			return nil, cursorActionError(err)
		}
		cw.AuditCapture(p.ThreadID, cowork.CapPointerControl, target, dec.GrantID, hashString(desc))
		return map[string]any{"ok": true, "action": desc, "element": elemLabel}, nil
	})

	reg.agent("cowork.scroll", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		// seed is set only on the bare branch: there the guard point IS the mirror, which is
		// global and may move while this call waits on consent (audit F26).
		var seed *point
		switch {
		case p.X != nil && p.Y != nil:
			pt, _, err := resolveGlobalPoint(cw, *p.X, *p.Y, p.RelativeTo)
			if err != nil {
				return nil, err
			}
			at = pt
			prof := pstate.resolve(p.ThreadID, nil)
			ops = append(ops, pstate.moveOps(at.X, at.Y, prof, newPointerRNG())...)
		default:
			// Scroll lands at the current pointer position; we must know it to run the
			// geometric guard (a scroll on Agent Kate's UI is refused). Fail closed —
			// including when a relative nudge, or an absolute move the desktop could not
			// apply, is what made the position unverifiable.
			last, ok := pstate.last()
			if !ok {
				if why := pstate.mirrorLoss(); why != "" {
					return nil, ipc.Errorf(ipc.CodeInvalidParams, bareClickRefusal(why))
				}
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"pass x,y (or move the pointer first) so the scroll location can be verified")
			}
			at = last
			seed = &last
		}
		if p.DY != 0 {
			ops = append(ops, scrollOp(0, p.DY))
		}
		if p.DX != 0 {
			ops = append(ops, scrollOp(1, p.DX))
		}
		desc := fmt.Sprintf("scroll vertical %d / horizontal %d notches at (%d,%d)", p.DY, p.DX, at.X, at.Y)
		return runPointerAction(d, ctx, pstate, p.ThreadID, pointerTarget(desc), desc, ops, seed,
			func(play pointerPlay) { pstate.commitPointer(p.ThreadID, play, at, true) })
	})

	reg.agent("cowork.pointerDrag", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		ops := pstate.dragOps(from, to, prof, newPointerRNG())
		desc := fmt.Sprintf("drag from (%d,%d) to (%d,%d)%s", from.X, from.Y, to.X, to.Y, frame)
		// A drag that fails mid-play is the worst case for a stale mirror: the button may be
		// down and the cursor anywhere between the endpoints. commitPointer destroys it.
		return runPointerAction(d, ctx, pstate, p.ThreadID, pointerTarget(desc), desc, ops, nil,
			func(play pointerPlay) { pstate.commitPointer(p.ThreadID, play, to, true) })
	})

	reg.agent("cowork.setPointerProfile", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.ui("cowork.setPointerBounds", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.agent("cowork.listElements", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		// Resolve AND clear the target in one decision, BEFORE the prompt, exactly as on the
		// R2 action path: reading our own UI is refused too (audit F50).
		wins, listErr := cw.KDE().ListWindows(5 * time.Second)
		win, target, gerr := resolveA11yReadWindow(cw.Authority, wins, listErr, p.TargetWindowID)
		if gerr != nil {
			if errors.Is(gerr, errNoA11yTarget) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, gerr.Error())
			}
			cw.AuditRefusal(p.ThreadID, cowork.CapA11yRead, target, gerr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
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

	reg.agent("cowork.readText", func(ctx context.Context, raw json.RawMessage) (any, error) {
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
		// Same fused resolve+guard as listElements: the exact bounds and labels of our own
		// consent dialog and policy toggles are the targeting data an injection attack needs
		// (audit F50), and no legitimate agent use for reading our own UI exists.
		wins, listErr := cw.KDE().ListWindows(5 * time.Second)
		win, target, gerr := resolveA11yReadWindow(cw.Authority, wins, listErr, p.TargetWindowID)
		if gerr != nil {
			if errors.Is(gerr, errNoA11yTarget) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, gerr.Error())
			}
			cw.AuditRefusal(p.ThreadID, cowork.CapA11yRead, target, gerr.Error())
			return nil, ipc.Errorf(codeCoworkDenied, gerr.Error())
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

	reg.agent("cowork.activateElement", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.agent("cowork.setElementText", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

	reg.agent("cowork.launchBrowser", func(ctx context.Context, raw json.RawMessage) (any, error) {
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

// errNoA11yTarget marks "there is no window to read here" — a caller mistake (bad/absent
// targetWindowId, no focused window, KWin unreadable), not a policy refusal. The handlers
// map it to invalid-params and everything else from resolveA11yReadWindow to a denial.
var errNoA11yTarget = errors.New("no target window")

// resolveA11yReadWindow resolves the window an a11y READ (desktop_list_elements /
// desktop_read_text) is aimed at AND clears it through the self-target read guard, in ONE
// place that both read handlers call.
//
// SECURITY (audit F50): resolution and guard are deliberately fused. When each handler
// resolved a window and then separately called the guard, the only thing pinning the guard
// to the handlers was a source scan for the call — which cannot tell a real call from one
// rewritten to fail open. Here there is a single decision, and it is pure (the caller hands
// in the KWin snapshot), so the refuse/allow matrix is driven directly by a test.
//
// It FAILS CLOSED at every step: an unreadable window list, an unknown id, no identifiable
// focused window, a window that reports neither owner nor class, and any Agent Kate window
// all refuse rather than read. The Target is returned even on a refusal so the caller can
// audit what was refused.
func resolveA11yReadWindow(auth *cowork.Authority, wins []kde.Window, listErr error, windowID string) (kde.Window, cowork.Target, error) {
	if listErr != nil {
		return kde.Window{}, cowork.Target{}, fmt.Errorf("%w: the window list could not be read, so no window can be verified as not Agent Kate's own UI", errNoA11yTarget)
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
	} else {
		for _, w := range wins {
			if w.Active && !w.Minimized {
				win, found = w, true
				break
			}
		}
	}
	if !found {
		return kde.Window{}, cowork.Target{}, fmt.Errorf("%w: pass targetWindowId (from desktop_list_windows), or focus a window first", errNoA11yTarget)
	}
	target := cowork.Target{
		Kind: cowork.TargetWindow, WindowID: win.InternalID,
		ResourceClass: win.ResourceClass, Label: orDefault(win.Caption, win.ResourceClass),
	}
	if err := guardA11yReadWindow(auth, win); err != nil {
		return win, target, err
	}
	return win, target, nil
}

// errNoCaptureTarget marks "there is no such window to capture" — a stale or unknown
// windowId, which is a caller mistake rather than a policy refusal. The screenshot handler
// maps it to invalid-params and everything else from resolveCaptureTarget to a denial.
var errNoCaptureTarget = errors.New("no window to capture")

// captureDecision is what one capture request resolved to: the Target the consent prompt
// and the audit line name (described from LIVE compositor data, never from what the agent
// claimed), plus the Agent Kate rectangles that must not appear in the returned pixels.
type captureDecision struct {
	Target cowork.Target
	// RedactRects are our own windows, in absolute desktop pixels, for a WHOLE-FRAME capture.
	RedactRects []cowork.WindowRect
}

// resolveCaptureTarget resolves what a desktop_screenshot is about to capture and clears it
// through the self-target guard, in ONE decision the handler calls — the same fused shape as
// resolveA11yReadWindow, and for the same reason: a resolution and a guard that are separate
// statements can only be pinned by reading the source, which cannot tell a real call from
// one rewritten to fail open.
//
// SECURITY (audit F35, plan 29): the refusal in Authorize inspects only Target.ResourceClass
// and Target.Label — both written by the AGENT — and a Target carries no PID at all. So a
// capture that named one of our windows by id with a blank or borrowed class walked straight
// through a check the code claimed refused it. Here the window id is looked up in the live
// KWin list and cleared by the owner-and-class matrix every other guard uses
// (guardA11yReadWindow: PID first, class second, and no evidence at all is a refusal).
//
// The whole-frame case (no window named — the default) is NOT closed by any refusal, and
// this function is deliberate about that: our windows are in those pixels and the only
// enforceable answer is to black them out where the frame exists, which is the UI's capture
// pipeline. What is enforced here is the precondition — the rectangles are computed, and a
// capture whose frame we cannot even enumerate is refused rather than taken blind. The
// remaining exposure, in plain words: a full-screen screenshot still shows whatever of Agent
// Kate is on screen. What that does NOT give an agent is a way to act on us — every click,
// keystroke, a11y read and a11y action against our own windows is refused by identity, not
// by obscurity — and the sharpest reading, the consent dialog the human is answering right
// now, is refused by the prompt-pending rule in Authorize.
func resolveCaptureTarget(auth *cowork.Authority, wins []kde.Window, listErr error, t cowork.Target) (captureDecision, error) {
	out := captureDecision{Target: t}
	if auth == nil {
		return out, fmt.Errorf("refused: the self-target guard is unavailable, so what would be captured cannot be verified as not Agent Kate's own UI")
	}
	// FAIL CLOSED: without the live window list neither the named window's owner nor the set
	// of our windows inside a full frame can be established.
	if listErr != nil {
		return out, fmt.Errorf("refused: the window list could not be read, so nothing in this frame can be verified as not Agent Kate's own interface")
	}

	if t.Kind == cowork.TargetWindow {
		if t.WindowID == "" {
			return out, fmt.Errorf("%w: pass a windowId from desktop_list_windows, or omit the target to capture the screen", errNoCaptureTarget)
		}
		var win kde.Window
		found := false
		for _, w := range wins {
			if w.InternalID == t.WindowID {
				win, found = w, true
				break
			}
		}
		if !found {
			return out, fmt.Errorf("%w: that window no longer exists — re-list windows with desktop_list_windows", errNoCaptureTarget)
		}
		// Describe it from what the compositor says, not from what the caller wrote: the
		// consent prompt and the audit line must name the window that will really be captured.
		out.Target.ResourceClass = win.ResourceClass
		out.Target.Label = orDefault(win.Caption, orDefault(win.ResourceClass, t.Label))
		// A screenshot of a window IS a read of it, so it takes the read guard verbatim —
		// one matrix, no second copy to drift out of step with it.
		if err := guardA11yReadWindow(auth, win); err != nil {
			return out, err
		}
		return out, nil
	}

	// Whole-frame capture: nothing is named, so nothing can be refused by name.
	rects := make([]cowork.WindowRect, 0, len(wins))
	for _, w := range wins {
		rects = append(rects, cowork.WindowRect{
			X: w.X, Y: w.Y, W: w.Width, H: w.Height, PID: w.PID, ResourceClass: w.ResourceClass,
		})
	}
	out.RedactRects = auth.SelfWindowRects(rects)
	// Say so in the prompt. The human approving "capture the screen" should be told when the
	// frame will include Agent Kate itself — this is the one place that fact is known, and
	// the Label reaches both the consent dialog and the activity log. (Label plays no part in
	// grant matching, so this does not fragment a session grant.)
	if len(out.RedactRects) > 0 {
		out.Target.Label = orDefault(t.Label, "the screen") + " — includes the Agent Kate window"
	}
	return out, nil
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
		// SECURITY (audit F50, regression A3): the advice has to match the gate. A delay-free
		// batch is supervised by POLLING (startInjectFocusPoll), which needs no activation
		// signal, so "send it without delays" is a real, different shape that can succeed —
		// telling the agent to retry the identical timed script would only loop it.
		return nil, nil, fmt.Errorf("refused: the focused window cannot be watched for the duration of this timed script (%v) — send the same keystrokes WITHOUT delays (an immediate batch is supervised without KWin's activation signal), or retry once the compositor is responsive again", err)
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

// How a typing batch is supervised between the moment its ops are released and the portal's
// reply. The choice is about MECHANISM only — see injectSupervisorKind.
const (
	supervisorNone  = "none"
	supervisorWatch = "watch"
	supervisorPoll  = "poll"
)

// injectFocusPollInterval is how often the poll supervisor re-reads the focused window. It
// is the fallback mechanism, so it trades a little KWin traffic for not needing a resident
// script; at 200 ms an immediate batch on a healthy desktop (portal replies in ms) usually
// costs zero KWin calls, while a batch stuck behind the RemoteDesktop handshake — which
// takes seconds — gets many.
const injectFocusPollInterval = 200 * time.Millisecond

// injectSupervisorKind decides HOW one batch is supervised.
//
// SECURITY (audit F50): WHETHER a batch is supervised depends on `typing` alone. The span
// used to gate that, which left a span-0 typing batch queued behind the RemoteDesktop
// handshake playing unsupervised against a stale focus check. It now only picks the
// mechanism — so reintroducing a span condition here, by any spelling, makes an immediate
// typing batch return supervisorNone and fails the test that pins this.
//
// Why two mechanisms (regression A2): making EVERY keystroke install and tear down a
// resident kde.ActiveWindowWatch put a single keypress at the mercy of KWin's
// window-activation signal — a capability plain typing never needed — and refused the batch
// outright when it was missing. A timed script still uses the watch (it is the right tool
// for 30 s of wall clock, and that path already required it before F50). An IMMEDIATE batch
// polls the window list instead: the same primitive focusVerifiedInjectTarget just used
// successfully two lines earlier, so it adds no new failure mode and cannot fail to be
// established.
func injectSupervisorKind(typing bool, spanMs int) string {
	if !typing {
		return supervisorNone
	}
	if spanMs > 0 {
		return supervisorWatch
	}
	return supervisorPoll
}

// startInjectSupervision establishes the supervisor injectSupervisorKind selected and
// returns its stop func plus the abort channel to hand runPortalAbortable. stop is always
// non-nil; abort is nil only for a batch that types nothing.
func startInjectSupervision(cw *cowork.Service, target cowork.Target, typing bool, spanMs int) (func(), <-chan string, error) {
	switch injectSupervisorKind(typing, spanMs) {
	case supervisorWatch:
		w, ch, err := startInjectFocusWatch(cw, target)
		if err != nil {
			return func() {}, nil, err
		}
		return w.Stop, ch, nil
	case supervisorPoll:
		stop, ch := startInjectFocusPoll(cw, target)
		return stop, ch, nil
	}
	return func() {}, nil, nil
}

// startInjectFocusPoll supervises an IMMEDIATE typing batch by re-reading the focused
// window until it is stopped, feeding every reading through injectFocusAbortReason — the
// same fail-closed matrix the resident watch uses.
//
// It cannot fail to be established, which is the point (regression A2): the batch is not
// refused for want of a compositor feature. Fail-closed lives in the readings instead — a
// window list that cannot be read, or an unidentifiable focused window, ABORTS.
func startInjectFocusPoll(cw *cowork.Service, target cowork.Target) (func(), <-chan string) {
	auth := cw.Authority
	abort := make(chan string, 1)
	done := make(chan struct{})
	var once sync.Once
	safe.Go("cowork.injectFocusPoll", func() {
		t := time.NewTicker(injectFocusPollInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			wins, err := cw.KDE().ListWindows(4 * time.Second)
			reason := injectFocusAbortReason(auth, target, activeWindowEventFrom(wins, err))
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
	return func() { once.Do(func() { close(done) }) }, abort
}

// activeWindowEventFrom turns one window-list snapshot into the activation report the
// resident watch delivers, so both supervisors share injectFocusAbortReason's verdict.
// Pure. Every "we could not tell" maps to Error, which that matrix treats as an abort.
func activeWindowEventFrom(wins []kde.Window, listErr error) kde.ActiveWindowEvent {
	if listErr != nil {
		return kde.ActiveWindowEvent{Error: "the window list could not be read: " + listErr.Error()}
	}
	for _, w := range wins {
		if w.Active && !w.Minimized {
			return kde.ActiveWindowEvent{
				InternalID: w.InternalID, Caption: w.Caption,
				ResourceClass: w.ResourceClass, PID: w.PID,
			}
		}
	}
	return kde.ActiveWindowEvent{Error: "no active window could be identified"}
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

// guardA11yReadWindow is the READ-side sibling of guardA11yTarget: it refuses an a11y
// read (desktop_list_elements / desktop_read_text) aimed at one of Agent Kate's own
// windows.
//
// SECURITY (audit F50): the R2 action path refused self-targets while the R1 read path did
// not, so an agent could enumerate the exact bounds and labels of the consent dialog, the
// Cowork policy toggles and the kill-switch — the targeting data every pointer/keyboard
// self-target attack needs, handed over by a capability the user thinks of as "look at the
// screen". There is no legitimate agent use for reading our own interface.
//
// It FAILS CLOSED on the same matrix as the other guards: no authority, or a window with
// neither an owning process nor an application class, is refused rather than read.
func guardA11yReadWindow(auth *cowork.Authority, win kde.Window) error {
	const selfMsg = "refused: that window belongs to Agent Kate — the agent may not read its own interface, including its consent prompts, policy toggles and kill-switch"
	if auth == nil {
		return fmt.Errorf("refused: the self-target guard is unavailable, so the window's owner cannot be verified")
	}
	// IsSelfWindow already tries the PID first and is decisive on it even when KWin reports
	// no resource class (audit F7's lesson), so there is no separate IsSelfPID call here —
	// it would add nothing but the impression that it does.
	if auth.IsSelfWindow(win.PID, win.ResourceClass) {
		return fmt.Errorf("%s", selfMsg)
	}
	if win.PID <= 0 && win.ResourceClass == "" {
		return fmt.Errorf("refused: that window reports neither an owning process nor an application class, so it cannot be verified as not Agent Kate's own UI — re-list windows with desktop_list_windows and name one")
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
//
// SECURITY (audit F50, plan 29): it answers with codeUIOnly (-32012), not codeCoworkDenied
// (-32010). A refusal of an RPC only the human's own window may drive is not a Cowork
// consent decision — nothing was denied by the user, and no grant would change it — and the
// error code is part of the wire contract. Ten cowork gates (getPolicy, setPolicy,
// killSwitch, listGrants, listAudit, revokeGrant, respondGrant, portalResult,
// setPointerBounds, requestGrant) plus the two in cowork_enable.go were telling clients
// "Cowork denied this" for a permanent structural refusal. The sentence is unchanged
// (uiOnlyRefusal, handlers.go) so nothing matching on the text moves. See handlers.go:
// -32010 denied, -32011 cowork-busy, -32012 UI-only; the space is shared across
// cmd/akcore, so a new code must be checked against all three.
func requireUI(d handlerDeps, ctx context.Context) error {
	return requireUIWindow(d.srv, ctx)
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

// coworkGeometry is the production cursorGeometry: live KWin rectangles plus the consent
// authority's own-identity match. It exists so the guard's INPUT is an interface a test can
// supply, which is what lets the real guard→fire decision be driven without a session bus
// (audit F25/F26 wiring).
type coworkGeometry struct{ cw *cowork.Service }

func (g coworkGeometry) Windows() ([]cowork.WindowRect, error) {
	wins, err := g.cw.KDE().ListWindows(4 * time.Second)
	if err != nil {
		return nil, err
	}
	rects := make([]cowork.WindowRect, 0, len(wins))
	for _, w := range wins {
		rects = append(rects, cowork.WindowRect{
			X: w.X, Y: w.Y, W: w.Width, H: w.Height, PID: w.PID, ResourceClass: w.ResourceClass,
		})
	}
	return rects, nil
}

func (g coworkGeometry) IsSelfPoint(x, y int, wins []cowork.WindowRect) bool {
	return g.cw.IsSelfPoint(x, y, wins)
}

// cursorBounds is the desktop layout a stream's relative motion is accounted against — read
// only when the stream actually contains relative motion, since it costs a KWin round trip
// (cached for desktopBoundsTTL) and no other op needs it.
func cursorBounds(cw *cowork.Service, ps *pointerState, ops []map[string]any) kde.DesktopLayout {
	if !opsHaveRelative(ops) {
		return kde.DesktopLayout{}
	}
	return ps.desktopBounds(cw)
}

// runPointerAction is the shared tail for the positioned-pointer handlers: it gates the
// action under the R2 pointer_control capability, enforces the geometric self-target
// guard against LIVE geometry at every point the OPS act, plays them through the UI portal,
// and audits the literal target. A pure move derives no action points and so takes no
// geometric guard (motion may pass over Agent Kate's windows — only a click/scroll on them
// is dangerous); nothing here decides that, the ops do.
//
// commit (never nil in practice) is called with what became of the ops, WHILE THE FIRE LOCK
// IS STILL HELD: a refusal before the portal ran leaves the mirror alone, a play that did
// not provably land destroys it, and only a proven landing commits it. The distinction is
// load-bearing — see the "playback evidence" block in cowork_pointer.go — and doing it
// under the lock is what stops another thread from reading the mirror in the gap between
// the portal's reply and the update (audit F26).
//
// seed is non-nil when the action's guard evidence is the MIRROR itself (a scroll at the
// current cursor): the mirror is global, so it is re-proven under the lock before firing.
// It is not optional for such an action — cursorAction.run fails closed without it.
func runPointerAction(d handlerDeps, ctx context.Context, ps *pointerState, threadID string,
	target cowork.Target, desc string, ops []map[string]any,
	seed *point, commit func(pointerPlay)) (any, error) {
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
	// SECURITY (audit F26): from here to the portal's reply nothing else may touch the
	// cursor. The section is taken AFTER Authorize, which can wait minutes on a human, and
	// waiting for it is bounded + cancellable so one long script cannot park every other
	// agent (regression A1) — see cursorAction.run / pointerState.acquireFire.
	if err := (cursorAction{
		Geometry: coworkGeometry{cw},
		Ops:      ops,
		Seed:     seed,
		Bounds:   cursorBounds(cw, ps, ops),
		Fire: func(hold *cursorHold, ops []map[string]any) (bool, error) {
			res, err := runCursorPortal(d, ctx, hold, threadID, ops, 45*time.Second, nil)
			if err != nil {
				return false, err
			}
			return opsLandedAsAimed(ops, res), nil
		},
		Commit:  commit,
		Refused: func(err error) { cw.AuditRefusal(threadID, cowork.CapPointerControl, target, err.Error()) },
	}).run(ctx, ps, threadID); err != nil {
		return nil, cursorActionError(err)
	}
	cw.AuditCapture(threadID, cowork.CapPointerControl, target, dec.GrantID, hashString(desc))
	return map[string]any{"ok": true, "action": desc}, nil
}

// runPortal runs the core↔UI portal round-trip and returns the UI's result.
func runPortal(d handlerDeps, ctx context.Context, kind string, payload map[string]any, timeout time.Duration) (kde.PortalResult, error) {
	return runPortalAbortable(d, ctx, kind, payload, timeout, nil)
}

// runCursorPortal is the ONLY way a cursor-affecting op stream reaches the UI.
//
// SECURITY (audit F26): it demands a live cursorHold, so a dispatch that skipped the
// guard→fire section cannot compile a hold out of thin air and fails closed instead of
// racing. The check runs before anything is sent, so a wiring mistake refuses rather than
// half-plays.
func runCursorPortal(d handlerDeps, ctx context.Context, hold *cursorHold, threadID string,
	ops []map[string]any, timeout time.Duration, abort <-chan string) (kde.PortalResult, error) {
	if !hold.valid() {
		return kde.PortalResult{}, ipc.Errorf(codeCoworkDenied,
			"refused: pointer ops may only be released under exclusive control of the shared cursor (internal: no cursor hold)")
	}
	return runPortalAbortable(d, ctx, "inject", map[string]any{"threadId": threadID, "ops": ops}, timeout, abort)
}

// cursorActionError maps what cursorAction.run returned onto the wire: a refusal decided
// inside the section is a denial, a contention failure is codeCoworkBusy (retry the same
// call), and anything else — a portal error — is already an RPC error and passes through.
func cursorActionError(err error) error {
	if err == nil {
		return nil
	}
	var refusal cursorRefusal
	if errors.As(err, &refusal) {
		return ipc.Errorf(codeCoworkDenied, refusal.Error())
	}
	var busy cursorBusy
	if errors.As(err, &busy) {
		return ipc.Errorf(codeCoworkBusy, busy.Error())
	}
	var rpc *ipc.RPCError
	if errors.As(err, &rpc) {
		return rpc
	}
	return ipc.Errorf(ipc.CodeInternalError, err.Error())
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
