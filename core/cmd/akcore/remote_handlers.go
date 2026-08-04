package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"agentkate/internal/ipc"
	"agentkate/internal/remote"
)

// registerRemoteHandlers is the desktop's control plane for remote access.
// Every method is UI-window-only. Paired devices never use these IPC methods:
// they reach the narrow remote.Backend through a verified HTTPS session.
func registerRemoteHandlers(d handlerDeps) {
	d.srv.Handle("remote.setEnabled", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Enabled            bool   `json:"enabled"`
			BindAddr           string `json:"bindAddr"`
			AllowAllInterfaces bool   `json:"allowAllInterfaces"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if d.remote == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "remote access is unavailable in this core")
		}
		addr, fp, err := d.remote.setEnabled(p.Enabled, p.BindAddr, p.AllowAllInterfaces)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true, "addr": addr, "certFingerprint": fp}, nil
	})

	d.srv.Handle("remote.status", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		if d.remote == nil || d.remote.server() == nil {
			return map[string]any{"enabled": false, "devices": []any{}, "webuiBuild": "unavailable"}, nil
		}
		srv := d.remote.server()
		devices := srv.Devices()
		rows := make([]map[string]any, 0, len(devices))
		for _, dev := range devices {
			caps := make([]string, len(dev.Capabilities))
			for i, cap := range dev.Capabilities {
				caps[i] = string(cap)
			}
			row := map[string]any{
				"id": dev.ID, "name": dev.Name, "pairedAt": remoteTime(dev.PairedAt),
				"lastSeen": remoteTime(dev.LastSeen), "revoked": dev.RevokedAt != nil, "capabilities": caps,
			}
			if dev.RevokedAt != nil {
				row["revokedAt"] = remoteTime(*dev.RevokedAt)
				row["revokeReason"] = dev.RevokeReason
			}
			rows = append(rows, row)
		}
		return map[string]any{
			"enabled": srv.Running(), "addr": srv.Addr(), "certFingerprint": srv.CertFingerprint(),
			"killSwitch": srv.KillSwitch(), "auditTampered": srv.AuditTampered(),
			"devices": rows, "webuiBuild": remoteWebUIBuild(),
		}, nil
	})

	d.srv.Handle("remote.setDeviceCapabilities", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			DeviceID     string              `json:"deviceId"`
			Capabilities []remote.Capability `json:"capabilities"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if d.remote == nil || d.remote.server() == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "remote access is unavailable in this core")
		}
		dev, changed, err := d.remote.server().SetDeviceCapabilities(p.DeviceID, p.Capabilities)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if dev.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown or revoked device")
		}
		return map[string]any{"ok": true, "changed": changed, "capabilities": dev.Capabilities}, nil
	})

	d.srv.Handle("remote.pairDevice", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if d.remote == nil || d.remote.server() == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "remote access is unavailable in this core")
		}
		token, url, device, err := d.remote.server().MintDevice(strings.TrimSpace(p.Name))
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Token is returned exactly once to the UI. It is never persisted in a
		// status response, audit detail, or a general IPC notification.
		return map[string]any{"token": token, "pairingUrl": url, "device": map[string]any{
			"id": device.ID, "name": device.Name, "pairedAt": remoteTime(device.PairedAt),
		}}, nil
	})

	d.srv.Handle("remote.revokeDevice", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			DeviceID string `json:"deviceId"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if d.remote == nil || d.remote.server() == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "remote access is unavailable in this core")
		}
		return map[string]any{"ok": d.remote.server().RevokeDevice(p.DeviceID, remoteDefault(p.Reason, "revoked by desktop user"))}, nil
	})

	d.srv.Handle("remote.killSwitch", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			On bool `json:"on"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if d.remote == nil || d.remote.server() == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "remote access is unavailable in this core")
		}
		d.remote.server().SetKillSwitch(p.On)
		return map[string]any{"ok": true, "killSwitch": d.remote.server().KillSwitch()}, nil
	})

	d.srv.Handle("remote.auditTail", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			SinceSeq int64 `json:"sinceSeq"`
			Limit    int   `json:"limit"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if d.remote == nil || d.remote.server() == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "remote access is unavailable in this core")
		}
		entries, next, err := d.remote.server().AuditTail(p.SinceSeq, p.Limit)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if entries == nil {
			entries = []remote.AuditEntry{}
		}
		return map[string]any{"entries": entries, "nextSeq": next, "tampered": d.remote.server().AuditTampered()}, nil
	})
}

func remoteTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func remoteDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
