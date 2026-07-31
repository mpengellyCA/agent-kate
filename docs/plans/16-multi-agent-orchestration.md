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
- ~~**Custom agents are discovered from directories**, which works regardless of
  launch flags~~ — **CORRECTED IN P3, see that phase's retrospective.** The
  directories are real (`<project>/.agents/agents/*.md`,
  `<project>/.kimi-code/agents/*.md`, `~/.agents/agents/`,
  `$KIMI_CODE_HOME/agents/`) and the file format is as documented (YAML
  frontmatter `name`, `description`, `tools`, `disallowedTools`, `subagents`,
  `model_preference` + a Markdown body that *is* the system prompt), but they
  are read ONLY by kimi's **v2 engine**, which `kimi acp` never runs. Over ACP
  the subagent set is a compiled-in table. Directory discovery therefore does
  NOT work "regardless of launch flags"; it works only under `kimi -p` with
  `KIMI_CODE_EXPERIMENTAL_FLAG=1`.
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
  ~~ACP accepts no launch flags, so: `Agents` → write each profile as a Markdown
  agent file into the thread worktree at `.agents/agents/<name>.md` (verified
  project-scope discovery; `model` maps to `model_preference` only as
  primary/secondary — concrete model ids are not expressible, so a non-empty
  `Model` is reported as *not applied*).~~ **CORRECTED IN P3:** `kimi acp` runs
  the v1 engine, which resolves subagents from a compiled-in table and reads no
  agent-file directory at all, so those files would be dead on arrival —
  `CustomSubagents` is **false** and every profile is reported unapplied. See
  P3's retrospective for the probe. `SystemPrompt` → **not supported**:
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
- ~~**Kimi custom agents from the UI.** The discovered-agents vocabulary
  (project + user `.agents/agents/`, `.kimi-code/agents/`) can be surfaced as
  "subagent profiles" in the ensemble editor's worker rows.~~ **DROPPED by
  P3's probe:** those directories feed kimi's v2 engine only, so an ACP thread
  can use none of them — listing them in the editor would offer the user
  profiles their agent cannot reach. Kimi's usable subagent vocabulary over
  ACP is exactly `coder` / `explore` / `plan`. Main-agent `--agent` selection
  likewise remains unavailable over ACP — documented, not emulated.
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
- **P2 — MCP traffic core + transcript rendering (4a + 4b). ✅ LANDED.**
  `mcp.activity` notifications broadcast from the IPC dispatch;
  `permSummary`/`activityFor` MCP branches, ToolInspector overview entries and
  the ⇄ delegate glyph. Immediately useful for the existing cooperation tools.
  Where the sketch met the real code:
  - **Bridge connections were NOT identifiable.** The sketch assumed a bridge
    conn is recognisable because it is "spawned per thread with `--thread`",
    but the only thing that ever tagged one was the Cowork keystone's
    `BindBridge`, called lazily from `requireCoworkBridge` — a Cooperation
    bridge's connection carried role `""` for its whole life. P2 adds a
    one-call `bridge.identify` handshake at bridge startup (both catalogues),
    so `conn.role`/`connTID` are set before the first tool can run. The feed's
    `threadId` is therefore the *connection's bound* thread, never a
    self-asserted param — and Cowork's own lazy bind still agrees with it.
  - **The feed is UI-only**: a new `ipc.Server.NotifyUI` sends to connections
    that identified as the UI, not the existing broadcast `Notify` (which
    reaches bridges too). One agent's activity must not land on another
    agent's bridge — that is both an information-leak and a prompt-injection
    surface, and bridges have no use for notifications anyway.
  - **The map is keyed by RPC method, not by tool name.** Both harnesses'
    bridges make the identical calls, so keying on the method is what makes
    the feed harness-agnostic by construction (no `backend == "…"` anywhere).
    `TestMCPToolMapCoversBridgeCallSites` scrapes `mcp.go`/`mcp_cowork.go` for
    every `client.Call*` literal and fails in BOTH directions (an unmapped
    call site, a map entry nobody calls), plus asserts every mapped name is a
    tool the catalogues actually advertise. Unknown methods still report under
    their raw name — a new RPC can never make traffic vanish.
  - **`argsSummary` is a redaction boundary, not just a length cap.**
    `request_permission` names the gated tool but never its input (a Bash
    command line is exactly where a token lives); `desktop_set_text` names the
    element but never the text (it may be a password); `launch_agent` shows
    `<backend>/<model>: <title>` but never the briefing; message-like fields
    keep only their first line; unmapped methods get an EMPTY summary rather
    than a params dump. Everything is capped at 120 bytes on a rune boundary.
  - Dropped the sketch's `server` field: the tool name already says which
    catalogue it came from (`desktop_*` vs the rest), so it was pure
    duplication in every row.
  - `list_agents` activity includes the bridge's own internal `agent.list`
    lookups (`discard_agent` does one before discarding). That is real RPC
    traffic on the wire and is shown honestly rather than filtered.
  - **The UI needed no notification plumbing**: `CoreClient::notification` is
    a generic signal, so `mcp.activity` already flows to any subscriber. No
    panel consumes it yet (the cross-thread timeline is P5), so the pipe is
    proved end-to-end in `scripts/smoke-orchestrate.py` instead — which had to
    start sending `handshake`, since the feed is UI-only.
  - The ⇄ glyph is `mcp__cooperation__*` only; Cowork rows keep the 🔧 wrench,
    since driving the desktop is tool use rather than agent-to-agent traffic.
  - Wiring `permSummary` into `TranscriptModelTest` pulled
    `AgentChatHelpers` → `HarnessTraits` → `ProviderConfig`/`CoreClient` into
    that target (its `modelAvailable` helpers need the registry); the test
    target gained those sources and `Qt6::Network`.
  - Incidental fix while getting the suite green (folded into the P1
    remediation commit, since it repairs a P1 test): `permAutoResponder`
    (P1's stand-in for the human) raced the server's `accept` —
    `permission.requested` is a fire-and-forget broadcast to *registered*
    connections, so an ask fired microseconds after the responder's `Dial`
    reached nobody and `TestAuthorizeAgentTarget` hung for the full 8-minute
    permission timeout. It now does one request/reply round trip before
    returning. Reproduced once under load, green over repeated runs since.

  **P2 remediation** (post-landing review of b63f3e3):
  - **The UI leaked what the core redacted.** `request_permission` rows had no
    `permSummary` branch, so they fell through to the compact-JSON dump — and
    that input is the *gated tool's raw arguments*, the most secret-bearing
    payload in the catalogue. The row now names the gated tool only (both
    `tool_name` and `toolName`, the spellings the bridge accepts) and never
    falls through, even with no name present. Core-side redaction is only half
    a boundary if the transcript prints the same payload two rows above.
  - **Role is now one-way in both directions.** `MarkUI` set role `"ui"`
    unconditionally, so a bridge could call `handshake`, pass `RequireUI`
    (answering its own grant prompts) and join the UI-only `mcp.activity`
    feed — the exact inversion `BindBridge` refuses. It now returns with a
    warning for a bridge connection, handing back the primary-UI slot if it
    had optimistically claimed it (`s.mu` and `idMu` are never held together,
    since `NotifyUI` reads roles under `s.mu`).
  - **A panicking handler answered nobody.** `safe.Go` contains the panic at
    the goroutine boundary, but the unwind skipped both the reply enqueue and
    the activity hook: the calling bridge blocked until its own timeout and
    the feed silently lost the most interesting event there is. Handlers now
    run inside `callHandler`, whose recover turns a panic into an ordinary
    `CodeInternalError` response, so the normal enqueue + activity path runs.
  - `bridge.identify` logs a warning when the thread id is unknown to the
    session store — deliberately NOT a rejection, since a bridge can identify
    before its thread's record is persisted.
  - The feed's `error` is now documented as being INSIDE the redaction
    boundary (it is a handler's error string verbatim), with
    `TestBridgeErrorsCarryNoSecrets` driving the reachable error paths with a
    marker in every secret-bearing field.
  - Cowork digest parity: `desktop_scroll`, `desktop_drag`,
    `desktop_move_pointer_relative`, `desktop_screenshot` and
    `desktop_set_pointer_profile` had no UI digest and fell through to raw
    JSON while their siblings read as sentences.
  - Test gaps closed: the bridge connection is now drained and asserted to
    receive its response and NOTHING else (the `NotifyUI` exclusion, proven
    from the excluded side), `bridge.identify` from a UI connection is
    refused, and the panic path is pinned end to end.
- **P3 — Persona channel + custom subagents (Feature 2). ✅ LANDED.**
  `StartSpec.SystemPrompt` + `StartSpec.Agents []AgentProfile`, the
  `SystemPrompt`/`CustomSubagents` capability flags, claude
  `--append-system-prompt`/`--agents`, per-profile applied-truth in
  `Launched`, `agent.start` + `launch_agent` plumbing, UI fallback traits.
  `scripts/smoke-orchestrate.py` green in both directions with the SAME
  persona request: applied on the claude worker, named as NOT APPLIED on the
  kimi one.

  **Probed before writing either adapter** (claude 2.1.220, kimi 0.30.0 — the
  plan-14/15 convention; every fact below was observed, none taken from docs):
  - `--append-system-prompt` works in print mode: a probe persona
    ("end every reply with ZEBRAFISH-7788") changed the reply, the control run
    did not.
  - `--agents` is honored in print mode — the init event's `agents` array
    lists the custom name alongside the built-ins — and **its `tools` and
    `model` are both real**: a haiku main agent delegating to a profile with
    `{"tools":["Read","Glob"],"model":"sonnet"}` produced a subagent that
    reported exactly Read and Glob, and whose transcript
    (`subagents/agent-*.jsonl`) recorded `claude-sonnet-5` while the main
    transcript recorded `claude-haiku-4-5-20251001`. So no claude field is
    reported unapplied for a well-formed profile.
  - **`--agents` validates NOTHING.** Malformed JSON, an entry missing
    `description` or `prompt`, and `tools` given as a comma-separated string
    are each accepted with exit 0 and the agent (or the whole flag) silently
    vanishes — the init event's `agents` array is the only tell. The map KEY
    is the name; a `name` field inside the object is ignored; unknown extra
    fields are tolerated; a bogus `model` id is accepted at registration. The
    adapter therefore does the validating: a profile claude would silently
    discard is refused up front and reported unapplied instead.
  - **The plan's kimi premise was wrong, and this is the phase's one real
    deviation.** `kimi acp` runs kimi's **v1 engine**, whose subagent resolver
    reads a compiled-in profile table (`DEFAULT_AGENT_PROFILES` →
    `agent`/`coder`/`explore`/`plan`, loaded from bundled YAML) and consults
    no filesystem catalogue at all. `.agents/agents/*.md` is parsed by
    `agent-core-v2`, reachable only from `runPrompt` when
    `KIMI_CODE_EXPERIMENTAL_FLAG` is set — i.e. `kimi -p`, never `kimi acp`.
    Live proof, with the agent file present in a git **worktree** (`.git` a
    file, the case that mattered): an ACP `session/prompt` delegation returned
    `subagent error: Subagent profile "akprobe" was not found` — with the ACP
    child's process cwd inside the worktree, and again with
    `KIMI_CODE_EXPERIMENTAL_FLAG=1` in its env — while
    `KIMI_CODE_EXPERIMENTAL_FLAG=1 kimi --agent akprobe -p` found that very
    file (and `--agent nosuchagent` enumerated `plan, agent, coder, explore,
    akprobe`, from `/tmp` only the four built-ins). **So the planned kimi
    agent-file writer was NOT built**: it would have written files into the
    user's worktree that the running agent provably cannot read, which is
    silent emulation. `CustomSubagents` is false for kimi, and every requested
    profile is reported unapplied with the reason. (The v2 frontmatter parser,
    read out of the binary, also pins the format for whenever ACP does reach
    v2: `description` required, name kebab-case and defaulted from the
    filename, `tools` a list *or* a comma string, `model_preference` strictly
    `primary`/`secondary` — a concrete model id is a parse ERROR, not a
    downgrade, which is why the plan's "map Model to model_preference" idea
    was never expressible anyway.)

  Where the sketch met the real code:
  - **`Launched` reports per-profile truth, not one flag.** A harness can lose
    a whole profile (kimi) or one field of it, so `Launched.Agents` carries one
    `AppliedAgent{Name, Applied, Unapplied []string}` per REQUESTED profile, in
    request order. `harness.UnappliedAgents(profiles, reason)` is the one-liner
    for a capability-false adapter. `unappliedPersona` (orchestrate.go) turns
    that into the same `unapplied` list P1 already rendered, plus a
    **backstop**: any requested profile the adapter reported nothing about is
    named anyway, so a future adapter that ignores `spec.Agents` cannot make
    the request disappear.
  - **The claude argv builder moved out of `Supervisor.Start`.** The flags
    were untestable without spawning the CLI; `buildStartArgs(opts)` is now a
    pure function (`internal/agent/startargs_test.go` pins the two new flags,
    that `--system-prompt` is never used — it would REPLACE claude's own
    prompt and hide the tool/skill injections — and the resume/fork/cowork
    argv that came along for the ride).
  - **The `--agents` JSON is built in the adapter, not the supervisor.**
    `harness_claude.go` owns the CLI's vocabulary (which fields exist, which
    are required), and `agent.StartOptions.AgentsJSON` is the rendered
    payload. That keeps the "what claude accepts" knowledge in the same file
    as the capability that claims it.
  - **One wording for capability gates, two exits.** `unsupported()` (the hard
    RPC error) and the persona reports now share `unsupportedDetail()`, so a
    downgrade and a refusal describe a missing capability identically.
  - The bridge passes both channels through **verbatim** as
    `harness.AgentProfile` — the bridge is the same package as the core, so
    the neutral shape is shared rather than re-declared, and `launch_agent`'s
    schema advertises `system_prompt` + `agents` with `name`/`description`/
    `prompt` required. Its result gained `System prompt: …` /
    `Subagent profiles available to the worker: …` lines, and `NOT APPLIED:
    <option> — <reason>.` for entries that carry a reason instead of a
    downgraded value.
  - `kimi -p` + `KIMI_CODE_EXPERIMENTAL_FLAG=1` is NOT a workaround worth
    taking: it is a different protocol (v1 event shapes, no ACP session
    handshake, no `session/set_config_option`), i.e. a second kimi harness,
    not a P3 flag. Left for a future plan if the v2 engine ever reaches ACP.
  - No UI beyond the two fallback traits, as scoped — P4 gates on them.

  **P3 remediation** (post-landing review of 0a4c311):
  - **The persona was silently lost on every relaunch.** `session.Record`
    stored nothing about it, so `resumeThread` and `forkAgentThread` (and
    promote, which ends in `resumeThread`) rebuilt the launch without it — the
    human stopped one agent and resumed a different one, and P4's ensembles
    would have inherited that. The record now carries `SystemPrompt` +
    `Agents` (both `omitempty`, so pre-P3 records are untouched and resume
    exactly as before), and all three paths re-pass them. What is stored is
    what `Launched` **confirmed applied**, never the request
    (`appliedPersona`, agents.go): a kimi thread applied nothing, so it
    persists nothing and keeps reporting nothing on every later resume. A
    profile applied with per-field losses is stored as requested, so the
    resume re-runs the identical translation and lands in the identical place.
  - **Argv size guard.** Each persona flag is ONE argv element, capped by the
    kernel at `MAX_ARG_STRLEN` (128 KiB on Linux); an oversize system prompt
    or `--agents` payload would have failed the spawn with an opaque `E2BIG`
    that looks nothing like "your prompt is too long". The claude adapter now
    measures both before the spawn and drops an oversize one with a reason
    naming the limit. Since the `--agents` flag carries every profile at once,
    an oversize payload refuses ALL of them — reported per profile.
  - `Launched` gained `SystemPromptUnapplied`, so an adapter that knows *why*
    (oversize, not missing) is not overwritten by the shared capability
    wording. Same fix for the per-profile fallback, which claimed "not
    supported by Claude Code agents" for a verdict with no reason attached —
    now "not applied; the harness gave no reason".
  - One emptiness rule for the system prompt: `strings.TrimSpace` in the
    adapter, `buildStartArgs` and `unappliedPersona` alike (an empty
    `--append-system-prompt` still reads as a custom prompt to the CLI).
  - `AgentProfile.Tools` documents that tool names pass through unvalidated,
    matching both CLIs (neither rejects an unknown name; it simply grants
    nothing). Validating would mean a per-harness tool catalogue that goes
    stale every CLI release.
  - `docs/security-model.md` §1 notes that persona text travels as argv and is
    persisted in cleartext — same-uid readable, so it is instructions, not a
    secrets channel (credentials stay env-only, §3).
  - New tests: resume/fork/promote replay (a real `resumeThread` and
    `forkAgentThread` over a fake harness, plus the pre-P3 record that must
    stay empty), `appliedPersona` narrowing, an `agent.launchWorker` record
    round-trip, the on-disk session shape, and the argv-limit boundaries
    (at the limit passes, one byte over refuses).
- **P4 — Ensembles (Feature 3). ✅ LANDED.** `core/internal/modes` (built-ins
  merged with the user's `modes.json`, user-wins + suppression), the five
  `mode.*` RPCs, the master-prompt template, the New Agent dialog's ensemble
  picker, the roster quick menu's Ensembles section, and the ensemble editor.
  `scripts/smoke-orchestrate.py` gained a third leg (C) that applies an
  ensemble and lets its rendered briefing — nothing else — drive a real
  cross-thread launch; 43/43 checks green.

  **Probed before writing the built-ins** (the plan's model strings were
  partly wrong, and a shipped ensemble naming a model no CLI knows is a
  silent failure at first use):
  - `claude -p /model` on 2.1.220 →
    `sonnet, opus, haiku, fable, best, sonnet[1m], opus[1m], fable[1m],
    opusplan, default, or a full model ID`. So `fable`/`opus`/`sonnet` are
    real vocabulary, and `resolveModel` is now a pass-through (the tier→id map
    died with live model discovery) — the CLI resolves the alias itself.
  - kimi 0.30.0's ACP config-option enumeration (a live `DiscoverOptions`
    probe, no tokens spent) → models `kimi-code/kimi-for-coding` ("K2.7
    Coding"), `kimi-code/kimi-for-coding-highspeed`, `kimi-code/k3`,
    `kimi-code/k3-256k`; thinking `low|high|max`; modes
    `default|plan|auto|yolo`. The plan's `kimi/kimi-for-coding` was missing
    the `kimi-code/` prefix and would not have launched — the shipped
    "Opus Controls Kimi Coders" uses the real id, and
    `TestBuiltInsCarryRealVocabulary` now fails if a built-in ever drifts off
    a probed id.

  Where the sketch met the real code:
  - **The controller is born a controller.** P1 derived `Role` from
    "has a parent" (`worker`) / "has launched someone" (`controller`), but a
    mode.apply controller has neither at birth. `launchMeta` gained an explicit
    `Role`, so the roster nesting P5 builds on is correct from the first event
    rather than after the first `launch_agent`.
  - **The master prompt goes through BOTH channels where they exist.** The
    opening message is the harness-neutral path (byte-identical text on kimi
    and claude), and where `Capabilities().SystemPrompt` is true the same
    rendered text is ALSO pinned as the system prompt — the orchestration
    rules then survive the opening message ageing out of a long run. That is
    reported, not assumed: `mode.apply` returns `systemPromptApplied` plus the
    same `unapplied` list `launch_agent` uses, and the UI prints the losses in
    the controller's first system line.
  - **The one thing the ensemble layer reads from a harness is its permission
    vocabulary**, positionally: `PermissionModes`'s LAST entry is offered to
    the controller as the "run this worker unattended" mode. That keeps the
    permission guidance in each engine's own spelling with no `backend == "…"`
    compare, and a discovered-vocabulary harness (kimi) contributes no hint
    rather than a guessed one (`TestNoPermissionHintWithoutVocabulary`).
    Documented in HARNESSES.md's sharp edges, since it constrains how a new
    adapter orders its list.
  - **Engine ids in the built-ins are data, not gates.** `"claude"` / `"kimi"`
    appear in `builtin.go` the same way they would in a user's saved ensemble:
    as registry keys, resolved through `Registry.Get` and failing loudly when
    absent (`TestModeApplyValidation`). No behaviour branches on them.
  - **`mode.apply` takes an optional `task`** (appended under a "## The task"
    heading), which the plan did not have. Without it an applied ensemble opens
    with a briefing and no job, so the controller's first act is to ask what to
    do — one wasted turn on every launch. The New Agent dialog's task box feeds
    it.
  - **Validation stops at what apply needs**: a name, and a role name per
    worker. Model ids and permission modes are deliberately NOT checked
    against a list — that vocabulary belongs to the harness and changes with
    every CLI release, so a stale allow-list would reject models that started
    working today (`TestValidateRejectsUnusableEnsembles` pins both halves).
  - **UI: a bug the editor would have shipped.** An editable model combo
    reports a stale `currentIndex` when its edit text was set to an id the
    local catalogue has never seen — so saving an ensemble written on another
    machine would have silently cleared its model. `EnsembleDialog::modelIdFor`
    is the fix and is public+static purely so `ui/tests/EnsembleDialogTest`
    can pin it (mutation-checked: reading the index alone fails the test).
    Switching a row's engine now also clears its model, since a model id
    belongs to exactly one engine's vocabulary.
  - `EnsembleCatalog` (`ui/src/state/`) mirrors `mode.list` for all three
    surfaces, in the shape of the existing `HarnessRegistry` singleton; an
    older core answers with an error and the UI simply offers no ensembles
    (the New Agent dialog hides the row entirely rather than showing a picker
    with one option).
  - `AgentPanel::adoptRunningThread` was fork-specific (it replays the source
    transcript and prints "forked from …"). Split into a shared
    `bindStartedThread` plus a new `adoptStartedThread` for a thread with no
    inherited conversation — the ensemble controller.
  - **The master prompt's tool names are right on both engines**, which was not
    obvious: the smoke's kimi controller emits `tool_use:
    mcp__cooperation__launch_agent` — the same name the claude controller does,
    because the kimi translator normalises it — so one harness-neutral prompt
    naming `mcp__cooperation__*` is correct for every engine.
  - Not gated any further than `agent.start` is: an agent bridge could in
    principle call `mode.apply` over the bus, exactly as it could call
    `agent.start`. That is the pre-existing localhost/same-uid model
    (`docs/security-model.md` §1), not a new surface — the cross-subtree
    approval gate exists for *controlling other threads*, which mode.apply
    does not do.
- **P5 — Cross-thread UI (4c). ✅ LANDED.** The AI Inspector's follow-mode
  toggle with its merged all-threads timeline, roster nesting of workers under
  their controller (⇄ badge + live-worker count, orphans re-homed), and the
  Cooperation panel's Recent activity strip. `ui/tests/AgentRosterTest`
  (7 cases) guards the tree; a live offscreen run of the real app driving a
  real ensemble is the end-to-end evidence.

  Where the sketch met the real code:
  - **Agent-launched workers were never in the roster at all.** The plan
    assumed nesting was a presentation problem, but the UI only ever learned
    about threads through `session.listThreads` at project-open — a worker a
    controller launched mid-run appeared nowhere until the next restart. P5
    adds `AgentDock::refreshOrchestrationLinks`, which adopts worker records
    into panels (binding a *running* one live via P4's `adoptStartedThread`,
    so it is not offered a pointless Resume) and then nests + badges them. It
    runs on a project's restore and on the `mcp.activity` feed for exactly
    `launch_agent` / `close_agent` / `discard_agent` — 250 ms behind the tool,
    so the record it reads is already written. Without this the whole feature
    would have rendered an empty tree.
  - **Nesting broke every roster traversal.** The tree had been project→agent
    everywhere: the working animation, the text/tag filter, the tag menu, the
    attention roll-up and `agentItem` all walked `project->child(j)`, and
    `selectedProject()` took exactly one step up. They now go through
    `agentRows()` (depth-first) and `projectOf()`.
  - **Two ways a nested agent could be lost, both found by writing the tests:**
    closing a controller deleted its workers' ROWS as tree children while the
    worker threads kept running (they are separate agents — `removeAgent` now
    re-homes them onto the project first), and a filter match on a nested
    worker was invisible because Qt hides a row whose parent is hidden (the
    filter now un-hides the controllers above every visible row). Both are
    mutation-checked; so is the cycle guard in `setAgentParent`, whose absence
    makes the test *hang* rather than fail.
  - **The all-threads timeline is a separate bounded model**, as the plan
    required: a 500-row ring in its own `QTreeWidget`, collected even while the
    per-thread view is showing (switching modes has history, not an empty
    pane). It shows time, source agent, tool, digest and duration, and names
    the agent from the roster's own titles (`agentTitlesChanged`, already
    emitted for the WorktreeDashboard) rather than a bare thread id.
  - **Redaction boundary unchanged**: both new views render the core's
    `argsSummary` verbatim and add no new field to the feed. A failed call
    shows the feed's `error`, which P2's remediation documented as inside the
    boundary (`TestBridgeErrorsCarryNoSecrets`) — a failed launch reading as a
    blank row would be worse.
  - The Cooperation panel's strip reuses its existing 150 ms debounce (it now
    also fires for `mcp.activity`), and is deliberately short (25 rows,
    newest first): it answers "what just happened", while the inspector is the
    timeline.
  - **Live evidence**: the real `agentkate` binary, run offscreen against a
    throwaway XDG home, applied an ensemble whose controller launched a worker;
    the worker was adopted (`role: worker`, parented to the controller), the UI
    stayed alive, and its log contained **zero** Qt warnings/asserts. The
    roster's rendering itself is not observable from outside the process — that
    is what `AgentRosterTest` covers.
- **P6 — Parity & feature sweep (Features 5 + 6). ✅ LANDED**, as five commits
  in the order (a) compaction → (b) env overlay → (d) skills → (e) claude
  sweep → (c) subagent transcripts. Every claim below was probed against the
  real binaries first; three of the five probes changed what got built.

  **(a) Registry-honest compaction.** `Compact(ctx, CompactSpec)` is a Harness
  method; one spec covers both mechanisms (`Hot` = an in-session turn, cold =
  a fresh pass over the stored session). The claude adapter owns both — the
  subprocess moved to `agent.Supervisor.CompactCold`, where the binary path
  lives — and `compact/llm.go` became `prompt.go`: an instruction to a model,
  with no CLI knowledge left in a harness-neutral package. kimi returns the
  shared `harness.Unsupported` wording, which a test pins against the RPC
  gate's so a downgrade and a refusal read alike.
  - **Removing `d.sup` exposed a real bug it had been hiding.** Four
    destructive guards asked the CLAUDE supervisor whether a thread of ANY
    engine was running: "discard changes", "remove worktree" and the cleanup
    analysis all read a live *kimi* agent as not running and would have
    proceeded to touch its worktree underneath it. They ask the registry now.
    `handlerDeps` has no supervisor handle at all any more.
  - The exit-compaction tracker resolves each record's own harness and skips
    one that cannot compact, instead of spawning a `claude` for every
    cold-strategy record whatever ran the thread.

  **(b) Per-thread env overlay.** `StartSpec.Env`, applied identically by both
  adapters (`agent.ApplyEnvOverlay`, after provider routing).
  - **Probed:** `KIMI_CODE_HOME=/tmp/x kimi acp` wrote `device_id`, `logs/`
    and `migrations-effort.json` under the override, and the real home holds
    `sessions/`, `session_index.jsonl`, `credentials` and `oauth`. So the
    variable relocates a thread's ENTIRE kimi state — which also means
    pointing one at an empty directory **un-authenticates** it. That sharp
    edge is why this ships as a lever with no default policy: a per-thread-home
    feature has to solve credentials first.
  - Persisted and replayed on resume/fork/promote (a relaunch without it would
    look for the session in a different home), and deliberately unreachable
    from `launch_agent` — a worker's environment decides where its credentials
    come from and which endpoint they go to, so accepting it from a model
    would route around the permission prompt guarding every other route. A
    test drives the RPC with an `env` parameter and asserts it is ignored.

  **(d) Skills for every engine.** Install/Uninstall now maintain
  `<target>/.agents/skills/` alongside `.claude/skills/`.
  - **Probed, because P3's lesson is that kimi documenting a directory proves
    nothing about ACP:** a probe skill dropped in `<project>/.agents/skills/`
    appeared in a live ACP session's command list as `skill:akprobeskill`, and
    the agent named it when asked. (The binary also carries `.agents/skills`
    and `.kimi-code/skills` as literal discovery paths, and its own bundled
    import skill states `.agents` skills are supported by default.) Unlike
    `.agents/agents/`, this one is real over ACP — so the links are not dead
    files.
  - `ListInstalled` still reads the canonical `.claude` directory (Install
    writes the same links everywhere; listing all would report each skill once
    per engine), and Uninstall's "refuse to delete what we do not own" check
    now runs per directory.

  **(e) Claude launch-option sweep.** `FallbackModels` / `DisallowedTools` /
  `AddDirs` on `StartSpec`, each with its own capability flag and its own
  advanced field in the New Agent dialog.
  - **Probed:** all three exist on 2.1.220, but two are VARIADIC
    (`<tools...>`, `<directories...>`) and greedily eat the argv that follows —
    `claude -p --add-dir /tmp "prompt"` swallowed the prompt and failed with
    *"Input must be provided"*. Our threads pass the prompt over stdin as
    stream-json, so nothing is at risk today; each list value is still passed
    as its own flag occurrence so that stays true if the argv ever changes.
  - **The plan's kimi mapping is dropped, not faked.** "DisallowedTools maps
    onto written profile frontmatter when profiles exist" has nowhere to land:
    P3 proved agent files are unreachable over ACP. kimi declares all three
    false and reports a request for any of them per option with its own
    reason, through the new `Launched.UnappliedOptions`.
  - Persisted and replayed like the persona and the env: a resume that forgot
    `DisallowedTools` would hand the thread back a tool the human took away.
  - The UI hides a row an engine cannot apply, and `collect()` reads nothing
    from a hidden row — so a value typed before switching engines cannot leak
    into the launch. That is why a UI-driven start never has to report these
    as downgrades: it only offers what the engine can do.

  **(c) Subagent transcripts, both engines.** `agent.subagentTranscripts`
  behind a new `SubagentTranscripts` capability, served through an optional
  `subagentTranscriber` interface (the `modelDiscoverer` pattern) so a backend
  without subagent files carries no stub.
  - **Probed on disk:** claude keeps
    `<project>/<session>/subagents/agent-<id>.jsonl` (files, transcript shape);
    kimi keeps `<session-dir>/agents/<id>/wire.jsonl` — one DIRECTORY per
    subagent, ids `main` / `agent-0` / `agent-1`, holding its own wire
    protocol. `main` is the THREAD's own log and is never offered as a helper.
  - kimi's log types were read out of real sessions: `context.append_message`
    is already the transcript shape, and `context.append_loop_event` carries
    `content.part` (text and think), `tool.call {name,args}` and
    `tool.result {result.output}` — which map onto the blocks the dialog
    already renders. Engine bookkeeping (`llm.request`, `usage.record`,
    `step.begin/end`, tool snapshots) has nothing to show. The label a menu
    entry carries is the `profileName` the wire log records — `coder` /
    `explore` / `plan`, the compiled-in set P3 documented.
  - UI: a "Helpers" menu on the agent panel, hidden entirely for an engine
    that writes no such files (a greyed button would suggest the conversations
    exist but are out of reach), rebuilt from the core on every open since
    subagents appear as the agent delegates.

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

- `cd core && go test ./...` — bridge tool handlers, `agent.wait`, the modes
  store round-trip and built-in vocabulary, `mode.*` over the bus, the
  compaction routing + capability gate, the env/sweep record round-trips and
  replay, the subagent-transcript layouts, and the capability matrix in
  `harness_caps_test.go`. (The planned "kimi agent-file writer" was never
  built: P3 proved agent files are unreachable over ACP.)
- `cmake --build build && ctest --test-dir build` — 7 targets, including
  `EnsembleDialogTest` (P4) and `AgentRosterTest` (P5). The planned
  TranscriptModelTest MCP cases landed with P2.
- `python3 scripts/smoke-orchestrate.py` — three legs now: claude→kimi,
  kimi→claude, and an ensemble whose briefing alone drives a real launch.
  `smoke-agent.py` / `smoke-kimi.py` / `smoke-resume.py` / `smoke-fork.py` /
  `smoke-interrupt.py` stay green.
- Dogfood: P5's UI paths were exercised by running the real `agentkate`
  binary offscreen against a throwaway XDG home, applying an ensemble and
  watching its controller launch a worker (adopted with role "worker",
  parented, zero Qt warnings). The remaining manual pass — take over a worker
  mid-run, close the ensemble via `close_agent`, watch the all-threads
  inspector fill — is the human's, and needs a real desktop session.
