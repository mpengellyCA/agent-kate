package modes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "modes.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, path
}

func names(list []Mode) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Name)
	}
	return out
}

// TestBuiltInsAreAvailableUnconfigured: a first run with no modes.json still
// offers the shipped catalogue — the picker is never empty.
func TestBuiltInsAreAvailableUnconfigured(t *testing.T) {
	s, path := newTestStore(t)
	list := s.List()
	if len(list) != len(BuiltIns()) {
		t.Fatalf("List() = %v, want the %d built-ins", names(list), len(BuiltIns()))
	}
	for _, m := range list {
		if !m.BuiltIn {
			t.Errorf("%q is not marked built-in", m.Name)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("reading the catalogue wrote %s (err %v); it must stay read-only", path, err)
	}
}

// TestBuiltInsCarryRealVocabulary guards the one thing a shipped ensemble can
// get silently wrong: a model string no CLI knows. The exact ids are probed
// facts (plan 16 P4) — claude aliases and kimi's ACP enumeration.
func TestBuiltInsCarryRealVocabulary(t *testing.T) {
	claudeAliases := map[string]bool{
		"sonnet": true, "opus": true, "haiku": true, "fable": true,
		"best": true, "opusplan": true, "default": true,
	}
	kimiModels := map[string]bool{
		"kimi-code/k3": true, "kimi-code/k3-256k": true,
		"kimi-code/kimi-for-coding": true, "kimi-code/kimi-for-coding-highspeed": true,
	}
	check := func(name string, p Participant) {
		switch p.Backend {
		case "claude":
			if !claudeAliases[p.Model] {
				t.Errorf("%s: %q is not a claude model alias", name, p.Model)
			}
		case "kimi":
			if !kimiModels[p.Model] {
				t.Errorf("%s: %q is not a discovered kimi model id", name, p.Model)
			}
		default:
			t.Errorf("%s: unknown backend %q", name, p.Backend)
		}
	}
	for _, m := range BuiltIns() {
		check(m.Name+"/controller", m.Controller)
		if len(m.Workers) == 0 {
			t.Errorf("%s has no worker roles", m.Name)
		}
		for _, w := range m.Workers {
			check(m.Name+"/"+w.Role, w)
			if w.Isolation != "" && w.Isolation != "auto" &&
				w.Isolation != "isolated" && w.Isolation != "workspace" {
				t.Errorf("%s/%s: bad isolation %q", m.Name, w.Role, w.Isolation)
			}
		}
	}
}

// TestSaveGetDeleteRoundTrip: a user ensemble survives a reopen, and deleting
// it removes it for good.
func TestSaveGetDeleteRoundTrip(t *testing.T) {
	s, path := newTestStore(t)
	mine := Mode{
		Name:       "My Crew",
		Controller: Participant{Backend: "kimi", Model: "kimi-code/k3"},
		Workers: []Participant{{
			Role: "coder", Backend: "claude", Model: "opus", Isolation: "auto",
		}},
		MasterPrompt: "Do it your way, {{ensemble_name}}.",
	}
	if _, err := s.Save(mine); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("My Crew")
	if !ok {
		t.Fatal("saved ensemble did not survive a reopen")
	}
	if got.Controller.Model != "kimi-code/k3" || len(got.Workers) != 1 ||
		got.Workers[0].Model != "opus" || got.MasterPrompt != mine.MasterPrompt {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if got.BuiltIn {
		t.Error("a user ensemble is marked built-in")
	}
	if got.Updated.IsZero() {
		t.Error("Save did not stamp Updated")
	}

	if err := reopened.Delete("My Crew"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := reopened.Get("My Crew"); ok {
		t.Error("deleted ensemble is still resolvable")
	}
	again, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	if _, ok := again.Get("My Crew"); ok {
		t.Error("delete did not survive a reopen")
	}
}

// TestUserEntryShadowsBuiltIn: editing a shipped ensemble overrides it without
// duplicating the name, and deleting the edit reveals the original again.
func TestUserEntryShadowsBuiltIn(t *testing.T) {
	s, path := newTestStore(t)
	name := BuiltIns()[0].Name
	edited := BuiltIns()[0]
	edited.Controller.Model = "haiku"
	if _, err := s.Save(edited); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list := reopened.List()
	if seen := strings.Count(strings.Join(names(list), "\n"), name); seen != 1 {
		t.Fatalf("built-in %q appears %d times after an override: %v", name, seen, names(list))
	}
	got, _ := reopened.Get(name)
	if got.Controller.Model != "haiku" || got.BuiltIn {
		t.Fatalf("override not in effect: %+v", got)
	}

	// Deleting the override is "undo my edit", not "delete the built-in".
	if err := reopened.Delete(name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	restored, ok := reopened.Get(name)
	if !ok || !restored.BuiltIn || restored.Controller.Model == "haiku" {
		t.Fatalf("deleting an override did not restore the built-in: %+v", restored)
	}
}

// TestDeletedBuiltInStaysDeleted: the built-in list is code, so a deletion has
// to be recorded as a suppression or it returns on the next launch.
func TestDeletedBuiltInStaysDeleted(t *testing.T) {
	s, path := newTestStore(t)
	name := BuiltIns()[0].Name
	if err := s.Delete(name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.Get(name); ok {
		t.Fatalf("deleted built-in %q came back after a reopen", name)
	}
	if got := len(reopened.List()); got != len(BuiltIns())-1 {
		t.Fatalf("List() has %d entries, want %d", got, len(BuiltIns())-1)
	}
	// Saving under the name again is an un-delete.
	if _, err := reopened.Save(BuiltIns()[0]); err != nil {
		t.Fatalf("Save after delete: %v", err)
	}
	if _, ok := reopened.Get(name); !ok {
		t.Fatal("re-saving a suppressed name did not bring it back")
	}
}

func TestValidateRejectsUnusableEnsembles(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Save(Mode{Name: "  "}); err == nil {
		t.Error("saved a nameless ensemble")
	}
	if _, err := s.Save(Mode{
		Name:    "Nameless Worker",
		Workers: []Participant{{Backend: "claude", Model: "opus"}},
	}); err == nil {
		t.Error("saved a worker with no role name")
	}
	// A model id we have never heard of is NOT rejected: the vocabulary belongs
	// to the harness and changes with every CLI release.
	if _, err := s.Save(Mode{
		Name:       "Tomorrow's Model",
		Controller: Participant{Backend: "claude", Model: "claude-opus-9"},
	}); err != nil {
		t.Errorf("rejected an unknown model id: %v", err)
	}
}

// TestRenderFillsPlaceholders: the controller must receive a briefing with no
// unsubstituted placeholders and with each role's exact launch arguments.
func TestRenderFillsPlaceholders(t *testing.T) {
	m := Mode{
		Name:       "Test Crew",
		Controller: Participant{Backend: "claude", Model: "fable"},
		Workers: []Participant{
			{Role: "coder", Backend: "kimi", Model: "kimi-code/k3", Isolation: "auto",
				Notes: "Implements changes."},
			{Role: "locked", Backend: "claude", Model: "opus", PermissionMode: "plan"},
		},
	}
	out := Render(m, "/home/u/proj", map[string]string{
		"claude": "bypassPermissions", "kimi": "yolo",
	})
	for _, placeholder := range []string{"{{ensemble_name}}", "{{workspace}}", "{{worker_roster}}"} {
		if strings.Contains(out, placeholder) {
			t.Errorf("%s survived rendering", placeholder)
		}
	}
	for _, want := range []string{
		"Test Crew", "/home/u/proj",
		`backend="kimi"`, `model="kimi-code/k3"`, `isolation="auto"`,
		"Implements changes.",
		`backend="claude"`, `model="opus"`, `permission_mode="plan"`,
		"mcp__cooperation__launch_agent", "mcp__cooperation__wait_agent",
		"mcp__cooperation__post_note", "claim_file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prompt is missing %q", want)
		}
	}
	// The unattended hint uses the harness's OWN vocabulary, and only for roles
	// that did not already pin a mode.
	if !strings.Contains(out, `permission_mode="yolo"`) {
		t.Error("no unattended hint for the kimi role")
	}
	if strings.Contains(out, `permission_mode="bypassPermissions"`) {
		t.Error("gave an unattended hint to a role that already pins a mode")
	}
}

// TestRenderUsesCustomPromptAndSurvivesEmptyRoster.
func TestRenderUsesCustomPromptAndSurvivesEmptyRoster(t *testing.T) {
	m := Mode{
		Name:         "Solo",
		Controller:   Participant{Backend: "claude", Model: "opus"},
		MasterPrompt: "You are {{ensemble_name}} in {{workspace}}.\n{{worker_roster}}",
	}
	out := Render(m, "/tmp/p", nil)
	if !strings.HasPrefix(out, "You are Solo in /tmp/p.") {
		t.Fatalf("custom master prompt not used: %q", out)
	}
	if strings.Contains(out, "{{worker_roster}}") || !strings.Contains(out, "no worker roles") {
		t.Fatalf("empty roster not rendered honestly: %q", out)
	}
}

// TestNoPermissionHintWithoutVocabulary: a harness whose permission modes are
// discovered per session contributes no hint, rather than a guessed one.
func TestNoPermissionHintWithoutVocabulary(t *testing.T) {
	m := Mode{
		Name:       "Quiet",
		Controller: Participant{Backend: "kimi", Model: "kimi-code/k3"},
		Workers:    []Participant{{Role: "coder", Backend: "kimi", Model: "kimi-code/k3"}},
	}
	if out := Render(m, "/p", map[string]string{}); strings.Contains(out, "Unattended:") {
		t.Errorf("invented a permission mode with no vocabulary: %q", out)
	}
}

// TestWorkerRosterDefaultsBackendToController: an unqualified worker inherits
// the controller's engine, which is what launch_agent does with an empty
// backend — the roster must say the same thing.
func TestWorkerRosterDefaultsBackendToController(t *testing.T) {
	m := Mode{
		Name:       "Inherit",
		Controller: Participant{Backend: "kimi", Model: "kimi-code/k3"},
		Workers:    []Participant{{Role: "coder"}},
	}
	if out := WorkerRoster(m, nil); !strings.Contains(out, `backend="kimi"`) {
		t.Errorf("roster did not inherit the controller's backend: %q", out)
	}
}
