# 29 — Pass-2 remediation: consent quality, honest labelling, and first-run friction

**Source:** [`docs/audits/FINDINGS-kimi-k3-pass2-2026-08-01.md`](../audits/FINDINGS-kimi-k3-pass2-2026-08-01.md)
— an independent audit (Kimi K3) run against the brief in
[`docs/audits/AUDIT-BRIEF-kimi-k3-pass2.md`](../audits/AUDIT-BRIEF-kimi-k3-pass2.md),
whose centre of gravity was deliberately **user experience first, human-facing
security second**.

Pass 2 verified the pass-1 remediation: **22 of 24 findings CLOSED**, the two
carried-open Cowork items **CONFIRMED** (one broader than we had scoped it), and
26 new findings filed as F25–F50. This plan sequences the response.

It is deliberately **not** a feature plan. Where a finding's real fix is a feature
already approved in [plan 20](20-approved-features-program.md), this doc lands the
**honest interim fix now** and records the hand-off, because several of these
findings are actively misleading a user *today* and cannot wait for an XL feature.

---

## The shape of the report

The findings cluster into four groups, and the ordering below follows severity
and blast radius, not report order.

| Group | Findings | Why it leads |
|---|---|---|
| **Consent bypass and caller binding** | F25, F26, F27, F34, F35, F36 | The last guard on a path where an agent widens its own authority. Two were carried open from pass 1. |
| **Honest labelling** | F28, F30, F31, F32, F50(a11y read, scope preselect) | A control that promises containment or a one-off decision, over a mechanism that delivers neither. Same failure shape as the `manual` permission mode we removed. |
| **Destructive and misleading actions** | F29, F41, F38 | Data loss under an agent-scoped label, and a false "has not changed anything yet". |
| **First-run and daily friction** | F37, F39, F40, F42–F49, F50 (grouped) | Where a competent person gets stuck, and where a shipped feature turns out never to have worked. |

---

## Round 1 — consent bypass and caller binding *(security)*

Four clusters on disjoint file sets, a build gate, then adversarial verification
with CLOSED / MOVED / PARTIAL / UNFIXED / REGRESSED verdicts. MOVED is the
verdict that mattered in every previous round and is hunted for deliberately.

1. **Cowork pointer guard** — F25, F26, plus the two F50 Cowork items
   (unrestricted a11y *read* of AgentKate's own UI; span-0 `injectInput` queued
   behind the portal handshake).
   - F25's fix is a **compiler invariant**: track `motionUntil` and refuse any
     pointer op scheduled before in-flight motion ends. Overlapping absolute
     motion is never semantically meaningful, so the invariant costs nothing
     legitimate — and it restores the property both the GuardPts and
     `FinalPos == lastPos` silently assumed.
   - F26's fix is to **key the mirror globally** and serialize guard→fire. The
     availability cost (threads invalidate each other's evidence) is accepted;
     the resource being guarded is global, so per-thread evidence was never
     sound.
2. **Handler caller-binding** — F34, F35, F36. `requireUI` on the pull endpoints
   and the privileged cluster; `RequireBridge` on `permission.request`;
   `wait_agent` bound like its siblings. **Constraint:** a gate on a handler with
   a legitimate non-UI caller breaks the running product, so every RPC name is
   grepped across `ui/src`, the bridge and the MCP paths before it is gated.
3. **Kimi consent scope** — F27. Never fall back across scope: if `allow_once` is
   absent, answer `cancelled` and say so. A one-off "Approve" must never become a
   standing grant the human was never offered.
4. **Consent dialogs and dead toggles** — F31, F32, and the ConsentDialog /
   EnsembleDialog / kill-switch items from F50. Unimplemented capabilities leave
   `AllToggleable()` until their tools exist, and any already-persisted policy
   entry for them is ignored rather than honoured the day v3 ships.

## Round 2 — destructive actions, honest labelling, dead rendering *(ux+security)*

`ui/src/AgentPanel.cpp` is the bottleneck file — most of these findings live in
it — so it is owned by a single agent per round and other agents are forbidden
from touching it.

- **F29 (high, data loss)** — "Discard changes" on a workspace-mode agent runs
  `git reset --hard` + `git clean -fd` on the user's real checkout, destroying
  their own uncommitted work, behind a dialog that frames the blast radius as
  agent-scoped ("in worktree #3"). The dashboard already guards "Remove
  worktree…" with `isolated`; this is the same guard, missing.
- **F30 (high)** — the UI calls a git worktree a "sandbox" while
  `docs/security-model.md` says in bold that it is **not** one. Drop the word.
  `NewAgentDialog`'s "(git worktree)" parenthetical is already the honest form;
  everything else aligns to it.
- **F41 (high)** — `git diff HEAD` is tracked-only, so a workspace agent that
  creates new files produces an empty diff and the UI states, falsely, that the
  agent "has not changed anything yet". This is the owner's own reported pain
  ("where did my code go") reproduced by our own diff command.
- **F39 (high)** — live subagent text has **never been painted**: the delegate
  only builds the result document inside `if (done)`. The feature was shipped in
  `c945893` with the bug present. Its own comment describes preventing exactly
  the "⋯ for the whole run" behaviour it produces.
- **F40 (high)** — tool errors render identically to successes; `is_error` is
  never read on the per-tool path.
- **F28 (high)** — the highest-frequency prompt in the product truncates Bash at
  240 chars with no way to read the rest, and the truncation point is
  attacker-controllable. Elide the *middle* (payloads hide at the tail) and add a
  Details… view over the full input, which is already client-side.
- **F37, F38** — the first-run dead ends: a missing CLI is discovered only after
  the user has written and sent their first task, and Simple mode plus
  sandbox-by-default hides every path to get the changes back.

## Round 3 — daily friction *(ux)*

F42–F49 and the remaining F50 group: KWallet keys that cannot resume an agent,
rate-limited agents invisible outside their own panel, a blank chat feed, the
prominent "+ New Agent" button bypassing the friendly dialog, dead provider
routes on first run, the open-project set not restored, find that cannot see the
error text you are staring at, and the recommended default failing on a repo with
no commits.

---

## Where this meets the approved feature program

Four findings are the UX half of a feature already approved in plan 20. Each gets
an honest interim fix in the rounds above **and** a hand-off, so the feature work
inherits a correct starting point rather than re-litigating it.

| Finding | Interim fix (this plan) | Feature that subsumes it |
|---|---|---|
| **F43** — rate-limited agents invisible outside their own panel | Amber roster token on non-allowed transitions + one coalesced notification | **[Plan 28](28-scheduling-and-autonomy.md) Phase 2** — rate-window auto-resume. F43 is literally its UX half: the same `rate_limit_event` `resetsAt` that drives the marker drives the resume. Plan 20 already nominates Phase 2 as the program's early win; **pull it forward and land F43 with it.** |
| **F37, F46, F42** — missing CLI found too late; dead provider routes offered; a KWallet key cannot resume | `findExecutable` check at handshake + unavailable engines marked; providers without a resolvable key suffixed; provider id persisted for resume | **[Plan 26](26-engine-services.md)** — the preflight health card (`claude doctor` / `kimi doctor`) is the real answer. The interim fix must be written so the card *replaces* it, not so the card has to work around it. |
| **F30, F29, F41, F38** — "sandbox" that isn't; discard that isn't scoped; diff that lies | Honest labels, an isolation guard on discard, untracked files in the workspace diff | **[Plan 23](23-contained-worktrees-and-checkpoints.md)** — the containment rework is what would *make* the word "sandbox" true. Plan 23 §2–4 must re-examine these labels when it lands: if containment is real, the honest word changes again. F41's diff is also a checkpoint input (§5–7). |
| **F50** — panel-rail shortcuts (Alt+1…9) are bound as raw `QShortcut`s, invisible in every menu and unreachable by the command palette | Append the binding to the rail tooltips | **[Plan 27](27-kde-presence.md) §1** — the KActionCollection refactor is the actual fix and is already step 0 of the program. Every raw `QShortcut` found here is an item on its list. |

**F33** — "no single answer to *right now, what can my agents do without me*" —
has no feature owner and deserves one. The cooperation authority (every agent may
launch, send, wait, close and spend money on new workers within its subtree, with
zero prompts) is not shown anywhere as authority. A Permissions overview listing,
per agent, its permission mode, cowork-enabled flag, active grants and live
cross-subtree approvals — with revoke where the core supports it — is the natural
consumer of the tray's aggregate state (plan 27 §2) and belongs beside it.

---

## Conventions this remediation holds

- **Fail closed.** Agents run at the user's own uid; the property being defended
  is that a prompt-injected agent cannot widen its own authority without an
  informed human decision. Where evidence is missing, refuse.
- **Every behavioural fix carries a test that fails if the logic is inverted.**
  Round 5 of the previous programme certified two dead parsers because the
  fixtures were invented rather than captured; fixtures pin real wire shapes.
- **Adversarial verification is not optional.** Three of the four pass-1
  remediation rounds existed because the previous round's fix was rated MOVED or
  PARTIAL. A fix is not done because its author says so.
- **Honest labelling is a security control.** The removed `manual` permission
  mode is the reference case: the CLI accepted the flag and silently ran
  `default`, so the label promised supervision the mechanism never delivered. Any
  label claiming containment, isolation, supervision or a one-off decision is
  checked against what the code actually does.

---

## Outcome

Four rounds ran between 2026-08-01 and 2026-08-02: three remediation rounds and
one convergence pass, 35 agents, each round ending in a build gate and
adversarial verification with CLOSED / MOVED / PARTIAL / UNFIXED / REGRESSED
verdicts. Committed in seven slices on `kimi-code-backend`. Final gate: build
clean, `go vet` and `gofmt` silent, `go test -race` clean, **25/25 ctest** (up
from 13 at the start of the programme — nine new test targets).

**What the rounds cost, and why they were worth it.** Every round after the
first existed because the previous round's verifier rated a fix **MOVED** — the
named path closed, an equivalent one left open. That verdict fired eleven times
across the programme. The pattern was consistent enough to be a rule about this
codebase: *a fix lands at the site the finding named and not at its siblings.*
The gate caught two defects that compiled clean and passed every test — two
agents independently claiming the same JSON-RPC error number, and a `connected()`
signal removed with nothing wired to its replacement, which would have shipped an
app that came up inert and silent.

**Vacuous tests were the other systemic finding.** Verifiers mutation-tested by
deleting a fix and re-running; six tests passed anyway, including one guarding
the default button on the highest-authority dialog in the product. The recurring
idiom was a test that counts string literals in the source file it is testing —
which passes over any rewrite that keeps the text and inverts the logic. Round 1b
onward made mutation testing mandatory and required the mutation output in the
report.

**Three files had no test binary at all.** `MainWindow.cpp`, `AgentDock.cpp` and
`AgentRoster.cpp` are compiled only by the application target, so a verifier
deleted every enablement guard in them with the suite still green. That is why
guards there kept evaporating between rounds. The convergence pass extracted the
decision into `AgentActions::compute` with a test target that can see it; the
rest of those files remain unpinned.

### Backlog — ranked, for whoever picks this up next

Nothing below is a regression; these are the residues the verifiers judged worth
recording rather than blocking on.

1. **Give the three untested UI files a test binary.** `WorktreeDashboard.cpp`,
   `WorktreeDiffDialog.cpp`, `MainWindow.cpp` and `AgentDock.cpp` are compiled by
   one target only. Every guard and every string wired into them this programme
   is unpinned, and the `DraftDisposition` behaviour — Close keeps a draft,
   `agent.discarded` forgets it — is guarded by nothing but code inspection.
2. **Make the handler inventory's caller-bound basis prove itself.** `checkBasis`
   now runs the pin for some bases; `basisCallerBound` still needs a cross-thread
   probe that drives the method from a bridge bound elsewhere and requires a
   refusal. Until then the inventory — this codebase's main defence against
   ungated handlers accruing again — is partly declarative.
3. **Extend the Cowork handler-drive harness** to `playInput`,
   `pointerClickElement`, `activateElement`, `setElementText`, `listElements`,
   `readText` and `screenshot`. The fixture and fake UI exist; the a11y paths
   need a stub `ElementInfo`/`ElementBounds`, which is the only new work.
4. **Replace the two remaining source-scanning Go tests**
   (`cowork_focuswatch_test.go:224`, `cowork_cursorsection_test.go:399`). They
   count literals and pass over a fail-open rewrite.
5. **Per-card rate-limit state.** `AgentCardDelegate::AgentStatus` has no
   rate-limited value, so a parked agent still shows the green "Working" arc on
   its roster card. The expiry-aware `RateLimitState` it needs already exists.
6. **Detect the residue instead of only disclosing it.** The diff surfaces now
   say honestly what they cannot show (ignored paths, absolute-path writes
   outside the tree). Surfacing those writes in transcript tool cards is the only
   thing that would turn "we cannot show you this" into "here it is" — and it is
   the direct answer to the owner's original "agents escape the worktree"
   complaint. **This belongs to [plan 23](23-contained-worktrees-and-checkpoints.md);
   containment is what makes it tractable.**
7. **An `enable_cowork` digest branch** in `AgentChatHelpers.cpp`. The core now
   supplies a description the generic key scan prints verbatim, so the bar is
   honest today, but a real digest beats a core-authored sentence.
8. **A bare `send_agent`/`wait_agent` still looks one-off to the human.** The
   composite prompt now states its standing grant; the single-action prompt has
   nowhere to say it. Pre-existing, and the same finding class as F27.
9. Smaller: fold `AgentPanel::draftKey()` onto `DraftStore::threadKey` (two
   byte-identical derivations, one pinned); ensemble *worker* engine availability;
   `WorktreeDiffDialog`'s disclosure label still uses `QPalette::Disabled`, which
   reads as *inapplicable*.

### For plan 27 §1 (the KActionCollection refactor)

The raw-`QShortcut` inventory the fixers produced is accurate and worth keeping —
it turns that refactor's first step from a search into a checklist. When it
lands, `MainWindow::refreshPanelTooltips()` and `PanelInfo::help` can be deleted
wholesale; the method comment already says so.
