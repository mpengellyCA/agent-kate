package remote

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ringSize is the depth of the replay ring, in events.
//
// It mirrors the IPC server's outboundBuffer (1024) for the same reason that
// number was chosen there: it is the amount of traffic a client may fall behind
// by and still catch up. Here it is also the resumability budget — a phone that
// sleeps through a tunnel replays from Last-Event-ID only while its cursor is
// still inside the ring, and gets an honest `gap` the moment it is not. Bigger
// would hide gaps at the cost of memory that a long-running arena would never
// give back; smaller would make `gap` the normal answer.
const ringSize = 1024

// clientBuffer is the per-connection outbound depth.
//
// Deliberately a quarter of the ring: a client that has fallen 256 events behind
// on a link we are actively writing to is not going to recover by being given
// more room, it is going to be told to re-sync. Unlike the IPC server, which
// sheds the OLDEST notification and keeps going, an SSE consumer cannot be left
// with a hole it does not know about — so overflow marks the client and forces a
// `gap` rather than quietly editing its stream.
const clientBuffer = 256

// keepaliveInterval is how often a `: keepalive` comment goes out.
//
// 20s is under every commonly-deployed idle timeout (nginx and most mobile
// carrier NATs sit at 60s), so a socket that has actually died surfaces as a
// real EventSource onerror within one interval instead of as silence that looks
// exactly like an agent having nothing to say.
const keepaliveInterval = 20 * time.Second

// sseWriteDeadline bounds one write+flush, mirroring the IPC server's
// writeDeadline. A phone that vanished without closing its TCP connection
// otherwise parks this goroutine until the kernel gives up, which on a mobile
// link can be minutes.
const sseWriteDeadline = 30 * time.Second

// sseRetryMillis is the reconnect delay advertised to EventSource. Three seconds
// is long enough not to hammer a core that is shutting down and short enough
// that walking back into Wi-Fi feels instant.
const sseRetryMillis = 3000

// threadInterestWindow keeps per-thread agentEvent traffic flowing into the ring
// for a short while after the last viewer of that thread disconnects, so a
// momentary drop-out replays instead of producing a gap. Two minutes covers a
// lift, a tunnel and a screen lock; anything longer and the phone is better
// served by refetching the transcript anyway.
const threadInterestWindow = 2 * time.Minute

// Event names on the wire. These are part of the frozen contract.
const (
	evHello               = "hello"
	evGap                 = "gap"
	evRoster              = "roster"
	evTurnState           = "turnState"
	evPermissionRequested = "permissionRequested"
	evPermissionResolved  = "permissionResolved"
	evAgentEvent          = "agentEvent"
	evAgentGone           = "agentGone"
	evRevoked             = "revoked"
)

// Subscription scopes.
const (
	scopeRoster = "roster"
	scopeThread = "thread"
)

// Gap reasons, per the frozen contract.
const (
	gapRingOverflow  = "ring-overflow"
	gapSlowClient    = "slow-client"
	gapUnknownCursor = "unknown-cursor"
)

// ringEvent is one published event, retained for replay.
type ringEvent struct {
	id   uint64
	name string
	// threadID scopes agentEvent delivery. Empty for events that go everywhere.
	threadID string
	data     []byte
}

// hub owns the global event sequence, the replay ring and the live subscribers.
type hub struct {
	mu      sync.Mutex
	next    uint64 // id of the last event assigned; ids start at 1
	ring    []ringEvent
	head    int // next write index
	n       int // occupancy
	clients map[*sseClient]struct{}
	// interest records when a thread last had a viewer, so high-volume
	// agentEvent traffic is only minted while somebody could possibly want it.
	interest map[string]time.Time
	now      func() time.Time
}

func newHub(now func() time.Time) *hub {
	if now == nil {
		now = time.Now
	}
	return &hub{
		ring:     make([]ringEvent, ringSize),
		clients:  make(map[*sseClient]struct{}),
		interest: make(map[string]time.Time),
		now:      now,
	}
}

// publish assigns the next id, records the event for replay, and offers it to
// every interested subscriber. It NEVER blocks: every offer is non-blocking, so
// no phone on a bad connection can back pressure into the core's notification
// fan-out and stall the desktop.
//
// The offers happen with h.mu still held on purpose. Snapshotting the client set
// and releasing first would let two concurrent publishers interleave their
// deliveries to the same client, and a client that receives id 6 before id 5 has
// a Last-Event-ID that lies. Since every offer is O(1) and cannot block, holding
// the lock costs nothing and buys total ordering.
func (h *hub) publish(name, threadID string, data []byte) uint64 {
	h.mu.Lock()
	h.next++
	ev := ringEvent{id: h.next, name: name, threadID: threadID, data: data}
	h.ring[h.head] = ev
	h.head = (h.head + 1) % len(h.ring)
	if h.n < len(h.ring) {
		h.n++
	}
	for c := range h.clients {
		c.offer(ev)
	}
	h.mu.Unlock()
	return ev.id
}

// headID returns the id of the most recently published event, or 0 if none.
// A transcript snapshot pairs itself with this so the client can subscribe from
// the exact point the snapshot ended.
func (h *hub) headID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.next
}

// oldestLocked returns the id of the oldest retained event; 0 when the ring is
// empty. Caller holds h.mu.
func (h *hub) oldestLocked() uint64 {
	if h.n == 0 {
		return 0
	}
	idx := (h.head - h.n + len(h.ring)) % len(h.ring)
	return h.ring[idx].id
}

// subscribe registers c and returns the events it missed, the hub's head id at
// the instant it joined, and a gap reason if the cursor could not be honoured.
//
// Registration and the replay snapshot happen under one lock acquisition, which
// is what makes the join gapless AND duplicate-free: anything already in the
// ring is in the replay slice, anything published afterwards goes to the
// channel, and there is no instant in between. head is read under the same lock
// for the same reason — reported a moment later it would name an event this
// client has not been promised.
func (h *hub) subscribe(c *sseClient, fromID uint64, hasCursor bool) (replay []ringEvent, head uint64, gap string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.scope == scopeThread && c.threadID != "" {
		h.interest[c.threadID] = h.now()
	}

	if hasCursor {
		switch {
		case fromID > h.next:
			// A cursor from a previous core (ids restart at 1 each run) or a
			// fabricated one. Either way we cannot serve it; say so rather than
			// silently starting from nothing.
			gap = gapUnknownCursor
		case h.n == 0 && fromID < h.next:
			gap = gapRingOverflow
		case h.n > 0 && fromID+1 < h.oldestLocked():
			gap = gapRingOverflow
		}
	}

	if hasCursor && gap == "" {
		for i := 0; i < h.n; i++ {
			idx := (h.head - h.n + i + len(h.ring)) % len(h.ring)
			ev := h.ring[idx]
			if ev.id <= fromID {
				continue
			}
			if c.wants(ev) {
				replay = append(replay, ev)
			}
		}
	}

	h.clients[c] = struct{}{}
	return replay, h.next, gap
}

func (h *hub) unsubscribe(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	if c.scope == scopeThread && c.threadID != "" {
		// Record the moment interest ended so a reconnect inside the window
		// still finds its events in the ring.
		h.interest[c.threadID] = h.now()
	}
	h.pruneInterestLocked()
	h.mu.Unlock()
	c.close()
}

// pruneInterestLocked drops stale interest entries so a long-lived arena does
// not accumulate one map entry per thread ever viewed. Caller holds h.mu.
func (h *hub) pruneInterestLocked() {
	cutoff := h.now().Add(-threadInterestWindow)
	for id, at := range h.interest {
		if at.Before(cutoff) {
			delete(h.interest, id)
		}
	}
}

// interested reports whether anybody plausibly wants this thread's per-event
// traffic. Without this gate an eight-agent arena would fill the replay ring
// with per-token output and evict the permission prompts a reconnecting phone
// actually needs — the ring is a fixed budget and agentEvent is by far the
// hungriest consumer of it.
func (h *hub) interested(threadID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.scope == scopeThread && c.threadID == threadID {
			return true
		}
	}
	at, ok := h.interest[threadID]
	if !ok {
		return false
	}
	if h.now().Sub(at) >= threadInterestWindow {
		// Drop it here rather than sweeping the whole map: this runs on the
		// hottest publish path in the package, and paying O(threads) on every
		// agent event to reclaim one entry would be a poor trade.
		delete(h.interest, threadID)
		return false
	}
	return true
}

// hasRosterSubscriber reports whether a roster snapshot is worth computing.
// Calling the backend to build a roster nobody is watching would be work the
// core pays for on every turn flip.
func (h *hub) hasRosterSubscriber() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.scope == scopeRoster {
			return true
		}
	}
	return false
}

// terminate closes every stream matching pred, after handing it `reason` so the
// client learns why instead of seeing a bare disconnect. This is what makes a
// revoke effective against a stream that was opened before it.
func (h *hub) terminate(reason string, pred func(*sseClient) bool) int {
	h.mu.Lock()
	var hit []*sseClient
	for c := range h.clients {
		if pred(c) {
			hit = append(hit, c)
			delete(h.clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range hit {
		c.revokeWith(reason)
	}
	return len(hit)
}

// sseClient is one live subscriber.
type sseClient struct {
	scope     string
	threadID  string
	deviceID  string
	sessionID string

	ch   chan ringEvent
	wake chan struct{}
	rev  chan string

	closed    chan struct{}
	closeOnce sync.Once

	// dropped is set by a publisher that found the buffer full and cleared by
	// the writer once it has emitted the resulting gap.
	dropped atomic.Bool
}

func newSSEClient(scope, threadID, deviceID, sessionID string) *sseClient {
	return &sseClient{
		scope:     scope,
		threadID:  threadID,
		deviceID:  deviceID,
		sessionID: sessionID,
		ch:        make(chan ringEvent, clientBuffer),
		wake:      make(chan struct{}, 1),
		rev:       make(chan string, 1),
		closed:    make(chan struct{}),
	}
}

// wants is the SERVER-side subscription filter. Doing this client-side would
// mean shipping every token of an eight-agent arena to a phone rendering one
// chat, which is a battery bill the user pays for nothing.
//
// Note what is NOT filtered: turnState, permission and agentGone events reach
// both scopes regardless of thread. They are small, and a phone reading one
// chat still needs to learn that a different agent is now parked on a prompt.
func (c *sseClient) wants(ev ringEvent) bool {
	switch ev.name {
	case evRoster:
		return c.scope == scopeRoster
	case evAgentEvent:
		return c.scope == scopeThread && ev.threadID == c.threadID
	default:
		return true
	}
}

// offer hands one event to this client without ever blocking the publisher.
//
// On overflow we drop and mark. We do NOT shed the oldest and carry on the way
// the IPC server does: that policy works there because the UI re-derives its
// state from snapshots, whereas an SSE consumer applies a stream incrementally
// and a silently edited stream leaves it wrong with no way to find out. Marking
// makes the client re-sync, which is the only honest recovery.
func (c *sseClient) offer(ev ringEvent) {
	if !c.wants(ev) {
		return
	}
	select {
	case c.ch <- ev:
		return
	default:
	}
	c.dropped.Store(true)
	// Poke the writer so it emits the gap promptly even if nothing else is
	// coming; without this a client that stalled once could sit on a stale
	// stream until the next event or keepalive.
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *sseClient) revokeWith(reason string) {
	select {
	case c.rev <- reason:
	default:
	}
	// Wake the writer even if rev was already full — it is about to exit anyway.
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *sseClient) close() {
	c.closeOnce.Do(func() { close(c.closed) })
}

// drain empties the buffered backlog. Called by the WRITER, never a publisher,
// so there is no contention over who is receiving from the channel.
func (c *sseClient) drain() {
	for {
		select {
		case <-c.ch:
		default:
			return
		}
	}
}

// --- publish helpers -------------------------------------------------------

// TurnState is the roster-summary event. Wire encoding lives in server.go with
// every other encoder, so redaction and time formatting happen in one place.
type TurnState struct {
	ThreadID           string
	Busy               bool
	Attention          bool
	AwaitingPermission *Awaiting
}

// PermissionRequested announces a parked prompt.
//
// There is no `input` field, and there must never be one: permission.requested
// carries the raw tool arguments on the local socket because the desktop renders
// them, but those arguments are exactly where a password or a token lives. The
// phone gets the core-computed, already-redacted Summary.
type PermissionRequested struct {
	ThreadID  string
	RequestID string
	Kind      string
	ToolName  string
	Summary   string
	Deadline  time.Time
}

// PermissionResolved announces that a prompt was answered anywhere.
type PermissionResolved struct {
	ThreadID   string
	RequestID  string
	Allow      bool
	ResolvedBy string // "desktop" | "remote:<deviceName>" | "bridge" | "timeout"
}

// --- the wire ---------------------------------------------------------------

// sseWriter serialises frames onto one HTTP response.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// write emits one frame and flushes it, under a deadline. It reports false on
// any error, which the caller treats as "this connection is gone".
func (s *sseWriter) write(id uint64, name string, data []byte) bool {
	var b strings.Builder
	if id != 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatUint(id, 10))
		b.WriteByte('\n')
	}
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	// SSE forbids a raw newline inside a data line. Compact JSON never contains
	// one, but agentEvent embeds harness-produced json.RawMessage verbatim, so
	// splitting is the cheap way to be right rather than nearly right.
	for _, line := range strings.Split(string(data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return s.raw(b.String())
}

// comment emits an SSE comment line (used for keepalives).
func (s *sseWriter) comment(text string) bool { return s.raw(": " + text + "\n\n") }

func (s *sseWriter) raw(text string) bool {
	if err := s.rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline)); err != nil {
		// httptest's recorder and some middleware do not implement deadlines.
		// Losing the deadline is a liveness risk, not a correctness one, so we
		// continue rather than dropping a working stream.
		_ = err
	}
	if _, err := s.w.Write([]byte(text)); err != nil {
		return false
	}
	return s.rc.Flush() == nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// A payload we cannot marshal is a programming error; emitting an empty
		// object keeps the stream well-formed rather than desynchronising it.
		return []byte(`{}`)
	}
	return b
}
