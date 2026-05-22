package vsix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// openVSXAPI is the Open VSX registry REST endpoint. Open VSX is the
// vendor-neutral marketplace; unlike the VS Code Marketplace its API is open.
const openVSXAPI = "https://open-vsx.org/api"

// httpClient is shared across requests. The timeout is generous because a
// .vsix can be tens of megabytes.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

// vsxMetadata is the subset of an Open VSX extension record vsix consumes.
// Files maps a role ("download", "manifest", ...) to a URL.
type vsxMetadata struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Files     map[string]string `json:"files"`
}

// fetchMetadata looks up the latest published version of an extension.
func fetchMetadata(ctx context.Context, namespace, name string) (*vsxMetadata, error) {
	url := fmt.Sprintf("%s/%s/%s", openVSXAPI, namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open-vsx lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("open-vsx: extension %s.%s not found", namespace, name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open-vsx lookup %s: %s", url, resp.Status)
	}

	var meta vsxMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("open-vsx lookup: %w", err)
	}
	if meta.Version == "" {
		return nil, fmt.Errorf("open-vsx: %s.%s has no published version", namespace, name)
	}
	return &meta, nil
}

// downloadFile streams url to the file at dest.
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	return nil
}
