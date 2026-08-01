package main

import (
	"errors"
	"path/filepath"
	"testing"

	"agentkate/internal/cowork"
	"agentkate/internal/kde"
)

// selfAuthority builds a real consent Authority (no KDE bus) that knows Agent Kate's own
// window class and PID, so the self-target guards are exercised against the production
// matching logic rather than a stand-in.
func selfAuthority(t *testing.T) *cowork.Authority {
	t.Helper()
	dir := t.TempDir()
	svc, _, err := cowork.New(
		filepath.Join(dir, "grants.json"),
		filepath.Join(dir, "audit.jsonl"),
		filepath.Join(dir, "policy.json"),
		nil, nil, nil)
	if err != nil {
		t.Fatalf("cowork.New: %v", err)
	}
	svc.SetSelfIdentity([]string{"org.kde.agentkate"}, []int{4242})
	return svc.Authority
}

func foreignWindow() kde.Window {
	return kde.Window{
		InternalID: "w-firefox", Caption: "Docs — Firefox", ResourceClass: "firefox",
		PID: 7, Active: true, X: 0, Y: 0, Width: 800, Height: 600,
	}
}

// --- F3: keyboard injection must not be able to type into Agent Kate's own UI ---------

func TestResolveInjectTargetRefusesSelfFocusedWindowByClass(t *testing.T) {
	auth := selfAuthority(t)
	wins := []kde.Window{{
		InternalID: "w-ak", Caption: "Agent Kate — desktop CONTROL request",
		ResourceClass: "org.kde.agentkate", PID: 99, Active: true,
	}}
	if _, err := resolveInjectTargetFrom(auth, wins, nil, ""); err == nil {
		t.Fatal("typing into the focused Agent Kate window must be refused")
	}
	// ...and equally when the agent names it explicitly.
	if _, err := resolveInjectTargetFrom(auth, wins, nil, "w-ak"); err == nil {
		t.Fatal("typing into an explicitly named Agent Kate window must be refused")
	}
}

func TestResolveInjectTargetRefusesSelfFocusedWindowByPID(t *testing.T) {
	auth := selfAuthority(t)
	// KWin reports no resourceClass at all (the fail-open case): the PID is the only
	// evidence left and it must be decisive.
	wins := []kde.Window{{InternalID: "w-ak2", Caption: "", ResourceClass: "", PID: 4242, Active: true}}
	if _, err := resolveInjectTargetFrom(auth, wins, nil, ""); err == nil {
		t.Fatal("PID evidence alone must refuse an Agent Kate keyboard target")
	}
}

func TestResolveInjectTargetFailsClosedWhenUnresolvable(t *testing.T) {
	auth := selfAuthority(t)
	if _, err := resolveInjectTargetFrom(auth, nil, errors.New("kwin unreachable"), ""); err == nil {
		t.Fatal("an unreadable window list must refuse, not type blind")
	}
	if _, err := resolveInjectTargetFrom(auth, nil, errors.New("kwin unreachable"), "w-firefox"); err == nil {
		t.Fatal("an unreadable window list must refuse even for a named window")
	}
	// KWin answered, but nothing is active (or everything is minimized).
	inactive := []kde.Window{{InternalID: "w-x", ResourceClass: "firefox", PID: 7, Active: false}}
	if _, err := resolveInjectTargetFrom(auth, inactive, nil, ""); err == nil {
		t.Fatal("no identifiable focused window must refuse")
	}
	minimized := []kde.Window{{InternalID: "w-y", ResourceClass: "firefox", PID: 7, Active: true, Minimized: true}}
	if _, err := resolveInjectTargetFrom(auth, minimized, nil, ""); err == nil {
		t.Fatal("a minimized active window must not be accepted as the keyboard target")
	}
	// A named window that no longer exists.
	if _, err := resolveInjectTargetFrom(auth, []kde.Window{foreignWindow()}, nil, "w-gone"); err == nil {
		t.Fatal("a vanished target window must refuse")
	}
	// No authority to compare against at all.
	if _, err := resolveInjectTargetFrom(nil, []kde.Window{foreignWindow()}, nil, ""); err == nil {
		t.Fatal("a missing self-identity authority must refuse")
	}
}

func TestResolveInjectTargetAllowsForeignFocusedWindow(t *testing.T) {
	auth := selfAuthority(t)
	tgt, err := resolveInjectTargetFrom(auth, []kde.Window{foreignWindow()}, nil, "")
	if err != nil {
		t.Fatalf("a normal focused window must be allowed: %v", err)
	}
	// The resolved id is what the handler re-focuses before injecting, so it must be
	// filled in even though the caller named no window.
	if tgt.WindowID != "w-firefox" {
		t.Fatalf("resolved windowId = %q, want w-firefox", tgt.WindowID)
	}
	if tgt.ResourceClass != "firefox" || tgt.Label != "Docs — Firefox" {
		t.Fatalf("target not described for the consent prompt: %+v", tgt)
	}
}

// --- F7: a11y actions must not be able to drive Agent Kate's own UI -------------------

func TestGuardA11yTargetRefusesSelfPID(t *testing.T) {
	auth := selfAuthority(t)
	info := kde.ElementContext{Role: "text", Name: "phrase", PID: 4242}
	// The KWin lookup found nothing (the old fail-open path): PID must still refuse.
	if err := guardA11yTarget(auth, info, kde.Window{}, false, nil); err == nil {
		t.Fatal("an element owned by an Agent Kate PID must be refused")
	}
	// And when the lookup outright failed.
	if err := guardA11yTarget(auth, info, kde.Window{}, false, errors.New("kwin unreachable")); err == nil {
		t.Fatal("an element owned by an Agent Kate PID must be refused even with KWin down")
	}
}

func TestGuardA11yTargetFailsClosedWhenOwnerUnresolvable(t *testing.T) {
	auth := selfAuthority(t)
	info := kde.ElementContext{Role: "push button", Name: "Allow once", PID: 7}
	if err := guardA11yTarget(auth, info, kde.Window{}, false, errors.New("kwin unreachable")); err == nil {
		t.Fatal("an unreadable window list must refuse the R2 a11y action")
	}
	if err := guardA11yTarget(auth, info, kde.Window{}, false, nil); err == nil {
		t.Fatal("an element whose owning window cannot be found must be refused")
	}
	if err := guardA11yTarget(auth, kde.ElementContext{Role: "button", PID: 0}, kde.Window{}, false, nil); err == nil {
		t.Fatal("an element with no owning process must be refused")
	}
	if err := guardA11yTarget(nil, info, foreignWindow(), true, nil); err == nil {
		t.Fatal("a missing self-identity authority must refuse")
	}
}

func TestGuardA11yTargetRefusesSelfWindowClass(t *testing.T) {
	auth := selfAuthority(t)
	info := kde.ElementContext{Role: "push button", Name: "Allow once", PID: 99}
	akWin := kde.Window{InternalID: "w-ak", ResourceClass: "org.kde.agentkate", PID: 99}
	if err := guardA11yTarget(auth, info, akWin, true, nil); err == nil {
		t.Fatal("an element in a window with Agent Kate's class must be refused")
	}
}

func TestGuardA11yTargetAllowsForeignElement(t *testing.T) {
	auth := selfAuthority(t)
	info := kde.ElementContext{Role: "entry", Name: "Search", PID: 7}
	if err := guardA11yTarget(auth, info, foreignWindow(), true, nil); err != nil {
		t.Fatalf("a normal element must be allowed: %v", err)
	}
}
