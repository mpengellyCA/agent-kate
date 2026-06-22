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
	t := cowork.Target{Kind: cowork.TargetApp, Label: "the focused window"}
	if info.PID > 0 {
		if wins, err := cw.KDE().ListWindows(4 * time.Second); err == nil {
			for _, w := range wins {
				if w.PID == info.PID {
					t.ResourceClass = w.ResourceClass
					if w.Caption != "" {
						t.Label = w.Caption
					}
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
	return t, label
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
