# 05 — KDE Plasma Cowork: Virtual-Desktop Sandbox

A bounded workspace where Agent Kate can open a browser, LibreOffice, or any app and the user can
**watch it work in one place**, with capture/control confined to that space. The sandbox is a
dedicated **KWin Virtual Desktop** ("Agent Kate Sandbox"); a `vd_sandbox` grant (Agent 1) narrows
every capture/control `Authorize()` so screenshot/screencast/input only apply to windows whose
`desktops` include the sandbox VD. **Critical honesty up front: this is an _organizational_ boundary,
not a _security_ boundary** (§4). True isolation (nested compositor / separate session / container)
is explicit future work.

This slice depends on **Agent 3** (KWin window model + scripting + window events), **Agent 1**
(consent `Grant` store + `Authorize()` + `target` model), **Agent 2** (screencast of a VD as one
stream), and **Agent 4** (input injection confined to sandbox windows).

---

## Current state

- **No D-Bus, no KWin scripting today.** `go.mod` (module `agentkate`, go 1.26) has **no D-Bus lib**;
  add `github.com/godbus/dbus/v5 v5.1.0` per Context Pack §1/§5. KWin VD create/select/query and the
  window-added hook all run through `org.kde.kwin.Scripting.loadScript` → `Script.run()` →
  `callDBus(...)` back to a Go-registered service (REFERENCE-skill Layer 2). **One-shot scripts must
  `org.kde.kwin.Script.stop()` after reading; persistent event scripts persist until KWin restarts**
  (REFERENCE-skill Gotchas) — sandbox needs **both** a one-shot mutator and one long-lived event
  script.
- **Window model is Agent 3's.** Per REFERENCE-skill Layer 2, KWin 6.x `Window` exposes
  **`desktops` (VirtualDesktop[])**, `activities` (string[]), `onAllDesktops`, plus `internalId`
  (QUuid), `resourceClass`, `pid`, geometry. Agent 3 owns the canonical window list + `windowAdded`/
  `windowRemoved`/`currentDesktopChanged` events. **This slice does not re-enumerate windows; it
  reads Agent 3's model and adds VD-membership predicates.**
- **Consent authority is Agent 1's, in Go.** Grant model (Context Pack INV-2):
  `Grant{ id, threadId, capability, target, scope, grantedAt, expiresAt?, revokedAt? }`,
  `capability ∈ {…, vd_sandbox}` already reserved, `target` narrows to "a … virtual-desktop", grants
  **per-thread**. The enforcement primitive is Agent 1's **`Authorize(threadId, capability, target)`**
  called imperatively inside each Go tool (INV-3 R2: "gate lives imperatively inside the Go tool").
- **MCP tools = two edits** in `core/cmd/akcore/mcp.go`: append to `toolDefs()` (`mcp.go:446`) and a
  `case` in `runTool`'s `switch name` (`mcp.go:132-133`), returning a string via `toolResult`
  (`mcp.go:573`). Tools auto-meter (Context Pack §1).
- **RPCs register up-front** in `registerHandlers` (`main.go:437`) via `d.srv.Handle("cowork.method", fn)`;
  `handlerDeps` (`main.go:420`) reaches subsystems; teardown in `gracefulShutdown` (`main.go:302-323`).
  **Handler map is not goroutine-safe after `Serve`** — all `cowork.*` handlers register statically.
- **Separate `cowork` MCP server, off by default** (INV-5): new entry in `writeMCPConfig`
  (`main.go:2778`) + `--allowedTools mcp__cowork` token (analog of `agent.go:288`).
- **Events coalesce; UI re-derives from snapshot RPC** (INV-6, precedent `agent.go:158-236`, 25 ms).
  Sandbox membership changes ride `cowork.sandboxChanged`; the UI pulls truth from `cowork.sandboxWindows`.
- Every goroutine is `safe.Go("pkg.site", fn)` (`safe/safe.go:22`); don't hold a mutex across a
  blocking D-Bus call (Context Pack §1).

---

## Proposed design

### 1. Mechanism — Virtual Desktops vs Activities

| Axis | **KWin Virtual Desktop** (recommend v1) | KDE Activity |
|---|---|---|
| Per-window membership | `window.desktops` (VirtualDesktop[]) — directly settable from a KWin script | `window.activities` (string[]) — also scriptable, coarser |
| Create / select | `workspace.createDesktop(pos,name)`, set `workspace.currentDesktop`; `VirtualDesktopManager` D-Bus | Activities D-Bus (`org.kde.ActivityManager`); heavier, user-facing concept |
| Switching cost | Instant compositor switch; one stream covers the whole VD (Agent 2 `types`=Virtual, Plasma 6.5+) | Switching reshuffles whole session state, plasmoids, "stopped/running" lifecycle |
| Persistence | Ephemeral by nature; we create/remove on demand — ideal for a sandbox | Persisted across reboots in KActivities DB; littering the user's Activity list is intrusive |
| User mental model | "a desktop the agent works on" — familiar, disposable | "a context/project" — conflates with the user's own workflow organization |
| Capture targeting | ScreenCast `types` bit 4 = **Virtual** (Plasma 6.5+) screencasts a whole VD as one node | No portal source type for an Activity; would fall back to per-window |

**Recommend KWin Virtual Desktop for v1.** It gives a first-class per-window membership property
(`desktops`), a disposable create/remove lifecycle that matches "spin up a sandbox, tear it down,"
and — decisively — a portal capture source type (`types` bit 4 Virtual) so Agent 2 can screencast the
whole sandbox as **one** stream. Activities are a persisted, user-facing organizational concept;
hijacking one for an ephemeral agent sandbox pollutes the user's Activity list and couples to heavier
lifecycle (running/stopped) for no isolation benefit. We **read** `activities` for the confinement
check (defense in depth — a window pinned to a different Activity is also out of scope) but we do
**not** use Activities as the primary boundary. *Deviation note: none from INV; INV-2 lists
`vd_sandbox` as the capability and "virtual-desktop" as the target — this confirms VD.*

**KWin scripting primitives** (one-shot mutator script, loaded → `run()` → `stop()`):

- **Create / find the sandbox VD** — idempotent. JS:
  ```javascript
  // ak_sandbox_open.js  (one-shot, runs via Scripting.loadScript→run→stop)
  (function () {
    var found = null;
    for (var d of workspace.desktops)            // VirtualDesktop[]
      if (d.name === "Agent Kate Sandbox") { found = d; break; }
    if (!found) found = workspace.createDesktop(workspace.desktops.length, "Agent Kate Sandbox");
    callDBus("io.agentkate.Cowork", "/Sandbox", "io.agentkate.Cowork.Sandbox",
             "SandboxReady", found.id /*QUuid str*/, found.name, found.x11DesktopNumber);
  })();
  ```
  Go registers `io.agentkate.Cowork` `/Sandbox` and receives `SandboxReady(id,name,number)`; it
  persists the VD id as `SandboxState.vdId` (see §6). **`workspace.createDesktop` signature is a v1
  spike** — confirm arity (`createDesktop(position, name)`) against the running Plasma; fallback is
  bumping `workspace.desktopGridWidth`/`desktops` count then renaming. (Spike S1.)

- **Move a window onto the sandbox** — by `internalId` (matches Agent 3's window identity):
  ```javascript
  for (var c of workspace.windowList())
    if (c.internalId.toString() === TARGET_UUID) { c.desktops = [SANDBOX_VD]; c.onAllDesktops = false; }
  ```
  Setting `desktops = [vd]` (singleton array) makes membership exactly-the-sandbox; `onAllDesktops=false`
  prevents an all-desktops window from trivially defeating the predicate.

- **Query membership** — we do **not** add a parallel enumerator. Agent 3's window model already
  carries `desktops`; the sandbox predicate is `isSandboxed(w) := SANDBOX_VD ∈ w.desktops &&
  !w.onAllDesktops && SANDBOX_ACTIVITY-compatible(w)`. `cowork.sandboxWindows` filters Agent 3's
  snapshot by this predicate (no extra D-Bus round-trip).

- **Select / focus the sandbox** — `desktop_use_sandbox()` sets `workspace.currentDesktop = SANDBOX_VD`
  (one-shot script). This is the only call that changes what the *user* sees, so it is a deliberate,
  visible act (the UX "switch to watch the agent" — §5).

### 2. Confinement model — the `vd_sandbox` grant + the enforcement point

The `vd_sandbox` grant (capability `vd_sandbox`, `target = {kind:"virtual_desktop", vdId, vdName}`,
scope typically `session`) is **not itself a capture/control grant** — it authorizes *the existence
and use of a sandbox boundary*. Confinement works by **narrowing the `target` of capture/control
grants to the sandbox VD** and validating membership **at action time**.

**Enforcement point — the single chokepoint:** Agent 1's `Authorize(threadId, capability, target)`,
called imperatively at the top of every capture/control Go tool (`desktop_screenshot`,
`desktop_start_screencast`, `desktop_inject_input`, `desktop_click_element`). For a **sandbox-scoped**
grant (a grant whose `target.kind == "virtual_desktop"` and `target.vdId == SANDBOX_VD`), Authorize
additionally calls into this slice's `SandboxGuard.Check(target)`:

```
SandboxGuard.Check(actionTarget) →
  1. resolve actionTarget to a concrete Window via Agent 3's current model
     (by internalId; for a region/coord target, resolve the window under the geometry)
  2. reload that window's CURRENT desktops/onAllDesktops/activities from Agent 3
     (time-of-CHECK == time-of-USE: never trust a cached membership)
  3. require:  SANDBOX_VD ∈ w.desktops
            && w.onAllDesktops == false
            && (sandboxActivity == "" || w.activities ∋ sandboxActivity || w.activities is empty/all)
            && w is NOT an Agent Kate window  (INV-4 f: exclude AK's own surfaces)
  4. for a whole-VD screencast target: require the stream's VD == SANDBOX_VD (no per-window check)
  → allow only if ALL hold; else deny with reason "target left the sandbox"
```

**Exactly what it checks** (per action, never cached): (a) the target window's live `desktops`
includes the sandbox VD; (b) the window is **not** `onAllDesktops` (an all-desktops window straddles
the boundary → excluded); (c) `activities` is compatible (defense in depth); (d) the window is not an
Agent Kate surface (self-approval guard, INV-4 f); (e) for VD-wide screencast, that the stream is the
sandbox VD itself. **Time-of-check-equals-time-of-use:** because the agent can move a window *off* the
sandbox between grant and action, membership is re-read from Agent 3's live model on **every**
Authorize, not snapshotted at grant time. If a window has left the sandbox, the action is denied and an
audit row is written (`reason=escaped_sandbox`).

This keeps the gate **imperative and in Go** (INV-3 R2) and **reuses Agent 1's chokepoint** — no
second policy engine. SandboxGuard is a thin predicate Agent 1 invokes; it owns no consent state.

### 3. Launching apps into the sandbox

Two mechanisms; **recommend "launch + move via a transient window-added hook" for v1**, with a KWin
**window rule** as a v2 hardening:

- **`desktop_launch_in_sandbox(app)`** (recommend v1): Go spawns the app detached
  (`safe.Go` + `exec.CommandContext`, e.g. `kstart --` or the app's `.desktop` exec; sandboxed under
  the agent's own uid). Concurrently, a **long-lived event script** (loaded once, kept resident — §6)
  listens for `workspace.windowAdded`. On each add, the script reports `resourceClass`/`pid`/`caption`/
  `internalId` to Go. Go matches the new window against a **pending-launch table** (matched by `pid`
  primarily, `resourceClass` as fallback within a short window, e.g. 10 s) and, on match, fires the
  one-shot move script to set `desktops=[SANDBOX_VD]`. **Race:** a window may render on the current VD
  for a few frames before it's moved. Mitigation: pre-arm a **KWin window rule** (below) so the move is
  applied by KWin at map time rather than after; or accept the brief flash for v1 (the UX is "watch the
  agent," a flash on the sandbox VD is acceptable, and capture is gated on membership so a pre-move
  frame is never captured).

- **KWin window rule** (recommend v2): write a rule to `~/.config/kwinrulesrc` (or via the
  `org.kde.KWin` rules D-Bus) matching `wmclass`/`title` → force `desktop = <sandbox number>`, applied
  at window-map time. More robust (no flash, survives a missed `windowAdded`) but **mutates the user's
  persistent KWin config** and needs careful teardown (remove the rule on sandbox close). Defer to v2;
  v1's launch+move is contained entirely in our scripts with no persistent config writes.

- **`desktop_open_sandbox()`**: ensure the sandbox VD exists (create-or-find, §1) and the `vd_sandbox`
  grant is present (request via Agent 1's interactive flow if absent). Returns `{vdId, vdName, number,
  empty:true}`. Idempotent — calling twice does not create a second VD.

- **`desktop_use_sandbox()`**: switch `workspace.currentDesktop` to the sandbox so the user sees it.
  Distinct from `open` because *switching the user's view* is a visible side effect that the UX may
  want to gate or surface ("Agent switched you to the sandbox").

All app launches are **audited** (INV-4 d): `audit{action:"launch_in_sandbox", app, pid, vdId}`.

### 4. HONEST security caveat — what the sandbox does and does NOT isolate (READ THIS)

> **A KWin Virtual Desktop is an ORGANIZATIONAL boundary, NOT a security isolation boundary.** It
> groups windows for display and for our capture/control *targeting*. It is **not** a sandbox in the
> security sense. Do not present it to the user as "the agent is contained."

**What the VD sandbox DOES provide:**
- A **single visible place** to watch the agent operate (one VD, one screencast stream — §5).
- A **targeting scope** for *our* capture/control: AK only screenshots/screencasts/injects against
  windows whose live `desktops` include the sandbox VD (the §2 enforcement). This bounds **what AK
  itself will do**, which is the actual threat we gate (a confused/prompt-injected agent asking AK to
  click an out-of-sandbox window is *denied*).
- A clean **lifecycle handle**: "close the sandbox" can enumerate and kill exactly the windows on that
  VD (§6).

**What the VD sandbox does NOT isolate** (state plainly to the user in the consent UI — Agent 6):
- **Filesystem:** apps on the sandbox VD run as the user's uid with the user's full home dir,
  credentials, SSH keys, browser profiles. A browser opened "in the sandbox" can read `~/.ssh`.
- **Clipboard:** Klipper/global clipboard is shared across all VDs. Copy in the sandbox → paste anywhere.
- **Global input / other apps:** real keyboard/mouse and X11/Wayland input are **not** partitioned by
  VD. Other apps can still receive global shortcuts, and a malicious app on the sandbox VD can affect
  the whole session (e.g. `xdotool`/`ydotool`-class tools, IPC to other apps).
- **Network, secrets, env:** full user network access, kwallet, dbus session bus — all shared.
- **The boundary is voluntary:** the agent (or any app) can call KWin to **move a window off the
  sandbox** or set `onAllDesktops`. We *detect* this (membership re-read denies capture/control on an
  escaped window — §2) but we **cannot prevent** the window from leaving. A VD is a display grouping,
  not a jail.
- **Not a privilege boundary:** nothing here drops capabilities, namespaces, or seccomp. An app
  launched in the sandbox has exactly the privileges the user has.

**Path to TRUE isolation (explicit future work, NOT in this plan):**
1. **Nested `kwin_wayland`** — run a child compositor inside an AK window (`kwin_wayland --socket
   ak-sandbox --width … --height …`), launch sandbox apps with `WAYLAND_DISPLAY=ak-sandbox`. Apps then
   physically cannot see the host's windows/input/clipboard; AK captures the nested compositor's output
   directly. This is the kwin-mcp-adjacent direction and the real isolation story. **Still shares the
   filesystem** unless combined with (3).
2. **Separate Wayland session / dedicated user** — run sandbox apps as a **separate, low-privilege
   user** with its own home, no access to the operator's secrets; capture that user's compositor.
3. **Container / VM** — bubblewrap/flatpak/podman or a microVM with a virtual display; the strongest
   filesystem+network isolation, the most plumbing, the highest latency for capture.

**v1 ships the VD sandbox with this caveat surfaced loudly in the consent dialog** (Agent 6 renders
the "this does not isolate files/clipboard/input" warning as `KMessageBox::Dangerous` itemized text).
Nested-compositor isolation is a **v3** item (L, its own plan). We do not oversell v1.

### 5. Interaction with capture/control

- **Screencast the whole sandbox as one stream** (Agent 2): with the `vd_sandbox` grant present, a
  `screencast` grant whose `target` is the sandbox VD lets Agent 2 call ScreenCast `SelectSources` with
  `types` bit **4 (Virtual)** (Plasma 6.5+) and pick the sandbox VD → a **single** PipeWire node for the
  whole sandbox. The user watches one preview that shows everything the agent does. **Fallback (Plasma
  < 6.5, no Virtual source type):** screencast each sandbox window individually (per-window `types` bit
  2) or fall back to `desktop_screenshot` stills (INV-6). Capability-detect at runtime (Spike S2).
- **Confine input injection** (Agent 4): every `desktop_inject_input` / `desktop_click_element`
  Authorize runs SandboxGuard.Check against the resolved target window — input only reaches windows
  live-resident on the sandbox VD, and **never** an Agent Kate window (INV-4 f). Coordinate-based
  injection resolves the window under the coordinate first; if it's outside the sandbox, deny.
- **The "user watches the agent work" UX:** `desktop_use_sandbox()` switches the user to the sandbox
  VD; the Cowork panel (Agent 6) shows the live screencast preview of the sandbox + an "active grants"
  chip + the kill-switch. The user sees the browser/LibreOffice the agent drives, in real time, in one
  place, and can revoke or kill at any moment. **Live frames stay UI-side** (INV-1/INV-6) — the agent
  gets stills on demand, the human gets the stream.

### 6. Lifecycle

- **Create on demand:** the sandbox VD is created lazily on first `desktop_open_sandbox()` (or first
  `launch_in_sandbox`). Nothing is created at AK startup. `SandboxState{ vdId, vdName, number,
  createdByAk: bool, launchedPids: []int, eventScriptId: int32 }` lives in `core/internal/cowork`
  (mutex-guarded, copy-out on read — Context Pack §1) and persists alongside Agent 1's consent file
  (`$XDG_DATA_HOME/agentkate/cowork-sandbox.json`, atomic temp+rename).
- **Long-lived event script:** the `windowAdded` hook (§3) is loaded **once** on first sandbox open and
  its `scriptId` recorded. It is **explicitly `stop()`ped in `gracefulShutdown`** (`main.go:302-323`)
  and on sandbox close, so it doesn't leak (REFERENCE-skill: scripts persist until KWin restarts).
- **Persistence across AK restart:** the VD itself persists in KWin until removed. On AK restart we
  **re-find** the sandbox VD by name (don't blindly create a second one) and reload `SandboxState`.
  **Grants do not auto-resurrect** — per Agent 1's restart semantics, live capture/control grants may
  auto-revoke on restart; the `vd_sandbox` grant follows Agent 1's scope rules (a `session` grant dies
  with the thread/session).
- **Close on session end / kill-switch:** `cowork.closeSandbox` (and the global kill-switch from Agent
  1, INV-4 d) does, in order: (1) revoke all sandbox-scoped grants for the thread; (2) optionally
  **close the apps** — enumerate windows on the sandbox VD and `c.closeWindow()` (graceful) for any
  whose `pid ∈ launchedPids`, escalating to `SIGTERM` the tracked pids if needed (only AK-launched
  pids — never the user's pre-existing windows); (3) optionally **remove the VD**
  (`workspace.removeDesktop(vd)`) **only if `createdByAk` and the VD is empty** (don't delete a VD the
  user adopted or that still holds user windows); (4) `stop()` the event script. **Kill-switch is
  immediate and global** (cuts every thread's desktop access). What persists after a normal close: the
  audit log (append-only, INV-4 d). What does not: grants, the VD (if AK-created and empty), live streams.
- **Default close behavior is conservative:** v1 default = revoke grants + stop scripts, **leave the
  VD and apps** (least surprising — the user may want to keep what's open). "Close apps + remove VD" is
  an explicit user choice (a checkbox in the close confirmation, Agent 6).

### 7. MCP tools + `cowork.*` RPCs

**MCP tools** (added in `core/cmd/akcore/mcp.go`: `toolDefs()` entry + `runTool` case → `b.client.Call`):

| Tool | Capability / tier | Consent | Does |
|---|---|---|---|
| `desktop_open_sandbox()` | `vd_sandbox` / R0-ish | session grant (interactive first time) | create-or-find sandbox VD; ensure grant; return `{vdId,vdName,number}` |
| `desktop_launch_in_sandbox(app)` | `vd_sandbox` | session grant + audited | launch app, move its window onto sandbox VD |
| `desktop_use_sandbox()` | `vd_sandbox` | session grant | switch user's current VD to the sandbox (visible) |

Capture/control tools (`desktop_screenshot`, `desktop_start_screencast`, `desktop_inject_input`,
`desktop_click_element`) are **owned by Agents 2/4**; this slice only contributes the SandboxGuard
predicate their Authorize calls invoke. No new capture/control tool is added here.

**Core RPCs** (registered statically in `registerHandlers`, `main.go:437`):

| RPC | Params | Returns | Notes |
|---|---|---|---|
| `cowork.openSandbox` | `{threadId}` | `{vdId,vdName,number,created}` | create-or-find; idempotent |
| `cowork.launchInSandbox` | `{threadId, app, args?}` | `{pid, matched:bool, windowId?}` | spawn + window-added match + move |
| `cowork.useSandbox` | `{threadId}` | `{ok}` | set currentDesktop |
| `cowork.sandboxWindows` | `{threadId}` | `[{internalId,caption,resourceClass,pid,onSandbox:true}]` | filters Agent 3's snapshot by the sandbox predicate; **no extra D-Bus** |
| `cowork.closeSandbox` | `{threadId, closeApps?, removeVd?}` | `{closed,removedVd}` | revoke grants, optional kill apps/remove VD |

**Notifications (core→UI):** `cowork.sandboxChanged` (membership/lifecycle delta — coalesced ≥25 ms,
INV-6) and the shared `cowork.sandboxWindows` pull RPC for re-derivation. Lifecycle events also ride
Agent 1's `cowork.sessionChanged`/`cowork.killSwitch`. SandboxGuard denials append to Agent 1's audit
log; no new notification needed for denials (they surface in the audit/active-grants view).

---

## Implementation steps

1. **(Go) Sandbox state + scripts.** `core/internal/cowork/sandbox.go`: `SandboxState` (mutex-guarded,
   copy-out), atomic persist to `cowork-sandbox.json`. Embed the three JS scripts
   (`ak_sandbox_open.js`, `ak_sandbox_move.js`, `ak_sandbox_event.js`). Register D-Bus service
   `io.agentkate.Cowork` `/Sandbox` to receive `SandboxReady` + `windowAdded` reports. Depends on the
   `godbus` dep being added (Context Pack §5) — coordinate so it's added once across Agents 3/4/5.
2. **(Go) KWin glue** in `core/internal/kde/kwin.go` (Agent 3's file): add `CreateOrFindSandboxVD`,
   `MoveWindowToVD(internalId, vd)`, `SetCurrentVD(vd)`, `RemoveVD(vd)`, `LoadEventScript`/`StopScript`.
   These are thin `loadScript→run→stop` wrappers reusing Agent 3's connection. **Coordinate with Agent
   3** so we share one KWin client + one window model, not two.
3. **(Go) SandboxGuard** `core/internal/cowork/sandbox_guard.go`: `Check(target) error` implementing
   §2. **Wire into Agent 1's `Authorize`** at the sandbox-scoped branch (coordinate the exact call
   signature with Agent 1). Pure predicate over Agent 3's live window model — no consent state.
4. **(Go) RPC handlers** in `registerHandlers` (`main.go:437`): `cowork.openSandbox`,
   `cowork.launchInSandbox`, `cowork.useSandbox`, `cowork.sandboxWindows`, `cowork.closeSandbox`. Add
   sandbox deps to `handlerDeps` (`main.go:420`). Each handler carries a timeout (Context Pack §1).
5. **(Go) MCP tools** in `core/cmd/akcore/mcp.go`: `toolDefs()` entries + `runTool` cases for
   `desktop_open_sandbox`, `desktop_launch_in_sandbox`, `desktop_use_sandbox`, each → one
   `b.client.Call("cowork.…")`. Gate behind the `cowork` MCP server (INV-5).
6. **(Go) Lifecycle teardown** in `gracefulShutdown` (`main.go:302-323`): `stop()` the event script,
   persist `SandboxState`. Wire `cowork.closeSandbox` + kill-switch to revoke + optional app-kill/VD-remove.
7. **(UI) Consent + watch UX** (Agent 6): the `vd_sandbox` consent dialog must render the §4 caveat as
   `KMessageBox::Dangerous` itemized warning ("does NOT isolate files/clipboard/input"); the Cowork
   panel shows the sandbox screencast preview + active grants + close-sandbox (with "close apps /
   remove VD" checkboxes).
8. **Tests** (INV-2 mandate): create/find idempotency; move-to-VD; SandboxGuard allow/deny incl.
   **escaped-window deny** and **onAllDesktops deny** and **AK-window deny**; close with/without
   app-kill + VD-remove; restart re-find (no duplicate VD); event-script stop on shutdown;
   concurrent open from two threads.

---

## Risks / considerations

- **Organizational ≠ security (the headline risk).** Mitigation: §4 surfaced loudly in consent UI;
  enforcement gates *AK's own* actions (the real threat), not the app's. Cross-ref Agent 1 (audit),
  Agent 6 (warning UI). True isolation is v3 (nested compositor).
- **Window escapes the sandbox between grant and action.** Mitigation: **time-of-check membership
  re-read** in SandboxGuard (§2) — never cache; deny + audit on escape. The agent moving a window out
  cannot smuggle a capture/click of an out-of-sandbox window.
- **`onAllDesktops` straddle.** A window on all desktops is "in" the sandbox by set-membership but also
  everywhere else. Mitigation: predicate **excludes `onAllDesktops`** windows from sandbox scope; the
  move script sets `onAllDesktops=false`.
- **Launch race (window renders on wrong VD briefly).** Mitigation: v1 accepts the flash (capture is
  membership-gated, so a pre-move frame is never captured); v2 KWin window rule applies the VD at map
  time. Cross-ref §3.
- **KWin window rule pollutes user config (if v2).** Mitigation: write a tagged rule, remove it on
  sandbox close; prefer the script path for v1 (no persistent config writes).
- **Event script leak.** A persistent KWin script outlives the process until KWin restarts. Mitigation:
  record `scriptId`, `stop()` in `gracefulShutdown` + on close; on restart, defensively `stop()` any
  prior `agentkate_*` script before reloading.
- **Killing the wrong app on close.** Mitigation: only kill `pid ∈ launchedPids` (AK-launched);
  `removeVd` only if `createdByAk && empty`. Never touch user-owned windows.
- **Plasma version drift.** `types`=Virtual screencast needs **6.5+** (Spike S2 capability-detect,
  fallback per-window/stills). `workspace.createDesktop` arity (Spike S1). Plasma 6.5.x portal-dialog
  virtual-input bug (REFERENCE-skill Gotchas) affects the *consent* click, not the sandbox itself.
- **Self-approval loop (INV-4 f).** AK windows are **never** sandbox-scoped targets — SandboxGuard
  explicitly denies AK surfaces; consent files are core-owned. The agent cannot click AK's own consent UI.
- **Multi-agent.** Per-thread grants; one shared sandbox VD is simplest for v1 but two threads sharing
  it muddies attribution. v1: one sandbox VD, grants per-thread (an action's grant identifies the
  thread). v2 consideration: per-thread sandbox VDs (named `Agent Kate Sandbox — <thread>`). Open Q.

---

## Acceptance

- `desktop_open_sandbox()` creates a VD named "Agent Kate Sandbox" (or re-finds it; never duplicates),
  returns its id/number, and is idempotent across repeated calls and across an AK restart.
- `desktop_launch_in_sandbox("firefox")` opens Firefox and its window ends up with
  `desktops == [sandbox VD]` and `onAllDesktops == false`; the launch is audited.
- `cowork.sandboxWindows` returns exactly the windows whose **live** membership is the sandbox VD,
  derived from Agent 3's model with no extra KWin round-trip.
- A `screenshot`/`screencast`/`inject_input` Authorize for a sandbox-scoped grant **succeeds** for a
  window on the sandbox VD and **fails (audited, reason=escaped_sandbox)** the instant that window is
  moved off the sandbox or set `onAllDesktops` — verified by a test that moves the window between grant
  and action.
- Authorize **always denies** an Agent Kate window as a sandbox capture/control target.
- With Plasma 6.5+, a single screencast of the sandbox VD shows all sandbox windows in one stream;
  on < 6.5 it falls back (per-window or stills) without erroring.
- `cowork.closeSandbox{closeApps:true,removeVd:true}` kills only AK-launched pids, removes the VD only
  if AK-created and empty, revokes all sandbox grants, and `stop()`s the event script; default close
  (no flags) leaves apps + VD and only revokes grants + stops scripts.
- The kill-switch immediately revokes sandbox grants across all threads and stops capture/control.
- The consent dialog plainly states the sandbox does **not** isolate files/clipboard/input.
- Every goroutine uses `safe.Go`; no mutex is held across a D-Bus call; the event script is stopped on
  `gracefulShutdown`.

---

## Phasing (S/M/L · v1/v2/v3)

- **v1 (M):** create/find sandbox VD; `desktop_open_sandbox` / `desktop_use_sandbox` /
  `desktop_launch_in_sandbox` (launch + move via window-added hook, accept brief flash); SandboxGuard
  wired into Agent 1's Authorize (membership re-read, escaped/onAllDesktops/AK-window denies);
  `cowork.sandboxWindows` over Agent 3's model; conservative close (revoke + stop scripts, leave VD/apps);
  consent caveat in UI; tests. *Depends on Agents 1+3 landing first; Agent 2 stills suffice for "watch."*
- **v2 (M):** whole-VD single-stream screencast (Agent 2 `types`=Virtual, capability-detect + fallback);
  KWin window rule for flash-free launch; `closeSandbox{closeApps,removeVd}` with app-kill; optional
  per-thread sandbox VDs.
- **v3 (L):** **true isolation** — nested `kwin_wayland` compositor (separate `WAYLAND_DISPLAY`), with
  capture pointed at the nested compositor; separate-user / container path scoped as its own plan. This
  is where the §4 caveat is actually retired.
