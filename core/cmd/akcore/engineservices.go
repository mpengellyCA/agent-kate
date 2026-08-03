// Engine services (plan 26): the RPC surface for questions you ask an ENGINE
// before any thread exists — preflight health, and the kimi provider
// registry. Registered from registerHandlers via registerEngineServiceHandlers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"

	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/kimi"
	"agentkate/internal/session"
)

// healthProbeTimeout bounds each INDIVIDUAL health probe (a --version, a
// doctor run, a model discovery). A hung doctor yields an Unknown check for
// that probe — never an error, never a blank card. A var so tests can shrink
// it without waiting out real seconds.
var healthProbeTimeout = 10 * time.Second

// engineHealthTTL is how long one engine's health verdict is served from
// cache. The New Agent dialog refreshes on every engine-combo change and
// neither doctor is instant, so the window is what keeps the dialog snappy.
const engineHealthTTL = 30 * time.Second

// runHealthProbe runs one best-effort CLI probe under the shared per-check
// timeout. It reports the trimmed combined output, whether the DEADLINE (not
// the CLI) ended it, and the CLI's own error. dir may be empty.
func runHealthProbe(ctx context.Context, dir, bin string, args ...string) (
	out string, timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(raw)), errors.Is(ctx.Err(), context.DeadlineExceeded), err
}

// healthTail keeps a doctor's complaint readable on a card: the LAST lines
// are where both CLIs put the verdict.
func healthTail(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// versionCheck is the shared shape of the "is the binary there at all" probe:
// `<bin> --version`, first line as the version. A missing binary is the one
// unambiguous BAD — the engine cannot start — while a hung one is Unknown.
func versionCheck(ctx context.Context, bin string) (harness.Check, string) {
	out, timedOut, err := runHealthProbe(ctx, "", bin, "--version")
	switch {
	case err == nil:
		version := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		return harness.Check{Name: "binary", State: harness.HealthOK,
			Detail: bin + " " + version}, version
	case timedOut:
		return harness.Check{Name: "binary", State: harness.HealthUnknown,
			Detail: bin + " --version did not answer within " +
				healthProbeTimeout.String()}, ""
	default:
		detail := bin + " did not run: " + err.Error()
		if out != "" {
			detail += " (" + healthTail(out) + ")"
		}
		return harness.Check{Name: "binary", State: harness.HealthBad,
			Detail: detail}, ""
	}
}

// doctorCheck runs one non-interactive doctor invocation. Exit 0 is ok; a
// complaint is WARN (the doctors flag fixable configuration, not always a
// refusal to start); a hang is Unknown, never bad — the whole reason Unknown
// exists as a state.
func doctorCheck(ctx context.Context, name, dir, bin string, args ...string) harness.Check {
	out, timedOut, err := runHealthProbe(ctx, dir, bin, args...)
	switch {
	case err == nil:
		return harness.Check{Name: name, State: harness.HealthOK,
			Detail: healthTail(out)}
	case timedOut:
		return harness.Check{Name: name, State: harness.HealthUnknown,
			Detail: bin + " " + strings.Join(args, " ") +
				" did not answer within " + healthProbeTimeout.String()}
	default:
		detail := healthTail(out)
		if detail == "" {
			detail = err.Error()
		}
		return harness.Check{Name: name, State: harness.HealthWarn, Detail: detail}
	}
}

// projectHealther is the optional side-interface for engines whose doctor
// reads per-project settings (claude: "Reads settings files in the current
// directory without a trust prompt"). Same precedent as modelDiscoverer —
// cheaper than widening Harness for one engine's quirk.
type projectHealther interface {
	HealthIn(ctx context.Context, project string) (harness.Health, error)
}

// providerRegistrar is the optional side-interface for engines that keep a
// persistent provider registry (Capabilities().ProviderRegistry). The
// kimiProvider.* handlers route through it so no backend string compare
// appears outside the adapter.
type providerRegistrar interface {
	ListProviders(home string) ([]kimi.Provider, error)
	AddProvider(home, url string) error
	ImportCatalog(home string) ([]kimi.Provider, error)
	RemoveProvider(home, id string) error
}

// engineHealthCache holds each engine/project's last verdict for engineHealthTTL.
// It lives in the handler layer so the adapters stay
// stateless probes; the TTL plus the UI's Reactive equality guard is what
// keeps the preflight card flicker-free AND cheap.
type engineHealthCache struct {
	mu      sync.Mutex
	entries map[healthCacheKey]healthCacheEntry
}

type healthCacheKey struct {
	engineID string
	project  string
}

type healthCacheEntry struct {
	health harness.Health
	at     time.Time
}

func registerEngineServiceHandlers(d handlerDeps) {
	cache := &engineHealthCache{entries: map[healthCacheKey]healthCacheEntry{}}

	// healthFor answers one engine, serving the cache inside the TTL. An
	// adapter error is folded into an all-Unknown verdict: engine.health is
	// best-effort END TO END, and a blank card teaches the user nothing.
	healthFor := func(h harness.Harness, project string) harness.Health {
		id := h.Capabilities().ID
		key := healthCacheKey{engineID: id, project: project}
		cache.mu.Lock()
		if e, ok := cache.entries[key]; ok && time.Since(e.at) < engineHealthTTL {
			cache.mu.Unlock()
			return e.health
		}
		cache.mu.Unlock()
		var (
			hh  harness.Health
			err error
		)
		if ph, ok := h.(projectHealther); ok && project != "" {
			hh, err = ph.HealthIn(context.Background(), project)
		} else {
			hh, err = h.Health(context.Background())
		}
		if err != nil {
			hh = harness.Health{
				EngineID: id,
				State:    harness.HealthUnknown,
				Checks: []harness.Check{{Name: "health",
					State: harness.HealthUnknown, Detail: err.Error()}},
			}
		}
		if hh.EngineID == "" {
			hh.EngineID = id
		}
		cache.mu.Lock()
		cache.entries[key] = healthCacheEntry{health: hh, at: time.Now()}
		cache.mu.Unlock()
		return hh
	}

	// engine.health — preflight for one engine ({engineId}) or all of them.
	// project points claude's doctor at the right directory; it is part of the
	// cache key, so a verdict based on one project's settings never appears on
	// another project's card during the TTL.
	//
	// UI-only: the checks spawn engine CLIs and the verdict names local
	// configuration state; neither is an agent's business.
	d.srv.Handle("engine.health", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			EngineID string `json:"engineId"`
			Project  string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p) // both optional
		var targets []harness.Harness
		if p.EngineID != "" {
			h, ok := d.harnesses.Get(p.EngineID)
			if !ok {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown engine "+p.EngineID)
			}
			targets = []harness.Harness{h}
		} else {
			targets = d.harnesses.All()
		}
		// Engines probe in parallel: each is already bounded per check, and a
		// slow kimi doctor must not delay a claude verdict.
		out := make([]harness.Health, len(targets))
		var wg sync.WaitGroup
		for i, h := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = healthFor(h, p.Project)
			}()
		}
		wg.Wait()
		return map[string]any{"engines": out}, nil
	})

	// --- the kimi provider registry (plan 26 phase 3) -----------------------
	//
	// Every kimiProvider.* handler mutates (or maps) the USER'S engine
	// configuration, so each is UI-only for the same reason StartSpec.Env is
	// not agent-reachable: a worker's credentials source is not a lever we
	// hand to a model. None of them appears in the MCP tool catalogue either
	// (TestProviderRPCsAreNotAgentReachable).

	// registryEngine finds the (sole, today) engine with a provider registry,
	// through the capability + side-interface — never a backend string compare.
	registryEngine := func() (providerRegistrar, error) {
		for _, h := range d.harnesses.All() {
			if !h.Capabilities().ProviderRegistry {
				continue
			}
			if r, ok := h.(providerRegistrar); ok {
				return r, nil
			}
		}
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"no registered engine keeps a provider registry")
	}

	// homeFor resolves which registry a call edits: the thread's own
	// KIMI_CODE_HOME when it has one, else "" — the user's default home,
	// which IS that thread's registry when no overlay was set.
	homeFor := func(threadID string) (string, error) {
		if threadID == "" {
			return "", nil
		}
		rec, ok := d.sessions.Get(threadID)
		if !ok {
			return "", ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+threadID)
		}
		if rec.Backend != session.BackendKimi {
			return "", ipc.Errorf(ipc.CodeInvalidParams,
				"thread "+threadID+" does not use Kimi Code")
		}
		return session.LaunchEnv(rec.Env)["KIMI_CODE_HOME"], nil
	}

	type providerParams struct {
		ThreadID string `json:"threadId"` // optional: whose home
		URL      string `json:"url"`      // kimiProvider.add
		ID       string `json:"id"`       // kimiProvider.remove
	}
	parse := func(raw json.RawMessage) (providerParams, error) {
		var p providerParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return p, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
			}
		}
		return p, nil
	}

	d.srv.Handle("kimiProvider.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		p, err := parse(raw)
		if err != nil {
			return nil, err
		}
		reg, err := registryEngine()
		if err != nil {
			return nil, err
		}
		home, err := homeFor(p.ThreadID)
		if err != nil {
			return nil, err
		}
		providers, err := reg.ListProviders(home)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"providers": providers, "home": home}, nil
	})

	d.srv.Handle("kimiProvider.add", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		p, err := parse(raw)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.URL) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "url is required")
		}
		reg, err := registryEngine()
		if err != nil {
			return nil, err
		}
		home, err := homeFor(p.ThreadID)
		if err != nil {
			return nil, err
		}
		if err := reg.AddProvider(home, strings.TrimSpace(p.URL)); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		providers, err := reg.ListProviders(home)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"providers": providers, "home": home}, nil
	})

	d.srv.Handle("kimiProvider.catalog", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		p, err := parse(raw)
		if err != nil {
			return nil, err
		}
		reg, err := registryEngine()
		if err != nil {
			return nil, err
		}
		home, err := homeFor(p.ThreadID)
		if err != nil {
			return nil, err
		}
		providers, err := reg.ImportCatalog(home)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"providers": providers, "home": home}, nil
	})

	d.srv.Handle("kimiProvider.remove", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		p, err := parse(raw)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.ID) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "id is required")
		}
		reg, err := registryEngine()
		if err != nil {
			return nil, err
		}
		home, err := homeFor(p.ThreadID)
		if err != nil {
			return nil, err
		}
		if err := reg.RemoveProvider(home, strings.TrimSpace(p.ID)); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		providers, err := reg.ListProviders(home)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"removed": p.ID, "providers": providers, "home": home}, nil
	})
}
