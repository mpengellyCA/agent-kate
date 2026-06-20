package vsix

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveGuardsCacheDir(t *testing.T) {
	cache := t.TempDir()
	m := NewManager(cache)

	// A real installed extension dir is removable.
	extDir := filepath.Join(cache, "pub.ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("pub.ext"); err != nil {
		t.Fatalf("Remove(pub.ext): %v", err)
	}
	if _, err := os.Stat(extDir); !os.IsNotExist(err) {
		t.Fatal("extension dir should be gone")
	}

	// Removing an absent extension is not an error.
	if err := m.Remove("pub.absent"); err != nil {
		t.Fatalf("Remove(absent) should be a no-op, got %v", err)
	}

	// A sentinel sibling of the cache dir must survive any crafted id.
	sentinel := filepath.Join(filepath.Dir(cache), "victim")
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../victim", "..", "pub.name/../../victim"} {
		_ = m.Remove(bad) // an invalid id may error; what matters is the FS
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("Remove(%q) escaped the cache and deleted the sentinel", bad)
		}
	}
}

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

func TestHeuristicFindsTailwindStyleServer(t *testing.T) {
	dir := t.TempDir()
	// A Tailwind-style layout: activation bundle in dist/extension.js spawns
	// dist/tailwindServer.js. The basename doesn't match server.js exactly,
	// so detection has to come from the main-script scan.
	writeExtension(t, dir, `{
		"name":"vscode-tailwindcss","version":"0.14.0","main":"dist/extension.js",
		"contributes":{"languages":[{"id":"tailwindcss","extensions":[".css"]}]}
	}`, map[string]string{
		"dist/extension.js":      `const serverModule = "./tailwindServer.js"; require(serverModule);`,
		"dist/tailwindServer.js": "//server",
	})

	ext, err := load("bradlc.vscode-tailwindcss", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server == nil {
		t.Fatal("expected a recipe via main-scan or basename suffix match")
	}
	if !strings.HasSuffix(ext.Server.Args[0], "tailwindServer.js") {
		t.Fatalf("recipe args = %v, want tailwindServer.js", ext.Server.Args)
	}
}

func TestMainScanRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// A package.json main pointing at ../../etc/passwd must not be honored.
	writeExtension(t, dir, `{
		"name":"evil","version":"1.0.0","main":"../../../../../../etc/passwd",
		"contributes":{"languages":[{"id":"x","extensions":[".x"]}]}
	}`, nil)
	// And a literal inside a real main that tries to escape should be ignored.
	writeExtension(t, dir+"-2", `{
		"name":"evil2","version":"1.0.0","main":"dist/extension.js",
		"contributes":{"languages":[{"id":"x","extensions":[".x"]}]}
	}`, map[string]string{
		"dist/extension.js": `require("../../../../etc/server.js");`,
	})
	for _, d := range []string{dir, dir + "-2"} {
		ext, err := load("acme.evil", d)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if ext.Server != nil {
			t.Fatalf("traversing main must not yield a recipe, got %+v", ext.Server)
		}
	}
}

func TestMainScanCaps(t *testing.T) {
	// An oversized main bundle still gets scanned (we read up to the cap)
	// but does not exhaust memory. We assert detection still works when the
	// server string sits inside the first cap-bytes window.
	dir := t.TempDir()
	prefix := `var x = "./dist/realServer.js"; ` // server reference up front
	padding := strings.Repeat("x", 1<<20)         // 1 MiB of filler
	writeExtension(t, dir, `{
		"name":"big","version":"1.0.0","main":"dist/extension.js",
		"contributes":{"languages":[{"id":"x","extensions":[".x"]}]}
	}`, map[string]string{
		"dist/extension.js": prefix + padding,
		"dist/realServer.js": "//s",
	})
	ext, err := load("acme.big", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server == nil || !strings.HasSuffix(ext.Server.Args[0], "realServer.js") {
		t.Fatalf("expected realServer.js recipe, got %+v", ext.Server)
	}
}

func TestCatalogLoads(t *testing.T) {
	c := Catalog()
	if len(c) == 0 {
		t.Fatal("embedded catalog is empty")
	}
	for _, e := range c {
		if e.ID == "" || e.DisplayName == "" {
			t.Fatalf("catalog entry missing fields: %+v", e)
		}
		if !strings.Contains(e.ID, ".") {
			t.Fatalf("catalog id %q must be publisher.name", e.ID)
		}
	}
}

func TestServerHintForExternalLSP(t *testing.T) {
	// Go relies on gopls on PATH. Whether or not gopls is actually installed
	// on the test host, the user-facing hint should be populated when no
	// recipe is produced.
	dir := t.TempDir()
	writeExtension(t, dir, `{
		"name":"go","version":"0.0.0",
		"contributes":{"languages":[{"id":"go","extensions":[".go"]}]}
	}`, nil)
	ext, err := load("golang.go", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server == nil && ext.ServerHint == "" {
		t.Fatal("expected either a gopls recipe or a server hint")
	}
	if ext.Server == nil && !strings.Contains(ext.ServerHint, "gopls") {
		t.Fatalf("hint = %q, want a gopls mention", ext.ServerHint)
	}
}

func TestDevSensePHPRegistry(t *testing.T) {
	dir := t.TempDir()
	writeExtension(t, dir, `{
		"name":"phptools-vscode","version":"1.0.0",
		"contributes":{"languages":[{"id":"php","extensions":[".php"]}]}
	}`, map[string]string{"out/server/devsense.php.ls": "#!/bin/sh\n"})
	// Mark the bundled server executable, as the real .vsix layout does.
	if err := os.Chmod(filepath.Join(extensionRoot(dir), "out/server/devsense.php.ls"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ext, err := load("devsense.phptools-vscode", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ext.Server == nil {
		t.Fatal("expected a devsense recipe pointing at the bundled .ls binary")
	}
	if !strings.HasSuffix(ext.Server.Command, "devsense.php.ls") {
		t.Fatalf("recipe command = %q, want the bundled binary", ext.Server.Command)
	}
	if ext.Server.Source != "registry" {
		t.Fatalf("source = %q, want registry", ext.Server.Source)
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
