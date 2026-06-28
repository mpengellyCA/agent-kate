# 11 — Third-Party API Providers (Fireworks, OpenRouter) via the Anthropic endpoint

> **Status: implemented** (all three phases). Core: `core/internal/agent/provider.go`
> (`buildEnv` + tests), `StartOptions.Provider`, `agentStartParams.Provider`, the
> `session.Record` provider snapshot, and resume rebuild. UI: `ui/src/ProviderConfig.*`
> (KWallet-backed store, gated on `AK_HAVE_KWALLET`), `ui/src/ProvidersDialog.*`, the
> per-agent **Provider** picker + model-combo repopulation in `AgentPanel`, and the
> **Options ▸ Configure API Providers…** menu entry. Built and tested (Go + ctest green).

## Goal

Let a human configure an Agent Kate agent to run the **Claude Code CLI harness against a
third-party, Anthropic-compatible API** — primarily **Fireworks / Fire Pass** and
**OpenRouter** — instead of Anthropic's own endpoint. Each agent picks a **provider
profile** at creation (default: *Claude (direct)*), so the multi-agent arena can run
GLM, Kimi, Claude, etc. **side by side** on the same task.

The routing mechanism is settled and trivial at the wire level: Claude Code already
honours a handful of `ANTHROPIC_*` / `CLAUDE_CODE_*` **environment variables**. Fireworks'
own integration ("FireConnect") and Fire Pass setup do nothing more than write those vars
(plus model-slot overrides) into `~/.claude/settings.json`:

```
ANTHROPIC_BASE_URL=https://api.fireworks.ai/inference
ANTHROPIC_API_KEY=fw_…            ANTHROPIC_AUTH_TOKEN=fw_…   (alias)
ANTHROPIC_MODEL=accounts/fireworks/routers/glm-5p2-fast
ANTHROPIC_DEFAULT_OPUS_MODEL / _SONNET_MODEL / _HAIKU_MODEL = …
ANTHROPIC_SMALL_FAST_MODEL / CLAUDE_CODE_SUBAGENT_MODEL = …
```

Agent Kate spawns its own headless `claude` per thread and **does not currently set
`cmd.Env`** (`core/internal/agent/agent.go:325` — the child inherits `akcore`'s
environment verbatim). So the entire feature is: **compute a per-thread environment from
the selected provider profile and hand it to that one `exec.Command`.** No settings.json
mutation, no global state, no interference between agents.

## Decisions (defaults; adjustable)

These three were open product questions; the plan assumes the following and each is cheap
to change:

1. **Scope — per-agent provider profiles.** Profiles (name, base URL, model-slot map,
   credential reference) are defined once; each agent selects one at start. *Claude
   (direct)* is the default and injects **no** env, preserving today's behaviour exactly.
   Rationale: Agent Kate's identity is running agents in parallel — comparing providers
   within one arena is the high-value case. (Alternative: a single global toggle. Smaller,
   but loses side-by-side.)
2. **Secrets — KWallet / KSecretService, with an environment-variable fallback; never
   plaintext on disk.** The non-secret half of a profile (base URL, model map) lives in
   `agentkaterc` (KConfig). The API key lives in the KDE secret service, keyed by profile
   id. A profile may instead declare an **env-var name** (e.g. `FIREWORKS_API_KEY`) and the
   key is resolved from the environment at launch — covering headless / "I manage keys
   outside the app" setups and keeping `akcore` resume self-sufficient. The key is **never**
   written to `threads.json` or logged.
3. **Deliverable — this plan first** (repo convention; see `docs/plans/`).

## Mechanism, grounded

The launch path and every place a per-agent setting is threaded today:

| Stage | Location | What flows |
|---|---|---|
| UI form | `ui/src/AgentPanel.cpp:502-773` | combos: model tier, effort, mode, isolation; `m_coworkCheck` |
| UI → core | `ui/src/AgentPanel.cpp:1741-1771` (`onSendClicked`) | `agent.start` params via `CoreClient::call` (`ui/src/ipc/CoreClient.cpp:123`) |
| core params | `core/cmd/akcore/main.go:445-454` (`agentStartParams`) | `workspacePath, prompt, permissionMode, effort, model, isolation, coworkEnabled, attachments` |
| core start | `core/cmd/akcore/main.go:2356` (`startAgentThread`) → `resolveModel` (`:2787`) | builds `agent.StartOptions`, persists `session.Record` (`:2402`) |
| supervisor | `core/internal/agent/agent.go:276` (`Start`) → `:325` `exec.Command` | **`cmd.Env` unset today** ← injection point |
| persistence | `core/internal/session/session.go:31-61` (`Record`) | resume replays `Model/Effort/PermissionMode/CoworkEnabled` |
| resume | `core/cmd/akcore/main.go:2431` (`resumeAgentThread`) | rebuilds `StartOptions` from the `Record` |

`coworkEnabled` is the exact analog to follow end-to-end: a per-agent value added to the
form, the `agent.start` params, `agentStartParams`, `StartOptions`, and `session.Record`,
then re-applied on resume. The provider follows the same spine, plus an env-computation
step in the supervisor and a secret-resolution step in the UI.

## Data model

A **provider profile** (non-secret fields, persisted in KConfig and mirrored over IPC):

```go
// core/internal/agent (new provider.go) — shared by core; UI has a parallel struct.
type Provider struct {
    ID        string            `json:"id"`        // stable key, e.g. "fireworks", "openrouter-glm"
    Name      string            `json:"name"`      // display label
    BaseURL   string            `json:"baseUrl"`   // ANTHROPIC_BASE_URL; "" ⇒ Claude direct (no injection)
    AuthToken string            `json:"authToken,omitempty"` // resolved secret; crosses the local IPC socket per-launch, NEVER persisted to disk
    EnvVar    string            `json:"envVar,omitempty"` // fallback: read token from this env var
    Models    map[string]string `json:"models,omitempty"` // slot → provider model id (see below)
}
```

`Models` slots map to Claude Code's override vars:

| slot key | env var set | purpose |
|---|---|---|
| `main`    | `ANTHROPIC_MODEL`                 | default model (used when `--model` is absent) |
| `opus`    | `ANTHROPIC_DEFAULT_OPUS_MODEL`    | opus-tier resolution |
| `sonnet`  | `ANTHROPIC_DEFAULT_SONNET_MODEL`  | sonnet-tier resolution |
| `haiku`   | `ANTHROPIC_DEFAULT_HAIKU_MODEL`   | haiku-tier resolution |
| `subagent`| `CLAUDE_CODE_SUBAGENT_MODEL` **and** `ANTHROPIC_SMALL_FAST_MODEL` | subagents / small-fast |

An empty `BaseURL` is the sentinel for **Claude (direct)** — `buildEnv` returns the
inherited environment unchanged, so default agents are byte-for-byte unaffected.

### Built-in presets

- **Claude (direct)** — `BaseURL: ""`. Default.
- **Fireworks (Fire Pass)** — `BaseURL: https://api.fireworks.ai/inference`,
  `EnvVar: FIREWORKS_API_KEY`, `main: accounts/fireworks/routers/glm-5p2-fast` with the
  rest of the slots defaulting to the same router (Fire Pass `fpk_…` keys route every alias
  to one model; standard `fw_…` keys can fan out per the FireConnect default table —
  `glm-latest` / `glm-5p1` / `minimax-m2p5`).
- **OpenRouter** — `BaseURL` and model slugs are provider-specific; confirm the exact
  Anthropic-compatible base URL and slug format from OpenRouter's Claude Code docs at
  implementation time. The architecture is provider-agnostic, so OpenRouter is *only* a
  preset row — no code paths special-case it.

Presets are starting points the user edits (key + model ids) in the settings UI.

## Core changes (Go)

### 1. Per-thread environment — `core/internal/agent/agent.go`

Add `Provider *Provider` to `StartOptions` (`:86-98`). In `Start`, before `cmd.Start()`:

```go
cmd := exec.Command(s.claudeBin, args...)
cmd.Dir = opts.WorkDir
cmd.Env = buildEnv(os.Environ(), opts.Provider)   // NEW
```

`buildEnv(base, p)`:
- `p == nil || p.BaseURL == ""` → return `base` unchanged (Claude direct).
- else: copy `base`, **strip** any inherited `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_*_MODEL`,
  `ANTHROPIC_SMALL_FAST_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL` (so a real Anthropic key in
  `akcore`'s env is **never** forwarded to a third-party base URL — a security must), then
  append `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY` **and** `ANTHROPIC_AUTH_TOKEN` (= token),
  and one var per populated `Models` slot.

Resolve the token last: prefer `p.AuthToken` (supplied by the UI from KWallet); if empty
and `p.EnvVar != ""`, read `os.Getenv(p.EnvVar)`. If still empty for a non-direct provider,
**fail the start** with a clear "no credential for provider X" lifecycle error rather than
silently calling an endpoint unauthenticated.

`--model` interaction: today `resolveModel` (`main.go:2787`) maps `opus→claude-opus-4-8`
etc. Under a provider those Claude ids are wrong. Simplest correct rule: when a provider is
selected, the UI sends the **provider model id verbatim** as `model` (the combo's itemData
holds provider ids, not tier tokens). `resolveModel`'s `default` branch already passes an
unknown token through unchanged, so a provider model path flows straight to `--model` with
no core change. If `model` is empty, `--model` is omitted and `ANTHROPIC_MODEL` (the `main`
slot) takes effect. Never log the resolved token; redaction belongs in `s.log` call sites.

### 2. IPC params — `core/cmd/akcore/main.go`

Extend `agentStartParams` (`:445`) with a `Provider *agent.Provider` (the UI sends the
non-secret fields **plus** the resolved `authToken` for this launch only — `authToken` is
accepted over the local Unix socket but never persisted). Thread it into `StartOptions` in
`startAgentThread` (`:2379`).

### 3. Persistence & resume — `session.Record` + `resumeAgentThread`

Add to `session.Record` (`session/session.go:31`) a **non-secret** provider snapshot:
`ProviderID, ProviderBaseURL, ProviderEnvVar, ProviderModels map[string]string`. The
**token is never stored.** On resume (`main.go:2431`), rebuild `opts.Provider` from those
fields and re-resolve the token: (a) from the env var if `ProviderEnvVar` is set
(self-sufficient, no UI), else (b) the UI must re-supply it. For v1, make resume of a
KWallet-only profile require the UI to re-attach the token before sending `agent.resume`
(the UI already drives resume); document the env-var path as the headless-friendly option.

## UI changes (C++ / Qt / KF6)

### 1. Settings → Providers (manage profiles)

A small dialog (model `ExtensionsDialog` / `SkillsDialog` style) listing profiles with
add/edit/delete. Per profile: name, base URL, env-var name (optional), the five model-slot
fields, and an API-key field whose value is written to **KWallet/KSecretService**, never to
KConfig. Non-secret fields persist under a new `[Providers]` KConfig group (the
`RecentProjects`/`AgentPanel` KConfig pattern, e.g. `AgentPanel.cpp:502-618`). Ship the
three presets pre-populated (key blank until the user enters one).

Secret backend: KF6 **KWallet** (`KWallet::Wallet`, folder `agentkate`, entry = profile id)
or **qtkeychain** if a lighter dep is preferred. Either is a new optional dependency — gate
behind a CMake `find_package` so a build without it degrades to env-var-only profiles.

### 2. Per-agent picker — `AgentPanel`

Add a **Provider** combo to the form (`AgentPanel.cpp:767-773`), above Model, populated from
the saved profiles + "Claude (direct)" (default, index 0). On change, **repopulate the
Model combo** from the selected profile's model ids (itemData = provider model id) instead
of the opus/sonnet/haiku/fable tiers; for Claude-direct, restore today's tier list
(`:595-600`). Like `m_coworkCheck`, keep it **non-sticky-by-default** is unnecessary here —
provider can stick like model/effort (KConfig `[Agent] provider`). Freeze the combo once
`m_threadId` is set, matching the other combos.

In `onSendClicked` (`:1741`), when a non-direct profile is selected, resolve its key (KWallet
or env), and add a `provider` object to the `agent.start` params:
`{id, name, baseUrl, envVar, authToken, models}`. For Claude-direct, omit `provider`
entirely so the request is identical to today's.

## Security notes

- **Never forward Anthropic's real key to a third party.** `buildEnv` strips inherited
  `ANTHROPIC_*` before injecting the provider's — the single most important correctness/
  safety property here. Add a focused unit test.
- **No secret on disk in our files.** KConfig and `threads.json` hold only non-secret
  fields. The token lives in KWallet or the user's environment.
- **No secret in logs.** Audit `agent.go` / `main.go` log lines added on this path; the
  token must never appear, even truncated.
- **Local socket only.** `authToken` crosses the UI↔core Unix socket (already the trust
  boundary for everything else); it is consumed for the launch and dropped, not echoed back
  or persisted.
- **base URL hygiene.** Require `https://` (allow `http://localhost` for local proxies);
  reject anything else in the settings dialog.

## Testing

- `buildEnv` unit tests (Go): direct ⇒ env unchanged; provider ⇒ correct vars set, inherited
  `ANTHROPIC_*` scrubbed, token from `AuthToken` and from `EnvVar` fallback, missing-credential
  error. (`core/internal/agent/agent_test.go` neighbours.)
- `resolveModel` pass-through for a provider model path (already covered by the `default`
  branch; add a case).
- Round-trip: `agent.start` with a provider → `session.Record` snapshot (no token) → resume
  re-injects from env var.
- Manual dogfood (self-hosted): create a Fireworks/Fire Pass profile, start an agent, confirm
  via the transcript/usage meter that it's hitting the Fireworks endpoint; run a second
  Claude-direct agent simultaneously and confirm isolation.

## Phasing

- **Phase 1 (core, testable headless):** `Provider` type + `buildEnv` + `StartOptions`/IPC
  param + `agentStartParams`, with the token resolved from `EnvVar`. No UI yet — a profile can
  be exercised by sending `agent.start` with a `provider` blob. This alone delivers the
  feature for env-managed keys.
- **Phase 2 (persistence/resume):** `session.Record` snapshot + `resumeAgentThread`.
- **Phase 3 (UI):** Providers settings dialog + KWallet backend + per-agent picker +
  model-combo repopulation.

## File-by-file change list

| File | Change |
|---|---|
| `core/internal/agent/provider.go` *(new)* | `Provider` type, `buildEnv`, env-key constants |
| `core/internal/agent/agent.go` | `StartOptions.Provider`; set `cmd.Env = buildEnv(...)` at `:325`; redact token in logs |
| `core/internal/agent/provider_test.go` *(new)* | `buildEnv` cases incl. ANTHROPIC_* scrub |
| `core/cmd/akcore/main.go` | `agentStartParams.Provider`; pass into `StartOptions`; persist/rebuild snapshot in `startAgentThread`/`resumeAgentThread`; `resolveModel` pass-through test |
| `core/internal/session/session.go` | non-secret `Provider*` fields on `Record` |
| `ui/src/AgentPanel.{h,cpp}` | Provider combo, model-combo repopulation, key resolution, `provider` in `agent.start` |
| `ui/src/ProvidersDialog.{h,cpp}` *(new)* | manage profiles; KWallet/qtkeychain secret backend |
| `ui/src/MainWindow.cpp` | menu/action to open Providers settings |
| `CMakeLists.txt` | optional `find_package(KF6Wallet)` (or qtkeychain), gated |
| `docs/plans/README.md` | index row |
