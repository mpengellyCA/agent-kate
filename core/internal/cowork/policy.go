package cowork

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const policySchemaVersion = 1

type policyFile struct {
	SchemaVersion int          `json:"schemaVersion"`
	Enabled       []Capability `json:"enabled"`
}

// Policy is the global capability pre-authorization posture — the Cowork panel's
// toggle switchboard (Phase 2, "global posture" + "full no-prompt while on"). A
// capability present in the policy is allowed for ANY cowork-enabled agent with NO
// per-action prompt; it still passes the kill-switch, audit-tamper, and
// self-target hard guards, and every pre-authorized action is written to the audit
// log. It is the user's standing decision, so it overrides even the R2 per-action
// default. The kill-switch clears it (a true panic button).
type Policy struct {
	mu       sync.Mutex
	path     string
	enabled  map[Capability]bool
	onChange func()
}

// DefaultPolicyPath mirrors the grants/audit data dir.
func DefaultPolicyPath() string { return dataFile("cowork-policy.json") }

// LoadPolicy reads the policy file (empty/deny-all if absent or corrupt).
func LoadPolicy(path string) (*Policy, error) {
	p := &Policy{path: path, enabled: map[Capability]bool{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, fmt.Errorf("cowork: read policy: %w", err)
	}
	var pf policyFile
	if err := json.Unmarshal(b, &pf); err != nil {
		// Corruption ⇒ deny-all (do not inherit unknown state); set the bad file aside.
		_ = os.Rename(path, path+".corrupt")
		return p, nil
	}
	for _, c := range pf.Enabled {
		if c.Valid() {
			p.enabled[c] = true
		}
	}
	return p, nil
}

// SetOnChange registers a callback fired (outside the lock) after any mutation.
func (p *Policy) SetOnChange(fn func()) {
	p.mu.Lock()
	p.onChange = fn
	p.mu.Unlock()
}

// Allows reports whether capability c is globally pre-authorized.
func (p *Policy) Allows(c Capability) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled[c]
}

// List returns the set of enabled capabilities (copy-out).
func (p *Policy) List() map[Capability]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[Capability]bool, len(p.enabled))
	for c, v := range p.enabled {
		if v {
			out[c] = true
		}
	}
	return out
}

// Set turns a capability toggle on or off and persists.
func (p *Policy) Set(c Capability, on bool) error {
	if !c.Valid() {
		return fmt.Errorf("cowork: unknown capability %q", c)
	}
	p.mu.Lock()
	if on {
		p.enabled[c] = true
	} else {
		delete(p.enabled, c)
	}
	err := p.saveLocked()
	p.mu.Unlock()
	p.fireOnChange()
	return err
}

// Clear disables every toggle (the kill-switch panic button calls this).
func (p *Policy) Clear() {
	p.mu.Lock()
	had := len(p.enabled) > 0
	p.enabled = map[Capability]bool{}
	if had {
		_ = p.saveLocked()
	}
	p.mu.Unlock()
	if had {
		p.fireOnChange()
	}
}

func (p *Policy) saveLocked() error {
	caps := make([]Capability, 0, len(p.enabled))
	for c, v := range p.enabled {
		if v {
			caps = append(caps, c)
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	b, err := json.MarshalIndent(policyFile{SchemaVersion: policySchemaVersion, Enabled: caps}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

func (p *Policy) fireOnChange() {
	p.mu.Lock()
	fn := p.onChange
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// AllToggleable lists every capability the UI surfaces as a switch, in display
// order (read capabilities first, then control). remote_desktop is reserved/unused
// and intentionally excluded.
func AllToggleable() []Capability {
	return []Capability{
		CapWindowList,
		CapScreenshot,
		CapA11yRead,
		CapScreencast,
		CapVDSandbox,
		CapA11yAction,
		CapInputInject,
	}
}
