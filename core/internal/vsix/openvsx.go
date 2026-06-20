package vsix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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

// downloadFile streams url to the file at dest. When onProgress is non-nil it
// is called as bytes arrive with (bytesDone, total); total is 0 if the server
// sends no Content-Length.
func downloadFile(ctx context.Context, url, dest string, onProgress ProgressFunc) error {
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

	var src io.Reader = resp.Body
	if onProgress != nil {
		total := resp.ContentLength // -1 when unknown
		if total < 0 {
			total = 0
		}
		onProgress(0, total)
		src = &progressReader{r: resp.Body, total: total, onProgress: onProgress}
	}
	if _, err := io.Copy(f, src); err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	return nil
}

// progressReader wraps a reader and reports cumulative bytes read.
type progressReader struct {
	r          io.Reader
	done       int64
	total      int64
	onProgress ProgressFunc
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		p.onProgress(p.done, p.total)
	}
	return n, err
}

// searchEntry is the subset of an Open VSX search hit vsix consumes.
type searchEntry struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// Search queries the Open VSX registry for extensions matching query and maps
// the hits to CatalogEntry values. It is network-dependent; callers should
// treat any error as "no results" rather than fatal.
func (m *Manager) Search(ctx context.Context, query string) ([]CatalogEntry, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	u := fmt.Sprintf("%s/-/search?query=%s&size=25", openVSXAPI, url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open-vsx search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open-vsx search %s: %s", u, resp.Status)
	}
	var body struct {
		Extensions []searchEntry `json:"extensions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("open-vsx search: %w", err)
	}
	out := make([]CatalogEntry, 0, len(body.Extensions))
	for _, e := range body.Extensions {
		if e.Namespace == "" || e.Name == "" {
			continue
		}
		display := e.DisplayName
		if display == "" {
			display = e.Name
		}
		out = append(out, CatalogEntry{
			ID:          e.Namespace + "." + e.Name,
			DisplayName: display,
			Summary:     e.Description,
			Category:    "Open VSX",
		})
	}
	return out, nil
}
