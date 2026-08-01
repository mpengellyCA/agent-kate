# Audit brief — pass 2: user experience, friction, and security

**For:** the same independent auditing agent (Kimi K3), second pass.
**Prerequisite reading:** your own pass-1 report,
`docs/audits/FINDINGS-kimi-k3-2026-08-01.md`.
**Deliverable:** `docs/audits/FINDINGS-kimi-k3-pass2-<YYYY-MM-DD>.md`.
**Authorisation:** unchanged — the owner's own application, on the owner's own
machine, at the owner's request. Defensive review. Findings only: read anything,
write only your report. No code edits, no commits, no pushes.

Pass 1 was a systems audit and it was excellent — it found a real privilege
escalation, a real data-exposure problem, and a real consent bypass, and it
recorded what was sound so nothing had to be re-audited. All of it has now been
acted on (status below).

**This pass has a deliberately different centre of gravity.** The product goal
is "the best AI arena for KDE Plasma users", and the biggest remaining risks are
no longer exotic — they are the places where a real person gets **stuck,
confused, or misled**, and the places where a security control only works if the
human understands what they are agreeing to. So: **user experience and friction
first, security second, and especially the overlap between them.** Deep systems
archaeology is explicitly *not* what is wanted this time.

---

## Status of the project since your pass 1

Four remediation rounds (8, 9, 10, 11) acted on your report, each followed by
adversarial verification that repeatedly caught fixes which were technically
present but did not hold. **Treat the table below as claims to be tested, not
facts** — three of the four rounds existed precisely because the previous
round's fixes were rated MOVED or PARTIAL rather than CLOSED.

| ID | Claimed status | What was done |
|---|---|---|
| F1 | Closed | Single cross-engine permissiveness ranking; escalation, effective workspace isolation, or shedding the parent's tool restrictions stops for a human. Workers inherit `DisallowedTools`/`AddDirs` (they were being silently dropped). Reservation-based worker caps. The approval prompt renders the real facts — the first attempt rendered as the two words "same engine". |
| F2 | Closed | Stores 0700/0600 with migration of existing world-readable files; credential-shaped env values redacted on disk and on the wire. |
| F3 | **Partly open — see below** | Keyboard path resolves the active window and fails closed; pointer mirror treated as evidence (relative accumulation only while provably on a real screen; absolute ops that did not provably land destroy it; batches abandon at the first unapplied op). |
| F4 | Closed | Provenance requires the resolved path inside the repo's own worktree dir, registered with git, no symlinked component; the analyser classifies a bogus record BLOCKED. |
| F5 | Closed | `requireUI` on start/resume/fork/promote/setOption/mode.save/mode.delete/attach; cowork flag routed through one gate. |
| F6 | Closed | `permission.requested`, `agent.event` and the cowork consent/grant notifications are UI-only, not broadcast. |
| F7 | Closed | a11y actions refuse on PID match and fail closed when the owning window cannot be resolved. |
| F8 | Closed | Flip disclosed by all three causing paths, applied only after the grant lands, restored on decline, kill-switch, **last agent switched off** (the consent text promised this and only exit delivered it), and next-run recovery. |
| F9–F12 | Closed | Blocking stdin write moved off the lock; transcript replay capped and event logs/audit rotated and deleted with their thread; size checked before every read; blocking D-Bus off the GUI thread. |
| F13 | **Known gap, documented** | `SO_PEERCRED` cannot help — the adversary runs at the user's own uid. Narrowed by an atomic single-claim UI role and a per-bridge secret minted at spawn. Written up in `docs/security-model.md`. |
| F14–F16 | Closed | Scheme whitelist on every `openUrl`; images resolve only inside allowed roots, non-regular files never opened, painted rows included (the first fix rested on a premise comment that was false); markdown neutralisation rebuilt after three rounds of divergence from md4c. |
| F17–F24 | Closed | Handler contexts cancelled on disconnect; `_usage` no longer dropped and cumulative usage no longer billed as per-turn; private socket directories; plain-text labels; busy cursor restored; notification storms coalesced. |

**Two items are OPEN and are the first thing to verify** — found by the final
round's verifiers, in the Cowork pointer guard:

1. **`cowork.playInput` bypass (rated critical).** `plan.FinalPos` is the last
   position in *event* (fire-time) order, but the cursor ends at the last
   absolute move *op* in global time order, and those differ. A crafted
   timeline may therefore commit a mirror position the cursor is not at —
   defeating the same guard the round was closing.
2. **The mirror is per-thread; the cursor is global (rated high).** One
   Cowork-enabled thread can park the real cursor over an AgentKate window
   while a second thread's mirror still reads clean.

Both are in defence-in-depth for a feature that requires explicit human consent
to enable, which is why the work was committed rather than held — but they are
real, and a remediation plan for them is wanted.

Also landed since your snapshot, and relevant to how the app now behaves:

- **Token-by-token streaming** for claude threads, with a coalescing flush, plus
  live subagent text forwarding.
- **A real system-event dispatch table** — compaction boundaries, model
  fallbacks, API errors and rate-limit windows are now rendered instead of
  silently dropped.
- **An exact context meter** driven by the CLI's own accounting, now on both
  engines.
- **Kimi parity work**: real in-session compaction, mid-session mode/model
  convergence, `/usage` context, session browse-resume.
- **KDE shell integration**: desktop notifications with click-to-raise, single
  instance with argv forwarding, window geometry restore.
- **A `manual` permission mode was removed** after probing showed the CLI
  accepts the flag and silently runs `default` — it promised supervision it did
  not deliver.

## Job 1 — User experience and friction (the priority)

Audit AgentKate the way a **new-but-technical KDE user** would meet it, and then
the way a **power user running five agents** would live in it. You may start the
app (offscreen or on the live desktop) and drive it; you may read the UI code to
explain what you observe. Report what is confusing, slow to understand,
easy to get wrong, or impossible to recover from.

The journeys that matter, roughly in the order a user meets them:

1. **First five minutes.** Install → first launch → first agent producing useful
   work. Where does a competent person hesitate? What is unexplained
   (`ui/src/WelcomeDialog.*`, the Simple/Advanced experience toggle, the New
   Agent dialog's option surface)? Count the decisions demanded before the first
   prompt can be sent, and say which ones could have defaults.
2. **The permission prompt.** This is the single highest-frequency interaction
   in the product and the one where friction and safety collide. Is it obvious
   *what* is being approved, *which agent* is asking, and what "Allow" will
   permit next time? Does approving feel proportionate to the risk, or does
   volume train the user to click through? Is there any path where a user
   approves something other than what they read?
3. **Knowing where you are needed.** With several agents running, how does the
   user learn that one is blocked, one finished, one failed? Assess the roster
   markers, the new desktop notifications (do they arrive at the right moments —
   and never when the user is already looking at that agent?), the Jobs panel,
   and whether the window's own state answers "what needs me right now" at a
   glance.
4. **Reading a transcript.** Long turns, streamed text, tool calls, subagent
   output, images. Is the hierarchy legible? Can the user tell agent prose from
   tool output from an error? Does scrollback stay usable during heavy
   streaming, and does the view fight the user (jumping, losing position,
   collapsing state)?
5. **Worktrees and "where did my code go".** The owner's own reported pain:
   *agents get confused about paths and escape the worktree*. From the UI alone,
   can a user tell which directory an agent is actually working in, whether its
   changes are isolated, and how to get them back into the main checkout? Where
   does the interface's language ("isolated") diverge from what is enforced?
6. **Error and recovery paths.** Kill akcore mid-session; start an agent with a
   broken CLI install; exhaust a rate-limit window; drop a huge attachment;
   cancel mid-turn. For each: is the message accurate, does it say what to *do*,
   and can the user actually recover without restarting the app? Vague or
   blaming error text is a finding.
7. **Discoverability for the non-CS user.** The command palette, empty states,
   keyboard reachability, and whether any panel is a blank box with no
   explanation of what would fill it. Which features are effectively invisible
   unless you already know they exist?
8. **Settling in.** Does the app remember what it should between runs (window
   state, panel layout, last project, per-agent choices) and forget what it
   should not? Anything a user must re-do every session is friction worth
   reporting.

For each finding here, say **what the user experiences**, not just what the code
does — and give the smallest change that would remove the friction. Concrete
beats comprehensive: five sharp, well-evidenced UX findings are worth more than
twenty style notes. Screenshots are not expected; precise description is.

## Job 2 — Security, focused on the human-facing controls

Stay on the surfaces where security depends on a person understanding something.
Do **not** re-audit the transport, the markdown pipeline, KWallet handling, or
subprocess construction — you certified those in pass 1 and they have not
materially changed.

1. **Verify the pass-1 fixes** (the status section above lists them). For each,
   rule **CLOSED / MOVED / PARTIAL / UNFIXED / REGRESSED**. "MOVED" — the
   original path blocked but an equivalent one still open — is the most valuable
   verdict you can produce; hunt for it deliberately.
2. **Consent quality, not just consent presence.** Every dialog that grants
   authority (tool permissions, Cowork enable, desktop control, worker launch
   approval): does it state the *actual* consequence in language a tired human
   reads correctly at 11pm? Is the risky choice ever the easy one (default
   button, Enter key, pre-checked box, muscle-memory position)? Can a user tell
   a one-off approval from a standing grant? A dialog that is technically
   correct but reliably misread is a security finding.
3. **Honest labelling.** Anywhere the UI claims containment, isolation,
   supervision or safety, check that the code delivers exactly that. The removed
   `manual` mode is the pattern: an honest-sounding label over a weaker
   mechanism. Look for others.
4. **Standing authority.** What can an agent do *without* asking, once any grant
   is given? Is the scope of each toggle discoverable, and can the user see what
   is currently granted and revoke it in one place?
5. **One live question worth answering** (carried from your pass-1 "not
   audited"): does claude's `acceptEdits` mode auto-accept `Edit`/`Write`
   **outside** the session working directory? If it does, the UI's isolation
   language is wrong in the default mode, which is both a UX and a security
   finding. This is worth a live probe.

## Job 3 — Anything genuinely dangerous you trip over

If something serious surfaces outside the two jobs above, file it. But do not go
looking for it: the systems sweep is done, and this pass earns its value on the
human-facing half.

## Reporting

Same format as pass 1 (`file:line` evidence, severity + confidence, failure
narrative, fix sketch, effort), with two changes:

- Open with a **verification table** for pass-1 findings:

  | ID | Title (short) | Verdict | Note |
  |---|---|---|---|

- Number new findings **F25 onward** so the two reports compose, and tag each
  with its lens — `ux`, `security`, or `ux+security` for the overlap, which is
  where the most valuable findings in this pass will be.

Keep the discipline that made pass 1 useful: verify before filing, argue the
opposite before believing yourself, mark confidence honestly, and keep the
"found sound" section — a UX journey you tried and found genuinely smooth is
worth recording, so it does not get "improved" later.
