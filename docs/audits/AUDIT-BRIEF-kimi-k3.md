# Audit brief — AgentKate quality / stability / efficiency / security

**For:** an independent auditing agent (Kimi K3) working in this repository.
**Deliverable:** a single new findings document at
`docs/audits/FINDINGS-<engine>-<YYYY-MM-DD>.md` (create the directory if needed).
**Authorisation:** this is the owner's own application, audited on the owner's own
machine, at the owner's request. It is a defensive review of a local desktop app —
find weaknesses so they can be fixed. Do not write exploit tooling; write findings.

---

## 0. What AgentKate is (so your findings land in the right place)

AgentKate is a native KDE Plasma desktop application that runs multiple AI coding
agents in parallel ("a multi-agent coding arena"). Two processes:

- **`akcore`** — a Go daemon (`core/`, binary `akcore`). Owns agent lifecycles,
  spawns CLI agent processes (`claude`, `kimi`), holds session state, serves
  JSON-RPC over a **unix domain socket** to the UI, and exposes an **MCP server**
  so agents can call orchestration tools.
- **`agentkate`** — a Qt6/KF6 C++ GUI (`ui/`). Talks to akcore over that socket.

Both run as the **same unprivileged desktop user**. That is the central fact of
the threat model: there is no privilege boundary between the agents and the user's
own account. The security value AgentKate can offer is therefore about *containment,
honesty and blast radius*, not about sandboxing a hostile local root.

Key subsystems you will meet:

| Area | Where | Note |
|---|---|---|
| IPC framing | `core/internal/ipc/` | Newline-delimited JSON, 16 MiB frame cap, oversize-survival reader |
| Agent process spawn | `core/internal/agent/`, `core/internal/kimi/` | Builds argv/env for the CLIs; per-thread env injection for third-party providers |
| Session/persistence | `core/internal/session/` | JSONL stores, transcripts, attachments |
| Worktrees | `core/internal/worktree/` | Per-thread git worktrees (agents are *supposed* to stay inside them) |
| Cowork (desktop control) | `core/internal/cowork/`, `ui/src/cowork/` | xdg-desktop-portal RemoteDesktop/ScreenCast: real keyboard, pointer and screen capture |
| MCP orchestration tools | `core/cmd/akcore/mcp_*.go` | Agents can launch/send/close other agents, enable Cowork |
| Permissions | `core/internal/permission/`, per-thread modes | Gates what an agent may do |
| Secrets | KWallet integration, provider API keys | Third-party provider routing |
| UI rendering | `ui/src/TranscriptDelegate.cpp`, `AgentPanel.cpp` | Renders untrusted model/tool output as rich text |

## 1. Scope

Audit **the whole repository as it stands on branch `kimi-code-backend`**, with
particular attention to the large uncommitted delta (`git diff 2bd555d` plus
untracked files) landed by a six-round automated review sweep on 2026-08-01:
Claude stream/control channel work, kimi capability parity, KDE shell integration
(notifications, single-instance, activation), attachment paste/drop/caching, jobs
panel, and Cowork portal hardening.

Out of scope: the `docs/plans/2x-*.md` files describe *unbuilt* features — read
them for intent, but do not report findings against code that does not exist.

## 2. The four lenses

Report findings under whichever lens fits; a finding may carry more than one.

### 2.1 Security (the lens that motivated this brief — go deepest here)

Work the following surfaces concretely. For each, the question is not "is there a
CVE" but "what can go wrong, who can cause it, and what is the blast radius".

1. **Untrusted input from model output.** Everything an agent emits is untrusted
   text controlled, ultimately, by whatever the model read — including files in a
   repo the user just cloned. Trace it: does model/tool output ever reach a shell,
   a `QProcess`, a URL handler, a file path, a D-Bus call, or a rich-text renderer
   without neutralisation? Look hard at Markdown→HTML rendering (`markdownToHtml`,
   `neutralizeMarkdownRawHtml`, `TranscriptDelegate`) for HTML/script injection,
   local-file exfiltration via remote resource loads, and `file://`/`data:` URL
   handling. Check attachment name/path handling for traversal (`../`), and image
   decoding paths for resource exhaustion (decompression bombs, absurd dimensions).
2. **Prompt-injection reachability.** AgentKate exposes MCP tools that let an
   agent act on the *system* (launch other agents, send them messages, enable
   desktop control). A malicious file in a repo can instruct an agent. Which tools
   can be invoked with no human in the loop? Which ones widen authority (Cowork =
   real input injection into the user's desktop; orchestration = spawning more
   agents)? Is every authority-widening tool gated by an explicit human approval
   that names the agent, the reason, and the effect — and can that gate be
   satisfied by anything other than a human click?
3. **Worktree escape.** Per-thread worktrees are meant to bound where an agent
   writes. Determine, by reading the spawn path (cwd, `--add-dir`, allowed tools,
   permission mode), how an agent escapes to the parent repo or `$HOME`, and
   whether AgentKate ever *tells the user* it is contained when it is not. The
   owner already reports agents wandering out; find the mechanisms.
4. **IPC trust boundary.** The unix socket: check its path, permissions (mode,
   directory, `XDG_RUNTIME_DIR` vs `/tmp`), whether any peer credential check
   exists, and what an unrelated process on the same machine (or another user, if
   the path allows it) could do by connecting. Same question for the MCP endpoint
   and any HTTP/localhost listener you find. Frame-cap and parser robustness:
   malformed/oversize frames, unbounded allocations, panics reachable from the
   wire.
5. **Secrets.** API keys and provider credentials: how they are read, stored
   (KWallet vs plain config), passed to child processes (env vs argv — argv is
   world-readable via `/proc`), and whether they can leak into logs, transcripts,
   crash reports, exported sessions, or the `argsSummary` shown in the UI. Check
   the redaction paths actually cover what they claim.
6. **Subprocess construction.** Every `exec.Command` / `QProcess`: is any argument
   built from untrusted data? Is a shell ever interposed (`sh -c`)? Are `PATH` and
   the environment controlled, or can a repo-local binary shadow a tool?
7. **Cowork / desktop control.** This subsystem can type and click as the user.
   Audit: how a session is established, what re-authorisation happens on re-attach,
   whether an agent can enable it without a fresh human approval, whether the
   permission report the user sees is *true* (it recently was not), and what
   happens on crash — including the desktop-wide `org.a11y.Status` flip and its
   restore paths.
8. **Filesystem hygiene.** Temp files and cache/data directories: predictable
   paths, symlink-following, `O_EXCL` on create, permissions on files containing
   transcripts (which may hold secrets the model was shown), and whether pruning
   can delete data the user believes is durable.
9. **Denial of service on the user.** Unbounded growth (transcripts, caches,
   jobs, notification storms), a wedged D-Bus/portal call freezing the GUI thread,
   and any path where one misbehaving agent degrades the whole app.
10. **Supply chain, lightly.** `go.mod` and the CMake/KF6 dependency set: pinned?
    any dependency doing network I/O at build time? Do not audit upstream code —
    just report the exposure surface.

### 2.2 Stability

Lifetime bugs across async boundaries (Qt: callbacks capturing raw `this`;
`QDBusPendingCallWatcher`; `KNotification` ownership. Go: goroutine and channel
leaks, context cancellation, mutex coverage of shared state — run
`go test -race ./...`). Signal/slot ordering and re-entrancy. Error paths that
return without releasing what they took. Partial refactors (grep every caller of a
changed function). Reconnect/replay/interrupt/engine-switch edge cases.

### 2.3 Efficiency

Quantify, don't vibe. Per-paint work in the delegate; per-token work in the
streaming path; per-event allocations in the Go translators; IPC chatter per turn;
startup cost; unbounded in-memory growth. State the cost in calls-per-paint or
per-turn, before and after your proposed fix.

### 2.4 Code quality

Cross-engine seam violations (engine `if`-ladders in the UI where a capability
flag belongs), asymmetries between the claude and kimi harness adapters, dead code
and dead stores, **comments that lie about the code**, duplicated logic, and test
quality — do fixtures pin *real* wire shapes, and would each test actually fail if
the logic were inverted? Check `docs/plans/README.md` and plan 19 against what the
code now does.

## 3. Method

- **Read the code.** Every finding must cite `file:line` and quote enough to prove
  it. Findings without evidence are noise and will be discarded.
- **Probe live where it is safe and non-destructive.** `claude --help`,
  `kimi --help`, `go test`, `go vet`, `ls -l` on socket paths, reading your own
  `/proc/self`, building the project. Do **not** run destructive commands, do not
  touch the user's real sessions, do not exfiltrate anything, do not commit or push.
- **Verify before reporting.** Prefer "I ran X and got Y" over "this looks like".
  If you cannot confirm, say so and mark confidence.
- **Adversarial self-check.** Before writing a finding, argue the opposite for a
  moment: is there a guard elsewhere that makes it unreachable? Report only what
  survives. It is entirely acceptable to report that a surface is sound.
- **Do not modify code.** This is a read-and-report engagement. The only file you
  create is the findings document.

## 4. Output format

Write `docs/audits/FINDINGS-<engine>-<YYYY-MM-DD>.md`:

```markdown
# Findings — <engine>, <date>

## Summary
<10 lines max: what you audited, what shape it is in, the 3 things that matter most.>

## Findings

### F1. <one-line title>
- **Lens:** security | stability | efficiency | quality
- **Severity:** critical | high | medium | low   **Confidence:** confirmed | likely | unverified
- **Where:** `path/to/file.cpp:123` (plus other sites)
- **What:** the defect, stated plainly.
- **Why it matters:** a concrete failure narrative — who does what, and what breaks.
  For security findings: attacker, precondition, capability gained, blast radius.
- **Evidence:** quoted code / command output that proves it.
- **Fix sketch:** the smallest change that closes it, and any trade-off.
- **Effort:** small | medium | large

### F2. …

## Surfaces audited and found sound
<Brief list. This is as valuable as the findings — it tells the next reviewer
what not to re-do, and it keeps the report honest.>

## Not audited / blocked
<Anything you could not reach, and why.>
```

Order findings by severity, then by blast radius. Aim for precision over volume:
twenty vague findings are worth less than five proven ones.

## 5. Ground rules

- Nothing gets committed, pushed, deleted, or reconfigured. Findings only.
- No exploit tooling, no persistence mechanisms, no credential harvesting — even
  as a demonstration. Describe the weakness; the owner will fix it.
- If a finding involves a real secret you happened to see, **redact it** in the
  report and say that you redacted it.
- If you believe some part of this brief is wrong about the codebase, say so in
  the report. Being corrected is a good outcome.
