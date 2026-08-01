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

### Launching agents: an agent cannot give a worker authority it does not hold

`launch_agent` creates a real agent thread on behalf of another agent. Until the
2026-08-01 audit (F1) it forwarded the caller's `permission_mode` and `isolation`
verbatim, so an agent running in `acceptEdits` — where Bash stops and asks the
human — could launch itself a `bypassPermissions` worker in the user's **main
checkout** with no prompt anywhere in the chain. That turned the documented
"determined same-uid attacker" (out of scope, §0) into a pure prompt-injection
capability, which is squarely *in* scope.

`agent.launchWorker` now runs every agent-initiated launch through
`core/cmd/akcore/authority.go` before anything is created:

- **One cross-engine permissiveness ordering**
  (`plan < default < acceptEdits < auto < dontAsk < bypassPermissions`, with
  kimi's `yolo` on the same rung as `bypassPermissions`). A requested mode above
  the launcher's own mode needs one human approval, asked through the same
  broker and the same `permission.requested` flow as every other gated tool, so
  it appears in the launching agent's own panel. Fail-closed both ways: an
  **unknown** requested mode ranks above every known mode (so a future engine's
  vocabulary is asked about, not assumed safe), and an unknown or missing mode on
  the **launcher** delegates nothing.
- **An *effective* isolation of `workspace` always asks**, regardless of the
  launcher's own isolation. Running in the human's main checkout is authority,
  not a preference. *Effective*, not requested: `worktree.Create` silently
  degrades `auto` (and the unspecified default) to the real checkout whenever the
  project has no commit to branch from — a fresh repo, or a workspace that is not
  a git repository — so a gate that matched the literal string `"workspace"` let
  a worker land in the user's own files while the dialog stayed shut. The gate
  asks the worktree layer what *this project* would actually do
  (`worktree.EffectiveIsolation`) and gates on that answer; the dialog names it
  too. `TestEffectiveIsolationIsWhatIsGated`.
- **Restrictions are inherited.** A worker launches with its parent's
  `disallowedTools`, `addDirs`, `strictMcpConfig` and `maxBudgetUsd`. Until the
  follow-up pass these were not passed at all, so *every* worker escaped its
  launcher's deny-list by default — a thread launched with `--disallowedTools
  Bash` could launch one with Bash and never involve the human. A launch that
  asks for **fewer** disallowed tools, or for a directory the launcher cannot
  reach, is an escalation like any other and asks. Both lists are **normalised
  once, at the boundary** (`inheritRestrictions`), and the launch gets the
  normalised values: the check used to compare trimmed entries while the caller's
  untrimmed slice went to the CLI, so `" Bash"` passed the escalation check as
  `Bash` and then banned nothing. `TestRestrictionEntriesAreNormalisedOnce`.
- **Concurrency caps** (`maxLiveWorkersPerParent = 8`,
  `maxLiveWorkersPerTree = 24`), taken as a **reservation** under one mutex
  together with the live count. Counting persisted records alone was
  check-then-act: a worker's record is written after the CLI handshake, seconds
  after the gate ran, so N concurrent `launch_agent` calls all passed one count.
  The slot is held across the human's approval and released when the launch
  finishes. Counted over the whole orchestration tree, so a worker launching
  sub-workers cannot multiply past the cap. A cap is a **refusal**, not a
  question — asking the human to raise it on the agent's behalf would make it no
  cap at all.
- **The caller is bound to the parent it names.** `agent.launchWorker` measures
  the requested authority against the *parent thread's*, which measures nothing
  if any connection can name any parent; the handler requires the caller to
  already BE that thread's bridge (`requireUIOrOwnBridge` → `RequireBridge`),
  which since the F13 fix below means it presented the secret that thread's
  bridge was launched with. A connection that never identified can launch
  nothing at all.
- **…and so is every other per-thread caller** (2026-08-01). The same reasoning
  applies to `fromThreadId`, the parameter `agent.send`, `agent.stopClose` and
  `agent.discard` gate on: `authorizeAgentTarget` is arithmetic on two thread
  ids and authenticates nobody, so with the id unbound a bridge could name its
  controller as itself and inherit that controller's whole subtree — or omit the
  field, which the gate reads as "the UI" and waves through with no ask at all.
  All three now run `requireCallerThread` first (`core/cmd/akcore/orchestrate.go`):
  the UI passes, a bridge passes only for its OWN thread, anything else is
  refused before the target is even considered. Tests:
  `TestPerThreadHandlersBindTheCaller`, `TestUIActsWithoutNamingAThread`,
  `TestDiscardGoesThroughGate`.
- **The prompt is written to be read.** The dialog states, in plain words: that
  approving starts a new agent with more authority than the asker (only when
  that is true), the mode it wants against the mode it holds, the isolation —
  spelling out that `workspace` means *your real files* — any restriction it
  sheds, which thread is asking, and the worker's first instruction. It is built
  to the UI permission bar's 240-character budget, fixed facts first and
  agent-supplied text fitted into what is left, so a wordy title or prompt cannot
  push a fact past the elision point. It is deliberately **not** sent under the
  `mcp__cooperation__launch_agent` tool name: the UI digests that name by
  printing the call's `backend`/`model`/`title` arguments, and the escalation
  prompt used to render as the two words *"same engine"* — an approval dialog the
  human cannot understand is not a control. `TestEscalationPromptRendersTheFacts`
  pins the rendered text against a port of the UI's own summariser.
  A refusal means the worker is **not launched** (`NOT APPLIED`), and approvals
  are deliberately **not cached** — every escalating launch is its own act of
  authority.
- The ensemble roster hints and the `launch_agent` tool description no longer
  advertise the never-ask modes as the way to get autonomous work
  (`core/internal/modes/builtin.go`, `core/cmd/akcore/modes.go`).

Fields that cross the same boundary and are deliberately **not** gated, with the
reasoning recorded in `authority.go`: `system_prompt` and `agents` (persona text;
a subagent runs under the worker's own permission mode and its tool list can only
narrow the session's), `backend` (cannot exceed what the human installed; its
real risk is a different mode vocabulary, which the ordering handles), `model`
and `effort` (spend, not authority). `WorkspacePath` and `Env` are not on the
agent-facing surface at all — a worker always roots in its parent's project, and
`Env` is applied *after* the provider credential scrub, so no agent-facing path
may set it.

For a harness whose mode vocabulary is *discovered* at session handshake (kimi),
a launch that requests **no** mode is ranked against **the CLI's own default** —
the `mode` option's `currentValue`, which option discovery already reads and
caches (`harness.DiscoveredOption.Current`). An earlier revision substituted the
launcher's mode and recorded that as a known approximation; it under-ranked a
real escalation (a kimi thread pinned to `plan` seeding an unspecified worker
that comes up in `default`), and the value it called unreadable was already being
read, so it is used. A probe that fails reports nothing and the rank falls back to
the most permissive baseline any engine applies — an unknown baseline costs a
prompt, never a silent grant.

There is exactly **one** permissiveness ordering, `permissivenessRanks` in
`authority.go`. `Capabilities().PermissionModes` is picker order, not
permissiveness order, and the never-ask hint in the ensemble roster is chosen by
rank (and only when the top-ranked mode really is one that never asks).

### Thread creation and permission answers are UI-only

The handlers that take caller-supplied authority had no role check until the same
audit (F5, F6). All now require the UI role:

- `agent.start` — arbitrary `workspacePath`, `coworkEnabled`, a provider override
  **and** an `Env` overlay applied *after* the provider credential scrub, so it
  could rewrite `ANTHROPIC_BASE_URL` and redirect the injected token to an
  attacker endpoint.
- `agent.resume` — replays a dormant thread's persisted authority and accepts a
  provider (with its API token).
- `mode.apply` — the same authority as `agent.start`, reached through a saved
  ensemble.
- `session.attach` — creates a dormant thread rooted at an arbitrary project path
  from a caller-supplied session id, which `agent.resume` then launches. Gating
  `agent.start` while leaving its two-step equivalent open would be no gate.
- `permission.respond` — the human's *answer*. Without the check any connection
  could resolve any open request id and race the human to "Allow" on the primary
  approval flow of both backends.
- `agent.fork` — creates a thread carrying the source's whole authority: its
  permission mode, its `Env` overlay with the source's provider routing, its
  persona and its `coworkEnabled` flag. A caller that could fork could duplicate
  any thread's authority, cowork included, with no human in the chain. Its
  inherited `CoworkEnabled` also routes through `authorizeCoworkAtStart`, the
  gate every thread-creating handler shares.
- `agent.promote` — stops a thread, moves its working tree and session onto a new
  branch and relaunches it.
- `agent.setOption` — the sharpest of them: `option: "permissionMode"` re-arms a
  **live** thread's authority and persists it. Without the check a
  prompt-injected agent could raise its own mode to `bypassPermissions`
  mid-turn — shorter than launching a worker, and invisible to the launch gate.
- `mode.save` / `mode.delete` — an ensemble is a stored recipe for creating
  threads, naming their permission modes and isolation. A caller that could write
  one could plant a `bypassPermissions`/`workspace` crew for the human to launch
  later with one trusting click.

The rest of the handler table was re-read for the same class. What creates or
reconfigures a thread is listed above; the destructive and installer handlers
(`cleanup.archiveAndRemove`, `git.removeWorktree`, `git.discardChanges`,
`skills.create`/`install`, `vsix.install`) are **not** UI-gated today — they do
not create authority, but they do write files every future agent reads, and they
are the next candidates (F4 covers the first of them).

`CoworkEnabled` at thread-creation time routes through one gate
(`authorizeCoworkAtStart`): a UI caller passes, because the flag can only come
from ticking the box in the New Agent / ensemble dialog and re-prompting for the
same click would train the human to dismiss the dialog that matters; any other
caller must go through `askCoworkEnable`, which denies on timeout and denies when
there is no UI to ask.

Notifications were demoted from broadcast to UI-only (F6): `permission.requested`,
which carries the request id **and the raw tool input** (bash command lines, file
contents); `agent.event`, the full live transcript of every thread; and
`cowork.grantRequested`, the desktop consent prompt, which carries a broker
request id and the literal action being asked about — the window, the element,
the text bound for a field. All used to reach every connection, including every
other agent's MCP bridge. The Cowork state notices driven by the same dialog
(`cowork.killSwitch`, `cowork.grantsChanged`, `cowork.policyChanged`) went with
it. The UI rebuilds a dormant thread through `agent.transcript`, so nothing is
lost by the feeds being UI-only.

What still broadcasts, and why it may: `agent.tagsChanged`, `agent.discarded`,
`git.invalidated`, `git.log.invalidated`, `shutdown.progress` and
`vsix.installProgress` carry thread ids, an extension id or a progress counter —
no request id to race and no content to leak. `agent.reviewRequested` carries a
cooperation-board summary, and the board is shared between agents by design
(`read_notes` serves the same text to any of them).

### Persona text: the system prompt is off the command line, subagents are not

A thread's persona — plan 16's `system_prompt` and custom subagent profiles —
reaches the `claude` CLI at spawn. Where it travels matters because
`/proc/<pid>/cmdline` is world-readable on Linux: argv is visible to **every
local user**, not just processes at your uid, for the life of the process.

- **System prompt: private.** When the installed CLI advertises
  `--append-system-prompt-file` (claude 2.1.220 does, in its help's
  `--append-system-prompt[-file]` shorthand), the text is staged in a `0600`
  temp file and only the *path* goes in argv
  (`writePersonaFile` / `buildStartArgs`, `core/internal/agent/agent.go`); the
  file is unlinked when the thread is reaped. Staging that fails is a **failed
  launch**, not a silent fallback to argv. An older CLI without the flag still
  gets `--append-system-prompt` inline — dropping the persona instead would hand
  the human a different agent than the one they configured.
- **Custom subagents: still in argv.** `--agents` takes a JSON string and
  claude 2.1.220 has **no file form** (`--agents-file` is rejected as an unknown
  option — probed live). Subagent names, descriptions and prompts are therefore
  readable by any local user while the thread runs. Do not put anything
  confidential in a subagent profile.

Both are persisted in `threads.json` so a resume can replay them; that file is
now `0600` inside a `0700` data directory (§3), so persistence is owner-only.
A persona is instructions to an agent, not a place for secrets — API credentials
are deliberately handled the other way (env, never argv, never persisted — §3).

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
- **`IdentifyBridge`** tags a connection as an agent bridge for a thread. It is
  the **only** way that role is acquired, it is reached from exactly one handler
  (`bridge.identify`), and it rejects a UI connection trying to invoke an agent
  capability and a bridge trying to switch to a *different* thread
  (`core/internal/ipc/server.go`, `IdentifyBridge`). This is what stops
  cross-thread grant theft: thread A's bridge cannot claim `threadId:B` to spend
  B's grant.
- **`bridge.identify`** is where that binding is *earned*. Every `akcore mcp`
  bridge (Cooperation and Cowork alike) calls it once at startup and must present
  the **per-launch secret** akcore minted for that thread and passed to the
  bridge in its environment (`core/cmd/akcore/bridgeauth.go`,
  `core/cmd/akcore/mcpactivity.go`). A bridge whose identify is refused **exits**
  rather than serving a catalogue it cannot use (`core/cmd/akcore/mcp.go`).
- **`RequireBridge`** is what every *other* handler calls: it asserts an identity
  that already exists and never creates one. Before this, `BindBridge` bound on
  first use from any of five call sites, so `bridge.identify` was decorative — a
  connection could skip it and take a thread's identity through
  `agent.launchWorker`, `cowork.requestEnable`, `cowork.threadState`,
  `cowork.toolsListed` or any desktop call. Regression tests:
  `TestBridgeIdentityNeedsItsSecret`, `TestLaunchWorkerBindsTheCallerToItsParent`.
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
`RequireBridge` and additionally requires the bound thread has opted into Cowork
(`core/cmd/akcore/cowork.go`); `requireUIOrOwnBridge` is the same assertion with
a UI escape hatch (`core/cmd/akcore/cowork_enable.go`); `requireUI` calls
`RequireUI` (`core/cmd/akcore/cowork.go`).

### What the keystone STOPS

1. **Prompt-injection via a co-resident agent's bridge.** A poisoned repo steering
   agent A cannot make A's bridge grant itself a capability — grants come only from
   the UI (`core/internal/ipc/server.go:495-500`), and an agent bridge connection is
   refused the UI role (`core/internal/ipc/server.go:481-482`).
2. **A bridge spending another thread's grant.** A bridge is bound to one thread for
   its lifetime and rejected if it presents a mismatched thread id
   (`core/internal/ipc/server.go`, `IdentifyBridge`/`RequireBridge`).
3. **A connection *taking* a thread's identity by naming it.** The bridge role now
   costs the per-launch secret akcore gave that thread's bridge; a connection
   that cannot produce it holds no role at all, and a roleless connection can
   reach no agent-facing handler (see the F13 section below).

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

#### KNOWN GAP: the socket's roles are self-asserted (audit F13)

This is recorded here as an **open gap with a named fix**, not as an accepted
cost, because F1 showed how short the distance is between "in-band guardrail" and
"full bypass": one `Bash` command reads the socket path off the agent's own bridge
argv (`akcore mcp --socket …`), and a forged `handshake` on that socket answers
its own permission prompts, disarms the kill-switch and enables Cowork. Every
gate in §§1–2 rests on a role nobody verifies.

*Mitigation shipped 2026-08-01 (partial):* `MarkUI` now refuses a **second**
connection the UI role while another live connection holds it, and the refusal is
returned as an error from `handshake` rather than a silent downgrade
(`core/internal/ipc/server.go`, `MarkUI`; regression test
`TestSecondUIRoleClaimRefused`). The refusal is **one atomic decision**: the scan
for an existing UI, the bridge-role check and the role assignment all happen
under `s.mu` (then `c.idMu`, the same order `NotifyUI` takes). The first
implementation scanned under `s.mu`, released it, and only then wrote the role
under `c.idMu` — two connections handshaking at once could each scan while the
other was still roleless and both come away holding the role, which is exactly
what the refusal exists to prevent; the comment claimed an atomicity the code did
not have. This narrows the window to "a forger that connects *before* the real
UI"; it does not close it, and it authenticates nobody. A disconnect frees the
slot, so the real UI reconnecting always wins it back.

*Mitigation shipped 2026-08-01 (the bridge half, and it is no longer partial):*
the **bridge** role is no longer self-asserted. akcore mints a fresh secret
**per bridge process** for every launch (`core/cmd/akcore/bridgeauth.go`), hands
it to that bridge in its **environment** — `AGENTKATE_BRIDGE_SECRET`, wired into
the claude `--mcp-config` server entries and the kimi ACP `mcpServers` list
(`core/cmd/akcore/agents.go`) — and `bridge.identify` demands it back before
`IdentifyBridge` will bind anything. The other five call sites that used to bind
on first use now call `RequireBridge`, which asserts and never creates, so the
secret cannot be skipped by entering through another handler. A bridge whose
identify fails exits instead of serving. Secrets are dropped when a thread is
discarded, and one superseded launch's stay valid across a relaunch so a
winding-down run's bridges are not orphaned. Tests:
`TestBridgeIdentityNeedsItsSecret`, `TestBridgeSecretRotation`,
`TestLaunchWorkerBindsTheCallerToItsParent`.

*And the secret is SPENT while its bridge is connected* (2026-08-01, the second
round). Knowing a secret is not the hard part for a same-uid attacker — a live
bridge's `/proc/<pid>/environ` is owner-readable, and claude's `--mcp-config`
file has to hold the value on disk for the CLI to pass it on — so the gate
cannot rest on the value staying unread. `bridge.identify` now *redeems* rather
than *verifies*: the first connection to present a secret claims it, and every
other connection presenting the same secret is refused for as long as that
holder is connected (`bridgeSecrets.redeem`). A holder that disconnects frees
its slot immediately, so a bridge whose engine respawns it is never locked out,
and a redemption whose binding then fails is released rather than parked. This
is why each launch mints **one secret per bridge** (Cooperation and Cowork):
with a shared secret the second bridge to spawn would be indistinguishable from
a replay of the first. Tests: `TestBridgeSecretIsSpentWhileItsBridgeLives`,
`TestBridgeSecretReplayRefusedOverTheSocket`.

*Where the secret is allowed to live.* Environment, never argv
(`/proc/<pid>/cmdline` is world-readable). For claude it also passes through the
per-launch `--mcp-config` file, which the earlier text wrongly described as
"only in memory": that file is real, and it now lives 0600 inside a 0700
`$XDG_RUNTIME_DIR/agentkate/mcp` (`mcpConfigDir`, which fails the launch rather
than write into a directory it does not own) instead of a world-listable
`/tmp/agentkate-mcp-<threadId>-*.json` that advertised the live roster to every
user on the box. The supervisor deletes it when the thread's process exits
(`core/internal/agent/agent.go`, `mcpConfig` cleanup). Against a *same-uid*
reader it is no better than the environment — which is the point of the replay
gate above. Test: `TestMCPConfigCarriesSecretsInEnvOnly`.

*What the replay gate does NOT close:* an attacker that wins the race to a
secret **before** its bridge redeems it (an MCP server the engine has not
spawned yet) takes the slot, and the real bridge is then locked out. That is a
loud failure — the bridge exits, the core logs the refusal, and the thread's
tools are visibly missing — not a silent theft, and it is the fail-closed
direction. Same-uid isolation still does not exist (§0).

*Verification status:* claude's `--mcp-config` `env` block is the documented
stdio-server field (`claude mcp add -e KEY=value` writes it) and the whole path
is covered by the Go suite. The **kimi** side rides the ACP `mcpServers[].env`
array (`EnvVariable[]` in the ACP schema); a probe against kimi 0.30 could not be
made to spawn an ACP-provided stdio server at all — with or without an `env`
block, so the addition is not what stops it — which means kimi's forwarding is
**expected but not yet observed live**. If kimi ever drops the environment, the
failure is loud, not silent: the bridge exits and the core logs `refused a bridge
identity … no secret was presented — if this is a real bridge, its engine did not
forward the environment it was launched with`. Worth one live kimi thread before
this is relied on.

What that closes: the in-band path the finding is about. A prompt-injected agent
with a shell could previously read the socket path out of its own bridge's argv,
open a second connection, name **any** thread, and act as it — the arena's most
privileged thread included. Naming a thread is now worth nothing.

What it does **not** close: same-uid process isolation, which does not exist
(§0). `/proc/<pid>/environ` is owner-readable, so a determined same-uid attacker
can still lift another bridge's secret out of its process environment — but that
is a targeted read of a specific live process, not a one-line forgery; env was
chosen over argv precisely because `/proc/<pid>/cmdline` is world-readable while
`environ` is not; and since the redemption round above, a secret lifted from a
bridge that is *running* is refused when replayed, which is the case that read
gets you. The **UI** role is still self-asserted (below).

*Why `SO_PEERCRED` does not fix this:* peer credentials give uid/gid/pid, and the
adversary runs at **the user's own uid** — the check passes for the attacker.
Binding the role to a peer whose `/proc/<pid>/exe` is the installed UI binary was
considered and rejected for this round: an agent with a shell can copy or
re-exec any binary it can read, the core has no reliable way to learn the UI's
installed path (it is spawned *by* the UI, and outlives UI restarts), and a
false negative silently strips the real UI of all authority.

*The fix that closes the rest (the doc's own Option B):* its bridge half has now
shipped — per-launch bridge tokens passed by environment and required in
`bridge.identify`, above. The **UI** half has not: it needs a per-run UI token
minted by the core and passed to the UI on a protected channel (not argv, which
is world-readable — see §1), required in `handshake`. Until that ships, a
same-uid process that connects before the real UI is the UI, and that — not the
bridge role — is what remains of F13.

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

### Agent Kate's own stores are owner-only

Everything Agent Kate keeps under `$XDG_DATA_HOME/agentkate` is created `0700`
(directories) / `0600` (files) through one helper, `core/internal/fsperm`:
`threads.json` and its archive, compaction summaries, attachment sidecars, the
Cowork grant/policy/audit files. This is the discipline both harness CLIs
already apply to their own homes (`~/.claude/projects` and `~/.kimi-code` are
`drwx------`), and the data class is the same — thread records carry the
persona, the per-thread env overlay and a title cut from the opening prompt;
summaries are condensed conversations; sidecars name every file the human
attached.

Opening a store also **migrates** it: builds before this one created `0755`
directories of `0644` files, so `NewStore` chmods anything looser than intended
and logs once. New-file modes alone would have left every existing installation
exposed. The data directory itself is tightened when the thread store opens,
which puts every sibling store behind an unlistable directory. A store that
cannot confirm it is private fails the open rather than proceeding.

Note this protects against *other local users*, not against same-uid processes
(§0), which can read these files by definition.

### Credential-shaped env overlays are not persisted

A per-thread environment overlay is persisted so a resume can point the CLI at
the same state directory it was launched with. Values whose **key** matches
`(?i)KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL` are replaced on the way to disk with
the marker `__agentkate_not_stored__` (`core/internal/session/env.go`): the key
survives so the UI can show that the variable is set but its value was not kept,
and the value is re-resolved at resume from the environment `akcore` itself runs
in — the same mechanism the provider token already uses (`ProviderEnvVar` is
stored, the token never is). A marker that cannot be resolved is dropped before
the child is spawned rather than passed through as a literal value.

The match is deliberately broad (it fires on `MONKEY_BUSINESS` too): a redacted
non-secret costs one re-resolution from the environment, an un-redacted secret
costs a cleartext credential in a file that is never deleted — `threads.json` is
rewritten, but the archive is forever.

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

### The pointer mirror is evidence, so it may never go stale

The geometric guard for a *bare* click/scroll (one that fires wherever the cursor
already is) has only one piece of evidence: the core's per-thread mirror of the last
commanded pointer position (`pointerState`, `core/cmd/akcore/cowork_pointer.go`). A
mirror that says "safe spot" while the real cursor sits on an Allow button is worse
than no mirror, so **every** path that moves the pointer either commits an exact
position or destroys the mirror:

- absolute move/click/drag/positioned scroll → `setLast` (exact), only after the
  portal op succeeded;
- relative motion (`move_rel`, the mouse-look input) → `applyRelative`, which
  accumulates the delta while the result provably stays inside the desktop rectangle
  KWin reports (`kde.Client.DesktopBounds`), and otherwise **destroys** the mirror.
  Unknown desktop bounds, a walk into the compositor's edge clamp, or a portal error
  mid-stream all take the destroy branch, and every bare button/scroll then refuses
  with a message naming the cause.

Relative motion used to leave the mirror untouched ("a grab makes the true position
unknowable"). That was a bypass, not a caveat: an agent could park the cursor
somewhere harmless with the absolute tool, walk the real cursor onto an Agent Kate
window with `move_rel`, and fire a bare click the guard cleared against a position the
cursor had left. The timeline compiler applies the same rule per event, so a bare
`button` inside a script is guarded at the *accumulated* point (audit F3, pointer half).

### Keyboard injection — the target is verified for the whole span, not once

Keystrokes follow **focus**, not coordinates, so the geometric guard does not cover
them. A script may also span up to 30 s (`injectMaxSpanMs` / `timelineMaxSpanMs`)
while the portal keeps injecting, and `cw.Authorize` may block for minutes waiting
on a human. Four layers, all fail-closed, cover that span:

1. **Before consent** — `resolveInjectTarget` (`core/cmd/akcore/cowork.go`) resolves
   "the focused window" for real and refuses when it is ours, when the window list
   cannot be read, when no active window can be identified, when the window carries
   **no identity evidence at all** (neither an owning pid nor a resource class —
   `IsSelfWindow` returning false there means "no evidence", not "verified other"),
   and when it has no window id to re-verify focus against later.
2. **After consent** — `focusVerifiedInjectTarget` re-asserts focus on the approved
   window and reads it back. A failed `ActivateWindow`, or an active window that is
   not the approved one, **refuses the batch**; it is not a warning.
3. **During playback** — for a timed typing batch, `startInjectFocusWatch` subscribes
   to KWin's window-activation signal (`core/internal/kde/watch.go`) for the whole
   span and cancels the remaining ops on any activation that is not the granted
   window. Failing to establish the watch refuses the batch.
4. **In the UI, with no compositor involved** — `CoworkPortal` hooks
   `QGuiApplication::focusWindowChanged` and aborts a playback in flight the instant
   any Agent Kate window takes focus, releasing anything still held down.

Layer 4 is what makes the R2 typed-phrase consent dialog un-typeable by the very
playback that raised it; the dialog itself additionally never lets Return commit
(`ui/src/cowork/ControlConsentDialog.cpp`).

### The desktop-wide accessibility flip is consented and reverted

Cowork switches the session's `org.a11y.Status` (`IsEnabled`,
`ScreenReaderEnabled`) on so applications export their AT-SPI tree. That is a real
**global** change — every app becomes readable by any process in the session — so:

- it happens only **after** the portal's remote-control grant lands
  (`becomeReady` in `ui/src/cowork/CoworkPortal.cpp`), never before the dialog is
  answered, so declining leaves the desktop untouched;
- **every** path that can cause it discloses it in the prompt that authorises that
  path: the agent-asks enable dialog and the per-action control prompt
  (`ui/src/cowork/CoworkPanel.cpp`, `ui/src/cowork/ControlConsentDialog.cpp`), plus
  the three panel buttons that reach it — Enable, "Grant desktop access now", and the
  Chromium browser launcher — via `CoworkPanel::confirmDesktopAccessibilityFlip`.
  That confirmation is skipped only when the flip is **already** in effect (nothing
  new happens, and the human was told when it was turned on). Declining any of them
  flips nothing, so there is nothing to restore;
- the agent-facing browser launch (`CoworkPortal::handleLaunchBrowser`) may only
  **re-assert** a flip the human already consented to — it is never the thing that
  first switches the desktop into accessibility mode;
- **turning desktop access off restores it**: when the last thread's `CoworkEnabled`
  goes false the core sends `cowork.restoreDesktopFlags` and the UI calls
  `restoreAtspiStatus()` (`noCoworkThreadsLeft` in `core/cmd/akcore/cowork_enable.go`).
  Before that, only the kill-switch and app exit did, which made the consent text's
  own promise false for the ordinary way people stop (audit F8). It fails **safe**:
  an unreadable store leaves the flags alone rather than breaking a live agent;
- the **kill-switch restores it**: `Authority.Kill` sends
  `restoreDesktopFlags:true` and the UI answers by calling `restoreAtspiStatus()`,
  so "stop ALL desktop access" is literal;
- the pre-flip values are parked on disk before the flip, so a crash is recovered
  on the next run.

### The opt-in switch — an always-wired bridge that grants nothing

The Cowork MCP bridge is spawned for **every** thread, not only opted-in ones
(`core/cmd/akcore/agents.go` `writeMCPConfig` / `coworkMCPServer`), because
neither CLI can be handed a new MCP server once it is running — a bridge that
only exists at launch could never be switched on afterwards. Presence is not
access:

- A bridge whose thread has not opted in advertises an **empty** tool catalogue
  (`core/cmd/akcore/mcp_cowork.go` `advertisedTools`), so the agent cannot see a
  desktop tool, let alone call one.
- That is cosmetic, not the gate. The real gate is unchanged and server-side:
  every desktop RPC still runs `requireCoworkBridge`, which refuses a thread
  whose record says Cowork is off (`core/cmd/akcore/cowork.go`). The
  `--allowedTools mcp__cowork` entry now present on every claude thread grants
  nothing for the same reason — the allow-list was never the authority.
- Switching the opt-in on is **UI-only** (`cowork.setEnabled` behind
  `requireUI`), with the same self-asserted-identity caveat as §2.
- An agent may **ask** — the `enable_cowork` MCP tool, and `launch_agent`'s
  `cowork` flag — but never grant. Both block on an explicit human prompt
  carrying the agent's stated reason (`core/cmd/akcore/cowork_enable.go`
  `askCoworkEnable`), and a target outside the caller's own worker subtree needs
  the usual second orchestration approval on top.
- Enabling raises the OS permission dialog immediately (the preflight), which is
  a usability property, not a security one: the desktop's own portal grant is
  still what authorises screen capture and input injection, and it is taken in
  front of the human rather than mid-task.

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

Because a standing toggle is what removes the per-action prompt, the desktop-access
dialog reads the live policy at dialog time and **names the toggles that are on**
instead of promising that every action still asks (`CoworkPanel::handleEnableRequested`).
An unreadable policy says so rather than implying there are none — an approval the
human cannot understand is not a control. `input_inject` being pre-authorized is
specifically the precondition of the injection attack the four layers above close.

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
| Store permissions `0700`/`0600` + open-time migration (`core/internal/fsperm`) | Owner-only transcript/session state | Other local users reading threads, summaries, sidecars, env overlays | Any same-uid process |
| Env-overlay redaction (`core/internal/session/env.go`) | Credential-shaped values never written to `threads.json` or the archive | A pasted API key persisting in cleartext forever | A credential under a key that looks like nothing (e.g. `MY_THING`) |
| Persona staged in a `0600` file (`core/internal/agent/agent.go`, `writePersonaFile`) | System prompt out of `/proc/<pid>/cmdline` | Every local user reading the persona via `ps` | `--agents` subagent profiles — no file form exists (§1) |
| Audit hash chain + fail-closed (`core/internal/cowork/audit.go:32-48`, `core/internal/cowork/consent.go:152-156`) | Tamper **detection** | Naive truncation/in-place edits | A same-uid full-file rewrite-and-rechain |
| Kill-switch (`core/internal/cowork/consent.go:292-317`) | User panic button (UI-only) | Standing grants/sessions after the user reacts | A same-uid process that forged the UI role |
| KWin VD sandbox (`docs/plans/08-kde-cowork/05-sandbox.md:182-209`) | Organizational grouping | Mis-targeted AK actions | Filesystem/clipboard/input/network/privilege — not isolation |

---

## 8. Future hardening (not yet shipped)

True tamper-*prevention* requires privilege separation — running the consent/audit
authority (or all of `akcore`) as a separate uid the agent's tools cannot write to.
This is Option B in the review and is scheduled as v2/v3 hardening, not present
today (`docs/plans/08-kde-cowork/08-review-findings.md:131-142`).
