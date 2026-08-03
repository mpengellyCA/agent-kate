// Engine services (plan 26): the health cache, the timeout discipline, and
// the provider RPCs' reachability boundary.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/kimi"
	"agentkate/internal/session"
)

// Health keeps every test fake implementing the widened Harness interface
// (they all embed *fakeHarness): a fake engine honestly answers Unknown.
func (f *fakeHarness) Health(context.Context) (harness.Health, error) {
	return harness.Health{EngineID: "fake", State: harness.HealthUnknown}, nil
}

// healthCountFake counts how many times its Health actually RUNS — the
// observable the cache test turns on.
type healthCountFake struct {
	*fakeHarness
	calls atomic.Int32
}

func (h *healthCountFake) Health(context.Context) (harness.Health, error) {
	h.calls.Add(1)
	return harness.Health{
		EngineID: "fake",
		State:    harness.HealthWarn,
		Version:  "9.9.9",
		Checks: []harness.Check{
			{Name: "binary", State: harness.HealthOK, Detail: "fake 9.9.9"},
			{Name: "config", State: harness.HealthWarn, Detail: "something is off"},
		},
	}, nil
}

// engineServiceCore is the minimal bus for this file: the engine-service
// handlers over one registered harness, plus a test-only door for taking the
// UI role (requireUIWindow refuses everything else).
func engineServiceCore(t *testing.T, h harness.Harness) *ipc.Client {
	return engineServiceCoreWithSessions(t, h, testSessions(t))
}

func engineServiceCoreWithSessions(t *testing.T, h harness.Harness, sessions *session.Store) *ipc.Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "engine.sock")
	srv := ipc.NewServer(sock, log)
	harnesses := harness.NewRegistry(h.Descriptor().ID)
	harnesses.Register(h)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	registerEngineServiceHandlers(handlerDeps{
		srv: srv, harnesses: harnesses, sessions: sessions,
		gitCache: gitCache, log: log,
	})
	srv.Handle("test.markUI", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !srv.MarkUI(ctx) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest, "UI role refused")
		}
		return map[string]any{"ok": true}, nil
	})
	serveIPC(t, srv, sock)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.CallTimeout("test.markUI", nil, nil, 5*time.Second); err != nil {
		t.Fatalf("markUI: %v", err)
	}
	return client
}

// projectHealthFake makes the cache key observable: its doctor-equivalent
// health depends on the project it was asked to inspect.
type projectHealthFake struct {
	*fakeHarness
	projectA atomic.Int32
	projectB atomic.Int32
}

func (h *projectHealthFake) HealthIn(_ context.Context, project string) (harness.Health, error) {
	switch project {
	case "/project-a":
		h.projectA.Add(1)
	case "/project-b":
		h.projectB.Add(1)
	}
	return harness.Health{EngineID: "fake", State: harness.HealthOK,
		Checks: []harness.Check{{Name: "doctor", State: harness.HealthOK, Detail: project}}}, nil
}

// registryHarness exposes the optional providerRegistrar seam without spawning
// a real Kimi process, and counts registry reads so invalid thread ids cannot
// silently fall through to the user's default home.
type registryHarness struct {
	*fakeHarness
	listCalls atomic.Int32
}

func (h *registryHarness) Descriptor() harness.HarnessDescriptor {
	return harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: session.BackendKimi,
		DisplayName: "Kimi Code", Health: harness.HealthOK,
		Operations: harness.Operations(harness.OperationProviderRegistry)}
}
func (h *registryHarness) ListProviders(string) ([]kimi.Provider, error) {
	h.listCalls.Add(1)
	return []kimi.Provider{}, nil
}
func (h *registryHarness) AddProvider(string, string) error { return nil }
func (h *registryHarness) ImportCatalog(string) ([]kimi.Provider, error) {
	return []kimi.Provider{}, nil
}
func (h *registryHarness) RemoveProvider(string, string) error { return nil }

// TestHealthIsCached pins the 30 s handler-layer cache: two engine.health
// calls inside the window run the adapter's probes ONCE and answer
// identically. Inverted (a cache that never hits, or one that re-probes per
// call), the call counter reads 2 and this fails on the first assertion.
func TestHealthIsCached(t *testing.T) {
	fake := &healthCountFake{fakeHarness: &fakeHarness{}}
	client := engineServiceCore(t, fake)

	call := func() string {
		var raw json.RawMessage
		if err := client.CallTimeout("engine.health",
			map[string]any{"engineId": "fake"}, &raw, 10*time.Second); err != nil {
			t.Fatalf("engine.health: %v", err)
		}
		return string(raw)
	}
	first := call()
	second := call()
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("two engine.health calls inside the TTL ran the probes %d times, want 1", got)
	}
	if first != second {
		t.Fatalf("cached reply differs from the first:\n  first:  %s\n  second: %s", first, second)
	}
	// The verdict itself survives the wire: worst-of-checks (ok + warn) is warn.
	var res struct {
		Engines []harness.Health `json:"engines"`
	}
	if err := json.Unmarshal([]byte(first), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Engines) != 1 || res.Engines[0].State != harness.HealthWarn {
		t.Fatalf("engines = %+v, want one warn verdict", res.Engines)
	}
}

// TestHealthCacheKeepsProjectsSeparate prevents a `claude doctor` verdict
// based on project A's settings from appearing in project B's New Agent card.
func TestHealthCacheKeepsProjectsSeparate(t *testing.T) {
	fake := &projectHealthFake{fakeHarness: &fakeHarness{}}
	client := engineServiceCore(t, fake)
	call := func(project string) {
		t.Helper()
		if err := client.CallTimeout("engine.health", map[string]any{
			"engineId": "fake", "project": project,
		}, nil, 5*time.Second); err != nil {
			t.Fatalf("engine.health(%s): %v", project, err)
		}
	}
	call("/project-a")
	call("/project-a") // cache hit
	call("/project-b") // distinct doctor settings, must not reuse A
	if got := fake.projectA.Load(); got != 1 {
		t.Errorf("project A was probed %d times, want one cached probe", got)
	}
	if got := fake.projectB.Load(); got != 1 {
		t.Errorf("project B was probed %d times, want its own probe", got)
	}
}

// TestProviderThreadHomeFailsClosed makes the "specific thread's registry"
// selector safe: an unknown id or a thread without the descriptor's
// provider-registry operation is an invalid request, not an empty home that
// would edit the user's default registry.
func TestProviderThreadHomeFailsClosed(t *testing.T) {
	sessions := testSessions(t)
	if err := sessions.Put(session.Record{ThreadID: "claude-thread", Backend: session.BackendClaude}); err != nil {
		t.Fatalf("put claude record: %v", err)
	}
	h := &registryHarness{fakeHarness: &fakeHarness{}}
	client := engineServiceCoreWithSessions(t, h, sessions)
	for _, tc := range []struct {
		threadID string
		want     string
	}{
		{"missing-thread", "unknown thread missing-thread"},
		{"claude-thread", "does not use a provider registry"},
	} {
		err := client.CallTimeout("kimiProvider.list", map[string]any{
			"threadId": tc.threadID,
		}, nil, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("kimiProvider.list(%q) error = %v, want %q", tc.threadID, err, tc.want)
		}
	}
	if got := h.listCalls.Load(); got != 0 {
		t.Errorf("registry ListProviders ran %d times for rejected thread ids", got)
	}
}

// TestHealthUnknownOnTimeout drives the claude adapter against a stub CLI
// whose doctor HANGS: the doctor check must come back Unknown — not bad, not
// an error, not a blank card — and the version check (which the stub answers
// promptly) must still be ok. This is the per-check timeout discipline; a
// probe that let one hung subprocess fail the whole verdict would fail here.
func TestHealthUnknownOnTimeout(t *testing.T) {
	dir := t.TempDir()
	// The stub sleeps far past the (shrunken) per-check timeout on `doctor`,
	// answers `--version` instantly, and gives model discovery nothing.
	script := `#!/bin/sh
case "$1" in
  --version) echo "9.9.9 (stub)"; exit 0;;
  doctor) sleep 10; exit 0;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := healthProbeTimeout
	healthProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { healthProbeTimeout = old })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newClaudeHarness(agent.NewSupervisor("", log,
		func(string, []json.RawMessage) {}), "", "")
	got, err := h.Health(context.Background())
	if err != nil {
		t.Fatalf("Health returned an error for a hung doctor; the contract is "+
			"best-effort with Unknown checks: %v", err)
	}
	byName := map[string]harness.Check{}
	for _, c := range got.Checks {
		byName[c.Name] = c
	}
	if c := byName["doctor"]; c.State != harness.HealthUnknown {
		t.Errorf("a hung doctor yielded state %q (%s), want %q",
			c.State, c.Detail, harness.HealthUnknown)
	}
	if c := byName["binary"]; c.State != harness.HealthOK {
		t.Errorf("the responsive --version probe yielded %q (%s), want ok",
			c.State, c.Detail)
	}
	if got.Version != "9.9.9 (stub)" {
		t.Errorf("version = %q, want the stub's own line", got.Version)
	}
	// Roll-up: ok + unknown + unknown is unknown — never bad on a timeout.
	if got.State != harness.HealthUnknown {
		t.Errorf("overall state = %q, want %q (a timeout has not said \"bad\")",
			got.State, harness.HealthUnknown)
	}
}

// TestProviderRPCsAreNotAgentReachable guards the plan-26 boundary the same
// way StartSpec.Env is guarded: a worker's credentials source is not a lever
// we hand to a model. Two halves — the MCP tool catalogues advertise no
// provider tool at all, and the registered RPCs (plus engine.health) refuse a
// real, authenticated agent bridge with the UI-only sentence.
func TestProviderRPCsAreNotAgentReachable(t *testing.T) {
	for _, defs := range map[string][]map[string]any{
		"cooperation": toolDefs(),
		"cowork":      coworkToolDefs(),
	} {
		for _, def := range defs {
			name, _ := def["name"].(string)
			if strings.Contains(strings.ToLower(name), "provider") {
				t.Errorf("the MCP tool catalogue advertises %q — the provider "+
					"registry must not be agent-reachable", name)
			}
		}
	}

	sock, secrets, _ := inventoryCore(t, []session.Record{{ThreadID: probeSelf}})
	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	asBridge(t, secrets, bridge, probeSelf)
	for _, method := range []string{
		"engine.health",
		"kimiProvider.list", "kimiProvider.add",
		"kimiProvider.catalog", "kimiProvider.remove",
	} {
		err := bridge.CallTimeout(method, map[string]any{}, nil, 10*time.Second)
		if err == nil || !strings.Contains(err.Error(), uiOnlyRefusal) {
			t.Errorf("%s answered an agent bridge (err = %v); want the UI-only refusal",
				method, err)
		}
	}
}
