// Package permission coordinates human approval for agent tool calls.
//
// Raw tool input never enters a Broker.  Desktop handlers retain it long
// enough to render the local approval UI; the broker holds only a redacted
// request record plus the two narrowly allowlisted render payloads needed to
// answer a plan or an AskUserQuestion from another human surface.
package permission

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Decision is a human answer. UpdatedInput is only used for the two prompt
// forms whose response has a structured payload. ResolvedBy is attribution,
// never an authority claim: the authenticated surface decides it.
type Decision struct {
	Allow        bool
	UpdatedInput json.RawMessage
	ResolvedBy   string
}

// Detail contains the only raw-derived content a non-desktop human surface may
// render. It remains empty for ordinary tool approvals.
type Detail struct {
	Plan      string
	Questions json.RawMessage
}

// Request is the remote-safe metadata for a pending approval. Its shape
// deliberately has no raw tool-input field.
type Request struct {
	ID       string
	ThreadID string
	ToolName string
	Summary  string
	Opened   time.Time
	Deadline time.Time
	Detail
}

// TerminalReason says why a pending prompt stopped being actionable.
type TerminalReason string

const (
	Resolved    TerminalReason = "resolved"
	TimedOut    TerminalReason = "timed_out"
	Interrupted TerminalReason = "interrupted"
	Exited      TerminalReason = "exited"
	NoHuman     TerminalReason = "no_human"
)

// Resolution is an immutable terminal event for an approval. Every outcome —
// answer, timeout, interruption, process exit, and no available human — uses
// this one type so a mirrored surface can clear a stale prompt consistently.
type Resolution struct {
	Request  Request
	Decision Decision
	Reason   TerminalReason
	At       time.Time
}

// Observer receives remote-safe edge records. Implementations must not block;
// the broker calls them after releasing its lock.
type Observer interface {
	PermissionOpened(Request)
	PermissionResolved(Resolution)
}

type entry struct {
	req    Request
	ch     chan Decision
	remote bool
}

// Broker matches pending approvals to a single human decision.
type Broker struct {
	mu       sync.Mutex
	pending  map[string]*entry
	observer Observer
}

// New returns an empty broker.
func New() *Broker { return &Broker{pending: make(map[string]*entry)} }

// SetObserver installs the in-process remote-safe event sink. The observer is
// optional because no remote listener exists until the desktop enables one.
func (b *Broker) SetObserver(observer Observer) {
	b.mu.Lock()
	b.observer = observer
	b.mu.Unlock()
}

// Open creates a redacted pending approval with no renderable detail.
func (b *Broker) Open(threadID, toolName, summary string, timeout time.Duration) (Request, chan Decision) {
	return b.OpenWithDetail(threadID, toolName, summary, Detail{}, timeout)
}

// OpenWithDetail creates a redacted pending approval. Callers can only pass a
// Detail value already extracted by RenderableDetail; there is intentionally no
// raw input argument on this API.
func (b *Broker) OpenWithDetail(threadID, toolName, summary string, detail Detail, timeout time.Duration) (Request, chan Decision) {
	return b.open(threadID, toolName, summary, detail, timeout, true)
}

// OpenLocal creates a desktop-only approval used for Cowork consent. It shares
// the broker's one-answer semantics with tool approvals but is never projected
// to a remote device and cannot be resolved through the remote human surface.
func (b *Broker) OpenLocal() (string, chan Decision) {
	req, ch := b.open("", "", "", Detail{}, 0, false)
	return req.ID, ch
}

func (b *Broker) open(threadID, toolName, summary string, detail Detail, timeout time.Duration, remote bool) (Request, chan Decision) {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	now := time.Now().UTC()
	req := Request{
		ID: "perm-" + hex.EncodeToString(raw[:]), ThreadID: threadID,
		ToolName: toolName, Summary: summary, Opened: now, Deadline: now.Add(timeout),
		Detail: detail,
	}
	entry := &entry{req: req, ch: make(chan Decision, 1), remote: remote}
	b.mu.Lock()
	b.pending[req.ID] = entry
	observer := b.observer
	b.mu.Unlock()
	if remote && observer != nil {
		observer.PermissionOpened(req)
	}
	return req, entry.ch
}

// OpenForThread is retained for legacy, local-only callers. It carries no raw
// input and should not be used by new approval flows.
func (b *Broker) OpenForThread(threadID string) (string, chan Decision) {
	req, ch := b.Open(threadID, "", "Approval required", 0)
	return req.ID, ch
}

// Resolve answers an open request and returns the remote-safe request record.
func (b *Broker) Resolve(id string, decision Decision) (Request, bool) {
	resolution, entry, observer, ok := b.take(id, decision, Resolved)
	if !ok {
		return Request{}, false
	}
	entry.ch <- decision
	if observer != nil {
		observer.PermissionResolved(resolution)
	}
	return resolution.Request, true
}

// Close removes a request whose wait has ended without a human answer.
func (b *Broker) Close(id string, reason TerminalReason) (Request, bool) {
	if reason == "" {
		reason = TimedOut
	}
	resolution, _, observer, ok := b.take(id, Decision{}, reason)
	if !ok {
		return Request{}, false
	}
	if observer != nil {
		observer.PermissionResolved(resolution)
	}
	return resolution.Request, true
}

// CancelThread immediately denies every request owned by a terminal thread.
// It returns the number cancelled for existing callers; the observer receives
// one Resolution for each prompt so all remote views clear them.
func (b *Broker) CancelThread(threadID string, reason TerminalReason) int {
	if threadID == "" {
		return 0
	}
	if reason == "" {
		reason = Interrupted
	}
	type delivery struct {
		resolution Resolution
		entry      *entry
		remote     bool
	}
	b.mu.Lock()
	deliveries := make([]delivery, 0)
	for id, entry := range b.pending {
		if entry.req.ThreadID != threadID {
			continue
		}
		delete(b.pending, id)
		deliveries = append(deliveries, delivery{resolution: Resolution{
			Request: entry.req, Reason: reason, At: time.Now().UTC(),
		}, entry: entry, remote: entry.remote})
	}
	observer := b.observer
	b.mu.Unlock()
	for _, delivery := range deliveries {
		delivery.entry.ch <- Decision{}
		if delivery.remote && observer != nil {
			observer.PermissionResolved(delivery.resolution)
		}
	}
	return len(deliveries)
}

// take removes one entry and snapshots the observer without holding the lock
// while a channel send or publication happens.
func (b *Broker) take(id string, decision Decision, reason TerminalReason) (Resolution, *entry, Observer, bool) {
	b.mu.Lock()
	entry, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	observer := b.observer
	b.mu.Unlock()
	if !ok {
		return Resolution{}, nil, observer, false
	}
	if !entry.remote {
		observer = nil
	}
	return Resolution{Request: entry.req, Decision: decision, Reason: reason, At: time.Now().UTC()}, entry, observer, true
}

// Pending returns all live requests oldest first. It returns value copies, so
// a caller cannot mutate the broker's canonical record.
func (b *Broker) Pending() []Request {
	return b.listPending(false)
}

// PendingRemote returns only approvals explicitly opened for the paired-device
// surface. It keeps desktop-only broker uses out of remote roster projections.
func (b *Broker) PendingRemote() []Request {
	return b.listPending(true)
}

func (b *Broker) listPending(remoteOnly bool) []Request {
	b.mu.Lock()
	out := make([]Request, 0, len(b.pending))
	for _, entry := range b.pending {
		if remoteOnly && !entry.remote {
			continue
		}
		out = append(out, entry.req)
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Opened.Equal(out[j].Opened) {
			return out[i].ID < out[j].ID
		}
		return out[i].Opened.Before(out[j].Opened)
	})
	return out
}

// Get returns a live request by id.
func (b *Broker) Get(id string) (Request, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.pending[id]
	if !ok {
		return Request{}, false
	}
	return entry.req, true
}

// GetRemote returns a pending approval only when it was explicitly opened for
// a remote human surface. Desktop-only broker uses (such as Cowork consent)
// must remain invisible and unresolvable to paired devices.
func (b *Broker) GetRemote(id string) (Request, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.pending[id]
	if !ok || !entry.remote {
		return Request{}, false
	}
	return entry.req, true
}

// PendingFor returns the oldest live request for threadID.
func (b *Broker) PendingFor(threadID string) (Request, bool) {
	for _, request := range b.Pending() {
		if request.ThreadID == threadID {
			return request, true
		}
	}
	return Request{}, false
}
