// Package permission coordinates per-tool approval. When an agent wants to use
// a gated tool — or asks a clarifying question via AskUserQuestion — the
// Cooperation MCP bridge opens a request here; the UI resolves it once the
// human responds.
package permission

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// Decision is a human's answer to a permission request. For ordinary tools
// only Allow matters. For AskUserQuestion, UpdatedInput carries the answered
// question payload that Claude Code expects back.
type Decision struct {
	Allow        bool
	UpdatedInput json.RawMessage
}

// Broker matches pending permission requests to their human decisions.
type Broker struct {
	mu      sync.Mutex
	pending map[string]chan Decision
	// threads records the thread that owns each prompt.  A prompt is a
	// turn-local interaction: when that turn is interrupted or exits, leaving
	// its frame parked until the ordinary timeout keeps the CLI needlessly
	// blocked.  It is deliberately private to the broker so every caller gets
	// the same fail-closed cancellation behaviour.
	threads map[string]string
}

// New returns an empty Broker.
func New() *Broker {
	return &Broker{pending: make(map[string]chan Decision), threads: make(map[string]string)}
}

// Open registers a new pending request and returns its id and a channel that
// will receive the decision exactly once.
func (b *Broker) Open() (string, chan Decision) {
	return b.OpenForThread("")
}

// OpenForThread registers a pending request owned by threadID.  An empty id
// retains Open's legacy behaviour: it can only be resolved or timed out, not
// selected by CancelThread.
func (b *Broker) OpenForThread(threadID string) (string, chan Decision) {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	id := "perm-" + hex.EncodeToString(raw[:])
	ch := make(chan Decision, 1)

	b.mu.Lock()
	b.pending[id] = ch
	if threadID != "" {
		b.threads[id] = threadID
	}
	b.mu.Unlock()
	return id, ch
}

// Resolve delivers a decision to a waiting request. It returns false if the id
// is unknown (already resolved, or timed out).
func (b *Broker) Resolve(id string, d Decision) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
		delete(b.threads, id)
	}
	b.mu.Unlock()
	if ok {
		ch <- d
	}
	return ok
}

// Close drops a pending request without delivering a decision (used on timeout).
func (b *Broker) Close(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	delete(b.threads, id)
	b.mu.Unlock()
}

// CancelThread immediately denies every pending prompt owned by threadID.
// The zero Decision is a refusal, never an approval; delivering it (rather
// than merely dropping the map entry) wakes the bridge frame now.  This is
// used for interruption and process exit, where waiting for the eight-minute
// human timeout would otherwise pin an already-dead turn.
func (b *Broker) CancelThread(threadID string) int {
	if threadID == "" {
		return 0
	}
	b.mu.Lock()
	var cancelled []chan Decision
	for id, owner := range b.threads {
		if owner != threadID {
			continue
		}
		if ch, ok := b.pending[id]; ok {
			cancelled = append(cancelled, ch)
			delete(b.pending, id)
		}
		delete(b.threads, id)
	}
	b.mu.Unlock()
	for _, ch := range cancelled {
		ch <- Decision{}
	}
	return len(cancelled)
}
