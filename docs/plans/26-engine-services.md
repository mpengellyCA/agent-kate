# 26 — Engine services: the Kimi provider registry, and preflight health for both engines

**Status: IN PROGRESS.** Core health and provider-registry plumbing plus the
initial UI are implemented and security-gated; terminal remedy execution,
roster health surfacing, and dedicated UI coverage remain. Covers IDEAS #9 (Kimi-native provider routing via
`kimi provider`) and #11 (engine preflight health check built on `kimi doctor`).
Program context: [20-approved-features-program.md](20-approved-features-program.md).

**Size: M.** No spike needed — both mechanisms were probed live this session.

> **User note on #9:** *"Lets use this as an option while still having first
> class support for using claude code with other providers."*

These two features are grouped because they are the same *shape*: things you ask
an **engine** before you have a thread. Every existing harness method takes a
`threadID`; both of these take none, and both surface in the same place — the
New Agent dialog's engine picker (`ui/src/NewAgentDialog.cpp:72-90`).

## Why

**Provider routing today is Claude-shaped, and that is correct but incomplete.**
Plan 11 shipped third-party routing by env injection: `agent.buildEnv`
(`core/internal/agent/provider.go:115`) scrubs inherited Anthropic credentials
and points a thread at an Anthropic-compatible endpoint, with profiles in
`ui/src/ProviderConfig.h` and keys in KWallet. `Capabilities.ProviderRouting` is
true for claude, false for kimi — and the flag is honest, because kimi's
mechanism is genuinely different: a **persistent registry** in the engine's home
directory, not a per-launch environment.

```
kimi provider add <url>     Import every provider listed in a custom registry (api.json)
kimi provider catalog       Discover and import providers from the public models.dev catalog
kimi provider list --json   Emit the raw providers/models config as JSON
kimi provider remove <id>   Remove a provider and every model alias that referenced it
```

Run live on this box:

```
$ kimi provider list
managed:kimi-code  type=kimi  models=4  source=oauth
Default model: kimi-code/k3
```

And configured providers *already* surface in the model `configOption` AgentKate
discovers at handshake — the plumbing is half-done. What is missing is a way to
put anything in the registry from inside AgentKate.

The user's note settles the design question before it is asked: this is an
**additional** option, not a replacement. Plan 11's env-injection routing stays
first-class and untouched; kimi gets a second, differently-shaped mechanism, and
the capability set grows a second flag rather than overloading the first.

**And an unhealthy engine is currently indistinguishable from a broken agent.**
Today the only signal that `kimi` is unauthenticated, or misconfigured, or
missing, is that `agent.start` fails — surfacing as `acp session/new: <error>`
wrapped by `handshake()` (`core/internal/kimi/thread.go`), with the same
opacity for a missing binary, a bad `config.toml` and a needed login. Meanwhile
the CLIs answer all three questions directly:

```
kimi doctor config|tui        Validate config.toml / tui.toml, non-interactively
claude doctor                 Check the health of your Claude Code installation
```

and kimi's ACP handshake advertises exactly what to do about auth — verified
live this session:

```json
"authMethods": [{"id":"login","type":"terminal","name":"Login with Kimi account",
  "args":["--login"],
  "_meta":{"terminal-auth":{"command":"kimi","args":["login"]}}}]
```

AgentKate discards that entire `initialize` result today (`thread.go` and
`discover.go` both call `client.call(ctx, "initialize", …, nil)` with a nil
out), so the one piece of actionable guidance the engine offers is thrown away.

## Verified facts

| Fact | How verified | Consequence |
|---|---|---|
| **`providers/list` is not implemented over ACP** (`-32601 Method not found`, with and without a session) | Live probe, kimi 0.30.0 | The registry is managed by **shelling out**, not over the live connection. This kills the tidier design and must be recorded so nobody retries it |
| `kimi provider list --json` exists and emits raw config | `kimi provider list --help` | Structured read; no prose parsing |
| `kimi provider add <url>` imports from a custom `api.json` registry; `catalog` imports from models.dev | `kimi provider --help` | Two different add flows, and the UI needs both |
| `kimi provider remove <id>` removes the provider **and every model alias that referenced it** | `kimi provider --help` | Removal is destructive beyond the provider row — the confirm dialog must say so |
| `KIMI_CODE_HOME` relocates a kimi thread's sessions, config **and credentials** | `docs/HARNESSES.md` (verified on 0.30.0) | Per-thread provider sets compose with per-thread homes — and pointing a thread at an empty home also un-authenticates it |
| kimi's config shape carries `providers{id:{type,base_url,default_model,has_api_key}}`, `default_provider`, `default_model` | Config decoder in the 0.30.0 bundle | The JSON we will parse, with `has_api_key` as a boolean — **the key itself is never emitted**, which is what makes reading it safe |
| kimi's provider record also carries `status` and `models` | Same decoder | A provider can be listed and unhealthy; show it |
| `kimi doctor config [path]` / `kimi doctor tui [path]` validate non-interactively | `kimi doctor --help` | Health check needs no TTY |
| `claude doctor` *"Reads settings files in the current directory without a trust prompt"* | `claude doctor --help` | Safe to run per-project; the claude adapter's `Health()` has a real implementation |
| `initialize`'s `authMethods` names the exact terminal command (`kimi login`) | Live probe | The health card can offer a button that runs the right thing, not generic advice |

## Phase 1 — `Health()` on the harness

The first engine-level (thread-less) harness method. Plan 21's `LiveSessions()`
is the second; whichever lands first sets the pattern.

**`core/internal/harness/harness.go`:**

```go
// HealthState is a traffic light, deliberately coarse — the detail lives in
// Checks. Unknown is a real state: a probe that timed out has not said "bad".
const (
    HealthOK      = "ok"
    HealthWarn    = "warn"    // usable, something is off
    HealthBad     = "bad"     // will not start
    HealthUnknown = "unknown"
)

// Check is one named health assertion, with a remedy the UI can act on.
type Check struct {
    Name    string `json:"name"`    // "binary", "version", "auth", "config", "models"
    State   string `json:"state"`
    Detail  string `json:"detail"`
    // Remedy is a command the user can run, verbatim, e.g. "kimi login".
    // Taken from the engine where it advertises one (kimi's authMethods
    // _meta.terminal-auth) — never invented by us.
    Remedy string `json:"remedy,omitempty"`
}

type Health struct {
    EngineID string  `json:"engineId"`
    State    string  `json:"state"`   // worst of Checks
    Version  string  `json:"version"`
    Checks   []Check `json:"checks"`
    // Models is the discovered catalogue size, so the card can say
    // "4 models" without a second round trip.
    Models int `json:"models"`
}
```

plus `Health(ctx) (Health, error)` on the interface. **No capability flag** —
every harness can answer, even if the answer is `unknown`. A flag would let an
adapter opt out of being diagnosable, which is the opposite of the point.

**Kimi** (`harness_kimi.go`): `kimi --version` → binary + version;
`kimi doctor config` → config check; the `authMethods` now decoded from
`initialize` → auth check with `Remedy: "kimi login"`; `DiscoverOptions()`'s
cached model list → models. **This requires decoding the `initialize` result**
at `core/internal/kimi/thread.go` and `discover.go`, which today pass `nil` —
a small, independently useful fix that also improves every handshake error.

**Claude** (`harness_claude.go`): `claude --version`; `claude doctor` (run in
the project dir — it reads settings there without a trust prompt);
`DiscoverModels` for the catalogue. Auth state comes from `claude doctor`'s
output rather than a separate probe.

Both are best-effort and **cached** (30 s) — the New Agent dialog opens often
and neither doctor is instant. Failure returns `HealthUnknown`, never an error
that would blank the card.

New RPC `engine.health` in `core/cmd/akcore/handlers.go`, taking an optional
`engineId` (all engines when absent) and an optional `project` (so
`claude doctor` runs in the right directory).

## Phase 2 — The preflight card (UI)

- A collapsible card under the engine combo in `ui/src/NewAgentDialog.cpp`,
  refreshed when the engine selection changes (the combo already has an
  `currentIndexChanged` handler at `:197` that rebuilds per-backend choices —
  hook in there).
- Traffic-light chip via `ChipPainter` plus one line per non-OK check.
- A check with a `Remedy` gets a button: **"Run `kimi login`"** opens a
  `TerminalPanel` tab with that command — the terminal already exists
  (`ui/src/TerminalPanel.cpp`), and a terminal auth flow needs a terminal, which
  is precisely why `authMethods` says `type: "terminal"`.
- **Start is not blocked on `bad`.** It warns and lets the user proceed: a
  health check that is wrong must never be the thing that stops work. It *does*
  change the failure message — a start that fails after a `bad` auth check
  reports the diagnosis instead of the raw RPC error.
- Health also feeds the roster: an engine that goes `bad` while agents are
  running shows in the status area rather than only at launch time.
- Flicker rule: health is a `Reactive<Health>` per engine; the 30 s cache plus
  equality guard means the card repaints only on a real state change.

## Phase 3 — The Kimi provider registry (core)

New `core/internal/kimi/provider.go`, all functions taking an explicit `home`
(the thread's `KIMI_CODE_HOME`, or the user's default) because *which registry*
is the whole point:

```go
func ListProviders(home string) ([]Provider, error)  // kimi provider list --json
func AddProvider(home, url string) error             // kimi provider add <url>
func ImportCatalog(home string) ([]Provider, error)  // kimi provider catalog
func RemoveProvider(home, id string) error           // kimi provider remove <id>
```

`Provider` mirrors the CLI's own JSON — `{id, type, baseUrl, defaultModel,
hasApiKey, status, models[]}` — and **carries no key**. The CLI does not emit
it; we do not ask for it; nothing in AgentKate ever holds a kimi provider
credential. That is a genuine advantage over the claude-side story (where we
broker keys through KWallet) and it should be stated in the UI so the difference
is understood rather than looking like an omission.

New `Capabilities.ProviderRegistry bool` — **not** a change to
`ProviderRouting`. Two flags for two mechanisms:

| | `ProviderRouting` (plan 11) | `ProviderRegistry` (this plan) |
|---|---|---|
| Mechanism | env injection at spawn | persistent registry in the engine home |
| Scope | per thread | per home (shared by threads with the same home) |
| Credentials | ours, via KWallet or env var | the engine's, we never see them |
| Claude | **true** | false |
| Kimi | false | **true** |

New RPCs: `kimiProvider.list` / `.add` / `.catalog` / `.remove`, each taking an
optional `threadId` to resolve which home. **Every one of these mutates the
user's engine configuration**, so each is UI-only: none is reachable from an
agent, for the same reason `StartSpec.Env` is not
(`core/internal/harness/harness.go:160-165` — a worker's credentials source is
not a lever we hand to a model).

## Phase 4 — Providers UI, both kinds

`ui/src/ProvidersDialog.cpp` grows a second section rather than being rewritten:

- **"API providers (Claude Code)"** — today's list, untouched. Still the way to
  run claude against Fireworks/OpenRouter/anything Anthropic-compatible.
- **"Kimi provider registry"** — a new list, driven by `kimiProvider.*`, with:
  - **Import from models.dev** (`catalog`) — the discovery path.
  - **Add from a registry URL** (`add <url>`) — the custom `api.json` path.
  - **Remove**, whose confirm dialog states plainly that it also removes *every
    model alias that referenced it* (the CLI's own wording; a user who loses
    their aliases without warning will not know what happened).
  - A per-row `hasApiKey` / `status` indicator, and a note that keys are held by
    kimi, not by AgentKate.
- A **home selector**: "user default" or a specific thread's `KIMI_CODE_HOME`,
  so the composability the feature exists for is visible. Two kimi agents in one
  project can target different provider sets — that only makes sense if you can
  see which registry you are editing.
- 22 of the `tr()` calls the KDE audit found are in `ProvidersDialog.cpp`. This
  is the file being edited anyway: **convert them to `i18n()` in the same
  change.** It costs nothing now and is a separate chore later.
- `HarnessTraits` mirrors `providerRegistry`; the section hides when no
  registered engine has it.

## Verify

| Phase | What proves it |
|---|---|
| 1 | `go test ./cmd/akcore/…`: `TestHealthWorstStateWins` (a `bad` check makes the whole engine `bad`; `warn` + `ok` is `warn`); `TestHealthUnknownOnTimeout` (a doctor that hangs yields `unknown`, not `bad`); `TestHealthIsCached` (two calls inside the window run the subprocess once). |
| 1 | `TestInitializeAuthMethodsDecoded` in `core/internal/kimi/` — the handshake result is no longer discarded and the `_meta.terminal-auth` command is extracted. |
| 1 | Manual: `mv ~/.kimi-code/credentials{,.bak}` → `engine.health` reports `auth: bad` with remedy `kimi login` → restore → `ok`. |
| 2 | Manual: rename the `kimi` binary → the card shows `binary: bad`, Start still works for claude agents, and starting a kimi agent fails with the diagnosis rather than the raw ACP error. |
| 2 | Qt test `ui/tests/PreflightCardTest.cpp`: publishing an identical `Health` twice causes zero repaints. |
| 3 | `TestListProvidersParsesCLIJSON` against a captured `kimi provider list --json` fixture, plus a malformed fixture that must yield an error rather than a half-parsed list. |
| 3 | `TestProviderOpsUseTheGivenHome` — a fake `kimi` script asserts `KIMI_CODE_HOME` is set to the requested home on every op. This is the bug that would otherwise silently edit the user's real registry. |
| 3 | `TestProviderRPCsAreNotAgentReachable` — the MCP tool catalogue (`core/cmd/akcore/mcp.go`) contains no `kimiProvider.*` tool. Same guard style as the `Env` reachability rule. |
| 4 | Manual: import the models.dev catalogue into a throwaway `KIMI_CODE_HOME`, start a kimi agent with that home, confirm the new providers' models appear in the model picker via the existing discovered-options path. |
| 4 | Manual: remove a provider, confirm the warning names the alias consequence, confirm the model picker updates. |
| 4 | `grep -c 'tr(' ui/src/ProvidersDialog.cpp` → 0. |

## Non-goals

- **Replacing plan 11.** The user's note is explicit. Claude-side env-injection
  routing stays first-class and is not touched by this plan.
- **Holding kimi provider credentials.** kimi's own `credentials` store owns
  them. We never read, write or broker a kimi provider key — and the UI says so.
- **A unified provider abstraction.** The two mechanisms differ in scope,
  ownership and lifetime. One `Provider` type covering both would have to lie
  about at least one of them.
- **Auto-remediation.** Health *offers* `kimi login` in a terminal. It does not
  run login flows, rewrite `config.toml`, or reinstall a CLI.
- **Blocking a launch on a health verdict.** Warn, never block.

## Open questions for the user

1. **Per-thread kimi homes by default?** The registry composes with
   `KIMI_CODE_HOME`, but today a kimi thread uses the user's real home unless
   told otherwise. Should selecting a non-default provider set for an agent
   *automatically* give it its own home — which also means it must be
   authenticated separately — or should that stay an explicit choice? (This is
   the same question plan 22 asks for skills, and it should get the same answer.)
2. **How aggressive should preflight be?** Running `claude doctor` and
   `kimi doctor` every time the New Agent dialog opens costs two subprocesses.
   Options: on dialog open (30 s cache, this plan's default), on app start only,
   or a manual "Check engines" button.
3. **Should engine health appear in the system tray?** Plan 27's tray aggregates
   agent state. "Your kimi login expired" is arguably the same class of thing —
   or it is noise in a menu that should be about agents.
