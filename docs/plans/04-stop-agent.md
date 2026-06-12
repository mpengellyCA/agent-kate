# 04 — True Stop / Interrupt Mid-Response

## Goal

Let the user **interrupt an agent while it is responding** — immediately, to save
credits when a prompt was a mistake or misunderstood, and as a safety control if the
agent misbehaves. Today the Stop button only takes effect *after* the current turn
completes.

## Current state — why Stop is deferred

Spawn / IO / lifecycle live in `core/internal/agent/agent.go`:

- The child is `claude --print --output-format stream-json --input-format stream-json`
  (`agent.go` spawn args ~`:168-169`, `exec.Command` `:204`, `cmd.Start()` `:235`).
- stdin is a pipe kept **open across turns**; each user turn is one JSON line
  `{"type":"user","message":{...}}` written by `Send()` (`agent.go:264-288`,
  write at `:284`). It is closed only by `Stop()`.
- stdout is read line-by-line by `pumpStdout` (`agent.go:331-358`), each line relayed
  to the UI; `reap()` waits on `cmd.Wait()` (`agent.go:458-486`).

Stop path, end to end:

- UI Stop button → `onStopClicked` (`AgentPanel.cpp:1407-1414`) → RPC `agent.stop`.
- Core `agent.stop` handler (`core/cmd/akcore/main.go:468-481`): optionally runs a
  hot-compaction, then `sup.Stop(threadID)`.
- `Supervisor.Stop` (`agent.go:292-316`): **closes stdin**, and after a **5-second**
  grace, force-kills the process if still alive.

**The reason it's deferred:** closing stdin only signals EOF. The `claude` child
reads one complete user message, processes that turn **to completion** (tool calls +
assistant output), and only notices EOF when it next goes to read input. There is
**no in-band cancel message** in the stream-json input protocol. So Stop effectively
means "finish this turn, then exit." The 5s backstop `proc.Kill()` rarely fires
because the turn usually finishes first.

Also note: the process is **not** started in its own process group (no
`SysProcAttr.Setpgid`), so a signal would hit only the `claude` parent, not its
child tool processes / MCP subprocesses.

## Proposed design

Provide a **hard interrupt** that terminates the in-flight turn immediately, then
makes the agent cleanly **resumable** so the user can correct course without losing
the session.

### Mechanism: signal the process group, escalate, then resume

1. **Start agents in their own process group.** Set
   `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` at spawn (`agent.go:204`).
   This lets us signal the whole group (claude + its spawned tools) instead of
   orphaning children.
2. **New supervisor method `Interrupt(threadID)`** distinct from `Stop`:
   - `syscall.Kill(-pgid, syscall.SIGINT)` to interrupt the group (SIGINT is the
     gentlest that actually stops generation; claude treats it like Ctrl-C).
   - escalate: if still alive after a short grace (~1–2s), `SIGTERM`, then `SIGKILL`.
     Much shorter than the current 5s because the intent is *immediate*.
   - mark the thread interrupted so `reap()` records this as a user-interrupt, not a
     crash (affects the lifecycle event + status the UI shows).
3. **Make the turn resumable.** The session id is already captured from stream-json
   (`agent.go:340-348`, persisted as `session.Record.SessionID`), and resume already
   works via `--resume <sessionID>` (`agent.go` ~`:190`, RPC `agent.resume`
   `main.go:311-328`). After an interrupt the thread goes **dormant but resumable**;
   the user's next message resumes the session, replaying up to the last *completed*
   turn (the interrupted turn's partial work is discarded — which is the desired
   "undo the mistake" behavior).
4. **Probe for an in-band cancel first.** Before committing to signals, verify
   whether the installed `claude` build accepts a stream-json control message to
   abort the current turn (e.g. a `{"type":"control_request",...}`/interrupt frame).
   The headless stream-json CLI is the documented integration surface
   ([claude-code-compliant-interface]); if a cancel frame exists, prefer it (cleaner
   than signals, keeps the process warm for the next turn with no resume cost). Treat
   signals as the reliable fallback. **Action item: confirm against the current CLI.**

### UI

- Keep the existing **Stop** button semantics as today's graceful "finish + stop"
  OR repurpose it — recommended: while a turn is in flight, the primary button is
  **Stop (interrupt now)** calling the new `agent.interrupt`; when idle it's the
  normal send affordance. `AgentPanel` already tracks `m_idle`
  (`AgentPanel.cpp:124`, set false on send `:1233`, true on `result` event `:1750`),
  so the button can switch label/behavior on that state.
- After interrupt, show the thread as "stopped — resumable"; the next prompt resumes.

## Implementation steps

1. `agent.go`: add `Setpgid` at spawn; capture `pgid` on the thread struct.
2. `agent.go`: add `Interrupt(threadID)` (signal group, escalate, flag user-interrupt);
   leave `Stop` for graceful shutdown/`StopAll`.
3. `main.go`: register `agent.interrupt` RPC → `sup.Interrupt`; emit a lifecycle
   event phase like `"interrupted"` so the UI can distinguish it from `"exited"`.
4. `CoreClient` + `AgentPanel`: add an interrupt call; bind the Stop button to it
   while `!m_idle`; update status rendering for the interrupted/resumable state.
5. Confirm/clean resume: ensure an interrupted thread can be resumed via the existing
   `agent.resume` path and that partial output already streamed to the UI is visibly
   marked as interrupted.
6. **Spike first:** test whether the current `claude` CLI honors an in-band cancel
   frame; if yes, implement that as the primary path and keep signals as fallback.

## Risks / considerations

- **Orphaned children / MCP servers**: the reason for `Setpgid` + group signal — a
  bare `proc.Kill()` on the parent can leave tool subprocesses running. Verify the
  Cooperation MCP connection and any spawned tools die with the group.
- **Partial writes / corrupt state**: killing mid-tool-call could leave a tool's
  side effects half-done (a file write). This is inherent to interruption; document
  it. Worktree isolation limits blast radius to that agent's worktree.
- **Resume cost**: a hard kill loses the warm process; the next turn pays a resume
  (prefix re-cache). Acceptable for an explicit user interrupt. The in-band cancel
  path (if available) avoids this.
- **Don't regress graceful shutdown**: `StopAll()` at core shutdown
  (`agent.go:318`) should stay graceful (it interplays with compaction — see
  [06-compaction-shutdown.md]); only the user-facing button uses the new hard path.
- **Signal portability**: `Setpgid`/`syscall.Kill` are Unix; fine for the Linux/KDE
  target.

## Acceptance

Hit Stop mid-response → generation halts within ~1–2s, no further tokens billed, the
thread shows "stopped (resumable)", and the next prompt continues the session.
