# Agent Kate — Security Model

This document states, plainly and honestly, what Agent Kate's defenses do and do
not protect against. It is a threat-model document: understatement is the failure
mode. Every claim is grounded in a `file:line` reference to the code that makes it
true. Paths are relative to the repository root.

If you take one thing from this doc, take this:

> **Agent Kate runs the USER'S agents with the USER'S credentials at the USER'S
> uid. It is not a sandbox and does not pretend to be one.** Its defenses are
> proportionate to a *prompt-injection-driven* adversary (a poisoned repo or web
> page steering an otherwise-trusted agent), not to a determined local attacker
> who already controls your login session.

---

## 0. The core trust assumption

Each agent is a headless `claude` process with `Bash`/`Edit`/`Write`, running in
its own git worktree at the **same uid as `akcore`**. A coding agent with a shell
at your uid can already read your SSH keys, your KWallet, your browser profiles,
and `socat` any of your local sockets — Agent Kate does not create that exposure,
it inherits it. This is stated as the first root cause of the Cowork adversarial
review (`docs/plans/08-kde-cowork/08-review-findings.md:17-35`) and ratified by
the user as the project's trust posture
(`docs/plans/08-kde-cowork/08-review-findings.md:139-142`).

The realistic adversary the defenses below target is **prompt-injection-driven
misbehavior**, not a same-uid attacker
(`docs/plans/08-kde-cowork/08-review-findings.md:30-35`). Same-uid processes are
**not isolated from each other** by anything in this codebase.

---

## 1. IPC transport — per-uid protection only

`akcore` and the UI talk JSON-RPC 2.0 over a Unix domain socket
(`core/internal/ipc/server.go:48-49`). The socket is protected two ways, both of
which stop *other users on a shared host* and neither of which isolates a
*same-uid* process:

- The socket lives in `$XDG_RUNTIME_DIR`, which is mode `0700` (user-only). The
  code documents this as the primary protection
  (`core/internal/ipc/server.go:95-100`).
- Defense in depth: the socket file itself is chmod'd to `0o600`
  (`core/internal/ipc/server.go:98`). The same comment is explicit that this
  "does not stop a same-uid process (see 08 §F Option A), but it does stop other
  users on a shared host" (`core/internal/ipc/server.go:95-97`).

The server also applies resource-exhaustion backpressure, which is availability
hardening, not an isolation boundary:

- A per-connection cap on in-flight dispatch goroutines,
  `maxInFlightPerConn = 256` (`core/internal/ipc/server.go:41`), acquired before
  each handler runs so a stalled client applies backpressure to itself alone
  (`core/internal/ipc/server.go:167-181`). The cap is deliberately per-connection,
  not global, so a long-blocking handler cannot be released by a frame on a
  different connection and deadlock
  (`core/internal/ipc/server.go:36-40`).
- A single writer goroutine per connection with a `writeDeadline = 30s`
  (`core/internal/ipc/server.go:25-27`, `core/internal/ipc/server.go:385-405`),
  so a dead-but-not-closed client cannot park a producer forever.
- Inbound frames are capped at `maxFrameBytes = 16 MiB`
  (`core/internal/ipc/server.go:17-19`, `core/internal/ipc/server.go:158-159`).

**Bottom line:** the socket is a per-uid boundary. Any process at your uid can
connect to it.

### Orchestration approvals ride on self-asserted identity

The plan-16 orchestration verbs (`send_agent` / `close_agent` / `discard_agent`
targeting a thread outside the caller's own worker subtree) require a one-time
human approval per (caller, target, action), asked through the normal permission
flow (`core/cmd/akcore/orchestrate.go`, `authorizeAgentTarget`). Note that the
`fromThreadId` this gate keys on is **self-asserted** by the calling bridge
process — the IPC server does not bind orchestration RPCs to a connection
identity the way the Cowork keystone does (§2). Within the trust model above
that is the intended tier: the gate is a guardrail that keeps a *well-behaved*
agent from steering or destroying threads it doesn't own without the human
noticing — it is not authentication, and any same-uid process (including an
agent's own tools talking to the socket directly) could assert another thread's
id. Grants live in memory only and last for the current core run.

---

## 2. The Cowork keystone — per-connection identity

Cowork (the desktop see/control feature) needs to distinguish the UI (which may
answer grant prompts, press the kill-switch, run portal sessions) from an agent's
MCP bridge (which may only *request* consent-gated capabilities). The IPC server
therefore carries a per-connection identity — role, bound thread id, and an
`isPrimary` flag — set once at handshake/first-use and read by the consent
handlers (`core/internal/ipc/server.go:278-301`, `core/internal/ipc/server.go:413-447`).

### What the keystone enforces

- **`MarkUI`** tags a connection as the UI; the first UI to handshake becomes the
  primary that runs portal sessions (`core/internal/ipc/server.go:451-468`). It is
  called from the `handshake` handler (`core/cmd/akcore/main.go:501-504`).
- **`BindBridge`** tags a connection as an agent bridge for a thread on first use
  (trust-on-first-use) and rejects two things: a UI connection trying to invoke an
  agent capability, and a bridge trying to switch to a *different* thread
  (`core/internal/ipc/server.go:470-493`). This is what stops cross-thread grant
  theft: thread A's bridge cannot claim `threadId:B` to spend B's grant.
- **`bridge.identify`** is the same binding, done up front: every `akcore mcp`
  bridge (Cooperation and Cowork alike) calls it once at startup
  (`core/cmd/akcore/mcpactivity.go`), so a connection's role and thread are
  known before its first tool call rather than on first Cowork use. It carries
  no new trust — it is the same trust-on-first-use assertion `BindBridge`
  always made — but it is what lets the core attribute the `mcp.activity` feed
  to a thread from the connection instead of a self-asserted parameter.
- **`RequireUI`** gates the UI-only RPCs — grant responses, kill-switch,
  revoke, enable — so an agent can never grant itself anything
  (`core/internal/ipc/server.go:495-500`). `NotifyUI` is its notification
  counterpart: the `mcp.activity` feed goes only to UI connections, so one
  agent never receives another agent's tool traffic.
- **`NotifyPrimaryUI`** routes portal requests only to the primary UI, and
  returns false (fail-closed) when no primary UI is connected
  (`core/internal/ipc/server.go:502-518`; the primary is cleared on disconnect at
  `core/internal/ipc/server.go:146-148`).

These are actually wired at the RPC handler layer: `requireCoworkBridge` calls
`BindBridge` and additionally requires the bound thread has opted into Cowork
(`core/cmd/akcore/cowork.go:1221-1235`); `requireUI` calls `RequireUI`
(`core/cmd/akcore/cowork.go:1237-1243`).

### What the keystone STOPS

1. **Prompt-injection via a co-resident agent's bridge.** A poisoned repo steering
   agent A cannot make A's bridge grant itself a capability — grants come only from
   the UI (`core/internal/ipc/server.go:495-500`), and an agent bridge connection is
   refused the UI role (`core/internal/ipc/server.go:481-482`).
2. **A bridge spending another thread's grant.** A bridge is bound to one thread for
   its lifetime and rejected if it presents a mismatched thread id
   (`core/internal/ipc/server.go:489-491`).

### What the keystone DOES NOT stop

It does **not** stop a determined same-uid process from forging a handshake on the
raw socket. The role is self-asserted on first contact (trust-on-first-use); any
process at your uid can open the socket (see §1), call `handshake`, and thereby
become "the UI" — answering its own grant prompts, disarming the kill-switch, or
flipping `CoworkEnabled`. The code says this in its own words: "a malicious local
process can still forge a handshake on the raw socket (Option A, 08 §F). This gate
stops the realistic prompt-injection path … not a determined raw-socket forger"
(`core/internal/ipc/server.go:421-424`). The original finding (the raw-socket
bypass) is `docs/plans/08-kde-cowork/08-review-findings.md:27`.

**Same-uid processes are not isolated from one another.** The keystone raises the
bar for an *in-band agent acting through its own MCP bridge*; it is not a sandbox.

---

## 3. Third-party provider env-strip — no credential forwarding

Agents can be routed at a third-party, Anthropic-compatible endpoint (Fireworks,
OpenRouter, …) purely by environment injection
(`core/internal/agent/provider.go:8-30`). The security-relevant property is that a
real Anthropic key sitting in `akcore`'s environment is **never forwarded to a
third-party base URL**.

`buildEnv` owns a fixed set of `ANTHROPIC_*` / `CLAUDE_CODE_*` variables
(`core/internal/agent/provider.go:53-63`) and, when a provider is active, **strips
every one of them from the inherited environment** before appending the provider's
own base URL, token, and model overrides
(`core/internal/agent/provider.go:100-110`). So a real Anthropic key in akcore's
environment can never reach a foreign endpoint, and no stale override leaks
through (`core/internal/agent/provider.go:48-52`).

The resolved secret is supplied per launch over the trusted local IPC socket and
is **never persisted to disk or written to logs**
(`core/internal/agent/provider.go:18-22`). A routed provider with no resolvable
credential returns an error rather than spawning an unauthenticated request
(`core/internal/agent/provider.go:82-98`). For "Claude direct" (empty BaseURL) the
environment is returned unchanged, so default agents are byte-for-byte unaffected
(`core/internal/agent/provider.go:84-87`).

This is asserted by test: `TestBuildEnvScrubsInheritedAnthropicVars` injects a
literal `real-claude-key` and fails if that substring appears anywhere in the
routed environment (`core/internal/agent/provider_test.go:81-86`).

---

## 4. Cowork consent — grants, audit, kill-switch

The Cowork consent authority is the policy brain for every desktop see/control
action. Its design choice — ratified as Option A — is **"detect, not prevent"**:
because the agent runs at the same uid, the consent and audit files are within its
reach, so the design re-derives trust server-side and makes tampering *detectable*
rather than impossible (`core/internal/cowork/grants.go:1-12`).

### Grants

- Every grant's `GrantedBy` is hardcoded to `"user"` and re-derived server-side on
  `Add` — the on-disk value is never trusted
  (`core/internal/cowork/grants.go:117-118`, `core/internal/cowork/grants.go:278`,
  `core/internal/cowork/grants.go:209`).
- Grants are per-thread and capability-scoped; `Match` keys on `ThreadID` and a
  strict target-cover test (`core/internal/cowork/grants.go:291-304`,
  `core/internal/cowork/grants.go:429-450`).
- R2 (control) actions outside the sandbox are **per-action**: never satisfied by a
  remembered grant, regardless of the scope the user picked
  (`core/internal/cowork/consent.go:172-182`,
  `core/internal/cowork/consent.go:210-213`).
- Capabilities have a fixed risk tier; an unknown capability is treated as the
  highest risk (fail-safe) (`core/internal/cowork/grants.go:55-67`).
- A hard self-target guard refuses any R2 action aimed at Agent Kate's own UI
  (`core/internal/cowork/consent.go:157-160`), with a geometric analogue that stops
  the agent moving the pointer onto AK's own Allow/kill-switch buttons and clicking
  (`core/internal/cowork/consent.go:459-482`).

### Audit hash chain — DETECTION, not prevention

The audit log is an append-only JSONL file where each entry is hash-chained:
`Hash = sha256(canonical(entry with Hash=""))` and `PrevHash` links to the prior
entry, so truncation or in-place mutation is detectable on load
(`core/internal/cowork/audit.go:32-48`, `core/internal/cowork/audit.go:109-115`).
On load the chain is verified; a mismatch sets `tampered=true`
(`core/internal/cowork/audit.go:59-100`), and the authority then **fails closed** —
`Authorize` denies all access when the audit chain failed verification
(`core/internal/cowork/consent.go:152-156`).

This is detection, not prevention, and the code says so. A same-uid writer can
replace every line and recompute the chain from a new genesis; nothing anchors the
head hash outside the agent's reach. This exact limitation is recorded as finding
A2 (`docs/plans/08-kde-cowork/08-review-findings.md:26`) and the package doc states
the v1 posture is "detect, not prevent," with true tamper-prevention (privilege
separation) scheduled as v2 hardening
(`core/internal/cowork/grants.go:6-11`,
`docs/plans/08-kde-cowork/08-review-findings.md:131-134`).

### Kill-switch — DETECTION-tier control

The kill-switch revokes all grants, tears down every live session, clears the
global policy toggles, and disables new access until re-armed
(`core/internal/cowork/consent.go:292-317`). It is gated UI-only via `RequireUI`
(`core/cmd/akcore/cowork.go:1237-1243`). Like the audit chain, it is a control the
*user* operates after observing behavior — it presumes a non-malicious-local
adversary, since a same-uid process that forged the UI role (§2) could re-arm it
(`core/internal/cowork/consent.go:357-366`).

### Global policy toggles

A capability switched on in the policy is allowed with no per-action prompt, but
still passes the kill-switch, audit-tamper, and self-target hard guards, and every
pre-authorized action is still audited (`core/internal/cowork/policy.go:19-25`,
`core/internal/cowork/consent.go:162-169`).

---

## 5. Cowork access is "you with a keyboard"

When you grant a control capability, you are not putting an agent in a box. You are
handing it the equivalent of **your own keyboard and mouse**: R2 capabilities
inject real input and act "as the user" (`core/internal/cowork/grants.go:41-43`,
`core/internal/cowork/grants.go:49-53`). Anyone enabling Cowork must understand
this. The plan mandates first-run risk education for exactly this reason
(`docs/plans/08-kde-cowork/08-review-findings.md:111`).

The defenses above (per-action R2 consent, self-target guards, audit, kill-switch)
constrain **what Agent Kate itself will do** on your behalf — a confused or
prompt-injected agent asking AK to click an out-of-scope window is denied
(`docs/plans/08-kde-cowork/05-sandbox.md:191-193`). They do not, and cannot, turn
the agent into a sandboxed entity.

---

## 6. The KWin virtual-desktop "sandbox" is ORGANIZATIONAL, not security

The Cowork sandbox places agent activity on its own KWin virtual desktop. **A KWin
virtual desktop is an organizational boundary, not a security isolation boundary.**
This is stated in the plan and must not be mistaken for isolation
(`docs/plans/08-kde-cowork/05-sandbox.md:182-186`,
`docs/plans/08-kde-cowork/07-wiring-and-roadmap.md:31`).

What it does **not** isolate (`docs/plans/08-kde-cowork/05-sandbox.md:197-209`):

- **Filesystem:** apps "in the sandbox" run as your uid with your full home dir,
  SSH keys, and browser profiles. A browser opened in the sandbox can read `~/.ssh`.
- **Clipboard:** the global clipboard is shared across all virtual desktops.
- **Global input / other apps:** real keyboard/mouse and X11/Wayland input are not
  partitioned by virtual desktop.
- **Network, secrets, env:** full user network, KWallet, and the D-Bus session bus
  are all shared.
- **The boundary is voluntary:** any app (or the agent) can ask KWin to move a
  window off the sandbox VD; AK *detects* the escape and denies capture/control on
  the escaped window, but cannot *prevent* the window from leaving.
- **Not a privilege boundary:** nothing drops capabilities, namespaces, or seccomp.

The sandbox's real value is a single visible place to watch the agent operate and a
*targeting scope* that bounds what AK itself does — not containment of the agent
(`docs/plans/08-kde-cowork/05-sandbox.md:188-195`).

---

## 7. Summary table

| Defense | What it is | Stops | Does NOT stop |
|---|---|---|---|
| Socket `0o600` + `$XDG_RUNTIME_DIR` `0700` (`core/internal/ipc/server.go:95-100`) | Per-uid transport boundary | Other users on a shared host | Any same-uid process connecting |
| Cowork keystone identity (`core/internal/ipc/server.go:413-518`) | Per-connection role/thread binding | Agent self-granting via its bridge; cross-thread grant theft | A same-uid process forging a handshake on the raw socket |
| Provider env-strip (`core/internal/agent/provider.go:100-110`) | Credential isolation | Anthropic key leaking to a third-party base URL | — (this is a genuine prevention) |
| Audit hash chain + fail-closed (`core/internal/cowork/audit.go:32-48`, `core/internal/cowork/consent.go:152-156`) | Tamper **detection** | Naive truncation/in-place edits | A same-uid full-file rewrite-and-rechain |
| Kill-switch (`core/internal/cowork/consent.go:292-317`) | User panic button (UI-only) | Standing grants/sessions after the user reacts | A same-uid process that forged the UI role |
| KWin VD sandbox (`docs/plans/08-kde-cowork/05-sandbox.md:182-209`) | Organizational grouping | Mis-targeted AK actions | Filesystem/clipboard/input/network/privilege — not isolation |

---

## 8. Future hardening (not yet shipped)

True tamper-*prevention* requires privilege separation — running the consent/audit
authority (or all of `akcore`) as a separate uid the agent's tools cannot write to.
This is Option B in the review and is scheduled as v2/v3 hardening, not present
today (`docs/plans/08-kde-cowork/08-review-findings.md:131-142`).
