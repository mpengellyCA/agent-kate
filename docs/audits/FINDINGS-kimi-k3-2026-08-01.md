# Findings — kimi-k3, 2026-08-01

## Summary

Audited the whole repo on branch `kimi-code-backend`, with emphasis on the uncommitted post-`2bd555d` delta (claude control channel, kimi parity, KDE shell integration, attachments, jobs panel, Cowork hardening). Method: ten parallel read-only sub-audits (UI rendering, IPC, spawn/containment/MCP gating, secrets, Cowork, Go stability incl. `go vet` + `go test -race ./...` (clean), Qt async lifetime, efficiency, code quality/tests, filesystem hygiene/DoS), each citing `file:line`. The transport/transport-adjacent hardening from the sweep is genuinely good (frame caps, backpressure, panic containment, consent plumbing, KWallet discipline). The three things that matter most: (1) `launch_agent` lets any prompt-injected agent spawn `bypassPermissions`/`yolo` workers with no human approval — this collapses the documented "determined same-uid attacker" into an in-band prompt-injection primitive; (2) AgentKate's own transcript/session stores are world-readable (0644/0755) while both harness CLIs and AgentKate's own cowork package use 0700/0600; (3) the Cowork keyboard-injection path lacks the focused-window self-target guard the pointer path has, so the typed-phrase consent dialog can be self-approved by an agent holding the `input_inject` toggle.

## Findings

### F1. `launch_agent` spawns ungated workers (`bypassPermissions` / `yolo`, `workspace` isolation) with zero human approval
- **Lens:** security
- **Severity:** critical · **Confidence:** confirmed
- **Where:** `core/cmd/akcore/mcp.go:449-514`, `core/cmd/akcore/orchestrate.go:264-350`, `core/internal/agent/agent.go:154`, `core/cmd/akcore/modes.go:27-36`, `core/internal/modes/builtin.go:60`
- **What:** The MCP `launch_agent` tool is auto-allowed for every thread (`allowedTools := "mcp__cooperation,mcp__cowork"`, agent.go:154) and forwards caller-supplied `permission_mode` and `isolation` verbatim into `agent.launchWorker`. The core gates **only** cowork behind a human prompt (`askCoworkEnable`, orchestrate.go:318); there is no approval, validation, or clamp on `PermissionMode` (accepts `bypassPermissions` / kimi `yolo`) or `Isolation` (accepts `worktree.ModeWorkspace` — run in the parent's main checkout, orchestrate.go:301-306). The ensembles feature actively briefs controllers to use the most permissive mode for unattended workers (modes.go:27-36, builtin.go:60).
- **Why it matters:** Precondition: a prompt-injected agent (poisoned repo content) in the default `acceptEdits` mode, where Bash is human-gated. The agent calls `launch_agent(prompt, permission_mode:"bypassPermissions", isolation:"workspace")` — no human is asked anything. The worker has ungated Bash in the user's primary checkout; from there it can read the socket path off its own bridge argv (`akcore mcp --socket …`), forge `handshake` to become "UI" (handlers.go:235 → `MarkUI`, server.go:716-751), answer its own permission prompts, and enable Cowork — full desktop control with no human click anywhere in the chain. This converts the documented "out of scope: determined same-uid process" adversary into a pure prompt-injection capability. Secondary: unbounded agent fan-out (API spend), ungated edits in the main checkout.
- **Evidence:** `core/cmd/akcore/orchestrate.go:264-350` — the handler's only gate is the cowork branch; no permission-mode or isolation check. `core/internal/agent/agent.go:154` — MCP tools bypass CLI-side prompting by design.
- **Fix sketch:** In `agent.launchWorker`, require human approval (existing broker flow) whenever the requested mode is more permissive than the parent thread's mode, and whenever `isolation == "workspace"`; cap concurrent workers per parent; stop advertising `bypassPermissions` in the ensemble roster hint without the same gate.
- **Effort:** small

### F2. AgentKate-owned transcript/session stores are world-readable (0644 in 0755 dirs)
- **Lens:** security
- **Severity:** high · **Confidence:** confirmed (code + live `stat`)
- **Where:** `core/internal/session/session.go:257-264` (threads.json), `:398-405` (archive), `core/internal/kimi/thread.go:435-436` (kimi-events full transcripts), `core/internal/session/attachments.go:132-139`, `core/internal/compact/storage.go:31,61`; also `agents.go:149` + `session.go:90-97` (per-thread `Env` overlay persisted verbatim into `threads.json`)
- **What:** All AgentKate-owned stores are `MkdirAll 0o755` + write `0o644`. Live probe: `-rw-r--r-- threads.json`, `kimi-events/t-….jsonl` (436 KB full transcript), summaries, attachment sidecars — while `~/.claude/projects` and `~/.kimi-code` are `drwx------` and AgentKate's own cowork stores are 0700/0600 (`cowork/grants.go:230-260`). Records include persisted `SystemPrompt`, per-thread `Env` overlays (a user putting `KIMI_API_KEY=…` in the env overlay gets it persisted in cleartext, world-readable, forever — archive copies too), and `Title` = first 70 chars of the opening prompt (`agents.go:709-717`).
- **Why it matters:** On any multi-user host, any local user reads everything the model was shown for kimi threads — files the agent read (`cat .env` outputs), pasted credentials, compaction summaries — plus launch env secrets. Both harness CLIs protect the same data class; AgentKate re-exposes it.
- **Evidence:** `core/internal/kimi/thread.go:435-436`: `os.MkdirAll(s.eventDir, 0o755)` / `os.OpenFile(..., 0o644)`. Live: `-rw-r--r-- ~/.local/share/agentkate/kimi-events/*.jsonl`.
- **Fix sketch:** `0o700` dirs / `0o600` files at the five write sites (mirroring `cowork/audit.go:126-129`); strip `KEY|TOKEN|SECRET|PASSWORD`-matching values from persisted `Env` (re-resolve at resume like the provider token).
- **Effort:** small

### F3. Cowork keyboard injection has no focused-window self-target guard — typed-phrase consent dialog can be self-approved
- **Lens:** security
- **Severity:** high · **Confidence:** confirmed (code path); likely (end-to-end reliability)
- **Where:** `core/cmd/akcore/cowork.go:357-370` vs `cowork_pointer.go:336-353`; `ui/src/cowork/ControlConsentDialog.cpp:16,91,98-99`; `core/cmd/akcore/mcp.go:101-105`
- **What:** Pointer paths have a rigorous geometric self-target guard (`guardPointerTargets`, fail-closed). The keyboard path does not: with no `targetWindowId`, the consent target is just `"the focused window"`, `IsSelfTarget` passes, and keystrokes go to whatever is focused — including AgentKate's own consent dialogs. The R2 consent dialog is a *typing* challenge (`"allow control"` + Enter), the phrase field is focused by default, and the bridge dispatches MCP frames concurrently (`safe.Go("mcp.handle", …)`).
- **Why it matters:** With the `input_inject` policy toggle on (the advertised "type without asking" mode), a prompt-injected agent issues parallel calls: a delayed `desktop_inject_input` typing the phrase + Enter, and any R2 action (e.g. `desktop_click`). The dialog appears focused; the in-flight playback types the phrase and presses Enter. The agent launders an R2 consent through its `input_inject` standing authorization — crossing the per-capability consent boundary with no human involved. The phrase mechanism implicitly assumes the agent cannot type — false exactly when `input_inject` is pre-authorized.
- **Evidence:** `core/cmd/akcore/cowork.go:357`: `target := cowork.Target{Kind: cowork.TargetWindow, WindowID: p.TargetWindowID, Label: "the focused window"}` — class resolution only inside `if p.TargetWindowID != ""`.
- **Fix sketch:** In `injectInput`/`playInput`, when no target window is given, resolve the active window via `ListWindows` and run the same self-class check as the pointer path — fail closed if unresolvable. Defense in depth: never let Enter default to Allow in `ControlConsentDialog`.
- **Effort:** small

### F4. `cleanup.archiveAndRemove` → `os.RemoveAll` on an unvalidated record path
- **Lens:** security, stability
- **Severity:** high · **Confidence:** likely (each step code-verified; end-to-end not executed)
- **Where:** `core/internal/worktree/worktree.go:560-577`, `core/cmd/akcore/handlers.go:2435-2442`, `core/internal/worktree/cleanup.go:193-206`, `ui/src/CleanupDialog.cpp:284-290`
- **What:** When `git worktree remove --force` fails, the fallback is `os.RemoveAll(wt.Path)` with no check that `wt.Path` is a git-registered worktree or under `<repoRoot>/.agentkate/worktrees/`. `AnalyzeCandidate` can classify a record pointing at an arbitrary clean in-repo directory as `safe`, and the Cleanup dialog pre-checks safe rows with no loss-confirmation prompt.
- **Why it matters:** A prompt-injected agent (file tools, same uid) edits `~/.local/share/agentkate/threads.json` (0644, owner-writable) so a record's `Worktree.Path` points at an unrelated project. Next core start registers it; Cleanup shows it "Safe", pre-checked; one click deletes the entire unrelated directory tree. Non-adversarial store corruption hits the same path.
- **Evidence:** `worktree.go:560-577` — `git worktree remove --force` failure → `os.RemoveAll(wt.Path)` with no provenance check.
- **Fix sketch:** Before the fallback, require `wt.Path` in `git -C RepoRoot worktree list --porcelain` output and/or `EvalSymlinks(wt.Path)` under `RepoRoot/.agentkate/worktrees/`; refuse otherwise.
- **Effort:** small

### F5. `agent.start` accepts `CoworkEnabled`, `Env`, `Provider`, `WorkspacePath` with no caller-role check
- **Lens:** security
- **Severity:** medium · **Confidence:** confirmed (missing check); exploitability depends on F1-class socket access
- **Where:** `core/cmd/akcore/handlers.go:340-383` (params at :58-89), `core/internal/agent/agent.go:501`
- **What:** The handler launches threads with caller-supplied `CoworkEnabled`, arbitrary `Env` overlay, provider override, and arbitrary `WorkspacePath` — with no `requireUI`. The agent-facing paths for the same capabilities are human-gated (`enable_cowork` → `askCoworkEnable`). The `Env` overlay is applied **after** provider credential scrubbing (agent.go:501), so it can rewrite `ANTHROPIC_BASE_URL` and redirect the injected provider token to an attacker endpoint.
- **Why it matters:** Today it needs raw socket access — which F1 provides from prompt injection. Once reachable it is a strictly better primitive than any agent-facing tool: cowork-enabled, bypassPermissions, arbitrary-workspace, token-exfiltrating thread with no human prompt.
- **Evidence:** `handlers.go:340-383` — no role check; contrast `cowork.respondGrant` (`cowork.go:134-137`) and `cowork.setEnabled` (`cowork_enable.go:339-351`).
- **Fix sketch:** Add `requireUI(d, ctx)` to `agent.start`, `agent.resume`, `mode.apply`; route `CoworkEnabled:true` through `askCoworkEnable` regardless of caller.
- **Effort:** small

### F6. `permission.respond` not UI-gated; `permission.requested` (id + full tool input) broadcast to all connections; `agent.event` broadcast to all connections
- **Lens:** security
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `core/cmd/akcore/handlers.go:1503-1519`, `handlers.go:2695-2701`, `core/cmd/akcore/run.go:195`, `core/internal/ipc/server.go:445-489`
- **What:** Three related broadcast/gating gaps: (a) `permission.respond` resolves broker requests with no origin check, unlike its Cowork siblings; (b) `askHumanPermission` notifies via `srv.Notify` (all connections — every agent bridge and anonymous client) disclosing the request id **and raw tool input** (bash command lines, possibly secrets), so any same-uid watcher can race the human to Allow; (c) `agent.event` (full live transcripts of every thread) is pushed to every connection, contradicting the feed-isolation design that `NotifyUI` exists for (`server.go:466-468`).
- **Why it matters:** Cross-agent information leak (tool inputs, full transcripts) to every other agent — exactly what `mcpactivity.go:180-182` takes care never to do — plus a self-approval race on the primary tool-approval flow of both backends. Below the project's own keystone tier ("grants come only from the UI").
- **Evidence:** `handlers.go:1503` — `func(_ context.Context, …)`, no check; `handlers.go:2695` — `srv.Notify` not `NotifyUI`; `run.go:195` — `srv.Notify("agent.event", …)`.
- **Fix sketch:** `requireUI` on `permission.respond`; `NotifyUI` for `permission.requested` and `agent.event` (verify UI reconnect re-fetch path; dormant replay uses `agent.transcript`, unaffected).
- **Effort:** small

### F7. `a11y_action` self-target check fails open on `ListWindows` failure; PID evidence ignored
- **Lens:** security
- **Severity:** medium · **Confidence:** confirmed (code)
- **Where:** `core/cmd/akcore/cowork.go:1098-1110`, `core/internal/cowork/consent.go:438-448`, `core/cmd/akcore/run.go:330-333`
- **What:** `elementTargetWindow` learns the target's self-class only via a secondary KWin `ListWindows` lookup; on error (or PID mismatch) `ResourceClass` stays empty and `IsSelfTarget` returns false — fail open, unlike `guardPointerTargets` which refuses. The authority already holds the UI's PID in `selfPIDs`, but `IsSelfTarget` never consults PIDs.
- **Why it matters:** With the `a11y_action` toggle on plus a lookup failure, an agent can `desktop_set_text` the consent-dialog phrase field and `desktop_activate_element` its Allow button — consent self-approval with no keystroke guessing.
- **Fix sketch:** Hard-refuse when `info.PID ∈ selfPIDs`; fail closed for R2 a11y actions when the owning window can't be resolved.
- **Effort:** small

### F8. Silent desktop-wide `org.a11y.Status` flip: undisclosed, forced before consent completes, left on after decline/kill-switch
- **Lens:** security
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `ui/src/cowork/CoworkPortal.cpp:501-502`, `:1149` vs `:1160`, `:597-603`; `ui/src/cowork/BrowserLaunch.cpp:67-70`; `ui/src/cowork/CoworkPanel.cpp:691-697`
- **What:** Enabling Cowork (or preflight, or agent-triggered Chromium launch) force-sets global `IsEnabled`/`ScreenReaderEnabled` on the session bus. (1) No consent text discloses it — the same "permission report not true" class that was just fixed elsewhere. (2) The flip happens *before* the portal dialog is answered; a decline leaves it on until app exit. (3) The kill-switch tears down the portal session but not the a11y flags, despite its "Stop ALL desktop access" label.
- **Why it matters:** The whole desktop is switched into accessibility mode for the app's run: every application exports its AT-SPI tree readable by *any* local process, and assistive technologies may activate — a real global permission change the user is never told about. (The crash/restore machinery itself is good: parked originals, PID-scoped record, next-run recovery.)
- **Fix sketch:** Disclose the flip in the enable dialog/preflight tooltip; restore flags on preflight decline and optionally on kill-switch; consider whether `ScreenReaderEnabled` is needed vs `IsEnabled` alone.
- **Effort:** small

### F9. Claude backend: blocking stdin write while holding `t.mu` wedges every recovery path
- **Lens:** stability
- **Severity:** medium · **Confidence:** likely (trigger condition unverified — whether a hung `claude` stops draining stdin)
- **Where:** `core/internal/agent/agent.go:597-608` (`Send`), `:973-992` (`sendControlResult`), `:724-747` (`Interrupt`)
- **What:** All three write to the child's stdin pipe while holding the thread mutex. Messages routinely exceed the 64 KiB pipe buffer (base64 image attachments, `buildUserContent` agent.go:287-314). If `claude` wedges mid-turn and stops reading stdin, a large Send blocks forever holding `t.mu`; Interrupt, `abortPending`, `Stop`/`closeStdin`, and `pumpStdout` then all serialize behind it — the thread is unkillable from the UI. The kimi backend deliberately uses a dedicated write mutex (`wmu`, `kimi/acp.go:227-236`) with no thread lock held.
- **Fix sketch:** Snapshot state under `t.mu`, release, then write under a dedicated write mutex (kimi pattern); re-check state on write error.
- **Effort:** small

### F10. Unbounded transcript replay (no core→UI frame cap) + transcript stores grow forever
- **Lens:** stability, efficiency
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `core/internal/session/relocate.go:15-39`, `core/internal/kimi/thread.go:343-369`, `core/cmd/akcore/handlers.go:433-467`, `core/internal/ipc/server.go:24`, `ui/src/ipc/CoreClient.cpp:33,405`; no removal path for `kimi-events` (`handlers.go:2487-2490` cleanup skips them)
- **What:** Replay reads the entire transcript into memory and returns it as one RPC result; the 16 MiB frame cap is inbound-only, the UI's 15 MiB cap is outbound-only, and `CoreClient` buffers inbound unboundedly (`m_buf.append`). kimi event logs append on every resume and are never deleted (not on discard, not on cleanup); `cowork-audit.jsonl` and `threads-archive.json` likewise grow forever.
- **Why it matters:** A months-old thread replayed loads hundreds of MB into core and UI in one frame → freeze/OOM; the store grows without bound (disk-fill DoS on the user).
- **Fix sketch:** Paginate/cap `agent.transcript` (the `PreviewTranscript` path already streams); delete kimi event logs in discard/cleanup teardown; rotate or cap audit/archive files.
- **Effort:** medium

### F11. AttachmentBuilder reads whole files before any size check
- **Lens:** security (DoS), efficiency
- **Severity:** medium · **Confidence:** confirmed
- **Where:** `ui/src/AttachmentBuilder.cpp:287` (and `:380` for ranged excerpts)
- **What:** `const QByteArray bytes = file.readAll();` precedes the 5 MB image cap (:302) and 256 KB text truncation (:319). The budget system bounds what is *admitted*, not what is *read*.
- **Why it matters:** Dropping a 10 GB log/ISO onto the input (or an agent-suggested "attach this file") allocates the whole file in the GUI process → memory exhaustion / OOM-kill of the UI.
- **Fix sketch:** `QFileInfo::size()` check before `readAll()`; for text, `file.read(kMaxTextBytes + slack)`.
- **Effort:** small

### F12. Synchronous D-Bus on the GUI thread in BrowserLaunch
- **Lens:** stability
- **Severity:** medium · **Confidence:** confirmed (code); impact unverified
- **Where:** `ui/src/cowork/BrowserLaunch.cpp:61-70`, reached from `CoworkPanel.cpp:964` and `CoworkPortal.cpp:934`
- **What:** Constructs a `QDBusInterface` (synchronous introspection, ~25 s default timeout) and makes two blocking `Set` calls on the GUI thread. `CoworkPortal.cpp:128-130` explicitly documents avoiding this exact pattern ("a wedged a11y bus would stall whoever built it").
- **Why it matters:** A wedged `org.a11y.Bus` freezes the whole GUI for up to ~75 s during an agent-triggered browser launch.
- **Fix sketch:** Reuse CoworkPortal's raw-`QDBusMessage` + `asyncCall` (or 2 s `kA11yCallTimeoutMs`) helpers.
- **Effort:** small

### F13. Unauthenticated socket with self-asserted roles (documented/accepted — recorded for the audit trail)
- **Lens:** security
- **Severity:** high (absolute) · **Confidence:** confirmed — explicitly ratified project posture (`docs/security-model.md:8-14,150-163,350-355`)
- **Where:** `core/cmd/akcore/handlers.go:235` → `core/internal/ipc/server.go:716-751`; socket path discoverable via bridge argv (`mcp.go:44-48`) and predictable default (`run.go:37-42`)
- **What:** No peer-credential check; any same-uid connection calling `handshake` becomes primary UI. The audit record should reflect that the distance between "in-band guardrail" and "full bypass" is one Bash command the gated agents are fully capable of running — and that F1 makes that distance reachable from pure prompt injection. Not a new bug; reported so the posture is re-evaluated against F1.
- **Fix sketch (doc's own Option B):** per-run UI token passed on a protected channel (not argv), required in `handshake`; per-thread bridge tokens via env, required in `bridge.identify`. SO_PEERCRED alone does not help (agents are same-uid).
- **Effort:** medium

### F14. Model-controlled links passed unfiltered to `QDesktopServices::openUrl`
- **Lens:** security
- **Severity:** medium · **Confidence:** confirmed (code); likely (impact)
- **Where:** `ui/src/TranscriptDelegate.cpp:1140-1143`, `ui/src/AgentPanel.cpp:549-554`; contrast the whitelist in `ui/src/RichTextView.cpp:124-129`
- **What:** Markdown links in assistant messages go to `QDesktopServices::openUrl` with any scheme — `file://`, scheme-less absolute paths, `smb://`, `mailto:` with prefilled body, any installed custom handler. Link text is fully decoupled from target. The project already has the right policy in `RichTextView` (http/https/mailto only) but the two transcript click paths don't use it.
- **Why it matters:** Attacker = malicious repo content shaping model output; one user click opens arbitrary local files/URLs in OS handlers. Direct RCE is unlikely via modern `xdg-open`, but scheme-handler abuse and forced opens are real.
- **Fix sketch:** Route both paths through one helper enforcing the `RichTextView` whitelist (open `file://` only after confirmation or in the internal editor).
- **Effort:** small

### F15. `QTextBrowser` instances render images from arbitrary local paths, incl. blocking special files
- **Lens:** security (DoS)
- **Severity:** medium · **Confidence:** likely
- **Where:** `ui/src/SubAgentTranscriptDialog.cpp:224` (feeds `markdownToHtml(model text)`), `ui/src/TranscriptDelegate.cpp:1056` (selection overlay document parented to a `QTextBrowser`), `ui/src/RichTextView.cpp:119,268`
- **What:** Qt6 `QTextBrowser::loadResource` does `QFile(...).readAll()` for any image name resolving to a local file — no scheme allowlist, no size cap, synchronous on the GUI thread. Model markdown `![x](/dev/zero)` → unbounded read → GUI hang/OOM; `![x](file:///home/user/secret.png)` renders a private image into the dialog. (Remote exfiltration via `<img src=http:…>` is *not* possible — Qt6's loader is local-only; verified against Qt source.) The painted transcript rows use parentless `QTextDocument`s (TranscriptDelegate.cpp:338) and are not affected — the overlay bypasses that protection.
- **Fix sketch:** Install a `QTextDocument::ResourceProvider`/loadResource override on these browsers serving only `qrc:` and allowlisted roots, with a byte cap.
- **Effort:** small

### F16. `neutralizeMarkdownRawHtml` fence tracking desyncs from CommonMark — raw HTML reaches `setMarkdown`
- **Lens:** security
- **Severity:** low-medium · **Confidence:** likely (spec rule + code verified; payload not executed against md4c)
- **Where:** `ui/src/MarkdownUtil.cpp:22-54`
- **What:** Any line starting with ≥3 backticks is treated as a fence opener and subsequent lines are copied verbatim. Per CommonMark, an opening backtick fence whose info string contains a backtick is not a fence — md4c parses it as paragraph text and the following line as a raw HTML block, which Qt's markdown importer honors ("handled in the same way as in setHtml"). So ````` ```rust`x` ````` followed by an HTML line escapes neutralization. No script engine in Qt rich text, but arbitrary Qt-HTML subset (colors, tables, images) inside the trust surface where users decide on permission prompts — visual spoofing beyond what markdown allows.
- **Fix sketch:** Prefer `QTextDocument::setMarkdown` with `MarkdownNoHTML` (Qt discards HTML tags) over hand-rolled neutralization; or reject backtick-containing info strings in the fence check. A unit test feeding the payload through `neutralizeMarkdownRawHtml` + `setMarkdown` settles it.
- **Effort:** small

### F17. Handler context never cancelled on client disconnect — parked `agent.wait` goroutines leak per dead bridge
- **Lens:** stability
- **Severity:** low-medium · **Confidence:** confirmed
- **Where:** `core/internal/ipc/server.go:153,233-235` (Serve ctx passed unchanged); intended contract documented at `core/internal/agent/turnwait.go:182-185`
- **What:** A controller agent calling `wait_agent` (up to 1 h, orchestrate.go:29) whose bridge dies leaves the server-side handler goroutine parked in `TurnTracker.Wait` until the deadline — the documented disconnect-release ("a disconnected bridge must release its waiter") isn't wired. Each stop/restart of a waiting controller leaks one goroutine for up to an hour. Self-healing.
- **Fix sketch:** Derive `connCtx, cancel := context.WithCancel(ctx)` per connection in `serveConn`, `defer cancel()`, pass `connCtx` to `dispatch`.
- **Effort:** small

### F18. Expanded tool rows rebuild two `QTextDocument`s per paint; per-paint chip overhead
- **Lens:** efficiency
- **Severity:** medium (visible scroll jank) · **Confidence:** high
- **Where:** `ui/src/TranscriptDelegate.cpp:694-712` (docs deleted at :833-834); chip overhead at `:112-117`, `:450`/`:504`
- **What:** Tool detail/result rows bypass the `m_docCache` that Message/Note/Thinking rows use: per paint — 2 heap `QTextDocument` allocations + 2 full layouts, O(detail+result length) ≈ 1–3 ms/frame at 50 KB, worse after "Show full output" (unbounded). During a scroll past one expanded large row: ~50–100 ms of layout per second of scrolling. Additionally: thumbnail-key string building (~6 QString allocs + a global-mutex `QPixmapCache::find` per image chip per paint) and `layoutAttachmentChips` runs twice per paint plus per click/tooltip.
- **Fix sketch:** Cache detail/result docs keyed by `StableIdRole`, invalidated via the existing `invalidateRow` path (as `bodyDoc` does); compute chip layout once per `layoutRow`; memoize thumbnails in the existing TTL entry. After fix: per paint per expanded row → 1 hash lookup (~µs); chip string allocs per paint → 0.
- **Effort:** small

### F19. `_usage` events emitted by core, silently dropped by UI; kimi cumulative usage summed as per-turn billing
- **Lens:** quality
- **Severity:** medium-low · **Confidence:** high
- **Where:** `core/internal/kimi/translate.go:239-253` (emits `_usage` "announcing it immediately"), `thread.go:1163`; `ui/src/AgentPanel.cpp:5384-6047` (no `_usage` branch); accumulation at `AgentPanel.cpp:5774-5778`, `AiInspectorPanel.cpp:272-278`
- **What:** (a) `renderEvent` handles `_context`/`_commands`/`_options`/`_stderr`/`_lifecycle` but not `_usage` — the event is dead wire traffic every turn and the meter only updates a turn later, contradicting the event's own docstring (the translator struct comment elsewhere admits "trails by one turn" — the code disagrees with itself). (b) kimi's `usage.input_tokens` is a cumulative context snapshot (translate.go:267-283) but the UI accumulates it as per-turn deltas, so a thread holding 4k/8k/12k over three turns reports "24k in" — and a per-turn line that reads as billing that never happened.
- **Fix sketch:** Add a `_usage` branch mirroring `_context` (or stop emitting and fix the comment); guard session-total accumulation on `HarnessTraits.usageReporting` (already false for kimi).
- **Effort:** small

### F20. Predictable `/tmp` fallbacks: socket path (cross-user DoS, Listen→Chmod race) and modes fallback (symlink-following write)
- **Lens:** security
- **Severity:** low-medium · **Confidence:** confirmed (conditional on `XDG_RUNTIME_DIR` unset / modes.json unreadable)
- **Where:** `core/cmd/akcore/run.go:41` + `core/internal/ipc/server.go:123-136`; `run.go:147` + `core/internal/modes/modes.go:154-158`
- **What:** (a) Socket fallback `/tmp/agentkate.sock`: another user can pre-create it (sticky `/tmp` → `os.Remove` EPERM → core refuses to start), and the socket exists with umask perms between `Listen` and `Chmod(0600)`. (b) The modes fallback writes `/tmp/agentkate-modes-fallback.json` via `os.WriteFile`, which follows a pre-planted symlink — another local user can redirect the write onto any victim-writable file. Narrow triggers; normal path (XDG_RUNTIME_DIR 0700, verified) is unaffected.
- **Fix sketch:** `0700` per-user subdirectory for the fallback socket (or umask 077 around `Listen`); `O_EXCL`/`O_NOFOLLOW` create for the modes fallback.
- **Effort:** small

### F21. Model-influenced strings in `QLabel`s rely on `Qt::AutoText` sniffing (ToolInspectorDialog)
- **Lens:** security
- **Severity:** low · **Confidence:** likely
- **Where:** `ui/src/ToolInspectorDialog.cpp:235` (Bash description), `:300-301` (MCP digest incl. arbitrary agent prose), `:323-329`, `:359`
- **What:** Plain `new QLabel(text)` with default `Qt::AutoText` — tag-like content is parsed as rich text (styled spoof content, `data:` image decode) inside the dialog. The rest of the codebase is disciplined (`toHtmlEscaped()` everywhere else); these look like oversights.
- **Fix sketch:** `setTextFormat(Qt::PlainText)` on those labels.
- **Effort:** small

### F22. Stuck application-wide busy cursor when CleanupDialog closes mid-analysis
- **Lens:** stability
- **Severity:** low-medium · **Confidence:** confirmed (by code path; not reproduced live)
- **Where:** `ui/src/CleanupDialog.cpp:489-507`, lifetime guard at `ui/src/ipc/CoreClient.cpp:434-436`; dialog is non-modal `WA_DeleteOnClose` (`WorktreeDashboard.cpp:248-249`)
- **What:** "Advise" sets an app-wide override cursor; if the dialog is closed while the round-trip is in flight, the lifetime guard drops the reply and `restoreOverrideCursor()` never runs — every window shows a busy cursor until restart.
- **Fix sketch:** Restore the cursor in `~CleanupDialog()` behind a `m_cursorHeld` flag, or an RAII guard scoped to the dialog.
- **Effort:** small

### F23. Persona/system-prompt text travels in `claude` argv (world-readable via `/proc/<pid>/cmdline`)
- **Lens:** security
- **Severity:** low · **Confidence:** confirmed
- **Where:** `core/internal/agent/agent.go:188-195` (`--append-system-prompt`, `--agents`)
- **What:** Any local user can read persona prompts (which can embed sensitive instructions/context) via `ps` for the process lifetime. API keys never go here (provider routing is pure env — sound); kimi's argv is clean (`acp` only). Disclosed in security-model.md; recorded for completeness.
- **Fix sketch:** Move persona text into a 0600 temp settings file if the CLI supports one; otherwise a docs note.
- **Effort:** small

### F24. Smaller confirmed items (grouped)
- **Lens:** various · **Severity:** low · **Confidence:** as noted
- **Debug logging of raw child output** (stability/secrets, likely): akcore logs at `slog.LevelDebug` unconditionally (`run.go:77`); undecodable ACP frames logged verbatim (`kimi/acp.go:96-98`); every kimi/claude stderr line at Debug (`kimi/thread.go:1852`, `agent.go:1310`) — lands in the UI Output panel and possibly the persistent systemd journal. Fix: gate Debug behind an env var; truncate the frame log.
- **MCP bridge stdio parser wedges on over-long line** (stability, confirmed): `mcp.go:89-105` `bufio.Scanner` 8 MiB cap — a longer line ends the read loop silently; the bridge stays alive but never answers again. Fix: shared `frameReader` or check `sc.Err()`.
- **`compact.Store.Put` shared `.tmp` path, no mutex** (stability, unverified): `compact/storage.go:51-65` — concurrent same-thread Puts can interleave on one tmp file and publish corrupt JSON. Fix: `sync.Mutex` (AttachmentStore pattern) or per-call unique tmp name.
- **`runHotCompactIfConfigured` queues a turn it never un-queues on error paths** (stability, likely): `agents.go:487-508` — error/empty-body returns skip `TurnFailed`; if the subsequent `agentStop` fails, the thread reads busy in `agent.wait`/Jobs until a real turn ends. Fix: `TurnFailed` on those returns.
- **Kimi JSONL replay: no crash-tail tolerance** (stability, confirmed): `kimi/thread.go:1917-1923` appends without fsync; `ReadTranscript` (:343-369) replays every non-empty line without `json.Valid` — a torn tail is relayed to the UI as an event. Fix: skip+log invalid trailing line.
- **KWin ScreenShot2 capture has no lifetime bound** (stability, confirmed): `CoworkPortal.cpp:688-783` — unlike `PortalResponseWaiter` (`kPortalWaiterLifetimeMs`, :149-155), a wedged KWin leaks read fd + notifier + watcher until exit. Fix: single-shot ~30 s timer forcing `finalize()`.
- **Cross-subtree orchestration grants persist for the whole core run** (security, confirmed): `orchestrate.go:32-58` — one approval authorizes unlimited later sends (days-long stale window). Fix: expire grants (e.g. 15 min) or scope per-use.
- **Enable dialog claims "Every individual action still asks for its own permission" — false with any policy toggle on** (security/quality, confirmed): `CoworkPanel.cpp:693-696` vs persisted `cowork-policy.json` toggles (`consent.go:166-169`). Fix: build the sentence from `cowork.getPolicy` at dialog time.
- **Failure-notification storm uncoalesced** (stability, confirmed code / unverified trigger): `AgentNotifier.cpp:229-233` — one popup per Error transition, no cooldown; a crash-looping agent spams. Fix: per-agent cooldown or failure batching.
- **Raw `CoreClient*` in sibling controllers, safe only via destructor ordering** (stability, latent): `ui/src/git/GutterController.h:67`, `BlameController.h:64` — this crash already happened once (MainWindow.cpp:~185-194 comment); `CoworkPortal` uses `QPointer<CoreClient>` for the same relationship. Fix: `QPointer` in both controllers.
- **`CoreClient::call` invokes the callback synchronously on send-failure** (stability, confirmed behavior, no current breakage): `CoreClient.cpp:259-268` — all callers assume async. Fix: one-line contract comment (or `QTimer::singleShot(0, …)` deferral).
- **`exec.Cmd.Wait` racing pipe drains in both reapers** (stability, unverified): `agent.go:1317`, `kimi/thread.go:1872` — per os/exec docs, tail output (final result event) can be discarded. Fix: `stdoutDrained` channel.
- **Daemon-spawned tools resolved via PATH with `cmd.Dir` in agent-controlled dirs** (security, low/medium confidence): `exec.Command("git"/"gh"/"rg"/"claude"/"kimi", …)` throughout; Go refuses relative-PATH resolution (`ErrDot`) so a repo-local shadow needs a relative PATH entry — fails loudly rather than executing. Fix: pin absolute paths at daemon startup.
- **vsix extraction: symlink-following writes, no decompressed-size cap** (security, unverified): `core/internal/vsix/archive.go:56` — zip-slip check is lexical only; `io.Copy` uncapped (zip bomb). Needs a malicious `.vsix`. Fix: `O_NOFOLLOW`/`Lstat` + `io.LimitReader`.
- **No cross-call rate limiting on input injection** (security, informational): per-call bounds exist (`cowork_inject.go:33-37`) but with a toggle on, back-to-back calls are unlimited.
- **Pre-authorization toggles for unimplemented capabilities (`screencast`, `vd_sandbox`)** (security, informational): `policy.go:152-164` — a forgotten standing toggle silently activates the capability when v3 ships.
- **Kimi threads silently skipped by skill-reload broadcast** (quality, confirmed deliberate): `handlers.go:2736` type-assert skips kimi with no notice; claude panels show "skills reloaded", kimi panels show nothing. Fix: one-line panel note ("relaunch to pick up new skills").
- **Dead/unreachable code left by the sweep** (quality, confirmed): `agents.go:668-672` `ErrCompactedInPlace` check in `runExitCompact` unreachable (gated by `ColdCompact`; only kimi returns it and its `ColdCompact` is false); `harness.go:370-374` interface doc cites a sentinel declared in `package main`, unresolvable from the package; `thread.go:1112-1116` comment promises a "panel that asks" usage-refresh caller that was never built.
- **Claude-side Go relay parses every streamed line 5×** (efficiency, confirmed count, low user impact): `agent.go:1115` + toolmeter + usagemeter + turnwait + relay probe ≈ 3–8 µs each; ~1–6 ms CPU/s during streaming, 60–80% redundant. Fix: classify once, pass types down.
- **Active find: per-paint O(row length) highlight rebuild** (efficiency, low): `TranscriptDelegate.cpp:218-233` — highlighted HTML rebuilt and discarded per paint while find is open. Fix: cache per (stableId, needle).

## Surfaces audited and found sound

- **IPC transport:** 16 MiB frame cap enforced both read directions with oversize-survival drain and conservative id recovery; per-conn in-flight cap (256) with backpressure; bounded outbound queue that never sheds responses; 30 s write deadlines; per-handler panic recovery; every goroutine via `safe.Go`; socket 0600 in 0700 `$XDG_RUNTIME_DIR` (verified live); `go vet ./...` clean; `go test -race ./...` all green. MCP transport is stdio-only — no network listener (only opt-in `AKCORE_PPROF`, off by default).
- **Markdown pipeline core:** `markdownToHtml` always pre-filters through `neutralizeMarkdownRawHtml`; streaming text uses pure escaping; tool results/details painted as plain text; Qt6 `QTextBrowser` has no network resource loading (verified against Qt source) — remote exfiltration via `<img>` is not possible.
- **RichTextView:** scheme whitelist (http/https/mailto), `setOpenLinks(false)`.
- **Attachment ingestion:** per-image 5 MB / total 12 MB budgets, NUL-sniffing, content-addressed atomic cache writes (tmp+rename), prune with age floor and `NoSymLinks`; no traversal in any write path; chip thumbnails use `setScaledSize` + post-decode clamp; Qt6's default 256 MB image allocation limit caps decompression bombs.
- **Secrets — the good half:** KWallet storage with no plaintext fallback (UI refuses key entry without a wallet); provider token never persisted core-side, never logged, never in argv (pure env); `buildEnv` scrubs inherited `ANTHROPIC_*`/`CLAUDE_CODE_*` before third-party injection (tested); `mcp.activity`/`argsSummary` default-deny digest with 120-char cap, tool inputs never echoed, `desktop_set_text` reports only element id ("The text itself may be a password"); cowork stores 0700/0600 with hash-chained audit that denies-all on tamper; both harness CLIs' own stores are 0700 (live probe).
- **Subprocess construction:** no shell interposed anywhere in `core/` — all argv-array `exec.Command`; `--` separators for `rg`/`git blame`; agent-authored text travels as discrete argv elements; worktree/branch names are core-generated ids.
- **Cowork — the good half:** `enable_cowork` human gate is unconditional, per-request, names thread/target/verbatim reason, deny on timeout/UI-absence; mid-session enable re-consents; role keystone one-way both directions; portal requests fail closed with no primary UI; R2-outside-sandbox forced per-action regardless of scope; portal sessions deliberately request no `persist_mode`/`restore_token` (every run re-authorizes via KDE's dialog); generation guards on every async continuation; `releaseHeld` before session close; screenshot pixels stream over in-memory pipe, portal PNG deleted after decode, `maxDim` clamped server-side; a11y crash/restore machinery (parked originals, PID-scoped record, next-run recovery) is solid; browser launch restricted to user-configured browsers; kill-switch revokes grants + clears toggles + tears down sessions, UI-only.
- **Containment honesty:** `docs/security-model.md` is candid ("not a sandbox"); UI "isolated" language refers to git working-copy separation; permissive modes carry honest labels and are not persisted as sticky picks. Outside-cwd writes and Bash route through the human permission broker in default modes of both harnesses. No false containment claims found — the gap is F1 making the documented out-of-scope adversary reachable, not messaging.
- **Qt async discipline:** zero context-less lambda connects / `QTimer::singleShot`s in `ui/src/`; KNotification ownership sound (autoDelete + QPointer + retract on quit); single-instance via D-Bus name (no pid/lock races); `AgentPanel` core notifications explicitly queued to avoid teardown re-entrancy; only worker thread touches no QObject state; all 10 UI test suites pass (0.74 s).
- **Efficiency — the good half:** IPC coalescing (25 ms core-side, 50 ms UI-side) caps frames at ≤40/s per thread; kimi path emits one event per completed message (zero per-token frames); delegate per-(row,width) height cache + doc cache; incremental model updates only (no resets per token); feed capped at 5000 rows; startup does no sync network/blocking D-Bus.
- **Test quality (delta):** claude fixtures are captured real wire shapes with "do not simplify" notes; new tests assert behavior that fails under inversion (refusals, counts, exact strings); no engine if-ladders introduced in the UI — new affordances gate on `HarnessTraits`, defaults match the Go adapters flag-for-flag; `docs/plans/README.md` "Landed 2026-08-01" claims verified claim-by-claim; plan 19 P4 claims check out.
- **Cleanup flow (other than F4):** server re-derives verdicts, blocked rows uncheckable, archive-before-delete ordering, `KMessageBox::Dangerous` loss confirmation for warned rows, no symlink-following deletes.

## Not audited / blocked

- **Live/dynamic probing:** the daemon was not started and no socket connected to (per method constraints). Findings depending on live behavior are marked: F3's end-to-end reliability (KWin focus policy, CLI tool-call concurrency), F9's trigger (whether hung `claude` drains stdin), whether claude's `acceptEdits` auto-accepts Edit/Write *outside* the session cwd (if yes, worktree containment has a file-level hole in the default mode — worth a live probe), kimi ACP mid-session mode vocabulary, `KMessageBox` default-button behavior on the Cowork enable dialog (if Enter defaults to Allow, F3's enable-approval variant needs a single keystroke), KDE portal dialog wording (external component).
- **UI-side C++ frame parser** (`CoreClient::onReadyRead`) buffer/parse robustness — the boundary runs both ways; the UI is the more privileged endpoint (holds portal sessions).
- **`remote-*` feature** (TLS key/devices/audit files present in the data dir, all 0600) — a remote-access surface outside this audit's scope; if it exposes sessions over the network it dwarfs every finding above. Recommend a dedicated audit.
- **kimi CLI's own stores** (`$KIMI_CODE_HOME` wire logs) — CLI-owned, permissions not verified.
- **Supply chain** (go.mod/CMake pinning, build-time network I/O) — not covered; the sub-audit slots were consumed by higher-priority surfaces.
- **`AiInspectorPanel` per-event work**, core jobs-store pruning, `TerminalPanel` (Konsole part — its own attack surface), LSP subsystem depth — skimmed or untouched.
- **vsix/skills/worktree RPC path containment** (arbitrary `target` dirs, git/gh argv construction) — noted as moot at same-uid today, worth a pass if the socket ever gains real authentication.
- **`AKCORE_PPROF`** binds a user-named address — off by default; a non-loopback value exposes pprof network-wide (one-line doc/comment fix, not a finding).
