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

	Browser  string   `json:"browser,omitempty"`  // launchBrowser: the browser that was opened
	Browsers []string `json:"browsers,omitempty"` // launchBrowser: names the user has configured

	// --- inject: per-batch playback outcome (audit F3, absolute half) ----------------
	// The UI cannot always play what it was handed: an absolute move whose point lies
	// inside no captured screen has no stream node to address and is DROPPED. Silently
	// swallowing that desynced the core's pointer mirror from the true cursor — the core
	// recorded the requested point while the cursor never moved — which is the same
	// bypass class as stale relative motion. So the UI reports what actually happened and
	// the core believes THIS, not its own request:
	//   OpsApplied/OpsDropped — ops that provably ran / provably did not.
	//   PtrKnown + PtrX/PtrY  — the last absolute move that actually landed.
	// A reply with no outcome fields (an older UI, or a non-inject kind) reads as
	// PtrKnown=false, which fails closed: the mirror is destroyed rather than trusted.
	OpsApplied int  `json:"opsApplied,omitempty"`
	OpsDropped int  `json:"opsDropped,omitempty"`
	PtrKnown   bool `json:"ptrKnown,omitempty"`
	PtrX       int  `json:"ptrX,omitempty"`
	PtrY       int  `json:"ptrY,omitempty"`
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
