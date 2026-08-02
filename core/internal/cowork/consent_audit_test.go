package cowork

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The consent audit log is what the model treats as authoritative: Authorize fails closed
// on a chain it cannot verify, and the Cowork panel shows it to the user as the record of
// what agents did and what they were allowed. So an entry that records an INTENT rather
// than an OUTCOME is not a cosmetic bug — it is the log asserting authority that was never
// granted (audit F35).

func auditEntries(t *testing.T, svc *Service) []AuditEntry {
	t.Helper()
	entries, _, err := svc.ListAudit("", 0, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return entries
}

// SECURITY (audit F35): SetPolicy appended the audit entry BEFORE calling Policy.Set, so
// F32's refusal path (a capability with no tool behind it cannot be armed) left a "grant"
// record reading "policy toggle on" for a toggle that was refused. The log would then
// claim a standing, no-prompt, any-agent pre-authorization existed for screencast.
func TestRefusedPolicyToggleIsAuditedAsARefusalNotAGrant(t *testing.T) {
	svc := newTestService(t, &fakeNotifier{})

	if err := svc.SetPolicy(CapScreencast, true); err == nil {
		t.Fatal("arming an unimplemented capability must be refused (F32)")
	}
	if svc.PolicyList()[CapScreencast] {
		t.Fatal("screencast must not be armed after a refused Set")
	}

	var refused bool
	for _, e := range auditEntries(t, svc) {
		if e.Capability != CapScreencast {
			continue
		}
		if e.Kind == AuditGrant {
			t.Fatalf("the audit log claims a standing grant for a REFUSED toggle: %+v", e)
		}
		if e.Kind == AuditDeny {
			refused = true
		}
	}
	if !refused {
		t.Fatal("a refused toggle must still be recorded — as a refusal")
	}
}

// The same code path must keep recording a real toggle correctly, so the fix cannot be
// "record nothing and pass".
func TestAcceptedPolicyToggleIsAuditedAsAGrantThenARevoke(t *testing.T) {
	svc := newTestService(t, &fakeNotifier{})

	if err := svc.SetPolicy(CapScreenshot, true); err != nil {
		t.Fatalf("SetPolicy on: %v", err)
	}
	if err := svc.SetPolicy(CapScreenshot, false); err != nil {
		t.Fatalf("SetPolicy off: %v", err)
	}

	var kinds []AuditKind
	for _, e := range auditEntries(t, svc) {
		if e.Capability == CapScreenshot {
			kinds = append(kinds, e.Kind)
		}
	}
	if len(kinds) != 2 || kinds[0] != AuditGrant || kinds[1] != AuditRevoke {
		t.Fatalf("expected [grant revoke] for the screenshot toggle, got %v", kinds)
	}
}

// SECURITY (audit F35): the grant-store write can fail, and when it did the request
// vanished from the log entirely — neither the ask nor its outcome was recorded, even
// though the human had answered. Whatever they answered, no access was given, so the
// outcome is a refusal and is recorded as one.
func TestGrantThatCouldNotBePersistedIsAuditedAsARefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory is not unwritable")
	}
	n := &fakeNotifier{allow: true, scope: ScopeOnce}
	svc := newTestService(t, n)
	n.auth = svc.Authority

	// Point the store at a path it cannot create, so Add fails after the human allowed.
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	svc.Authority.store.path = filepath.Join(ro, "sub", "grants.json")

	req := AuthRequest{ThreadID: "t", Capability: CapScreenshot,
		Target: Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.mozilla.firefox"}}
	d, err := svc.Authorize(context.Background(), req)
	if d.Allow || err == nil {
		t.Fatalf("a grant that could not be persisted must not allow: allow=%v err=%v", d.Allow, err)
	}

	var sawRefusal bool
	for _, e := range auditEntries(t, svc) {
		if e.Capability != CapScreenshot {
			continue
		}
		if e.Kind == AuditGrant {
			t.Fatalf("a grant that was never persisted must not be logged as one: %+v", e)
		}
		if e.Kind == AuditDeny {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatal("the failed grant left no trace in the audit log at all")
	}
}

// SECURITY (audit F35): the self-target refusal used to be gated on tier == TierR2, so
// desktop_screenshot (R1) could still hand an agent a pixel-exact picture of Agent Kate's
// own consent dialog, policy toggles and kill switch — the targeting data the pointer
// attacks need, through the one read capability that was still permitted.
func TestSelfTargetIsRefusedForReadCapabilitiesToo(t *testing.T) {
	self := Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.kde.agentkate"}

	// A notifier that would ALLOW if asked, so a refusal here can only come from the guard.
	for _, c := range []Capability{CapScreenshot, CapA11yRead, CapWindowList, CapInputInject} {
		n := &fakeNotifier{allow: true, scope: ScopeOnce}
		svc := newTestService(t, n)
		n.auth = svc.Authority
		d, _ := svc.Authorize(context.Background(), AuthRequest{
			ThreadID: "t", Capability: c, Target: self,
		})
		if d.Allow {
			t.Fatalf("%s against Agent Kate's own UI must be refused, got allow", c)
		}
		for _, e := range n.events {
			if e == "cowork.grantRequested" {
				t.Fatalf("%s self-target must be refused outright, never put to the human", c)
			}
		}
	}

	// Not a blanket refusal: the same capability against somebody else's window still
	// reaches the human and can be allowed.
	n := &fakeNotifier{allow: true, scope: ScopeOnce}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	d, _ := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapScreenshot,
		Target: Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.mozilla.firefox"},
	})
	if !d.Allow {
		t.Fatal("a screenshot of somebody else's window must still be grantable")
	}
}

// SelfWindowRects is the query a capture path needs to black our windows out of a
// full-screen frame — the case the target-name refusal above cannot see (audit F35).
func TestSelfWindowRectsPicksOutOurOwnWindows(t *testing.T) {
	svc := newTestService(t, &fakeNotifier{})
	svc.SetSelfIdentity(nil, []int{4242})

	wins := []WindowRect{
		{X: 0, Y: 0, W: 800, H: 600, PID: 11, ResourceClass: "org.mozilla.firefox"},
		{X: 10, Y: 10, W: 400, H: 300, PID: 4242, ResourceClass: ""},        // ours by PID
		{X: 20, Y: 20, W: 200, H: 100, PID: 99, ResourceClass: "AgentKate"}, // ours by class
		{X: 30, Y: 30, W: 0, H: 100, PID: 4242, ResourceClass: "agentkate"}, // zero-area: skipped
	}
	got := svc.SelfWindowRects(wins)
	if len(got) != 2 {
		t.Fatalf("expected the two real Agent Kate windows, got %+v", got)
	}
	for _, w := range got {
		if w.ResourceClass == "org.mozilla.firefox" {
			t.Fatalf("a foreign window must never be reported as ours: %+v", w)
		}
	}
	if len(svc.SelfWindowRects(nil)) != 0 {
		t.Fatal("no windows means no self rects")
	}
}

// SelfRectsIntersecting is the enforceable sibling: a capture whose frame is a NAMED
// RECTANGLE needs no blackout, because the rect is known before the shutter and an overlap
// can simply be refused (audit F35, round 4).
func TestSelfRectsIntersectingIsHalfOpenAndFailsClosedOnAnEmptyRect(t *testing.T) {
	svc := newTestService(t, &fakeNotifier{})
	svc.SetSelfIdentity(nil, []int{4242})

	wins := []WindowRect{
		{X: 0, Y: 0, W: 2000, H: 1500, PID: 11, ResourceClass: "org.mozilla.firefox"},
		{X: 100, Y: 100, W: 400, H: 300, PID: 4242}, // ours: [100,500) x [100,400)
	}
	hit := func(x, y, w, h int) bool {
		return len(svc.SelfRectsIntersecting(Rect{X: x, Y: y, W: w, H: h}, wins)) > 0
	}
	// Overlaps, however small, and however far the box extends past us.
	for _, c := range [][4]int{{100, 100, 400, 300}, {200, 150, 10, 10}, {0, 0, 101, 101}, {0, 0, 4000, 3000}} {
		if !hit(c[0], c[1], c[2], c[3]) {
			t.Fatalf("region %v overlaps our window and must be reported", c)
		}
	}
	// Touching edges do not overlap — the honest crop right next to our window still works.
	for _, c := range [][4]int{{0, 100, 100, 300}, {500, 100, 100, 300}, {100, 400, 400, 300}, {1500, 1000, 100, 100}} {
		if hit(c[0], c[1], c[2], c[3]) {
			t.Fatalf("region %v does not overlap our window and must be cleared", c)
		}
	}
	// A rectangle with no extent cannot be verified against anything, and the pipeline turns
	// it into a whole-screen grab — so it reports every self window and the caller refuses.
	if len(svc.SelfRectsIntersecting(Rect{}, wins)) != 1 {
		t.Fatal("an empty region must fail closed, not clear")
	}
	// Nothing of ours on screen is still a clean answer.
	if len(svc.SelfRectsIntersecting(Rect{X: 0, Y: 0, W: 10, H: 10}, wins[:1])) != 0 {
		t.Fatal("no self windows means no overlap")
	}
}

// SECURITY (audit F35): the panic button latches on disk, so restarting akcore does not
// silently un-press it. The post-restart authority was already identical (Kill revokes
// every grant and clears every toggle), but a control that reports itself as un-pressed
// is its own hazard.
func TestAuthorityStartsKilledWhenTheLatchIsSet(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	b, err := json.MarshalIndent(policyFile{SchemaVersion: policySchemaVersion, KillSwitch: true}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	n := &fakeNotifier{allow: true}
	svc, _, err := New(filepath.Join(dir, "grants.json"), filepath.Join(dir, "audit.jsonl"),
		policyPath, nil, n, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.auth = svc.Authority
	if !svc.Killed() {
		t.Fatal("a persisted kill-switch must still be engaged after a restart")
	}
	d, _ := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapWindowList, Target: Target{Kind: TargetAny}})
	if d.Allow {
		t.Fatal("access must stay denied while the latch is down")
	}

	// …and an explicit re-arm lifts it, on disk as well as in memory.
	svc.Rearm("user re-armed")
	if svc.Killed() {
		t.Fatal("Rearm must lift the latch in memory")
	}
	reloaded, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Killed() {
		t.Fatal("Rearm must lift the latch on disk too, or the kill comes back next launch")
	}
}

// Kill writes the latch, so the next process starts denied without any extra wiring.
func TestKillLatchesThePolicyFile(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	n := &fakeNotifier{allow: true}
	svc, _, err := New(filepath.Join(dir, "grants.json"), filepath.Join(dir, "audit.jsonl"),
		policyPath, nil, n, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.auth = svc.Authority
	if err := svc.SetPolicy(CapScreenshot, true); err != nil {
		t.Fatal(err)
	}

	svc.Kill("panic")

	reloaded, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Killed() {
		t.Fatal("Kill must latch the kill-switch on disk")
	}
	if len(reloaded.List()) != 0 {
		t.Fatalf("Kill must also clear every toggle on disk, got %v", reloaded.List())
	}
}

// SECURITY (audit F35, plan 29): Authorize's two ALLOW shortcuts — a standing policy toggle
// and a remembered grant — each appended an AuditAction entry, the kind whose meaning is
// "a granted capability was EXERCISED", at the moment the DECISION was taken. Nothing had
// been exercised: the geometric guard, the focus guard, the timeline compiler and the
// portal all refuse downstream of that point. The panel shows this log to the user as the
// record of what agents did, so it must record what occurred.
func TestAuthorizeRecordsNoActionUntilTheActionHappens(t *testing.T) {
	target := Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.mozilla.firefox"}

	countActions := func(svc *Service) int {
		n := 0
		for _, e := range auditEntries(t, svc) {
			if e.Kind == AuditAction {
				n++
			}
		}
		return n
	}

	// (a) The standing toggle: allowed with no prompt at all.
	n := &fakeNotifier{allow: true, scope: ScopeSession}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	if err := svc.SetPolicy(CapScreenshot, true); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	d, err := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapScreenshot, Target: target})
	if err != nil || !d.Allow || d.GrantID != "policy" {
		t.Fatalf("a toggled-on capability must be pre-authorized: %+v err=%v", d, err)
	}
	if got := countActions(svc); got != 0 {
		t.Fatalf("the decision is not the deed: %d 'exercised' entries written before the capability ran", got)
	}
	// …and when the capability really does run, the handler's record still identifies it as
	// the toggle's doing, so nothing is lost by not writing it in advance.
	svc.AuditCapture("t", CapScreenshot, target, d.GrantID, "sha")
	if got := countActions(svc); got != 1 {
		t.Fatalf("the completed capture must be recorded exactly once, got %d", got)
	}
	for _, e := range auditEntries(t, svc) {
		if e.Kind == AuditAction && e.GrantID != "policy" {
			t.Fatalf("a pre-authorized use must still be identifiable as one: %+v", e)
		}
	}

	// (b) The remembered grant: the human answered once, and the reuse is not a new deed.
	n2 := &fakeNotifier{allow: true, scope: ScopeSession}
	svc2 := newTestService(t, n2)
	n2.auth = svc2.Authority
	first, err := svc2.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapScreenshot, Target: target})
	if err != nil || !first.Allow {
		t.Fatalf("the prompted grant must be allowed: %+v err=%v", first, err)
	}
	second, err := svc2.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapScreenshot, Target: target})
	if err != nil || !second.Allow || second.Reason != "existing grant" {
		t.Fatalf("the second call must reuse the grant: %+v err=%v", second, err)
	}
	if got := countActions(svc2); got != 0 {
		t.Fatalf("reusing a grant is not exercising a capability: %d 'exercised' entries", got)
	}
	// The grant itself is still recorded when it is really created.
	var grants int
	for _, e := range auditEntries(t, svc2) {
		if e.Kind == AuditGrant {
			grants++
		}
	}
	if grants != 1 {
		t.Fatalf("the human's one decision must appear once as a grant, got %d", grants)
	}
}

// SECURITY (audit F35, plan 29): a capture may not be taken while a consent prompt is on
// the user's screen. The self-target refusal covers a capture that NAMES one of our
// windows; a full-screen capture names nothing, and the sharpest thing in those pixels is
// the dialog the human is answering right now — including, when it belongs to another
// agent, what that agent is doing (the cross-agent read channel F6 closed on the wire).
// silentNotifier never answers a prompt, so one stays open for the length of the test. It
// records nothing: this test drives Authorize from two goroutines at once, and the shared
// fakeNotifier's event slice is not built for that.
type silentNotifier struct{}

func (silentNotifier) Notify(string, any)   {}
func (silentNotifier) NotifyUI(string, any) {}

func TestCaptureIsRefusedWhileAConsentPromptIsOpen(t *testing.T) {
	// A notifier that leaves the prompt standing: the human has not answered yet.
	svc := newTestService(t, silentNotifier{})
	svc.Authority.promptTimeoutR0R1 = 2 * time.Second

	screen := Target{Kind: TargetScreen, Label: "active screen"}
	// A11y read is not pre-authorized, so this call parks on the human.
	asked := make(chan struct{})
	go func() {
		close(asked)
		_, _ = svc.Authorize(context.Background(), AuthRequest{
			ThreadID: "t-other", Capability: CapA11yRead,
			Target: Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.mozilla.firefox"}})
	}()
	<-asked
	// Wait for the prompt to really be open.
	deadline := time.Now().Add(2 * time.Second)
	for svc.Authority.broker.open() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if svc.Authority.broker.open() == 0 {
		t.Fatal("precondition: a consent prompt should be open")
	}

	// Even with the standing toggle on — a decision to allow screenshots is not a decision
	// to photograph the next decision.
	if err := svc.SetPolicy(CapScreenshot, true); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	d, err := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapScreenshot, Target: screen})
	if err != nil || d.Allow {
		t.Fatalf("a capture while a consent prompt is on screen must be refused: %+v err=%v", d, err)
	}
	if !strings.Contains(d.Reason, "try again") {
		t.Fatalf("the refusal must tell the agent this is transient: %q", d.Reason)
	}

	// A NON-capture capability is unaffected: this rule is about what lands in a frame, not
	// a global freeze while the human thinks.
	if err := svc.SetPolicy(CapWindowList, true); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if d, _ := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapWindowList, Target: Target{Kind: TargetAny}}); !d.Allow {
		t.Fatal("listing windows must not be blocked by an unrelated open prompt")
	}

	// Once the prompt has timed out (nobody answered it), captures work again — the rule is
	// a pause, not a latch.
	deadline = time.Now().Add(5 * time.Second)
	for svc.Authority.broker.open() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if d, _ := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapScreenshot, Target: screen}); !d.Allow {
		t.Fatal("with no prompt on screen the pre-authorized capture must go ahead again")
	}
}
