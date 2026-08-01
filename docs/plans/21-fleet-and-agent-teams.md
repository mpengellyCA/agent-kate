# 21 — Fleet view of detached agents, and the agent-teams question

**Status: PLANNED.** Covers IDEAS #1 (fleet view of `claude --bg` /
`claude agents --json`) and #2 (the CLI's experimental agent-teams topology).
Program context: [20-approved-features-program.md](20-approved-features-program.md).

**Size: L** — Phase 1 is a spike (S), the fleet panel is M, and what the spike
commits us to on teams is anything from S (write it off) to L (adopt the native
protocol).

## Why

Two problems that look separate and are not.

**AgentKate's roster only knows threads AgentKate spawned.** The `claude` CLI
has had a detached-agent subsystem for a while and AgentKate has no concept of
it. Probed live on 2.1.220 from this box:

```
$ claude agents --json
[
  { "pid": 1446754, "cwd": "/home/mike/Dev/AgentKate", "kind": "interactive",
    "startedAt": 1785586146929, "sessionId": "37936008-…", "name": "agentkate-b4",
    "status": "busy" },
  { "pid": 1601997, "cwd": "/MikeStorage2", "kind": "interactive",
    "sessionId": "1fd26990-…", "name": "mikestorage2-13", "status": "busy" },
  { "pid": 1629823, "id": "b5685cc0", "cwd": "/MikeStorage2", "kind": "background",
    "startedAt": 1785590153724, "sessionId": "b5685cc0-…",
    "name": "Expand video compression to multiple storage volumes",
    "status": "idle", "state": "working" }
]
```

Three live agents on this machine, one of them a genuine background worker, and
AgentKate — the app whose premise is *being the place your parallel agents
live* — shows none of them. It is an island next to the CLI, not a control
plane over it.

**And the CLI has its own multi-agent topology, gated.** The 2.1.220 bundle
carries `isAgentSwarmsEnabled()` behind `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS`
or an `--agent-teams` argv flag, a `teammateMode` config whose values are
`"in-process" | "tmux" | "iterm2" | "kitty" | "wezterm" | "vscode"`, a snapshot
module (`captureTeammateModeSnapshot`, `getTeammateModeFromSnapshot`), and a
coordinator mode with its own system prompt, agent set and tool filter
(`isCoordinatorMode`, `getCoordinatorSystemPrompt`, `getCoordinatorAgents`,
`applyCoordinatorToolFilter`, `COORDINATOR_MODE_ALLOWED_TOOLS`,
`CLAUDE_CODE_COORDINATOR_MODE`). Comms MCP servers are identified by
`role: "comms"` and filtered by `isCoordinatorCommsMcpTool` /
`excludeCoordinatorCommsMcpTools`.

That is the coordinator/worker topology plan 16's orchestration MCP tools
(`launch_agent` / `send` / `wait` / `close`) reimplemented from the outside. If
the CLI's version is real and stable, running two divergent multi-agent models
is a tax we pay forever. If it is a gate that vanishes next release, building on
it is a trap. **Neither answer is knowable from the outside, so Phase 1 is a
spike and nothing else is committed until it reports.**

There is also one telling detail found in the bundle:

```
Forking is not available in coordinator sessions. Use /branch instead.
```

A coordinator session has different session semantics from an ordinary one.
That is exactly the kind of thing that breaks a UI which assumes every thread is
the same shape — and exactly why the spike matters before the panel is built.

## Verified facts (probed against the installed binaries)

| Fact | How verified | Consequence |
|---|---|---|
| `claude agents --json` enumerates every live session with `pid/cwd/kind/startedAt/sessionId/name/status`, and background rows additionally carry `id` and `state` | Run live on 2.1.220, output above | The fleet feed needs no daemon and no new protocol — it is one subprocess on a timer |
| `--all` adds completed background sessions | `claude agents --help` | The panel can offer "show finished" without a second mechanism |
| `--cwd <path>` filters to sessions started under a path | `claude agents --help` | Workspace scoping (the house rule) is free — filter to the active project |
| `--bg, --background` starts a session and returns immediately | `claude --help` | AgentKate can *create* fleet members, not only observe them |
| `claude agents` also takes `--model`, `--effort`, `--agent`, `--mcp-config`, `--add-dir`, `--dangerously-skip-permissions` as **defaults for sessions dispatched from agent view** | `claude agents --help` | These are the CLI's own dispatch defaults, not ours; do not confuse them with per-thread launch flags |
| `-n, --name` sets the session display name shown in `agents --json` and `/resume` | `claude --help` | **Prerequisite, landing now.** Without it every AgentKate thread is nameless in the fleet feed and cannot be told apart from a terminal one |
| Agent teams are gated on `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` / `--agent-teams`; `--agent-teams` is **not in `claude --help`** | `claude --help \| grep -i agent-teams` → no match; the string exists in the bundle | It is an undocumented experiment. House rule: it may only ship behind a feature flag with a written fallback |
| `teammateMode` values include `in-process`, `tmux`, `iterm2`, `kitty`, `wezterm`, `vscode` | Bundle strings | Only `in-process` is plausible for a GUI app; the terminal modes assume a multiplexer AgentKate does not run |
| Coordinator sessions refuse `--fork-session` | Bundle string `"Forking is not available in coordinator sessions. Use /branch instead."` | A coordinator thread is not an ordinary thread; the UI would need to gate Fork on it |

## Phase 1 — SPIKE: is the native teams protocol something to build on?

**This phase produces a written verdict and no shipped code.** Budget: half a
day. Everything it touches is read-only or a throwaway directory outside the
repo.

### Probe commands

```bash
# P1.1 — does the gate exist at all, and what does it change?
cd /tmp/ak-teams-spike && git init -q && git commit -q --allow-empty -m base
CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude --help | diff - <(claude --help)
CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude -p --output-format stream-json \
  --verbose 'say hi' | head -5          # does init advertise anything new?

# P1.2 — is it reachable headlessly, which is the only mode AgentKate uses?
CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude --agent-teams -p \
  --output-format stream-json --input-format stream-json --verbose \
  'spawn a teammate and have it echo OK' 2>&1 | tee teams-headless.jsonl

# P1.3 — what does coordinator mode do to the session's own shape?
CLAUDE_CODE_COORDINATOR_MODE=1 claude -p --output-format stream-json --verbose \
  'list your tools' | jq -r 'select(.type=="system" and .subtype=="init") | .tools[]'
# diff that tool list against a normal session's — COORDINATOR_MODE_ALLOWED_TOOLS
# and applyCoordinatorToolFilter say it is filtered.

# P1.4 — does a comms-role MCP server appear, and is it addressable?
#   run a stdio MCP server that logs every request, wired via --mcp-config with
#   {"role":"comms"}, and see whether the CLI treats it differently from ours.

# P1.5 — teammateMode: is "in-process" viable without a terminal multiplexer?
CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude --settings \
  '{"teammateMode":"in-process"}' -p 'spawn a teammate' --output-format stream-json
```

### Success criteria (what a "yes, adopt it" verdict requires)

All five must hold; any single failure is a "no":

1. **Reachable headlessly.** The teams topology works under
   `-p --output-format stream-json --input-format stream-json`. If it needs the
   interactive TUI or a terminal multiplexer, it is unreachable from AgentKate
   and the verdict is no, full stop.
2. **Observable.** Teammate lifecycle and messages arrive as stream-json events
   we can render, not only as TUI paint. If we cannot show a teammate's work in
   the transcript, we have adopted a protocol and lost our best feature.
3. **Controllable.** A teammate can be interrupted and stopped through the same
   control channel, so plan 04's stop/interrupt semantics still hold.
4. **Stable-ish.** The flag or env var appears in at least two consecutive CLI
   releases, or is documented. One release is a coin flip.
5. **Compatible with our worktree model.** A teammate can be pointed at a
   directory we choose. If teammates always share the coordinator's cwd, they
   collide, and plan 16's per-worker worktree isolation is lost.

### The three verdicts and what each commits us to

| Verdict | What Phase 2+ becomes |
|---|---|
| **Adopt** | The Cooperation bridge grows a `role: "comms"` mode; `launch_agent` becomes a thin wrapper that spawns a *teammate* instead of a sibling thread; plan 16's tools stay as the stable public API so no ensemble breaks. Add `Capabilities.NativeTeams`. **Open question 2 in plan 20 is a strategic call for the user, not an engineering one.** |
| **Interop** (it works but is not something to depend on) | We stay independent. The fleet panel *recognises* a coordinator/teammate tree in `claude agents --json` and renders it as a group, so a user driving teams from a terminal sees it in AgentKate — read-only. Cost: S. This is the expected outcome. |
| **Write off** | Record the probe results in `docs/HARNESSES.md` so the next person does not re-run them, and delete the idea from IDEAS.md with a pointer to the evidence. Cost: nil. |

Whatever the verdict, **Phase 2 onwards is unblocked** — the fleet panel does
not depend on it.

## Phase 2 — The fleet feed (core)

A harness-level query that takes no `threadID`. This is the same shape plan 26
adds for `Health()`, so build whichever lands first and the other reuses it.

**Harness interface** (`core/internal/harness/harness.go`):

```go
// LiveSession is one agent process the engine knows about but AgentKate may
// not have started. Neutral across engines: an engine that cannot enumerate
// its own processes returns (nil, nil) and gates the panel off.
type LiveSession struct {
    EngineID  string `json:"engineId"`  // owning harness
    SessionID string `json:"sessionId"`
    Name      string `json:"name"`
    Cwd       string `json:"cwd"`
    Kind      string `json:"kind"`      // "interactive" | "background"
    Status    string `json:"status"`    // engine vocabulary, passed through
    PID       int    `json:"pid"`
    StartedMs int64  `json:"startedMs"`
    // Ours reports whether this session belongs to a thread in our store.
    // Filled by the handler, not the adapter.
    Ours     bool   `json:"ours"`
    ThreadID string `json:"threadId,omitempty"`
}
```

plus `LiveSessions() ([]LiveSession, error)` on the interface and
`Capabilities.LiveSessionFeed bool`.

- **`core/cmd/akcore/harness_claude.go`** — implement `LiveSessions` by running
  `claude agents --json` (optionally `--cwd <project>`), unmarshalling directly
  into `LiveSession`. Best-effort like `DiscoverModels`: any failure returns an
  empty list, never an error that would blank a populated panel. Set
  `LiveSessionFeed: true`.
- **`core/cmd/akcore/harness_kimi.go`** — `LiveSessions` returns `(nil, nil)`;
  `LiveSessionFeed` stays false. Kimi has no process enumeration: `kimi acp`
  accepts no flags but `--login` (verified), and `session/list` lists *stored*
  sessions, which `BrowseSessions` already serves. Do not conflate the two —
  browse is history, fleet is what is running now.
- **New `core/cmd/akcore/fleet.go`** — `fleet.list` RPC. Merges every
  `LiveSessionFeed` harness, then stamps `Ours` / `ThreadID` from
  `d.sessions.GetBySession(sessionID)` — the same correlation
  `session.browse` already does at `handlers.go:547-553`, so extract that into a
  shared helper rather than writing it twice. Also `fleet.adopt`, which is
  `session.attach` (`handlers.go:571`) with the fleet row's `cwd` as the project
  and its `name` as the title — **reuse the handler, do not fork it**; the
  already-attached short-circuit at `handlers.go:590` is exactly right here.
- **`core/cmd/akcore/fleet.go`** — `fleet.dispatch` (Phase 4): start a detached
  background agent with `claude --bg`. Distinct from `agent.start`: nothing is
  supervised, no stdin pipe is held, and the reply is the new session id only.

Polling belongs in the **UI**, not the core: the core has no idea when a fleet
panel is visible, and a core-side timer would run `claude agents --json` forever
in the background of a machine with no window open.

## Phase 3 — The fleet panel (UI)

**New `ui/src/FleetPanel.{h,cpp}`**, registered in `MainWindow::setupPanels`
next to the existing `registerPanel(m_keyTasks, …)` block
(`ui/src/MainWindow.cpp:314`) with icon `system-run` and a `m_keyFleet` key.

Design:

- A `QTreeView` grouped by `cwd` (project), each row: name · kind badge ·
  status chip · age. `Ours` rows are dimmed with a "in this window" marker —
  they are informational, not adoptable.
- **Flicker rule, and this is the panel most at risk of breaking it.** The feed
  polls; a naive rebuild repaints the whole tree every 5 s. Hold the list in a
  `Reactive<QList<FleetRow>>` (`ui/src/state/Reactive.h`) with `operator==` on
  `FleetRow` covering every rendered field, and publish only on change — the
  exact discipline `AgentJob`/`JobsPanel` uses (`ui/src/state/AgentJob.h`).
  Poll at 5 s while the panel is raised and **stop entirely when it is not**
  (`SideBar::raisedChanged`, wired at `MainWindow.cpp:419`).
- Actions, all registered through the `KActionCollection` plan 27 §1 introduces
  so they reach the command palette: **Adopt into a panel** (calls
  `fleet.adopt` then the existing `attachRequested` path
  `SessionBrowserDialog` already emits), **Open working directory**,
  **Copy session id**, **Show finished** (toggles `--all`).
- Empty state: not a blank tree. One sentence plus one action —
  "No other agents running on this machine. Start one in a terminal with
  `claude --bg`, or dispatch one here." (IDEAS #14 is on HOLD, so this panel
  ships its own inline empty state rather than waiting for a shared widget.)
- **Async-callback rule.** Every `m_core->call("fleet.list", …)` continuation is
  `QPointer`-guarded. A poll reply arriving after the panel is destroyed is the
  documented SIGSEGV class.

**`ui/src/state/HarnessTraits.{h,cpp}`** — mirror `liveSessionFeed` so the panel
hides the tab entirely when no registered engine can feed it.

## Phase 4 — Dispatch and adopt round-trip

- **Dispatch:** a "New background agent…" action in the Fleet panel reusing
  `NewAgentDialog` in a reduced mode (task, model, effort, cwd; no worktree, no
  Cowork — a detached agent is not ours to isolate). Calls `fleet.dispatch`.
- **Adopt:** the adopted thread appears in the roster as dormant and resumes
  through the existing path. Two truths to surface honestly, both learned from
  `session.attach`'s own comments (`handlers.go:594-611`): the adopted thread
  runs **in the session's own directory, not an isolated worktree**, and
  resuming it means *our* process now owns the conversation — if the original
  `claude --bg` process is still live, two processes would share one session id.
  **Refuse to resume a fleet row whose `status` is not `idle`,** and say why.
- If the Phase 1 verdict was **Interop**, this is where the coordinator/teammate
  grouping lands: rows whose session is a teammate nest under their coordinator.

## Verify

| Phase | What proves it |
|---|---|
| 1 | A written verdict in this doc (amend §Phase 1 with a *Result* subsection), plus the raw `teams-headless.jsonl` kept in the spike dir. The five success criteria answered yes/no individually, not in aggregate. |
| 2 | Go unit test `TestFleetListMergesAndStampsOurs` in `core/cmd/akcore/`: a fake harness returning three `LiveSession`s, a session store containing one of them, asserts exactly one row has `Ours=true` and carries the right `ThreadID`. A second test asserts a harness whose `LiveSessions` errors is skipped, not fatal — mirroring `session.browse`'s existing behaviour. |
| 2 | Manual: `claude --bg -p 'sleep'` in another terminal, then `fleet.list` over the socket shows it with `kind: "background"`. |
| 3 | Qt test `ui/tests/FleetPanelTest.cpp` (pattern: `JobsPanelTest.cpp`): feeding the same row list twice produces **zero** model resets — the flicker guard is the thing under test. A second case: a row whose status changes produces exactly one dataChanged for that row. |
| 3 | Manual: raise the panel, confirm polling starts; collapse it, confirm (via a debug counter or a log line) polling stops. |
| 4 | Manual round trip: dispatch from AgentKate → the row appears in `claude agents --json` run from a terminal → adopt it → the transcript replays → send a message and get a reply. |
| 4 | Manual negative: try to adopt a row whose `status` is `busy`; the UI must refuse with the two-processes-one-session explanation, not silently attach. |

## Non-goals

- **Supervising foreign processes.** AgentKate does not become the parent of a
  `claude --bg` process it did not spawn. It can observe, adopt (which means
  *resume the conversation ourselves*), and dispatch. It cannot interrupt or
  stop a process it does not own — there is no channel to do so, and pretending
  otherwise with a `kill(pid)` would truncate a turn mid-write, which plan 04
  spent a whole feature avoiding.
- **A kimi fleet.** Kimi has no live-process enumeration. `LiveSessionFeed`
  stays false there and the panel says so rather than showing an empty kimi
  group.
- **Building on the teams gate before the spike reports.** Nothing in Phases
  2–4 depends on it.
- **Replacing `SessionBrowserDialog`.** Browse is *history* (sessions on disk);
  fleet is *now* (processes alive). They share the attach handler and nothing
  else.

## Open questions for the user

1. **Adoption semantics when the source process is alive.** The plan's position
   is to refuse. The alternative — adopt read-only, tailing the transcript file
   without resuming — is more useful and more work. Which?
2. **Teams (program open question 2).** ~~If the spike says "adopt", is that a
   direction you want taken, given it makes plan 16's orchestration tools a
   compatibility layer over someone else's protocol?~~
   **RESOLVED 2026-08-01: both stay first-class.** Plan 16's MCP tools are the
   bridge across agent types and providers; native teams are the tailored
   same-engine topology (cross-spawn, context reuse). Situational choice, not
   either/or — the spike's job is now to make native teams *coexist* cleanly
   with our orchestration (shared roster visibility, no double-management of
   the same thread), not to pick a winner.
3. **Should the fleet panel be Simple-mode visible?** It is arguably a power
   feature; the Simple/Advanced toggle exists (`MainWindow.cpp:1254-1312`) and
   this is the kind of panel that belongs behind it.
