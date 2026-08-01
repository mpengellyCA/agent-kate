# 19 — Jobs panel & attachment durability

Two dogfooding findings from 2026-07-31, unrelated in the UI but both about *state
that outlives the moment it was created*: background jobs that never stop being
drawn, and attachments whose on-disk origin disappears under them.

## Why

**A. The background-job chip row grows without bound and eats the chat.**
`AgentPanel::handleTaskEvent` inserts a chip per background task
(`AgentPanel.cpp:2921`, added to the flow at `:2925`) and **nothing ever removes
one**. `m_bgJobs` only grows; a finished job flips `done = true`
(`:2933-2975`) and keeps its chip forever. After a long session the
`FlowLayout` under the composer is fifty-odd `✓` chips occupying half the
viewport, which is both the reported "too many subagents and jobs" and a
straightforward memory leak of `QPushButton`s.

The data behind those chips is good — it is the *presentation* that has no
lifecycle and no home outside one agent's chat.

**B. Every job surface is per-agent or per-launch.** There is no place that
answers "what is running right now, across all my agents":

| Surface | Where it lives | Reach |
|---|---|---|
| shells + async subagents | `AgentPanel::m_bgJobs` (`AgentPanel.h:462-476`) | chip row in one agent's chat |
| Workflow runs (phases, sub-agents, tokens, live activity) | `WorkflowMonitor` (`WorkflowMonitor.h:34-80`) | modal off one tool row (`AgentPanel.cpp:3101-3106`) |
| subagent transcripts | `agent.subagentTranscripts` → `SubAgentTranscriptDialog` | "Helpers ▾" menu (`AgentPanel.cpp:1129-1182`) |
| orchestration workers | roster nesting + `⇄` badge (plan 16 P5) | roster tree |

And there is already a **registered-but-stubbed panel for exactly this**:
`MainWindow.cpp:259-265` puts key `tasks` on the bottom strip with
`StubPanel("Tasks / Hooks", "Background tasks, hook runs, and queued work appear
here.")`. It is the last remaining `StubPanel` in the UI.

**C. An attached file's origin path is dereferenced long after the send.**
The reported symptom was "screenshots may be deleted before they are uploaded".
The upload is in fact safe — `buildPathAttachments` `readAll()`s the file at
attach time (`AttachmentBuilder.cpp:60`) and base64s it (`:81`); nothing
downstream reopens the path (`agent.Attachment`'s own comment,
`core/internal/agent/agent.go:91-96`, says `Path` is "UI-side provenance the
harness never sends to the model"). But three *path*-keyed behaviours break the
moment a temp file is reaped:

1. `TranscriptDelegate.cpp:321-338` re-loads the chip thumbnail **from the
   path**, because `compactAttachments` (`AgentPanel.cpp:2805-2824`) strips
   `dataB64` off the You card. Deleted file → generic `🖼` glyph, permanently.
2. `AgentPanel::openAttachment` (`:2842-2856`) refuses with "the file has moved
   or been deleted since it was attached."
3. Replay has the same hole, since the core sidecar persists path-only
   (`core/internal/session/attachments.go:22-29`).

**D. No total attachment budget, against a hard frame cap.** There is a 5 MB
*per-image* limit (`AttachmentBuilder.cpp:74`) but no cap on the sum, while a
single JSON-RPC frame is capped at 16 MB (`core/internal/ipc/server.go:19-21`).
Overflow is not a clean error: `bufio.Scanner` stops, the read loop exits and the
core logs `connection read error` and drops the UI connection
(`server.go:178-202`). Four 5 MB images, or ~15 large screenshots in one message,
reach that cliff.

## Design

### Feature 1 — a real Jobs panel (fills the `tasks` stub)

`ui/src/JobsPanel.{h,cpp}`, bottom strip, relabelled **Jobs**. Aggregates
across every agent and project:

- **Rows**: agent (top level) → its jobs (children). Columns *Job / Kind /
  State / Elapsed*. A header line summarises `N running · M finished · K agents`.
- **Sources**, all already modelled — the panel adds no new core plumbing:
  - background shells + async subagents, from `AgentPanel::m_bgJobs`;
  - the agent's Workflow run, from the existing `WorkflowMonitor` state.
- **Filters**: All / Running / Finished, plus a text filter.
- **Clear finished**: drops finished rows (and their retained records).
- **Open** (double-click or button) reuses today's dispatch exactly —
  `local_bash` → open the output file in the editor; a subagent → live
  `SubAgentTranscriptDialog`; a workflow → `WorkflowMonitorDialog`.
- **Go to agent** focuses the owning agent in the roster.

Transport: `AgentPanel` gains `jobsChanged(threadId, QVector<AgentJob>)`;
`AgentDock::wireAgentPanel` forwards it (and emits an empty list on agent
close so rows can't outlive their agent); `MainWindow` feeds the panel and
pushes `agentTitlesChanged` (already emitted for the WorktreeDashboard) so rows
are named by agent rather than by thread id.

`struct AgentJob` lives in `ui/src/state/AgentJob.h` so `AgentPanel` and
`JobsPanel` share it without a circular include.

### Feature 2 — bound the chip row

The chip row keeps its purpose (immediate, in-context "something is running")
and loses its unbounded tail:

- chips exist for **running** jobs only; a job that finishes deletes its chip;
- a single trailing summary chip `✓ N finished` raises the Jobs panel
  (`openJobsPanelRequested` → `MainWindow::raisePanelByKey`);
- `m_bgJobs` retains finished records (the panel lists them) but is capped,
  dropping oldest-finished beyond `kMaxRetainedJobs`.

### Feature 3 — durable attachment previews

Add a **cache copy alongside** the origin path rather than replacing it, so
clicking a chip for a real workspace file still opens *that* file in the editor:

- in `buildPathAttachments`, after the existing `readAll()`, write image bytes to
  `CacheLocation/attachments/` and set `att["cachePath"]`; `att["path"]` keeps
  meaning "where it came from". This mirrors the tool-result image cache
  (`AgentPanel.cpp:4173-4205`), which is the established precedent.
- `compactAttachments` carries `cachePath` through to the You card.
- `TranscriptDelegate` thumbnail and `openAttachment` fall back to `cachePath`
  when `path` is gone.
- Core persists it for replay: `CachePath` on `agent.Attachment` and
  `session.AttachmentMeta`. It stays body-free — a path, not bytes.

### Feature 4 — total attachment budget

`buildPathAttachments` tracks the running encoded total (existing attachments
included) against `kMaxTotalAttachBytes = 12 MB`, comfortably inside the 16 MB
frame once the rest of the JSON is counted, and skips with a plain reason
("would exceed the 12 MB total attachment limit") instead of letting the send
kill the core connection.

## Phases

- **P1 — attachment durability + budget** (Features 3 + 4). ✅ **LANDED.**
  `AttachmentBuilder` writes a content-addressed copy of each image into
  `CacheLocation/attachments/` (re-attaching the same file reuses one copy; a
  truncated write is removed rather than left to be drawn) and tracks a running
  encoded total across repeated attach actions, not just within one. The
  delegate and `openAttachment` fall back to `cachePath`; `compactAttachments`
  carries it onto the You card; `agent.Attachment` and `session.AttachmentMeta`
  persist it so replay keeps it too. Sidecar stays body-free — a path, not bytes.

  Where the sketch met the real code:
  - **The reported bug was not the real one.** "Screenshots may be deleted
    before they are uploaded" is false — bytes are captured at attach time and
    the send was never at risk. What breaks is the *thumbnail*, *chip-open* and
    *replay*, all of which re-dereference the path afterwards. Fixing the
    upload would have been fixing nothing.
  - **Caching had to be additive.** Repointing `path` at the copy would have
    been simpler and wrong: clicking a workspace file's chip must open the real
    file, where edits count. Hence a second key rather than a substitution.
  - **A real adjacent bug fell out**: a 5 MB per-image cap with no cap on the
    sum, against a 16 MB frame limit whose overflow drops the UI connection
    instead of failing the request.

- **P2 — Jobs panel + bounded chip row** (Features 1 + 2). ✅ **LANDED.** UI-only.
  `JobsPanel` fills the `tasks` stub (relabelled **Jobs**); the tray draws
  running work only plus a `✓ N finished` chip that raises the panel.

  Where the sketch met the real code:
  - **The chip flood was a leak, not just clutter.** Chips were created and
    added and *never removed* — `m_bgJobs` only ever grew, so a long session
    leaked a `QPushButton` per background task and buried the composer. The
    tray is now rebuilt from the map (the pattern the queue/attachment bars
    already use) rather than holding chip pointers, because with chips coming
    and going a retained pointer to a deleted one is a crash.
  - **Insertion order had to be added.** `QHash` iteration is unordered, so the
    old bar's chip sequence was incidental; `BgJob::order` makes it real.
  - **"Clear finished" cannot be local.** The panel mirrors snapshots the agents
    publish, so clearing its own view would be undone by the next snapshot. It
    is routed to every agent's `forgetFinishedJobs()` instead — asserted by
    `JobsPanelTest::clearFinishedIsRoutedNotLocal`.
  - **Panels wired mid-flight would start empty**, since the panel only learns
    of jobs through change events; `wireAgentPanel` now calls `republishJobs()`.
  - **Rows must not outlive their agent**: `removeAgentEntry` publishes an empty
    set before the panel dies — the single choke point both close paths share.
  - **Rebuilding the tray on every tick would flicker.** The 15 s timer exists
    only to move a *minute*-granular elapsed suffix, so the tray keys on a
    fingerprint (running ids + output paths, finished count, workflow state) and
    relabels chips in place when it is unchanged — no relayout and no
    re-publish. The workflow had to be in that fingerprint despite having no
    chip: it is published to the panel, so without it the panel would freeze on
    a stale row for the whole run.
  - `StubPanel` now has no users anywhere. Its dead include is gone; the class
    is left as the scaffold for the next placeholder.

- **P3 — stability hardening (fleet review, 2026-08-01).** ✅ **LANDED.** A
  6-slice adversarially-verified review of P1+P2 (every non-minor finding
  independently re-derived by a skeptic before being acted on), then two fix
  rounds, the second reviewed by a second fleet. What it changed:

  **The frame-cap crash became a non-event at every layer.**
  - The core's IPC read loop (`core/internal/ipc/server.go`) no longer dies on
    an oversize line: a per-connection `frameReader` drains it, answers with an
    RPC error when the request id is recoverable (conservatively — never a
    nested or probe-truncated id, since a wrong id would resolve someone
    else's call), and keeps serving. Previously one bad frame killed the
    connection and, via `OnAllClientsGone`, the entire core with every agent.
    The Go *client* read loop had the same fatal scanner and got the same
    reader — an oversize `cowork.screenshot` result used to wedge the bridge.
  - The UI enforces the budget where frames are made, not only where
    attachments are built: `wouldOverflowFrame` (14 MiB on params, nesting
    under `CoreClient`'s 15 MiB whole-frame refusal, under the core's 16 MiB
    cap) guards the immediate send, the *queue* branch, and queued delivery —
    and a refused queued head is removed rather than left to block the queue.
    `restoreQueuedToComposer` re-budgets the merged set (composer's own
    attachments first, dedup before budgeting, dropped names in the notice);
    `buildItemAttachments` gained the same 256 KB truncation + shared budget
    as whole files; costs are true wire bytes (`QJsonDocument` compact
    serialisation), not UTF-16 code units.
  - After a drop the UI now *recovers*: `CoreClient` runs a bounded
    respawn/reconnect ladder (5 rounds, doubling backoff, handshake watchdog,
    `m_shuttingDown` gates so it never fights a quit) with a persistent
    banner. A **respawned** core is distinguished from a reconnect
    (`reconnected(bool coreRespawned)`): the dock replays the `_lifecycle`
    exited event into every live panel so agents settle into the normal
    dormant/resumable presentation instead of ghost-running against a core
    that never heard of them.
  - The crash observed on 2026-08-01 was the *installed* 0.1.257 package
    predating P1's budget — but three genuine bypasses (queue restore, item
    excerpts, uncounted message text) existed in-tree and are closed.

  **Jobs publish/reap got an architecture instead of patches.** The tray
  fingerprint now gates only chip rebuild/relabel; publishing compares the
  built `AgentJob` vector (field-complete `operator==`, deterministic sort)
  against the last published one, so finished-job mutations (late
  `outputFile`) and `republishJobs()` actually publish. `BgJob` carries
  `failed` (terminal status applies regardless of which event arrives first;
  the note stays one-shot via `noted`), the workflow row has a start time
  (zeroed on replay) and an id fallback, `forgetFinishedJobs` can clear a
  terminal workflow via `m_workflowForgotten`, and reaping keys on
  `publishedThreadId()` — the live id is already cleared on the Stop & close
  path, which stranded rows forever. Thread-id churn reaps the old id before
  publishing under the new one. Titles reach the panel without the git RPC
  (`pushAgentTitles` on rename/wire), and a job's file opens in the *owning*
  agent's editor group.

  **Smaller but real:** `cacheImageCopy` is atomic (`::rename`, flush-checked,
  size-verified reuse so a truncated copy self-repairs) and runs after the
  budget check; CoworkPortal routes all four portal failure paths through
  `failPortalStep` (detail appended centrally, decline wording softened,
  missing-backend detected by D-Bus error *name*), and `failInjectQueue`
  closes the half-built session instead of leaking it.

  **Deliberately deferred** (pre-existing, want their own pass): the blocking
  `bus.call` on the GUI thread in `portalRequest`, and a11y flags restored
  only in `~CoworkPortal`.

## Non-goals

- Clipboard image paste and raw (non-file-URL) image drops. Both are real gaps
  — `canAcceptDrop` (`AgentPanel.cpp:3277-3294`) takes local-file URLs only and
  there is no `QEvent::Paste` handler on the composer — but they are new input
  features, not this fix.
- Pruning the `tool-images` cache dir (nothing prunes it today either).
- Surfacing orchestration workers in the Jobs panel: they are agents, and the
  roster already nests and badges them (plan 16 P5). Duplicating them here
  would blur "job" and "agent".

## Verify

- `ui/tests/JobsPanelTest` — grouping by agent, a snapshot replacing only its
  own agent's rows, an empty snapshot reaping an agent, both filters (including
  matching on the *agent's* name), clear-finished being routed rather than
  applied locally, the three-way Open dispatch (shell → file with owning
  thread, workflow → monitor, subagent → transcript), and an id-less job still
  being a job row.
- `ui/tests/AttachmentBuilderTest` — the cached copy survives the origin being
  deleted and is byte-identical; a truncated cache copy is repaired on
  re-attach; it never displaces `path` for a workspace file; text attachments
  get no copy; the total budget rejects with an actionable reason and
  accumulates across calls; item excerpts are truncated and budgeted.
- `core/internal/ipc` — an oversize frame is survivable on the same
  connection (with and without a recoverable id), `frameID` refuses nested and
  probe-truncated ids, a 5 MiB frame round-trips byte-for-byte through the
  chunked reader, and the exact cap/CRLF/unterminated-EOF boundaries hold.
- Full suite green (9 ctest targets, `go build ./...` + `go test ./...`), and a
  20 s offscreen run of the real `agentkate` against a throwaway XDG home with
  **zero** Qt warnings or asserts (P3 re-verified all of the above).
