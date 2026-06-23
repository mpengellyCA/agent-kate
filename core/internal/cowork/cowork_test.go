package cowork

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeNotifier auto-answers grant prompts so Authorize can be exercised headlessly.
type fakeNotifier struct {
	auth   *Authority
	allow  bool
	scope  Scope
	events []string
}

func (f *fakeNotifier) Notify(method string, params any) {
	f.events = append(f.events, method)
	// auth==nil models a UI that never answers (→ Authorize times out, fails closed).
	if method == "cowork.grantRequested" && f.auth != nil {
		if m, ok := params.(map[string]any); ok {
			if id, ok := m["requestId"].(string); ok {
				f.auth.Respond(id, f.allow, f.scope, 0, false)
			}
		}
	}
}

func newTestService(t *testing.T, n Notifier) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, _, err := New(filepath.Join(dir, "grants.json"), filepath.Join(dir, "audit.jsonl"), filepath.Join(dir, "policy.json"), nil, n, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// fast prompts so timeout cases don't stall the suite
	svc.Authority.promptTimeoutR0R1 = 200 * time.Millisecond
	svc.Authority.promptTimeoutR2 = 200 * time.Millisecond
	return svc
}

func TestStoreAddMatchRevoke(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStore(filepath.Join(dir, "g.json"))
	if err != nil {
		t.Fatal(err)
	}
	tgt := Target{Kind: TargetWindow, WindowID: "w1"}
	g, err := s.Add("threadA", CapScreenshot, tgt, ScopeSession, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if g.GrantedBy != "user" || g.Tier != TierR1 {
		t.Fatalf("server-derived fields wrong: %+v", g)
	}
	now := time.Now()
	if s.Match("threadA", CapScreenshot, tgt, now) == nil {
		t.Fatal("expected match")
	}
	if s.Match("threadB", CapScreenshot, tgt, now) != nil {
		t.Fatal("cross-thread grant must NOT match")
	}
	if s.Match("threadA", CapScreenshot, Target{Kind: TargetWindow, WindowID: "w2"}, now) != nil {
		t.Fatal("different window must NOT match")
	}
	if s.Revoke(g.ID, "test") == nil {
		t.Fatal("revoke failed")
	}
	if s.Match("threadA", CapScreenshot, tgt, now) != nil {
		t.Fatal("revoked grant must NOT match")
	}
}

func TestRestartSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")
	s, _ := LoadStore(path)
	if _, err := s.Add("t", CapScreenshot, Target{Kind: TargetWindow, WindowID: "w"}, ScopeSession, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("t", CapWindowList, Target{Kind: TargetAny}, ScopeUntilRevoked, nil, false); err != nil {
		t.Fatal(err)
	}
	// Reload simulates a restart.
	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List("")
	if len(got) != 1 || got[0].Capability != CapWindowList {
		t.Fatalf("only until_revoked should survive restart, got %d: %+v", len(got), got)
	}
}

func TestAuthorizeAllowThenReuse(t *testing.T) {
	n := &fakeNotifier{allow: true, scope: ScopeSession}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	req := AuthRequest{ThreadID: "t", Capability: CapScreenshot, Target: Target{Kind: TargetWindow, WindowID: "w"}}

	d, err := svc.Authorize(context.Background(), req)
	if err != nil || !d.Allow {
		t.Fatalf("first authorize: allow=%v err=%v", d.Allow, err)
	}
	promptCount := 0
	for _, e := range n.events {
		if e == "cowork.grantRequested" {
			promptCount++
		}
	}
	// Second call must be satisfied by the remembered session grant — no new prompt.
	d2, _ := svc.Authorize(context.Background(), req)
	if !d2.Allow {
		t.Fatal("reuse should allow")
	}
	count2 := 0
	for _, e := range n.events {
		if e == "cowork.grantRequested" {
			count2++
		}
	}
	if count2 != promptCount {
		t.Fatalf("session grant should not re-prompt: %d -> %d", promptCount, count2)
	}
}

func TestAuthorizeDenyAndTimeout(t *testing.T) {
	n := &fakeNotifier{allow: false}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	d, _ := svc.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapScreenshot, Target: Target{Kind: TargetWindow, WindowID: "w"}})
	if d.Allow {
		t.Fatal("explicit deny must not allow")
	}

	// No-answer notifier → timeout → deny (fail-closed).
	svc2 := newTestService(t, &fakeNotifier{})
	d2, _ := svc2.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapWindowList, Target: Target{Kind: TargetAny}})
	if d2.Allow {
		t.Fatal("timeout must fail closed")
	}
}

func TestKillSwitch(t *testing.T) {
	n := &fakeNotifier{allow: true, scope: ScopeUntilRevoked}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	req := AuthRequest{ThreadID: "t", Capability: CapWindowList, Target: Target{Kind: TargetAny}}
	if d, _ := svc.Authorize(context.Background(), req); !d.Allow {
		t.Fatal("setup grant failed")
	}
	torn := false
	svc.RegisterTeardown(svc.store.List("t")[0].ID, func() { torn = true })

	svc.Kill("user pressed stop")
	if !svc.Killed() {
		t.Fatal("kill-switch should be engaged")
	}
	if !torn {
		t.Fatal("kill must run teardowns")
	}
	if d, _ := svc.Authorize(context.Background(), req); d.Allow {
		t.Fatal("killed authority must deny")
	}
	svc.Rearm("user resumed")
	if svc.Killed() {
		t.Fatal("rearm should clear kill")
	}
}

func TestR2NotRememberedOutsideSandbox(t *testing.T) {
	n := &fakeNotifier{allow: true, scope: ScopeSession}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	req := AuthRequest{ThreadID: "t", Capability: CapA11yAction, Target: Target{Kind: TargetWindow, WindowID: "w"},
		ActionPreview: &ActionDescriptor{Mechanism: "a11y_action", Element: "Save"}}
	if d, _ := svc.Authorize(context.Background(), req); !d.Allow {
		t.Fatal("first R2 should allow on user consent")
	}
	// R2 outside the sandbox is forced to once-scope → second call re-prompts.
	before := len(n.events)
	if d, _ := svc.Authorize(context.Background(), req); !d.Allow {
		t.Fatal("second R2 should allow (re-prompted)")
	}
	if len(n.events) <= before {
		t.Fatal("R2 outside sandbox must re-prompt every time, not reuse a grant")
	}
}

func TestSelfTargetRefused(t *testing.T) {
	n := &fakeNotifier{allow: true}
	svc := newTestService(t, n)
	n.auth = svc.Authority
	d, _ := svc.Authorize(context.Background(), AuthRequest{
		ThreadID: "t", Capability: CapInputInject,
		Target: Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.kde.agentkate"},
	})
	if d.Allow {
		t.Fatal("must refuse to control Agent Kate's own UI")
	}
}

func TestAuditChainAndTamper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	a, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := a.Append(AuditEntry{Kind: AuditGrant, ThreadID: "t", Capability: CapScreenshot, Detail: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	// Clean reload verifies.
	a2, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Tampered() {
		t.Fatal("clean chain must verify")
	}
	entries, _, _ := a2.Tail("", 0, 0)
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}

	// Tamper: flip a byte in the middle of the file.
	raw, _ := os.ReadFile(path)
	for i := range raw {
		if raw[i] == 'x' {
			raw[i] = 'y'
			break
		}
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a3, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if !a3.Tampered() {
		t.Fatal("mutated chain must be detected as tampered")
	}
}

func TestTamperedAuthorityFailsClosed(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "g.json")
	ap := filepath.Join(dir, "audit.jsonl")
	// Write a deliberately broken audit file.
	if err := os.WriteFile(ap, []byte(`{"seq":1,"kind":"grant","prevHash":"","hash":"deadbeef"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &fakeNotifier{allow: true}
	svc, warnings, err := New(gp, ap, filepath.Join(dir, "policy.json"), nil, n, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.auth = svc.Authority
	if len(warnings) == 0 {
		t.Fatal("tamper should surface a warning")
	}
	d, _ := svc.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapWindowList, Target: Target{Kind: TargetAny}})
	if d.Allow {
		t.Fatal("tampered audit must fail closed (deny all)")
	}
}

func TestPolicyPreauthNoPrompt(t *testing.T) {
	// A notifier that DENIES if ever prompted — so a pass proves NO prompt happened.
	n := &fakeNotifier{allow: false}
	svc := newTestService(t, n)
	n.auth = svc.Authority

	if err := svc.SetPolicy(CapScreenshot, true); err != nil {
		t.Fatal(err)
	}
	d, _ := svc.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapScreenshot, Target: Target{Kind: TargetScreen}})
	if !d.Allow || d.GrantID != "policy" {
		t.Fatalf("policy-enabled capability must allow with no prompt; got allow=%v id=%q", d.Allow, d.GrantID)
	}
	for _, e := range n.events {
		if e == "cowork.grantRequested" {
			t.Fatal("policy pre-auth must NOT raise a consent prompt")
		}
	}

	// R2 control (input injection) is honored too when toggled on (full no-prompt).
	if err := svc.SetPolicy(CapInputInject, true); err != nil {
		t.Fatal(err)
	}
	if d2, _ := svc.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapInputInject, Target: Target{Kind: TargetWindow, WindowID: "w"}}); !d2.Allow {
		t.Fatal("policy-enabled R2 control must allow with no prompt")
	}
	// ...but NEVER against Agent Kate's own UI, even with the toggle on.
	if d3, _ := svc.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapInputInject, Target: Target{Kind: TargetWindow, ResourceClass: "org.kde.agentkate"}}); d3.Allow {
		t.Fatal("self-target must be refused even when the toggle is on")
	}

	// Kill-switch clears the policy (panic button); afterwards it falls back to prompt.
	svc.Kill("panic")
	if len(svc.PolicyList()) != 0 {
		t.Fatal("kill-switch must clear all policy toggles")
	}
	svc.Rearm("resume")
	if d4, _ := svc.Authorize(context.Background(), AuthRequest{ThreadID: "t", Capability: CapScreenshot, Target: Target{Kind: TargetScreen}}); d4.Allow {
		t.Fatal("after kill cleared the policy, capability must fall back to prompt/deny")
	}
}
