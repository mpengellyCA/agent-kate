package agent

import (
	"fmt"
	"strings"
)

// Provider routes an agent thread's `claude` process at a third-party,
// Anthropic-compatible API endpoint (Fireworks / Fire Pass, OpenRouter, …)
// instead of Anthropic's own. Routing is pure environment injection: Claude Code
// honours ANTHROPIC_BASE_URL, the API key, and a set of model-slot overrides, so
// buildEnv translates a Provider into exactly those variables for one child.
//
// An empty BaseURL is the sentinel for "Claude (direct)": buildEnv injects
// nothing and the child inherits akcore's environment unchanged, so default
// agents are byte-for-byte unaffected.
//
// AuthToken is the resolved secret. It is supplied per launch over the trusted
// local IPC socket and is NEVER persisted to disk (session.Record carries only
// the non-secret snapshot) nor written to logs. When AuthToken is empty and
// EnvVar names a variable, buildEnv resolves the token from that variable in the
// child's base environment — the headless / "keys live in my shell" path.
type Provider struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	BaseURL   string            `json:"baseUrl"`
	AuthToken string            `json:"authToken,omitempty"`
	EnvVar    string            `json:"envVar,omitempty"`
	Models    map[string]string `json:"models,omitempty"`
}

// Routed reports whether p actually routes anywhere (a non-empty BaseURL). A nil
// Provider, or one with an empty BaseURL, means Claude direct.
func (p *Provider) Routed() bool { return p != nil && strings.TrimSpace(p.BaseURL) != "" }

// label is a human-friendly identifier for error messages (never the token).
func (p *Provider) label() string {
	switch {
	case p.Name != "":
		return p.Name
	case p.ID != "":
		return p.ID
	default:
		return p.BaseURL
	}
}

// managedEnvKeys are every Anthropic/Claude-Code variable buildEnv owns. When a
// provider is active these are stripped from the inherited environment before the
// provider's are appended — so a real Anthropic key sitting in akcore's
// environment can never be forwarded to a third-party base URL, and no stale
// override (a previous provider, the user's own ANTHROPIC_MODEL) leaks through.
var managedEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// modelSlotEnv maps a Provider.Models slot key to the Claude Code override
// variable(s) it sets. "subagent" feeds both the subagent model and the
// small/fast model so helper turns also land on the provider.
var modelSlotEnv = map[string][]string{
	"main":     {"ANTHROPIC_MODEL"},
	"opus":     {"ANTHROPIC_DEFAULT_OPUS_MODEL"},
	"sonnet":   {"ANTHROPIC_DEFAULT_SONNET_MODEL"},
	"haiku":    {"ANTHROPIC_DEFAULT_HAIKU_MODEL"},
	"subagent": {"CLAUDE_CODE_SUBAGENT_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"},
}

// buildEnv computes the environment for one `claude` child from base (normally
// os.Environ()) and the selected provider. For Claude direct (nil/empty BaseURL)
// it returns base unchanged. For a routed provider it strips the managed keys
// from base and appends ANTHROPIC_BASE_URL, the API key (as both ANTHROPIC_API_KEY
// and its ANTHROPIC_AUTH_TOKEN alias), and one variable per populated model slot.
//
// It returns an error rather than spawning an unauthenticated request when a
// routed provider has no resolvable credential.
func buildEnv(base []string, p *Provider) ([]string, error) {
	if !p.Routed() {
		return base, nil
	}

	token := p.AuthToken
	if token == "" && p.EnvVar != "" {
		token = lookupEnv(base, p.EnvVar)
	}
	if token == "" {
		if p.EnvVar != "" {
			return nil, fmt.Errorf("provider %q: no API credential (%s is unset and no key was supplied)", p.label(), p.EnvVar)
		}
		return nil, fmt.Errorf("provider %q: no API credential supplied", p.label())
	}

	out := make([]string, 0, len(base)+len(managedEnvKeys)+2)
	for _, kv := range base {
		if !isManagedEnv(kv) {
			out = append(out, kv)
		}
	}
	out = append(out,
		"ANTHROPIC_BASE_URL="+strings.TrimSpace(p.BaseURL),
		"ANTHROPIC_API_KEY="+token,
		"ANTHROPIC_AUTH_TOKEN="+token,
	)
	for slot, model := range p.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		for _, key := range modelSlotEnv[slot] {
			out = append(out, key+"="+model)
		}
	}
	return out, nil
}

// isManagedEnv reports whether the "KEY=value" entry names a managed key.
func isManagedEnv(kv string) bool {
	eq := strings.IndexByte(kv, '=')
	if eq < 0 {
		return false
	}
	key := kv[:eq]
	for _, m := range managedEnvKeys {
		if key == m {
			return true
		}
	}
	return false
}

// lookupEnv returns the value of name from a "KEY=value" slice, or "".
func lookupEnv(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}
