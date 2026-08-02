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

// Notifier pushes events to the UI. Wired to ipc.Server in main; the two
// methods differ in WHO receives the frame:
//
//   - Notify reaches every connection, agent bridges included. Correct only for
//     payload-free state changes that any client may safely learn.
//   - NotifyUI reaches connections that identified as the UI and nobody else.
//     Required for anything carrying a request id or the CONTENT of an action —
//     a broadcast consent prompt hands every other agent in the arena the id to
//     race the human on, plus a description of what the asking agent is doing
//     (audit F6, third site).
type Notifier interface {
	Notify(method string, params any)
	NotifyUI(method string, params any)
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

// open reports how many consent prompts are currently in front of the human.
func (b *grantBroker) open() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
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
	a := &Authority{
		store:     store,
		audit:     audit,
		policy:    policy,
		broker:    newGrantBroker(),
		notify:    notify,
		log:       log,
		teardowns: map[string]func(){},
		// Both spellings: KWin reports the Wayland app_id as either the reverse-DNS
		// desktop name or the bare component name depending on how the app set it.
		selfClasses:       map[string]bool{"org.kde.agentkate": true, "agentkate": true},
		selfPIDs:          map[int]bool{},
		promptTimeoutR0R1: 5 * time.Minute,
		promptTimeoutR2:   3 * time.Minute,
	}
	// SECURITY (audit F35): the panic button keeps its position across a restart. It was
	// in-memory only, so quitting akcore silently un-pressed it and the panel came back
	// claiming desktop access was live. Re-arming stays an explicit, confirmed human act.
	if policy != nil {
		a.killed = policy.Killed()
	}
	return a
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
	// SECURITY (audit F35): Agent Kate's own UI is off limits to EVERY capability, not
	// only control. This refusal used to be gated on tier == TierR2, which left
	// desktop_screenshot (R1) free to hand an agent a pixel-exact picture of the consent
	// dialog, the policy toggles and the kill switch — exactly the targeting data the
	// F25/F26 pointer attacks need, obtained through the one read capability round 1 left
	// permitted after it closed the a11y read path.
	//
	// Scope, stated plainly — and this check is WEAKER than it looks, so read it as what it
	// is rather than as the guard for the whole class:
	//
	//   - It inspects Target.ResourceClass and Target.Label, both of which the AGENT writes,
	//     and a Target carries no PID at all. So it cannot be the whole answer for any caller
	//     that names a window: an agent supplying a window id with a blank or borrowed class
	//     walks straight through here. Every such caller must ALSO resolve the id against
	//     live compositor data and check the owner — resolveA11yReadWindow and
	//     resolveCaptureTarget (package main) are the two that do, and both fail closed.
	//     This one is the backstop, not the gate.
	//   - A full-screen capture names no window at all, so nothing here can see it, and our
	//     windows are still in those pixels. The enforceable answer for that case is
	//     redaction inside the capture pipeline, where the frame and the live rects both
	//     exist; SelfWindowRects is the query for it and cowork.screenshot now hands the
	//     rectangles to the UI, but the blackout itself is not implemented yet — so a
	//     full-screen screenshot DOES still show whatever of Agent Kate is on screen. What is
	//     enforced meanwhile is the prompt-pending rule immediately below (the sharpest frame
	//     — the dialog being answered right now — is refused outright) and everything
	//     downstream: IsSelfPoint refuses any click or keystroke that lands on us however the
	//     agent worked out where we are, and the a11y read/action guards refuse our windows
	//     by identity. Pixels of our UI are an information leak, not a route to act on it.
	//   - desktop_screencast inherits all of the above the day it lands.
	if a.IsSelfTarget(req.Target) {
		a.auditDeny(req, "refused: target is Agent Kate's own UI")
		return Decision{Reason: "refused: Agent Kate cannot read or act on its own interface"}, nil
	}

	// SECURITY (audit F35, plan 29): no capture while a consent prompt is on screen.
	//
	// The self-target refusal above closes the capture that NAMES one of our windows. A
	// full-screen capture names nothing, so it cannot see one — and the sharpest thing in
	// those pixels is the dialog the human is answering RIGHT NOW: its exact wording, the
	// phrase the R2 dialog asks them to type, the position of every button, and, when the
	// prompt belongs to a DIFFERENT agent, what that agent is doing — the cross-agent read
	// channel NotifyUI was introduced to close (audit F6), reopened through a camera.
	//
	// Unlike frame redaction (which needs the pixels, and so the UI process — see
	// resolveCaptureTarget), this is enforceable right here, and it costs nothing in normal
	// use: prompts are rare, last seconds, and the refusal asks for a plain retry. It sits
	// ABOVE the policy toggle deliberately — a standing "always allow screenshots" is not a
	// decision to photograph the next decision.
	//
	// Known residue: it covers the prompts THIS broker owns (every capability grant). The
	// "let this agent use the desktop at all" dialog is raised through the permission broker
	// in package main (askCoworkEnable), which this object cannot see, so a capture is not
	// paused for that one. Closing it needs a shared "a decision is on screen" signal the
	// consent authority can read — handed on rather than faked here.
	if capturesTheFrame(req.Capability) && a.broker != nil && a.broker.open() > 0 {
		a.auditDeny(req, "refused: a consent prompt was on screen")
		return Decision{Reason: "refused: an Agent Kate consent prompt is open on the user's screen right now, and a capture would include it — nothing was captured; try again in a moment"}, nil
	}

	// Global pre-authorization (the toggle switchboard — Phase 2). A capability the
	// user has switched on is allowed with NO prompt, for any cowork-enabled agent,
	// overriding even the R2 per-action default. The kill-switch / audit-tamper /
	// self-target guards above still hard-block it.
	//
	// SECURITY (audit F35, plan 29): NOTHING is written to the audit log here, and nothing
	// at the reused-grant branch below either. Both used to append an AuditAction entry —
	// the kind whose meaning is "a granted capability was EXERCISED" — reading
	// "pre-authorized (toggle on)" / "reused existing grant", BEFORE the capability action
	// had happened, and often before it could: the geometric guard, the focus guard, the
	// compiler and the portal all refuse downstream of this point. The consent model treats
	// this log as authoritative and the panel shows it to the user as the record of what
	// agents DID, so an entry claiming an action occurred when it may not have is the same
	// record-before-outcome lie SetPolicy carried.
	//
	// The outcome is recorded where it is known instead: every caller of Authorize follows a
	// successful action with AuditCapture, whose GrantID is "policy" for a toggle and the
	// grant's own id for a reuse — so a pre-authorized use is still identifiable in the log,
	// and now appears once, when it really happened, rather than twice, once in advance. A
	// refusal after this point is recorded by AuditRefusal at the site that refuses.
	if a.policy != nil && a.policy.Allows(req.Capability) {
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
			return Decision{Allow: true, GrantID: g.ID, Reason: "existing grant"}, nil
		}
	}

	if a.notify == nil || a.broker == nil {
		a.auditDeny(req, "no consent UI available")
		return Decision{Reason: "no consent UI available"}, nil
	}

	reqID, ch := a.broker.Open()
	defer a.broker.Close(reqID)
	// UI-only, deliberately (audit F6): the payload carries the broker request
	// id and the literal action being asked about — the window, the element,
	// the text destined for a field. Broadcast, it would let any other agent's
	// bridge read one agent's desktop activity and answer the human's prompt
	// before they can. The consent dialog is the human's, so the frame is too.
	a.notify.NotifyUI("cowork.grantRequested", a.grantRequestPayload(reqID, req, tier))

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
			// SECURITY (audit F35): record the OUTCOME. The user said allow, the grant did
			// not happen, and nothing at all used to be written — so the log showed neither
			// the request nor its refusal. Whatever the human answered, no access was given.
			a.auditDeny(req, "refused: failed to persist grant: "+err.Error())
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

// capturesTheFrame reports whether a capability returns PIXELS of whatever is on screen, as
// opposed to a named, guardable object. Those are the capabilities whose target refusal
// cannot see what is in the frame, so they carry the prompt-pending rule above.
// desktop_screencast inherits it the day it lands, which is why it is listed now.
func capturesTheFrame(c Capability) bool {
	return c == CapScreenshot || c == CapScreencast
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
		"requestId":      reqID,
		"threadId":       req.ThreadID,
		"capability":     string(req.Capability),
		"riskTier":       string(tier),
		"target":         req.Target,
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
		// …and latch the button down on disk, so a restart does not silently un-press it
		// (audit F35). Best effort by necessity — the in-memory kill above is already in
		// force for this run — but loud, because a kill that lapses at the next launch is
		// exactly the surprise this is meant to remove.
		if perr := a.policy.SetKilled(true); perr != nil {
			a.log.Error("cowork: could not persist kill-switch state; it will not survive a restart", "err", perr)
		}
	}
	a.appendAudit(AuditEntry{Kind: AuditKill, Detail: reason})
	if a.notify != nil {
		// restoreDesktopFlags is the kill-switch's contract with the UI: "stop ALL desktop
		// access" must also put the desktop-wide org.a11y.Status flags (IsEnabled /
		// ScreenReaderEnabled) that Cowork flipped back the way the user had them — not
		// only tear down the RemoteDesktop session. Anything less leaves the whole session's
		// accessibility bus switched on after the human hit the panic button.
		// The UI half lives in ui/src/cowork/CoworkPortal.cpp (restoreAtspiStatus).
		a.notify.NotifyUI("cowork.killSwitch", map[string]any{
			"on":                  true,
			"reason":              reason,
			"at":                  time.Now(),
			"restoreDesktopFlags": true,
		})
	}
	return ids
}

// Shutdown runs the live-session teardowns for a graceful akcore exit. Unlike Kill
// (the panic button), it must NOT clear the user's standing policy toggles, revoke
// durable grants, or write a "kill" audit entry — a normal quit is not a panic. Ephemeral
// (session/once) grants lapse naturally via LoadStore's restart semantics on next launch.
func (a *Authority) Shutdown() {
	a.mu.Lock()
	teardowns := make([]func(), 0, len(a.teardowns))
	for id, fn := range a.teardowns {
		teardowns = append(teardowns, fn)
		delete(a.teardowns, id)
	}
	a.mu.Unlock()
	for _, fn := range teardowns {
		runTeardown(fn)
	}
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
	// SECURITY (audit F35): record the OUTCOME, never the intent. This used to append the
	// entry BEFORE calling Set, so F32's refusal path (a capability with no tool behind it
	// can no longer be armed) left an AuditGrant reading "policy toggle on" for a toggle
	// that was then REFUSED — the activity log claiming a standing no-prompt grant exists
	// when it does not. The state is now re-read from the policy after the write, so the
	// entry says what actually happened and a refusal is recorded as a refusal.
	err := a.policy.Set(c, on)
	armed := a.policy.Allows(c)
	e := AuditEntry{Capability: c}
	switch {
	case armed:
		e.Kind, e.Detail = AuditGrant, "policy toggle on"
	case on:
		// Asked to arm, and it is not armed: a refusal, not a grant.
		e.Kind, e.Detail = AuditDeny, "policy toggle on refused"
	default:
		e.Kind, e.Detail = AuditRevoke, "policy toggle off"
	}
	if err != nil {
		e.Detail += " (" + err.Error() + ")"
	}
	a.appendAudit(e)
	return err
}

// Rearm re-enables access after a kill (grants are NOT restored).
func (a *Authority) Rearm(reason string) {
	a.mu.Lock()
	a.killed = false
	a.mu.Unlock()
	// Un-latch on disk before announcing it, so the audit entry and the notification
	// describe a state that survives a restart (audit F35).
	if a.policy != nil {
		if perr := a.policy.SetKilled(false); perr != nil {
			a.log.Error("cowork: could not persist kill-switch re-arm", "err", perr)
		}
	}
	a.appendAudit(AuditEntry{Kind: AuditRearm, Detail: reason})
	if a.notify != nil {
		a.notify.NotifyUI("cowork.killSwitch", map[string]any{"on": false, "reason": reason, "at": time.Now()})
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
		a.appendAudit(AuditEntry{Kind: AuditRevoke, ThreadID: g.ThreadID, Capability: g.Capability, GrantID: id, Detail: reason})
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
		a.appendAudit(AuditEntry{Kind: AuditRevoke, ThreadID: threadID, Detail: reason})
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
//
// A Target carries no PID, so this check alone is NOT sufficient for any caller that
// can obtain the owning process: a window whose resourceClass we failed to read looks
// innocent here. Such callers must ALSO run IsSelfPID / IsSelfWindow and fail closed
// when neither piece of evidence can be gathered (see the R2 keyboard and AT-SPI guards
// in package main).
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

// IsSelfPID reports whether pid is one of Agent Kate's own processes (as registered by
// SetSelfIdentity). PID evidence is stronger than the window class: it comes straight
// from the AT-SPI element context or the KWin record and survives a failed / partial
// class lookup, which is exactly the case that used to let a self-target through.
// A non-positive pid is "unknown", never "not us" — callers must treat an unknown PID
// as unverified and fail closed themselves.
func (a *Authority) IsSelfPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selfPIDs[pid]
}

// IsSelfWindow reports whether a window identified by (pid, resourceClass) belongs to
// Agent Kate. It is the non-geometric sibling of IsSelfPoint: either piece of evidence
// is decisive on its own. Callers that cannot obtain EITHER must refuse the action —
// this function returning false only means "no evidence of self", not "verified safe".
func (a *Authority) IsSelfWindow(pid int, resourceClass string) bool {
	if a.IsSelfPID(pid) {
		return true
	}
	if resourceClass == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selfClasses[strings.ToLower(resourceClass)]
}

// WindowRect is a live KWin window rectangle in absolute desktop pixels, with the
// identity fields needed to decide whether it belongs to Agent Kate. Callers in
// package main translate kde.Window into this so cowork stays decoupled from kde.
type WindowRect struct {
	X, Y, W, H    int
	PID           int
	ResourceClass string
}

// IsSelfPoint reports whether the absolute desktop point (x,y) falls inside any
// Agent-Kate-owned window in wins — matched by the self PID set (and its ancestry
// is the caller's concern) or a self resourceClass. This is the geometric analogue
// of IsSelfTarget and is the defense that stops the agent from moving the pointer
// onto Agent Kate's own Allow/kill-switch buttons and clicking them. Half-open
// rect test [X, X+W) x [Y, Y+H). Returns false for an empty list.
func (a *Authority) IsSelfPoint(x, y int, wins []WindowRect) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, win := range wins {
		if win.W <= 0 || win.H <= 0 {
			continue
		}
		self := a.selfPIDs[win.PID] ||
			(win.ResourceClass != "" && a.selfClasses[strings.ToLower(win.ResourceClass)])
		if !self {
			continue
		}
		if x >= win.X && x < win.X+win.W && y >= win.Y && y < win.Y+win.H {
			return true
		}
	}
	return false
}

// SelfWindowRects returns the Agent-Kate-owned rectangles in wins (same PID / resourceClass
// evidence as IsSelfPoint, same half-open rect convention).
//
// SECURITY (audit F35): it is the list a capture path must black out before handing a frame
// to an agent. The self-target refusal in Authorize covers a capture that NAMES one of our
// windows; a full-screen capture names nothing, so our consent dialog, policy toggles and
// kill switch are still in the pixels and the refusal cannot see it. Redaction has to happen
// where the frame is, which is the portal capture path, not here — this is the query it
// needs. cowork.screenshot calls it (resolveCaptureTarget, package main) and ships the
// rectangles to the UI in the portal request as `redactRects`; the blackout itself is the
// UI half and is NOT implemented yet, so today this is a supplied precondition rather than
// an enforced one, and the gap is stated as such wherever it is visible.
// desktop_screencast inherits the same requirement when it lands.
// Zero-area rects are skipped; an empty result means "no evidence of self", which for a
// full-screen capture is not the same as "verified clean" — a caller that cannot enumerate
// windows at all must fail closed rather than assume nothing of ours is on screen.
func (a *Authority) SelfWindowRects(wins []WindowRect) []WindowRect {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []WindowRect
	for _, win := range wins {
		if win.W <= 0 || win.H <= 0 {
			continue
		}
		if a.selfPIDs[win.PID] ||
			(win.ResourceClass != "" && a.selfClasses[strings.ToLower(win.ResourceClass)]) {
			out = append(out, win)
		}
	}
	return out
}

// SelfRectsIntersecting returns the Agent-Kate-owned rectangles in wins that overlap the
// requested rectangle r (same identity evidence and same half-open convention as
// SelfWindowRects / IsSelfPoint).
//
// SECURITY (audit F35, round 4): this is the ENFORCEABLE half of the capture guard that
// SelfWindowRects is not. The window refusal covers a capture that names one of our
// windows; the full-frame case cannot be refused and waits on a blackout that does not
// exist yet. A capture whose frame is a NAMED RECTANGLE sits between the two and had
// neither: cowork.Target{Kind: "region"} is handed to KWin's CaptureArea verbatim, so an
// agent that cannot name our window can still draw a box around it and get the same
// pixel-exact picture of the consent dialog, the policy switches and the kill switch that
// the F25/F26 pointer attacks need. Unlike a full frame this needs no redaction: the rect
// is known before the shutter, so an overlap is simply refused.
//
// A non-positive r cannot be verified against anything, so it reports EVERY self window
// and the caller fails closed — an empty region silently becomes a whole-screen grab
// further down the pipeline (CoworkPortal::kwinCaptureCall falls back to
// CaptureActiveScreen), which is the one shape that must never pass as "a small region".
func (a *Authority) SelfRectsIntersecting(r Rect, wins []WindowRect) []WindowRect {
	self := a.SelfWindowRects(wins)
	if r.W <= 0 || r.H <= 0 {
		return self
	}
	var out []WindowRect
	for _, w := range self {
		// Half-open [X, X+W) x [Y, Y+H): touching edges do not overlap.
		if r.X < w.X+w.W && w.X < r.X+r.W && r.Y < w.Y+w.H && w.Y < r.Y+r.H {
			out = append(out, w)
		}
	}
	return out
}

// --- Pull surfaces ---------------------------------------------------------------

func (a *Authority) ListGrants(threadID string) ([]*Grant, bool) {
	return a.store.List(threadID), a.Killed()
}

func (a *Authority) ListAudit(threadID string, sinceSeq int64, limit int) ([]AuditEntry, int64, error) {
	return a.audit.Tail(threadID, sinceSeq, limit)
}

func (a *Authority) Tampered() bool { return a.audit.Tampered() }

// AuditRefusal records an action refused after consent (e.g. the geometric self-target
// guard caught a click aimed at Agent Kate's own UI) so the log shows the attempt, not
// only the successes.
func (a *Authority) AuditRefusal(threadID string, cap Capability, t Target, reason string) {
	a.appendAudit(AuditEntry{
		Kind: AuditDeny, ThreadID: threadID, Capability: cap, Target: &t, Detail: reason,
	})
}

// AuditCapture records a successful capture/read with a content hash (never content).
func (a *Authority) AuditCapture(threadID string, cap Capability, t Target, grantID, artifactHash string) {
	a.appendAudit(AuditEntry{
		Kind: AuditAction, ThreadID: threadID, Capability: cap, Target: &t,
		GrantID: grantID, ArtifactHash: artifactHash, Detail: "capture",
	})
}

// appendAudit writes one entry, and says so loudly when it cannot. Every caller used to
// discard the error (`_ = a.audit.Append(...)`), which meant a full disk or a permission
// problem silently produced a log with holes in it — and this log is what the consent
// model treats as authoritative (Authorize fails closed on a chain it cannot verify, and
// the panel presents it to the user as the record of what agents did). We still do not
// fail the operation on it: refusing to revoke a grant because we could not journal the
// revocation would be strictly worse.
func (a *Authority) appendAudit(e AuditEntry) {
	if err := a.audit.Append(e); err != nil {
		a.log.Error("cowork: audit append failed — the consent log is now incomplete",
			"kind", e.Kind, "capability", e.Capability, "threadId", e.ThreadID, "err", err)
	}
}

func (a *Authority) auditGrant(g *Grant) {
	a.appendAudit(AuditEntry{
		Kind: AuditGrant, ThreadID: g.ThreadID, Capability: g.Capability,
		Target: &g.Target, GrantID: g.ID, Detail: string(g.Scope),
	})
}

func (a *Authority) auditDeny(req AuthRequest, reason string) {
	t := req.Target
	a.appendAudit(AuditEntry{
		Kind: AuditDeny, ThreadID: req.ThreadID, Capability: req.Capability,
		Target: &t, Detail: reason,
	})
}

// (auditAction is deliberately gone: the only two callers wrote an "exercised" entry from
// inside Authorize, before the action had happened — see the record-before-outcome note
// there. AuditCapture, called by the handler once the action has really run, is the one
// place an AuditAction entry is written.)
