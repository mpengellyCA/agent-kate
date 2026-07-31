package agent

import (
	"strings"
	"testing"
)

// TestApplyEnvOverlay pins the merge rule both supervisors share (plan 16 P6):
// an overlay REPLACES a variable the base already has, adds the rest, and
// leaves everything else alone. A silent duplicate would be worse than either
// — the child would see whichever the libc happened to pick.
func TestApplyEnvOverlay(t *testing.T) {
	base := []string{"PATH=/bin", "KIMI_CODE_HOME=/home/u/.kimi-code", "HOME=/home/u"}

	got := ApplyEnvOverlay(base, map[string]string{
		"KIMI_CODE_HOME": "/wt/.agentkate/kimi-home",
		"AK_THREAD":      "t-1",
	})

	count := map[string]int{}
	for _, kv := range got {
		count[strings.SplitN(kv, "=", 2)[0]]++
	}
	for _, key := range []string{"PATH", "HOME", "KIMI_CODE_HOME", "AK_THREAD"} {
		if count[key] != 1 {
			t.Errorf("%s appears %d times, want exactly 1: %v", key, count[key], got)
		}
	}
	if !contains(got, "KIMI_CODE_HOME=/wt/.agentkate/kimi-home") {
		t.Errorf("overlay did not replace the inherited value: %v", got)
	}
	if contains(got, "KIMI_CODE_HOME=/home/u/.kimi-code") {
		t.Errorf("the inherited value survived the overlay: %v", got)
	}
	if !contains(got, "PATH=/bin") || !contains(got, "HOME=/home/u") {
		t.Errorf("overlay disturbed unrelated variables: %v", got)
	}
}

func TestApplyEnvOverlayEmptyIsIdentity(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/home/u"}
	if got := ApplyEnvOverlay(base, nil); len(got) != len(base) {
		t.Errorf("nil overlay changed the environment: %v", got)
	}
	if got := ApplyEnvOverlay(base, map[string]string{}); len(got) != len(base) {
		t.Errorf("empty overlay changed the environment: %v", got)
	}
}

// TestApplyEnvOverlayRunsAfterProviderRouting: the overlay is applied to the
// provider-built environment, so one rule holds — what the caller asked for is
// what the child gets — rather than the answer depending on ordering.
func TestApplyEnvOverlayRunsAfterProviderRouting(t *testing.T) {
	routed, err := buildEnv([]string{"PATH=/bin"}, &Provider{
		ID: "p", BaseURL: "https://example.invalid", AuthToken: "secret",
	})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	got := ApplyEnvOverlay(routed, map[string]string{"ANTHROPIC_BASE_URL": "https://other.invalid"})
	if !contains(got, "ANTHROPIC_BASE_URL=https://other.invalid") {
		t.Errorf("overlay did not win over provider routing: %v", got)
	}
	seen := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("ANTHROPIC_BASE_URL set %d times", seen)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
