# Remote access integration review — 2026-08-03

## Decision

**Do not merge `agentkate/t-dcd39f7825` as a branch.**  It is a valuable
implementation spike, but it predates the Kimi/Codex harness linkage work and
the authority remediation.  Resolving its conflicts mechanically would restore
paths deliberately closed by those changes.

Integrate it as a sequence of source-level transplants onto `kimi-code-backend`.
The HTTP/SSE protocol, the Vue application, the pairing UI, and much of the
test corpus are assets.  The core adapter, event fan-out, permissions, queue,
and native-shell wiring must be reimplemented against the current contracts.

Both starting points are healthy: `go test ./...` passes on the current branch
and on the remote branch; the remote web app also passes all 224 Vitest tests
and builds successfully.

## Why a normal merge is unsafe

The remote branch forked at `3e6b5cd`, before the current branch's Kimi/Codex
integration and the pass-3 authority work.  It changes 124 files (about 31k
added lines) while current changes touch most of its integration points.

The important conflict is semantic, not textual:

| Area | Current rule | Old remote implementation | Integration decision |
| --- | --- | --- | --- |
| Live transcript | `agent.event` is `NotifyUI` only; bridges must never read another thread's raw transcript. | Hooks the general notification path. | Keep a separate, typed, in-process remote event sink. Never widen IPC notification visibility. |
| Tool input / prompts | `permission.requested` is UI-only because it contains raw tool input. | Expands the broker and broadcasts resolution to every IPC client. | Keep the raw UI DTO local. Publish a separate redacted remote DTO; only plan/question render data may cross its allowlist. |
| Per-thread actions | UI calls are identified as UI; bridge calls are authenticated and bound to their own thread before any target is checked. | Calls `handlerDeps` helpers directly, which bypasses that provenance model. | Factor canonical human-surface operations, then call them from UI-gated RPC handlers and a separately authenticated remote principal. |
| IPC/UI role | UI authority is exclusive and a bridge may never claim it. | Relies on old `requireUI` helpers and has no representation of the new action inventory. | A paired device is **not** an IPC UI. It gets an explicit, narrow remote capability set only. |
| Thread state | `TurnTracker` now has capped retained output and reaping rules. | Adds snapshots/callbacks to its older shape. | Add the needed snapshot/change API to the current tracker without weakening F61 reaping or holding its lock while publishing. |
| Durable state | Current stores are being hardened to private filesystem permissions and current Cowork audit logs have bounded retention. | Remote stores use direct filesystem writes and an unbounded audit log. | Reuse the current private-store hardening and audit rotation pattern before device credentials are enabled. |

There are concrete regressions hidden inside a textual merge as well.  The old
versions of `agent.send`, `agent.interrupt`, `agent.transcript`, and
`agent.diff` predate the current caller/UI gates.  Taking them to accommodate
the queue or output-cap edits would reopen F13/F34/F36: a bridge could again
act without its bound identity or read another thread's transcript/diff.  The
integration must port the *operation* into the current gated handlers, never
replace those handlers with their historical versions.

## Target authority model

Remote access is a human surface, but it is not the desktop.  The integration
must make that distinction explicit rather than smuggling it through a missing
IPC context.

```
desktop IPC UI ── requireUIWindow ─┐
                                  ├─ human-surface operations ── harnesses/broker
paired HTTPS device ─ remote auth ─┘                  │
                                                     audit + typed event sink
agent bridge ── bridge secret + caller binding ── only agent-authorised operations
```

The shared operations are deliberately small: send (queue-only remotely),
interrupt, graceful stop, permission response, roster reads, transcript reads,
and read-only worktree/git projections.  They do not accept a caller-supplied
device name.  The authenticated session creates an immutable remote principal
that supplies attribution to audit/event code.  There is no method for agent
creation, settings changes, session browsing, discard, Cowork grants, screen
capture, input injection, or a filesystem-path parameter.

This gives the remote server a real authority boundary instead of treating its
in-process location as permission to bypass one.

## What transfers intact or nearly intact

- `core/internal/remote`'s route allowlist, HTTPS-only session design,
  fragment token exchange, rate limiter, cookie revocation epoch, SSE replay
  mechanics, scoped stream subscriptions, and API tests are the right starting
  material.
- The web app has a strong shape: no CDN, explicit dependency pins, history
  routing, a single `v-html` location, and
  `markdown-it(html:false) -> DOMPurify` before rendering untrusted output.
  Preserve its sanitizer corpus and its no-external-images rule.
- The Remote panel's interface selection, token-redacting pairing dialog, and
  QR logic are useful.  Port them into the current activity-shell and action
  collection rather than restoring old `MainWindow` code.
- The newer remote `NotifyPolicy` is a better replacement candidate for the
  old agent-id-based desktop notifier.  It must be rebased onto the current
  tray/window lifecycle so it neither duplicates tray attention nor retains
  stale alerts.
- The core-owned queue, output caps, permission summaries, and turn snapshots
  remain correct goals.  Their old implementation is not the integration
  source of truth, but their focused tests are valuable specifications.

## Required changes before exposure

1. **Event and permission split.** Add typed internal publication for remote
   consumers.  Do not change `NotifyUI` to `Notify`.  The broker keeps no
   generic raw input: it retains a redacted summary plus only the renderable
   `ExitPlanMode` plan or `AskUserQuestion` question list, both capped and
   typed.  Resolution, timeout, interruption, and exit must all publish the
   same terminal event.

2. **Canonical human actions.** Extract the actual send/interrupt/stop and
   permission-response operations from their RPC closures.  Preserve the
   current caller-binding checks on bridge entrypoints; remote entrypoints use
   the new remote principal instead.  Add an inventory/conformance test proving
   a remote principal cannot reach every UI-only or Cowork operation.

3. **Queue and transcript truth.** Make the send queue core-owned and arrange
   for every accepted human-surface send to be represented consistently in the
   desktop and remote transcripts.  The old branch identifies a real gap:
   `AgentPanel` currently creates its local “You” card from its own RPC reply,
   so a remote send can reach a harness without becoming visible on desktop.
   Do not ship remote send until that echo has one canonical source.

4. **Private durable stores.** Harden the remote device store, audit log,
   certificate, key, lock, and temporary files using the current `fsperm`
   discipline.  Defer file creation until remote access is enabled or a device
   mutation is requested; merely opening the panel must not create credentials.
   Carry the current Cowork audit log's bounded-retention/rotation design into
   the remote audit log.

5. **Harness-neutral projection.** Build roster and resume behavior from
   `harness.Registry` descriptors and linkage DTOs, not `backend` strings or
   pre-linkage record assumptions.  A remote send to a dormant agent must use
   the same resume policy as the desktop, including provider/wallet refusals
   and all harness capabilities.

6. **Current native shell.** Register the panel through the current shell's
   panel/action/tooltip seams, preserve keyboard discoverability, and integrate
   notification policy with tray presence.  Do not replay the branch's older
   `MainWindow`, `AgentPanel`, or `AgentNotifier` edits wholesale.

7. **Build and packaging.** Keep a committed web stub so Go-only work builds
   without Node.  Do not make an ordinary CMake build fetch/install JavaScript
   dependencies by default.  The release packaging flow should build the
   pinned bundle; developer builds can opt in and the Remote panel must
   identify a stub honestly.  Re-audit all package manifests and licences once
   the final asset set is selected.

8. **QR and hardware validation.** Keep the branch's pure C++ QR encoder only
   with independent known-vector/decode conformance tests.  The share URL,
   mobile self-signed-TLS behavior, notification field mapping, and background
   SSE behavior remain physical-device spikes, not verified product claims.

## Integration sequence

1. Land the core-only contract layer: redacted permission records/resolution,
   tracker snapshot/edge notifications, output caps, and canonical human action
   services.  Cover current Claude, Kimi, and Codex harnesses.
2. Land the core-owned send queue plus the cross-surface user-turn echo.  Port
   only tests that exercise the live busy-edge wiring, not fixtures that build
   a disconnected queue.
3. Transplant `internal/remote` as a transport package and adapt it to the
   new principal/backend interface.  Harden storage first; add route, authority,
   replay, revocation, and audit tests.
4. Wire the remote sink into the current relay and lifecycle paths.  Explicitly
   test interruption, exit, timeout, and resolution from desktop and phone.
5. Bring over the web app and embed/build plumbing without changing the
   default non-Node development path.  Retain the sanitizer test corpus.
6. Rebase the desktop notification policy and Remote panel onto the current
   shell, action collection, tray, and chat model.  Then update security and
   architecture documents from the implementation, not the old plan.
7. Run core/UI/web/smoke verification and the four phone-dependent spikes
   before calling pairing or notification mirroring complete.

## Immediate status

No remote-access code has been merged into `kimi-code-backend`.  The current
branch remains clean.  The source worktree was also returned to a clean state
after validation.
