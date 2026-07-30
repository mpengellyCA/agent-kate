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

Declare `Capabilities()` honestly — every flag gates real behavior:

| Field | Gates |
|---|---|
| `Fork` / `Promote` | agent.fork / agent.promote |
| `Compaction` | the whole summary family (setCompactStrategy, compactNow, summaryStatus, exit compaction, seeded resume) |
| `ProviderRouting` | provider overlays in the Engine picker + agent.start validation |
| `Cowork` | the desktop MCP server + the panel checkbox |
| `EffortLive` | whether the effort picker stays live while running |
| `MintsSessionID` | whether the core pre-mints the session id (claude `--session-id`) or your CLI assigns one that `Launch` reports back |
| `ModelPicker` | `tiers` (fixed tokens the core resolves) vs `discovered` (per-session enumeration + free text) |
| `PermissionModes` / `Efforts` | static vocabularies; empty = discovered per session via `configOptions` |

**Do not emulate what the CLI lacks.** A missing capability is honestly gated
with one shared message ("X is not supported by <DisplayName> agents"), never
faked.

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

- `session.Record.Backend` is `""` on records written before the registry
  existed; `Registry.Get("")` resolves it to the default harness. New records
  carry explicit ids.
- The registry is built once at startup and read-only afterwards — no locking,
  and nothing may register late.
- Provider routing is an *overlay on a harness*, not a harness: it swaps the
  API behind the same binary (env injection, `internal/agent/provider.go`).
  If your CLI exposes an Anthropic-compatible endpoint, that's a provider
  profile, not a new harness.
