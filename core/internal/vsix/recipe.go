package vsix

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// recipeResolver derives a launch recipe for one specific known extension.
type recipeResolver func(ext *Extension, man *manifest) *ServerRecipe

// registry maps an extension id to a curated, tested launch recipe. This is
// the reliable path; heuristicRecipe is only a fallback for everything else.
var registry = map[string]recipeResolver{
	"bmewburn.vscode-intelephense-client": resolveIntelephense,
	"devsense.phptools-vscode":            resolveDevSensePHP,

	// Extensions that delegate to a language-server binary the user installs
	// separately. Each recipe looks the binary up on PATH; the matching entry
	// in externalServerHint() supplies a helpful message when it's missing.
	"golang.go":                  pathRecipeResolver("gopls", nil, []string{"go"}, []string{".go"}),
	"rust-lang.rust-analyzer":    pathRecipeResolver("rust-analyzer", nil, []string{"rust"}, []string{".rs"}),
	"vscode.cpp":                 pathRecipeResolver("clangd", nil, []string{"c", "cpp"}, []string{".c", ".h", ".cc", ".cpp", ".hpp", ".cxx", ".hxx"}),
	"vscode.typescript":          pathRecipeResolver("typescript-language-server", []string{"--stdio"}, []string{"typescript", "typescriptreact", "javascript", "javascriptreact"}, []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}),
	"hashicorp.terraform":        pathRecipeResolver("terraform-ls", []string{"serve"}, []string{"terraform"}, []string{".tf", ".tfvars"}),
	"redhat.vscode-xml":          pathRecipeResolver("lemminx", nil, []string{"xml", "xsl", "xsd", "dtd"}, []string{".xml", ".xsl", ".xsd", ".dtd"}),
	"redhat.java":                pathRecipeResolver("jdtls", nil, []string{"java"}, []string{".java"}),
	"ms-azuretools.vscode-docker": pathRecipeResolver("docker-langserver", []string{"--stdio"}, []string{"dockerfile"}, []string{"Dockerfile"}),
	"charliermarsh.ruff":         pathRecipeResolver("ruff", []string{"server"}, []string{"python"}, []string{".py", ".pyi"}),
	"ms-python.python":           resolvePython,
}

// pathRecipeResolver returns a resolver that looks bin up on PATH and produces
// a ServerRecipe pointing at the absolute path. When bin is missing the
// resolver returns nil and the UI falls back to the hint in externalServerHint.
func pathRecipeResolver(bin string, args []string, ids, exts []string) recipeResolver {
	return func(_ *Extension, _ *manifest) *ServerRecipe {
		abs, err := exec.LookPath(bin)
		if err != nil {
			return nil
		}
		return &ServerRecipe{
			Command:        abs,
			Args:           args,
			LanguageIDs:    ids,
			FileExtensions: exts,
		}
	}
}

// resolvePython tries the Python language servers a user is likely to have on
// PATH, in order of preference. Pylance is proprietary and is not considered.
func resolvePython(_ *Extension, _ *manifest) *ServerRecipe {
	candidates := []struct {
		bin  string
		args []string
	}{
		{"pyright-langserver", []string{"--stdio"}},
		{"basedpyright-langserver", []string{"--stdio"}},
		{"pylsp", nil},
		{"jedi-language-server", nil},
	}
	for _, c := range candidates {
		abs, err := exec.LookPath(c.bin)
		if err != nil {
			continue
		}
		return &ServerRecipe{
			Command:        abs,
			Args:           c.args,
			LanguageIDs:    []string{"python"},
			FileExtensions: []string{".py", ".pyi"},
		}
	}
	return nil
}

// resolveDevSensePHP launches the PHP Tools language server binary the
// extension ships under out/server/. The binary name carries a .ls suffix and
// is platform-specific; the extension picks one at install time.
func resolveDevSensePHP(ext *Extension, _ *manifest) *ServerRecipe {
	bin := findShallowestFile(extensionRoot(ext.Dir), "devsense.php.ls")
	if bin == "" {
		return nil
	}
	info, err := os.Stat(bin)
	if err != nil || info.Mode()&0o111 == 0 {
		return nil
	}
	return &ServerRecipe{
		Command:        bin,
		LanguageIDs:    []string{"php"},
		FileExtensions: []string{".php"},
	}
}

// externalServerHints maps an extension id to a one-line note describing the
// external binary that would activate language support. The UI shows this when
// the registry recipe returned nil (typically because the binary is not on
// PATH).
var externalServerHints = map[string]string{
	"golang.go":                   "install gopls (`go install golang.org/x/tools/gopls@latest`) and ensure it is on PATH",
	"rust-lang.rust-analyzer":     "install rust-analyzer and ensure it is on PATH (rustup component add rust-analyzer)",
	"vscode.cpp":                  "install clangd and ensure it is on PATH",
	"vscode.typescript":           "install typescript-language-server (npm i -g typescript typescript-language-server)",
	"hashicorp.terraform":         "install terraform-ls and ensure it is on PATH",
	"redhat.vscode-xml":           "install lemminx and ensure it is on PATH",
	"redhat.java":                 "install jdtls (e.g. via your distro or eclipse.jdt.ls) and ensure it is on PATH",
	"ms-azuretools.vscode-docker": "install dockerfile-language-server-nodejs (npm i -g dockerfile-language-server-nodejs)",
	"charliermarsh.ruff":          "install ruff (pipx install ruff) and ensure it is on PATH",
	"ms-python.python":            "install a Python language server (e.g. pipx install python-lsp-server, or pyright-langserver)",
}

// serverHint returns the user-facing hint for an extension id, or "" if none
// applies (most extensions either bundle a server or have no LSP at all).
func serverHint(extensionID string) string {
	return externalServerHints[extensionID]
}

// resolveRecipe finds how to launch an extension's language server: a curated
// registry recipe first, then a best-effort heuristic. It returns nil when no
// server could be identified.
func resolveRecipe(ext *Extension, man *manifest) *ServerRecipe {
	if resolver, ok := registry[ext.ID]; ok {
		if r := resolver(ext, man); r != nil {
			r.Source = "registry"
			return r
		}
	}
	if r := heuristicRecipe(ext, man); r != nil {
		r.Source = "heuristic"
		return r
	}
	return nil
}

// resolveIntelephense locates the PHP language server the Intelephense
// extension bundles and launches it over stdio with Node. The server file is
// found by name so the recipe survives the extension's internal layout
// changing between releases.
func resolveIntelephense(ext *Extension, _ *manifest) *ServerRecipe {
	js := findShallowestFile(extensionRoot(ext.Dir), "intelephense.js")
	if js == "" {
		return nil
	}
	return &ServerRecipe{
		Command:        "node",
		Args:           []string{js, "--stdio"},
		LanguageIDs:    []string{"php"},
		FileExtensions: []string{".php"},
	}
}

// heuristicRecipe is the fallback for extensions with no registry entry: it
// looks for a server entry point by name. It is deliberately conservative and
// returns nil rather than guess wrongly — a wrong guess fails confusingly.
func heuristicRecipe(ext *Extension, man *manifest) *ServerRecipe {
	ids, exts := man.languages()
	if len(ids) == 0 {
		return nil // not a language extension — nothing to serve
	}
	root := extensionRoot(ext.Dir)

	if bin := findServerBinary(root); bin != "" {
		return &ServerRecipe{
			Command:        bin,
			Args:           []string{"--stdio"},
			LanguageIDs:    ids,
			FileExtensions: exts,
		}
	}
	if js := findServerScript(root); js != "" {
		return &ServerRecipe{
			Command:        "node",
			Args:           []string{js, "--stdio"},
			LanguageIDs:    ids,
			FileExtensions: exts,
		}
	}
	// Last resort: ask the activation bundle where its server lives. Many
	// extensions hide their server under non-canonical names (Tailwind's
	// dist/tailwindServer.js, etc.) that the basename walk above won't catch.
	if js := findServerByMainScan(root, man.Main); js != "" {
		return &ServerRecipe{
			Command:        "node",
			Args:           []string{js, "--stdio"},
			LanguageIDs:    ids,
			FileExtensions: exts,
		}
	}
	return nil
}

// findShallowestFile returns the path of the file named exactly name closest to
// root (fewest path separators), or "" if none exists.
func findShallowestFile(root, name string) string {
	best, bestDepth := "", 1<<30
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != name {
			return nil
		}
		if depth := strings.Count(path, string(os.PathSeparator)); depth < bestDepth {
			best, bestDepth = path, depth
		}
		return nil
	})
	return best
}

// findServerScript looks for a Node LSP server script by basename, preferring
// the shallowest match.
func findServerScript(root string) string {
	best, bestDepth := "", 1<<30
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// node_modules and .git can be enormous in some extensions; the
			// server we care about sits in the extension's own dist/out.
			name := d.Name()
			if name == "node_modules" || name == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.HasSuffix(name, ".js") {
			return nil
		}
		if name == "server.js" ||
			strings.HasSuffix(name, "server.js") ||
			strings.Contains(name, "language-server") ||
			strings.Contains(name, "languageserver") {
			if depth := strings.Count(path, string(os.PathSeparator)); depth < bestDepth {
				best, bestDepth = path, depth
			}
		}
		return nil
	})
	return best
}

// findServerBinary looks for a native (executable) language server in the
// conventional places an extension might ship one.
func findServerBinary(root string) string {
	for _, sub := range []string{"bin", "server", "out"} {
		entries, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := strings.ToLower(e.Name())
			if !strings.Contains(name, "server") && !strings.Contains(name, "lsp") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Mode()&0o111 == 0 {
				continue // not executable
			}
			return filepath.Join(root, sub, e.Name())
		}
	}
	return ""
}
