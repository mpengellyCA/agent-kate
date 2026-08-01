package cowork

import (
	"sync"
	"testing"
)

// TestIsSelfPIDAndWindow pins the PID-based evidence the R2 guards in package main rely
// on: a window whose resourceClass could not be read is still refused when its process is
// ours (audit F7 — the check used to fail OPEN in exactly that case).
func TestIsSelfPIDAndWindow(t *testing.T) {
	a := newTestService(t, &fakeNotifier{}).Authority
	a.SetSelfIdentity([]string{"org.kde.agentkate"}, []int{4242})

	if !a.IsSelfPID(4242) {
		t.Fatal("a registered self PID must be recognised")
	}
	if a.IsSelfPID(7) {
		t.Fatal("a foreign PID must not be self")
	}
	// An unknown pid is "no evidence", never "verified safe" — callers fail closed on it.
	if a.IsSelfPID(0) || a.IsSelfPID(-1) {
		t.Fatal("a non-positive PID must not report as self")
	}

	if !a.IsSelfWindow(4242, "") {
		t.Fatal("PID alone must be decisive when the class is unreadable")
	}
	if !a.IsSelfWindow(0, "ORG.KDE.AgentKate") {
		t.Fatal("class alone must be decisive, case-insensitively")
	}
	if a.IsSelfWindow(7, "firefox") {
		t.Fatal("a foreign window must not be self")
	}
}

// paramNotifier records the params of every notification (fakeNotifier keeps only names).
type paramNotifier struct {
	mu     sync.Mutex
	params map[string]map[string]any
}

func (p *paramNotifier) NotifyUI(method string, params any) { p.Notify(method, params) }

func (p *paramNotifier) Notify(method string, params any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.params == nil {
		p.params = map[string]map[string]any{}
	}
	if m, ok := params.(map[string]any); ok {
		p.params[method] = m
	}
}

// TestKillSwitchAsksUIToRestoreDesktopFlags pins the kill-switch's contract with the UI
// (audit F8): "stop ALL desktop access" must also tell the UI to put the desktop-wide
// org.a11y.Status flags back, not just tear down the RemoteDesktop session.
func TestKillSwitchAsksUIToRestoreDesktopFlags(t *testing.T) {
	n := &paramNotifier{}
	svc := newTestService(t, n)
	svc.Kill("test")

	m, ok := n.params["cowork.killSwitch"]
	if !ok {
		t.Fatal("Kill must notify the UI")
	}
	if m["on"] != true {
		t.Fatalf("kill notification must be on=true: %+v", m)
	}
	if m["restoreDesktopFlags"] != true {
		t.Fatalf("kill notification must ask the UI to restore the desktop a11y flags: %+v", m)
	}
}

// routeNotifier records which DIRECTION each notification took: Notify reaches
// every connection (agent bridges included), NotifyUI only the human's client.
type routeNotifier struct {
	mu        sync.Mutex
	broadcast []string
	uiOnly    []string
	auth      *Authority
}

func (r *routeNotifier) Notify(method string, params any) {
	r.mu.Lock()
	r.broadcast = append(r.broadcast, method)
	r.mu.Unlock()
}

func (r *routeNotifier) NotifyUI(method string, params any) {
	r.mu.Lock()
	r.uiOnly = append(r.uiOnly, method)
	r.mu.Unlock()
	// Answer the prompt so Authorize does not sit out its timeout.
	if method == "cowork.grantRequested" && r.auth != nil {
		if m, ok := params.(map[string]any); ok {
			if id, ok := m["requestId"].(string); ok {
				r.auth.Respond(id, false, ScopeOnce, 0, false)
			}
		}
	}
}

func (r *routeNotifier) sent() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.broadcast...), append([]string(nil), r.uiOnly...)
}

// TestConsentPromptIsNotBroadcast is the third site of audit F6: the consent
// prompt carries the broker request id AND the literal action being asked about
// (the window, the element, the text bound for a field). Broadcast, it hands
// every other agent's bridge one agent's desktop activity plus the id to race
// the human on. It goes to the UI and nobody else — as do the kill-switch and
// the grant/policy change notices the same dialog is driven by.
func TestConsentPromptIsNotBroadcast(t *testing.T) {
	n := &routeNotifier{}
	svc := newTestService(t, n)
	n.auth = svc.Authority

	dec, err := svc.Authority.Authorize(t.Context(), AuthRequest{
		ThreadID: "t-1", Capability: CapInputInject,
		Target:        Target{Kind: TargetWindow, Label: "the focused window"},
		ActionPreview: &ActionDescriptor{Mechanism: "input_inject", Detail: "hunter2"},
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if dec.Allow {
		t.Fatal("the fixture denied; the decision must be a refusal")
	}
	broadcast, uiOnly := n.sent()
	if len(broadcast) != 0 {
		t.Errorf("consent traffic was broadcast to every connection: %v", broadcast)
	}
	found := false
	for _, m := range uiOnly {
		if m == "cowork.grantRequested" {
			found = true
		}
	}
	if !found {
		t.Errorf("the UI never received the consent prompt: %v", uiOnly)
	}

	// The kill-switch and the state notices take the same route.
	svc.Kill("test")
	svc.Authority.Rearm("test")
	broadcast, uiOnly = n.sent()
	if len(broadcast) != 0 {
		t.Errorf("cowork state notices were broadcast: %v", broadcast)
	}
	if len(uiOnly) < 3 {
		t.Errorf("the UI missed a state notice: %v", uiOnly)
	}
}
