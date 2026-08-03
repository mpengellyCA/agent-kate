// Kimi's provider REGISTRY (plan 26 phase 3): a persistent set of LLM
// providers in the engine's own home directory, managed by shelling out to
// `kimi provider …`. Shelling out is not a shortcut — providers/list is NOT
// implemented over ACP (probed live: -32601 with and without a session), so
// the CLI is the only interface there is. Do not retry the tidier design.
//
// Every function takes an explicit home ("" = the user's default), because
// WHICH registry is the whole point: per-thread KIMI_CODE_HOME isolation
// composes with per-home provider sets, and an op that silently edited the
// user's real registry while the caller meant a thread's would be the worst
// bug this file can have (see TestProviderOpsUseTheGivenHome).
package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentkate/internal/agent"
)

// Provider mirrors one entry of `kimi provider list --json`, plus the model
// aliases that reference it. It NEVER carries a credential: the raw config
// does include an apiKey field (0.31.1 — the 0.30-era "has_api_key boolean"
// fact is stale), so parseProviders reads only its EMPTINESS and drops the
// value on the floor before anything else sees it. Nothing in Agent Kate ever
// holds a kimi provider key — kimi's own credential store owns them, which
// the UI states as the advantage it is.
type Provider struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	BaseURL      string `json:"baseUrl"`
	DefaultModel string `json:"defaultModel,omitempty"`
	// HasAPIKey: a credential is CONFIGURED for this provider — an API key,
	// or an OAuth grant (kimi's managed provider). Never the credential.
	HasAPIKey bool     `json:"hasApiKey"`
	Status    string   `json:"status,omitempty"` // the CLI's own status, verbatim
	Models    []string `json:"models"`           // aliases referencing this provider
}

// providerBin is the binary the registry ops run. Package-level (not a
// parameter) because the supervisor's own default is the same "kimi" from
// PATH (run.go constructs it with ""), and the ops must stay callable without
// a supervisor in hand.
const providerBin = "kimi"

// providerCmdTimeout bounds one registry op. Generous, because `catalog` and
// `add` fetch registries over the network; a wedged CLI is still reaped.
const providerCmdTimeout = 60 * time.Second

// providerErrorLimit is deliberately small: provider commands can involve
// remote registries, whose diagnostics are not a transcript store. More
// importantly, an engine diagnostic must never become a route by which a
// provider credential reaches the UI or a log.
const providerErrorLimit = 4096

var (
	// The CLI's config and HTTP failures commonly spell credentials in one of
	// these forms. This is a defence-in-depth scrub of stderr; stdout is never
	// included in an error at all because `provider list --json` contains raw
	// apiKey values on current Kimi versions.
	providerNamedSecretRE = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|auth(?:orization)?|bearer|secret|password|credential)\s*[:=]\s*["']?)[^\s,"'}]+`)
	providerBearerRE      = regexp.MustCompile(`(?i)\b(bearer|basic)\s+\S+`)
	providerURLSecretRE   = regexp.MustCompile(`(?i)https?://[^\s/@]+@[^\s]+|([?&](?:api[_-]?key|access[_-]?token|token|key|secret|password)=)[^&\s]+`)
)

func providerOperation(args []string) string {
	// Args after the verb can be a registry URL or a provider id. Neither
	// belongs in an IPC error: URLs may carry userinfo/query credentials, and
	// every useful operation label is already in the first two words.
	if len(args) >= 2 {
		return providerBin + " " + args[0] + " " + args[1]
	}
	return providerBin + " provider"
}

func safeProviderStderr(raw string) string {
	detail := strings.TrimSpace(raw)
	if detail == "" {
		return ""
	}
	detail = providerNamedSecretRE.ReplaceAllString(detail, "$1[redacted]")
	detail = providerBearerRE.ReplaceAllString(detail, "$1 [redacted]")
	detail = providerURLSecretRE.ReplaceAllStringFunc(detail, func(s string) string {
		if strings.HasPrefix(s, "?") || strings.HasPrefix(s, "&") {
			if i := strings.IndexByte(s, '='); i >= 0 {
				return s[:i+1] + "[redacted]"
			}
		}
		return "[redacted URL]"
	})
	if len(detail) > providerErrorLimit {
		detail = detail[:providerErrorLimit] + "…"
	}
	return detail
}

// runProvider executes one `kimi provider …` subcommand against the given
// home. home == "" runs against the user's default home (the environment is
// inherited untouched); anything else overlays KIMI_CODE_HOME via the same
// ApplyEnvOverlay every thread launch uses.
func runProvider(home string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), providerCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, providerBin, args...)
	if home != "" {
		cmd.Env = agent.ApplyEnvOverlay(os.Environ(),
			map[string]string{"KIMI_CODE_HOME": home})
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Do not fall back to stdout. `provider list --json` puts raw apiKey
		// values there, including when a command later exits unsuccessfully.
		detail := safeProviderStderr(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", providerOperation(args), err, detail)
		}
		return nil, fmt.Errorf("%s failed: %w", providerOperation(args), err)
	}
	return stdout.Bytes(), nil
}

// rawProviderConfig is the shape `kimi provider list --json` emits on 0.31.1:
// the raw providers/models config, camelCase, keyed by id and alias.
type rawProviderConfig struct {
	Providers map[string]struct {
		Type         string          `json:"type"`
		APIKey       string          `json:"apiKey"` // read for emptiness ONLY
		BaseURL      string          `json:"baseUrl"`
		DefaultModel string          `json:"defaultModel"`
		Status       string          `json:"status"`
		OAuth        json.RawMessage `json:"oauth"` // presence = oauth credential
	} `json:"providers"`
	Models map[string]struct {
		Provider string `json:"provider"`
	} `json:"models"`
}

// parseProviders decodes the raw config into the key-free Provider shape.
// Malformed JSON, or JSON without a providers object at all, is an ERROR —
// never a half-parsed (or confidently empty) list the UI would then render as
// "you have no providers". An empty providers object is a real answer: a
// fresh home genuinely has none.
func parseProviders(raw []byte) ([]Provider, error) {
	var cfg rawProviderConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("kimi provider list --json: %w", err)
	}
	if cfg.Providers == nil {
		return nil, fmt.Errorf(
			"kimi provider list --json: the reply carries no providers object " +
				"(a CLI too old or too new for this parser?)")
	}
	byProvider := map[string][]string{}
	for alias, m := range cfg.Models {
		byProvider[m.Provider] = append(byProvider[m.Provider], alias)
	}
	out := make([]Provider, 0, len(cfg.Providers))
	for id, p := range cfg.Providers {
		models := byProvider[id]
		sort.Strings(models)
		if models == nil {
			models = []string{}
		}
		out = append(out, Provider{
			ID:           id,
			Type:         p.Type,
			BaseURL:      p.BaseURL,
			DefaultModel: p.DefaultModel,
			HasAPIKey:    p.APIKey != "" || oauthPresent(p.OAuth),
			Status:       p.Status,
			Models:       models,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// oauthPresent reports whether the raw oauth block names a real grant.
func oauthPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null" && trimmed != "{}"
}

// ListProviders reads the registry: `kimi provider list --json`.
func ListProviders(home string) ([]Provider, error) {
	out, err := runProvider(home, "provider", "list", "--json")
	if err != nil {
		return nil, err
	}
	return parseProviders(out)
}

// AddProvider imports every provider a custom registry (api.json) URL lists:
// `kimi provider add <url>`.
func AddProvider(home, url string) error {
	_, err := runProvider(home, "provider", "add", url)
	return err
}

// ImportCatalog discovers and imports providers from the public models.dev
// catalogue (`kimi provider catalog`), then returns the registry as it now
// stands so the caller repaints from applied truth rather than assumption.
func ImportCatalog(home string) ([]Provider, error) {
	if _, err := runProvider(home, "provider", "catalog"); err != nil {
		return nil, err
	}
	return ListProviders(home)
}

// RemoveProvider removes one provider AND every model alias that referenced
// it (the CLI's own semantics — the confirm dialog must say so; see
// docs/plans/26-engine-services.md).
func RemoveProvider(home, id string) error {
	_, err := runProvider(home, "provider", "remove", id)
	return err
}
