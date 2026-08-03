// Package remote is Agent Kate's second surface: an opt-in HTTPS + SSE server
// that lets a phone watch the arena, answer a parked permission prompt, and read
// what an agent changed — without the phone ever becoming a privileged client.
//
// Three properties shape every decision in here.
//
// It is OFF by default and binds to ONE chosen interface. Nothing listens until
// the UI explicitly starts it, and a wildcard bind takes a second, separate
// opt-in. This is the first feature in the project to put agent I/O on a network
// transport, and the blast radius of getting it wrong is every agent on the box.
//
// It can never become the UI. The IPC role model gates eleven Cowork verbs on
// role == "ui" (screen capture, input injection, the kill-switch); a phone that
// could answer those would invert the whole consent model. The defence here is
// structural rather than procedural: the Backend interface simply has no method
// that reaches them, the route table is a frozen literal asserted by a test, and
// nothing in this package holds an ipc.Server.
//
// It must never block the core. Every event push is non-blocking with a
// drop-and-mark policy, so a phone on a dying mobile link cannot back-pressure
// into the core's notification fan-out and stall the desktop. That is the same
// hazard the IPC server's enqueue solves, with a different answer: the IPC layer
// sheds the oldest notification and carries on because the UI re-derives state
// from snapshots, while an SSE consumer applies its stream incrementally and has
// to be TOLD it lost something.
//
// Stdlib only, deliberately and permanently.
//
// # Wiring
//
// The core supplies a Backend adapter and pushes events in from the same
// notification fan-out the desktop reads:
//
//	rsrv, err := remote.New(remote.Config{
//	        BindAddr:    "192.168.1.20:8443", // required; chosen by the user
//	        DataDir:     "",                  // "" → the XDG data dir
//	        CoreVersion: version,
//	        Logger:      log,
//	}, &remoteBackend{deps: d})
//	// ... later, when the UI enables remote access:
//	err = rsrv.Start(ctx)
//
//	// In run.go's redacting projector, beside the desktop-only NotifyUI:
//	rsrv.PublishTranscript(threadID, projectedEvents)
//
//	// In turns.SetOnChange, beside srv.Notify("agent.turnState", …):
//	rsrv.PublishTurnState(remote.TurnState{ThreadID: threadID, Busy: busy, …})
//
// Every Publish* method is nil-safe, so a *Server left nil when remote access is
// off can be called unconditionally.
package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/safe"
)

// APIVersion is the frozen contract's major version. Within it, changes are
// additive only — a phone's cached PWA can be older than the core it talks to.
const APIVersion = 1

// backendTimeout bounds one call into the core on behalf of a remote request.
// Ten seconds is generous for a transcript read or a git status and short enough
// that a wedged backend surfaces as a 504 rather than as a phone that spins
// forever. HTTP request cancellation cuts it shorter whenever the phone gives up
// first.
const backendTimeout = 10 * time.Second

// rosterDebounce coalesces roster rebuilds. Turn flips arrive in bursts (eight
// agents finishing within a second of each other is a normal arena), and a
// roster is a snapshot, so emitting one per flip would cost N backend calls to
// deliver information the last one already contained.
const rosterDebounce = 300 * time.Millisecond

// shutdownGrace bounds the graceful HTTP shutdown. SSE streams are long-lived by
// construction and would otherwise hold Shutdown open indefinitely, so they are
// terminated explicitly first and this is only a backstop.
const shutdownGrace = 3 * time.Second

// Config is how the core turns remote access on. Every field the caller does not
// set has a safe default except BindAddr, which has no safe default at all.
type Config struct {
	// BindAddr is "host:port" and is REQUIRED. There is deliberately no default:
	// "listen on the right interface" is a decision the user makes, not one this
	// package guesses. A wildcard host additionally requires AllowAllInterfaces.
	BindAddr string

	// AllowAllInterfaces permits binding 0.0.0.0 / ::. Off by default because a
	// wildcard bind on a laptop that later joins a café network is the exact
	// mistake this feature must not make easy.
	AllowAllInterfaces bool

	// DataDir overrides the XDG data dir (tests, and a future portable mode).
	DataDir string

	// CoreVersion and WebUIBuild drive the version-skew banner via GET /meta.
	CoreVersion string
	WebUIBuild  string

	// StaticHandler serves the web UI. Left nil (the state of the world until
	// B3 lands) every non-API path answers 404, and the API still works.
	StaticHandler http.Handler

	Logger *slog.Logger

	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// Server is the remote HTTPS surface. The zero value is not usable; call New.
//
// Every Publish* method is safe on a nil receiver, so the core can hold a
// *Server that is nil when remote access is off and call it unconditionally from
// the notification fan-out. That is worth more than it looks: a nil check the
// caller can forget is a nil check that will eventually be forgotten, on the one
// path that runs for every event of every agent.
type Server struct {
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	backend Backend

	devices *DeviceStore
	audit   *Audit
	hub     *hub
	limiter *rateLimiter

	mux http.Handler

	// lifecycleMu serialises Start and Stop as whole operations. mu alone is not
	// enough: the bind and the certificate work happen between the "am I already
	// running" check and the assignment, so two enables racing on the UI toggle
	// could both get past it and leave an orphaned listener nothing can close.
	lifecycleMu sync.Mutex

	mu       sync.Mutex
	httpSrv  *http.Server
	listener net.Listener
	addr     string
	certFP   string

	rosterDirty chan struct{}
	stop        context.CancelFunc
	wg          sync.WaitGroup
}

// New prepares a server without binding anything or generating a certificate:
// the UI needs to mint a pairing token and read the device list before it turns
// the listener on, and doing filesystem work in New would make "look at the
// paired devices" a side-effecting operation.
func New(cfg Config, backend Backend) (*Server, error) {
	if backend == nil {
		return nil, errors.New("remote: backend is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	devicesPath := DefaultDevicesPath()
	auditPath := DefaultAuditPath()
	if cfg.DataDir != "" {
		devicesPath = filepath.Join(cfg.DataDir, "remote-devices.json")
		auditPath = filepath.Join(cfg.DataDir, "remote-audit.jsonl")
	}
	store, err := LoadDeviceStore(devicesPath, cfg.Now)
	if err != nil {
		// Do not replace an unreadable or corrupt credential store merely because
		// a panel opened. A user must resolve the durable-state failure explicitly.
		return nil, fmt.Errorf("remote: device store at %s is unusable: %w", devicesPath, err)
	}
	audit, err := LoadAudit(auditPath)
	if err != nil {
		cfg.Logger.Warn("remote audit log", "err", err)
		audit = &Audit{path: auditPath, tampered: true}
	}
	s := &Server{
		cfg:         cfg,
		log:         cfg.Logger,
		now:         cfg.Now,
		backend:     backend,
		devices:     store,
		audit:       audit,
		hub:         newHub(cfg.Now),
		limiter:     newRateLimiter(cfg.Now),
		rosterDirty: make(chan struct{}, 1),
	}
	s.mux = s.buildRouter()
	return s, nil
}

// Devices lists paired devices for the Remote Access panel.
func (s *Server) Devices() []Device {
	if s == nil {
		return nil
	}
	return s.devices.List()
}

// SetOnDevicesChanged registers a callback fired after the device table moves,
// so the panel can refresh without polling.
func (s *Server) SetOnDevicesChanged(fn func()) {
	if s == nil {
		return
	}
	s.devices.SetOnChange(fn)
}

// MintDevice pairs a new phone and returns the one and only plaintext copy of
// its token, plus the URL to hand it. The token rides in the URL FRAGMENT: a
// fragment is never sent to the server, so it cannot reach an access log, a
// proxy, or a Referer header on the first external link a transcript renders.
func (s *Server) MintDevice(name string) (token, pairingURL string, dev Device, err error) {
	if s == nil {
		return "", "", Device{}, errors.New("remote: server is not configured")
	}
	// Refuse to pair while nothing is listening.
	//
	// PairingURL used to fall back to the REQUESTED bind address, which quietly
	// produces a plausible-looking URL for a port this process does not own. In
	// the failure that found this — the listener refused to start because
	// something else already held the port — pairing still "succeeded" and
	// printed a QR code pointing at a stranger's service on that port. The user
	// scans it and lands somewhere else entirely.
	//
	// A pairing URL is only meaningful if there is a listener behind it, so the
	// two are tied together here rather than left to each caller to remember.
	if s.Addr() == "" {
		return "", "", Device{}, ErrNotListening
	}
	token, dev, err = s.devices.Mint(name)
	if err != nil {
		return "", "", Device{}, err
	}
	_ = s.audit.Append(AuditEntry{
		Kind: AuditPair, DeviceID: dev.ID, DeviceName: dev.Name,
		Detail: "device paired",
	})
	return token, s.PairingURL(token), dev, nil
}

// PairingURL builds the deep link a paired phone opens once. It returns "" when
// nothing is listening — deliberately, rather than falling back to the address
// that was REQUESTED. A URL naming a port this process failed to bind is worse
// than no URL: it points at whatever else happens to be on that port.
func (s *Server) PairingURL(token string) string {
	if s == nil {
		return ""
	}
	host := s.Addr()
	if host == "" {
		return ""
	}
	u := url.URL{Scheme: "https", Host: host, Path: "/", Fragment: "t=" + token}
	return u.String()
}

// RevokeDevice revokes a device AND terminates its live streams.
//
// The second half is the whole point. Rejecting new requests leaves an SSE
// connection opened before the revoke feeding a revoked phone indefinitely —
// which is not a revoke, it is a promise to revoke eventually. The stream is
// told `revoked` so the UI can say why, and then closed.
func (s *Server) RevokeDevice(id, reason string) bool {
	if s == nil {
		return false
	}
	changed := s.devices.Revoke(id, reason)
	closed := s.hub.terminate("revoked", func(c *sseClient) bool { return c.deviceID == id })
	if changed {
		name := ""
		if d, ok := s.devices.Get(id); ok {
			name = d.Name
		}
		_ = s.audit.Append(AuditEntry{
			Kind: AuditRevoke, DeviceID: id, DeviceName: name,
			Detail: fmt.Sprintf("revoked (%s); %d live stream(s) closed", reason, closed),
		})
		s.log.Info("remote device revoked", "device", id, "streams", closed)
	}
	return changed
}

// SetKillSwitch engages or re-arms the global switch. Engaging closes every live
// stream and makes every API request answer 503; the listener stays up, and
// static assets keep serving, so a phone gets a clear answer — and the web UI
// can say "switched off at the desktop" — instead of a connection refused it
// cannot tell apart from being out of range.
func (s *Server) SetKillSwitch(on bool) {
	if s == nil {
		return
	}
	s.devices.SetKillSwitch(on)
	kind := AuditRearm
	if on {
		kind = AuditKill
		n := s.hub.terminate("kill-switch", func(*sseClient) bool { return true })
		s.log.Warn("remote kill-switch engaged", "streamsClosed", n)
	}
	_ = s.audit.Append(AuditEntry{Kind: kind, Detail: "global kill-switch"})
}

// KillSwitch reports whether the global switch is engaged.
func (s *Server) KillSwitch() bool {
	if s == nil {
		return false
	}
	return s.devices.KillSwitch()
}

// AuditTampered reports that the audit chain failed verification on load. The
// panel must surface this: a broken chain is the only signal available that
// something with our uid edited the record of what a phone did.
func (s *Server) AuditTampered() bool {
	if s == nil {
		return false
	}
	return s.audit.Tampered()
}

// AuditTail returns recent audit entries for the panel.
func (s *Server) AuditTail(sinceSeq int64, limit int) ([]AuditEntry, int64, error) {
	if s == nil {
		return nil, 0, nil
	}
	return s.audit.Tail(sinceSeq, limit)
}

// Addr is the listening address, or "" when not running.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// CertFingerprint is the SHA-256 of the server certificate, colon-separated
// uppercase hex. The pairing panel shows it so a user on a hostile LAN can
// compare it against what the phone's browser reports — the only defence
// self-signed TLS has against an active MITM, since the warning itself has
// trained everybody to tap through.
func (s *Server) CertFingerprint() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.certFP
}

// Running reports whether the listener is up.
func (s *Server) Running() bool { return s.Addr() != "" }

// Start binds the listener and serves in the background. It returns once the
// socket is bound, so a bind failure is reported to the caller rather than
// logged into the void from a goroutine.
func (s *Server) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("remote: server is not configured")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.Lock()
	running := s.listener != nil
	s.mu.Unlock()
	if running {
		return errors.New("remote: already running")
	}

	host, _, err := net.SplitHostPort(s.cfg.BindAddr)
	if err != nil {
		return fmt.Errorf("remote: bindAddr must be host:port: %w", err)
	}
	if isWildcardHost(host) && !s.cfg.AllowAllInterfaces {
		return errors.New("remote: refusing to bind all interfaces; choose a specific address " +
			"or set AllowAllInterfaces")
	}

	certDir := s.cfg.DataDir
	if certDir == "" {
		certDir = filepath.Dir(DefaultDevicesPath())
	}
	cert, fp, err := ensureCert(certDir, host, s.now())
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", s.cfg.BindAddr)
	if err != nil {
		// Name the likeliest cause and the way out. "bind: address already in
		// use" on its own leaves the user guessing which of the two knobs
		// (interface or port) to touch, and the port is almost always the one.
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf(
				"remote: %s is already in use by another program; pick a different port: %w",
				s.cfg.BindAddr, err)
		}
		return fmt.Errorf("remote: listen %s: %w", s.cfg.BindAddr, err)
	}

	srv := &http.Server{
		Handler: s.mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// TLS 1.2 is the floor: every mobile browser that can render the web
			// UI speaks it, and anything older is a downgrade surface we gain
			// nothing from carrying.
			MinVersion: tls.VersionTLS12,
		},
		// ReadHeaderTimeout is the one timeout that can be set globally: a
		// ReadTimeout or WriteTimeout would cut SSE streams off mid-flight, so
		// those are handled per-write with a deadline instead.
		ReadHeaderTimeout: 15 * time.Second,
		// net/http logs TLS handshake failures to stderr by default, and with a
		// self-signed certificate those are ROUTINE — every phone that taps
		// "back" on the warning produces one. Routed to debug so they remain
		// diagnosable without drowning the core's log.
		ErrorLog: stdlog.New(slogWriter{log: s.log}, "", 0),
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.httpSrv = srv
	s.listener = ln
	s.addr = ln.Addr().String()
	s.certFP = fp
	s.stop = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	safe.Go("remote.rosterLoop", func() {
		defer s.wg.Done()
		s.rosterLoop(runCtx)
	})
	safe.Go("remote.serve", func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("remote server stopped", "err", err)
		}
	})
	// A cancelled parent context tears the listener down without the caller
	// having to remember Stop — matching how every other long-lived piece of the
	// core is wired. It also selects on runCtx so an explicit Stop releases this
	// goroutine rather than leaving one parked per Start.
	safe.Go("remote.ctxCloser", func() {
		select {
		case <-ctx.Done():
			_ = s.Stop(context.Background())
		case <-runCtx.Done():
		}
	})

	s.log.Info("remote access listening", "addr", s.addr, "fingerprint", fp)
	return nil
}

// Stop terminates every live stream and shuts the listener down.
func (s *Server) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.Lock()
	srv, ln, cancel := s.httpSrv, s.listener, s.stop
	s.httpSrv, s.listener, s.addr, s.stop = nil, nil, "", nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	// Close the streams first. Shutdown waits for in-flight requests, and an SSE
	// request is in flight until the phone walks away — without this, a graceful
	// shutdown is indistinguishable from a hang.
	s.hub.terminate("server-stopping", func(*sseClient) bool { return true })

	shutCtx, cancelShut := context.WithTimeout(ctx, shutdownGrace)
	defer cancelShut()
	err := srv.Shutdown(shutCtx)
	if err != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	s.log.Info("remote access stopped")
	return err
}

// Handler exposes the router for tests and for an embedder that wants to serve
// it over its own listener (a `tailscale serve` style setup). The security
// headers, auth and route allowlist are all inside it — there is no way to get
// the handlers without them.
func (s *Server) Handler() http.Handler { return s.mux }

// --- the push side ----------------------------------------------------------
//
// These are what the core's notification fan-out calls. All are nil-safe and all
// are non-blocking.

// PublishTurnState announces a busy/attention flip.
func (s *Server) PublishTurnState(ts TurnState) {
	if s == nil {
		return
	}
	s.hub.publish(evTurnState, ts.ThreadID, mustJSON(map[string]any{
		"threadId":           ts.ThreadID,
		"busy":               ts.Busy,
		"attention":          ts.Attention,
		"awaitingPermission": wireAwaiting(ts.AwaitingPermission),
		"serverTime":         s.nowRFC3339(),
	}))
	s.markRosterDirty()
}

// PublishPermissionRequested announces a parked prompt. The raw tool input is
// structurally absent from PermissionRequested; only the redacted Summary
// travels, and it is length-capped here because the core deliberately does not
// bound it (you must be able to read the whole Bash command you are approving on
// the desktop — a phone body is a different constraint).
func (s *Server) PublishPermissionRequested(p PermissionRequested) {
	if s == nil {
		return
	}
	s.hub.publish(evPermissionRequested, p.ThreadID, mustJSON(map[string]any{
		"threadId":   p.ThreadID,
		"requestId":  p.RequestID,
		"kind":       p.Kind,
		"toolName":   p.ToolName,
		"summary":    clip(p.Summary, maxSummaryBytes),
		"deadline":   rfc3339(p.Deadline),
		"serverTime": s.nowRFC3339(),
	}))
	s.markRosterDirty()
}

// PublishPermissionResolved announces that a prompt was answered anywhere. This
// is what stops a stale approve button on a lock screen from being tappable
// after the desktop already answered.
func (s *Server) PublishPermissionResolved(p PermissionResolved) {
	if s == nil {
		return
	}
	s.hub.publish(evPermissionResolved, p.ThreadID, mustJSON(map[string]any{
		"threadId":   p.ThreadID,
		"requestId":  p.RequestID,
		"allow":      p.Allow,
		"resolvedBy": p.ResolvedBy,
		"serverTime": s.nowRFC3339(),
	}))
	s.markRosterDirty()
}

// PublishTranscript forwards a coalesced batch of remote-safe typed events for
// one thread. It does not accept json.RawMessage, so a caller cannot widen the
// remote transcript by accidentally handing it a harness frame.
//
// It is dropped outright when nobody is watching that thread — see
// hub.interested. This is the highest-volume call in the package by orders of
// magnitude, and letting it into the ring unconditionally would evict the
// permission prompts a reconnecting phone came back for.
func (s *Server) PublishTranscript(threadID string, events []TranscriptEvent) {
	if s == nil || len(events) == 0 || threadID == "" {
		return
	}
	if !s.hub.interested(threadID) {
		return
	}
	s.hub.publish(evAgentEvent, threadID, mustJSON(map[string]any{
		"threadId": threadID,
		"events":   events,
	}))
}

// PublishAgentGone announces that a thread stopped, was discarded, or died.
func (s *Server) PublishAgentGone(threadID, reason string) {
	if s == nil || threadID == "" {
		return
	}
	s.hub.publish(evAgentGone, threadID, mustJSON(map[string]any{
		"threadId":   threadID,
		"reason":     reason,
		"serverTime": s.nowRFC3339(),
	}))
	s.markRosterDirty()
}

// markRosterDirty schedules a coalesced roster rebuild. Non-blocking by
// construction: this runs on the core's fan-out path.
func (s *Server) markRosterDirty() {
	select {
	case s.rosterDirty <- struct{}{}:
	default:
	}
}

// rosterLoop rebuilds and publishes the roster after a quiet period.
//
// The roster is derived HERE, from the same Backend.ListAgents that answers
// GET /api/v1/agents, rather than being pushed in by the core. Two producers of
// "the roster" is precisely the divergence this whole plan exists to stop.
func (s *Server) rosterLoop(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rosterDirty:
			if !pending {
				pending = true
				timer.Reset(rosterDebounce)
			}
		case <-timer.C:
			pending = false
			s.emitRoster(ctx)
		}
	}
}

func (s *Server) emitRoster(ctx context.Context) {
	if !s.hub.hasRosterSubscriber() {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, backendTimeout)
	defer cancel()
	agents, err := s.backend.ListAgents(callCtx)
	if err != nil {
		s.log.Warn("remote roster refresh failed", "err", err)
		return
	}
	s.hub.publish(evRoster, "", mustJSON(s.rosterBody(agents)))
}

func (s *Server) nowRFC3339() string { return rfc3339(s.now()) }

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// slogWriter adapts net/http's *log.Logger to slog at debug level.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.log.Debug("remote http", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}

func isWildcardHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}
