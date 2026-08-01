package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListAndInstall(t *testing.T) {
	root := t.TempDir()
	catDir := filepath.Join(root, "catalog")

	writeFile(t, filepath.Join(catDir, "review", "SKILL.md"),
		"---\nname: review\ndescription: Code review helper\n---\nbody\n")
	writeFile(t, filepath.Join(catDir, "quickfix.md"),
		"---\ndescription: \"Single-file skill\"\n---\nbody\n")
	writeFile(t, filepath.Join(catDir, "garbage", "not-a-skill.txt"), "x") // ignored

	c := New(catDir)
	skills, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(skills), skills)
	}
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	if byName["review"].Description != "Code review helper" || !byName["review"].IsDir {
		t.Errorf("bad review skill: %+v", byName["review"])
	}
	if byName["quickfix"].Description != "Single-file skill" || byName["quickfix"].IsDir {
		t.Errorf("bad quickfix skill: %+v", byName["quickfix"])
	}

	target := filepath.Join(root, "project")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Install("review", target); err != nil {
		t.Fatalf("install review: %v", err)
	}
	if _, err := c.Install("quickfix", target); err != nil {
		t.Fatalf("install quickfix: %v", err)
	}

	installed, err := c.ListInstalled(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 2 {
		t.Fatalf("want 2 installed, got %d: %+v", len(installed), installed)
	}
	for _, i := range installed {
		if !i.InCatalog {
			t.Errorf("installed %q should be marked InCatalog: %+v", i.Name, i)
		}
	}

	// One catalog, every engine (plan 16 P6): the same skill is linked into
	// BOTH engines' directories. Verified against kimi 0.30.0 — a skill in
	// .agents/skills/ reaches an ACP session; a Claude-only install would have
	// left kimi threads with no skills at all.
	for _, rel := range skillDirs {
		for _, name := range []string{"review", "quickfix.md"} {
			link := filepath.Join(target, rel, name)
			st, err := os.Lstat(link)
			if err != nil {
				t.Fatalf("%s was not installed: %v", link, err)
			}
			if st.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s is not a symlink into the catalog", link)
			}
		}
	}

	// Re-install replaces cleanly.
	if _, err := c.Install("review", target); err != nil {
		t.Fatalf("re-install: %v", err)
	}

	// Uninstall removes the symlinks.
	if err := c.Uninstall("review", target); err != nil {
		t.Fatalf("uninstall review: %v", err)
	}
	if err := c.Uninstall("quickfix", target); err != nil {
		t.Fatalf("uninstall quickfix: %v", err)
	}
	installed, err = c.ListInstalled(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 0 {
		t.Errorf("want 0 installed after uninstall, got %+v", installed)
	}
	// …from every engine's directory, not just the canonical one.
	for _, rel := range skillDirs {
		entries, err := os.ReadDir(filepath.Join(target, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s still holds %d entries after uninstall", rel, len(entries))
		}
	}
}

// TestUninstallLeavesForeignLinksAlone: the refusal to delete something the
// catalog does not own has to hold in EVERY engine directory, not just the
// first one Uninstall happens to visit.
func TestUninstallLeavesForeignLinksAlone(t *testing.T) {
	root := t.TempDir()
	catDir := filepath.Join(root, "catalog")
	writeFile(t, filepath.Join(catDir, "review", "SKILL.md"),
		"---\nname: review\ndescription: d\n---\nbody\n")
	c := New(catDir)
	target := filepath.Join(root, "project")
	// Install requires the project directory to already exist (it refuses to
	// seed a skills tree into an arbitrary caller-supplied path).
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Install("review", target); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Someone replaced the .agents link with a symlink of their own.
	foreign := filepath.Join(root, "elsewhere")
	writeFile(t, filepath.Join(foreign, "SKILL.md"), "---\n---\n")
	link := filepath.Join(target, skillDirs[1], "review")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	if err := c.Uninstall("review", target); err == nil {
		t.Error("uninstall removed a link that does not point into the catalog")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the foreign link was removed: %v", err)
	}
}

func TestUninstallRefusesNonSymlink(t *testing.T) {
	root := t.TempDir()
	c := New(filepath.Join(root, "catalog"))
	target := filepath.Join(root, "project")
	// A real (non-symlink) skill directory inside the target — user's own work.
	writeFile(t, filepath.Join(target, ".claude", "skills", "mine", "SKILL.md"), "---\n---\n")
	if err := c.Uninstall("mine", target); err == nil {
		t.Error("expected refusal to remove a non-symlink skill")
	}
}

func TestValidateName(t *testing.T) {
	c := New(t.TempDir())
	for _, bad := range []string{"", "..", "../etc", "a/b", `a\b`, "."} {
		if _, err := c.Get(bad); err == nil {
			t.Errorf("Get(%q) should have errored", bad)
		}
	}
}

func TestCreateAndRead(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "catalog"))

	// A description with a colon (a YAML gotcha) must still round-trip.
	const want = "Run gofmt to tidy Go code: format on save"
	skill, err := c.Create("format-go", "  Run gofmt to tidy Go code: format on save  ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !skill.IsDir || skill.Name != "format-go" {
		t.Fatalf("created skill = %+v", skill)
	}
	if skill.Description != want {
		t.Fatalf("returned description = %q, want %q", skill.Description, want)
	}

	// The new skill is discoverable and its frontmatter parses back.
	got, err := c.Get("format-go")
	if err != nil {
		t.Fatalf("Get after Create: %v", err)
	}
	if got.Description != want {
		t.Fatalf("description round-trip = %q, want %q", got.Description, want)
	}

	content, err := c.ReadContent("format-go")
	if err != nil {
		t.Fatalf("ReadContent: %v", err)
	}
	if !strings.Contains(content, "format-go") {
		t.Fatalf("unexpected content: %q", content)
	}

	// Creating a duplicate is rejected.
	if _, err := c.Create("format-go", "again"); err == nil {
		t.Fatal("Create should reject a duplicate name")
	}
	// Invalid names are rejected.
	if _, err := c.Create("../escape", ""); err == nil {
		t.Fatal("Create should reject an invalid name")
	}
}
