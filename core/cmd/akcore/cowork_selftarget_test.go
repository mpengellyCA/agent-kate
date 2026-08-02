package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agentkate/internal/cowork"
	"agentkate/internal/kde"
)

// selfService builds a real cowork.Service (no KDE bus, so every KWin call errors) that
// knows Agent Kate's own window class and PID, so the self-target guards are exercised
// against the production matching logic rather than a stand-in.
func selfService(t *testing.T) *cowork.Service {
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
	return svc
}

func selfAuthority(t *testing.T) *cowork.Authority {
	t.Helper()
	return selfService(t).Authority
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

// --- F50: a11y READS of Agent Kate's own UI are refused too ---------------------------
//
// The R2 action path refused self-targets from the start; the R1 READ path did not. A
// prompt-injected agent could therefore enumerate the exact bounds and labels of the
// consent dialog, the policy toggles and the kill-switch — precisely the targeting data
// the F25/F26 pointer attacks need — through a capability the user reads as "look at the
// screen". There is no legitimate agent use for reading our own interface.

func TestGuardA11yReadRefusesOwnWindow(t *testing.T) {
	auth := selfAuthority(t)
	// By class.
	byClass := kde.Window{InternalID: "w-ak", Caption: "Agent Kate — desktop CONTROL request",
		ResourceClass: "org.kde.agentkate", PID: 99}
	if err := guardA11yReadWindow(auth, byClass); err == nil {
		t.Fatal("listing/reading Agent Kate's own window must be refused")
	}
	// By PID alone — KWin reporting no class is the fail-open shape (audit F7's lesson).
	byPID := kde.Window{InternalID: "w-ak2", ResourceClass: "", PID: 4242}
	if err := guardA11yReadWindow(auth, byPID); err == nil {
		t.Fatal("PID evidence alone must refuse an Agent Kate read target")
	}
}

func TestGuardA11yReadFailsClosedWithoutEvidence(t *testing.T) {
	auth := selfAuthority(t)
	blank := kde.Window{InternalID: "w-blank", ResourceClass: "", PID: 0}
	if err := guardA11yReadWindow(auth, blank); err == nil {
		t.Fatal("a window with neither an owning process nor a class must not be read")
	}
	if err := guardA11yReadWindow(nil, foreignWindow()); err == nil {
		t.Fatal("a missing self-identity authority must refuse the read")
	}
}

func TestGuardA11yReadAllowsForeignWindow(t *testing.T) {
	auth := selfAuthority(t)
	if err := guardA11yReadWindow(auth, foreignWindow()); err != nil {
		t.Fatalf("reading an ordinary application window must still work: %v", err)
	}
}

// The guard is only worth anything if the read handlers actually apply it. Counting call
// sites in the source could not tell a real call from one rewritten to fail open, so
// resolution and guard are fused into one decision (resolveA11yReadWindow) that both
// cowork.listElements and cowork.readText call — and that a test can drive outright.
func TestA11yReadResolutionRefusesOurOwnUI(t *testing.T) {
	auth := selfAuthority(t)
	ak := kde.Window{
		InternalID: "w-ak", Caption: "Agent Kate — desktop CONTROL request",
		ResourceClass: "org.kde.agentkate", PID: 99, Active: true,
	}
	akNoClass := kde.Window{InternalID: "w-ak2", ResourceClass: "", PID: 4242, Active: true}
	blank := kde.Window{InternalID: "w-blank", ResourceClass: "", PID: 0, Active: true}

	refused := []struct {
		name     string
		auth     *cowork.Authority
		wins     []kde.Window
		listErr  error
		windowID string
	}{
		{"named explicitly", auth, []kde.Window{ak, foreignWindow()}, nil, "w-ak"},
		{"merely focused", auth, []kde.Window{ak}, nil, ""},
		{"by PID with no class", auth, []kde.Window{akNoClass}, nil, ""},
		{"neither owner nor class", auth, []kde.Window{blank}, nil, ""},
		{"no self identity", nil, []kde.Window{foreignWindow()}, nil, "w-firefox"},
	}
	for _, c := range refused {
		if _, _, err := resolveA11yReadWindow(c.auth, c.wins, c.listErr, c.windowID); err == nil {
			t.Fatalf("%s: reading this window must be refused", c.name)
		}
	}

	// A self-target refusal is a DENIAL, not "there is no such window": the two reach the
	// agent as different codes and teach different next steps.
	_, target, err := resolveA11yReadWindow(auth, []kde.Window{ak}, nil, "w-ak")
	if errors.Is(err, errNoA11yTarget) {
		t.Fatalf("reading Agent Kate's own window is a refusal, not a bad-target error: %v", err)
	}
	if !strings.Contains(err.Error(), "Agent Kate") {
		t.Fatalf("the refusal must name the self-target case: %v", err)
	}
	if target.WindowID != "w-ak" {
		t.Fatalf("the refused target must still be reported for the audit record, got %+v", target)
	}

	// "No window to read" stays a caller mistake.
	for _, c := range []struct {
		name     string
		wins     []kde.Window
		listErr  error
		windowID string
	}{
		{"kwin unreadable", nil, errors.New("kwin gone"), ""},
		{"unknown id", []kde.Window{foreignWindow()}, nil, "w-nope"},
		{"nothing focused", []kde.Window{}, nil, ""},
	} {
		_, _, err := resolveA11yReadWindow(auth, c.wins, c.listErr, c.windowID)
		if err == nil {
			t.Fatalf("%s: must not resolve to a readable window", c.name)
		}
		if !errors.Is(err, errNoA11yTarget) {
			t.Fatalf("%s: should be reported as a missing target, got %v", c.name, err)
		}
	}

	// And an ordinary application window still reads.
	win, target, err := resolveA11yReadWindow(auth, []kde.Window{ak, foreignWindow()}, nil, "w-firefox")
	if err != nil {
		t.Fatalf("reading an ordinary application window must still work: %v", err)
	}
	if win.InternalID != "w-firefox" || target.WindowID != "w-firefox" {
		t.Fatalf("resolved the wrong window: %+v / %+v", win, target)
	}
}

// --- F35 (plan 29): a SCREENSHOT of Agent Kate's own UI is refused for real --------------
//
// The refusal in Authorize inspects only Target.ResourceClass and Target.Label — both
// written by the agent — and a Target carries no PID at all, so a capture that named one of
// our windows by id with a blank or borrowed class walked straight past a check the code
// claimed refused it. The window id is now resolved against LIVE compositor data and
// cleared by the same owner-and-class matrix every other guard uses.

func TestCaptureTargetRefusesOurOwnWindow(t *testing.T) {
	auth := selfAuthority(t)
	ak := kde.Window{
		InternalID: "w-ak", Caption: "Agent Kate — desktop CONTROL request",
		ResourceClass: "org.kde.agentkate", PID: 99, Active: true,
		X: 100, Y: 100, Width: 400, Height: 300,
	}
	akNoClass := kde.Window{InternalID: "w-ak2", ResourceClass: "", PID: 4242, Active: true,
		X: 0, Y: 0, Width: 300, Height: 200}
	blank := kde.Window{InternalID: "w-blank", ResourceClass: "", PID: 0, Active: true,
		X: 0, Y: 0, Width: 100, Height: 100}
	wins := []kde.Window{ak, akNoClass, blank, foreignWindow()}

	refused := []struct {
		name   string
		auth   *cowork.Authority
		wins   []kde.Window
		list   error
		target cowork.Target
	}{
		// The exact bypass: name the window id, and lie about (or omit) the class.
		{"named by id with a BLANK class", auth, wins, nil,
			cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-ak"}},
		{"named by id with a BORROWED class", auth, wins, nil,
			cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-ak", ResourceClass: "firefox", Label: "Docs — Firefox"}},
		{"ours by PID with no class at all", auth, wins, nil,
			cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-ak2"}},
		{"a window with neither owner nor class", auth, wins, nil,
			cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-blank"}},
		{"the window list is unreadable", auth, nil, errors.New("kwin gone"),
			cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-firefox"}},
		{"…and for a whole-frame capture too", auth, nil, errors.New("kwin gone"),
			cowork.Target{Kind: cowork.TargetScreen, Label: "active screen"}},
		{"no self identity to compare against", nil, wins, nil,
			cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-firefox"}},
	}
	for _, c := range refused {
		got, err := resolveCaptureTarget(c.auth, c.wins, c.list, c.target)
		if err == nil {
			t.Fatalf("%s: this capture must be refused, got target %+v", c.name, got.Target)
		}
		if errors.Is(err, errNoCaptureTarget) {
			t.Fatalf("%s: this is a refusal, not a missing-window error: %v", c.name, err)
		}
	}

	// An ordinary window still captures, and is described from LIVE data — a borrowed class
	// in the request must not be what the human is shown.
	got, err := resolveCaptureTarget(auth, wins, nil,
		cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-firefox", ResourceClass: "org.kde.agentkate", Label: "totally innocent"})
	if err != nil {
		t.Fatalf("capturing an ordinary window must still work: %v", err)
	}
	if got.Target.ResourceClass != "firefox" || got.Target.Label != "Docs — Firefox" {
		t.Fatalf("the prompt must name the window the compositor reports, got %+v", got.Target)
	}

	// A stale id is a caller mistake, not a policy refusal — the two teach different next
	// steps and reach the agent as different codes.
	if _, err := resolveCaptureTarget(auth, wins, nil,
		cowork.Target{Kind: cowork.TargetWindow, WindowID: "w-gone"}); !errors.Is(err, errNoCaptureTarget) {
		t.Fatalf("an unknown window id should be reported as a missing target, got %v", err)
	}
}

// The whole-frame case: nothing can be refused by name, so the rectangles that must not
// appear in the pixels are computed and handed on, and the human is told the frame includes
// us. (The blackout itself lives in the UI's capture pipeline — see resolveCaptureTarget.)
func TestWholeFrameCaptureReportsOurRectsAndSaysSo(t *testing.T) {
	auth := selfAuthority(t)
	ak := kde.Window{InternalID: "w-ak", ResourceClass: "org.kde.agentkate", PID: 99,
		X: 100, Y: 100, Width: 400, Height: 300}
	wins := []kde.Window{ak, foreignWindow()}

	got, err := resolveCaptureTarget(auth, wins, nil,
		cowork.Target{Kind: cowork.TargetScreen, Label: "active screen"})
	if err != nil {
		t.Fatalf("a screen capture must not be blanket-refused: %v", err)
	}
	if len(got.RedactRects) != 1 || got.RedactRects[0].X != 100 || got.RedactRects[0].W != 400 {
		t.Fatalf("our own window must be reported for redaction, got %+v", got.RedactRects)
	}
	if !strings.Contains(got.Target.Label, "Agent Kate") {
		t.Fatalf("the human approving a screen capture must be told it includes us, got %q", got.Target.Label)
	}

	// With none of our windows on screen there is nothing to redact and nothing to warn about.
	clean, err := resolveCaptureTarget(auth, []kde.Window{foreignWindow()}, nil,
		cowork.Target{Kind: cowork.TargetScreen, Label: "active screen"})
	if err != nil {
		t.Fatalf("a clean screen capture must work: %v", err)
	}
	if len(clean.RedactRects) != 0 {
		t.Fatalf("nothing of ours is on screen, got %+v", clean.RedactRects)
	}
	if clean.Target.Label != "active screen" {
		t.Fatalf("no warning should be added when there is nothing to warn about, got %q", clean.Target.Label)
	}
}
