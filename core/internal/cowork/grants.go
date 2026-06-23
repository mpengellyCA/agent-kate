// Package cowork is the consent authority and state for the KDE Plasma Cowork
// feature: durable, capability-scoped, revocable, per-thread grants that gate every
// desktop see/control action, plus a tamper-evident audit log and a global
// kill-switch. It is the policy brain; raw D-Bus lives in core/internal/kde.
//
// Security note (see docs/plans/08-kde-cowork/08-review-findings.md §A/§F): the agent
// runs at the same uid as akcore, so these files are not beyond its reach. v1 ships
// "detect, not prevent": GrantedBy is always re-derived server-side (never trusted
// from disk), the audit chain is verified on load and tampering fails closed, and a
// global kill-switch + active-grants surface give the user control. True
// tamper-prevention (privilege separation) is scheduled as v2 hardening.
package cowork

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"

	"golang.org/x/sys/unix"
)

// Capability is one independently-consented desktop power. Tier is derived, never
// stored as an input (plan 07 §1.5).
type Capability string

const (
	CapWindowList     Capability = "window_list"     // R0
	CapVDSandbox      Capability = "vd_sandbox"      // R0
	CapA11yRead       Capability = "a11y_read"       // R1
	CapScreenshot     Capability = "screenshot"      // R1
	CapScreencast     Capability = "screencast"      // R1
	CapLaunchBrowser  Capability = "launch_browser"  // R1 — open a user-configured browser
	CapA11yAction     Capability = "a11y_action"     // R2
	CapInputInject    Capability = "input_inject"    // R2
	CapPointerControl Capability = "pointer_control" // R2 — move & click the pointer at absolute coords
	CapRemoteDesktop  Capability = "remote_desktop"  // R2 — reserved, unused (08 §Q5)
)

// Tier drives prompt strength and default scope.
type Tier string

const (
	TierR0 Tier = "R0" // passive metadata
	TierR1 Tier = "R1" // read content (can capture secrets)
	TierR2 Tier = "R2" // control (arbitrary action as the user)
)

// TierOf is the fixed capability→tier table.
func TierOf(c Capability) Tier {
	switch c {
	case CapWindowList, CapVDSandbox:
		return TierR0
	case CapA11yRead, CapScreenshot, CapScreencast, CapLaunchBrowser:
		return TierR1
	case CapA11yAction, CapInputInject, CapPointerControl, CapRemoteDesktop:
		return TierR2
	default:
		return TierR2 // unknown ⇒ treat as highest risk (fail-safe)
	}
}

// Valid reports whether c is a known capability.
func (c Capability) Valid() bool {
	switch c {
	case CapWindowList, CapVDSandbox, CapA11yRead, CapScreenshot, CapScreencast,
		CapLaunchBrowser, CapA11yAction, CapInputInject, CapPointerControl, CapRemoteDesktop:
		return true
	}
	return false
}

// Scope is how long/broad a grant lives. Chosen by the USER at grant time.
type Scope string

const (
	ScopeOnce         Scope = "once"          // single use, then revoked
	ScopeSession      Scope = "session"       // until thread ends / restart
	ScopeTimed        Scope = "timed"         // until ExpiresAt
	ScopeUntilRevoked Scope = "until_revoked" // persists across restarts until revoked
)

// TargetKind / Target narrow a grant to a specific surface (plan 07 §1.5, C3).
type TargetKind string

const (
	TargetWindow   TargetKind = "window"   // KWin internalId
	TargetApp      TargetKind = "app"      // resourceClass
	TargetScreen   TargetKind = "screen"   // output name
	TargetRegion   TargetKind = "region"   // a screen rect
	TargetVDesktop TargetKind = "vdesktop" // virtual-desktop id
	TargetSandbox  TargetKind = "sandbox"  // a vd_sandbox session id
	TargetAny      TargetKind = "any"      // no spatial target (window_list)
)

type Rect struct {
	X, Y, W, H int
}

type Target struct {
	Kind          TargetKind `json:"kind"`
	WindowID      string     `json:"windowId,omitempty"`
	ResourceClass string     `json:"resourceClass,omitempty"`
	Screen        string     `json:"screen,omitempty"`
	Region        *Rect      `json:"region,omitempty"`
	VDesktopID    string     `json:"vdesktopId,omitempty"`
	SandboxID     string     `json:"sandboxId,omitempty"`
	Label         string     `json:"label,omitempty"`
}

// Grant is a recorded user consent. GrantedBy is always "user" and is re-derived
// server-side on Add — the on-disk value is never trusted (08 §A1).
type Grant struct {
	ID           string     `json:"id"`
	ThreadID     string     `json:"threadId"`
	Capability   Capability `json:"capability"`
	Target       Target     `json:"target"`
	Scope        Scope      `json:"scope"`
	Tier         Tier       `json:"tier"`
	GrantedAt    time.Time  `json:"grantedAt"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	RevokeReason string     `json:"revokeReason,omitempty"`
	GrantedBy    string     `json:"grantedBy"` // always "user"; server-derived
	Redact       bool       `json:"redact,omitempty"`
}

// Active reports whether g currently authorizes action at time now.
func (g *Grant) Active(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
		return false
	}
	return true
}

func (g *Grant) clone() *Grant {
	c := *g
	if g.ExpiresAt != nil {
		t := *g.ExpiresAt
		c.ExpiresAt = &t
	}
	if g.RevokedAt != nil {
		t := *g.RevokedAt
		c.RevokedAt = &t
	}
	if g.Target.Region != nil {
		r := *g.Target.Region
		c.Target.Region = &r
	}
	return &c
}

const storeSchemaVersion = 1

type fileModel struct {
	SchemaVersion int      `json:"schemaVersion"`
	Grants        []*Grant `json:"grants"`
}

// Store is the durable grant table. All access is mutex-guarded and copy-out;
// cross-process mutations are serialized with a flock on a sidecar lock file.
type Store struct {
	mu       sync.Mutex
	path     string
	grants   map[string]*Grant
	onChange func() // fired (outside the lock) after any mutation
}

// LoadStore reads the grant file (empty if absent), applies restart semantics
// (only until_revoked grants that are still active survive a restart — session/
// timed/once grants and any live capture are dropped), and rewrites the pruned set.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, grants: map[string]*Grant{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("cowork: read grants: %w", err)
	}
	var fm fileModel
	if err := json.Unmarshal(b, &fm); err != nil {
		// Corruption: do NOT silently inherit unknown state. Start empty, leave the
		// bad file aside for inspection, and surface via the returned error wrapper.
		_ = os.Rename(path, path+".corrupt")
		return s, fmt.Errorf("cowork: grants file corrupt (moved to %s.corrupt), starting empty: %w", path, err)
	}
	if fm.SchemaVersion > storeSchemaVersion {
		return s, fmt.Errorf("cowork: grants schema v%d newer than supported v%d; refusing to load (no grants active)", fm.SchemaVersion, storeSchemaVersion)
	}
	now := time.Now()
	for _, g := range fm.Grants {
		if g == nil || g.ID == "" {
			continue
		}
		// Restart semantics: keep only durable, still-active grants.
		if g.Scope != ScopeUntilRevoked || !g.Active(now) {
			continue
		}
		g.GrantedBy = "user" // never trust on-disk provenance
		s.grants[g.ID] = g
	}
	// Persist the pruned set so the file reflects reality after a restart.
	if err := s.saveLocked(); err != nil {
		return s, err
	}
	return s, nil
}

// SetOnChange registers a callback fired after mutations (e.g. to Notify the UI).
func (s *Store) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *Store) lockPath() string { return s.path + ".lock" }

// withFileLock serializes read-modify-write across concurrent core instances.
func (s *Store) withFileLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("cowork: open lock: %w", err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("cowork: flock: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

func (s *Store) saveLocked() error {
	return s.withFileLock(func() error {
		gs := make([]*Grant, 0, len(s.grants))
		for _, g := range s.grants {
			gs = append(gs, g)
		}
		sort.Slice(gs, func(i, j int) bool { return gs[i].GrantedAt.Before(gs[j].GrantedAt) })
		b, err := json.MarshalIndent(fileModel{SchemaVersion: storeSchemaVersion, Grants: gs}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
			return err
		}
		tmp := s.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, s.path)
	})
}

// Add records a new user-approved grant. GrantedBy and Tier are server-derived.
func (s *Store) Add(threadID string, cap Capability, t Target, scope Scope, expiresAt *time.Time, redact bool) (*Grant, error) {
	g := &Grant{
		ID:         "grant-" + randHex(8),
		ThreadID:   threadID,
		Capability: cap,
		Target:     t,
		Scope:      scope,
		Tier:       TierOf(cap),
		GrantedAt:  time.Now(),
		ExpiresAt:  expiresAt,
		GrantedBy:  "user",
		Redact:     redact,
	}
	s.mu.Lock()
	s.grants[g.ID] = g
	err := s.saveLocked()
	out := g.clone()
	s.mu.Unlock()
	s.fireOnChange()
	return out, err
}

// Match returns an active grant for thread+capability whose target covers want, or
// nil. Matching is strict: a grant's target must cover the requested target.
func (s *Store) Match(threadID string, cap Capability, want Target, now time.Time) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.grants {
		if g.ThreadID != threadID || g.Capability != cap || !g.Active(now) {
			continue
		}
		if targetCovers(g.Target, want) {
			return g.clone()
		}
	}
	return nil
}

// ConsumeOnce revokes a once-scoped grant after a successful use.
func (s *Store) ConsumeOnce(id string) {
	s.mu.Lock()
	g := s.grants[id]
	if g != nil && g.Scope == ScopeOnce && g.RevokedAt == nil {
		now := time.Now()
		g.RevokedAt = &now
		g.RevokeReason = "consumed (once)"
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	s.fireOnChange()
}

// Revoke marks a single grant revoked. Returns the revoked grant copy or nil.
func (s *Store) Revoke(id, reason string) *Grant {
	s.mu.Lock()
	g := s.grants[id]
	var out *Grant
	if g != nil && g.RevokedAt == nil {
		now := time.Now()
		g.RevokedAt = &now
		g.RevokeReason = reason
		_ = s.saveLocked()
		out = g.clone()
	}
	s.mu.Unlock()
	if out != nil {
		s.fireOnChange()
	}
	return out
}

// RevokeThread revokes every active grant for a thread. Returns revoked ids.
func (s *Store) RevokeThread(threadID, reason string) []string {
	s.mu.Lock()
	now := time.Now()
	var ids []string
	for _, g := range s.grants {
		if g.ThreadID == threadID && g.RevokedAt == nil {
			g.RevokedAt = &now
			g.RevokeReason = reason
			ids = append(ids, g.ID)
		}
	}
	if len(ids) > 0 {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	if len(ids) > 0 {
		s.fireOnChange()
	}
	return ids
}

// RevokeAll revokes every active grant (kill-switch). Returns revoked ids.
func (s *Store) RevokeAll(reason string) []string {
	s.mu.Lock()
	now := time.Now()
	var ids []string
	for _, g := range s.grants {
		if g.RevokedAt == nil {
			g.RevokedAt = &now
			g.RevokeReason = reason
			ids = append(ids, g.ID)
		}
	}
	if len(ids) > 0 {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	if len(ids) > 0 {
		s.fireOnChange()
	}
	return ids
}

// SweepExpired revokes grants that have passed their expiry. Returns revoked ids.
func (s *Store) SweepExpired() []string {
	s.mu.Lock()
	now := time.Now()
	var ids []string
	for _, g := range s.grants {
		if g.RevokedAt == nil && g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
			g.RevokedAt = g.ExpiresAt
			g.RevokeReason = "expired"
			ids = append(ids, g.ID)
		}
	}
	if len(ids) > 0 {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	if len(ids) > 0 {
		s.fireOnChange()
	}
	return ids
}

// List returns grant copies, optionally filtered by thread, newest first.
func (s *Store) List(threadID string) []*Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Grant
	for _, g := range s.grants {
		if threadID != "" && g.ThreadID != threadID {
			continue
		}
		out = append(out, g.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GrantedAt.After(out[j].GrantedAt) })
	return out
}

func (s *Store) fireOnChange() {
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// targetCovers reports whether a grant target g authorizes action on the requested
// target w. Strict: a window grant only covers the same window; an app grant covers
// any window of that resourceClass; "any" covers window_list-style requests.
func targetCovers(g, w Target) bool {
	switch g.Kind {
	case TargetAny:
		return true
	case TargetWindow:
		return w.Kind == TargetWindow && w.WindowID != "" && g.WindowID == w.WindowID
	case TargetApp:
		return g.ResourceClass != "" && g.ResourceClass == w.ResourceClass
	case TargetScreen:
		return w.Kind == TargetScreen && g.Screen == w.Screen
	case TargetVDesktop:
		return w.Kind == TargetVDesktop && g.VDesktopID == w.VDesktopID
	case TargetSandbox:
		return g.SandboxID != "" && g.SandboxID == w.SandboxID
	case TargetRegion:
		return w.Kind == TargetRegion && g.Region != nil && w.Region != nil && *g.Region == *w.Region
	}
	return false
}

// DefaultGrantsPath / DefaultAuditPath mirror session.DefaultPath's data dir.
func DefaultGrantsPath() string { return dataFile("cowork-consents.json") }
func DefaultAuditPath() string  { return dataFile("cowork-audit.jsonl") }

func dataFile(name string) string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", name)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
