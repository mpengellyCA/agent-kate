package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"agentkate/internal/agent"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/modes"
	"agentkate/internal/session"
)

// personaFake is the fakeHarness with the persona channel declared, so
// mode.apply's "system prompt where the capability exists" branch is exercised
// against a harness that HAS it (the bare fakeHarness stands in for one that
// does not). It also declares a permission-mode vocabulary, since the master
// prompt's unattended hint reads the last entry.
type personaFake struct{ *fakeHarness }

func (p personaFake) Capabilities() harness.Capabilities {
	c := p.fakeHarness.Capabilities()
	c.SystemPrompt = true
	c.PermissionModes = []string{"default", "wideOpen"}
	return c
}

// modeTestCore spins the mode handlers over a real bus with one registered
// harness and an ensemble store seeded from a temp file.
func modeTestCore(t *testing.T, sessions *session.Store, h harness.Harness) (*ipc.Client, *modes.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "modes.sock")
	srv := ipc.NewServer(sock, log)
	store, err := modes.NewStore(filepath.Join(t.TempDir(), "modes.json"))
	if err != nil {
		t.Fatalf("modes.NewStore: %v", err)
	}
	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry(h.Capabilities().ID)
	harnesses.Register(h)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	d := handlerDeps{
		srv: srv, sup: sup, harnesses: harnesses,
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		threads: newThreadRegistry(), gitCache: gitCache,
		sessions: sessions, modes: store, log: log,
	}
	registerModeHandlers(d)
	serveIPC(t, srv, sock)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, store
}

// TestModeCatalogueRPCs drives list/get/save/delete over the bus.
func TestModeCatalogueRPCs(t *testing.T) {
	client, _ := modeTestCore(t, testSessions(t), &fakeHarness{})

	var list struct {
		Modes []struct {
			Name    string `json:"name"`
			BuiltIn bool   `json:"builtIn"`
		} `json:"modes"`
		DefaultMasterPrompt string `json:"defaultMasterPrompt"`
	}
	if err := client.Call("mode.list", map[string]any{}, &list); err != nil {
		t.Fatalf("mode.list: %v", err)
	}
	if len(list.Modes) != len(modes.BuiltIns()) || !list.Modes[0].BuiltIn {
		t.Fatalf("mode.list = %+v, want the built-ins", list.Modes)
	}
	if !strings.Contains(list.DefaultMasterPrompt, "{{worker_roster}}") {
		t.Error("mode.list did not serve the default master prompt template")
	}

	// Save, then read back through get.
	saveParams := map[string]any{"mode": map[string]any{
		"name":       "Bus Crew",
		"controller": map[string]any{"backend": "fake", "model": "fake-large"},
		"workers": []map[string]any{
			{"role": "coder", "backend": "fake", "model": "fake-small", "notes": "does the work"},
		},
	}}
	if err := client.Call("mode.save", saveParams, nil); err != nil {
		t.Fatalf("mode.save: %v", err)
	}
	var got struct {
		Mode struct {
			Name    string `json:"name"`
			BuiltIn bool   `json:"builtIn"`
			Workers []struct {
				Role  string `json:"role"`
				Notes string `json:"notes"`
			} `json:"workers"`
		} `json:"mode"`
	}
	if err := client.Call("mode.get", map[string]any{"name": "Bus Crew"}, &got); err != nil {
		t.Fatalf("mode.get: %v", err)
	}
	if got.Mode.BuiltIn || len(got.Mode.Workers) != 1 || got.Mode.Workers[0].Notes != "does the work" {
		t.Fatalf("mode.get = %+v", got.Mode)
	}

	if err := client.Call("mode.delete", map[string]any{"name": "Bus Crew"}, nil); err != nil {
		t.Fatalf("mode.delete: %v", err)
	}
	if err := client.Call("mode.get", map[string]any{"name": "Bus Crew"}, nil); err == nil {
		t.Error("mode.get resolved a deleted ensemble")
	}
	// Errors, not silent successes, for the unknown-name paths.
	if err := client.Call("mode.delete", map[string]any{"name": "Nope"}, nil); err == nil {
		t.Error("mode.delete accepted an unknown ensemble")
	}
	if err := client.Call("mode.save", map[string]any{"mode": map[string]any{"name": ""}}, nil); err == nil {
		t.Error("mode.save accepted a nameless ensemble")
	}
}

// TestModeApplyBriefsTheController is the phase's core assertion: applying an
// ensemble creates ONE thread, marked controller, whose opening message is the
// rendered master prompt (roster included) — and nothing else is spawned.
func TestModeApplyBriefsTheController(t *testing.T) {
	sessions := testSessions(t)
	proj := t.TempDir()
	fake := &fakeHarness{personaApplied: true}
	client, store := modeTestCore(t, sessions, personaFake{fake})

	if _, err := store.Save(modes.Mode{
		Name:       "Apply Me",
		Controller: modes.Participant{Backend: "fake", Model: "fake-large"},
		Workers: []modes.Participant{
			{Role: "coder", Backend: "fake", Model: "fake-small", Isolation: "workspace",
				Notes: "writes the code"},
		},
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	var res struct {
		ThreadID            string              `json:"threadId"`
		Backend             string              `json:"backend"`
		Ensemble            string              `json:"ensemble"`
		Applied             map[string]string   `json:"applied"`
		SystemPromptApplied bool                `json:"systemPromptApplied"`
		Unapplied           []map[string]string `json:"unapplied"`
	}
	if err := client.Call("mode.apply", map[string]any{
		"name": "Apply Me", "workDir": proj, "task": "Ship the thing.",
	}, &res); err != nil {
		t.Fatalf("mode.apply: %v", err)
	}
	if res.ThreadID == "" || res.Ensemble != "Apply Me" {
		t.Fatalf("mode.apply = %+v", res)
	}

	// Exactly one thread, and it is a controller from birth.
	recs := sessions.List("")
	if len(recs) != 1 {
		t.Fatalf("mode.apply created %d threads, want exactly the controller", len(recs))
	}
	rec := recs[0]
	if rec.Role != session.RoleController {
		t.Errorf("controller role = %q, want %q", rec.Role, session.RoleController)
	}
	if rec.ParentThreadID != "" {
		t.Errorf("controller has parent %q; it was launched by the human", rec.ParentThreadID)
	}
	if rec.Title == "" || !strings.Contains(rec.Title, "Apply Me") {
		t.Errorf("roster title = %q, want the ensemble name", rec.Title)
	}

	// The briefing: the rendered master prompt reached the harness as the
	// OPENING MESSAGE, with the roster and the task in it.
	spec := fake.spec()
	for _, want := range []string{
		"Apply Me", proj, "mcp__cooperation__launch_agent",
		`backend="fake"`, `model="fake-small"`, "writes the code",
		"Ship the thing.",
	} {
		if !strings.Contains(spec.Prompt, want) {
			t.Errorf("opening message is missing %q", want)
		}
	}
	// …and, since this harness declares the persona channel, also as the
	// system prompt, so the rules survive the opening message ageing out.
	if spec.SystemPrompt != spec.Prompt {
		t.Errorf("system prompt (%d bytes) is not the master prompt (%d bytes)",
			len(spec.SystemPrompt), len(spec.Prompt))
	}
	if !res.SystemPromptApplied {
		t.Error("systemPromptApplied is false on a harness that applied it")
	}
	// Applied-truth reaches the caller: this fake downgrades every model, and
	// mode.apply names that instead of pretending the ensemble ran as written.
	if res.Applied["model"] != "fake-small" {
		t.Errorf("applied model = %q, want the harness's own answer", res.Applied["model"])
	}
	if len(res.Unapplied) != 1 || res.Unapplied[0]["option"] != "model" ||
		res.Unapplied[0]["requested"] != "fake-large" {
		t.Errorf("unapplied = %v, want exactly the model downgrade", res.Unapplied)
	}
	// The unattended hint used this harness's own last permission mode.
	if !strings.Contains(spec.Prompt, `permission_mode="wideOpen"`) {
		t.Error("roster hint did not use the harness's declared vocabulary")
	}
}

// TestModeApplyReportsUnappliedPersona: on a harness with no persona channel
// the master prompt still arrives (as the opening message), and the missing
// channel is REPORTED rather than emulated or silently dropped.
func TestModeApplyReportsUnappliedPersona(t *testing.T) {
	sessions := testSessions(t)
	proj := t.TempDir()
	fake := &fakeHarness{} // no persona capability, applies nothing
	client, store := modeTestCore(t, sessions, fake)
	if _, err := store.Save(modes.Mode{
		Name:       "Honest",
		Controller: modes.Participant{Backend: "fake"},
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	var res struct {
		SystemPromptApplied bool                `json:"systemPromptApplied"`
		Unapplied           []map[string]string `json:"unapplied"`
	}
	if err := client.Call("mode.apply",
		map[string]any{"name": "Honest", "workDir": proj}, &res); err != nil {
		t.Fatalf("mode.apply: %v", err)
	}
	if res.SystemPromptApplied {
		t.Error("claimed the system prompt applied on a harness without the channel")
	}
	// Nothing was REQUESTED through a channel the harness lacks, so nothing is
	// reported as unapplied — the master prompt went where it always works.
	for _, u := range res.Unapplied {
		if u["option"] == "systemPrompt" {
			t.Errorf("reported a systemPrompt downgrade that was never requested: %v", u)
		}
	}
	if spec := fake.spec(); spec.SystemPrompt != "" {
		t.Error("sent a system prompt to a harness that has no such channel")
	} else if !strings.Contains(spec.Prompt, "mcp__cooperation__launch_agent") {
		t.Error("the controller was not briefed at all")
	}
}

func TestModeApplyValidation(t *testing.T) {
	client, _ := modeTestCore(t, testSessions(t), &fakeHarness{})
	for name, params := range map[string]map[string]any{
		"unknown ensemble": {"name": "Ghost Crew", "workDir": t.TempDir()},
		"missing workDir":  {"name": modes.BuiltIns()[0].Name},
	} {
		if err := client.Call("mode.apply", params, nil); err == nil {
			t.Errorf("mode.apply accepted %s", name)
		}
	}
	// A built-in naming an engine this arena does not have fails loudly rather
	// than silently falling back to the default engine.
	if err := client.Call("mode.apply", map[string]any{
		"name": modes.BuiltIns()[0].Name, "workDir": t.TempDir(),
	}, nil); err == nil {
		t.Error("mode.apply accepted an ensemble whose engine is not registered")
	}
}
