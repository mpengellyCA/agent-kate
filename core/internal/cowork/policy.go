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
	// KillSwitch is the panic button's latch. It lives here rather than in memory
	// only so that hitting "Stop ALL desktop access" survives an akcore restart —
	// see Policy.killed.
	KillSwitch bool `json:"killSwitch,omitempty"`
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

	// killed is the persisted kill-switch latch (audit F35). The panic button used to
	// be in-memory only, so an akcore restart silently un-pressed it: the panel came
	// back reading "Stop ALL desktop access" as if nothing had happened. The authority
	// after a restart was already identical (Kill revokes every grant and clears every
	// toggle before this file is written), so this is a LABELLING fix, not a widening
	// fix — but a panic button that reports itself as un-pressed is its own hazard.
	killed bool

	// stale records that the file on disk still lists entries LoadPolicy dropped. It
	// exists only so the next legitimate write is known to clean them up; loading
	// itself never touches the disk (audit F35 — see LoadPolicy).
	stale bool
}

// DefaultPolicyPath mirrors the grants/audit data dir.
func DefaultPolicyPath() string { return dataFile("cowork-policy.json") }

// LoadPolicy reads the policy file (empty/deny-all if absent or corrupt).
//
// SECURITY (audit F35): loading is READ-ONLY apart from setting a corrupt file aside.
// It used to rewrite the file in place whenever it dropped a stale entry, which made
// starting akcore against a read-only data dir a write attempt, and let a second
// akcore instance rewrite the file out from under the first one at startup. The
// in-memory posture is what enforcement reads and it is already correct (stale entries
// are dropped here AND re-denied by Allows), so nothing is gained by touching the disk
// at load time. The pruned set is written out by the next legitimate write — every
// Set/Clear serializes the live map, and p.stale makes sure a Clear with nothing live
// still performs that cleanup.
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
	p.killed = pf.KillSwitch
	dropped := false
	for _, c := range pf.Enabled {
		// SECURITY (audit F32): an entry for a capability that is not toggleable today
		// is dropped, not carried. Honouring it later would revive a standing no-prompt
		// grant the user set when the capability had no tool behind it — a
		// pre-authorization they will never be re-asked about.
		if c.Valid() && Toggleable(c) {
			p.enabled[c] = true
			continue
		}
		dropped = true
	}
	// Remember, do not rewrite: the next Set/Clear serializes only the live map, so the
	// stale entry leaves the disk then. It can never be honoured in the meantime —
	// Allows re-checks Toggleable on every read.
	p.stale = dropped
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
	// SECURITY (audit F32): the toggleable set is re-checked on every read, not only
	// at load/Set time. A capability that leaves the set (or was never in it) can
	// never be silently pre-authorized by a leftover entry — the answer is no.
	if !Toggleable(c) {
		return false
	}
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
	// SECURITY (audit F32): refuse to arm a toggle for a capability no tool implements.
	// Turning one OFF stays legal so a policy file written by an older build can always
	// be cleaned up.
	if on && !Toggleable(c) {
		return fmt.Errorf("cowork: capability %q cannot be pre-authorized", c)
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
	// p.stale: nothing live to clear, but the file still lists entries LoadPolicy
	// dropped — this is the legitimate write that prunes them (audit F35).
	if had || p.stale {
		_ = p.saveLocked()
	}
	p.mu.Unlock()
	if had {
		p.fireOnChange()
	}
}

// Killed reports the persisted kill-switch latch (audit F35). Read once at startup by
// newAuthority; the live answer during a run is Authority.Killed.
func (p *Policy) Killed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// SetKilled latches (or un-latches) the kill switch on disk so the panic button keeps
// its position across an akcore restart. Returns the write error rather than eating it:
// a kill we could not persist is a kill that quietly lapses at the next launch, and the
// caller logs it.
func (p *Policy) SetKilled(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed == on && !p.stale {
		return nil
	}
	p.killed = on
	return p.saveLocked()
}

func (p *Policy) saveLocked() error {
	caps := make([]Capability, 0, len(p.enabled))
	for c, v := range p.enabled {
		if v {
			caps = append(caps, c)
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	b, err := json.MarshalIndent(policyFile{
		SchemaVersion: policySchemaVersion, Enabled: caps, KillSwitch: p.killed}, "", "  ")
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
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	// Only live entries were just serialized, so whatever LoadPolicy dropped is now
	// gone from disk too (audit F35).
	p.stale = false
	return nil
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
// order (read capabilities first, then control).
//
// SECURITY (audit F32): a toggle here is a PERSISTED STANDING GRANT — allowed for
// any cowork-enabled agent with no per-action prompt, overriding even the R2
// per-action default. So a capability belongs here ONLY once a tool actually calls
// Authorize for it. remote_desktop is reserved/unused; screencast and vd_sandbox
// have no tool behind them yet ("land in v3", coworkToolDefs) — shipping switches
// for them would arm no-prompt grants the day those tools appear, from a decision
// the user made when the feature did not exist and will never be re-asked about.
// Add a capability here in the SAME change that lands its tool.
func AllToggleable() []Capability {
	return []Capability{
		CapWindowList,
		CapScreenshot,
		CapA11yRead,
		CapLaunchBrowser,
		CapA11yAction,
		CapInputInject,
		CapPointerControl,
	}
}

// Toggleable reports whether c may be pre-authorized by a standing policy toggle.
// It is the single source of truth behind AllToggleable(), and is enforced on load
// (stale entries dropped), on Set (refused) and on Allows (denied) — see F32.
func Toggleable(c Capability) bool {
	for _, t := range AllToggleable() {
		if t == c {
			return true
		}
	}
	return false
}
