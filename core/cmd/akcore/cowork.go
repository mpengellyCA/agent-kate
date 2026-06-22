package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			ThreadID string        `json:"threadId"`
			Target   cowork.Target `json:"target"`
			MaxDim   int           `json:"maxDim"`
			Format   string        `json:"format"`
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
			"interactive": false,
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
