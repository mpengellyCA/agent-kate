package kde

import "sync"

// PortalBroker correlates the core↔UI portal round-trip (plan 07 §1.4). The core
// can't run XDG portals (no Wayland surface); it borrows the primary UI by emitting
// a cowork.portalRequest notification keyed by a corrId and blocking here until the
// UI calls cowork.portalResult. It is the shape of permission.Broker with a distinct
// id namespace and a richer payload. FDs never cross — only tokens/node-ids/PNGs.
type PortalBroker struct {
	mu      sync.Mutex
	pending map[string]chan PortalResult
}

// PortalResult carries the serializable artifacts the UI returns for a portal op.
type PortalResult struct {
	CorrID       string `json:"corrId"`
	Kind         string `json:"kind"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	PNGB64       string `json:"pngB64,omitempty"` // screenshot / captureStill
	Mime         string `json:"mime,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	NodeID       int    `json:"nodeId,omitempty"`       // screencastStart
	CastToken    string `json:"castToken,omitempty"`    // screencastStart
	RestoreToken string `json:"restoreToken,omitempty"` // screencastStart (rotated)
}

func NewPortalBroker() *PortalBroker {
	return &PortalBroker{pending: map[string]chan PortalResult{}}
}

// Open mints a corrId and a buffered result channel.
func (b *PortalBroker) Open() (string, chan PortalResult) {
	corrID := "cap-" + randHex(6)
	ch := make(chan PortalResult, 1)
	b.mu.Lock()
	b.pending[corrID] = ch
	b.mu.Unlock()
	return corrID, ch
}

// Resolve delivers the UI's result for corrID exactly once. A late result (after
// timeout) is a harmless no-op.
func (b *PortalBroker) Resolve(corrID string, r PortalResult) bool {
	b.mu.Lock()
	ch, ok := b.pending[corrID]
	if ok {
		delete(b.pending, corrID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- r:
	default:
	}
	return true
}

func (b *PortalBroker) Close(corrID string) {
	b.mu.Lock()
	delete(b.pending, corrID)
	b.mu.Unlock()
}
