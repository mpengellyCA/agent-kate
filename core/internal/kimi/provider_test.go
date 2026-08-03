package kimi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listFixture is a captured `kimi provider list --json` (0.31.1 shape: the
// managed oauth provider verbatim from a live box) plus one api-key provider
// of the shape `provider add` writes. The apiKey VALUE is deliberately
// present and secret-looking: the parser must read only its emptiness.
const listFixture = `{
  "providers": {
    "managed:kimi-code": {
      "type": "kimi",
      "apiKey": "",
      "baseUrl": "https://api.kimi.com/coding/v1",
      "oauth": {"storage": "file", "key": "oauth/kimi-code"}
    },
    "openrouter": {
      "type": "openai",
      "apiKey": "sk-or-SECRET-NEVER-SURFACED",
      "baseUrl": "https://openrouter.ai/api/v1",
      "defaultModel": "openrouter/auto",
      "status": "ok"
    },
    "bare": {
      "type": "openai",
      "apiKey": "",
      "baseUrl": "https://example.test/v1"
    }
  },
  "models": {
    "kimi-code/k3": {"provider": "managed:kimi-code", "model": "k3"},
    "kimi-code/kimi-for-coding": {"provider": "managed:kimi-code", "model": "kimi-for-coding"},
    "openrouter/auto": {"provider": "openrouter", "model": "auto"}
  }
}`

func TestListProvidersParsesCLIJSON(t *testing.T) {
	got, err := parseProviders([]byte(listFixture))
	if err != nil {
		t.Fatalf("parseProviders: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d providers, want 3: %+v", len(got), got)
	}
	byID := map[string]Provider{}
	for _, p := range got {
		byID[p.ID] = p
	}
	managed := byID["managed:kimi-code"]
	if !managed.HasAPIKey {
		t.Errorf("the oauth provider must report a configured credential")
	}
	if managed.BaseURL != "https://api.kimi.com/coding/v1" || managed.Type != "kimi" {
		t.Errorf("managed provider mis-parsed: %+v", managed)
	}
	if want := []string{"kimi-code/k3", "kimi-code/kimi-for-coding"}; len(managed.Models) != 2 ||
		managed.Models[0] != want[0] || managed.Models[1] != want[1] {
		t.Errorf("managed models = %v, want %v (sorted aliases)", managed.Models, want)
	}
	or := byID["openrouter"]
	if !or.HasAPIKey || or.Status != "ok" || or.DefaultModel != "openrouter/auto" {
		t.Errorf("api-key provider mis-parsed: %+v", or)
	}
	if bare := byID["bare"]; bare.HasAPIKey {
		t.Errorf("a provider with an empty apiKey and no oauth must report no credential")
	}

	// THE key invariant: the credential value from the raw config must not
	// survive into anything a caller can see — under any field, any casing.
	reserialised, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(reserialised), "SECRET-NEVER-SURFACED") {
		t.Fatalf("the parsed provider list carries the raw apiKey value:\n%s", reserialised)
	}

	// Malformed JSON is an error, never a half-parsed list.
	if _, err := parseProviders([]byte(listFixture[:120])); err == nil {
		t.Fatalf("a truncated reply parsed without error")
	}
	// Valid JSON of the wrong shape is an error too — a confidently empty
	// list would tell the user their registry is empty when the parser simply
	// did not understand the reply.
	if _, err := parseProviders([]byte(`{"unexpected": true}`)); err == nil {
		t.Fatalf("a reply with no providers object parsed without error")
	}
	// ...but a genuinely empty registry (a fresh home) is a real, non-error
	// answer.
	empty, err := parseProviders([]byte(`{"providers": {}, "models": {}}`))
	if err != nil {
		t.Fatalf("an empty registry must parse: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("an empty registry parsed to %v", empty)
	}
}

// stubKimi installs a fake `kimi` first on PATH that logs KIMI_CODE_HOME and
// its argv to logPath, answering `provider list --json` with an empty
// registry so ListProviders round-trips.
func stubKimi(t *testing.T, logPath string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s|%s\n' "${KIMI_CODE_HOME-UNSET}" "$*" >> "` + logPath + `"
case "$*" in
  *"list --json"*) echo '{"providers":{},"models":{}}';;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kimi"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestProviderOpsUseTheGivenHome is the guard against the worst bug this file
// can have: an op that silently edits the USER'S real registry when the
// caller named a thread's home. Every op is asserted twice — a named home
// reaches the child as KIMI_CODE_HOME, and the empty home leaves the
// inherited environment untouched (it does NOT force the variable empty,
// which would un-home a child whose parent legitimately set it).
func TestProviderOpsUseTheGivenHome(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	stubKimi(t, logPath)

	const home = "/custom/kimi-home"
	if _, err := ListProviders(home); err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if err := AddProvider(home, "https://example.test/api.json"); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if _, err := ImportCatalog(home); err != nil {
		t.Fatalf("ImportCatalog: %v", err)
	}
	if err := RemoveProvider(home, "some-id"); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// list, add, catalog+list (ImportCatalog re-reads), remove.
	if len(lines) != 5 {
		t.Fatalf("stub saw %d calls, want 5:\n%s", len(lines), raw)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, home+"|") {
			t.Errorf("call %d ran without the requested home: %q", i, line)
		}
	}
	wantArgs := []string{
		"provider list --json",
		"provider add https://example.test/api.json",
		"provider catalog",
		"provider list --json",
		"provider remove some-id",
	}
	for i, want := range wantArgs {
		if got := strings.SplitN(lines[i], "|", 2)[1]; got != want {
			t.Errorf("call %d argv = %q, want %q", i, got, want)
		}
	}

	// The empty home is "the user's default": the inherited environment is
	// passed through, whatever it says.
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("reset log: %v", err)
	}
	t.Setenv("KIMI_CODE_HOME", "/inherited/home")
	if _, err := ListProviders(""); err != nil {
		t.Fatalf("ListProviders(\"\"): %v", err)
	}
	raw, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); !strings.HasPrefix(got, "/inherited/home|") {
		t.Errorf("the default-home op clobbered the inherited environment: %q", got)
	}
}

// TestProviderErrorsNeverExposeRegistrySecrets pins the other half of the
// key-free registry contract. A failed `provider list` may still have emitted
// raw JSON on stdout, and a failed add may echo its credential-bearing URL on
// stderr; neither can be allowed to travel through an RPC error to the UI.
func TestProviderErrorsNeverExposeRegistrySecrets(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
echo '{"providers":{"x":{"apiKey":"STDOUT-SECRET"}}}'
echo 'apiKey=STDERR-SECRET bearer BEARER-SECRET https://URL-SECRET@example.test/api.json?token=QUERY-SECRET' >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "kimi"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const registryURL = "https://CALLER-SECRET@example.test/api.json?token=CALLER-QUERY"
	err := AddProvider("", registryURL)
	if err == nil {
		t.Fatal("AddProvider succeeded despite the failing CLI")
	}
	got := err.Error()
	for _, secret := range []string{
		"STDOUT-SECRET", "STDERR-SECRET", "BEARER-SECRET", "URL-SECRET",
		"QUERY-SECRET", "CALLER-SECRET", "CALLER-QUERY", registryURL,
	} {
		if strings.Contains(got, secret) {
			t.Errorf("provider error leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "kimi provider add failed") ||
		!strings.Contains(got, "[redacted]") {
		t.Errorf("provider error lost its safe operation/diagnostic shape: %s", got)
	}
}

func TestProviderStderrIsBounded(t *testing.T) {
	got := safeProviderStderr(strings.Repeat("x", providerErrorLimit+100))
	if len(got) > providerErrorLimit+len("…") {
		t.Fatalf("safe stderr length = %d, want at most %d", len(got), providerErrorLimit+len("…"))
	}
}

// TestTerminalAuthRemedyExtracted pins the plan-26 fix for the discarded
// initialize result: the _meta.terminal-auth command is decoded VERBATIM (it
// is the one actionable remedy the engine offers), and an initialize without
// one yields no remedy at all — never an invented "kimi login".
func TestTerminalAuthRemedyExtracted(t *testing.T) {
	// Captured live from kimi 0.31.1 (trimmed to the relevant branch).
	initRaw := []byte(`{
	  "protocolVersion": 1,
	  "authMethods": [{
	    "id": "login", "type": "terminal", "name": "Login with Kimi account",
	    "args": ["--login"],
	    "_meta": {"terminal-auth": {"type": "terminal",
	      "label": "Login with Kimi account",
	      "command": "kimi", "args": ["login"], "env": {}}}
	  }],
	  "agentInfo": {"name": "Kimi Code CLI", "version": "0.31.1"}
	}`)
	if got := terminalAuthRemedy(initRaw); got != "kimi login" {
		t.Fatalf("remedy = %q, want the engine's own words %q", got, "kimi login")
	}
	if got := terminalAuthRemedy([]byte(`{"authMethods": []}`)); got != "" {
		t.Fatalf("an initialize advertising no terminal auth yielded remedy %q — remedies are never invented", got)
	}
	if got := terminalAuthRemedy([]byte(`not json`)); got != "" {
		t.Fatalf("garbage initialize yielded remedy %q", got)
	}
}
