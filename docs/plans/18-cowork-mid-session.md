# 18 — Cowork mid-session: enable it while the agent runs, and ask the desktop first

**Status: LANDED** (core + UI, both engines, three live smoke tests).

## The report

> I had an agent that was forked and then enabled to have cowork that wasn't able
> to use cowork because it never prompted me for input permission.

Two independent faults, and the second one hid the first.

**1. "Enabled" did not reach the running agent.** `cowork.setEnabled` only flipped
`session.Record.CoworkEnabled`. Everything that makes the desktop tools *exist*
was decided at launch: claude got its MCP servers from a `--mcp-config` file
written once (`writeMCPConfig`) plus `--allowedTools` built once
(`buildStartArgs`), and kimi got its `mcpServers` list once, at `session/new`.
A thread launched (or forked) without Cowork therefore had no `cowork` MCP server
in its process at all, and never would. The UI even admitted it — *"Restart or
resume it to load the desktop tools"* — but the agent had no way to know, so it
had no desktop tools to call and nothing ever asked the user for anything.

**2. The OS permission was only ever requested lazily.** The XDG RemoteDesktop +
ScreenCast session is created on the *first inject* (`startRemoteDesktop`), which
means the system dialog appears in the middle of an agent's turn — if it appears
at all. KDE refuses to persist a remote-desktop grant (`persist_mode` errors out;
see the comment in `CoworkPortal.cpp`), so this is a per-run grant that nothing
was asking for up front.

Also true, and worth fixing while here: kimi had `Cowork: false` — desktop access
was a claude-only feature — and no agent could ask for desktop access at all.

## What was probed (the design rests on these, not on assumptions)

Both against the installed CLIs, with a throwaway stdio MCP server that reveals a
second tool part-way through a session:

| Probe | Result |
|---|---|
| claude 2.1.220 honours MCP `notifications/tools/list_changed` | **Yes.** It re-listed on the notification and the newly revealed tool was both listed and *callable* in the same session. `--allowedTools mcp__probe` (a server prefix) covered the tool that appeared later. |
| kimi 0.30.0 honours it | **No.** The notification was delivered; no re-list followed, and the tool stayed invisible for the rest of the session. |
| kimi `session/resume` accepts a NEW `mcpServers` list and keeps context | **Yes.** A fresh process resumed the same session, recalled a codeword set before the restart, and immediately had the newly listed tool. |

That split is exactly what the `LiveToolReveal` capability now encodes.

## The design

**Wire the Cowork bridge into every thread; let it advertise nothing.** Since no
CLI can gain an MCP server mid-session, the server must always be there.
`writeMCPConfig` always emits the `cowork` server (and kimi's `Launch` always
passes `coworkMCPServer`), but `advertisedTools` returns an empty catalogue until
the thread has opted in, asked per `tools/list` via a new `cowork.threadState`
RPC. Presence grants nothing: `requireCoworkBridge` still refuses every desktop
RPC from a thread that has not opted in, which was always the real gate.

**Push the change down to the bridge.** New `ipc.Server.NotifyBridge` (core →
the bridge connections bound to one thread) and `ipc.Client.OnNotify` (the
bridge's side, previously dropping every server notification). On
`cowork.enabledChanged`, the Cowork bridge emits MCP
`notifications/tools/list_changed` and claude re-lists.

**Wait for the re-list, do not assume it.** The first live run exposed a race:
enabling and immediately sending a message started the next turn *before* claude
had re-listed, so that turn still saw no tools (they appeared a turn later). The
bridge now sends `cowork.toolsListed` after flushing each `tools/list` reply, and
`setCoworkEnabled` blocks (≤3s) for that ack. "Enabled" now means "usable on the
very next turn", and the reply says so (`revealed: true`).

**Re-attach where tools cannot be revealed.** For a harness with
`LiveToolReveal: false` (kimi), enabling a *running* thread waits for its current
turn to finish (`TurnTracker.Wait` — never throw away a turn in progress), stops
the process, waits for the reap, and resumes the same session with the bridge now
advertising. Context survives; the reply reports `applied: "reattach"` so every
surface can say what actually happened instead of guessing.

**Ask the desktop at enable time.** New portal request kind `preflight`: it turns
the accessibility bus on and stands up the RemoteDesktop + ScreenCast session
immediately, so the human answers one dialog while they are looking at the
screen, and every later action reuses the approved session. It captures nothing —
only the permission is taken. Fired automatically by `setCoworkEnabled`
(fire-and-forget, result broadcast as `cowork.preflightResult`) and manually by a
new **"Grant desktop access now"** button, which also covers re-acquiring the
grant after a kill-switch or an app restart.

**Let an agent ask, never grant.** New `enable_cowork` MCP tool (Cooperation
catalogue, so every thread has it) takes a mandatory `reason`, shown to the human
verbatim in a consent dialog. Approval is unconditional — even for a thread the
caller already controls — and a target outside the caller's worker subtree needs
the usual second orchestration approval on top. `launch_agent` gains a `cowork`
flag gated the same way, so spawning a worker is not a way around the prompt you
cannot skip for yourself; a refusal launches the worker without it and reports
`NOT APPLIED`.

## Changes

Core:

- `core/cmd/akcore/cowork_enable.go` (new) — `setCoworkEnabled`, the re-list ack
  (`revealWaiters`), `reattachForCowork`, `coworkPreflight`, `askCoworkEnable`,
  and the `cowork.threadState` / `cowork.setEnabled` / `cowork.preflight` /
  `cowork.requestEnable` / `cowork.toolsListed` handlers.
- `agents.go` — `writeMCPConfig` always carries the `cowork` server;
  `coworkMCPServer` for kimi.
- `internal/agent/agent.go` — `--allowedTools mcp__cooperation,mcp__cowork`
  always; `StartOptions.CoworkEnabled` deleted (the record is the truth now).
- `harness.Capabilities.LiveToolReveal`; kimi `Cowork: true`.
- `mcp.go` / `mcp_cowork.go` — gated `advertisedTools`, `listChanged: true` in
  `initialize`, the `enable_cowork` tool, `cowork.toolsListed`.
- `orchestrate.go` — `launch_agent`'s human-approved `cowork` flag.

UI:

- `CoworkPortal` — `handlePreflight` + preflight waiters resolved by the same
  session hand-shake the inject queue waits on.
- `CoworkPanel` — "Grant desktop access now", the agent-request consent dialog,
  and messaging that distinguishes live / re-attach / next-start (it used to
  always say "restart it").
- `AgentPanel` — the Cowork checkbox is live mid-session (it was frozen once a
  thread existed), reflects the thread's real state on bind, and reports what the
  switch actually did.

## Verification

Three live smoke tests, all passing:

- `scripts/smoke-cowork-enable.py` — claude: 0 cowork tools before, `applied:
  live` + `revealed: true`, 16 tools on the very next turn, no restart.
- `scripts/smoke-cowork-kimi.py` — kimi: `applied: reattach`, thread resumes,
  codeword from before the restart still known, tools present after.
- `scripts/smoke-cowork-request.py` — an agent calls `enable_cowork`, the human
  prompt arrives carrying its reason, approval fires the preflight portal request
  and the tools are live on the next turn.

Not yet exercised live: the portal dialog itself on this desktop (the smoke tests
answer the preflight round-trip with "no desktop session"), and the UI dialogs —
both need a GUI session with a real KDE portal in front of a human.
