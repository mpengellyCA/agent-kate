package agent

import (
	"strings"
	"testing"
)

// envMap turns a "KEY=value" slice into a map for order-independent assertions.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			m[kv[:eq]] = kv[eq+1:]
		}
	}
	return m
}

func TestBuildEnvDirectIsUnchanged(t *testing.T) {
	base := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=real-claude-key", "HOME=/home/x"}

	// nil provider and empty-BaseURL provider both mean "Claude direct".
	for _, p := range []*Provider{nil, {ID: "claude", BaseURL: ""}} {
		got, err := buildEnv(base, p)
		if err != nil {
			t.Fatalf("direct buildEnv: unexpected error: %v", err)
		}
		if len(got) != len(base) {
			t.Fatalf("direct buildEnv mutated env: got %v", got)
		}
		if envMap(got)["ANTHROPIC_API_KEY"] != "real-claude-key" {
			t.Fatalf("direct buildEnv should leave the inherited key intact")
		}
	}
}

func TestBuildEnvScrubsInheritedAnthropicVars(t *testing.T) {
	// akcore's own environment carries a real Anthropic key and a stale model
	// override. A routed provider must NOT forward either to its base URL.
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=real-claude-key",
		"ANTHROPIC_AUTH_TOKEN=real-claude-key",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"ANTHROPIC_MODEL=claude-opus-4-8",
		"CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-4-5",
	}
	p := &Provider{
		ID:        "fireworks",
		Name:      "Fireworks",
		BaseURL:   "https://api.fireworks.ai/inference",
		AuthToken: "fw_secret",
		Models: map[string]string{
			"main":     "accounts/fireworks/routers/glm-5p2-fast",
			"subagent": "accounts/fireworks/models/minimax-m2p5",
		},
	}
	got, err := buildEnv(base, p)
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	m := envMap(got)

	if m["ANTHROPIC_BASE_URL"] != "https://api.fireworks.ai/inference" {
		t.Errorf("base url = %q", m["ANTHROPIC_BASE_URL"])
	}
	if m["ANTHROPIC_API_KEY"] != "fw_secret" || m["ANTHROPIC_AUTH_TOKEN"] != "fw_secret" {
		t.Errorf("token not injected to both vars: key=%q auth=%q", m["ANTHROPIC_API_KEY"], m["ANTHROPIC_AUTH_TOKEN"])
	}
	if got, want := m["ANTHROPIC_MODEL"], "accounts/fireworks/routers/glm-5p2-fast"; got != want {
		t.Errorf("ANTHROPIC_MODEL = %q, want %q", got, want)
	}
	// subagent feeds both override vars.
	if m["CLAUDE_CODE_SUBAGENT_MODEL"] != "accounts/fireworks/models/minimax-m2p5" ||
		m["ANTHROPIC_SMALL_FAST_MODEL"] != "accounts/fireworks/models/minimax-m2p5" {
		t.Errorf("subagent slot not applied to both vars")
	}
	if m["PATH"] != "/usr/bin" {
		t.Errorf("non-managed var PATH was dropped")
	}
	// The real Claude key must appear nowhere in the result.
	for _, kv := range got {
		if strings.Contains(kv, "real-claude-key") {
			t.Fatalf("inherited Anthropic key leaked to provider env: %q", kv)
		}
	}
}

func TestBuildEnvResolvesTokenFromEnvVar(t *testing.T) {
	base := []string{"PATH=/usr/bin", "FIREWORKS_API_KEY=fw_from_env"}
	p := &Provider{
		ID:      "fireworks",
		BaseURL: "https://api.fireworks.ai/inference",
		EnvVar:  "FIREWORKS_API_KEY",
	}
	got, err := buildEnv(base, p)
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	m := envMap(got)
	if m["ANTHROPIC_API_KEY"] != "fw_from_env" {
		t.Errorf("token from env var not used: %q", m["ANTHROPIC_API_KEY"])
	}
}

func TestBuildEnvExplicitTokenWinsOverEnvVar(t *testing.T) {
	base := []string{"FIREWORKS_API_KEY=fw_from_env"}
	p := &Provider{
		BaseURL:   "https://api.fireworks.ai/inference",
		AuthToken: "fw_explicit",
		EnvVar:    "FIREWORKS_API_KEY",
	}
	got, _ := buildEnv(base, p)
	if envMap(got)["ANTHROPIC_API_KEY"] != "fw_explicit" {
		t.Errorf("explicit AuthToken should win over EnvVar")
	}
}

func TestBuildEnvMissingCredentialErrors(t *testing.T) {
	// Routed provider, no token, env var unset → error, no spawn.
	p := &Provider{ID: "fireworks", BaseURL: "https://api.fireworks.ai/inference", EnvVar: "FIREWORKS_API_KEY"}
	if _, err := buildEnv([]string{"PATH=/usr/bin"}, p); err == nil {
		t.Fatalf("expected an error when no credential resolves")
	}

	// No env var named at all.
	p2 := &Provider{ID: "x", BaseURL: "https://example.com"}
	if _, err := buildEnv(nil, p2); err == nil {
		t.Fatalf("expected an error when no credential supplied")
	}
}

func TestBuildEnvEmptyModelSlotsSkipped(t *testing.T) {
	p := &Provider{
		BaseURL:   "https://api.fireworks.ai/inference",
		AuthToken: "fw_secret",
		Models:    map[string]string{"main": "m", "opus": "   ", "sonnet": ""},
	}
	got, _ := buildEnv(nil, p)
	m := envMap(got)
	if _, ok := m["ANTHROPIC_DEFAULT_OPUS_MODEL"]; ok {
		t.Errorf("blank model slot should not set a var")
	}
	if _, ok := m["ANTHROPIC_DEFAULT_SONNET_MODEL"]; ok {
		t.Errorf("empty model slot should not set a var")
	}
	if m["ANTHROPIC_MODEL"] != "m" {
		t.Errorf("populated slot missing")
	}
}
