# Session knowledge dump — 2026-08-02

Written when the working session hit its token limit, so the next session (or a
fresh agent) can pick up without re-deriving the state. The session context was
compacted before this was written, so fine-grained conversational detail is
gone; everything below is reconstructed from the repo, the plan docs, and the
persistent memory index — all of it verified against the working tree today.

## Where we are

- **Branch:** `feature/approved-program` (base: `main`).
- **Program:** plan 20 — the approved-features program (IDEAS #1–#15 minus the
  three HOLD items), executed as plans 21–28, with remediation passes filed as
  plans 29 (pass 2) and 30 (pass 3, findings F51–F67).
- **Committed on this branch (most recent first):**
  - `8acafb6` pass-3 remediation rounds 3+4 (F58–F67, hot-path scaling +
    authority hygiene)
  - `d5aa74a` pass-3 remediation rounds 1+2 (F51–F57, C4)
  - `bb06d79` plan 30 audit doc
  - `18d9f6f` plan 27 **Phase 1** — single KActionCollection, command palette
    walks the collection (see the three findings recorded in
    `docs/plans/27-kde-presence.md`)
  - Earlier: plan 28 Phase 2 rate-window auto-resume (`0749ed2`), plan 29
    remediation (`608d433` and the fix commits below it), plan 25 fork-route
    rejection (`d34b5b0`), spike verdicts for plans 20/21/23/25 (`b4503ee`).
- **Uncommitted:** a large working set (~1,900 insertions across 45 files) that
  is the in-flight implementation of **plan 26, plan 27 §2–§3, and a new Codex
  engine backend**. Detail below. **Unknown:** whether the tree currently
  builds green and whether tests were last run passing — verify before
  committing (`core`: `go build ./... && go test ./...`; `ui`: the usual CMake
  build + ctest; `ui/tests/run-validator.sh` is a new untracked helper).

## The uncommitted work, by feature

### Plan 26 — Engine services (kimi provider registry + preflight health)
- `core/cmd/akcore/engineservices.go` (+ `_test.go`): new RPC surface for
  questions asked of an **engine** before any thread exists — preflight health
  (`kimi doctor`-shaped) and the kimi provider registry (`kimi provider
  add/catalog/...`). Registered from `registerHandlers` via
  `registerEngineServiceHandlers`.
- `core/internal/kimi/provider.go` (+ `_test.go`): the kimi-native provider
  registry client. Distinct from plan 11's Claude-shaped env-injection route
  (`core/internal/agent/provider.go`), which stays first-class.
- `core/internal/harness/health_test.go` + changes in `harness.go`: health
  lives on the neutral harness contract.
- UI consumers: `NewAgentDialog` (engine picker), `ProvidersDialog`,
  `EngineAvailability`, `HarnessTraits` all modified to surface health and the
  kimi registry.

### Plan 27 §2–§3 — KDE presence (tray + quick ask)
- `ui/src/shell/TrayPresence.{h,cpp}` (+ `TrayPresenceTest.cpp`): a
  `KStatusNotifierItem` — Passive when idle, Active while agents work,
  NeedsAttention when any agent is blocked on the human. Wired through
  `MainWindow`; `agentkate.notifyrc`, `.desktop.in`, `metainfo.xml` updated to
  match.
- `ui/src/QuickAskDialog.{h,cpp}`: the Meta+Shift+A surface — frameless
  always-on-top one-line prompt ("KRunner for your agent") that sends one
  message to the last-focused agent without switching windows. It owns no
  send logic: Enter emits `submitted()`, the window routes it through
  AgentDock's composer path and answers `acceptSent()` / `showError()`.
- `ui/src/shell/ActionIds.h` + `ActionIdsTest.cpp` grew entries for these
  (global shortcuts ride the plan-27-Phase-1 KActionCollection).

### Codex backend (new, third engine)
- `core/internal/codex/` (`codex.go`, `rpc.go` + tests): a Supervisor speaking
  the Codex CLI **app-server** protocol.
- `core/cmd/akcore/harness_codex.go`: adapts it to the neutral harness
  contract with a **deliberately modest capability set** — a protocol feature
  is not exposed until the supervisor translates and tests its full lifecycle.
- Registry/wiring touches: `run.go`, `handlers.go`, `harness_caps_test.go`,
  `harness.go`, plus `HarnessTraits`, `EngineAvailability`, `NewAgentDialog`
  on the UI side. `docs/HARNESSES.md`, `ARCHITECTURE.md`, `README.md` updated.

## Standing constraints and gotchas (from persistent memory — details in
`~/.claude/projects/-home-mike-Dev-AgentKate/memory/`)

- Claude backend must use the documented headless stream-json CLI, nothing
  undocumented.
- KDE-native UI: signature Midnight theme is palette-only on Breeze — never
  Fusion or app-wide QSS. Use FlowLayout + ElidingLabel for any new toolbar,
  chip row, or long label.
- IPC: never leave a `CoreClient::call` reply callback unguarded — this is the
  systemic use-after-free crash class found in the fleet audit.
- `QTextDocument::setMarkdown` eats text around `<T>` — route model output
  through `neutralizeMarkdownRawHtml` (MarkdownUtil).
- Transcript is memory-capped (5000 rows / 128 KB per result) after the oomd
  incident; keep new viewers bounded the same way (pass-3 F-findings enforced
  this — don't regress it).
- `QAction::setVisible(false)` clears `enabled`; the command palette carries an
  explicit `available` for this reason (plan 27 Phase 1 finding — applies to
  any new action consumer, including TrayPresence menus).
- Kimi CLI on this box is 0.31.1 — re-probe any 0.30-vintage fact, especially
  negative ones ("kimi can't do X").
- Worktree discipline: when a task says to work in a worktree, edit inside the
  worktree path only; create worktrees yourself from HEAD (subagent isolation
  worktrees branch from a stale base).
- After UI fixes that matter at runtime, reinstall the package (plan 19
  lesson) — the dogfooded instance runs the installed build.

## Suggested next steps

1. Build + test both stacks; run `ui/tests/run-validator.sh`.
2. Commit the working set — it splits naturally into three commits: plan 26
   engine services, plan 27 §2–§3 presence, and the codex backend.
3. Update the plan docs' status lines (26 and 27 still say PLANNED /
   PHASES 2–4 PLANNED; codex needs a home in the plan tree — likely plan 21
   fleet or a new doc) and the plan 20 program status line.
4. Then continue the program's execution order per
   `docs/plans/20-approved-features-program.md`.
