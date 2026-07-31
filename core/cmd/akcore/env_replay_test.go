package main

import (
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// The per-thread environment overlay (plan 16 P6) is the lever that points a
// CLI at its own state — KIMI_CODE_HOME moves a kimi thread's whole home
// (sessions, config, credentials; verified against kimi 0.30.0). So it has to
// survive every relaunch: a resume that forgot it would look for the session
// in a different home and find nothing. This is the same class of bug P3's
// remediation fixed for the persona.

var threadEnv = map[string]string{
	"KIMI_CODE_HOME": "/wt/.agentkate/kimi-home",
	"AK_TEST_MARKER": "1",
}

func TestLaunchRecordsTheEnvOverlay(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	proj := t.TempDir()

	if _, _, err := launchThread(d, fake, "t-env", "s-env", agentStartParams{
		WorkspacePath: proj,
		Prompt:        "go",
		Env:           threadEnv,
	}, launchMeta{}); err != nil {
		t.Fatalf("launchThread: %v", err)
	}
	if got := fake.spec().Env["KIMI_CODE_HOME"]; got != threadEnv["KIMI_CODE_HOME"] {
		t.Errorf("launch passed KIMI_CODE_HOME=%q", got)
	}
	rec, ok := sessions.Get("t-env")
	if !ok {
		t.Fatal("no record")
	}
	if rec.Env["KIMI_CODE_HOME"] != threadEnv["KIMI_CODE_HOME"] ||
		rec.Env["AK_TEST_MARKER"] != "1" {
		t.Fatalf("record did not persist the overlay: %+v", rec.Env)
	}
}

func TestResumeReplaysTheEnvOverlay(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	rec := session.Record{
		ThreadID: "t-env-resume", SessionID: "s-1", Project: t.TempDir(),
		Worktree: worktree.Worktree{ThreadID: "t-env-resume", Path: t.TempDir()},
		Backend:  "fake", Created: time.Now(), Status: session.StatusDormant,
		Env: threadEnv,
	}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	resumeThread(d, fake, rec, nil)

	if got := fake.spec().Env["KIMI_CODE_HOME"]; got != threadEnv["KIMI_CODE_HOME"] {
		t.Fatalf("resume dropped the overlay (KIMI_CODE_HOME=%q); the thread would "+
			"look for its session in a different home", got)
	}
}

// A record written before P6 resumes with no overlay at all — the environment
// it always had.
func TestResumeWithoutEnvStaysEmpty(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	rec := session.Record{
		ThreadID: "t-env-old", SessionID: "s-old", Project: t.TempDir(),
		Worktree: worktree.Worktree{ThreadID: "t-env-old", Path: t.TempDir()},
		Backend:  "fake", Created: time.Now(), Status: session.StatusDormant,
	}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	resumeThread(d, fake, rec, nil)

	if got := fake.spec().Env; len(got) != 0 {
		t.Fatalf("pre-P6 record resumed with an overlay: %+v", got)
	}
}

func TestForkCarriesTheEnvOverlay(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	repo := personaRepo(t)
	src := session.Record{
		ThreadID: "t-env-src", SessionID: "s-src", Project: repo,
		Worktree: worktree.Worktree{ThreadID: "t-env-src", Path: repo},
		Backend:  "fake", Created: time.Now(), Status: session.StatusDormant,
		Env: threadEnv,
	}
	if err := sessions.Put(src); err != nil {
		t.Fatalf("Put: %v", err)
	}

	forkAgentThread(d, fake, src, "t-env-fork", "", "", "Fork")

	if got := fake.spec().Env["KIMI_CODE_HOME"]; got != threadEnv["KIMI_CODE_HOME"] {
		t.Errorf("fork launched without the source's overlay (%q)", got)
	}
	rec, ok := sessions.Get("t-env-fork")
	if !ok {
		t.Fatal("fork record missing")
	}
	if rec.Env["KIMI_CODE_HOME"] != threadEnv["KIMI_CODE_HOME"] {
		t.Errorf("fork record did not carry the overlay: %+v", rec.Env)
	}
}

// An AGENT must not be able to set a worker's environment: it decides where
// the worker's credentials come from and which endpoint they are sent to, so
// accepting it would route around the permission prompt that guards every
// other way of doing the same thing. launch_agent ignores the parameter
// entirely — this drives the real RPC to prove it.
func TestLaunchWorkerIgnoresEnv(t *testing.T) {
	sessions := testSessions(t)
	proj := t.TempDir()
	if err := sessions.Put(session.Record{
		ThreadID: "t-parent", Project: proj, Created: time.Now(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake := &fakeHarness{}
	client := orchTestCore(t, sessions, agent.NewTurnTracker(), fake)

	if err := client.Call("agent.launchWorker", map[string]any{
		"parentThreadId": "t-parent",
		"backend":        "fake",
		"prompt":         "do the thing",
		"isolation":      "workspace",
		"env":            map[string]any{"ANTHROPIC_BASE_URL": "https://evil.invalid"},
	}, nil); err != nil {
		t.Fatalf("launchWorker: %v", err)
	}
	if got := fake.spec().Env; len(got) != 0 {
		t.Fatalf("an agent set its worker's environment: %+v", got)
	}
}

// The P6 sweep persists and replays for the same reason the env overlay does —
// a resume that forgot DisallowedTools would hand the thread back a tool the
// human took away.
func TestSweepOptionsPersistAndReplay(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	proj := t.TempDir()

	if _, _, err := launchThread(d, fake, "t-sweep", "s-sweep", agentStartParams{
		WorkspacePath:   proj,
		Prompt:          "go",
		FallbackModels:  []string{"sonnet", "haiku"},
		DisallowedTools: []string{"Bash"},
		AddDirs:         []string{"/ref"},
	}, launchMeta{}); err != nil {
		t.Fatalf("launchThread: %v", err)
	}
	spec := fake.spec()
	if len(spec.FallbackModels) != 2 || len(spec.DisallowedTools) != 1 || len(spec.AddDirs) != 1 {
		t.Fatalf("launch did not pass the sweep: %+v", spec)
	}
	rec, ok := sessions.Get("t-sweep")
	if !ok {
		t.Fatal("no record")
	}
	if len(rec.FallbackModels) != 2 || len(rec.DisallowedTools) != 1 || len(rec.AddDirs) != 1 {
		t.Fatalf("record did not persist the sweep: %+v", rec)
	}

	rec.Status = session.StatusDormant
	resumeThread(d, fake, rec, nil)
	spec = fake.spec()
	if len(spec.DisallowedTools) != 1 || spec.DisallowedTools[0] != "Bash" {
		t.Errorf("resume dropped the deny-list: %+v", spec.DisallowedTools)
	}
	if len(spec.AddDirs) != 1 || len(spec.FallbackModels) != 2 {
		t.Errorf("resume dropped part of the sweep: %+v", spec)
	}
}

// A harness that cannot express these options must SAY so per option — the
// kimi adapter's reasons, rendered into the same shape launch_agent reports.
func TestKimiReportsTheSweepUnapplied(t *testing.T) {
	h := newKimiHarness(nil, "", "")
	got := unappliedSweep(harness.StartSpec{
		FallbackModels:  []string{"x"},
		DisallowedTools: []string{"Bash"},
		AddDirs:         []string{"/ref"},
	})
	if len(got) != 3 {
		t.Fatalf("reported %d unapplied options, want all 3: %+v", len(got), got)
	}
	for _, u := range got {
		if u.Reason == "" {
			t.Errorf("option %q reported with no reason", u.Option)
		}
	}
	// Nothing requested, nothing reported.
	if n := len(unappliedSweep(harness.StartSpec{})); n != 0 {
		t.Errorf("reported %d unapplied options for an empty request", n)
	}
	if caps := h.Capabilities(); caps.FallbackModels || caps.DisallowedTools || caps.AddDirs {
		t.Error("kimi claims a sweep capability it does not have")
	}
}
