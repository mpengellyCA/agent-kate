# 14 — Harness Abstraction & Feature Parity (agent systems rework)

> **Status: in progress — P1–P2 landed.** Follows the Kimi Code backend branch
> (`kimi-code-backend`, `e0e9594` + review fixes `5a20c66`). Seven phases;
> land sequentially, one commit per phase, built green (`scripts/build.sh`,
> `go test ./...` incl. `-race`, `ctest --test-dir build`), smoke-verified
> (`scripts/smoke-kimi.py`, `scripts/smoke-*.py`) before moving on.

## Goal

Agent Kate should feel like the **better version** of Claude Code and Kimi
Code — never a feature-reduced wrapper. That means three things:

1. **One architecture for N harnesses.** Adding a backend must mean writing an
   adapter + declaring its capabilities — not sprinkling ~45 string compares
   across core and UI (the current count for kimi). Antigravity (once the
   `agy` CLI matures), and anything after it, should slot in.
2. **Feature parity with the native CLIs.** Everything a user can do in
   `claude` or `kimi` interactively must be reachable in the panel — or
   consciously, visibly deferred — and the UI must render everything the
   harness emits (thinking, plans, subagents, todos, background shells).
3. **Full session control, during and after a run** — mid-session model /
   effort / permission-mode changes, trustworthy interrupt/stop semantics,
   and surfacing the "hard to reference" harness data (context fill, per-tool
   token spend, turn durations, background-job progress).

## Where we are (assessment, 2026-07-30)

Three backend-integration patterns coexist, none composable with the others:

| Pattern | What varies | What it cost |
|---|---|---|
| **Provider routing** (plan 11, on main) | env only (`ANTHROPIC_BASE_URL`, creds, model slots) — same `claude` binary | clean; `provider.go` buildEnv + UI ProviderStore |
| **Antigravity branch** (stale, unmerged) | second driver *inside* `agent.Supervisor`; per-turn `agy --print` spawns; synthesized stream-json | no interface, no capability flags; degraded (no tool cards, no permissions, no MCP); abandoned |
| **Kimi backend** (this branch) | parallel `kimi.Supervisor`; ACP → stream-json translator | works well, but ~20 core + ~25 UI `backend == "kimi"` conditionals gate: provider, cowork, compaction, fork, promote, transcript source, permission tool, model-picker style, session-id lifecycle, resume flow |

The kimi translator (`core/internal/kimi/translate.go`) is the keeper: the
"translate foreign events into canonical Claude-shaped stream-json" idea is
what lets the whole render/relay/transcript stack stay backend-agnostic. The
scattered conditionals are the anti-pattern to remove — they are exactly a
capability set, expressed as string compares.

**On "Kimi should have been a provider":** a provider swaps the API behind
the *same* harness binary; Kimi Code is a *different* harness binary speaking
a different protocol (ACP). Conflating them in core would break both
abstractions — but the *UX* intuition is right: the user should face **one
"who runs this agent" picker**, not two. Phase 3 flattens harness × provider
into a single Engine list in the UI (Claude Code · Claude via Fireworks ·
Claude via OpenRouter · Kimi Code · …) while core keeps the two axes
orthogonal. (Aside: Moonshot exposes an Anthropic-compatible endpoint, so
"Kimi K2 via the Claude harness" could someday be a *provider* entry too —
the Engine list makes that a row, not a redesign.)

## Target architecture

### Core: `Harness` interface + capability registry

```go
// core/internal/harness/harness.go (new package)
type Harness interface {
    ID() string                       // "claude", "kimi", "antigravity"
    Start(opts StartOptions) (Thread, error)
    Send(threadID, text string, atts []Attachment) error
    Interrupt(threadID string) error
    Stop(threadID string) error
    Running(threadID string) bool
    StopAll()
    ReadTranscript(threadID string) ([]json.RawMessage, error)
    Capabilities() Capabilities
}

type Capabilities struct {
    Fork, Compaction, Promote  bool
    ProviderRouting, Cowork    bool
    Effort                     bool
    PermissionModes            []string // empty = harness always asks
    ModelPicker                ModelPickerKind // Tiers | FreeText | Enumerated
    MidSessionModelSwitch      bool
    UsageReporting             bool     // tokens/cost in result events
    SessionBrowse              bool     // on-disk session discovery
    SlashCommands              bool
}
```

- `agent.Supervisor` and `kimi.Supervisor` become the two implementations
  (thin wrappers at first — no big-bang rewrite of either).
- `handlerDeps` holds `harnesses map[string]Harness`; every
  `isKimiThread`/`agentSend`/`agentStop`/`agentInterrupt` helper and every
  per-method rejection (`fork`, `promote`, `compactNow`, `summaryStatus`,
  provider/cowork validation) is replaced by a capability lookup —
  `harnessFor(threadID).Capabilities().Fork` — with ONE shared error message
  ("X is not supported by <harness name> agents").
- The duplicated `startAgentThread` / `startKimiThread` worktree + registry +
  record + lifecycle scaffolding collapses into one `startThread` that calls
  `h.Start` in the middle.
- New RPC `agent.capabilities` (also embedded per-record in `agent.list`)
  returns the capability set so the UI never hardcodes it.

### UI: traits-driven affordances

- `HarnessTraits` fetched once per backend from `agent.capabilities`; every
  current `== QLatin1String("kimi")` site in `AgentPanel`/`AgentDock`/
  `NewAgentDialog` binds to a trait instead (fork button, Memory menu,
  effort/provider/cowork/mode pickers, model combo kind, roster badge —
  badge text comes from the harness display name, so "Kimi ·" is data too).
- **Engine picker** replaces the separate backend + provider combos in Setup
  and NewAgentDialog: one list of engines (harness + optional provider
  overlay), remembered as today, frozen after start.

## Feature-parity matrix (the gap list)

Facts verified against the current tree; **bold** = phase that closes it.

### Rendering gaps (harness emits it, panel drops it)

| Feature | Claude backend | Kimi backend | Close in |
|---|---|---|---|
| Thinking | `thinking`/`redacted_thinking` blocks dropped (`AgentPanel.cpp` renderEvent matches only text/tool_use) | `agent_thought_chunk` dropped by translator | **P2** collapsed "thought for…" card |
| Plan / todos | `TodoWrite` renders as generic tool row | ACP `plan` updates dropped | **P2** shared checklist card |
| Subagents | `Task` tool = generic row (only `Workflow` got a monitor) | n/a | **P4** Task nesting à la WorkflowMonitor |
| Background shells | `run_in_background` Bash / `BashOutput` = generic rows; no tray | n/a | **P4** background-jobs tray |
| Turn stats | `result.num_turns`, final result text, `permission_denials`, per-model `modelUsage` dropped | ACP exposes no usage (verify against kimi ≥0.30) | **P2** |
| Init payload | `slash_commands`, `tools`, `agents`, `skills` from init event dropped | `available_commands_update` dropped | **P3** |
| Non-text tool results | image blocks dropped by `toolResultText` | same path | **P4** |

### Control gaps (CLI can, panel can't)

| Feature | Native CLI | Agent Kate today | Close in |
|---|---|---|---|
| Slash commands | `claude`: custom commands, skills; `kimi`: `available_commands` | composer has zero slash support | **P3** autocomplete fed by init event / ACP list |
| Mid-session model/effort | `/model` etc.; SDK control_request subtypes (verify: `set_model`, `set_permission_mode`); kimi: `session/set_config_option` works mid-session | model/effort frozen at start (plan 05 never landed) | **P3** |
| Permission-mode change mid-run | `/permissions`, SDK `set_permission_mode` | frozen at start | **P3** |
| Plan mode | `--permission-mode plan` + ExitPlanMode flow | not offered | **P3** |
| Kimi model choice | kimi enumerates models (session `configOptions`) | free-text field | **P2** populate combo from handshake `configOptions` (extend the struct we already parse) |
| Kimi "when to ask" | kimi has ACP config options / modes (investigate yolo-equivalent) | disabled (correctly, since `5a20c66`) | **P3** investigate mapping |
| Session browse/attach | any on-disk claude session (`session.browse`) | claude only | **P6** kimi equivalent if the CLI exposes one |

### Observability gaps ("in the core, invisible to the user")

Core already computes, slog-only: `toolMeter` per-tool context cost
(`core/internal/agent/toolmeter.go`), `usageMeter` per-turn/session/final
usage (`usagemeter.go`), session-id changes, `LastTurnAt`. Nothing anywhere
computes ETAs. **P5** surfaces: context-fill meter (% of window used, the
number that predicts auto-compact), per-tool token spend in the AI Inspector
(finishes the `tool-token-spend` plan's Phase 2 telemetry), elapsed + average
turn duration on the working indicator (honest "running 4m · turns average
2m10s" beats a fake ETA), permission-request countdown (the 8-minute broker
timeout is invisible today), background-job elapsed/last-activity in the tray.

## Session-control semantics (reviewed)

- **Claude interrupt** — in-band `control_request:interrupt` + abort
  observer + SIGINT-process-group backstop. Matches the CLI's Esc. ✓
  (P1 correction: the ✓ held only mid-turn. An *idle* interrupt armed a
  backstop no result could disarm — the same kill-a-healthy-process hazard
  `5a20c66` fixed for kimi. P1 added claude-side turn tracking
  (`turnsInFlight`, the counterpart of kimi's `activePrompts`) and made idle
  interrupt a no-op.)
- **Kimi interrupt** — `session/cancel` + backstop; after `5a20c66` a cancel
  racing natural completion or an idle interrupt is a no-op. Matches ACP. ✓
- **Claude stop mid-turn** — `Stop` closes stdin then **kills after 5 s**, so
  stopping during a long turn truncates it and can leave the session JSONL
  mid-write. **P1**: `agent.stop` on a busy thread should interrupt, wait for
  the aborted result (bounded), then close stdin — same dignity the CLI gives
  Ctrl+C. Kimi mirror: cancel → await → close.
- **Double resume** — now rejected core-side for both backends. ✓
- **Send mid-turn** — core writes to stdin anytime; the CLI buffers to the
  turn boundary; the UI keeps its own visible queue and drains one per
  result. Good design — keep, and reuse unchanged for every harness.

## Phases

**P1 — Graceful mid-turn stop (S). ✅ LANDED.** Interrupt-then-stop sequencing
in both supervisors; tested with the fake-claude and fake-kimi scripts.
What the implementation added beyond the sketch:
- Claude-side turn tracking (`Thread.turnsInFlight`; every turn starts with
  our own Send and ends with exactly one `result`) — needed both to know a
  Stop is "busy" and to make an idle Interrupt a no-op (see the corrected
  interrupt note above).
- A `stopping` flag on both threads: repeated Stops coalesce, Sends during a
  stop are rejected deterministically ("thread is stopping") instead of
  racing the stdin close, and the `turn_aborted` lifecycle event is
  suppressed mid-stop (the exit note is the one the UI should show).
- The stream-json fake claude lives in `internal/agent/agent_test.go`
  (init/turns/interrupt), not cmd/akcore — supervisor tests sit with the
  supervisor, mirroring `internal/kimi/thread_test.go`; cmd/akcore's shell
  fake still covers the compaction flow.

**P2 — Rendering parity quick wins (M). ✅ LANDED.** Thinking cards (both
harnesses — translator maps `agent_thought_chunk` to a thinking block),
TodoWrite/plan checklist card (ONE card per thread, updated in place — the
current plan, not a trail of stale copies; translator maps ACP `plan` to the
TodoWrite tool_use shape so both backends feed it), result extras (num_turns,
denials, per-model usage in the inspector; error-result `result` text now
surfaces — success text is deliberately skipped, it duplicates the last
assistant message), kimi model enumeration from `configOptions`.
Facts verified against the real binaries while implementing:
- kimi 0.30 `session/new` configOptions: `model` (4 models w/ display names),
  `thinking` (low/high/max — the effort analogue), `mode`
  (default/plan/auto/yolo, with descriptions — the approval-mode analogue).
  This ANSWERS P3's "kimi approval-mode investigation": map "when to ask" to
  the `mode` config option. All three are persisted UI-side (KConfig
  `Agent/kimiOpt-<id>`) from the init event for P3 to consume.
- kimi 0.30 exposes no usage accounting (prompt response carries only
  stopReason) — the gap-list "verify against kimi ≥0.30" is settled: no
  usage for kimi until upstream adds it.
- claude result event: `modelUsage` is camelCase per model and includes
  `contextWindow` — the honest denominator for P5's context-fill meter;
  `num_turns`/`modelUsage` are session-cumulative snapshots (latest wins),
  `permission_denials` is per-turn.
- claude init event carries `slash_commands`/`tools`/`agents`/`skills` (P3's
  autocomplete feed) — confirmed present on claude 2.1.x.

**P3 — Session control (L).** Verify the CLI's control_request subtypes
against the installed `claude` (never assume); wire mid-session model /
effort / permission-mode where supported; plan mode; slash-command
autocomplete fed by init event + ACP `available_commands_update`; kimi
approval-mode investigation. Unfreeze the corresponding combos post-start
where the harness allows.

**P4 — Working-visibility (L).** Background-jobs tray (background shells +
BashOutput polling), Task-subagent monitor reusing the WorkflowMonitor
pattern, non-text tool results (images) rendered.

**P5 — Observability (M).** Context-fill meter, toolMeter/usageMeter surfaced
in the AI Inspector, turn-duration stats on the working indicator,
permission countdown. (No fake ETAs — elapsed + historical averages only.)

**P6 — Harness registry refactor (L).** The `Harness` interface + capability
registry + `agent.capabilities` RPC + UI traits + Engine picker; delete every
`backend == "kimi"` conditional outside the kimi package; write
`docs/HARNESSES.md` (how to add one). This lands *after* parity work so the
capability set is discovered from real needs, not invented.

**P7 — Antigravity revival spike (S, optional).** Re-evaluate `agy` against
the registry: if it can hold a session and report tool calls now, port the
old branch as a third harness; else document what it's still missing.

## Non-goals

- Rewriting the kimi translator or the provider env-injection — both are the
  good parts.
- A generic "any ACP agent" backend (Gemini CLI etc.) — the registry makes it
  *possible* later; certifying arbitrary agents is out of scope here.
- Kimi compaction/fork — kimi has no session-fork or summary-seed primitive;
  these stay honestly capability-gated rather than emulated.
