# Code Review: `kimi-code-backend` branch

- **Base:** `main` (merge-base diff, `main...kimi-code-backend`)
- **Commits:** 10 (e0e9594 … 16cb2a0)
- **Scope:** 49 files, +8204 / −529
- **Reviewed:** 2026-07-30

The branch adds Kimi Code as a second agent backend (`kimi acp` / ACP), a harness
abstraction with capability gates, observability (context meter, per-tool spend, turn
timing), working-visibility UI (background tray, live subagent transcripts), mid-session
control (model/mode switching, plan mode), and rendering parity (thinking cards, plan
checklists).

**Overall verdict:** Solid, mergeable-quality work. One HIGH UI bug, three MEDIUM
issues, and a set of LOW robustness/consistency items. Go code passes `go vet` and
`go test -race`; UI builds clean and `TranscriptModelTest` passes 13/13. The core↔UI
wire protocol was checked field-by-field and is consistent in both directions.

---

## Findings

### HIGH

#### H1. Editable model combo silently drops a hand-typed model id
- **Location:** `ui/src/AgentPanel.cpp:1430` (also `:1036`)
- **Issue:** The discovered-model engine picker calls `m_modelCombo->setEditable(true)`
  but never `setInsertPolicy(QComboBox::NoInsert)`. Qt's default `InsertAtBottom`
  appends the typed text as a dropdown item carrying **no `Qt::UserRole` data** and
  makes it current when the user presses Enter. `currentModel()` then hits the
  `currentText() == itemText(idx)` branch and returns `itemData(idx).toString()` —
  which is `""`. The typed model id is silently dropped: `agent.start` /
  `agent.setOption` send `model: ""` (CLI default), and while running the user gets the
  misleading "default applies from the next start" note. The dataless item also
  persists in the dropdown, so re-selecting it later reproduces the bug. Typing without
  pressing Enter works, making the failure look intermittent.
- **Fix:** In the `discovered` branch of `rebuildModelCombo()`, call
  `m_modelCombo->setInsertPolicy(QComboBox::NoInsert)` right after `setEditable(true)`,
  and/or harden `currentModel()` to fall back to `currentText().trimmed()` when
  `itemData(idx)` is invalid/empty.

### MEDIUM

#### M1. Kimi adapter reports unapplied config as applied
- **Location:** `core/cmd/akcore/harness_kimi.go:65`
- **Issue:** `kimiHarness.Launch` reports `spec.Model` / `spec.Effort` /
  `spec.PermissionMode` verbatim as the *applied* values, but the kimi handshake
  deliberately downgrades a CLI-rejected value to the default with only a log warning
  (`core/internal/kimi/thread.go:388-397` for model, `:413-428` for thinking/mode).
  The persisted `session.Record` then claims a model/mode the agent is not actually
  running, contradicting the `Launched` contract ("the record holds what the harness
  actually APPLIED … so a later resume replays reality"). Effects: roster/pickers show
  the wrong current model, and every resume re-attempts the bad value instead of
  recording the real default. The claude adapter does this correctly (`resolveModel`,
  defaulted mode).
- **Fix:** Have `kimi.Supervisor.Start` (or `Thread`) expose the config actually
  applied after the handshake — the translator already tracks corrected values via
  `newTranslator(sessionID, model, configOptions)` `CurrentValue`s — and populate
  `Launched` from that rather than from the request.

#### M2. stderr pipe not drained during ACP handshake
- **Location:** `core/internal/kimi/thread.go:302`
- **Issue:** `pumpStderr` is only launched *after* the ACP handshake completes
  (`s.handshake(t, opts)` at line 284). Between `cmd.Start()` (line 244) and the end of
  the handshake (up to 60 s), the child's stderr pipe is never read. (a) If kimi writes
  more than the OS pipe buffer (~64 KB) to stderr during startup — verbose logging,
  noisy shell rc, slow plugin — the child blocks on write and the handshake stalls into
  a spurious "acp initialize: context deadline exceeded". (b) On any handshake failure
  the stderr diagnostics are lost, so the user gets a bare RPC error with no hint why
  (not logged in, bad session id, …). The Claude backend starts `pumpStderr`
  immediately after `cmd.Start()` (`core/internal/agent/agent.go:440`), and this
  package's own probe path drains stderr immediately too (`discover.go:74`).
- **Fix:** Launch the stderr pump (or a plain drain goroutine for the pre-registration
  phase) right after `cmd.Start()`, before `s.handshake`. On handshake failure,
  optionally include the drained stderr tail in the returned error.

#### M3. Promote bar not gated on the harness's `promote` capability
- **Location:** `ui/src/AgentPanel.cpp:2015` (also `onPromoteClicked` at `:3335`)
- **Issue:** The "Move to a private copy" bar visibility condition
  (`!m_threadId.isEmpty() && !m_isolated && !m_promoting`) ignores `traits.promote`,
  even though the same `refresh()` gates fork, compaction, and cowork on their traits,
  and `HarnessTraits::promote` exists exactly for this (false for kimi). A kimi thread
  running non-isolated in the workspace shows the bar; clicking sends `agent.promote`,
  which the core rejects — the user finds out only via an after-the-fact error note.
- **Fix:** Add `traits.promote` to the visibility condition, and optionally a guard +
  note in `onPromoteClicked`, mirroring `runCompactNow`'s capability check.

### LOW

#### L1. Double-resume guard is check-then-act; supervisors don't reject duplicate thread ids
- **Location:** `core/cmd/akcore/handlers.go:295`, `core/internal/agent/agent.go:420`,
  `core/internal/kimi/thread.go:295`
- **Issue:** Two concurrent resumes can both pass `d.agentRunning` before either
  registers (the reply returns before `resumeThread` runs on its goroutine). Neither
  supervisor's `Start` rejects a duplicate thread id — both blindly overwrite
  `s.threads[t.ID]`, so the first process's `reap()` does `delete(s.threads, t.ID)` and
  deregisters the second, live process (unstoppable, unfound via `Running`; two
  processes stream events for one thread id). The comment at `handlers.go:291-294`
  describes exactly this corruption, but the guard only narrows the race.
- **Fix:** Make registration atomic — check-and-insert under `s.mu` in both
  supervisors' `Start`, returning `thread %q already running` on a duplicate so the
  race loser fails cleanly in `Launch` and emits the error lifecycle event.

#### L2. Interrupt backstop TOCTOU (stale pgid / thread-id reuse)
- **Location:** `core/internal/kimi/thread.go:597-618`
- **Issue:** After `time.Sleep`, the backstop re-resolves the thread *by ID* via
  `cancelPending(threadID)` but signals the `pgid` captured at `Interrupt` time. If the
  original thread was reaped during the sleep and the id was reused by a resumed
  thread, the stale backstop's condition passes and it SIGINTs the old pgid (normally
  harmless ESRCH, but pgid reuse makes a stray signal possible). Also, the cancel can
  be acked in the window between `cancelPending` returning true and the `syscall.Kill`.
  (The Claude backend has the identical pattern — pre-existing convention, not a
  regression.)
- **Fix:** Have the backstop verify the resolved `*Thread` is the same instance it was
  armed for (`s.thread(threadID) == t`) before signaling; optionally re-check
  `cancelPending` immediately before the kill.

#### L3. Inconsistent kill scope: leader-only vs process group
- **Location:** `core/internal/kimi/thread.go:285-287`, `:676-684`
- **Issue:** Handshake-failure teardown (`cmd.Process.Kill()`) and the stop
  kill-backstop (`proc.Kill()`) kill only the process-group leader, while
  `discover.go:88` and the interrupt backstop kill the whole group
  (`syscall.Kill(-pgid, …)`). If the handshake fails *after* `session/new` succeeded
  (thread.go:374-376), kimi may already have spawned children; a hung-stop
  `proc.Kill()` likewise leaves the group behind. Impact is limited for the MCP bridge
  (its serve loop exits on stdin EOF), but any kimi child that doesn't self-terminate
  on stdin EOF would linger.
- **Fix:** Use `syscall.Kill(-t.pgid, syscall.SIGKILL)` in both spots for consistency.

#### L4. Phantom tool_use card for unknown `toolCallId` completion
- **Location:** `core/internal/kimi/translate.go:295-321`
- **Issue:** A `tool_call_update` with status `completed`/`failed` for an unknown
  `toolCallId` (no preceding `tool_call`) creates an empty `toolCallState` and, because
  `done` is true, emits a spurious assistant `tool_use` card named `"Tool"` with empty
  input alongside the result.
- **Fix:** When `st` was just synthesized (no known title/kind), skip
  `emitToolUseLocked` and emit only the `tool_result`.

#### L5. Pending-RPC map entry leaks on context timeout
- **Location:** `core/internal/kimi/acp.go:179-189` (`acpClient.call`)
- **Issue:** On `ctx.Done()` the call returns without unregistering the pending
  callback. If the peer never answers and the stream stays open, one map entry leaks
  per timed-out call for the process's lifetime (`SetConfigOption`'s 15 s timeout is
  the reachable case; the handshake timeout is followed by process kill, which drains
  via `failAll`). Bounded and small, but easily avoided.
- **Fix:** On ctx timeout, lock `c.mu` and `delete(c.pending, key)` before returning
  (needs the id key returned from `send`, or registration via `send`'s internals).

#### L6. Init-event option persist doesn't notify `HarnessRegistry::changed()`
- **Location:** `ui/src/AgentPanel.cpp:3722` (vs. `ui/src/state/HarnessTraits.cpp:179`)
- **Issue:** Two writers persist discovered option lists, but only one notifies. The
  `agent.discoverOptions` probe path emits `changed()` after
  `persistDiscoveredOptions`, so the roster "+ New Agent" menu and open pickers
  rebuild. The init-event path in `AgentPanel::renderEvent` calls the static
  `persistDiscoveredOptions` directly with no emission — models learned from an actual
  kimi session never reach the roster quick menu or already-created panels' combos
  until app restart.
- **Fix:** Route the init-event persist through a registry instance method that emits
  `changed()` when the cache actually changed (or emit
  `HarnessRegistry::self()->changed()` from the panel after persisting).

#### L7. `SessionBrowserDialog` infers transcript capability from model vocabulary
- **Location:** `ui/src/SessionBrowserDialog.cpp:38`
- **Issue:** `backendHasTranscript()` infers "previewable/forgettable on-disk
  transcript" from `modelPicker == "tiers"`, conflating model vocabulary with
  transcript storage. Meanwhile `HarnessTraits` parses a dedicated `sessionBrowse`
  field (and `usageReporting`) that no UI code consumes. The proxy happens to be right
  for claude/kimi today but will misfire for a future harness, and bypasses the
  branch's own "never compare backend affordances outside traits" rule in spirit.
- **Fix:** Add an explicit trait (e.g. `transcriptPreview`/`transcriptForget`) to the
  capabilities payload and bind the dialog to it; drop or consume the unused
  `sessionBrowse`/`usageReporting` fields.

#### L8. `NewAgentDialog` rebuild resets user's combo selections
- **Location:** `ui/src/NewAgentDialog.cpp:140`
- **Issue:** The `HarnessRegistry::changed` connection re-runs
  `rebuildBackendChoices`, which clears and repopulates the model/permission/effort
  combos without preserving the current selection. If the capability fetch or the
  dialog's own option probe lands after the user has chosen, the selection silently
  resets to the first entry. `AgentPanel::rebuildEngineCombo()` shows the correct
  pattern (capture `currentData()` before clear, restore after).
- **Fix:** Preserve each combo's `currentData()` across the rebuild and restore via
  `findData`/`setCurrentIndex` when still present.

#### L9. `kimiDefaults()` fallback missing `sessionBrowse = true`
- **Location:** `ui/src/state/HarnessTraits.cpp:52-61`
- **Issue:** The built-in fallback omits `t.sessionBrowse = true`, but the Go adapter
  declares `SessionBrowse: true` (`core/cmd/akcore/harness_kimi.go:39`, frozen by
  `harness_caps_test.go:62-64`). `docs/HARNESSES.md:104` and plan 15 require the mirror
  to be exact. Impact is nil today (no UI consumer of the trait), but the drift is the
  silent-desync class the lockstep rule exists to prevent.
- **Fix:** Add `t.sessionBrowse = true;` in `kimiDefaults()`.

#### L10. `ARCHITECTURE.md` "spawns" diagram mangled
- **Location:** `ARCHITECTURE.md:22-23`
- **Issue:** The branch replaced the third column (`MCP clients`) with a second
  `kimi acp` column, so `kimi acp` now appears twice and the MCP-clients leaf is gone —
  while the `akcore` bullet list below still describes the Cooperation MCP server.
- **Fix:** Restore three columns: ``headless `claude` or `kimi acp` `` /
  `language servers` / `MCP clients`.

#### L11. Plan 15 missing from the plans index
- **Location:** `docs/plans/README.md:23`
- **Issue:** `docs/plans/15-kimi-parity-finish.md` landed on this branch, but the index
  table only adds the plan-14 row.
- **Fix:** Add the plan-15 row to the index table.

### Test coverage gaps (no code bug, but high regression risk)

- **T1. Interrupt escalation & mid-turn death untested** — `core/internal/kimi/thread.go:597-618`,
  `:883-916`. The riskiest code in the package (SIGINT/SIGKILL backstop on a hung turn,
  `reap()` reporting phase `"interrupted"`, `isStreamClosed` early-return in
  `onPromptDone` at `:531`) has no test. `TestKimiInterruptIdle` only asserts the
  no-op cases. Suggestion: a fake-kimi mode that ignores `session/cancel`; with short
  backstop delays assert the process dies, `_lifecycle` phase is `"interrupted"`, and a
  subsequent `Start{Resume: true}` works; plus a fake mode that exits mid-turn and
  asserts no synthetic `result` event.
- **T2. `ListSessions` / `DiscoverOptions` only covered by the opt-in live smoke test**
  (`KIMI_LIVE_SMOKE`, never runs in CI). The probe-session filtering logic
  (`probeDirPrefix` prefix match, `discover.go:160-164`) is untested by default.
  Suggestion: implement `session/list` in `fakeKimiScript` (one probe-dir session, one
  normal) and assert the probe entry is filtered out.

---

## Verified clean (explicitly checked, no issues)

- **Wire protocol (Go emitter ↔ C++ parser):** `agent.event` batch shape with legacy
  fallback; `_lifecycle` (core-level adds worktree fields the kimi supervisor's own
  lacks — consistent); `_stderr.text`; `_commands`; `permission.requested` /
  `permission.respond`; `agent.reviewRequested`; `agent.start` params+reply;
  `agent.capabilities` (every `Capabilities` JSON field matches `fromJson`);
  `agent.discoverOptions`; kimi `initEvent` shape vs. `docs/HARNESSES.md:30-33`;
  KConfig key composition.
- **Concurrency:** supervisor lifecycle (handshake-before-registration,
  `activePrompts` counter, cancel-vs-stop sequencing, `reapWG` shutdown ordering);
  `agent.Supervisor` `turnsInFlight`/`controls` bookkeeping — all race-tight under
  `-race`.
- **Qt ownership/threading:** new dialogs use `WA_DeleteOnClose`; chips/timers parented;
  every `CoreClient::call` callback passes a context object or guards with `QPointer`;
  backend events handled on the UI thread; per-session meters/checklist/inspector state
  reset on start/resume/thread-switch.
- **Backward compatibility:** legacy records with empty `Backend` resolve through the
  registry default; claude path behavior-preserving apart from the deliberate,
  well-tested graceful-Stop redesign; MCP bridge, permission broker, session
  resume/fork/compaction intact.
- **Build files:** `ui/CMakeLists.txt` includes the new source; `core/go.mod`/`go.sum`
  unchanged.
- **`scripts/smoke-kimi.py`:** correct against the real RPC contract; cleanup paths sound.

## Suggested fix order

1. H1 (functional data-loss bug in the feature it undercuts)
2. M1, M3 (wrong state surfaced to the user)
3. M2 (debuggability + rare startup stall)
4. T1, T2 (lock in the risky paths before further work)
5. L1–L11 (robustness/consistency sweep)
