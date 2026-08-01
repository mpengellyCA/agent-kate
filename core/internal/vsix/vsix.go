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
	"errors"
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
	ID         string        `json:"id"`                   // "publisher.name"
	Name       string        `json:"name"`                 // human-readable display name
	Version    string        `json:"version"`              // installed version
	Dir        string        `json:"dir"`                  // unpacked root; contains extension/
	Server     *ServerRecipe `json:"server"`               // nil when no server could be detected
	ServerHint string        `json:"serverHint,omitempty"` // user-facing hint about how to enable a server when Server is nil
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

// ProgressFunc reports download progress as bytes done out of total. total is
// 0 when the server sends no Content-Length, signalling an indeterminate
// download.
type ProgressFunc func(done, total int64)

// Install downloads the latest version of extensionID ("publisher.name") from
// Open VSX, unpacks it and detects its language server. A cached copy of the
// same version is reused. The returned Extension has Server populated when a
// launch recipe was found.
func (m *Manager) Install(ctx context.Context, extensionID string) (*Extension, error) {
	return m.InstallProgress(ctx, extensionID, nil)
}

// InstallProgress behaves like Install but additionally reports download
// progress through onProgress (which may be nil). onProgress is only invoked
// during the network download, not the cache-hit fast path.
func (m *Manager) InstallProgress(ctx context.Context, extensionID string, onProgress ProgressFunc) (*Extension, error) {
	namespace, name, err := splitID(extensionID)
	if err != nil {
		return nil, err
	}
	meta, err := fetchMetadata(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	// Containment before anything destructive: the RemoveAll below wipes this
	// directory, so it must provably be inside the cache.
	dir, err := m.extensionDir(extensionID)
	if err != nil {
		return nil, err
	}

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

	if err := downloadFile(ctx, downloadURL, tmpName, onProgress); err != nil {
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
	dir, err := m.extensionDir(extensionID)
	if err != nil {
		return nil, err
	}
	return load(extensionID, dir)
}

// Remove deletes an installed extension from the cache directory. The id is
// validated with the same splitID guard used on install, and the resolved
// path is confirmed to live under cacheDir, so a crafted id ("../foo") can
// never delete anything outside the cache. Removing an extension that is not
// installed is not an error — the end state is the same.
func (m *Manager) Remove(extensionID string) error {
	if _, _, err := splitID(extensionID); err != nil {
		return err
	}
	// Defence in depth: even after splitID, confirm the resolved directory is
	// genuinely inside cacheDir before any RemoveAll.
	dir, err := m.extensionDir(extensionID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// LatestVersion looks up the newest published version of an installed
// extension on Open VSX. It is best effort: any error (offline, removed from
// the registry) is returned so the caller can simply omit the field.
func (m *Manager) LatestVersion(ctx context.Context, extensionID string) (string, error) {
	namespace, name, err := splitID(extensionID)
	if err != nil {
		return "", err
	}
	meta, err := fetchMetadata(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return meta.Version, nil
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
	if ext.Server == nil {
		ext.ServerHint = serverHint(id)
	}
	return ext, nil
}

// splitID splits "publisher.name" on its first dot, after validating that the
// id is usable as a single path component.
func splitID(id string) (namespace, name string, err error) {
	if err := validateExtensionID(id); err != nil {
		return "", "", err
	}
	i := strings.IndexByte(id, '.')
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("invalid extension id %q, want publisher.name", id)
	}
	return id[:i], id[i+1:], nil
}

// validateExtensionID rejects any id that is not a single, ordinary path
// component. Every cache path in this package is filepath.Join(cacheDir, id),
// and Install RemoveAll's that path before unpacking — so an id containing a
// separator or a ".." element ("a/../../x.y" passes a naive dot check) would
// be an arbitrary-directory delete. Ids come in over the IPC socket, i.e. from
// an agent, so this is the containment boundary for the whole package.
func validateExtensionID(id string) error {
	if id == "" {
		return errors.New("extension id is required")
	}
	if strings.ContainsAny(id, `/\`) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("invalid extension id %q: must be a single name", id)
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid extension id %q", id)
	}
	if filepath.Clean(id) != id {
		return fmt.Errorf("invalid extension id %q", id)
	}
	return nil
}

// extensionDir resolves the cache directory for an extension id, refusing any
// id that does not stay inside cacheDir. Fails closed: an id whose path cannot
// be made absolute is an error, not a fallback to the joined path.
func (m *Manager) extensionDir(extensionID string) (string, error) {
	if err := validateExtensionID(extensionID); err != nil {
		return "", err
	}
	dir := filepath.Join(m.cacheDir, extensionID)
	cacheAbs, err := filepath.Abs(m.cacheDir)
	if err != nil {
		return "", err
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(cacheAbs, dirAbs)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to use %q: resolves outside the extension cache",
			extensionID)
	}
	return dir, nil
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
