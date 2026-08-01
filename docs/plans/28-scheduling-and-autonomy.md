# 28 — Native scheduling and resume: long-running autonomy without self-escalation

**Status: PLANNED.** Covers IDEAS #15 (extended timer and resume support),
added to IDEAS.md on 2026-08-01 after the rest of this program was clustered.
Program context: [20-approved-features-program.md](20-approved-features-program.md).

**Size: L.**

> **From the idea, written from direct experience:** *"driving a multi-hour
> autonomous program from inside a Claude Code session hits hard limits —
> in-session wakeups cap at 1 hour and die with the session, rate-window
> exhaustion stalls everything until a human returns, and the workaround (a
> systemd timer resuming the session headlessly with `--permission-mode
> bypassPermissions`) is exactly the kind of ungated self-escalation that
> permission classifiers rightly block."*

## Why

That paragraph contains the whole argument, and its last clause is the design
constraint. There are three separate failures:

**1. A wakeup cannot outlive its session.** An agent that wants to continue in
three hours has, from inside a session, only in-session sleeps — capped, and
dead the moment the session ends. So long autonomy is impossible from where the
agent stands.

**2. A rate-limit stall is a full stop.** When the five-hour window is
exhausted, every thread waits for a human to come back and re-poke it — even
though the exact reset time is now known. The `rate_limit_event` readout
landing with the current work carries
`{"status","resetsAt","rateLimitType","overageStatus"}`, so the machine has the
one fact it needs to resume itself and does nothing with it.

**3. The available workaround is a self-escalation.** A systemd timer running
`claude --resume … --permission-mode bypassPermissions` is the only way to do
this from outside today, and it is exactly the pattern a permission classifier
should block: work resumes with *more* authority than the human granted, in a
context where nobody is watching. The correct response is not to find a way
around the classifier. It is to make the capability exist **with** the gate.

**AgentKate is the right owner and already most of the way there.** `akcore` is
a long-lived daemon with per-thread permission modes persisted on the record
(`session.Record.PermissionMode`), a resume path that replays applied truth, a
`TurnTracker` that knows when a turn is in flight, a Jobs panel for background
work (plan 19), and `KDBusService` activation for raising the UI when something
needs eyes. Scheduling here is a *policy-respecting* feature; scheduling from a
systemd unit is a bypass.

## Verified facts

| Fact | How verified | Consequence |
|---|---|---|
| `claude` runs its own background daemon: `~/.claude/daemon/{roster.json,dispatch,control.key}`, `~/.claude/daemon.status.json` with a supervisor pid and worker map, a control socket at `/tmp/cc-daemon-<uid>/<hash>/control.sock`, and `~/.claude/jobs/<id>/` | Read from disk this session; `daemon.log` shows `bg spawned`, `bg settled` | The CLI has a background subsystem. It is **undocumented** and its control socket is internal — house rule says it may not be load-bearing. We observe it via `claude agents --json` (plan 21) and own scheduling ourselves |
| `scheduled_task_fire` is a `system` subtype (18 refs in the 2.1.220 bundle) | Bundle strings | The CLI has its own scheduled tasks. Render the subtype in the dispatch table so a CLI-side schedule firing is visible — but do not build on it |
| `rate_limit_event` carries `resetsAt` (unix seconds), `rateLimitType` (`five_hour`), `status`, `overageStatus` | Probed live on 2.1.220 in the audit; emitted unconditionally, no flag | Auto-resume-on-reset needs no new plumbing: the fact arrives on every turn |
| systemd **user** timers work here (`systemctl --user list-timers` shows two) and `systemd-run` is available for transient timers | Run live | The resurrection path is real |
| **`loginctl show-user $USER -p Linger` → `Linger=no`** | Run live | ~~Load-bearing~~ **Moot by design decision (2026-08-01):** the user chose visible-desktop-only execution — nothing fires while logged out, so lingering is never wanted. The fact stays recorded so nobody "fixes" it |
| `session.Record` persists `PermissionMode`, `Env`, `DisallowedTools`, `AddDirs`, provider routing, persona | `core/internal/session/session.go:45-124` | A scheduled resume can replay the thread's exact authority. Nothing needs to be re-decided at 3am |
| `TurnTracker.Wait` is already used to avoid discarding a turn in progress | `core/cmd/akcore/cowork_enable.go` (plan 18) | A timer that fires mid-turn queues rather than interrupts, reusing existing machinery |
| The Cooperation MCP's consent pattern — mandatory `reason`, shown verbatim, human approves, `NOT APPLIED` on refusal | `enable_cowork`, plan 18 | The structural template for the `schedule` tool (mandatory verbatim reason, `NOT APPLIED` vocabulary) — though per the 2026-08-01 resolution the consent *dialog* is dropped: creation is free, control lives in the hard switches |

## The rule this plan exists to enforce

**A scheduled action never has more authority than the human granted the
thread.** Concretely:

- A scheduled resume launches with the thread's **persisted** `PermissionMode`.
  There is no scheduler-side permission override, no `bypassPermissions` flag,
  and no code path that can construct one — enforced by a test, not a comment.
- A scheduled turn that hits a permission prompt **blocks and raises attention**
  exactly as an interactive one does. Unattended does not mean unsupervised; it
  means the human is notified rather than present.
- An agent may **create** schedules freely (resolved 2026-08-01 — no consent
  step), but scheduling grants no authority: the fired turn runs under the
  thread's persisted permission mode, the Phase 3 visibility invariant always
  holds, and the human's **hard switches** (global / per-project / per-agent,
  invisible and immutable to agents) decide whether anything actually fires.

Write this rule at the top of the scheduler package. It is the reason the
feature is legitimate.

## Phase 1 — The timer store and the scheduler loop (core)

New `core/internal/schedule/`:

```go
// Timer is one scheduled action against one thread. Persisted, so it survives
// a UI restart, an akcore restart and a reboot.
type Timer struct {
    ID       string `json:"id"`
    ThreadID string `json:"threadId"`
    // Kind: what fires.
    //   "resume"  — start the thread if dormant (no prompt)
    //   "prompt"  — resume if needed, then send Text as a user message
    //   "wake"    — resume when a rate-limit window resets (When is advisory)
    Kind string `json:"kind"`
    Text string `json:"text,omitempty"`

    // Exactly one of When / Every is set.
    When  time.Time     `json:"when,omitempty"`
    Every time.Duration  `json:"every,omitempty"`
    // NotBefore lets a rate-window wake be re-armed to the exact resetsAt.
    NotBefore time.Time `json:"notBefore,omitempty"`

    // CreatedBy: "user" or "agent". An agent-created timer records
    // the reason it gave, verbatim, so a 3am fire is explicable at 9am.
    CreatedBy string `json:"createdBy"`
    Reason    string `json:"reason,omitempty"`

    Enabled  bool      `json:"enabled"`
    LastFire time.Time `json:"lastFire,omitempty"`
    LastErr  string    `json:"lastErr,omitempty"`
    Fires    int       `json:"fires"`
    // MaxFires caps a repeating timer. 0 = unlimited, and the UI warns.
    MaxFires int `json:"maxFires,omitempty"`
}
```

- Store: one JSONL file beside the session store, with the same
  load/flush/Update discipline `session.Store` uses (`session.go:149-301`) —
  including its atomic-write pattern. Do not invent a second persistence style.
- Loop: a single ticker in akcore, waking on the nearest deadline. **All times
  UTC**, with a monotonic guard: a laptop resuming from suspend must fire the
  timers it slept through **once**, not once per missed interval. That is the
  classic bug in this feature and it belongs in a test.
- Firing: resolve the thread, `TurnTracker.Wait` if busy (queue, never
  interrupt), resume through the ordinary harness path with the record's own
  options, then `Send` for a `prompt` timer. **Every fire emits a `_lifecycle`
  event** so the transcript shows *"resumed by timer 'nightly review'"* — an
  agent that wakes with no idea why is a debugging nightmare.
- A timer whose thread was deleted is disabled with a reason, never dropped
  silently.

New RPCs: `schedule.list` / `.create` / `.update` / `.delete` / `.snooze` /
`.runNow`.

## Phase 2 — Rate-window awareness

The highest-value half, and nearly free once `rate_limit_event` is being read.

- The UI already hoists the latest `rate_limit_info` into a shared reactive
  state (part of the current stream-channel work). Forward it to the core as
  `schedule.noteRateLimit`, or — better — observe it core-side in
  `pumpStdout`'s event path, where the event already passes.
- On `status != "allowed"`: for every thread with queued work, arm a `wake`
  timer at `resetsAt` (plus a small jitter — several threads waking at the same
  instant would re-exhaust the window immediately; stagger them).
- On the next `status == "allowed"`, cancel outstanding wakes: the window may
  reset early, or overage may be enabled.
- Rate limits are **account-wide, not per-thread**. One shared window state, one
  wake schedule, staggered fires. Modelling it per-thread would produce a
  thundering herd.
- Surface it: the panel's rate-limit chip gains *"resumes at 14:37"* when a wake
  is armed, so the user knows the stall is being handled rather than ignored.

## Phase 3 — Visible resurrection: firing when the app is closed

*(Reworked 2026-08-01 per the user's resolutions to open questions 2 and the
follow-up: scheduled work NEVER runs hidden. The user must be logged in and
the AgentKate UI must be on the active desktop, displayed, for a task to run.
There is NO linger path and no headless execution — the resurrection layer's
only job is to put the UI on screen, visibly, and then fire.)*

The visibility invariant, in order of situation:

1. **App open and displayed** (including plan 27 §2's tray-hidden state ONLY if
   the main window is shown when the task actually starts): timers fire
   normally. If the window is tray-hidden at fire time, the fire first raises
   the window (KDBusService activation path), then starts the task.
2. **App closed, session unlocked:** a systemd user timer
   (`~/.config/systemd/user/agentkate-schedule.timer` + oneshot service,
   installed only while at least one schedule exists, removed with the last,
   `OnCalendar` at the next due time) **launches the full AgentKate UI as a
   normal, visible desktop launch** — window on the active desktop, then the
   due task fires with a catch-up notice. It never starts akcore headless and
   never runs a task before the window is up.
3. **Screen locked, user logged in** *(revised 2026-08-01, superseding the
   same-day deferral rule)*: **tasks continue as normal.** Running work keeps
   running, due timers fire, and an app-closed fire still performs the visible
   launch of (2) — the window comes up behind the lock screen and is live the
   moment the user unlocks. Locked-but-logged-in counts as "displayed": the
   session is the user's, the UI is on it, nothing is hidden.
4. **Sleep / standby:** the scheduler arms a **system wake alarm** for the
   nearest due timer so a suspended machine wakes to run it. Mechanism order:
   systemd user timer with `WakeSystem=true`; if the user unit lacks
   `CAP_WAKE_ALARM` (common), fall back to a `timerfd` on
   `CLOCK_REALTIME_ALARM` held by akcore, and degrade honestly to
   fire-on-resume (catch-up semantics, marked in the Timers view) when neither
   is permitted. Verifying which mechanism this machine allows is a Phase 3
   probe task. After wake, the normal path applies — typically (3), since the
   machine wakes to a lock screen.
5. **Logged out:** nothing fires, ever. No `loginctl enable-linger`, offered or
   otherwise — the probe fact that `Linger=no` on this machine is now a
   non-issue because the design never wants lingering. Timers that came due
   while logged out do NOT fire blindly on login (systemd `Persistent=true`
   only triggers the visible launch): the user gets a **catch-up dialog**
   *(added 2026-08-01 per user direction)* listing every missed task —
   grouped by project, then agent, with each row showing the timer's kind,
   prompt text, creator and reason — offering four run scopes:
   - **one by one** — tick individual tasks (with per-row Skip),
   - **all for a project**,
   - **all for an agent**,
   - **run all**.
   Skipped tasks stay listed in the Timers view as "missed — skipped at
   catch-up" (a repeating timer simply re-arms for its next slot; a one-shot
   keeps its record until dismissed). The dialog is the login-time face of the
   same hard-switch philosophy: the human decides what a backlog of scheduled
   work actually does, and agents cannot observe whether their missed fire ran
   now, later, or not at all.

`AgentNotifier` gains a `scheduledResume` event in `ui/agentkate.notifyrc` so
"your agent woke up and is working again" is a notification the user can
independently silence; every auto-launch under (2) also announces itself with
one ("AgentKate opened to run scheduled task: <name>").

## Phase 4 — Agent-requested schedules (free to schedule, hard switches to stop)

*(Reworked 2026-08-01 per the user's resolution to open question 1: NO approval
prompts and NO grant toggle. Agents schedule freely; the human holds hard
kill-switches that agents cannot see.)*

This is what closes the loop on the reported pain: the agent driving a
multi-hour program wants to arrange its own continuation.

New Cooperation MCP tool **`schedule`** (no longer `request_schedule` — there
is no request/consent step), registered like the tools in
`core/cmd/akcore/mcp_cowork.go`:

- Mandatory `when` (absolute or relative), `kind`, `reason`, and for a `prompt`
  timer the `text` it wants sent to itself. The reason is stored verbatim and
  shown in the Timers view — it is documentation now, not a consent pitch.
- The call **always succeeds** (given valid arguments) and creates the timer
  with `CreatedBy: "agent"`. Creation raises a `scheduledCreated` notification
  and the row appears in the Timers view immediately — visibility is the
  control surface, not approval.
- **Hard switches, enforced in the scheduler, invisible to agents:** the user
  can pause/disable firing globally, per-project, or per-agent. A suppressed
  timer still exists and still reads as scheduled from the agent's side — the
  scheduler simply does not fire it while the switch is off, and nothing an
  agent can call reports, overrides, or re-enables a switch. Suppressed rows
  are visibly marked in the Timers view (the human sees everything; the agent
  sees nothing).
- A worker scheduling a *different* thread still needs the orchestration
  relationship that `launch_agent` establishes (it must own or have launched
  that thread) — cross-thread scheduling of an unrelated agent's thread is
  refused with `NOT APPLIED`, the established vocabulary.
- Firing always obeys Phase 3's visibility invariant and the thread's own
  permission mode — free scheduling never widens what a fired turn may do.

## Phase 5 — UI: the Timers view and per-agent controls

- **Timers view.** The Jobs panel (`ui/src/JobsPanel.cpp`, plan 19) is the
  natural host — it is already "background work from every agent". A second
  section: next fire, target agent, kind, the prompt to be delivered, who
  created it and why. Actions: snooze, run now, disable, delete.
  - Flicker rule: `Reactive<QList<Timer>>` with equality over every rendered
    field; the countdown column ticks like `JobsPanel::tickElapsed` already
    does, without republishing the list.
- **Per-agent controls.** In the agent panel's menu: "Schedule…" (a small dialog
  — in N minutes / at a time / every N hours, plus optional prompt text) and
  "Snooze until rate limit resets" when a window is exhausted.
- **Honesty markers, which are most of the UX:**
  - a row whose delivery depends on the app staying open is marked, with the
    reason (no systemd user session — the only degraded case left, since
    visible resurrection covers app-closed and nothing ever fires logged out),
  - an unlimited repeating timer (`MaxFires == 0`) is marked, because that is a
    runaway cost generator and the user should see it,
  - `CreatedBy: "agent"` rows show the agent's reason inline, plus the
    suppressed marker when a hard switch is holding them.
- All actions register with plan 27 §1's `KActionCollection`.
- Composes with plan 27 §2's tray: "2 running · 1 waiting on you · next timer
  14:37".

## Verify

| Phase | What proves it |
|---|---|
| 1 | `go test ./internal/schedule/…`: `TestFiresOnceAfterMissedWindow` (advance a fake clock past three intervals of an hourly timer → exactly **one** fire); `TestDisabledTimerNeverFires`; `TestTimerForDeletedThreadDisablesWithReason`; `TestStorePersistsAcrossReload`. |
| 1 | **The rule, as a test:** `TestScheduledResumeUsesRecordPermissionMode` — a record with `PermissionMode: "default"` fires and the launch spec carries `"default"`. Plus `TestNoSchedulerPathCanSetBypassPermissions`, a source-level assertion (grep the package for `bypassPermissions` → zero hits) wired into the test suite so it cannot be reintroduced. |
| 1 | `TestFireWaitsForTurnInProgress` — a timer firing mid-turn queues and delivers after the `result`, never interrupting. |
| 2 | `TestRateLimitArmsStaggeredWakes` — three stalled threads produce three wakes with distinct times, all ≥ `resetsAt`. |
| 2 | `TestAllowedStatusCancelsWakes`. |
| 2 | Manual: exhaust a window (or inject a synthetic `rate_limit_event` with a near `resetsAt`), confirm the chip reads "resumes at HH:MM" and the thread resumes itself. |
| 3 | `TestSystemdUnitWrittenAndRemoved` — creating the first timer writes the unit, deleting the last removes it, and neither leaves a stale unit behind. |
| 3 | Manual, the real test: set a timer for +5 min, close AgentKate entirely, wait. The full UI launches **visibly on the active desktop**, a catch-up notice appears, and the task runs on screen — never headless. |
| 3 | Manual (locked-screen rule, revised): lock the screen with the app closed, let a timer come due — the launch + fire happen immediately behind the lock, and the work is on screen at unlock. Log out entirely — nothing fires until next login. |
| 3 | Manual (wake-from-suspend): schedule a timer +10 min, suspend the machine — it wakes and the task runs visibly. Record which mechanism worked (WakeSystem user unit vs CLOCK_REALTIME_ALARM vs fire-on-resume fallback); if only the fallback is permitted, the Timers view must say so on affected rows. |
| 3 | `ui/tests/CatchUpDialogTest.cpp`: three missed timers across two projects render grouped; "all for project A" runs exactly A's two and leaves the third listed; per-row Skip marks "missed — skipped at catch-up"; a repeating skipped timer re-arms for its next slot. |
| 3 | Manual (catch-up): schedule two one-shots on different agents, log out past both, log back in — the catch-up dialog appears before anything fires; "one by one" runs only the ticked task; nothing fired without the dialog's say-so. |
| 3 | Manual negative: with no systemd user session, the Timers view marks every row as app-must-be-open. |
| 3 | Source-level assertion alongside the bypassPermissions grep: no scheduler path may start akcore or a thread without the UI process confirmed on screen (the fire path requires the UI's ack — pin it with a test on the handshake). |
| 4 | Live smoke `scripts/smoke-schedule.py`, shaped like `scripts/smoke-cowork-request.py`: an agent calls `schedule`, the timer exists immediately with its reason verbatim and `CreatedBy: "agent"`, and a `scheduledCreated` notification fired — no consent dialog anywhere. |
| 4 | `TestAgentCannotScheduleUnrelatedForeignThread` — refused `NOT APPLIED`; own/launched threads schedule freely. |
| 4 | **The switch rule, as tests:** `TestHardSwitchSuppressesFiringButNotListing` (switched-off timer never fires, still lists as scheduled to the agent) and `TestNoAgentPathCanReadOrWriteSwitches` (source-level: the MCP tool surface exposes no switch state — grep-pinned like the bypassPermissions rule). |
| 5 | `ui/tests/TimersViewTest.cpp` — republishing an identical timer list causes zero model resets; the countdown ticks without one. |
| 5 | Manual end-to-end, the feature's real acceptance test: a multi-hour program. Give an agent a task, let it schedule an hourly continuation itself (no prompt should appear — only the notification and the Timers row), leave the window tray-hidden, and confirm six hours later that it ran six times visibly, that a permission prompt it hit at fire 3 raised a notification and waited, that flipping the per-agent hard switch mid-run silently held the next fire, and that the transcript names the timer at every resume. |

## Non-goals

- **Building on the CLI's own daemon.** `~/.claude/daemon`'s control socket is
  internal and undocumented. We observe background sessions through the
  documented `claude agents --json` (plan 21) and own scheduling ourselves —
  which is also the only way it can be cross-engine, since kimi has no
  equivalent at all.
- **Any scheduler-side permission override.** Stated as a rule, enforced by a
  test. If a scheduled run needs more authority, the human changes the thread's
  permission mode, in the open.
- **cron syntax.** "In N minutes", "at HH:MM", "every N hours" covers the stated
  need. A cron expression field is a request-driven addition, not a launch one.
- **Cloud or cross-machine scheduling.** Timers are local to this akcore.
- **Waking the machine.** No RTC wakeups. If the box is asleep the timer fires
  on resume (once — see Phase 1).
- **Scheduling agent *creation*.** Timers act on existing threads. "Start a new
  agent every morning" is a different feature with a different consent shape.

## Open questions for the user

1. **Standing self-schedule grant.** ~~Is a standing grant acceptable, or should
   every schedule be individually approved?~~
   **RESOLVED 2026-08-01: no per-schedule approval and no grant toggle — agents
   schedule freely; the user holds hard kill-switches.** Agents are not limited
   in how they schedule continuations. In exchange, the scheduler guarantees
   total visibility and control on the human side: every schedule is visible in
   the Timers view the moment it exists, and the user has **hard switches** —
   pause/disable ALL schedules, per-project, or per-agent — enforced in the
   event scheduler itself. The switches are **invisible to agents**: a
   suppressed agent's schedule requests still "succeed" from its point of view;
   the scheduler simply does not fire them while the switch is off. No agent
   can observe, override, or re-enable a switch.
2. **Lingering.** ~~Explain / offer / never mention `loginctl enable-linger`?~~
   **RESOLVED 2026-08-01: never linger — visible-desktop-only execution.**
   Scheduled tasks fire only while the user is logged in with AgentKate on an
   active, displayed desktop. Nothing executes hidden, headless, or logged-out;
   there is no linger path at all. Follow-up decisions (same day, second
   revision): app closed + logged in → the timer **auto-launches the full UI
   visibly** and then fires (visible resurrection); screen locked but logged
   in → **tasks continue as normal**, including visible launches (live at
   unlock) — locked-but-logged-in counts as displayed; sleep/standby → the
   scheduler **arms a system wake alarm** so the machine wakes for due timers
   (WakeSystem / CLOCK_REALTIME_ALARM, honest fire-on-resume fallback);
   logged out → nothing fires; on next login a **catch-up dialog** lists every
   missed task with four run scopes (one by one / all for a project / all for
   an agent / run all) and per-row Skip — nothing from the backlog fires
   without it. Real-time visibility remains the hard requirement — no headless
   firing. See the reworked Phase 3.
3. **Rate-window auto-resume default.** Should a stalled thread resume itself
   the moment the window resets (this plan's default — it is the reported pain),
   or only when the user has explicitly opted that thread into unattended
   operation? Auto-resume means tokens are spent without anyone present.
4. **Runaway protection.** Should a repeating timer require a `MaxFires` or a
   spend ceiling? `--max-budget-usd` (landing now) is the natural pairing: a
   scheduled agent with a budget cap is much safer than one without.
