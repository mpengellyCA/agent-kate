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
	// The never-ask hint uses the harness's OWN vocabulary, only for roles that
	// did not already pin a mode — and NAMES the human approval it costs rather
	// than selling it as the way to get autonomous work (audit F1).
	if !strings.Contains(out, `never-ask mode is "yolo"`) {
		t.Error("no never-ask hint for the kimi role")
	}
	if !strings.Contains(out, "needs the human's approval") {
		t.Error("the never-ask hint does not name the human approval it costs")
	}
	if strings.Contains(out, `"bypassPermissions"`) {
		t.Error("gave a never-ask hint to a role that already pins a mode")
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

// TestFlushRefusesSymlinkedTemp pins audit F20b: the store's staging file must
// never be written *through* a pre-planted symlink. On the /tmp fallback path
// another local user can create <path>.tmp pointing at a file of theirs (or of
// ours) and os.WriteFile would follow it, redirecting the write. flush now
// unlinks the name first (which removes the link, never its target) and creates
// the staging file with O_EXCL|O_NOFOLLOW, so a link re-planted in the race
// window fails the save instead of redirecting it.
func TestFlushRefusesSymlinkedTemp(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "modes.json")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("do not clobber"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, store+".tmp"); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Mode{Name: "attacked"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "do not clobber" {
		t.Fatalf("write followed the symlink: victim now %q", b)
	}
	fi, err := os.Lstat(store)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("store path is a symlink")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("store written group/world readable (mode %04o)", perm)
	}
	if _, err := os.Lstat(store + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging file left behind: %v", err)
	}
}

// TestFlushFailsOnRacedSymlink covers the other half: a symlink that appears
// between the unlink and the create (an attacker spinning on the path) must
// make the save fail, not silently write somewhere else.
func TestFlushFailsOnRacedSymlink(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "modes.json")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the unlink impossible to win: a read-only parent directory would be
	// the real-world equivalent; here we simply pre-create the staging path as a
	// symlink AND make Remove fail by removing write permission after planting.
	if err := os.Symlink(victim, store+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s := &Store{path: store, user: map[string]Mode{}, suppressed: map[string]bool{}}
	if err := s.flush(); err == nil {
		t.Fatal("flush succeeded with an unremovable symlinked staging path")
	}
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "intact" {
		t.Fatalf("write followed the symlink: victim now %q", b)
	}
}

// TestStoreMigratesWorldReadableModes is the case every existing installation
// is in (audit F2, migration gap): modes.json already exists, created 0644 in a
// 0755 directory by an earlier build. flush's 0600 applies only to files it
// CREATES, so without a migration pass at open the file a user already has
// stays world-readable forever — and an ensemble carries the master prompt and
// every role's system prompt.
func TestStoreMigratesWorldReadableModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentkate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "modes.json")
	if err := os.WriteFile(path, []byte(`{"modes":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	di, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %o after open, want 700", got)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("modes.json mode = %o after open, want 600", got)
	}
}

// A planted staging symlink must not stop the daemon starting. harden
// deliberately skips <path>.tmp: flush unlinks and re-creates it
// O_EXCL|O_NOFOLLOW at 0600, so there is nothing to migrate, and failing the
// OPEN on a name any local user can plant (on the /tmp fallback path) would
// turn a handled condition into a denial of service.
func TestStoreOpensDespiteSymlinkedStagingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "modes.json")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err != nil {
		t.Fatalf("NewStore refused to open over a planted staging symlink: %v", err)
	}
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "intact" {
		t.Fatalf("open touched the symlink's target: %q", b)
	}
}

// A store FILE replaced by a symlink is the chmod-redirect primitive and must
// fail the open loudly rather than tighten someone else's file.
func TestStoreRefusesSymlinkedStoreFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "modes.json")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil {
		t.Fatal("NewStore opened a store whose file is a symlink")
	}
	fi, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("chmod'd through the symlink: victim now %o", got)
	}
}
