package vsix

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// manifest is the subset of a VS Code extension's package.json that vsix needs.
type manifest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Publisher   string `json:"publisher"`
	Version     string `json:"version"`
	Main        string `json:"main"`
	Contributes struct {
		Languages []struct {
			ID         string   `json:"id"`
			Extensions []string `json:"extensions"`
			Aliases    []string `json:"aliases"`
		} `json:"languages"`
	} `json:"contributes"`
}

// displayName is the human-readable name, falling back to the package name.
func (m *manifest) displayName() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}

// languages returns the LSP language ids and file extensions the manifest
// declares, each de-duplicated and order-preserving.
func (m *manifest) languages() (ids, exts []string) {
	seenID := map[string]bool{}
	seenExt := map[string]bool{}
	for _, l := range m.Contributes.Languages {
		if l.ID != "" && !seenID[l.ID] {
			seenID[l.ID] = true
			ids = append(ids, l.ID)
		}
		for _, e := range l.Extensions {
			if e != "" && !seenExt[e] {
				seenExt[e] = true
				exts = append(exts, e)
			}
		}
	}
	return ids, exts
}

// readManifest parses extension/package.json from an unpacked extension dir.
func readManifest(dir string) (*manifest, error) {
	b, err := os.ReadFile(filepath.Join(extensionRoot(dir), "package.json"))
	if err != nil {
		return nil, fmt.Errorf("extension manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("extension manifest: %w", err)
	}
	return &m, nil
}
