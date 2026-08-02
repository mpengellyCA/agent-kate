# 30 — Pass-3 remediation: lifecycle correctness, bounded reads, and hot-path scaling

**Source:** a fresh internal audit of the post-refactor tree (2026-08-02, HEAD
`18d9f6f`), run after the performance/stability/multi-agent programme (plans
13–19, the pass-1/pass-2 security audits, and the plan-29 remediation rounds).
Unlike passes 1 and 2 — whose centre of gravity was security and UX — this pass
aimed at the refactor's own goals: **concurrency correctness, process lifecycle,
and performance that scales with activity rather than history.** Finding numbers
continue the audit series (pass 2 ended at F50): **F51–F67**.

Baseline at audit time: build clean, `go vet` silent, `go test -race ./...`
green, **27/27 ctest**. Everything below is therefore a logic/ordering/scaling
defect the current suite does not pin — every fix in this plan lands with a
test that fails if the logic is inverted, per the standing convention.

It is deliberately **not** a feature plan. Where a finding's real fix belongs
to an approved feature, this doc lands the honest interim fix and records the
hand-off.

---

## The headline: the sibling-rule, confirmed at the harness level

Plan 29's outcome named the pattern *"a fix lands at the site the finding named
and not at its siblings."* This audit confirms it at the largest scale yet:
**three of the four Round-1 findings are claude-side audit fixes that were
never ported to the kimi harness.**

| Pass-3 finding | The fix it mirrors | Where the original landed |
|---|---|---|
| F51 — kimi `reap()` drains before `Wait()` | F24 lost-tail protection | `core/internal/agent/agent.go:1562-1583` |
| F52 — kimi ACP writes have no deadline | F9 `deadlineWriter` / `writeBroken` | `core/internal/agent/agent.go:462-501` |
| F53 — `askCoworkEnable` parks 8 min with no UI | F35 pass-3 zero-delivery refusal | `core/cmd/akcore/handlers.go:3348-3367` |

The kimi harness was built for capability parity (plans 14/15/16); the
hardening parity did not come with it. **Rule going forward: any fix to
`core/internal/agent/` is not done until `core/internal/kimi/` has been checked
for the same class, and vice versa.** Add it to the review checklist; the two
supervisors are now large enough that "the other harness" is the sibling most
often missed.

---

## The findings, clustered

Sequenced by severity and blast radius, not report order.

| Round | Findings | Why it leads |
|---|---|---|
| **1 — lifecycle correctness (core)** | F51, F52, F53, F54 | An agent that loses its final events, cannot be interrupted, or outlives its own destruction is the stability headline of the refactor. |
| **2 — bounded reads (UI)** | F55, F56, F57 | The F11 class — *bound what is read, not what is admitted* — reopened in the file viewers, reachable with no install step. |
| **3 — hot-path scaling (UI + core)** | F58, F59, F60, F61, F62 | Work that scales with session *history* on per-keystroke / per-poll cadences. The performance half of the refactor's goal. |
| **4 — authority surface & hygiene** | F63, F64, F65, F66, F67 | Residuals: one unbound mutable surface, one missing timeout, one `safe.Go` violation, one semantic decision owed. |

---

## Round 1 — lifecycle correctness *(core, stability)*

1. **F51 (high) — kimi `reap()` waits for the output drains *before*
   `cmd.Wait()`; the F24 tail protection is wired backwards.**
   `core/internal/kimi/thread.go:2351-2367` (correct order:
   `core/internal/agent/agent.go:1562-1583`). The drain channels close only on
   EOF, which happens only when the process dies — so the wait launched at
   thread start (`thread.go:757`) always burns the full 5 s grace and logs two
   spurious warnings, and at *actual* exit the deadline is long spent: after
   `Wait()` returns there is no drain wait at all. The ACP read loop can still
   be translating the final turn's frames while `reap` marks the thread dead,
   emits `exited`, and closes `t.logFile` (`thread.go:2397-2403`) — and
   `emitEvents` silently drops events on a nil `logFile`
   (`thread.go:2408-2424`), which for kimi is the **only** transcript.
   Final-turn events can arrive after `exited`, or be lost outright.
   **Fix:** mirror agent.go — `Wait()` first, then the two drained channels
   under the shared `drainGrace` deadline, then mark dead/emit. Test: scripted
   fake `kimi acp` that writes a final frame and exits; assert the frame lands
   in the JSONL log before `exited`.

2. **F52 (high) — kimi ACP writes have no deadline: a wedged `kimi acp` wedges
   Interrupt, and Stop's kill path behind it.**
   `core/internal/kimi/acp.go:241-249` (`write` holds `wmu` around a plain
   `Write`), called from `Interrupt` at `core/internal/kimi/thread.go:1552` —
   where the blocking `session/cancel` notify runs *before* the signal-backstop
   goroutine is armed (`thread.go:1552-1583`). A child that stops draining
   stdin with a large frame in flight (any base64 image overflows the 64 KiB
   pipe buffer) parks every writer on `wmu` forever; the backstop is never
   armed, so Esc does nothing, `abortThenClose` (`thread.go:1666`) blocks
   before `closeStdin`, and `StopAll` at shutdown hangs until the UI SIGKILLs
   the core.
   **Fix:** `SetWriteDeadline` on the stdin pipe with a `writeBroken` latch,
   exactly as F9 did for claude; arm the interrupt backstop *before* the
   notify. Test: fake child that never reads stdin; assert Interrupt returns
   within the deadline and the kill backstop fires.

3. **F53 (medium) — `askCoworkEnable` parks for the full 8-minute timeout when
   no UI is connected.** `core/cmd/akcore/cowork_enable.go:293-311` discards
   the `NotifyUI` delivery count; the twin fix in `askHumanPermission`
   (`handlers.go:3348-3367`, F35 pass 3) was missed here and at the Cowork
   consent authority (`core/internal/cowork/consent.go:288`). With the UI
   crashed or headless, `enable_cowork` or a `cowork:true` launch parks a
   bridge handler goroutine for the whole `permissionTimeout`.
   **Fix:** fail closed in ~1 ms on a zero delivery count, with the
   established "no window connected" wording, in both places.

4. **F54 (medium) — `agent.discard` removes the worktree without waiting for
   the stopped process to exit.** `core/cmd/akcore/handlers.go:1471-1495`.
   Graceful busy-stop can take ~11 s; the next statement is
   `worktree.Remove(wt)` — `git worktree remove --force` + `branch -D`. The
   sibling `agent.stopClose` polls up to 6 s for exit before archiving
   (`handlers.go:1026-1028`); discard doesn't wait at all. The still-live
   agent keeps running with its cwd deleted, and its still-authenticated
   bridge can keep messaging siblings through persisted `ParentThreadID` after
   the human believes it destroyed.
   **Fix:** poll `d.agentRunning` with a deadline after `agentStop`, as
   stopClose does; on timeout, SIGKILL the process group via the supervisor
   rather than proceeding blindly.

## Round 2 — bounded reads *(UI, stability)*

`EditorArea::openFile` reaches all three synchronously on a project-tree
double-click — and again on every file-watcher reload, so an agent rewriting
the file re-triggers them. Files only have to sit in a project the user opens.

5. **F55 (medium) — CSV/XLSX viewers read entire files (and decompress entire
   zip entries) uncapped, on the GUI thread.** `ui/src/CsvView.cpp:129-145`
   (`QTextStream::readAll()`, full parse), `ui/src/XlsxReader.cpp:18-25`
   (`KArchiveFile::data()` decompresses each entry fully, no cap), reached from
   `ui/src/EditorArea.cpp:511-513`. A 400 MB CSV is a multi-second freeze and a
   possible OOM kill; a corrupt `.xlsx` with a gigabyte-scale
   `sharedStrings.xml` is the zip-bomb shape with no install step. This is
   F11's class, fixed in `AttachmentBuilder` but not here.
   **Fix:** `QFileInfo::size()` first and refuse/truncate with the
   `readCapped` idiom (`AttachmentBuilder.cpp:65-68`); in `XlsxReader`, compare
   `KArchiveEntry::size()` against a cap before `data()`, and cap rows/cells
   per sheet.

6. **F56 (medium) — `XlsxReader::columnIndex` lets a crafted cell reference
   force an unbounded append loop.** `ui/src/XlsxReader.cpp:29-39`, padding at
   `:94-96`. `<c r="ZZZZZZ1">` yields `col` ≈ 3·10⁸ and the padding loop
   appends hundreds of millions of empty `QString`s; a few more letters
   overflows the `int` accumulator (UB). An agent with file tools can produce
   the file.
   **Fix:** clamp to OOXML's own maximum (16384 = "XFD"), break accumulation
   past the clamp so the `int` can never overflow, skip out-of-range refs.
   Test fixture: a minimal crafted sheet — pin a real wire shape, not an
   invented one.

7. **F57 (low) — SubAgentTranscriptDialog markdown-renders per poll, and polls
   forever.** `ui/src/SubAgentTranscriptDialog.cpp:297-300, 310-392`: up to
   1 MB of JSON-parse + `markdownToHtml` on the GUI thread every 1.2 s, with no
   terminal detection — the timer runs at full cadence even after the sub-agent
   finished.
   **Fix:** stop or back off the poll once the owning job is reported finished
   (the Jobs feed already knows); plain-text render for lines over a size
   threshold.

## Round 3 — hot-path scaling *(UI + core, performance)*

The shared shape: work proportional to the session's *history* runs on a
per-keystroke or fixed-poll cadence, so the cost grows while the activity
stays flat.

8. **F58 (medium) — find-in-conversation rebuilds every row's search text on
   every keystroke.** `ui/src/AgentPanel.cpp:3331-3336` calls
   `m_model->searchText(row)` for all rows per keystroke; a Tool row's
   `searchText` (`ui/src/TranscriptModel.cpp:265-272`) re-joins name + summary
   + detail + the full retained result (up to 128 KB) into a fresh `QString`
   each time; `setFind` (`:302-315`) additionally scans every Message/Note row
   twice per needle change. At the 5000-row cap, the control whose whole point
   is interactivity lags with every character.
   **Fix:** cache each row's lowercased search text, invalidated from
   `touched()` — the same seam the delegate's height cache already uses.

9. **F59 (medium) — WorkflowMonitor re-reads the whole journal and every agent
   transcript tail on every 1.5 s poll, and never stops for a non-terminal
   run.** `ui/src/WorkflowMonitor.cpp:182-186, 311-412`. The journal is
   re-opened and line-parsed in full each refresh, plus up to 64 KB read +
   parse per agent; the poll stops only on Completed/Failed (`:234-236`), so a
   run that dies without writing its final JSON is re-read in full every 1.5 s
   for the life of the panel.
   **Fix:** track a journal offset and parse only appended bytes (the
   `readBoundedTail` helper in `SafeContent` is the existing pattern);
   coalesce `tailActivity` to agents whose file mtime moved; add an
   age/no-change poll stop or mark the run failed when the transcript dir
   vanishes.

10. **F60 (low-medium) — AiInspectorPanel sums cumulative context snapshots as
    per-turn spend — F19's fix is incomplete; the per-thread view is also
    unbounded.** `ui/src/AiInspectorPanel.cpp:271-279` adds kimi's cumulative
    `result`-event usage into session totals with no
    `HarnessTraits::usageReporting` gate, while the sibling fix in
    `AgentPanel.cpp:6446-6489` explicitly guards on it ("summing it produced
    session totals that grew quadratically"). Separately, `m_rows` /
    `m_toolNameById` (`:236-238`) and the per-thread `m_timeline` grow without
    cap — only the all-threads view has one (`kMaxActivityRows`, `:25`).
    **Fix:** route the accumulation through the same `usageReporting` check;
    drop each `m_rows`/`m_toolNameById` entry when its `tool_result` lands;
    ring-cap the timeline.

11. **F61 (low) — `TurnTracker` entries (and their retained `lastText`) are
    pruned only on discard/stopClose.** `core/internal/agent/turnwait.go:50-57,
    187-207`. A plain `agent.stop` leaves the thread's `turnState` — including
    the complete final assistant message, potentially hundreds of KB — in the
    map forever; `Wait` on a never-seen id *creates* an entry, and any
    approved bridge can call `agent.wait` on any id. Monotonic growth over a
    long-lived desktop session.
    **Fix:** cap or drop `lastText` on terminal lifecycle; sweep entries for
    ids no longer in the session store once no waiter is registered.

12. **F62 (low) — `session.Store` archive operations do whole-file I/O under
    the global store mutex.** `core/internal/session/session.go:539-636`.
    `Archive` reads and rewrites the entire `threads-archive.json` (up to
    4 MB) holding `s.mu`; `ListArchived` re-reads the whole file on every
    call, also under the lock. Every other caller — including the relay's
    throttled `sessions.Update` on each turn result — stalls behind that I/O.
    **Fix:** snapshot under `s.mu`, do the archive file I/O after releasing,
    re-acquire only to delete + flush; cache the archive listing.

## Round 4 — authority surface & hygiene

13. **F63 (low) — coop RPCs trust a caller-supplied `owner`.**
    `core/cmd/akcore/handlers.go:1756-1844` (`coop.setOpenFiles`,
    `coop.postNote`, `coop.setPresence`, `coop.claimFile`, `coop.releaseFile`,
    `coop.requestReview`) take `owner`/`author`/`thread` verbatim from params,
    defaulting to `"human"`. A prompt-injected agent can release the human's
    file claims or plant notes authored as "human"; the Cooperation panel the
    human reads will show it. Advisory, so the blast radius is confusion, not
    authority — but after the F13/F36 caller-binding sweep this is the one
    unbound mutable surface left.
    **Fix:** for bridge connections force `owner = ConnRef.ThreadID()`;
    reserve `"human"` for the UI role, as `requireCallerThread` already does
    elsewhere.

14. **F64 (low) — `search.Run` has no timeout or context cancellation.**
    `core/internal/search/search.go:96-167`. Unbounded `rg` on a huge root
    pins an IPC handler goroutine (and one of the connection's 256 dispatch
    slots); a disconnecting client doesn't kill the child.
    **Fix:** `exec.CommandContext` with a sane cap.

15. **F65 (low) — `kde/watch.go:160` launches a raw `go func` without panic
    recovery, against the project's own `safe.Go` rule.** Latent today (the
    body is simple), but it is the single exception to the rule that one panic
    must not take down the daemon and orphan every agent; and a watch dropped
    without `Stop()` parks the pump on `<-raw` forever.
    **Fix:** `safe.Go("kde.actwatch", …)`; close `raw` in `unregisterNonce`.

16. **F66 (low, needs a maintainer decision) — `OnAllClientsGone` counts
    agent-bridge connections, so "don't outlive the UI" doesn't hold while
    agents run.** `core/internal/ipc/server.go:269-271`, wired at
    `core/cmd/akcore/run.go:530-533`. When the UI quits with agents running,
    the core stays headless until every bridge disconnects — potentially
    forever for a resident idle engine. Maybe deliberate (lets threads finish
    and cold-compact), but the run.go comment says "last client", and the
    useful desktop semantic is "last *UI* client".
    **Fix:** decide, then either count only `role == "ui"` connections or
    correct the comment to state the intended behaviour.

17. **F67 (low) — `agent.start`'s success reply is trusted to contain a
    `threadId`.** `ui/src/AgentPanel.cpp:3582-3591`. An empty id leaves the
    user's message committed to the feed, `m_pendingOpening` latched forever
    (no `_lifecycle/started` can arrive for an empty id, and `onNotification`
    drops everything while the id is empty, `:4923-4925`), and the F37
    give-the-prompt-back path never runs because it fires only on `error`.
    **Fix:** treat an empty `threadId` as a start failure: error note +
    `restoreUnsentToComposer()`, same as the error branch.

---

## Carried open from prior audits (verified still present)

Not re-filed as findings; listed so the rounds can pick them up or explicitly
hand them off.

- **Transcript replay renders the entire history synchronously**
  (`ui/src/AgentPanel.cpp:2352-2354`). The real fix is chat virtualization —
  **[plan 10](10-panel-responsiveness.md) Phase 2**. Do not patch locally;
  schedule plan 10 P2.
- **Find-highlight HTML rebuilt per paint per matching row**
  (`ui/src/TranscriptDelegate.cpp:218-233`) — lands naturally beside F58's
  search-text cache (Round 3).
- **Selection overlay closed by every find keystroke**
  (`TranscriptModel::setFind` full-range `dataChanged` →
  `ui/src/AgentPanel.cpp:667-673`) — same; narrow the `dataChanged` range when
  the cache lands.
- **LSP hover renders markdown in an unguarded `QTextDocument`**
  (`ui/src/lsp/LspHoverProvider.cpp:86-91`) — swap to `GuardedTextDocument`;
  small enough to ride Round 2.

The plan-29 ranked backlog (items 1–9: untested UI files, `basisCallerBound`
probe, Cowork handler-drive extension, source-scanning Go tests, per-card
rate-limit state, escape detection, `enable_cowork` digest, bare
`send_agent`/`wait_agent` labelling, smaller folds) **stands as written**;
nothing in this audit displaces it. The plan-27 §1 hand-off item is now moot —
the KActionCollection refactor landed in `18d9f6f`.

## What the audit checked and found clean

So the next pass does not re-tread it:

- **claude supervisor lifecycle** (`agent.go`): dup-registration refusal, F24
  pipe ownership, F9 write mutex/deadline + `writeBroken` latch, interrupt
  backstop with stale-pgid guard, control-request waiter cleanup in `reap`,
  hot-compact sharing via `sync.Once`.
- **IPC layer both sides**: per-conn outbound queue with response-vs-
  notification backpressure, per-conn dispatch semaphore, oversize-frame
  draining, `MarkUI`'s atomic single-UI decision, panic recovery per handler;
  `FrameReader` cap/oversize-drop/squeeze, `CoreClient` guarded pending
  replies, disconnect drain, handshake watchdog.
- **Orchestration authority** (`authority.go`, `orchestrate.go`,
  `bridgeauth.go`, `mcpactivity.go`): worker-slot reservation ledger atomic
  with the live count, permission-mode ranking fails closed both directions,
  restriction inheritance normalizes before comparing, bridge secrets
  per-bridge, constant-time, replay-gated.
- **`TurnTracker` broadcast/timeout mechanics**, **RateWaker**, **exit-
  compaction tracker**, **kimi turn accounting** (`activePrompts`, internal-
  turn abandon/preempt/drop-latch owner ids), **gitstatus cache**, **coop
  state**, **kimi translator**, **usage/tool meters**, **compact/summary
  store**, **pointer guard→fire section**.
- **UI streaming transcript**: stable-key addressing survives eviction,
  per-row `heightInvalidated`, capped doc/height caches, all painted documents
  are `GuardedTextDocument`s, escaped streaming paints, begin/end insert-remove
  pairs correct.
- **UI object lifetime**: no context-less `connect`/`QTimer::singleShot` in
  `ui/src`; reply callbacks context- or `QPointer`-guarded.
- **KActionCollection refactor** (`18d9f6f`): palette availability check,
  `QPointer`-guarded panel commands, per-slot rail ids.
- All pass-1/pass-2 fixes spot-verified still hold (F11, F12, F19-at-source,
  F22, F24, F28, F31, F37, F39, F40, F43, F44, F45, F48, F50).

---

## Conventions this remediation holds

Carried from plan 29, restated because the verifiers keep earning them:

- **Fail closed.** Where evidence is missing, refuse.
- **Every behavioural fix carries a test that fails if the logic is
  inverted.** Mutation testing is mandatory; the mutation output goes in the
  round report.
- **Adversarial verification is not optional.** Verdicts: CLOSED / MOVED /
  PARTIAL / UNFIXED / REGRESSED. MOVED is hunted for deliberately — and this
  audit adds the harness-level variant: **every core fix is checked against
  the sibling harness before it can be rated CLOSED.**
- **Fixtures pin real wire shapes.** The F56 test uses a real (minimal,
  crafted) `.xlsx`, not an invented byte string.
- **`AgentPanel.cpp` is the bottleneck file.** One agent owns it per round;
  other agents are forbidden from touching it.

## Suggested round order and gates

1. **Round 1** (F51–F54) — core-only, disjoint files except `handlers.go`
   (F53, F54 — same agent). Gate: `go vet`, `gofmt`, `go test -race ./...`,
   plus the new lifecycle tests.
2. **Round 2** (F55, F56, F57 + LSP-hover carry-over) — UI viewers; no
   overlap with `AgentPanel.cpp` beyond F57's dialog, so the Round-3 owner
   holds the panel file. Gate: ctest.
3. **Round 3** (F58, F59, F60 + the two find/paint carry-overs; F61, F62
   core-side in parallel) — `AgentPanel.cpp` single-owner round. Gate: ctest +
   `go test -race`.
4. **Round 4** (F63–F67) — small, mostly disjoint; F66 waits on the
   maintainer decision. Gate: full suite + convergence verifier over the whole
   plan, with the sibling-harness check applied to Rounds 1–3.
