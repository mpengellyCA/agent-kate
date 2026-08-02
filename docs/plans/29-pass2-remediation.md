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
