package vsix

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitID(t *testing.T) {
	ns, name, err := splitID("bmewburn.vscode-intelephense-client")
	if err != nil {
		t.Fatalf("splitID: %v", err)
	}
	if ns != "bmewburn" || name != "vscode-intelephense-client" {
		t.Fatalf("splitID = %q, %q", ns, name)
	}
	for _, bad := range []string{"noseparator", ".name", "publisher.", ""} {
		if _, _, err := splitID(bad); err == nil {
			t.Fatalf("splitID(%q) should have failed", bad)
		}
	}
}

// writeZip builds a zip file at path from a name->content map.
func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, body := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnzip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "ext.vsix")
	writeZip(t, src, map[string]string{
		"extension/package.json":  `{"name":"x"}`,
		"extension/out/server.js": "server code",
	})
	dest := t.TempDir()
	if err := unzip(src, dest); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "extension", "out", "server.js"))
	if err != nil || string(got) != "server code" {
		t.Fatalf("extracted file = %q, err %v", got, err)
	}
}

func TestUnzipRejectsEscape(t *testing.T) {
	src := filepath.Join(t.TempDir(), "evil.vsix")
	writeZip(t, src, map[string]string{"../escape.txt": "pwned"})
	if err := unzip(src, t.TempDir()); err == nil {
		t.Fatal("unzip should reject an entry that escapes the destination")
	}
}

// writeExtension builds an unpacked extension directory in the layout unzip
// produces: a package.json plus arbitrary files under extension/.
func writeExtension(t *testing.T, root, manifestJSON string, files map[string]string) {
	t.Helper()
	ext := extensionRoot(root)
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, "package.json"),
		[]byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(ext, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManifestLanguages(t *testing.T) {
	dir := t.TempDir()
	writeExtension(t, dir, `{
		"name":"x","displayName":"X Lang","version":"1.2.3",
		"contributes":{"languages":[
			{"id":"foo","extensions":[".foo",".fo"]},
			{"id":"foo","extensions":[".foo"]}
		]}
	}`, nil)
	man, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if man.displayName() != "X Lang" || man.Version != "1.2.3" {
		t.Fatalf("manifest = %+v", man)
	}
	ids, exts := man.languages()
	if len(ids) != 1 || ids[0] != "foo" {
		t.Fatalf("ids = %v (expected de-duplicated [foo])", ids)
	}
	if len(exts) != 2 {
		t.Fatalf("exts = %v (expected de-duplicated [.foo .fo])", exts)
	}
}

func TestHeuristicRecipe(t *testing.T) {
	dir := t.TempDir()
	writeExtension(t, dir, `{
		"name":"x","version":"1.0.0",
		"contributes":{"languages":[{"id":"foo","extensions":[".foo"]}]}
	}`, map[string]string{"out/server.js": "//server"})

	ext, err := load("acme.x", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server == nil {
		t.Fatal("expected a heuristic recipe")
	}
	if ext.Server.Source != "heuristic" {
		t.Fatalf("source = %q, want heuristic", ext.Server.Source)
	}
	if ext.Server.Command != "node" || len(ext.Server.Args) != 2 ||
		!strings.HasSuffix(ext.Server.Args[0], "server.js") ||
		ext.Server.Args[1] != "--stdio" {
		t.Fatalf("recipe = %+v", ext.Server)
	}
}

func TestIntelephenseRegistry(t *testing.T) {
	dir := t.TempDir()
	writeExtension(t, dir, `{
		"name":"vscode-intelephense-client","version":"1.10.0",
		"contributes":{"languages":[{"id":"php","extensions":[".php"]}]}
	}`, map[string]string{
		"node_modules/intelephense/lib/intelephense.js": "//php server",
	})

	ext, err := load("bmewburn.vscode-intelephense-client", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server == nil {
		t.Fatal("expected the Intelephense registry recipe")
	}
	if ext.Server.Source != "registry" {
		t.Fatalf("source = %q, want registry", ext.Server.Source)
	}
	if ext.Server.Command != "node" ||
		!strings.HasSuffix(ext.Server.Args[0], "intelephense.js") ||
		ext.Server.Args[len(ext.Server.Args)-1] != "--stdio" {
		t.Fatalf("recipe = %+v", ext.Server)
	}
	if len(ext.Server.LanguageIDs) != 1 || ext.Server.LanguageIDs[0] != "php" {
		t.Fatalf("languageIds = %v, want [php]", ext.Server.LanguageIDs)
	}
}

func TestNoRecipeForNonLanguageExtension(t *testing.T) {
	dir := t.TempDir()
	writeExtension(t, dir, `{"name":"theme","version":"1.0.0"}`, nil)
	ext, err := load("acme.theme", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server != nil {
		t.Fatalf("a theme extension should yield no recipe, got %+v", ext.Server)
	}
}
