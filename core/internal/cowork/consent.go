package cowork

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

var errKilled = errors.New("cowork: desktop access disabled (kill-switch engaged)")

// Notifier pushes events to the UI (wired to ipc.Server.Notify in main).
type Notifier interface {
	Notify(method string, params any)
}

// ActionDescriptor is the concrete, literal action shown to the user for R2
// control prompts (plan 04 §3) — never a bare tool name.
type ActionDescriptor struct {
	Mechanism   string `json:"mechanism"` // "a11y_action" | "input_inject"
	AppName     string `json:"appName,omitempty"`
	WindowTitle string `json:"windowTitle,omitempty"`
	Element     string `json:"element,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// AuthRequest is the single input to Authorize (plan 07 §1.5, C1). Scope is NOT an
// input — the user chooses it at grant time; the caller passes only a suggestion.
type AuthRequest struct {
	ThreadID       string
	Capability     Capability
	Target         Target
	SuggestedScope Scope
	ActionPreview  *ActionDescriptor // R2 only
}

// Decision is the Authorize result.
type Decision struct {
	Allow   bool
	GrantID string
	Reason  string
}

// grantDecision is the user's answer routed back from the UI via respondGrant.
type grantDecision struct {
	Allow        bool
	Scope        Scope
	ExpiresInSec int
	Redact       bool
}

// grantBroker is the interactive-prompt rendezvous (the shape of permission.Broker,
// a distinct id namespace so the two never cross-resolve — plan 01 INV-2).
type grantBroker struct {
	mu      sync.Mutex
	pending map[string]chan grantDecision
}

func newGrantBroker() *grantBroker {
	return &grantBroker{pending: map[string]chan grantDecision{}}
}

func (b *grantBroker) Open() (string, chan grantDecision) {
	id := "grant-req-" + randHex(6)
	ch := make(chan grantDecision, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	return id, ch
}

func (b *grantBroker) Resolve(id string, d grantDecision) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- d:
	default:
	}
	return true
}

func (b *grantBroker) Close(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// Authority is the consent brain: grant store + audit + interactive prompt + kill
// switch + teardown registry + anti-escalation guards.
type Authority struct {
	store  *Store
	audit  *Audit
	policy *Policy
	broker *grantBroker
	notify Notifier
	log    *slog.Logger

	mu          sync.Mutex
	killed      bool
	teardowns   map[string]func() // grantID -> live-session teardown
	selfClasses map[string]bool   // AK's own window resourceClasses (lowercased)
	selfPIDs    map[int]bool

	// promptTimeout overrides the per-tier default (tests set it short).
	promptTimeoutR0R1 time.Duration
	promptTimeoutR2   time.Duration
}

func newAuthority(store *Store, audit *Audit, policy *Policy, notify Notifier, log *slog.Logger) *Authority {
	if log == nil {
		log = slog.Default()
	}
	return &Authority{
		store:             store,
		audit:             audit,
		policy:            policy,
		broker:            newGrantBroker(),
		notify:            notify,
		log:               log,
		teardowns:         map[string]func(){},
		selfClasses:       map[string]bool{"org.kde.agentkate": true},
		selfPIDs:          map[int]bool{},
		promptTimeoutR0R1: 5 * time.Minute,
		promptTimeoutR2:   3 * time.Minute,
	}
}

// Authorize is the single gate every cowork capability calls before acting. It is
// the imperative, server-side gate — it does NOT depend on Claude's
// --permission-prompt-tool funnel (08 §A/§B; plan 01 §4).
func (a *Authority) Authorize(ctx context.Context, req AuthRequest) (Decision, error) {
	if !req.Capability.Valid() {
		return Decision{Reason: "unknown capability"}, nil
	}
	tier := TierOf(req.Capability)

	if a.Killed() {
		a.auditDeny(req, "kill-switch engaged")
		return Decision{Reason: "desktop access is disabled (kill-switch engaged)"}, nil
	}
	if a.audit.Tampered() {
		// Fail closed: we cannot trust the consent history.
		a.log.Error("cowork: consent audit chain failed verification; denying all access")
		return Decision{Reason: "consent audit integrity check failed; desktop access disabled"}, nil
	}
	if tier == TierR2 && a.IsSelfTarget(req.Target) {
		a.auditDeny(req, "refused: target is Agent Kate's own UI")
		return Decision{Reason: "refused: Agent Kate cannot control its own interface"}, nil
	}

	// Global pre-authorization (the toggle switchboard — Phase 2). A capability the
	// user has switched on is allowed with NO prompt, for any cowork-enabled agent,
	// overriding even the R2 per-action default. Still audited; the kill-switch /
	// audit-tamper / self-target guards above still hard-block it.
	if a.policy != nil && a.policy.Allows(req.Capability) {
		a.auditAction(req, "policy", "", "pre-authorized (toggle on)")
		return Decision{Allow: true, GrantID: "policy", Reason: "pre-authorized (toggle on)"}, nil
	}

	now := time.Now()
	// R2 outside the sandbox is per-action: never satisfied by a remembered grant.
	rememberable := tier != TierR2 || req.Target.Kind == TargetSandbox
	if rememberable {
		if g := a.store.Match(req.ThreadID, req.Capability, req.Target, now); g != nil {
			if g.Scope == ScopeOnce {
				a.store.ConsumeOnce(g.ID)
			}
			a.auditAction(req, g.ID, "", "reused existing grant")
			return Decision{Allow: true, GrantID: g.ID, Reason: "existing grant"}, nil
		}
	}

	if a.notify == nil || a.broker == nil {
		a.auditDeny(req, "no consent UI available")
		return Decision{Reason: "no consent UI available"}, nil
	}

	reqID, ch := a.broker.Open()
	defer a.broker.Close(reqID)
	a.notify.Notify("cowork.grantRequested", a.grantRequestPayload(reqID, req, tier))

	timeout := a.promptTimeoutR0R1
	if tier == TierR2 {
		timeout = a.promptTimeoutR2
	}
	select {
	case d := <-ch:
		if !d.Allow {
			a.auditDeny(req, "denied by the user")
			return Decision{Reason: "denied by the user"}, nil
		}
		scope := d.Scope
		if scope == "" {
			scope = req.SuggestedScope
		}
		if scope == "" {
			scope = ScopeOnce
		}
		// R2 outside the sandbox is never remembered, regardless of what was asked.
		if tier == TierR2 && req.Target.Kind != TargetSandbox {
			scope = ScopeOnce
		}
		var exp *time.Time
		if scope == ScopeTimed {
			secs := d.ExpiresInSec
			if secs <= 0 {
				secs = 300
			}
			t := time.Now().Add(time.Duration(secs) * time.Second)
			exp = &t
		}
		g, err := a.store.Add(req.ThreadID, req.Capability, req.Target, scope, exp, d.Redact)
		if err != nil {
			return Decision{Reason: "failed to persist grant"}, err
		}
		a.auditGrant(g)
		return Decision{Allow: true, GrantID: g.ID, Reason: "granted by the user"}, nil
	case <-ctx.Done():
		a.auditDeny(req, "cancelled")
		return Decision{Reason: "cancelled"}, ctx.Err()
	case <-time.After(timeout):
		a.auditDeny(req, "consent prompt timed out")
		return Decision{Reason: "consent prompt timed out"}, nil
	}
}

// Respond delivers the user's decision (called by the cowork.respondGrant handler,
// which must verify the caller is the UI — origin check lives at the RPC layer).
func (a *Authority) Respond(requestID string, allow bool, scope Scope, expiresInSec int, redact bool) bool {
	return a.broker.Resolve(requestID, grantDecision{Allow: allow, Scope: scope, ExpiresInSec: expiresInSec, Redact: redact})
}

// GrantDirect records a grant the user created proactively from the UI's "Share…"
// affordance (no agent request to answer). The caller (RPC handler) must verify the
// origin is the UI. Returns the new grant.
func (a *Authority) GrantDirect(threadID string, cap Capability, t Target, scope Scope, expiresInSec int, redact bool) (*Grant, error) {
	if a.Killed() {
		return nil, errKilled
	}
	var exp *time.Time
	if scope == ScopeTimed {
		if expiresInSec <= 0 {
			expiresInSec = 300
		}
		tt := time.Now().Add(time.Duration(expiresInSec) * time.Second)
		exp = &tt
	}
	g, err := a.store.Add(threadID, cap, t, scope, exp, redact)
	if err != nil {
		return nil, err
	}
	a.auditGrant(g)
	return g, nil
}

func (a *Authority) grantRequestPayload(reqID string, req AuthRequest, tier Tier) map[string]any {
	suggested := req.SuggestedScope
	if suggested == "" {
		if tier == TierR2 {
			suggested = ScopeOnce
		} else {
			suggested = ScopeSession
		}
	}
	p := map[string]any{
		"requestId":     reqID,
		"threadId":      req.ThreadID,
		"capability":    string(req.Capability),
		"riskTier":      string(tier),
		"target":        req.Target,
		"suggestedScope": string(suggested),
	}
	if req.ActionPreview != nil {
		p["actionPreview"] = req.ActionPreview
	}
	return p
}

// --- Kill-switch -----------------------------------------------------------------

// Kill revokes all grants, tears down every live session, and disables new access.
func (a *Authority) Kill(reason string) []string {
	a.mu.Lock()
	a.killed = true
	teardowns := make([]func(), 0, len(a.teardowns))
	for id, fn := range a.teardowns {
		teardowns = append(teardowns, fn)
		delete(a.teardowns, id)
	}
	a.mu.Unlock()

	for _, fn := range teardowns {
		runTeardown(fn)
	}
	ids := a.store.RevokeAll("kill-switch: " + reason)
	// Panic button: also clear every pre-authorization toggle, so re-arming starts
	// from a clean (deny-all) posture rather than silently restoring standing access.
	if a.policy != nil {
		a.policy.Clear()
	}
	_ = a.audit.Append(AuditEntry{Kind: AuditKill, Detail: reason})
	if a.notify != nil {
		a.notify.Notify("cowork.killSwitch", map[string]any{"on": true, "reason": reason, "at": time.Now()})
	}
	return ids
}

// PolicyList returns the current global capability pre-authorizations.
func (a *Authority) PolicyList() map[Capability]bool {
	if a.policy == nil {
		return map[Capability]bool{}
	}
	return a.policy.List()
}

// SetPolicy turns a global capability toggle on or off (UI-only at the RPC layer).
func (a *Authority) SetPolicy(c Capability, on bool) error {
	if a.policy == nil {
		return nil
	}
	if on {
		_ = a.audit.Append(AuditEntry{Kind: AuditGrant, Capability: c, Detail: "policy toggle on"})
	} else {
		_ = a.audit.Append(AuditEntry{Kind: AuditRevoke, Capability: c, Detail: "policy toggle off"})
	}
	return a.policy.Set(c, on)
}

// Rearm re-enables access after a kill (grants are NOT restored).
func (a *Authority) Rearm(reason string) {
	a.mu.Lock()
	a.killed = false
	a.mu.Unlock()
	_ = a.audit.Append(AuditEntry{Kind: AuditRearm, Detail: reason})
	if a.notify != nil {
		a.notify.Notify("cowork.killSwitch", map[string]any{"on": false, "reason": reason, "at": time.Now()})
	}
}

func (a *Authority) Killed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.killed
}

// RevokeGrant revokes one grant and tears down any live session bound to it.
func (a *Authority) RevokeGrant(id, reason string) *Grant {
	g := a.store.Revoke(id, reason)
	if g != nil {
		a.runTeardownFor(id)
		_ = a.audit.Append(AuditEntry{Kind: AuditRevoke, ThreadID: g.ThreadID, Capability: g.Capability, GrantID: id, Detail: reason})
	}
	return g
}

// RevokeThread revokes every grant for a thread (e.g. on agent discard).
func (a *Authority) RevokeThread(threadID, reason string) []string {
	ids := a.store.RevokeThread(threadID, reason)
	for _, id := range ids {
		a.runTeardownFor(id)
	}
	if len(ids) > 0 {
		_ = a.audit.Append(AuditEntry{Kind: AuditRevoke, ThreadID: threadID, Detail: reason})
	}
	return ids
}

// --- Teardown registry (live portal/screencast/sandbox sessions) -----------------

func (a *Authority) RegisterTeardown(grantID string, fn func()) {
	a.mu.Lock()
	a.teardowns[grantID] = fn
	a.mu.Unlock()
}

func (a *Authority) runTeardownFor(grantID string) {
	a.mu.Lock()
	fn := a.teardowns[grantID]
	delete(a.teardowns, grantID)
	a.mu.Unlock()
	if fn != nil {
		runTeardown(fn)
	}
}

func runTeardown(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// --- Anti-escalation: never act on Agent Kate's own UI -----------------------------

func (a *Authority) SetSelfIdentity(classes []string, pids []int) {
	a.mu.Lock()
	for _, c := range classes {
		if c != "" {
			a.selfClasses[strings.ToLower(c)] = true
		}
	}
	for _, p := range pids {
		if p > 0 {
			a.selfPIDs[p] = true
		}
	}
	a.mu.Unlock()
}

// IsSelfTarget reports whether t points at Agent Kate's own UI (by resourceClass or
// label). Window-target callers must populate ResourceClass for this to apply.
func (a *Authority) IsSelfTarget(t Target) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t.ResourceClass != "" && a.selfClasses[strings.ToLower(t.ResourceClass)] {
		return true
	}
	if t.Label != "" && a.selfClasses[strings.ToLower(t.Label)] {
		return true
	}
	return false
}

// --- Pull surfaces ---------------------------------------------------------------

func (a *Authority) ListGrants(threadID string) ([]*Grant, bool) {
	return a.store.List(threadID), a.Killed()
}

func (a *Authority) ListAudit(threadID string, sinceSeq int64, limit int) ([]AuditEntry, int64, error) {
	return a.audit.Tail(threadID, sinceSeq, limit)
}

func (a *Authority) Tampered() bool { return a.audit.Tampered() }

// AuditCapture records a successful capture/read with a content hash (never content).
func (a *Authority) AuditCapture(threadID string, cap Capability, t Target, grantID, artifactHash string) {
	_ = a.audit.Append(AuditEntry{
		Kind: AuditAction, ThreadID: threadID, Capability: cap, Target: &t,
		GrantID: grantID, ArtifactHash: artifactHash, Detail: "capture",
	})
}

func (a *Authority) auditGrant(g *Grant) {
	_ = a.audit.Append(AuditEntry{
		Kind: AuditGrant, ThreadID: g.ThreadID, Capability: g.Capability,
		Target: &g.Target, GrantID: g.ID, Detail: string(g.Scope),
	})
}

func (a *Authority) auditDeny(req AuthRequest, reason string) {
	t := req.Target
	_ = a.audit.Append(AuditEntry{
		Kind: AuditDeny, ThreadID: req.ThreadID, Capability: req.Capability,
		Target: &t, Detail: reason,
	})
}

func (a *Authority) auditAction(req AuthRequest, grantID, artifactHash, detail string) {
	t := req.Target
	_ = a.audit.Append(AuditEntry{
		Kind: AuditAction, ThreadID: req.ThreadID, Capability: req.Capability,
		Target: &t, GrantID: grantID, ArtifactHash: artifactHash, Detail: detail,
	})
}
