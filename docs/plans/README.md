# Agent Kate — Improvement Plans

A planning set for the next round of Agent Kate improvements, written after extended
daily dogfooding. Each item has its own grounded plan file with the relevant
`file:line` references, a proposed design, and step-by-step implementation notes.

## Index

| # | Improvement | Area | Plan | Rough size |
|---|-------------|------|------|-----------|
| 1 | Agent list → visual card list | UI (C++) | [01-agent-list-cards.md](01-agent-list-cards.md) | M |
| 2 | Markdown preview / raw / split toggle | Editor (C++) | [02-markdown-preview.md](02-markdown-preview.md) | M |
| 3 | Wire the toolbar Search box | UI (C++) | [03-toolbar-search.md](03-toolbar-search.md) | S |
| 4 | True stop / interrupt mid-response | Core (Go) + UI | [04-stop-agent.md](04-stop-agent.md) | M |
| 5 | Prompt queue, send-now, model switching (incl. Fable) | Core (Go) + UI | [05-queue-models-midsession.md](05-queue-models-midsession.md) | L |
| 6 | Fix auto-compaction on exit | Core (Go) | [06-compaction-shutdown.md](06-compaction-shutdown.md) | M |
| 7 | Document & media viewing (PDF/CSV/Office/AV) | Editor (C++) | [07-document-media-viewing.md](07-document-media-viewing.md) | M–L |
| 8 | KDE Plasma Cowork (share/see/control the desktop, consent-gated) | Core (Go) + UI (C++) | [08-kde-cowork/](08-kde-cowork/README.md) | L (phased v1/v2/v3) |
| 10 | Panel responsiveness & resize performance | UI (C++) | [10-panel-responsiveness.md](10-panel-responsiveness.md) | M (Phase 2: chat virtualization) |
| 11 | Third-party API providers (Fireworks, OpenRouter) via the Anthropic endpoint | Core (Go) + UI (C++) | [11-third-party-providers.md](11-third-party-providers.md) | M (phased core/persist/UI) |
| 12 | UI identity & approachability (signature theme, theme override, command palette, Simple/Advanced modes, layouts, panel responsiveness) | UI (C++) | [12-ui-identity-and-approachability.md](12-ui-identity-and-approachability.md) | L (phases 1–3 done; 4–8 planned) |
| 13 | Agent experience overhaul (chat selection/copy, interrupt vs stop, save+autosave, attachments, tool inspector, fork agent, roster cards v2, Git Log v2, Worktrees v2, Cowork control centre, file-browser worktree tabs) | Core (Go) + UI (C++) | [13-agent-experience-overhaul.md](13-agent-experience-overhaul.md) | XL (11 phases, one major release) |
| 14 | Harness abstraction & feature parity (capability registry, Engine picker, thinking/plan/subagent/background-shell rendering, mid-session control, context-fill & token observability) | Core (Go) + UI (C++) | [14-harness-abstraction-and-parity.md](14-harness-abstraction-and-parity.md) | XL (7 phases) |
| 15 | Kimi parity finish (option discovery, engine-aware new-agent quick menu, session browse) | Core (Go) + UI (C++) | [15-kimi-parity-finish.md](15-kimi-parity-finish.md) | M |
| 16 | Multi-agent orchestration (cross-harness launch/send/wait MCP tools, user-defined controller/worker ensembles, live MCP traffic view, Kimi first-class parity, Claude flag sweep) | Core (Go) + UI (C++) | [16-multi-agent-orchestration.md](16-multi-agent-orchestration.md) | XL (6 phases) |
| 17 | Editor session scoping (startup reopens files from unrelated projects) | UI (C++) | [17-editor-session-scoping.md](17-editor-session-scoping.md) | S–M |
| 18 | Cowork mid-session (enable desktop access on a running agent, agent-requested via MCP, OS permission taken up front) | Core (Go) + UI (C++) | [18-cowork-mid-session.md](18-cowork-mid-session.md) | M |
| 19 | Jobs panel (background work from every agent, filling the last stubbed panel) & attachment durability (cached image copies, total send budget) — ✅ **landed + hardened**, P1–P4 | UI (C++) + Core (Go) | [19-jobs-panel-and-attachment-durability.md](19-jobs-panel-and-attachment-durability.md) | M |
| **20** | **Approved features program** — the map for 21–27: clustering, dependency graph, execution order, and how each user note shapes its design | Program doc | [20-approved-features-program.md](20-approved-features-program.md) | — |
| 21 | Fleet view of detached agents (`claude agents --json` / `--bg`) + the agent-teams spike | Core (Go) + UI (C++) | [21-fleet-and-agent-teams.md](21-fleet-and-agent-teams.md) | L |
| 22 | Extension catalogue — plugins that subsume Skills, granular per component, cross-engine | Core (Go) + UI (C++) | [22-extension-catalogue.md](22-extension-catalogue.md) | L |
| 23 | Contained worktrees (agents can no longer escape) + a checkpoint timeline on top | Core (Go) + UI (C++) | [23-contained-worktrees-and-checkpoints.md](23-contained-worktrees-and-checkpoints.md) | XL |
| 24 | The interaction channel — agents asking the user (both engines), and hooks made visible and per-agent | Core (Go) + UI (C++) | [24-agent-questions-and-hooks.md](24-agent-questions-and-hooks.md) | L |
| 25 | Session portability — export, visualize, and a cross-engine Fork | Core (Go) + UI (C++) | [25-session-portability-and-fork.md](25-session-portability-and-fork.md) | M–L |
| 26 | Engine services — the Kimi provider registry, and preflight health for both engines | Core (Go) + UI (C++) | [26-engine-services.md](26-engine-services.md) | M |
| 27 | KDE presence — one KActionCollection, a system tray item, and global shortcuts | UI (C++) | [27-kde-presence.md](27-kde-presence.md) | M–L |
| 28 | Native scheduling & resume — persistent timers, rate-window auto-resume, gated agent-requested schedules | Core (Go) + UI (C++) | [28-scheduling-and-autonomy.md](28-scheduling-and-autonomy.md) | L |
| 29 | Pass-2 remediation — consent quality, honest labelling, destructive actions, first-run friction (F25–F50) | Core (Go) + UI (C++) | [29-pass2-remediation.md](29-pass2-remediation.md) | M (3 rounds) |
| 30 | Pass-3 remediation — lifecycle correctness, bounded reads, hot-path scaling (F51–F67, internal audit of the perf/stability/multi-agent refactor) | Core (Go) + UI (C++) | [30-pass3-remediation.md](30-pass3-remediation.md) | M (4 rounds) |

Size key: **S** ≈ <½ day, **M** ≈ 1–2 days, **L** ≈ 3–5 days, **XL** ≈ a release of its own.

Plans **21–28** are one program, approved together from the 2026-08 capability-drift
and KDE-citizen audits (28 was added to the same program a few hours later). Read
[20](20-approved-features-program.md) first: it holds the dependency graph, the
recommended execution order, and the parallel-track split.
Its headline is that **plan 27 §1 (the KActionCollection refactor) should land before
anything else in that set** — five of the seven clusters add actions, and every action
born outside the collection has to be retrofitted later.

**Status (2026-08-02): the program is started.** Landed so far: plan 27 §1 (the
KActionCollection refactor, `18d9f6f`), plan 28 Phase 2 (rate-window auto-resume,
`0749ed2`), and all three step-1 spikes with verdicts recorded in plans 21/23/25.
Plan 24's question channel landed earlier under a different design than the plan
describes (kimi `isQuestionRequest` + the in-panel question form); its hooks half is
untouched. Plans 21, 22, 23, 25, 26 and the rest of 27/28 are not started. The source
list is [IDEAS.md](../IDEAS.md), where each
item carries its approval and, where the user gave one, a recorded decision that shapes
the plan rather than being a preference on top of it:

- **22** — the plugin catalogue must *subsume* the existing Skills feature rather than
  sit beside it, and be granular per component and cross-engine. That is why the plan is
  an "extension catalogue" and not a plugin panel.
- **23** — checkpoints are the occasion to rework the worktree system, which today is
  escapable and has confused agents. The recorded direction is to contain each agent
  (a Linux container was the user's suggestion) so it cannot reach higher directories
  without a deliberate path and hatch. Containment leads, the timeline sits on top.
- **24** — agent-asks-the-user is wanted for *every* engine that can support it, not
  just claude, which is what makes it an interaction channel rather than a claude
  feature.
- **26** — `kimi provider` routing is an *option*; first-class support for running
  Claude Code against other providers stays.

Three IDEAS items are on **hold** and deliberately have no plan doc: live workspace-diff
from the CLI's VCS events (#5), cloud `claude ultrareview` as an action (#6), and the
first-run tour with shared empty states (#14).

## Landed 2026-08-01

One day, six review rounds (an adversarially-verified fleet review of plan 19
P1+P2, then fix rounds 2–6, each re-reviewed by a fresh fleet across five review
workflows). It moved far more code than a plan-19 hardening pass suggests,
because each round's review kept finding parity and citizenship gaps next to the
bug it was sent after. What came out of it, so a future reader knows why one
day's diff is this large:

- **Token-by-token streaming.** `claude --include-partial-messages` is now on,
  and `stream_event` deltas render into the live assistant card
  (`flushStreamedText`) instead of arriving as one block at turn end. The core
  batches a burst of deltas into a single notification, and stored transcripts
  hold no `stream_event`s, so replay is unaffected.
- **A real `system` event dispatch.** `system` subtypes used to fall through
  as noise. They are now routed: `task_*` / `background_tasks_changed` drive
  the jobs tray, `thinking_tokens` drives the working indicator's activity
  line, `init` seeds slash commands and persists the session's discovered
  config options, and everything else — model fallbacks, compaction
  boundaries, API errors — goes through `renderSystemSubtype` rather than
  being dropped.
- **Rate-limit readout.** `rate_limit_event` folds into a header chip
  (`applyRateLimit`): which window, when it resets, whether the account is on
  overage — with a note added only on a status *transition*, so a steady
  stream of ticks does not spam the feed.
- **The claude control channel** (`core/internal/agent/control.go`). Probed
  against 2.1.220: the CLI accepts `control_request` lines on the already-open
  stdin of a running print-mode session. That gives mid-session
  `set_max_thinking_tokens` (effort mapped onto Claude's own think-keyword
  budgets, so a live change lands where a relaunch at that effort would),
  `reload_skills`, `get_context_usage` with a per-category breakdown, and
  `list_models` — the last of which reports each model's supported effort
  tiers, now carried on `DiscoveredOptionValue.Efforts`. Empty means "the
  harness said nothing", read as every tier, never as none.
- **Kimi capability parity.** Hot compaction via `/compact` as prompt text
  (`Compaction` true, `ColdCompact` false — the flag split is new), and a
  context readout recovered from `/usage`, which kimi answers locally with
  nothing billed. Both correct standing claims in
  [plan 14](14-harness-abstraction-and-parity.md); see its Non-goals and the
  P2 note for what was wrong and why.
- **KDE shell citizenship.** `KDBusService` single-instance (with the second
  process's command line forwarded and its positional path resolved against
  *its* working directory, parsed by a parser built the same way startup's is
  so `--desktopfile <name>` is not mistaken for a project), a pinned
  `desktopFileName` tying the binary, the `.desktop` entry and the notifyrc
  together, `Keywords=` for launcher search, a KNotification setup
  (`ui/src/notify/`), and a metainfo `<developer>` block replacing the
  deprecated `<developer_name>`.
- **Attachments and jobs** — [plan 19](19-jobs-panel-and-attachment-durability.md)
  itself, P1–P4: the Jobs panel filling the last `StubPanel`, a bounded chip
  row, durable content-addressed attachment copies, a total send budget
  enforced where frames are made, clipboard paste and raw-pixel drops, and
  cache pruning with separate policies for derived and user data. The
  frame-cap crash class is closed at four layers, and the UI now recovers from
  a dropped core instead of ghost-running against one.
- **Cowork hardening.** Every portal call is async (a wedged
  `xdg-desktop-portal` can no longer stall the GUI thread), every response
  waiter has a lifetime backstop, all four failure paths route through
  `failPortalStep`, abandoned sessions are closed rather than orphaned, and
  the desktop-wide a11y flip is crash-safe: originals are persisted before the
  flip and replayed at a later startup, owned by exactly one PID.

## Architecture recap (why each item lands where it does)

Agent Kate is two processes (see [ARCHITECTURE.md](../../ARCHITECTURE.md)):

- **`agentkate`** — C++/Qt6/KF6 UI. Thin: renders, handles input, owns the core's
  lifecycle. Items 1, 2, 3 are mostly here; 4 and 5 have UI halves.
- **`akcore`** — Go orchestration core. Spawns one headless
  `claude -p --output-format stream-json --input-format stream-json` per agent
  thread, manages worktrees, sessions, search, LSP, and the Cooperation MCP.
  Items 4, 5, 6 are mostly here.

They speak newline-delimited JSON-RPC 2.0 over a Unix domain socket. Adding a feature
that spans both processes means: a new/changed RPC method registered in
`core/cmd/akcore/main.go`, a `CoreClient::call(...)` from the UI, and the core-side
handler delegating to `core/internal/...`.

## Suggested sequencing

1. **#3 toolbar search** and **#4 stop** first — small, high daily value, low risk.
   #3 is nearly self-contained; #4 fixes a credit-wasting / safety gap.
2. **#6 compaction** next — it's a correctness bug with a clear diagnosis; fixing it
   protects every long session.
3. **#5 queue / models / mid-session** — largest, but #4 (interrupt) is a natural
   prerequisite for a good "send now vs. queue" UX, so do it after #4.
4. **#1 agent cards**, **#2 markdown preview**, and **#7 document/media viewing** —
   UI-only, no protocol changes; parallelizable, slot in whenever. #2 and #7 share the
   `EditorArea` dispatch rework, so do them together (one ordered file-type resolver).

Items 4–6 touch the agent supervisor and shutdown path in the same files
(`core/internal/agent/agent.go`, `core/cmd/akcore/main.go`); doing them in a
deliberate order avoids merge churn.

## Cross-cutting notes

- **No new heavy dependencies** are required for any item. Markdown preview (#2) uses
  `QTextDocument::setMarkdown` + `QTextBrowser`, both already linked; there is **no**
  QtWebEngine in the build and none is needed.
- **Model IDs** (#5): the current tier→id map in `core/cmd/akcore/main.go`
  (`resolveCompactModel`, ~line 1847) is the single source of truth and is stale
  (`claude-opus-4-7`). Refresh it to the current generation (Opus 4.8, Sonnet 4.6,
  Haiku 4.5) and add Fable as part of #5.
- **Stop + queue + compaction** all hinge on one fact: stdin to the `claude` child is
  kept open across turns, and there is no in-band cancel message — see #4.

## Note on #2 + #7 (shared editor dispatch)

Both add new non-text viewers hosted as editor tabs. They should share **one ordered
file-type resolver** in `EditorArea::openFile` (markdown → csv → image → generic
KPart → media → text) rather than each bolting on its own `if`. #7 also narrows
`ImageView::canDisplay` so PDFs stop being hijacked into a static first-page image and
route to the Okular KPart instead. Plan them as a single editor-dispatch refactor.
