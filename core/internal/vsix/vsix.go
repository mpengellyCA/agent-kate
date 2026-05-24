// Package vsix downloads VS Code extensions from the Open VSX registry,
// unpacks them, and works out how to launch the language server each one
// bundles — letting Agent Kate reuse VS Code's language tooling (e.g.
// Intelephense for PHP) to push Kate towards VS Code parity.
//
// Detecting a server inside an arbitrary extension is not generally solvable:
// the launch logic normally lives in the extension's JavaScript, which expects
// the VS Code extension host. vsix therefore uses a curated registry of launch
// recipes for known extensions, with a best-effort heuristic fallback for the
// rest (see recipe.go).
package vsix

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ServerRecipe describes how to launch the language server an extension
// bundles. Command is run with Args; the final argument is the server's stdio
// flag, so it speaks LSP over its standard streams.
type ServerRecipe struct {
	Command        string   `json:"command"`        // executable: a path or a PATH name
	Args           []string `json:"args"`           // arguments, ending in the stdio flag
	LanguageIDs    []string `json:"languageIds"`    // LSP language ids the server handles
	FileExtensions []string `json:"fileExtensions"` // file extensions (".php") it handles
	Source         string   `json:"source"`         // "registry" or "heuristic"
}

// Extension is one installed VS Code extension.
type Extension struct {
	ID      string        `json:"id"`      // "publisher.name"
	Name    string        `json:"name"`    // human-readable display name
	Version string        `json:"version"` // installed version
	Dir     string        `json:"dir"`     // unpacked root; contains extension/
	Server  *ServerRecipe `json:"server"`  // nil when no server could be detected
}

// versionMarker is written into an extension's cache dir to record which
// version is unpacked there, so Install can skip a re-download.
const versionMarker = ".akversion"

// Manager installs and tracks VS Code extensions under a cache directory.
type Manager struct {
	cacheDir string
}

// NewManager returns a Manager storing extensions under cacheDir.
func NewManager(cacheDir string) *Manager {
	return &Manager{cacheDir: cacheDir}
}

// DefaultCacheDir is where extensions live when the UI does not override it.
func DefaultCacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "agentkate", "extensions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".cache", "agentkate", "extensions")
}

// Install downloads the latest version of extensionID ("publisher.name") from
// Open VSX, unpacks it and detects its language server. A cached copy of the
// same version is reused. The returned Extension has Server populated when a
// launch recipe was found.
func (m *Manager) Install(ctx context.Context, extensionID string) (*Extension, error) {
	namespace, name, err := splitID(extensionID)
	if err != nil {
		return nil, err
	}
	meta, err := fetchMetadata(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.cacheDir, extensionID)

	// Already have this exact version unpacked? Reuse it.
	if cachedVersion(dir) == meta.Version {
		if ext, err := load(extensionID, dir); err == nil {
			return ext, nil
		}
	}

	downloadURL := meta.Files["download"]
	if downloadURL == "" {
		return nil, fmt.Errorf("open-vsx: %s has no downloadable .vsix", extensionID)
	}

	tmp, err := os.CreateTemp("", "agentkate-vsix-*.zip")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)

	if err := downloadFile(ctx, downloadURL, tmpName); err != nil {
		return nil, err
	}

	// Replace any previous (possibly different) version atomically enough:
	// wipe, then unpack fresh.
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := unzip(tmpName, dir); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, versionMarker),
		[]byte(meta.Version), 0o644); err != nil {
		return nil, err
	}
	return load(extensionID, dir)
}

// List returns every installed extension. Directories that do not hold a valid
// extension are skipped.
func (m *Manager) List() ([]*Extension, error) {
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Extension
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ext, err := load(e.Name(), filepath.Join(m.cacheDir, e.Name()))
		if err != nil {
			continue // half-written or unrelated dir
		}
		out = append(out, ext)
	}
	return out, nil
}

// Get returns a single installed extension, or an error if it is not present.
func (m *Manager) Get(extensionID string) (*Extension, error) {
	return load(extensionID, filepath.Join(m.cacheDir, extensionID))
}

// load reads an unpacked extension directory into an Extension, including its
// resolved server recipe.
func load(id, dir string) (*Extension, error) {
	man, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	ext := &Extension{
		ID:      id,
		Name:    man.displayName(),
		Version: man.Version,
		Dir:     dir,
	}
	ext.Server = resolveRecipe(ext, man)
	return ext, nil
}

// splitID splits "publisher.name" on its first dot.
func splitID(id string) (namespace, name string, err error) {
	i := strings.IndexByte(id, '.')
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("invalid extension id %q, want publisher.name", id)
	}
	return id[:i], id[i+1:], nil
}

// cachedVersion reads the version marker from an extension cache dir, or "".
func cachedVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, versionMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// extensionRoot is the subdirectory of an unpacked .vsix that holds the actual
// extension files (a .vsix always nests them under extension/).
func extensionRoot(dir string) string {
	return filepath.Join(dir, "extension")
}
