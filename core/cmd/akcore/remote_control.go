package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"agentkate/internal/permission"
	"agentkate/internal/remote"
	"agentkate/internal/remote/webui"
)

// remoteControl owns the opt-in HTTPS listener and is the remote-safe broker
// observer. It has no IPC identity: the desktop's control RPCs will call it
// through their existing UI-only gates, while paired devices reach it solely
// through the HTTPS session authenticator.
type remoteControl struct {
	ctx context.Context
	log *slog.Logger

	srv atomic.Pointer[remote.Server]
	mu  sync.Mutex

	deps     handlerDeps
	backend  remote.Backend
	dataDir  string // test override; production uses the private XDG location.
	bind     string
	allowAll bool
}

func newRemoteControl(ctx context.Context, log *slog.Logger) *remoteControl {
	return &remoteControl{ctx: ctx, log: log}
}

// attach constructs an unbound server only. It neither opens a port nor mints
// credentials/certificates, so starting the desktop core or opening a future
// Remote panel cannot create remotely usable state as a side effect.
func (c *remoteControl) attach(d handlerDeps) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deps = d
	c.backend = &remoteBackend{d: d}
	srv, err := c.build("", false)
	if err != nil {
		c.log.Warn("remote access unavailable", "err", err)
		return
	}
	c.srv.Store(srv)
}

func (c *remoteControl) build(bind string, allowAll bool) (*remote.Server, error) {
	if c == nil || c.backend == nil {
		return nil, errors.New("remote access is not configured")
	}
	return remote.New(remote.Config{
		BindAddr:           bind,
		AllowAllInterfaces: allowAll,
		DataDir:            c.dataDir,
		CoreVersion:        version,
		StaticHandler:      webui.Handler(),
		WebUIBuild:         remoteWebUIBuild(),
		Logger:             c.log,
	}, c.backend)
}

func remoteWebUIBuild() string {
	if webui.Built() {
		return version
	}
	return "stub"
}

func (c *remoteControl) server() *remote.Server {
	if c == nil {
		return nil
	}
	return c.srv.Load()
}

func (c *remoteControl) canAnswerPermissions() bool {
	srv := c.server()
	if srv == nil || !srv.Running() || srv.KillSwitch() {
		return false
	}
	for _, device := range srv.Devices() {
		if device.Active() {
			return true
		}
	}
	return false
}

// setEnabled serialises the visible listener lifecycle. A caller must pass a
// specific host:port; there is intentionally no convenience LAN default.
func (c *remoteControl) setEnabled(enabled bool, bindAddr string, allowAll bool) (string, string, error) {
	if c == nil {
		return "", "", errors.New("remote access is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	srv := c.srv.Load()
	if srv == nil {
		return "", "", errors.New("remote access is unavailable; inspect its durable state")
	}
	if !enabled {
		if err := srv.Stop(context.Background()); err != nil {
			return "", "", err
		}
		return "", srv.CertFingerprint(), nil
	}
	if strings.TrimSpace(bindAddr) == "" {
		return "", "", errors.New("bindAddr is required, as host:port")
	}
	if srv.Running() && c.bind == bindAddr && c.allowAll == allowAll {
		return srv.Addr(), srv.CertFingerprint(), nil
	}
	if srv.Running() {
		if err := srv.Stop(context.Background()); err != nil {
			return "", "", err
		}
	}
	if c.bind != bindAddr || c.allowAll != allowAll {
		fresh, err := c.build(bindAddr, allowAll)
		if err != nil {
			return "", "", err
		}
		c.bind = bindAddr
		c.allowAll = allowAll
		c.srv.Store(fresh)
		srv = fresh
	}
	if err := srv.Start(c.ctx); err != nil {
		return "", "", err
	}
	return srv.Addr(), srv.CertFingerprint(), nil
}

func (c *remoteControl) stop(ctx context.Context) {
	if srv := c.server(); srv != nil {
		_ = srv.Stop(ctx)
	}
}

// PermissionOpened and PermissionResolved are the broker observer contract.
// The broker's Request/Resolution DTOs structurally cannot contain generic raw
// tool input, and all terminal paths enter PermissionResolved.
func (c *remoteControl) PermissionOpened(req permission.Request) {
	if srv := c.server(); srv != nil {
		srv.PublishPermissionRequested(remote.PermissionRequested{
			ThreadID: req.ThreadID, RequestID: req.ID, Kind: remotePermissionKind(req.ToolName),
			ToolName: req.ToolName, Summary: req.Summary, Deadline: req.Deadline,
		})
	}
}

func (c *remoteControl) PermissionResolved(res permission.Resolution) {
	resolvedBy := res.Decision.ResolvedBy
	if resolvedBy == "" {
		resolvedBy = string(res.Reason)
	}
	if srv := c.server(); srv != nil {
		srv.PublishPermissionResolved(remote.PermissionResolved{
			ThreadID: res.Request.ThreadID, RequestID: res.Request.ID,
			Allow: res.Decision.Allow, ResolvedBy: resolvedBy,
		})
	}
}

func (c *remoteControl) publishRawEvents(threadID string, raw []json.RawMessage) {
	srv := c.server()
	if srv == nil {
		return
	}
	events, _ := projectRemoteTranscript(raw, 200, 256*1024)
	srv.PublishTranscript(threadID, events)
}

func (c *remoteControl) publishTurnState(threadID string, busy bool) {
	srv := c.server()
	if srv == nil {
		return
	}
	status := ""
	if c.deps.sessions != nil {
		if rec, ok := c.deps.sessions.Get(threadID); ok {
			status = rec.Status
		}
	}
	parked := remotePendingByThread(c.deps.broker)
	awaiting := remoteAwaiting(parked[threadID])
	srv.PublishTurnState(remote.TurnState{
		ThreadID: threadID, Busy: busy, Attention: awaiting != nil || (status == "running" && !busy),
		AwaitingPermission: awaiting,
	})
}

func (c *remoteControl) publishAgentGone(threadID, reason string) {
	if srv := c.server(); srv != nil {
		srv.PublishAgentGone(threadID, reason)
	}
}
