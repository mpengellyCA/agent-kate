package vsix

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// recipeResolver derives a launch recipe for one specific known extension.
type recipeResolver func(ext *Extension, man *manifest) *ServerRecipe

// registry maps an extension id to a curated, tested launch recipe. This is
// the reliable path; heuristicRecipe is only a fallback for everything else.
var registry = map[string]recipeResolver{
	"bmewburn.vscode-intelephense-client": resolveIntelephense,
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
		if err != nil || d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.HasSuffix(name, ".js") {
			return nil
		}
		if name == "server.js" ||
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
