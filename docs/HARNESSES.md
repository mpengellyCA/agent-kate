# Adding an agent harness

A **harness** is one way of running an agent: a CLI binary spoken to over some
protocol. Agent Kate ships two — Claude Code (`claude -p`, stream-json) and
Kimi Code (`kimi acp`, ACP) — and is built so a third slots in by writing an
adapter and declaring its capabilities. No `backend == "…"` string compare may
appear outside the adapter and its supervisor package; the registry and the
capability set carry every difference.

## The pieces

```
core/internal/harness/harness.go   the Harness interface, Capabilities, Registry
core/internal/<name>/…             your supervisor: process lifecycle + event translation
core/cmd/akcore/harness_<name>.go  the adapter: supervisor → Harness
core/cmd/akcore/run.go             registration (one line)
ui/src/state/HarnessTraits.cpp     built-in fallback traits (mirror your Capabilities)
```

## 1. Write the supervisor (`core/internal/<name>`)

The supervisor owns the child processes and translates the CLI's native
protocol into **Claude-shaped stream-json events** — that translation is the
load-bearing idea: the relay, transcript, replay and rendering stacks stay
backend-agnostic because every harness feeds them the same shapes. Follow
`core/internal/kimi` as the reference; `translate.go` is the pattern.

Events your translator must produce:

- `{"type":"system","subtype":"init","session_id":…,"model":…}` once per
  process start. If the CLI enumerates models/modes per session, embed them as
  `configOptions: [{id, name, currentValue, options:[{value,name,description}]}]`
  — the UI persists them and populates its pickers from that.
- `assistant` / `user` messages with `text`, `thinking`, `tool_use` and
  `tool_result` content blocks (image blocks pass through as base64
  `{"type":"image","source":{…}}`).
- A `TodoWrite`-shaped `tool_use` for plan/checklist updates.
- One `{"type":"result", …}` per turn — the UI's turn boundary.
- Synthetic types are underscore-prefixed: `_lifecycle`
  (started/resumed/exited/interrupted/turn_aborted/error), `_stderr`,
  `_commands` (the slash-command list for the composer's autocomplete).

Contracts the rest of the system relies on:

- **Transcript**: if the CLI has no replayable transcript of its own, append
  every translated event to a per-thread JSONL log and serve it from
  `ReadTranscript` (kimi's event-log pattern). Resume must APPEND, never
  truncate.
- **Interrupt**: cancel in-band, keep the process resident, emit
  `turn_aborted` when the cancelled turn's result lands; a signal backstop for
  hung turns. An idle interrupt must be a no-op (an armed backstop with no
  result coming kills a healthy process).
- **Stop**: on a busy thread, interrupt first, wait (bounded) for the aborted
  result, then close stdin — never truncate a turn mid-write. Reject Sends
  while stopping.
- **Permissions**: route asks through the shared broker
  (`askHumanPermission`) so the UI approval flow is identical. If the CLI's
  permissions do NOT flow over the Cooperation MCP bridge, spawn the bridge
  with `--no-permission-tool` so the agent never sees the claude-only
  `request_permission` tool.

## 2. Write the adapter (`core/cmd/akcore/harness_<name>.go`)

Implement `harness.Harness`. `Launch(spec)` translates the neutral `StartSpec`
into your supervisor's options and returns a `Launched` with what was actually
APPLIED (session id, resolved model, defaulted mode) — the record persists
that, so resume replays reality. `SetOption` maps
`model|effort|permissionMode` onto your CLI's mid-session mechanism, or
returns an error naming the harness.

`StartSpec.Env` is a per-thread environment overlay every adapter applies the
same way (`agent.ApplyEnvOverlay`, after provider routing). It exists because
some CLIs have no flags worth shaping — `kimi acp` has none at all — and their
only per-thread lever is the environment: `KIMI_CODE_HOME` relocates a kimi
thread's entire home (sessions, config **and credentials**, verified on 0.30.0),
so pointing one at an empty directory also un-authenticates it. That is why the
overlay ships as a lever with no default policy. It is persisted on the record
and replayed on resume/fork/promote — a relaunch without it would look for the
session in a different home — and it is deliberately unreachable from
`launch_agent`: a worker's environment decides where its credentials come from
and which endpoint they go to.

`Compact(ctx, spec)` runs one context compaction and returns the summary body.
Both mechanisms live in your adapter, distinguished by `spec.Hot`: hot sends
`spec.Prompt` into the LIVE thread and returns its reply (no re-cache, needs a
running process); cold runs a fresh pass over `spec.SessionID` from
`spec.WorkDir` on `spec.Model`. If your CLI has neither, declare
`Compaction: false` and return `harness.Unsupported("Compaction", …)` — the
same wording the RPC gate uses, which a test pins.

Declare `Capabilities()` honestly — every flag gates real behavior:

| Field | Gates |
|---|---|
| `Fork` / `Promote` | agent.fork / agent.promote |
| `Compaction` | the whole summary family (setCompactStrategy, compactNow, summaryStatus, exit compaction, seeded resume) |
| `ProviderRouting` | provider overlays in the Engine picker + agent.start validation |
| `Cowork` | the desktop MCP server + the panel checkbox |
| `EffortLive` | whether the effort picker stays live while running |
| `TranscriptPreview` | whether the session browser can preview/forget an on-disk transcript store (false = the transcript lives only in the core's event log) |
| `MintsSessionID` | whether the core pre-mints the session id (claude `--session-id`) or your CLI assigns one that `Launch` reports back |
| `ModelPicker` | `tiers` (fixed tokens the core resolves) vs `discovered` (per-session enumeration + free text) |
| `PermissionModes` / `Efforts` | static vocabularies; empty = discovered per session via `configOptions` |
| `SystemPrompt` | whether `StartSpec.SystemPrompt` reaches the CLI (persona alongside its own prompt) |
| `CustomSubagents` | whether `StartSpec.Agents` reaches the CLI (caller-defined subagent profiles) |
| `FallbackModels` / `DisallowedTools` / `AddDirs` | the list-valued launch options; each gates one advanced field in the New Agent dialog, and a false flag means `Launch` reports the request in `Launched.UnappliedOptions` instead of dropping it |
| `SubagentTranscripts` | the panel's "Helpers" menu (`agent.subagentTranscripts`); implement the optional `subagentTranscriber` interface to point at your CLI's per-subagent files |

**Do not emulate what the CLI lacks.** A missing capability is honestly gated
with one shared message ("X is not supported by <DisplayName> agents"), never
faked.

### The two persona channels

`StartSpec` carries a persona in two forms, each capability-gated:

- **`SystemPrompt`** — text the agent runs with in ADDITION to the CLI's own
  system prompt. A flag that *replaces* the prompt is not this channel: it
  hides the CLI's tool and skill injections, so an adapter without an additive
  flag declares `SystemPrompt: false` rather than substituting one.
- **`Agents []AgentProfile`** — `{Name, Description, Prompt, Tools, Model}`
  subagent definitions for the session (`Tools` empty = all tools, `Model`
  empty = the thread's own).

Neither is ever a hard failure. `Launch` reports what it managed —
`Launched.SystemPromptApplied`, and one `Launched.AppliedAgent` per requested
profile naming per-field losses — and the orchestration layer surfaces the
rest as downgrades. An adapter whose capability is false returns
`harness.UnappliedAgents(spec.Agents, reason)` and leaves
`SystemPromptApplied` false; there is a backstop for the profiles an adapter
forgets to report, so a request can be refused but never silently dropped.

Today: **claude** applies both (`--append-system-prompt`, `--agents`; the
`--agents` JSON keys the profile by name and honors `tools` and `model`, but
validates nothing — the adapter refuses profiles the CLI would silently
discard). **kimi** applies neither: `kimi acp` has no system-prompt channel,
and its v1 engine resolves subagents from a compiled-in table
(`coder`/`explore`/`plan`). Kimi's documented `.agents/agents/*.md` catalogue
belongs to its v2 engine, which is reachable only via `kimi -p` with
`KIMI_CODE_EXPERIMENTAL_FLAG=1` — never over ACP — so writing agent files
would leave dead files in the worktree. Callers fold the persona into the
opening message instead, which behaves identically on every harness.

## 3. Register it

In `run.go`, after constructing your supervisor:

```go
harnesses.Register(newMyHarness(msup, exePath, *socket))
```

Registration order is the engine-picker order. That's the whole core hookup:
routing (`harnessFor`), the capability gates, `agent.capabilities`, the
unified start/resume paths and the UI pickers all follow automatically.

## 4. UI fallback traits

`ui/src/state/HarnessTraits.cpp` seeds built-in defaults served until
`agent.capabilities` answers (and against older cores). Add a defaults entry
mirroring your `Capabilities()` exactly. Everything else — the Engine picker,
mode/effort/model pickers, fork/compaction affordances, the roster badge — is
traits-driven and needs no per-harness code. Discovered option lists persist
under KConfig `[Agent] <id>Opt-<option>`; sticky picks under `<id>Mode` /
`<id>Thinking`.

## 5. Test it

- A fake CLI script driving your supervisor end to end
  (`internal/kimi/thread_test.go` is the pattern): full turn, interrupt,
  graceful stop mid-turn, resume-appends-transcript, permission bridge.
- Freeze your capability set in `cmd/akcore/harness_caps_test.go` — a flipped
  flag silently enables or hides whole feature families.
- A smoke script against the real CLI (`scripts/smoke-kimi.py` is the
  pattern), and run `go test ./...` (with `-race` on your package),
  `scripts/build.sh`, `ctest --test-dir build`.

## Sharp edges

- A new harness joins the **ensembles** (`internal/modes`) automatically: an
  ensemble names engines by registry id and models in that engine's own
  vocabulary, both passed through untouched. The one thing the ensemble layer
  reads from a harness is `Capabilities().PermissionModes` — its LAST entry is
  offered to controllers as the "run this worker unattended" mode, so order
  yours from most supervised to least. A harness whose modes are discovered per
  session (empty list) simply gets no such hint, never a guessed one.
- `session.Record.Backend` is `""` on records written before the registry
  existed; `Registry.Get("")` resolves it to the default harness. New records
  carry explicit ids.
- The registry is built once at startup and read-only afterwards — no locking,
  and nothing may register late.
- Provider routing is an *overlay on a harness*, not a harness: it swaps the
  API behind the same binary (env injection, `internal/agent/provider.go`).
  If your CLI exposes an Anthropic-compatible endpoint, that's a provider
  profile, not a new harness.
- There is no direct supervisor handle in `handlerDeps` — every per-thread
  action goes through the registry (`d.harnessFor`, `d.agentRunning`,
  `d.agentStop`). The last exception (the Claude supervisor, kept for hot
  compaction) went away in plan 16 P6, and it had been silently reporting a
  live *kimi* thread as not running to the destructive git guards.
