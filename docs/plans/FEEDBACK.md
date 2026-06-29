# FEEDBACK — A blunt third-party read on Agent Kate

A perspective on the project as it stands today (2026-06-28), written after a deep read
of the codebase, the plans in `docs/plans/`, and the supporting memory notes. Not a
plan — no file-by-file change list, no phases. The point is to take a step back and
say what is going right, what is at risk, and where I'd cut.

## Snapshot

- Two processes (`agentkate` = C++/Qt6/KF6 UI, `akcore` = Go core) over a JSON-RPC
  bus on a Unix domain socket.
- One headless `claude` per agent thread, isolated in its own git worktree, with a
  Cooperation MCP for shared workspace state and an optional Cowork MCP for
  desktop view/control.
- Size: `ui/src` = 1.3 MB / 127 files, `core/` (incl. tests) = ~1 MB / 91 files. The
  desktop substrate — AT-SPI, KWin scripting, KParts (Okular/Calligra/Ark/…),
  KWallet, xdg-portal — is carried by KDE Frameworks, not by Agent Kate.
- The README is candid that the project "was, by and large, coded directly by
  Claude Opus 4.7." That provenance leaves fingerprints and shapes what I look for.

## What's going right (with evidence)

**IPC (`core/internal/ipc/server.go`)** is the best file in the codebase. The
backpressure policies are spelled out: per-connection in-flight cap (deliberately
*not* global, with the deadlock reasoning attached — `maxInFlightPerConn`'s comment),
notification-shed policy that will *never drop a response*, write-deadline + a
dedicated writer goroutine per connection so producers never block on the socket.
The Cowork keystone (`role` / `threadID` / `isPrimary` + `MarkUI` / `BindBridge` /
`RequireUI` / `NotifyPrimaryUI`) is the right shape for cross-cutting capability
identity and the same-uid honesty is in writing, not buried.

**Agent supervisor (`core/internal/agent/agent.go`)** does not just spawn — it
coalesces with semantic boundaries, deduplicates on `immediately-preceding byte
equality` (the exact `claude --verbose` partial-snapshot pattern), uses `Setpgid`
so signals land on the whole process tree, has an *in-band then signal-grade*
interrupt path, and ships concurrency-shareable hot-compaction. The decision
comments explain *why* the dedup window is the immediately-preceding event — that
is a person who has hit the failure mode.

**Provider routing (`core/internal/agent/provider.go`)** is small, opinionated, and
correct-by-design at the security boundary. `buildEnv` strips inherited
`ANTHROPIC_*` before adding the provider's, so a real Claude key in `akcore`'s env
cannot be forwarded to a third-party base URL. The test
(`provider_test.go:37-87`) asserts the *exact* leaked-key substring cannot appear
in the resulting env. That is the kind of security work that compounds.

**Skills (`core/internal/skills/skills.go`)** is already a centrally catalogued,
symlinked-into-target system with explicit refusal heuristics: refuses to delete
unmanaged symlinks (`:265`), refuses symlinks whose `Readlink` target does not
resolve into the catalog (`:261-264`), caps content reads at 256 KiB
(`maxSkillContentBytes`). The right kind of defensive.

**`safe.Go(name, fn)`** wraps every long-lived goroutine in panic-recovery with a
named slog entry. ~200 bytes. Means a panic in one goroutine cannot kill the
whole daemon.

**Plan culture.** Eleven numbered plans, every one grounded on `file:line`
references, an acknowledged phasing, and a file map. The Cowork dossier alone is
eleven sub-plans with an adversarial review gate and an amended v1 scope. Decisions
land in writing, not in PR descriptions, so they survive.

## Agentic-author fingerprints worth normalising

Coding agents tend to leave recognisable tells. Several show up here. None is
fatal; each is a clean refactor target.

1. **`core/cmd/akcore/main.go` is 2,936 lines, 28 functions.** It owns flag parsing, `runCore`, `registerHandlers`, every `agent.start` / `agent.resume` /
   `agent.promote` path, the model tier resolver, the MCP-config writer, and the
   exit-compaction tracker. Reading it linearly is hard even for the author.
   Extract `run.go` (lifecycle + flags), `handlers.go` (RPC registration +
   per-method dispatch), and `agents.go` (start / resume / promote / compact).
   Half-day refactor, low risk, large readability payoff.

2. **`ui/src/AgentPanel.cpp` is 2,835 lines, but the dominant content has already
   moved out.** The chat-surface widget graph is now `TranscriptModel` +
   `TranscriptDelegate` driving a `QListView` (commit `ab4cab7`, plan 10 Phase 2,
   landed + ctest clean; `5cf5516` adds the in-RAM feed cap). Resize is now
   ~3 ms/step regardless of transcript length, per the height-cache fix in the
   plan 10 phase-2 notes. **What remains in the 2,835 lines is the form, send,
   tool dispatch, search, copy, and replay state machine.** A further slim-down
   is real but smaller; the flagship UI refactor has shipped. The leak-hunt
   memory's open items are `EditorArea::m_groups` (empty groups not pruned on
   last-tab-close) and `gitstatus.LogModel::m_rows` (user-driven growth) —
   neither lives in `AgentPanel.cpp`, and the second was named `LogModel`
   loosely in the original review when it should have been `gitstatus.LogModel`.

3. **Same-uid honesty.** The keystone stops prompt-injection via a co-resident
   agent's bridge, and stops a bridge spending another thread's grant; it does
   *not* stop a determined same-uid process from forging a handshake on the raw
   socket. `os.Chmod(socketPath, 0o600)` + `$XDG_RUNTIME_DIR` 0700 are the only UID
   defences. Plan 08's framing is correct but not loud enough — this needs to be
   in the README and in any user-facing threat-model doc.

4. **Compaction is one-shot.** Hot-exit compaction captures *one* summary per
   thread. At current Opus context sizes that is fine; at longer contexts it
   will start to drift. Worth staging a rolling, per-turn compaction *before* it
   becomes a problem, not when it does.

5. **The "agent other than claude" surface area is implicit.** The README invites
   PRs for Codex/OpenCode support; the plan for #5 keynote-modes defers; nothing
   in core talks about it. Either say "v1 is Claude-only by design" loudly or
   introduce a one-screen `agent.Harness` interface behind a build tag. Don't
   leave it implicit.

## Where I was wrong earlier

You were right on three points; I'm conceding them now in writing.

- **Bloat.** I called the surface area "enormous." It isn't. 1.3 MB UI +
  1 MB core including tests, on top of KDE's substrate, is not bloated.
  Most of what looks like surface area is composition.
- **Audience.** "Linux power users who want an AI harness that actually fits the
  platform — work, not just code" is not narrow in the consequential sense. The
  Cursor / JetBrains / VSCode / Zed-with-OpenCode lane on Linux is embarrassing;
  Claude Code et al. are terminal-resident by design. The lane Agent Kate
  occupies has, in practice, no competition.
- **Preset modes.** Skills are already a catalogue that installs into projects.
  If *Coding / Office / Business Analysis / Document Creation / Presentations*
  modes land as **bundles of skills + a default tool roster + (optionally) a
  small UI template** applied to the same harness, the per-mode line-item cost
  is small. Do *not* fork the harness into one UI per mode — that is the
  failure mode. Modes are content of the harness, not code paths in it.

## Where I'd cut (decide what *not* to build)

- **A separate `MediaView` beyond what plan 07 ships.** Plan 07 §Tier 2 was right;
  QtMultimedia gating via `find_package` is fine. Don't move beyond it.
- **A plugin architecture in v1.** Resist "we should be extensible." Two real
  consumers (Cowork, Provider) earn their abstractions; don't add a third on
  speculation.
- **KWin virtual desktops as a *security* boundary.** Plan 08 §5 already says
  "organizational, not a security boundary." Stamp this in the security doc so
  nobody files a CVE on it later.
- **A custom markdown renderer.** Plan 02 routes through `QTextDocument::setMarkdown`,
  which is right. Custom renderers die.

## Concrete recommendations (next ~12 months)

> **Note on a self-correction from a verification pass:** an earlier draft of
> this section led with "Close Plan 10 Phase 2 — land `TranscriptModel`
> virtualisation." That recommendation was based on the plan doc + the
> MEMORY.md index phrasing and was not reconciled against `git log`. Chat
> virtualisation shipped at `ab4cab7` (commit verified, code in tree) plus the
> in-RAM feed cap at `5cf5516`. Plan 10 is *closed*; the recommendation is
> removed and replaced by what's actually open.

These are *graded*: do them in order, each one buys more than the next.

1. **Write `docs/security-model.md`.** State explicitly: Agent Kate trusts the
   *user's* agents with the user's credentials; IPC is per-uid protected;
   keystone identity stops prompt-injection via a bridge; same-uid processes
   are *not* isolated; the audit log + kill-switch are detection, not
   prevention. Anyone enabling Cowork *must* understand that an agent with
   Cowork access is "you with a keyboard," not "the agent in a sandbox." This
   is the largest honest gap in the project as it stands.
2. **Split `core/cmd/akcore/main.go`** into `run.go`, `handlers.go`, `agents.go`.
   Same wire surface. Easier to onboard a second contributor; easier to read in
   review.
3. **Slim `AgentPanel.cpp` further along its remaining seams** — form, send,
   tool dispatch, search, copy, replay. The flagship chat refactor has shipped.
   What remains is real but smaller.
4. **Build the preset-mode architecture around the skills catalogue, not
   around new UI.** New modes ship as catalogues (`agentkate-modes/office`,
   `agentkate-modes/business-analysis`, …) — directory skills + a tool roster +
   a starter template. The UI gains a "currently in mode X" indicator; the
   harness gains nothing.
5. **Cowork v2 opens up *only* once v1 lands with the per-connection cap and
   the audit hash chain.** The keystone identity is the linchpin; if it holds,
   control primitives follow cleanly. If it doesn't, they shouldn't.
6. **Provider routing is the right precedent for plugin-side infrastructure.**
   When a second consumer wants the same shape (a *lint* side-car that runs on
   diffs pre-commit; a *test* side-car that runs the relevant ctest on diffs),
   extract that pattern. Don't invent a plugin abstraction before then.

## Closing posture

If I had to compress this to a single sentence for project strategy:

> **Agent Kate's defensibility comes from owning the platform layer — the parts
> of an AI harness that depend on a real desktop substrate — not from competing
> on model surface area.**

The current direction is right. The code quality is higher than it has any
right to be. The biggest danger is the obvious next move: agentic breadth —
more modes, more harnesses, more providers, more control primitives —
without a discipline that says "we earn each one." The biggest opportunity is
the unsubtle one: on a Linux desktop with a real desktop substrate, an AI
harness that *uses* the substrate (rather than wrapping the substrate in a
web view) has a multi-year lead that nobody else is contesting. Stay the
course; pick the cuts carefully.
