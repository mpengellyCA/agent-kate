# Plan 16 — Multi-agent orchestration, user-defined ensembles, and Kimi first-class

The arena already runs Claude and Kimi threads side-by-side in one workspace, but
they are **islands**: an agent can list and discard other agents
(`mcp__cooperation__list_agents` / `discard_agent`) yet cannot *create* one, brief
one, or collect its result. This plan turns Agent Kate into a true multi-agent
orchestrator: MCP tools for cross-harness subagent spawning, user-customizable
controller/worker **ensembles** ("Fable Controls Opus", "Kimi K3 Controls K2.7
Code", …), a master-prompt channel that points controllers at those tools, and a
real-time MCP traffic view so the user can watch the orchestration happen.

It also closes the remaining "Kimi is a guest, Claude is the host" gaps and
exposes harness features both CLIs already ship but we don't surface
(`--append-system-prompt`, `--agents`, `--fallback-model` on the Claude side;
agent files, env overlay, subagent wire transcripts on the Kimi side).

All rules from docs/HARNESSES.md hold: no `backend == "…"` compares outside the
adapter/supervisor/`HarnessTraits`, capability-gated honesty, and `Launch` must
report what was actually applied.

## Why

- The user-facing pitch of Agent Kate is a *multi-agent arena*; today
  "multi-agent" means "several human-launched threads cooperating via a notes
  board". Agents themselves cannot delegate across harnesses — the one thing the
  architecture is uniquely positioned to do (a Kimi controller driving Claude
  workers, Fable orchestrating K3, etc.).
- Kimi is first-class in the registry sense (own supervisor, ACP translation,
  permission bridge) but second-class in capability: no fork/compaction/promote,
  no usage reporting, no subagent transcript viewing, no persona injection, no
  env customization. Some gaps are honest CLI limitations (keep the gate); some
  are *our* hardcoded Claude assumptions (fix those).
- MCP traffic is invisible. The Cooperation bridge handles every cross-agent
  call, but the UI sees only generic `Tool` rows with raw-JSON summaries, and the
  core broadcasts nothing about bridge activity. For orchestration to be
  trustworthy it must be *inspectable live*.

## Verified facts (probed against real binaries + official docs)

**claude 2.1.220** (`claude --help`):

- `--append-system-prompt <prompt>` — append to the default system prompt.
- `--agents <json>` — inline custom subagent definitions:
  `'{"reviewer": {"description": "…", "prompt": "…"}}'`.
- `--agent <agent>` — select the agent for the session.
- `--fallback-model <model>` — comma-separated fallback list (print mode only).
- `--allowedTools` / `--disallowedTools`, `--effort low|medium|high|xhigh|max`,
  `--add-dir`, `--bg/--background`, `--brief`, `--betas`.

**kimi 0.30.0** (`kimi --help`, `kimi acp --help`,
[Agents and Sub-Agents](https://www.kimi.com/code/docs/en/kimi-code-cli/customization/agents.html),
[kimi acp reference](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/kimi-acp.html)):

- `kimi acp` takes **no harness-shaping flags** (only `--login` / `--help`).
  `--agent`, `--agent-file`, `--model`, `--plan`, `--yolo`, `--skills-dir` are
  main-command flags and cannot be passed through ACP.
- ACP `session/new` accepts `cwd` + `mcpServers` (stdio/http/sse forwarded);
  model/thinking/mode are post-handshake `session/set_config_option` values
  (already implemented in `core/internal/kimi/thread.go:486-510`).
- **Custom agents are discovered from directories**, which works regardless of
  launch flags: `<project>/.agents/agents/*.md` and `<project>/.kimi-code/agents/*.md`
  (project scope), `~/.agents/agents/` + `$KIMI_CODE_HOME/agents/` (user scope).
  Agent file = YAML frontmatter (`name`, `description`, `tools`,
  `disallowedTools`, `subagents`, `model_preference`) + Markdown body that *is*
  the system prompt. `model_preference: secondary` routes the subagent to the
  `[secondary_model]` config model when
  `KIMI_CODE_EXPERIMENTAL_SECONDARY_MODEL=1` is set.
- `$KIMI_CODE_HOME/SYSTEM.md` replaces the main agent's system prompt; kimi
  honors `KIMI_CODE_HOME` for all per-user state (→ per-thread isolation lever).
- Built-in subagents `coder` / `explore` / `plan`; subagent transcripts live at
  `<session-dir>/agents/<id>/wire.jsonl` (JSONL, chronological).
- ACP provides **no usage accounting and no fork** — those gates stay honest.
- Discovered model vocabulary (plan 15): `kimi-code/k3` ("K3"),
  `kimi-code/kimi-for-coding` ("K2.7 Coding"), `kimi-code/k3-256k`, etc.
- `kimi -p --output-format stream-json` exists (non-interactive JSONL) but is a
  different event shape from Claude's; not needed by this plan.

**Core today** (from the audit):

- `harness.StartSpec` (`core/internal/harness/harness.go:96-112`) has **no
  SystemPrompt field**; no supervisor passes `--append-system-prompt` or writes
  agent files.
- The Cooperation bridge (`core/cmd/akcore/mcp.go:155-411`) has `list_agents` +
  `discard_agent` but **no launch/send/wait**. It already holds everything a
  launch tool needs: an authenticated IPC client, the calling thread's id, and
  its workspace (`--socket --thread --workspace` flags, `main.go:26-29`).
- `resolveModel` tier map (`core/cmd/akcore/agents.go:549-562`):
  `opus→claude-opus-4-8`, `sonnet→claude-sonnet-4-6`,
  `haiku→claude-haiku-4-5-20251001`, `fable→claude-fable-5`. Kimi models flow as
  raw discovered ids through the same `Model` field.
- Hot/cold compaction bypass the registry and call Claude-specific paths
  (`agents.go:334-418`, `handlers.go:1073-1079`,
  `core/internal/compact/llm.go:42-60` shells `claude --resume`).
- Claude spawn already injects per-thread MCP config via `--mcp-config`
  (`agents.go:656-685`); kimi gets the same bridge via ACP `mcpServers`
  (`agents.go:644-651`). Both CLIs therefore already expose every new
  cooperation tool to their agents with zero CLI-side work.

**UI today**:

- `AgentPanel::renderEvent` (`ui/src/AgentPanel.cpp:3690-4230`) has **no
  fallthrough** — unknown event types are silently dropped; every `mcp__*` tool
  renders as a generic Tool row; `permSummary` (`AgentChatHelpers.cpp:49-56`)
  has no MCP branch; `ToolInspectorDialog`'s registry
  (`ToolInspectorDialog.cpp:228-288`) has no `mcp__` entries.
- The only cross-thread views are the roster (flat project→agent tree,
  `AgentRoster`), the CooperationPanel (state snapshot, not call traffic), and
  the AiInspectorPanel (follows the *active* thread only).
- No modes/persona/ensemble concept anywhere; KConfig `[Agent]` group holds
  per-harness sticky options (`HarnessTraits.cpp:14-29`).

---

## Feature 1 — Orchestration MCP tools (the keystone)

Add agent-control verbs to the Cooperation bridge, next to
`list_agents`/`discard_agent` in `core/cmd/akcore/mcp.go`. New tools:

- **`launch_agent`** `{ backend?, model?, prompt, title?, isolation?,
  permissionMode?, effort?, systemPrompt?, subagents?, wait? }`
  → starts a *real* Agent Kate thread (worktree, roster entry, live transcript)
  via the existing `agent.start` machinery, parented to the calling thread.
  `backend` empty = same harness as the caller; validated against the registry,
  capability-gated (`agent.capabilities` rules apply per target backend).
  Returns `{ threadId, sessionId?, applied }` (applied-truth, same contract as
  `harness.Launched`). With `wait: true` the call blocks until the worker goes
  idle and additionally returns its last assistant text (see `wait_agent`).
- **`send_agent`** `{ threadId, message, wait? }` → `agent.send` to a live
  thread the caller owns (same parent) or any thread in its workspace.
- **`wait_agent`** `{ threadId, timeoutSec? }` → blocks until the target thread
  is idle (no running turn) or the timeout fires; returns
  `{ status, lastText }`. Implemented as a new core-side RPC `agent.wait`
  (condition-variable on the thread's turn state in
  `core/internal/agent/agent.go`, fed by the same turn lifecycle that drives
  `_lifecycle` events) rather than bridge-side polling — the bridge's IPC call
  simply blocks.
- **`close_agent`** `{ threadId }` → polite stop (archive, reversible — the
  existing `session archive` path, `session.go:303-453`); `discard_agent`
  remains the destructive variant.

Design decisions:

- **Workers are full threads, not hidden headless runs.** They appear in the
  roster with live status, get their own transcript, and can be inspected or
  taken over by the human at any time. This is what makes orchestration
  *visible* (Feature 4) and costs nothing — `agent.start` already exists.
  (Alternative considered: ephemeral `claude -p`/`kimi -p` subprocesses spawned
  by the bridge. Rejected: invisible to the UI, no worktree management, no
  permission routing, duplicates the supervisor.)
- **Result collection** uses `wait_agent` + transcript tail instead of a new
  event subscription on the bridge connection. The bridge stays a thin RPC
  client; all state remains core-side.
- **Permissions:** a worker that needs approval surfaces as `NeedsInput` in the
  UI exactly like a human-launched thread (`askHumanPermission`,
  `handlers.go:2470-2486` is already backend-agnostic). The controller's
  `wait_agent` simply keeps waiting (timeout applies). Deadlock avoidance is the
  mode author's job (master prompt tells controllers to pick `permissionMode:
  "auto"`/`acceptEdits` for unattended workers); the tools stay honest.
- **Ownership & safety:** `session.Record` gains `ParentThreadID string` and
  `Role string` (`"controller" | "worker" | ""`). `send_agent`/`close_agent`
  outside the caller's own subtree require the human's approval once (reuse the
  permission prompt). `discard_agent` keeps its current self-discard refusal.
- The bridge's tool catalogue (`mcp.go:468-593`) advertises the new tools with
  honest descriptions of the backend vocabulary (the bridge learns its own
  thread's backend; cross-backend vocabulary is enumerated via `agent.list`).

## Feature 2 — Persona / system-prompt channel + custom subagent profiles

Two harness-native mechanisms, one neutral spec.

**`harness.StartSpec` gains `SystemPrompt string` and `Agents []AgentProfile`**
(`harness.go:96-112`), plus capability flags `SystemPrompt` and
`CustomSubagents` on `Capabilities` (`harness.go:33-65`).

```go
// AgentProfile is one custom subagent definition, harness-neutrally.
type AgentProfile struct {
    Name        string   `json:"name"`
    Description string   `json:"description"`
    Prompt      string   `json:"prompt"`            // body / system prompt
    Tools       []string `json:"tools,omitempty"`   // allowlist; empty = all
    Model       string   `json:"model,omitempty"`   // harness-specific id or ""
}
```

- **Claude adapter** (`harness_claude.go` / `agent.go:314-456`): `SystemPrompt`
  → `--append-system-prompt`; `Agents` → `--agents '<json>'` (verified flag).
  Both fully supported → capabilities true.
- **Kimi adapter** (`harness_kimi.go` / `core/internal/kimi/thread.go:260-412`):
  ACP accepts no launch flags, so: `Agents` → write each profile as a Markdown
  agent file into the thread worktree at `.agents/agents/<name>.md` (verified
  project-scope discovery; `model` maps to `model_preference` only as
  primary/secondary — concrete model ids are not expressible, so a non-empty
  `Model` is reported as *not applied*). `SystemPrompt` → **not supported**:
  capability false, `Launched` reports it unapplied, and the caller (Feature 3)
  folds the persona into the opening prompt instead — which works identically on
  both harnesses. (Alternative considered: per-thread `KIMI_CODE_HOME` with a
  `SYSTEM.md`. Rejected for personas — it replaces the *whole* main prompt,
  hiding the tool/skill injections; it returns in Feature 5 as an isolation
  lever, not a persona channel.)
- Both adapters report applied-truth through the existing `Launched` struct —
  the UI already displays downgrades (plan 15 pattern).

The ensemble master prompt (Feature 3) is delivered as the **opening user
message** on every harness — the one channel that is guaranteed identical
everywhere — with `SystemPrompt` as an optional capability-gated enhancement on
Claude.

## Feature 3 — User-defined ensembles ("modes")

An **ensemble** is a named, user-editable recipe: one controller + zero or more
worker roles + a master prompt. Shipped examples (all editable, all deletable,
none privileged):

- **Fable Controls Opus** — controller `claude/fable`; workers
  `claude/opus` (coder), `claude/sonnet` (scout).
- **Fable Controls Kimi K3** — controller `claude/fable`; worker
  `kimi/kimi-code/k3`.
- **Kimi K3 Controls K2.7 Code** — controller `kimi/kimi-code/k3`; workers
  `kimi/kimi-code/kimi-for-coding`, `kimi/kimi-code/kimi-for-coding-highspeed`.
- **Opus Controls Kimi Coders** — controller `claude/opus`; workers
  `kimi/kimi-for-coding` ×2.

**Store (core-side):** `modes.json` under `$XDG_DATA_HOME/agentkate/` — same
atomic-write JSON pattern as `threads.json` (`session.go:91-101`). Built-ins
ship as defaults merged on load (user entries win on name collision; a deleted
built-in is recorded as suppressed). New `core/internal/modes/` package; new
RPCs `mode.list` / `mode.get` / `mode.save` / `mode.delete` / `mode.apply`
registered in `core/cmd/akcore/main.go`.

```jsonc
{
  "name": "Kimi K3 Controls K2.7 Code",
  "controller": { "backend": "kimi", "model": "kimi-code/k3",
                  "permissionMode": "default", "isolation": "auto" },
  "workers": [
    { "role": "coder",  "backend": "kimi", "model": "kimi-code/kimi-for-coding",
      "permissionMode": "auto", "isolation": "workspace" }
  ],
  "masterPrompt": "You are the controller of an Agent Kate ensemble… {{worker_roster}} …"
}
```

**`mode.apply { name, workDir }`** does the mechanical part core-side: create
the controller thread with `Role: "controller"`, render the master-prompt
template (placeholders: `{{workspace}}`, `{{worker_roster}}` — a table of role →
`launch_agent` argument hints, `{{ensemble_name}}`), and send it as the opening
message. Workers are *not* pre-spawned — the controller launches them on demand
via `launch_agent`, using the roster as its menu. (Alternative considered:
eagerly spawn all workers. Rejected: burns tokens/worktrees for roles the task
may never need; lazy keeps the arena clean. The mode editor can mark a role
`prewarm: true` later if dogfooding wants it.)

The **master-prompt template** is the behavior heart; the shipped default tells
the controller: which ensemble it runs, that it orchestrates via
`mcp__cooperation__launch_agent` / `send_agent` / `wait_agent`, the exact
backend/model strings for each worker role, when to use `wait: true` vs
fire-and-forget, to keep the human informed via `post_note`, and to claim files
before editing. Written harness-neutrally (both Claude and Kimi controllers
receive the same text).

**UI:** a new **Ensemble picker** in `NewAgentDialog`
(`ui/src/NewAgentDialog.cpp:52-184`) and the roster quick menu
(`AgentDock::seedEngineChoices`, `AgentDock.cpp:661-693`) — "Ensembles" section
above the engine list; picking one calls `mode.apply`. A small **Ensemble
editor** dialog (name, controller engine/model via the existing pickers, worker
role rows, master-prompt QTextEdit with placeholder help) round-tripping
`mode.*` RPCs. Applied ensembles persist nothing UI-side beyond the existing
`[Agent]` stickiness.

## Feature 4 — Real-time MCP traffic in the UI

Three layers, each independently shippable.

**4a. Core broadcasts bridge activity.** Every request arriving on a bridge
connection (identifiable: spawned per thread with `--thread`) is wrapped by the
IPC server (`core/internal/ipc/server.go`) to emit an `mcp.activity`
notification: `{ threadId, server, tool, argsSummary, durationMs, ok, error? }`.
`argsSummary` is a small tool-aware digest (path, note text, target thread,
backend/model for `launch_agent`) — capped, never secrets. This is the
orchestration firehose; cheap because bridge traffic is human-scale.

**4b. Transcript rendering for `mcp__*` tools.** Add MCP branches to
`agentkate::permSummary` and `activityFor` (`AgentChatHelpers.cpp:49-116`):
`post_note` → the note text, `claim_file`/`release_file` → the path,
`launch_agent` → `"<backend>/<model>: <title>"`, `send_agent` → target +
first line, `list_agents`/`get_presence` → fixed short labels. Register
`mcp__cooperation__*` and `mcp__cowork__desktop_*` overviews in
`ToolInspectorDialog.cpp:228-288`. Give cooperation tool rows a distinct glyph
(⇄) in `TranscriptDelegate::layoutRow` (`TranscriptDelegate.cpp:471-640`) so
orchestration calls are visually distinct from file tools.

**4c. Cross-thread views.** 
- `AiInspectorPanel` gains a follow-mode toggle: *Active thread* (today) vs
  *All threads* — the latter renders the `mcp.activity` stream as one merged
  timeline (time, source thread chip, tool, summary, duration, ✓/✗). This is
  the "inspect in real time" surface.
- `session.Record.ParentThreadID`/`Role` (Feature 1) let the roster nest
  workers under their controller (`AgentRoster` + `AgentDock::Entry`), with a
  ⇄ badge on controllers and a ▸ count of live workers. `AgentCardDelegate`
  already renders badges/previews; add role tint.
- CooperationPanel adds a "Recent activity" section fed by the same
  notification (bounded list, 150 ms debounce like the existing sections).

## Feature 5 — Kimi first-class parity

Honest gates stay (no usage accounting, no fork — ACP simply doesn't offer
them). Fix what's *ours*:

- **Registry-honest compaction.** Move hot/cold compaction behind the `Harness`
  interface: add `Compact(threadID, strategy)` to the interface, implement it in
  the Claude adapter (today's logic), return the shared "not supported" error in
  the Kimi adapter (`Compaction` capability is already false there — the gate is
  honest). This removes the `d.sup` special-case from `handlerDeps`
  (`handlers.go:79-99`) and the direct `claude --resume` shell-out from the
  shutdown path (`compact/llm.go:42-60` becomes adapter-internal).
- **Kimi env overlay.** `kimi.Supervisor` spawns with plain `os.Environ()`
  today. Add a per-thread env map (StartSpec `Env map[string]string`,
  neutral): enables `KIMI_CODE_HOME=<worktree>/.agentkate/kimi-home` for
  session/home isolation per thread, and
  `KIMI_CODE_EXPERIMENTAL_SECONDARY_MODEL=1` when an ensemble uses
  `model_preference: secondary` worker profiles.
- **Kimi subagent transcript viewing.** Extend the subagent-transcript path
  (plan 14's `SubAgentTranscriptDialog`, driven from
  `AgentPanel::noteWorkflowLaunch`) to kimi's `<session-dir>/agents/<id>/wire.jsonl`
  (verified on-disk format): the kimi translator (`translate.go`) emits the
  subagent spawn as a recognizable tool row, and the dialog live-tails the wire
  file with the same chat rendering. Claude keeps its `agent-<id>.jsonl` path —
  one dialog, two adapters behind a harness capability `SubagentTranscripts`.
- **Kimi custom agents from the UI.** The discovered-agents vocabulary
  (project + user `.agents/agents/`, `.kimi-code/agents/`) can be surfaced as
  "subagent profiles" in the ensemble editor's worker rows (read-only listing via
  a `kimi`-side probe, mirroring `DiscoverOptions`). Main-agent `--agent`
  selection remains unavailable over ACP — documented, not emulated.
- **Skills for kimi threads.** The skills installer currently symlinks into
  `<target>/.claude/skills/` (`core/internal/skills`) — Claude-shaped. Extend to
  also link `.agents/skills/` (kimi's cross-tool discovery dir, same convention
  as `.agents/agents/`) so both harnesses see the same skill catalog.

## Feature 6 — Claude feature sweep (expose what 2.1.220 already ships)

Per-thread, via the neutral `StartSpec` / option plumbing (all capability-gated,
static-true for Claude, false/absent for Kimi unless noted):

- `--append-system-prompt` and `--agents` — Feature 2 (the big two).
- `--fallback-model` — new optional `FallbackModels []string` on StartSpec;
  small UI field in the model row's advanced popover (print-mode-only flag —
  ours is exactly print mode).
- `--disallowedTools` — new `DisallowedTools []string`; advanced per-thread
  option; kimi maps it onto agent-file frontmatter when subagent profiles are
  written (main-agent level: honest no-op, reported).
- `--add-dir` — additional workspace dirs per thread (`AddDirs []string`); kimi
  ACP has no equivalent → capability-gated off.
- Sweep but deliberately **skip** (documented in Non-goals): `--bg` (our threads
  are the background system), `--chrome`, `--bare`, `--betas`, `--brief`.

## Phases

- **P1 — Orchestration MCP tools (Feature 1). ✅ LANDED.** Core-only:
  `agent.wait` + `agent.launchWorker` RPCs, four bridge tools
  (`launch_agent`/`send_agent`/`wait_agent`/`close_agent`),
  `ParentThreadID`/`Role` on `session.Record`, permission reuse.
  `scripts/smoke-orchestrate.py` green in both directions live (claude/haiku
  controller → kimi worker "KIMIPONG"; kimi/k3 controller → claude worker
  "CLAUDEPONG"; parent linkage + roles asserted via `agent.list`).
  Where the sketch met the real code:
  - **`agent.wait` is NOT a condvar on the claude thread's turn state** as
    sketched — that state (`Thread.turnsInFlight`) is claude-only and kimi's
    `activePrompts` is private to its package, yet wait must cover kimi
    workers too. Instead a backend-agnostic `agent.TurnTracker`
    (`core/internal/agent/turnwait.go`) mirrors turns at the orchestration
    layer: handlers mark a turn queued (agent.start pre-queues the opening
    prompt BEFORE the async launch so a wait racing the start never sees a
    false idle; agent.send queues before writing), the relay feeds every
    event back in (`result` ends a turn; terminal `_lifecycle` ends the
    thread), and `emitLifecycle` mirrors the orchestration-layer phases that
    never cross the relay (launch "error" would otherwise strand waiters).
    Waiters block on a replace-on-change broadcast channel — the Go condvar
    idiom that supports a timeout (`sync.Cond` cannot) — no polling.
  - **`lastText` is captured by the tracker, not a transcript tail**: claude
    `result` events carry the turn's final text verbatim; kimi's carry none,
    so the last non-empty assistant text event stands in (the kimi
    translator flushes a turn's trailing text as one event).
  - **`launch_agent` is a synchronous `agent.launchWorker` RPC**, a refactor
    of `startThread` into a shared `launchThread` — the async reply-first
    contract of `agent.start` exists for the UI's event ordering and is
    irrelevant to the bridge, and only a synchronous launch can put
    applied-truth (`Launched` + an `unapplied` diff of requested vs applied)
    in the tool result instead of a promise. Registration lives in
    `registerOrchestrationHandlers` (`cmd/akcore/orchestrate.go`), wired from
    `registerHandlers` — handlers.go, not main.go as sketched.
  - **Workers root at the parent's `Project`, resolved core-side from the
    parent record** — the bridge's `--workspace` flag is the caller's
    *worktree*, and rooting there would nest worktrees. Sibling worktrees,
    real roster threads, existing archive semantics (`close_agent` =
    `agent.stopClose` + `fromThreadId`).
  - **Empty `backend` resolves to the CALLER's harness**, from the parent
    record via the registry — letting it fall to the registry default would
    have silently handed a kimi controller claude workers.
  - **Cross-subtree control** (`send_agent`/`close_agent` on threads not
    transitively parented under the caller) asks the human once per
    (caller, target, action) — an in-memory grant cache behind the existing
    `askHumanPermission` flow. `close_agent` refuses self-close on both
    sides, mirroring `discard_agent`'s self-discard refusal.
  - `agent.list` rows now carry `parentThreadId`/`role` (P5's roster-nesting
    feed) and the bridge's `list_agents` prints the linkage. No new
    capability flags in P1 — the four planned flags arrive with P3/P5/P6;
    `harness_caps_test.go` unchanged and still frozen.
  - `launch_agent wait:true` blocks the controller's MCP tool call for the
    whole worker launch + first turn; verified live on BOTH CLIs (precedent:
    `request_permission` already blocks for minutes).
  - Incidental fixes while getting "smokes still green": `smoke-agent.py`
    predated the coalesced-batch `agent.event` shape and crashed on
    `params["event"]` (pre-existing at HEAD — now batch-aware like
    smoke-kimi); `smoke-kimi.py` now pins `permissionMode: "default"`
    because this machine's `~/.kimi-code/config.toml` sets
    `default_permission_mode = "auto"`, which auto-approves and bypassed the
    very permission bridge the smoke proves.

  **P1 remediation** (post-landing review of c3c73fc):
  - `discard_agent` closed the approval hole: the bridge now sends
    `fromThreadId` and the `agent.discard` handler runs
    `authorizeAgentTarget` before anything else — cross-subtree discards need
    the same one-time human approval as send/close; UI discards stay ungated.
  - Honest wait contract: the "exited" message no longer claims `send_agent`
    resumes a dormant thread (both backends reject sends to dead processes);
    it now says the human / `agent.resume` must bring it back.
  - Untracked-turn gaps: the seeded-resume summary prompt and the hot-compact
    prompt (both `runHotCompactIfConfigured` and `agent.compactNow`'s hot
    path) now `TurnQueued` before sending, so `wait_agent` cannot see a false
    idle during those turns.
  - Self-targeting refused bridge-side for `send_agent` / `wait_agent` /
    `close_agent` (a self send/wait would park the caller's own turn until
    timeout by construction).
  - Hygiene: approval grants are pruned when a thread is discarded (and the
    catalogue says grants last for the current core run); malformed tool
    arguments surface the JSON error instead of a misleading required-field
    message; `agent.stopClose` forgets the thread in the turn tracker;
    `agent.wait` selects on the request context so a disconnected bridge
    releases its waiter; the `launch_agent` catalogue no longer hardcodes
    engine names.
  - `docs/security-model.md` §1 now notes `fromThreadId` is self-asserted —
    the gate is a guardrail inside the localhost trust model, not
    authentication.
  - New tests: approval gate semantics (approve-once, deny-not-cached,
    per-action grants, prune), discard-through-the-gate over the full handler
    set, `launchWorker` applied-truth via a fake registered harness, tracker
    interrupt/multi-turn/ctx-cancel/seeded-resume cases, bridge self-refusals
    and malformed-args.
- **P2 — MCP traffic core + transcript rendering (4a + 4b)**. `mcp.activity`
  notifications; `permSummary`/`activityFor`/ToolInspector/delegate glyph.
  Immediately useful for the existing cooperation tools.
- **P3 — Persona channel + custom subagents (Feature 2)**. `StartSpec`
  extensions, claude `--append-system-prompt`/`--agents`, kimi agent-file
  writer, capability flags, applied-truth reporting.
- **P4 — Ensembles (Feature 3)**. `core/internal/modes`, `mode.*` RPCs, built-in
  ensembles, master-prompt template, `NewAgentDialog`/quick-menu picker,
  ensemble editor dialog.
- **P5 — Cross-thread UI (4c)**. All-threads inspector timeline, roster
  controller/worker nesting, cooperation activity feed.
- **P6 — Parity & feature sweep (Features 5 + 6)**. Compaction behind the
  interface, kimi env overlay + wire transcripts + skills dirs, Claude flag
  sweep. Independently sliceable; land in any order after P1.

Sequencing rationale: P1 is the keystone and UI-free; P2 makes it observable
before P4 builds automation on top; P3 is P4's persona plumbing; P5/P6 are
parallel-friendly once P1's record fields exist.

## Non-goals

- No new harnesses (Codex/OpenCode remain separate, future work).
- No orchestration *intelligence* core-side (no scheduler, no DAG engine) — the
  controller model is the orchestrator; we ship tools + prompts + visibility.
- No emulated kimi features (usage, fork, main-agent selection over ACP,
  concrete-model subagent routing). Gates stay honest per HARNESSES.md.
- No claude `--bg`/`--chrome`/`--bare`/`--betas`/`--brief` plumbing; no kimi
  hooks/plugins/`kimi web` integration.
- No changes to the stream-json event contract beyond additive notifications
  (`mcp.activity`); translators unchanged except Feature 5's subagent row.

## Conventions (must hold)

- No `backend == "…"` string compares outside the adapter/supervisor and
  `HarnessTraits` — new capabilities (`SystemPrompt`, `CustomSubagents`,
  `SubagentTranscripts`, `AddDirs`) gate everything new.
- `Launch`/`launch_agent` report **applied-truth**; unapplied requests surface
  in the UI as downgrades (plan 15 pattern), never silently.
- Workers are real threads: worktree rules, archive semantics, and permission
  prompts behave exactly as for human-launched agents.
- Bridge stays a thin stdio↔IPC client; all orchestration state lives core-side.
- Plan-doc / review conventions per `docs/plans/README.md`; HARNESSES.md and
  ARCHITECTURE.md (Cooperation MCP table) updated in the phases that change
  them.

## Verify

- `cd core && go test ./...` (new: bridge tool handlers, `agent.wait`, modes
  store round-trip, kimi agent-file writer, capability matrix in
  `harness_caps_test.go`).
- `cmake --build build && ctest --test-dir build` (new: TranscriptModelTest
  cases for MCP summaries; ensemble editor dialog test).
- `python3 scripts/smoke-orchestrate.py` (P1, both directions), plus existing
  `smoke-agent.py` / `smoke-kimi.py` / `smoke-fork.py` stay green.
- Manual dogfood: apply "Kimi K3 Controls K2.7 Code", watch the all-threads
  inspector show `launch_agent` → worker thread → `wait_agent` result; take
  over a worker mid-run; close the ensemble via `close_agent`.
