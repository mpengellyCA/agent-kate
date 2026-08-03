# 31 — Refactor review remediation: linearizable lifecycle, durable recovery, and quiet hidden views

**Status: PLANNED.** Source: independent read-only review of the substantive
commits made on 2026-08-01 and 2026-08-02 (through `8acafb6`). This is a
remediation plan, not a feature expansion. It records five defects found after
the large harness, IPC, session, Cowork, and UI refactor.

The ordering is by authority/data-loss blast radius. Each change must preserve
the existing security properties: a human stop must remain authoritative,
archive must be recoverable, and a failed desktop integration restore must fail
safe rather than silently changing the user's desktop.

## Findings and order

| Order | Finding | Severity | Introduced by | Why it leads |
|---|---|---|---|---|
| 1 | R1 — rate-wake cancellation race | High | `0749ed2` | A stopped, closed, or shutdown agent can relaunch itself. |
| 2 | R2 — archive/update lost write | High | `8acafb6` | Session/lifecycle metadata can be silently discarded. |
| 3 | R3 — a11y crash-recovery record cleared too early | High | `adb0505` | Screen-reader flags can remain changed without recovery data. |
| 4 | R4 — hidden transcript still renders watcher changes | Medium | `d5aa74a` | Background GUI work scales with sub-agent output despite hiding the view. |
| 5 | R5 — synchronous a11y D-Bus calls on GUI path | Medium | `adb0505` | A wedged session bus can freeze a normal user action for seconds. |

---

## R1 — Make rate-window wake cancellation linearizable

`RateWaker.mature` currently removes the wake and releases its mutex before it
invokes `Fire`. A concurrent `Cancel` or `Stop` sees no armed entry and cannot
prevent that already-matured callback from reaching `resumeThread`.

Affected paths:

- `core/internal/schedule/ratewake.go` (`mature`, `Cancel`, `Stop`)
- `core/cmd/akcore/ratewake.go` (`fireRateWake`)
- manual resume, stop, close, discard, and graceful shutdown callers

**Required design:** give every armed wake a generation/epoch and retain enough
state to invalidate an in-flight fire. `Cancel` and `Stop` must invalidate the
generation; firing must re-check that generation immediately before harness
launch. Shutdown must invalidate all wakes and wait for active fire callbacks
before its final `StopAll` sweep finishes. Do not hold the waker mutex while
calling into a harness.

**Acceptance tests:**

- A deterministic timer matures, then `agent.stop` wins before the callback's
  final launch check; no launch occurs.
- The same holds for manual resume, close, discard, and global shutdown.
- Shutdown cannot return with a wake callback that can subsequently start an
  agent.
- A non-racing due wake still resumes exactly once and preserves the thread's
  persisted permission mode.

---

## R2 — Serialize archive against per-thread updates

`Store.Archive` snapshots a record under `s.mu`, drops that lock for archive
I/O, then deletes the current live record. An `Update` between the snapshot and
the delete is persisted briefly but absent from the archive and then removed.

Affected path: `core/internal/session/session.go` (`Store.Archive`, `Update`).

**Required design:** introduce a per-thread archival state/lock or a versioned
compare-and-commit protocol shared with `Update`. Archive I/O may remain off the
global store lock, but it must commit the exact version it archives and prevent
or retry a concurrent update. Never restore global-lock contention merely to
avoid the race.

**Acceptance tests:**

- Pause archive after its snapshot; update session/lifecycle metadata; release
  archive; assert either the update is included in the archive or the archive
  retries on the newer version.
- An archive write failure leaves the current live record intact.
- Concurrent archives of different threads do not serialize on unrelated
record-level work.

---

## R3 — Keep a11y recovery data until restoration is confirmed

Crash recovery writes the original `org.a11y.Status` values, issues two
asynchronous `Set` calls, then clears the only durable record. If the process
or session bus fails before delivery, Agent Kate has lost the values needed to
put the desktop back.

Affected path: `ui/src/cowork/CoworkPortal.cpp` recovery of persisted a11y
originals.

**Required design:** retain the record until both asynchronous restore replies
succeed. If either fails, preserve it for the next startup recovery attempt.
A bounded synchronous restore is acceptable only for the true teardown path and
must not clear the record on a failed/timeout reply. Keep the existing
foreign-owner rule: one process must not clear another process's recovery
record.

**Acceptance tests:**

- Simulate delivery failure after the first/second `Set`; assert the persisted
record remains.
- Successful restoration clears the record exactly once.
- A later startup retries a retained record and preserves foreign ownership.

---

## R4 — Do no transcript rendering while the dialog is hidden

`SubAgentTranscriptDialog::hideEvent` stops polling but its
`QFileSystemWatcher::fileChanged` connection still calls `pullNew`, which reads,
parses, and updates the text browser while hidden.

Affected path: `ui/src/SubAgentTranscriptDialog.cpp`.

**Required design:** while hidden, watcher events may mark the dialog dirty or
maintain only a cheap coalesced flag; they must not read, parse markdown, or
mutate the view. `showEvent` performs one bounded catch-up pull and clears the
flag. Preserve path re-arming for visible dialogs and the existing bounded-tail
behaviour.

**Acceptance tests:**

- Hide the dialog, emit repeated watcher changes, and assert no transcript read
  or document update occurs.
- Show it and assert exactly one catch-up read renders the accumulated tail.
- Visible dialogs still re-arm a watcher after atomic file replacement.

---

## R5 — Remove synchronous a11y reads from normal GUI actions

The Cowork enable path synchronously reads two status properties and verifies
them with another blocking read. The bounded two-second call is still a
multi-second GUI freeze when the session bus is wedged. Teardown has a separate
bounded synchronous requirement, but normal launch does not.

Affected path: `ui/src/cowork/CoworkPortal.cpp` (`enableAtspiStatusForLaunch`,
`verifyAtspiEnabled`).

**Required design:** capture/verify through asynchronous calls with an explicit
pending state. Do not launch the remote-control flow until the required status
is actually confirmed, and surface timeout/failure honestly. The durable
original-state record from R3 must be written before the first mutating request
and retained on failure.

**Acceptance tests:**

- A stalled a11y service returns control to the GUI event loop without a
blocking call.
- Normal successful enable verifies the state before dependent Cowork actions
become available.
- A failed enable leaves the recovery record available and reports a usable
error instead of claiming accessibility control is enabled.

## Integration gate

Before marking this plan landed:

1. Run focused Go race tests for `internal/schedule`, `cmd/akcore`, and
   `internal/session`.
2. Run affected Qt tests with an available D-Bus/offscreen fixture; add the
   new deterministic fake-service coverage above.
3. Run `go test ./...`, the configured UI build/ctest suite, and
   `git diff --check` in an environment that permits AF_UNIX test sockets.
4. Update plans 28 and 27/Cowork documentation only where the corrected
   lifecycle or a11y behaviour changes a user-visible promise.
