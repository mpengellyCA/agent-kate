package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentkate/internal/agent"
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
	echoes   humanEchoStore
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
	events = c.echoes.consumeObserved(threadID, events)
	srv.PublishTranscript(threadID, events)
}

// publishAcceptedHumanSend is separate from publishRawEvents because the core
// created this user turn itself. It is already a remote-safe typed DTO; routing
// it through the raw projector would couple the remote surface to a desktop
// event shape and invite attachment data to drift across the boundary.
func (c *remoteControl) publishAcceptedHumanSend(threadID, text string, at time.Time, atts []agent.Attachment) {
	event := remote.TranscriptEvent{Kind: "user", Text: text, Attachments: remoteAttachmentMarkers(atts), At: at}
	c.echoes.add(threadID, event)
	if srv := c.server(); srv != nil {
		srv.PublishTranscript(threadID, []remote.TranscriptEvent{event})
	}
}

// mergeHumanEchoes makes an accepted user turn visible to a reconnecting phone
// before a harness has flushed its own transcript record. Once the harness
// produces the equivalent typed user event, consumeObserved removes the
// synthetic copy so remote clients never see a duplicate. The store holds only
// the already-redacted user text, is process-local, and is bounded: it is an
// ordering bridge, not a second transcript store.
func (c *remoteControl) mergeHumanEchoes(threadID string, events []remote.TranscriptEvent) []remote.TranscriptEvent {
	if c == nil {
		return events
	}
	return c.echoes.merge(threadID, events)
}

const (
	maxHumanEchoesPerThread = 64
	maxHumanEchoTextBytes   = 256 * 1024
	humanEchoMaxAge         = 24 * time.Hour
)

type humanEchoStore struct {
	mu       sync.Mutex
	byThread map[string][]remote.TranscriptEvent
}

func (s *humanEchoStore) add(threadID string, event remote.TranscriptEvent) {
	if s == nil || threadID == "" || event.Kind != "user" || event.Text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byThread == nil {
		s.byThread = make(map[string][]remote.TranscriptEvent)
	}
	now := time.Now().UTC()
	items := pruneHumanEchoes(s.byThread[threadID], now)
	items = append(items, event)
	for len(items) > maxHumanEchoesPerThread || humanEchoTextBytes(items) > maxHumanEchoTextBytes {
		items = items[1:]
	}
	s.byThread[threadID] = items
}

// consumeObserved returns only new harness events. Matching is one-to-one and
// ordered, so two identical follow-ups remain two messages rather than
// accidentally coalescing. A harness record may arrive long after acceptance
// when a remote follow-up waits behind a slow turn; an event from before the
// acceptance timestamp is the only one that must not consume the new echo.
func (s *humanEchoStore) consumeObserved(threadID string, events []remote.TranscriptEvent) []remote.TranscriptEvent {
	if s == nil || len(events) == 0 {
		return events
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := pruneHumanEchoes(s.byThread[threadID], time.Now().UTC())
	if len(items) == 0 {
		return events
	}
	out := make([]remote.TranscriptEvent, 0, len(events))
	for _, event := range events {
		if i := matchingHumanEcho(items, event); i >= 0 {
			items = append(items[:i], items[i+1:]...)
			continue
		}
		out = append(out, event)
	}
	if len(items) == 0 {
		delete(s.byThread, threadID)
	} else {
		s.byThread[threadID] = items
	}
	return out
}

func (s *humanEchoStore) merge(threadID string, events []remote.TranscriptEvent) []remote.TranscriptEvent {
	if s == nil {
		return events
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := pruneHumanEchoes(s.byThread[threadID], time.Now().UTC())
	if len(items) == 0 {
		return events
	}
	// A read can beat the live relay. Consume any harness event in this page
	// before adding the not-yet-persisted typed echo.
	remaining := make([]remote.TranscriptEvent, 0, len(items))
	observed := append([]remote.TranscriptEvent(nil), events...)
	for _, echo := range items {
		if i := matchingObservedHumanEcho(echo, observed); i >= 0 {
			observed = append(observed[:i], observed[i+1:]...)
		} else {
			remaining = append(remaining, echo)
		}
	}
	if len(remaining) == 0 {
		delete(s.byThread, threadID)
	} else {
		s.byThread[threadID] = remaining
	}
	out := append([]remote.TranscriptEvent(nil), events...)
	out = append(out, remaining...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func pruneHumanEchoes(items []remote.TranscriptEvent, now time.Time) []remote.TranscriptEvent {
	cutoff := now.Add(-humanEchoMaxAge)
	first := 0
	for first < len(items) && items[first].At.Before(cutoff) {
		first++
	}
	return append([]remote.TranscriptEvent(nil), items[first:]...)
}

func humanEchoTextBytes(items []remote.TranscriptEvent) int {
	total := 0
	for _, item := range items {
		total += len(item.Text)
	}
	return total
}

func matchingHumanEcho(items []remote.TranscriptEvent, event remote.TranscriptEvent) int {
	for i, echo := range items {
		if humanEchoMatches(echo, event) {
			return i
		}
	}
	return -1
}

func matchingObservedHumanEcho(echo remote.TranscriptEvent, events []remote.TranscriptEvent) int {
	for i, event := range events {
		if humanEchoMatches(echo, event) {
			return i
		}
	}
	return -1
}

func humanEchoMatches(echo, event remote.TranscriptEvent) bool {
	if echo.Kind != "user" || event.Kind != "user" || echo.Text != event.Text {
		return false
	}
	if echo.At.IsZero() || event.At.IsZero() {
		return false
	}
	// A transcript timestamp slightly before acceptance can result from a small
	// clock/flush skew, but an older historical user turn must never consume a
	// fresh identical prompt during a reconnect merge.
	return !event.At.Before(echo.At.Add(-time.Minute))
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
