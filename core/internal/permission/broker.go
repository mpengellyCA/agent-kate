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
}

// New returns an empty Broker.
func New() *Broker {
	return &Broker{pending: make(map[string]chan Decision)}
}

// Open registers a new pending request and returns its id and a channel that
// will receive the decision exactly once.
func (b *Broker) Open() (string, chan Decision) {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	id := "perm-" + hex.EncodeToString(raw[:])
	ch := make(chan Decision, 1)

	b.mu.Lock()
	b.pending[id] = ch
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
	b.mu.Unlock()
}
