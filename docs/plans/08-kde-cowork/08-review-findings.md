# 08 — Review Findings & Remediation (adversarial review gate)

Four adversarial reviewers (security, KDE/D-Bus API correctness, architecture/feasibility, completeness)
attacked the plan. They found **legitimate, serious problems** that converge on **two root causes** plus
**two API dead-ends**. This file records every finding, its remediation, and the **revised v1 scope** the
findings force. **This file post-dates and amends `07-wiring-and-roadmap.md` §4.1 (v1 scope) and §4
(sizing).**

> Reviewer verdicts (verbatim gist): **Security** — "NOT sound enough to build v1 as written; it
> threat-models the wrong adversary." **Architecture** — "v1 not buildable/standalone as frozen; add a
> per-connection identity registry — the missing keystone." **KDE-API** — "v1 API-feasible; fix two real
> dead-ends before coding." **Completeness** — "enable/onboarding path, capability-unavailable handling +
> test boundary, and concurrent-instance + HiDPI are the top-3 to add."

---

## A. Root cause 1 — we threat-modeled the wrong adversary (the agent has a shell)

Agent Kate's agent is a **coding agent with `Bash`/`Edit`/`Write` running at the SAME uid as `akcore`**.
Every consent defense in 01/07 implicitly assumed an *externally confined* agent that can only act through
MCP tools. That assumption is false. Concretely:

| # | Finding (sev) | Why the v1 design fails | Remediation (lands in v1 unless noted) |
|---|---|---|---|
| A1 | **CRITICAL** — consent + audit files are agent-writable | `cowork-consents.json` / `cowork-audit.jsonl` live in `$XDG_DATA_HOME/agentkate/` (same dir as `threads.json`), same uid → the agent's `Bash`/`Edit` can forge `GrantedBy:"user"` grants, rewrite the whole audit file, and recompute the hash chain from a new genesis. "Core-owned, never agent-writable" (01 §7, INV-4f) is **false**. | **Decision required (§F).** Minimum v1: (a) `Store.Add` **re-derives `GrantedBy` server-side** — never trust the on-disk flag; (b) **load-time + periodic chain verification**, fail-closed + UI tamper alert on mismatch; (c) document the boundary honestly. Full fix = privilege separation (§F Option B). |
| A2 | **CRITICAL** — hash-chain audit is not tamper-evident vs a full-file rewriter | A same-uid writer replaces every line and recomputes the chain; nothing anchors the head hash outside the agent's reach. Q3's "chain catches truncation/mutation" assumes an attacker who can't rewrite — wrong here. | Anchor the chain head where the agent can't reach it (privilege-separated appender, or an external/remote mirror), OR accept "detect-not-prevent" for v1 and **say so**. Tie to §F. |
| A3 | **HIGH** — raw socket bypasses the MCP bridge entirely | Any same-uid process can `socat`/`nc` `$XDG_RUNTIME_DIR/agentkate-<pid>.sock` and send arbitrary JSON-RPC: `cowork.respondGrant{allow:true}`, `cowork.killSwitch{on:false}`, `cowork.revokeGrant`, flip `CoworkEnabled`. The Q4 "bridge verb set excludes respondGrant" defense is irrelevant to the raw socket. | **Connection-origin assertion is a v1 requirement (was Q4→v2).** UI-only RPCs (`respondGrant`, `killSwitch{on:false}`, `revokeGrant`, `setCoworkEnabled`) reject any non-UI connection. Requires the §C keystone. |
| A4 | **HIGH** — `CoworkEnabled` is agent-flippable | It's read from `session.Record`/`threads.json` (agent-writable) at spawn; the agent can self-enable Cowork, then prompt-injection re-spawns. | Gate `CoworkEnabled` behind a **UI-only RPC** (`cowork.setEnabled`, origin-checked) and **don't re-read it from an agent-writable file at spawn** — hold it in core memory / origin-trusted store. Requires the §C keystone. |

**Honest framing for the user:** a coding agent with shell at your uid can *already* read your SSH keys and
`socat` any local socket — Cowork does not create that exposure, it inherits it. The realistic adversary
Cowork must defend against is **prompt-injection-driven misbehavior** (a poisoned repo/page steering the
agent), not a determined local attacker who already owns your session. The v1 remediations (server-derived
grant provenance, origin-checked UI-only RPCs, tamper detection + kill-switch + audit) are **proportionate to
that adversary**; true tamper-*prevention* needs privilege separation (§F).

---

## B. Root cause 2 — the IPC server has no per-connection identity (the missing keystone)

Architecture review's central finding: `ipc.Handler = func(ctx, json.RawMessage)` passes **no connection
identity** (`server.go:32,179`), `Notify` broadcasts to all conns with no per-client targeting
(`server.go:197-216`), and `handshake` records nothing per-conn (`main.go:438-446`). Four separate
"defenses" in the plan silently depend on an identity that does not exist:

| # | Finding (sev) | Depends on identity for… |
|---|---|---|
| B1 | **BLOCKER** — opt-in is cosmetic at the RPC layer | `--cowork` only filters the agent's own `toolDefs()`; the global `cowork.*` handlers accept calls from *any* thread's bridge over the shared socket. A non-cowork thread can call `cowork.screenshot` directly. → must verify caller is cowork-enabled **in the handler**. |
| B2 | **BLOCKER** — `threadId` is self-asserted (cross-thread grant theft) | The bridge passes `threadId` as an unauthenticated flag; thread A can claim `threadId:B` to spend B's grant. → need a server-side **conn→thread binding** recorded at handshake/first call. |
| B3 | **HIGH** — "primary-UI-only runs the portal" is unimplementable as written | No `primary` flag is stored per-conn; `Notify` can't target one client. → need per-conn role. |
| B4 | **HIGH** — `respondGrant`/UI-only origin assertion (A3/A4) | Needs to know a connection is "the UI." |

**Remediation (ONE keystone closes B1–B4 + A3 + A4):** add a minimal **per-connection identity/role
registry** to the IPC server — at `handshake`, tag the connection with a role (`ui` | `bridge`) and, for
bridges, bind the `threadId`; thread it through to handlers (extend `Handler` to receive a conn handle, or
stash a per-conn context). This is **new, previously-unbudgeted v1 work** and is the single most important
change the review produced. It is a small, well-contained server change (`core/internal/ipc/server.go` +
`handshake` in `main.go`) but it is a **v1 prerequisite** — without it the consent model is bypassable.

---

## C. The keystone: per-connection identity/role registry (NEW v1 work)

Add to `core/internal/ipc/server.go`:
- A per-`conn` struct field `role string` + `threadId string` + `primary bool`, set during `handshake`.
- `Handler` gains access to the calling conn (e.g. `Handler = func(ctx, *Conn, json.RawMessage)` or a
  `ctx` value carrying `*connInfo`). All existing handlers get a trivial signature bump.
- `srv.NotifyConn(conn, …)` / `srv.NotifyRole("ui", …)` for targeted push (portal → primary UI only).
- Socket already lives in `$XDG_RUNTIME_DIR` (user-only 0700 dir) — confirm **0600 socket perms** at
  `Listen` (defense in depth; doesn't stop same-uid, but stops other users).

Handlers then enforce:
- `cowork.respondGrant`, `cowork.revokeGrant`, `cowork.killSwitch{on:false}`, `cowork.setEnabled` →
  **`role=="ui"` else reject** (A3, A4, B4).
- every `cowork.*` capability RPC → caller `role=="bridge"` AND its bound `threadId` is cowork-enabled AND
  the call's `threadId` **equals the bound one** (B1, B2).
- portal `cowork.portalRequest` → `NotifyRole("ui", …)`, and only the `primary` UI acts (B3).

This also retroactively hardens the **existing** `permission.respond`/`permission.request` path (same
self-assertion weakness, `main.go:1219-1259`) — a welcome side benefit, but scope it as cowork-only for v1
to avoid destabilizing the shipping permission flow.

---

## D. API dead-ends to fix before coding (KDE-API review)

Fixed/annotated in `REFERENCE-skill.md` (see its new `## CORRECTIONS` banner):

| # | Sev | Correction |
|---|---|---|
| D1 | **WRONG** | `AtspiRole` table was legacy pyatspi/ATK ordinals. Real D-Bus wire values (from `/usr/include/at-spi-2.0/atspi/atspi-constants.h`): `CHECK_BOX=7, DIALOG=16, FRAME=23, ICON=26, LABEL=29, LIST=31, MENU=33, MENU_BAR=34, MENU_ITEM=35, PAGE_TAB=37, **PASSWORD_TEXT=40**, BUTTON(PUSH_BUTTON)=43, RADIO_BUTTON=44, SCROLL_BAR=48, SLIDER=51, SPIN_BUTTON=52, STATUS_BAR=54, TABLE=55, TABLE_CELL=56, TERMINAL=60, TEXT=61, TOGGLE_BUTTON=62, TOOL_BAR=63, TREE=65, VIEWPORT=68, WINDOW=69, ENTRY=79, TABLE_ROW=90`. **`PASSWORD_TEXT=40` is redaction-critical** and was entirely absent. Affects v2, but the source doc had to be corrected now. |
| D2 | **WRONG** | Portal Go snippet extracted the `Response`-signal results (`sig.Body[1]`) then **discarded them** (`_ = results`) and returned the request path. The artifacts you need (`uri` / `streams` / `restore_token`) live in that vardict — return them. Every portal call in 02/04 inherits this if copied verbatim. |
| D3 | RISKY | `OpenPipeWireRemote` does **not** use Request/Response — it returns the PipeWire FD directly via SCM_RIGHTS in the method reply (read with `UnixFD`, UI-side). The sequence is not uniform with the others. |
| D4 | WRONG | `RemoteDesktop` `Notify*` signatures need the `options a{sv}` arg and correct types: `NotifyPointerMotionAbsolute(session:o, options:a{sv}, stream:u, x:d, y:d)`, `NotifyKeyboardKeysym(session:o, options:a{sv}, keysym:i, state:u)`. |
| D5 | **NOTE — affects design** | Portal injection coords for `NotifyPointerMotionAbsolute` are **relative to the associated ScreenCast STREAM**, not global and not window-local; RemoteDesktop **requires** a paired ScreenCast stream for absolute motion. The 04/03 window-local→absolute reconciliation must map to **stream space** (and pick an output on multi-monitor). Re-scope the coord work (SPIKE-COORD) accordingly. |
| D6 | RISKY | KWin `window.desktops` is `VirtualDesktop[]` **objects** (`.id` QUuid), not ints — the enumeration JS must emit `d.id` per element. `workspace.createDesktop(pos,name)` returns void on some 6.x (SPIKE-VDARITY) — don't depend on its return. |
| D7 | UNVERIFIED | ScreenCast `types` bit-4 "Virtual" (whole-VD single source) is a **KDE-specific, unproven** claim — keep the per-window screencast fallback primary for the sandbox (SPIKE-VIRTSRC). |
| D8 | NOTE | AT-SPI `GetExtents(coord_type=0)`-returns-window-local is a **KDE quirk**, not universal (GTK/Chromium differ); reconcile defensively. `QT_ACCESSIBILITY=1` enables **Qt apps only** — Chromium needs `--force-renderer-accessibility`, Firefox `a11y.force_disabled=0`; they are **often blank on Wayland** (SPIKE-A11YAPPS bounds what a11y can ever see). |

**v1-relevant:** only D2 (the portal results pattern) touches v1 directly — fix the reference and ensure the
v1 `desktop_screenshot` handler returns the signal's results. D1/D3–D8 gate v2/v3 but are now corrected in
the source doc.

---

## E. Completeness gaps to fold in

| # | Gap (sev) | Where it lands |
|---|---|---|
| E1 | **HIGH** — no user-facing *enable path*: `CoworkEnabled` is plumbed but unreachable (no widget, no owner) | v1: an owned `cowork.setEnabled` UI control in 06 + the origin-checked RPC (C). Pairs with A4. |
| E2 | **HIGH** — no onboarding / first-run risk education | v1: a first-run "what is Cowork / what it risks" panel notice (06) + a short user-doc deliverable. |
| E3 | **HIGH** — no capability-unavailable degraded path (missing `xdg-desktop-portal-kde` / PipeWire / AT-SPI / non-KDE DE / no compositor → hang/timeout) | v1: a **capability probe** at startup + a user-visible "unavailable" status; every tool returns a clean "capability unavailable" error, never a hang. |
| E4 | **HIGH** — no CI/headless **test boundary**: only the consent spine is headless-testable; the KDE/D-Bus/portal layers are implicitly manual-on-real-desktop | v1: add a "test boundary" section — unit-test the spine + pure helpers (coord math, grant store, chain) with `-race`; mark D-Bus/portal layers as real-desktop integration with a documented manual checklist; the v1-blocking spikes (GODBUS, CALLBACK) get a one-time manual gate. |
| E5 | **HIGH** — concurrent core instances (two AK windows) race on the shared `cowork-consents.json` + named sandbox VD | v1: single-writer lock (flock) or per-instance path decision in 01; document the supported topology. |
| E6 | **HIGH** — HiDPI / fractional scaling / multi-monitor coordinate math absent | v1 (screenshot sizing vs scaling) + v2/v3 (inject/stream coords). Add a scaling section to 02/03/04 + SPIKE-SCALE. |
| E7 | MED | DE-support matrix (KDE-only window-list/sandbox; cross-DE portal/a11y), X11-vs-Wayland per-slice correctness, dogfooding "uninstalled capability matrix", i18n of the consent UI + the literal "Agent Kate Sandbox" VD name, observability/debug surface, uninstall/data-hygiene cleanup, grant fate on thread-discard, consolidated **v1 definition-of-done** checklist. Fold into the relevant slices during implementation. |

---

## F. DECISION FOR THE USER — the same-uid trust boundary (A1/A2)

This is the one finding that changes appetite/scope and is genuinely the user's call.

- **Option A — Accept + mitigate + document (recommended for v1).** Keep `akcore` at the user's uid. Ship
  the in-band mitigations (server-derived grant provenance, origin-checked UI-only RPCs via the §C keystone,
  load-time audit-chain verification + fail-closed + tamper alert, global kill-switch). **Honestly document**
  that a fully-compromised same-uid agent can tamper with consent/audit state — detection, not prevention.
  Rationale: the realistic adversary is prompt injection, not a local attacker who already has your shell;
  proportionate; ships v1 on the timeline already scoped + the keystone. Plan privilege separation as a named
  v2/v3 hardening.
- **Option B — Privilege separation now.** Run the consent/audit authority (or all of `akcore`) as a
  **separate uid / systemd-hardened service** the agent's tools cannot write to (consent files 0600 owned by
  that uid; socket group-gated). True tamper-*prevention*. Significant architectural commitment: changes the
  process/install model, socket ownership, and how the UI spawns the core. Larger v1, slower.

**Director recommendation: Option A for v1, with the §C keystone mandatory and Option B scheduled as v2
hardening.** Surfaced to the user at sign-off.

> **✅ RATIFIED 2026-06-21 (user sign-off): Option A.** Keep `akcore` at the user's uid; ship the in-band
> mitigations (§C keystone + server-derived provenance + origin-checked UI-only RPCs + audit
> tamper-detection + kill-switch) and document the boundary honestly. Privilege separation (Option B) is
> scheduled as v2 hardening. **Implementation of the v1 walking skeleton (§G) is approved to begin.**

---

## G. Revised v1 scope (amends 07 §4.1)

v1 = the original walking skeleton **plus** the keystone and the A/E v1 remediations:

1. Build system + `godbus` dep + opt-in `cowork` MCP server (off by default).
2. **[NEW] Per-connection identity/role registry (§C)** — handshake roles (`ui`/`bridge`), conn→thread
   binding, `NotifyRole`/`NotifyConn`, 0600 socket. **Keystone — do first, it unblocks the gates below.**
3. Consent spine: grant store (atomic, restart semantics, migration, **flock for concurrent instances**,
   **server-derived `GrantedBy`**), `Authorize`, grant broker, the v1 RPCs + notifications, audit JSONL
   hash-chain **+ load-time verify + fail-closed + tamper alert**, kill-switch + teardown registry +
   self-target/anti-escalation guards (allowlist-shaped for R2), full `-race` tests.
4. **[NOW v1, was Q4/v2] Origin-checked UI-only RPCs** (`respondGrant`, `revokeGrant`, `killSwitch{on:false}`,
   `setEnabled`) + RPC-layer opt-in enforcement + cross-thread grant-theft prevention (uses the keystone).
5. **[NEW] Capability probe + graceful "unavailable"** for missing portal/PipeWire/AT-SPI/non-KDE/no-compositor.
6. `desktop_list_windows` (KWin one-shot script, no portal, R0).
7. `desktop_screenshot` (R1, portal round-trip, **returns the Response-signal results — D2**, ≤1568px,
   8 MiB guard, MCP image block, AK/sensitive-window exclusion as best-effort with explicit-target preference).
8. Cowork panel: active-grants + audit view + kill-switch + **`CoworkEnabled` enable control (E1)** +
   **first-run risk education (E2)** + capability-status line; `parent_window` X11 branch + Wayland `""`
   fallback.
9. `ControlConsentDialog` R2 shell (wired to no R2 tool yet).
10. **[NEW] v1 definition-of-done checklist** + the test-boundary doc (E4).

**Revised v1 sizing:** the spine + keystone + v1 remediations push v1 from "L" to **L→XL (≈5–8 days)**. The
keystone (~S–M), capability probe (~S), enable/onboarding UI (~S–M), and concurrent-instance + tamper-detect
(~S–M) are the additions over the original frozen scope. **v1-blocking spikes unchanged: SPIKE-GODBUS,
SPIKE-CALLBACK** (both one-time manual gates).

**v1-blocking remediations (must-fix before v1 ships):** A1(min)/A3/A4 via the §C keystone, B1, B2, D2, E1,
E3, E4, E5. Everything else is correctly v2/v3 or doc-time.

---

## H. What the review did NOT break (confirmed sound)

- The **portal round-trip itself** is deadlock-free: handler on its own goroutine, no lock held across the
  wait, `corrId` separates concurrent requests, fail-closed on no-UI (architecture review).
- **No-FD constraint** honored end-to-end: PipeWire/ScreenShot2 FDs stay UI-side; only base64/tokens/node-ids
  cross the bus (INV-1 holds).
- **Per-thread grant isolation** logic is sound (Match keys on ThreadID); **default-deny/fail-closed**
  discipline is consistent across timeout/transport/ctx/kill paths.
- v1 has **no hidden dependency** on v2/v3 pieces (`desktop_screenshot` uses ScreenShot2/portal directly, not
  `captureStill`; the `parent_window=""` fallback works on xdg-desktop-portal-kde).
- **v1 is API-feasible** (KDE review) — none of v1's two tools touch the wrong-role-table, Virtual-source,
  VD-arity, EIS, or xdg-export unknowns.
