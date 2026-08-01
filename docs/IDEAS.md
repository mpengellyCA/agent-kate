# IDEAS — proposed major features (awaiting approval)

Candidate features surfaced by the 2026-08-01 capability-drift and KDE-citizen audits.
Each is big enough to deserve its own plan doc and an explicit go-ahead. Smaller gaps
found by the same audits were fixed directly and are not listed here.

**Status:** the eleven `[APPROVED]` items below are now planned as one program —
see [docs/plans/20-approved-features-program.md](plans/20-approved-features-program.md)
for the clustering, dependency graph and execution order, and the `→ plan:` pointer
under each item for its own plan doc. The three `[HOLD]` items (#5, #6, #14) are out of
scope and stay listed here untouched.

## Claude Code integration

### 1. Fleet view of detached background agents (`claude --bg` + `claude agents --json`) [APPROVED]
claude 2.1.220 ships a detached-agent subsystem AgentKate has no concept of: `--bg`
starts a session and returns immediately, and `claude agents --json` enumerates every
live session on the machine with pid/cwd/kind/sessionId/name/status (verified live).
AgentKate's roster only knows threads it spawned. A Fleet panel that polls
`claude agents --json`, shows agents started from terminals or other tools, and lets the
user adopt one into a panel (session browse and `--resume` already exist) would make
AgentKate the control plane for all Claude work on the box rather than an island.

→ plan: [docs/plans/21-fleet-and-agent-teams.md](plans/21-fleet-and-agent-teams.md)

### 2. Agent teams / swarms — the CLI's native multi-agent topology (experimental) [APPROVED]
The 2.1.220 bundle gates a multi-agent teams system behind
`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` / `--agent-teams`: `teammateMode` config,
snapshot module, and coordinator-comms MCP servers (`role: "comms"`). This is the
coordinator/worker topology our plan-16 orchestration MCP tools reimplemented from the
outside. Worth a spike to decide: adopt the CLI's native team protocol (comms-role
servers and coordinator semantics for free) or stay independent and accept two divergent
multi-agent models.

→ plan: [docs/plans/21-fleet-and-agent-teams.md](plans/21-fleet-and-agent-teams.md) (Phase 1 is the spike)

### 3. Plugin manager panel [APPROVED with User Note]
`claude plugin` is a full package manager (marketplace, install/uninstall/update,
enable/disable, `details` with projected token cost, `eval`, `tag`) plus
`--plugin-dir`/`--plugin-url` for session-scoped loading and a `reload_plugins` control
request for hot swap. AgentKate built a skills catalog, but plugins are the strictly
larger unit users actually distribute. A Plugins panel mirroring the Skills catalog,
with per-agent plugin sets and hot reload, closes the biggest "my Claude Code setup
doesn't come with me" gap.

#### User Note: This should encompase the existing Skills functionality and it should take a more granular and cross agent compatible approeach 

→ plan: [docs/plans/22-extension-catalogue.md](plans/22-extension-catalogue.md)

### 4. Checkpoints / rewind — restore the workspace to any point in a session [APPROVED with User Note]
Claude Code keeps `~/.claude/file-history` and offers rewind; AgentKate has zero
references to checkpoints anywhere. Given AgentKate already owns a git worktree per
thread, it can do this better than the CLI: a timeline rail on the transcript where
every turn boundary is a restorable point, backed by worktree state plus the CLI's file
history, with a diff preview before restoring.

#### User Note: This is an opportunity to improve and rework the Worktree system along with checkpoints. Currently it is escapable and agents have gotten confused. I think maybe we can run the agents inside a linux terminal container so it cant easily escape to higher directories without being given a deliberate path and hatch. 

→ plan: [docs/plans/23-contained-worktrees-and-checkpoints.md](plans/23-contained-worktrees-and-checkpoints.md)

### 5. Live workspace-diff view driven by the CLI's own VCS tracking [HOLD]
The CLI exposes `get_workspace_diff` as a control request and emits `vcs_state_changed`
/ `code_change_published` system events. AgentKate runs an independent gitstatus poller.
Subscribing to the CLI's events would update the diff the instant the agent writes a
file, and attribute each hunk to the turn that produced it — a per-turn "what this turn
changed" review surface, which is exactly what a multi-agent arena needs.

### 6. Cloud multi-agent review as a first-class action (`claude ultrareview`) [HOLD]
`claude ultrareview [target]` runs a cloud-hosted multi-agent review of a branch or PR
and prints findings. AgentKate already has a Problems panel and per-thread worktrees — a
"Review this worktree" action that runs ultrareview against the thread's branch and
lands findings in Problems would give every agent's work a review gate without leaving
the app or spending local tokens.

### 7. Hooks channel and hook manager [APPROVED]
`--settings`, `--setting-sources` and `--include-hook-events` are entirely unused, so
AgentKate sessions neither run user hooks visibly nor let the user manage them. A hooks
surface (view configured hooks, see hook_started/hook_progress/hook_response lifecycle
in the transcript, per-thread hook toggles) is large enough to be its own feature rather
than a gap fix.

→ plan: [docs/plans/24-agent-questions-and-hooks.md](plans/24-agent-questions-and-hooks.md) (§4–6)

### 8. Interactive agent questions (`request_user_dialog` / `side_question`) [APPROVED]
The CLI can ask the user something mid-turn; AgentKate currently drops these events. A
proper answer surface (inline question card in the transcript wired back over the
control channel) needs UX design and a response protocol — feature-sized, not gap-sized.

#### User Note: This is a much needed feature for all agent types that can support it

→ plan: [docs/plans/24-agent-questions-and-hooks.md](plans/24-agent-questions-and-hooks.md) (§1–3; the kimi
half also fixes a live bug — today an `AskUserQuestion` is auto-answered with its first option)

## Kimi Code integration

### 9. Kimi-native provider routing via `kimi provider` [Approved with user Note]
Kimi has a first-class multi-provider registry (`kimi provider add <api.json url>`,
`catalog` importing from models.dev, `list`, `remove`), and configured providers already
show up in the model configOption AgentKate discovers — the plumbing is half-done. A UI
for adding/listing providers (like the Claude-side picker) would let a Kimi agent run on
any models.dev endpoint, composing with per-thread `KIMI_CODE_HOME` isolation so
different agents can target different provider sets.

#### User Note: Lets use this as an option while still having first class support for using claude code with other providers

→ plan: [docs/plans/26-engine-services.md](plans/26-engine-services.md)

### 10. Session export and share for Kimi threads [APPROVED]
`kimi export [sessionId] -o out.zip` bundles a session as a shareable ZIP;
`kimi vis [sessionId]` launches a browser session visualizer. An "Export session…"
action (and a "Visualize" that opens the URL in existing KPartView/browser plumbing)
gives Kimi threads a shareable artifact and a bug-report bundle — and generalises to a
per-engine `ExportSession` harness method.

#### USER NOTE: We can use this to implement a Fork system as well since thats currently a gap with Claude vs Kimi

→ plan: [docs/plans/25-session-portability-and-fork.md](plans/25-session-portability-and-fork.md) (probed:
kimi 0.30 does **not** implement `session/fork`, so Phase 1 spikes three ways to build one)

### 11. Engine preflight health check built on `kimi doctor` [APPROVED]
`kimi doctor config` / `kimi doctor tui` validate config non-interactively, and
initialize's `authMethods` says whether login is needed. Today an unhealthy kimi install
surfaces only as a failed agent start. A per-engine preflight card in the New Agent
dialog (version, auth state, doctor verdict, model catalogue) turns opaque start
failures into a diagnosis — and gives the harness interface a natural `Health()` method
the Claude adapter can fill with `claude doctor`.

→ plan: [docs/plans/26-engine-services.md](plans/26-engine-services.md)

## KDE Plasma experience

### 12. System tray presence (KStatusNotifierItem) + close-to-tray [APPROVED]
Closing the window currently tears everything down (ShutdownDialog stops and compacts
every agent) because the app has nowhere to live without a window. For an app whose
premise is parallel background agents, a tray item is the single biggest missing Plasma
surface: aggregate state (N running / M waiting on you), per-agent context submenu,
Attention status pulsing when an agent hits the permission queue (the attention signal
already exists), and a close-to-tray preference so a long refactor keeps running.
Pairs with the KNotification work (same event sources, same click-to-raise path) and
KDBusService activation.

→ plan: [docs/plans/27-kde-presence.md](plans/27-kde-presence.md) (§2)

### 13. Global shortcuts (KGlobalAccel) for the three highest-frequency actions [APPROVED]
Desktop-wide bindings for: Show/Hide AgentKate (raise + focus the active composer),
Quick-ask the active agent (a small always-on-top prompt line — KRunner for your agent),
and Answer pending permission (jump to the first blocked agent). KGlobalAccel entries
also appear in System Settings ▸ Shortcuts, which is where KDE users discover what an
app can do. Depends on the KActionCollection refactor landing first.

→ plan: [docs/plans/27-kde-presence.md](plans/27-kde-presence.md) (§3; §1 **is** the KActionCollection
refactor, and it is the first thing the whole program should land)

### 14. First-run tour and shared per-panel empty states [HOLD]
The Simple/Advanced experience toggle and WelcomeDialog exist, but discovery stops
there: only one panel has an empty-state hint; Jobs, Cooperation, AI Inspector,
Problems, References, Search and Cowork all render as blank trees. A 3–4 step first-run
overlay tour plus a shared `EmptyState` widget (icon + one sentence + one action button)
would do more for the "best AI arena for KDE *users*" goal than any framework
integration — and the existing first-run profile means it can target Simple-mode users
only.

## Autonomy & scheduling

### 15. Extended timer and resume support — native scheduling for long-running autonomy [APPROVED]
Added 2026-08-01 from direct experience: driving a multi-hour autonomous program from
inside a Claude Code session hits hard limits — in-session wakeups cap at 1 hour and
die with the session, rate-window exhaustion stalls everything until a human returns,
and the workaround (a systemd timer resuming the session headlessly with
`--permission-mode bypassPermissions`) is exactly the kind of ungated self-escalation
that permission classifiers rightly block. AgentKate can own this properly: akcore is
already a long-lived daemon with per-thread permission gating, so scheduling and
resumption become first-class, *policy-respecting* features instead of external hacks:
- **Native scheduler in akcore**: persistent timers (survive UI restarts and reboots —
  state on disk, optionally backed by real systemd user timers that akcore itself
  installs/removes) that can start, resume, or prompt any thread at a given time or
  cadence.
- **Rate-window awareness**: the new rate_limit_event readout gives exact `resetsAt`
  times — a stalled/exhausted thread can be auto-resumed the moment its window resets,
  per-engine, with the queued work it was holding.
- **Resume-with-instructions**: "at 18:57, tell this agent to continue the review
  program" — scheduled prompts land through the normal harness send path with the
  thread's existing permission mode, not a bypass flag.
- **UI**: schedule/snooze controls on the agent panel + a Timers view (the Jobs panel
  is a natural host), showing next fire, target thread, and the prompt to be delivered.
- **Crash-safe, visibly**: a timer due while the app is closed auto-launches the
  full UI on the active desktop and then fires (systemd-backed visible
  resurrection via the KDBusService activation path) — never headless, never
  logged out. Locked-but-logged-in runs as normal (visible at unlock), and the
  scheduler arms **system wake alarms** so a sleeping machine wakes for due
  timers. *(Refined per Q7 answers; see plan 28 §3.)*

→ plan: [docs/plans/28-scheduling-and-autonomy.md](plans/28-scheduling-and-autonomy.md) (the
self-escalation point is the design constraint: a scheduled action never carries more
authority than the human granted the thread, enforced by a test rather than a comment)
