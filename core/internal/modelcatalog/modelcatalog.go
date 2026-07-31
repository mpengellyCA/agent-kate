// Package modelcatalog fetches a provider's live model list over HTTP, for
// routed (third-party, Anthropic-compatible) providers where the Claude CLI's
// `/model` alias vocabulary does not apply. It normalizes both the Anthropic
// Models API shape (`{data:[{id, display_name}], has_more, last_id}`) and the
// OpenAI-compatible shape (`{data:[{id}]}`) to a flat list.
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Model is one entry of a provider's catalog. ID is the concrete model id sent
// to the CLI; Name is a display label (the API's display_name when present,
// else the id).
type Model struct {
	ID   string
	Name string
}

// maxPages bounds pagination so a misbehaving endpoint can't loop forever.
const maxPages = 20

// modelsURL derives the models endpoint from an Anthropic-compatible base URL.
// Bases may or may not already include the "/v1" segment.
func modelsURL(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/models"
	}
	return b + "/v1/models"
}

// Fetch returns the provider's models, trying the Anthropic shape first
// (x-api-key + anthropic-version, paginated via after_id) and falling back to
// the OpenAI shape (Authorization: Bearer) if the first attempt yields nothing.
// The context bounds the whole operation; callers pass a short timeout.
func Fetch(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("modelcatalog: empty base url")
	}
	url := modelsURL(baseURL)
	if models, err := fetchAnthropic(ctx, url, apiKey); err == nil && len(models) > 0 {
		return models, nil
	}
	return fetchOpenAI(ctx, url, apiKey)
}

type apiModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type apiPage struct {
	Data    []apiModel `json:"data"`
	HasMore bool       `json:"has_more"`
	LastID  string     `json:"last_id"`
}

func fetchAnthropic(ctx context.Context, url, apiKey string) ([]Model, error) {
	var out []Model
	after := ""
	for page := 0; page < maxPages; page++ {
		u := url + "?limit=1000"
		if after != "" {
			u += "&after_id=" + after
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
		p, err := doPage(req)
		if err != nil {
			return nil, err
		}
		for _, m := range p.Data {
			out = append(out, toModel(m))
		}
		if !p.HasMore || p.LastID == "" {
			break
		}
		after = p.LastID
	}
	return out, nil
}

func fetchOpenAI(ctx context.Context, url, apiKey string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	p, err := doPage(req)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(p.Data))
	for _, m := range p.Data {
		out = append(out, toModel(m))
	}
	return out, nil
}

func doPage(req *http.Request) (apiPage, error) {
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return apiPage{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return apiPage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiPage{}, fmt.Errorf("modelcatalog: %s -> %d", req.URL.Host, resp.StatusCode)
	}
	var p apiPage
	if err := json.Unmarshal(body, &p); err != nil {
		return apiPage{}, err
	}
	return p, nil
}

func toModel(m apiModel) Model {
	name := strings.TrimSpace(m.DisplayName)
	if name == "" {
		name = m.ID
	}
	return Model{ID: m.ID, Name: name}
}
