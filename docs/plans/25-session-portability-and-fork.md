# 25 — Session portability: export, visualize, and a cross-engine Fork

**Status: PLANNED.** Covers IDEAS #10 (session export and share for Kimi
threads), reshaped by the user's note into a fork-first plan. Program context:
[20-approved-features-program.md](20-approved-features-program.md).

**Size: M–L.** Phase 1 is a spike.

> **User note:** *"We can use this to implement a Fork system as well since
> that's currently a gap with Claude vs Kimi."*

So the plan is written fork-first. Export is one harness method; fork is what
the same session-copying machinery buys, and it is the larger prize because it
closes a real capability asymmetry between the two engines.

## Why

**AgentKate has no way to get a conversation out.** A thread's history lives in
the CLI's session store (claude) or in the core's translated-event log (kimi),
and there is no bundle a user can attach to a bug report, hand to a colleague,
or archive. `kimi export [sessionId] -o out.zip` exists and we never call it;
claude has **no** `export` subcommand at all (verified against the 2.1.220
subcommand list), so for the default engine the bundler has to be ours.

**And Fork is a claude-only feature.** `Capabilities.Fork` is `true` for claude
and unset for kimi. `agent.fork` (`handlers.go:459`) refuses outright:

```go
if !h.Capabilities().Fork { return nil, unsupported("Forking", h.Capabilities()) }
```

Everything the user does with fork — try a different model on the same context,
branch a conversation before a risky refactor, run two approaches from one
setup — is unavailable on kimi. It is the largest remaining asymmetry after
plan 15 and plan 16 P6 closed the rest, and it is the one the user named.

The gap is not a missing flag. Probed live this session:

```
session/fork  →  -32601  "Method not found": session/fork
```

`session/fork` is in the ACP SDK 0.23.0 method table that kimi 0.30.0 bundles,
and kimi does **not** implement it. So a kimi fork has to be built, not enabled.

## Verified facts

| Fact | How verified | Consequence |
|---|---|---|
| claude has no `export` subcommand | `claude --help` subcommand list | `ExportSession` for claude is a core-side bundler, not a wrapper |
| claude forks natively via `--resume <id> --fork-session`, and the new id arrives in the init event | `agent.go:200-212`, `harness_claude.go:234-238` | The claude half already works; this plan does not touch it |
| `kimi export [sessionId] -o out.zip [--no-include-global-log]` | `kimi export --help` | The kimi bundle is a wrapper — and note it bundles `~/.kimi-code/logs/kimi-code.log` **by default**, which is a privacy consideration (see §3) |
| `kimi vis [sessionId] [--port] [--host] [--no-open]` launches a browser visualizer | `kimi vis --help` | "Visualize" is `--no-open` plus our own URL handling |
| `session/fork` is **not implemented** on kimi 0.30.0 | Live ACP probe, `-32601` | Fork must be constructed from the session store |
| kimi's session store: `~/.kimi-code/session_index.jsonl` with one `{"sessionId","sessionDir","workDir"}` line per session; sessions at `<home>/sessions/wd_<hash>/session_<uuid>/` containing `state.json`, `agents/`, `logs/` | Read from disk this session | The copy-fork candidate is concrete and testable |
| `state.json` does **not** contain the session id, but *does* contain absolute `agents.<id>.homedir` paths that embed it, plus `workDir`, `title`, `lastPrompt` | Read from disk | A copy-fork must rewrite those paths — that is the whole trick |
| `agentCapabilities.loadSession = true`; `session/load` is callable and replays history | Live probe + `thread.go:443-449` | `session/load` is the replay half of any fork mechanism |
| `session/resume` accepts a fresh `mcpServers` list and keeps context | Probed in plan 18 | A forked session can be resumed with different wiring |
| `KIMI_CODE_HOME` relocates a kimi thread's whole home, including its session store | `docs/HARNESSES.md`, shipped | A fork may need to land in the *same* home as its source. Non-obvious and load-bearing |

## Phase 1 — SPIKE: how does a kimi thread fork?

**Budget: half a day. Read-only against the repo; writes only to a throwaway
`KIMI_CODE_HOME`.**

Three candidate mechanisms, in order of preference.

### Candidate A — copy the session directory (preferred)

```bash
export KIMI_CODE_HOME=/tmp/ak-fork-spike/home
# 1. create a source session and give it something to remember
#    (drive `kimi acp` over stdio: session/new, session/prompt "remember CODEWORD=quokka")
# 2. copy it
SRC=$KIMI_CODE_HOME/sessions/wd_*/session_<src-uuid>
NEW=$(uuidgen)
cp -a "$SRC" "$(dirname $SRC)/session_$NEW"
# 3. rewrite the embedded absolute paths in state.json
sed -i "s|session_<src-uuid>|session_$NEW|g" "$(dirname $SRC)/session_$NEW/state.json"
# 4. append the index line
echo '{"sessionId":"session_'$NEW'","sessionDir":"…/session_'$NEW'","workDir":"…"}' \
  >> $KIMI_CODE_HOME/session_index.jsonl
# 5. resume the copy and ask for the codeword
```

**Success:** the copy resumes, recalls `CODEWORD=quokka`, and the **source
session is unchanged** after the fork has taken its own turns. Both halves
matter — a fork that corrupts its source is worse than no fork.

Open sub-questions the spike must answer: are there other id-bearing files under
`agents/` (each subagent has a `wire.jsonl`)? Does `session_index.jsonl` tolerate
an appended line without a restart? Does `session/list` show the copy?

### Candidate B — `session/load` into a fresh session

Create a new session with `session/new`, then `session/load` the source's id
into it. The adapter already knows `session/load` replays history
(`thread.go:443-449`, which deliberately avoids it for our own threads because
"an ACP-side replay would double every card"). **Success:** the new session id
differs from the source's and has the source's context.

**Likely failure mode:** `session/load` probably *attaches to* the source
session rather than copying into a new one, in which case both threads write to
the same store — the exact two-processes-one-session hazard plan 21 refuses. Test
for it explicitly by writing from the fork and reading from the source.

### Candidate C — export and replay

`kimi export` the source, unzip, and replay the messages into a new session as
seeded context. **Almost certainly lossy** (tool results and thinking blocks do
not round-trip as prompt content) and it pays a full re-cache. Documented for
completeness; expected to lose.

### Fallback if all three fail — summary-seeded fork, honestly labelled

`kimiHarness.Compact` already performs a real in-session compaction, and the
resume path already knows how to seed a new session from a stored summary
(`minKimiSummaryBytes`, `harness_kimi.go:267`, whose comment explains exactly
this machinery). A summary-seeded fork reuses it: compact, take the summary,
start a new session seeded with it.

It is **lossy and must say so** — in the fork dialog, in the fork's opening
transcript note, and in `Launched.UnappliedOptions`. This is program open
question 3, and it needs the user's call rather than ours.

### Verdict recording

Append a *Result* subsection here. If a candidate wins, set
`Capabilities.Fork: true` for kimi and add `Capabilities.ForkFidelity` (`"full"`
| `"summary"`) so the UI can warn precisely instead of lying by omission.

## Phase 1 RESULT — spiked 2026-08-02: **a fourth mechanism wins**

None of the three candidates below survives. kimi implements fork itself, and
it is reachable: **`POST /api/v1/sessions/<id>:fork`** (AIP-style colon, not a
slash) against a `kimi web` server bound to 127.0.0.1 produces a real,
full-fidelity fork. Run end-to-end, warm and cold, with the source
byte-verified as untouched.

**First, a correction that colours the whole document: the installed CLI is
0.31.1, not the 0.30 this plan and the project memory assume.** Re-stamp the
Verified-facts table. Also: `state.json` *does* carry the session id at
`version: 2` — the table describes the v1 shape.

The mechanism: spawn `kimi web --port <ephemeral> --no-open` with the thread's
`KIMI_CODE_HOME`, read the bearer token from `<home>/server.token`, call the
fork endpoint, shut the server down.

**Phase 1b, which this plan does not currently have and which gates everything:
worktree relocation.** kimi's fork lands in the *source's* workspace bucket,
and ACP resume ignores `cwd` — so the forked session must be moved into the
fork thread's own bucket (move the directory, rewrite `state.json`). Without
it, two AgentKate threads share one worktree while the UI claims each fork has
its own. That is a containment-labelling failure, not a fidelity one, and it is
the reason **`Capabilities.Fork` must not be flipped for kimi yet.**

**Interface consequences.** Do *not* add `ForkSession` to the harness
interface: `StartSpec.ForkSession` (harness.go:187) is already the neutral
seam, and the kimi adapter should honour it inside `Launch`, returning the new
id in `Launched.SessionID`. Add `Capabilities.ForkFlavors []string` alongside
`ForkFidelity` — because summary-seeded fork is universal and user-selectable
per the owner's answer to open question 1, a single fidelity scalar cannot
express "this engine offers both".

**Two risks to carry into Phase 1b.** The model-recall leg is unverified: the
fork is a byte-faithful copy of the wire journal plus a `forked` boundary, but
whether the forked session *recalls* the parent conversation was not proven —
run the codeword round-trip in a credentialed throwaway home, which this plan
already names as the proof. And forking a *running* thread can silently
truncate the fork's tail, because the source's wire is flushed only when its
handle is live in the forking process.

### DECISION (owner, 2026-08-02): **do not use the HTTP server**

The spike's own risk section is the reason. Reaching fork this way means
spawning `kimi web`, which exposes **full session control plus `/terminals/*`
and `/files/*` behind a single bearer token readable by any same-uid process**.
This product's entire threat model is that agents run at the user's own uid, and
four rounds of audit remediation were spent ensuring a prompt-injected agent
cannot widen its own authority. Standing up a general-purpose control server —
however briefly, however bound to 127.0.0.1 — to obtain one fork inverts that
for a convenience. The token file alone is a credential any agent on the box can
read.

So the mechanism is rejected on authority grounds, not on whether it works. It
works; it costs too much.

**What the spike is still worth.** It established that kimi *does* implement
fork internally (`ISessionLifecycleService.fork`) and what a correct fork
produces: a byte-faithful copy of the wire journal plus a `forked` boundary,
with the source untouched. That is the specification to hit — we just have to
reach it without the server.

**The remaining route, and what it needs.** Candidate A (copy the session
directory) was dismissed as "wrong as written" because a naive copy misses the
operations kimi's own fork performs. That is now a solvable problem rather than
a blocker: probe exactly what `ISessionLifecycleService.fork` writes to disk —
which files, which ids rewritten, what the `forked` boundary record looks like —
and replicate it directly. **This is the next investigation, and it should read
the on-disk result of a fork rather than the minified implementation.**

If that probe shows the on-disk shape cannot be reproduced faithfully, the
honest outcome is not a lower-fidelity fork wearing the word "fork". It is the
summary-seeded flavour, which the owner already made universal and
user-selectable, labelled as exactly what it is — and `Capabilities.Fork` stays
false for kimi. This codebase treats a label that overstates its mechanism as a
defect; a "fork" that silently drops the conversation would be one.

**Sequencing, revised:** Phase 2 `ExportSession` first — it is independent of
all of this, uses the plain `kimi export` CLI, and is shippable immediately.
Then the on-disk fork probe. Then Phase 1b (worktree relocation + the
credentialed recall round-trip) only if the probe succeeds. Plan 20's ordering
of 25 before 23 still holds.

## Phase 2 — `ExportSession` on the harness

**`core/internal/harness/harness.go`:**

```go
// ExportSpec is one session-bundling request.
type ExportSpec struct {
    ThreadID  string
    SessionID string
    WorkDir   string
    OutPath   string // absolute .zip path chosen by the user
    // IncludeLogs bundles the engine's diagnostic log. Off by default: kimi's
    // exporter includes ~/.kimi-code/logs/kimi-code.log unless told not to, and
    // that log is GLOBAL — it carries other sessions' activity, so a default-on
    // export would leak unrelated work into a shared bundle.
    IncludeLogs bool
}

// Exported reports what actually landed in the bundle.
type Exported struct {
    Path  string   `json:"path"`
    Bytes int64    `json:"bytes"`
    Parts []string `json:"parts"` // "transcript", "attachments", "record", "diff", "logs"
}
```

plus `ExportSession(ctx, ExportSpec) (Exported, error)` and
`Capabilities.SessionExport bool`.

**Kimi** (`harness_kimi.go`): shell out to
`kimi export <sessionId> -o <out> -y` with `--no-include-global-log` unless
`IncludeLogs`, with the thread's `KIMI_CODE_HOME` in the environment — a thread
with its own home has its session in that home and nowhere else.

**Claude** (`harness_claude.go`): a core-side bundler, because there is nothing
to wrap. New `core/internal/sessionexport/bundle.go` producing:

```
transcript.jsonl      session.ReadTranscript(sessionID)
record.json           the session.Record, with secrets stripped
attachments/          resolved from session/attachments.go's turn store
worktree.diff         worktree.Diff(rec.Worktree) — what this thread changed
subagents/            harness.SubagentTranscripts output
manifest.json         engine, versions, export time, part list
```

`record.json` **must** be scrubbed: `ProviderEnvVar` names an environment
variable and `Env` may carry anything the user set. The record deliberately
never stores the token (`session.go:113-118`) — do not let an export be the
place that regression happens. One `scrubForExport(Record) Record` with a test
that fails on any new field it does not know about.

The claude bundler is also the **fallback for kimi**: if `kimi export` fails, we
still have the core-side event log. Better a partial bundle than none.

## Phase 3 — Export and Visualize in the UI

- **"Export session…"** in the agent panel's menu → `QFileDialog` (default
  `<title>-<date>.zip` in Documents) → `agent.exportSession` → a status message
  linking the file via `KIO::highlightInFileManager`, already used elsewhere.
- **Include diagnostic log** is an explicit, default-off checkbox with a plain
  explanation of what it contains. See open question 2.
- **"Visualize session"** — gated on a new `Capabilities.SessionVisualizer`
  (kimi only). Runs `kimi vis <id> --no-open --host 127.0.0.1`, scrapes the
  bound URL from stdout, and opens it. Two rules: bind to `127.0.0.1` **only**
  (it is an unauthenticated local server over your conversation), and reap the
  process when the thread closes or the app quits so a stray server does not
  outlive the session. Open it in the existing `KPartView`/browser plumbing.
- Both actions register with plan 27 §1's `KActionCollection`, so they reach the
  command palette.
- Long-running: export runs under `safe.Go` core-side and appears as a **row in
  the Jobs panel** (plan 19's `AgentJob`), because a 200 MB bundle is exactly
  the kind of work that should not freeze a menu.

## Phase 4 — Fork, cross-engine

**`harness.StartSpec.ForkSession` stays the neutral request.** What changes is
that kimi can now honour it.

- `harness_kimi.go` `Launch`: when `spec.Resume && spec.ForkSession`, run the
  Phase 1 winner (via a new `kimi.ForkSession(srcID) (newID, error)` in the
  supervisor) and resume the **new** id. Return the new id in
  `Launched.SessionID`.
- `forkAgentThread` (`core/cmd/akcore/agents.go:282`) needs **no change** — it is
  already harness-neutral, already branches the worktree from the source's HEAD
  via `worktree.CreateFrom`, already carries persona/provider/Cowork forward.
  That is the payoff of the plan-14 seam and worth stating: enabling fork on a
  second engine is an adapter change and a capability flip, nothing more.
- `handlers.go:474`'s capability gate then simply passes.
- **`ui/src/ForkAgentDialog.cpp`** already takes a `backend` parameter and
  builds its model/effort pickers from that engine's vocabularies. It needs one
  addition: when `ForkFidelity == "summary"`, a `KMessageWidget` stating that
  this engine's fork carries a summary rather than the full conversation.

**Fork from a checkpoint.** [Plan 23](23-contained-worktrees-and-checkpoints.md)
§6 wants "restore files **and** fork the conversation from this turn" — the only
coherent rewind. That is `agent.fork` with `worktree.CreateFrom(base = the
checkpoint's commit)` instead of the source's HEAD. `CreateFrom` already takes
an arbitrary base (`worktree.go:124`), so it is a parameter, not a mechanism.
Add `Base string` to the `agent.fork` params here so plan 23 can call it without
touching this code.

## Phase 5 — Import (small, and it completes the loop)

An exported bundle should come back in. `session.import` unpacks a claude-shaped
bundle, writes the transcript where `session.Discover` will find it, restores
attachments, and creates a dormant record — reusing `session.attach`'s
already-attached short-circuit so a double import is a no-op.

Value: bug reports become reproducible ("send me the zip"), and it is the only
way a session moves between machines. Cost is small once Phase 2's manifest
exists. Kimi bundles are claude-shaped only in the core-side-log case; a real
`kimi export` zip is kimi's format and is **out of scope** — importing it would
mean reverse-engineering their archive.

## Verify

| Phase | What proves it |
|---|---|
| 1 | A *Result* subsection naming the winning candidate, with the codeword round-trip transcript and an explicit statement that the source session was unchanged afterwards. |
| 2 | `go test ./internal/sessionexport/…`: bundle contains every declared part; `TestScrubForExportDropsEveryCredentialField` uses reflection over `session.Record` and **fails on any newly added field** it has not been taught about — the only way this scrubber stays correct as the record grows. |
| 2 | `TestExportFallsBackToCoreLogWhenCLIFails` — a kimi export whose subprocess fails still produces a bundle, with `Parts` naming what is missing. |
| 3 | Manual: export a real thread, unzip, confirm the transcript replays and `record.json` contains no token, no env values, no provider key name. |
| 3 | Manual: Visualize on a kimi thread opens the visualizer; closing the thread reaps the server (`ss -tlnp` shows the port gone). |
| 4 | `TestForkKimiProducesDistinctSessionAndPreservesSource` — the integration test the spike's manual run becomes. |
| 4 | `harness_caps_test.go` extended: `Fork == true` implies `Launch` honours `ForkSession`, for every registered harness. |
| 4 | Manual: fork a kimi agent mid-conversation, ask the fork about something only the source knew, confirm it knows; send a message to the source, confirm the fork does not see it. |
| 4 | Manual: fork with `ForkFidelity == "summary"` (if that is the outcome) shows the warning banner before the user commits. |
| 5 | `TestImportRoundTrip` — export a thread, import into an empty store, transcript is byte-identical. |

## Non-goals

- **A cross-engine bundle format.** A kimi export is kimi's zip; a claude export
  is ours. The manifest names the engine and the reader decides. Inventing a
  neutral archive format would mean translating transcripts, which is a lossy
  operation we have no reason to perform.
- **Uploading or sharing.** Export writes a local file. No cloud, no paste
  service, no "share link".
- **Importing kimi's own archive.** See Phase 5.
- **Hosting the visualizer.** We launch kimi's, on loopback, and reap it. We do
  not proxy it or embed it beyond opening the URL.
- **Forking a coordinator session**, if plan 21's teams spike ends in adoption —
  the CLI refuses (`"Forking is not available in coordinator sessions"`) and so
  must the UI.

## Open questions for the user

1. **Kimi fork fidelity (program open question 3).** ~~If Phase 1's candidates
   all fail, is a lossy summary-seeded fork acceptable on kimi?~~
   **RESOLVED 2026-08-01: yes — and more.** Summary-seeded fork is not merely
   the fallback: offer it as a *user-selectable fork flavor* on every engine,
   even where native fork exists (a fresh-context fork seeded by a summary is
   sometimes preferable to a full-history clone). Fork UI therefore presents
   "Full history" (where the engine supports it) and "Summary-seeded (fresh
   context)" as labelled options, capability-gated per engine.
2. **Diagnostic logs in exports.** `kimi export` bundles the **global**
   `~/.kimi-code/logs/kimi-code.log` by default, which contains other sessions'
   activity. This plan defaults it **off**. Confirm that is what you want for a
   bug-report bundle, where the log is often the most useful part.
3. **Export scope.** Should "Export session" bundle just the conversation, or
   the conversation **plus the worktree diff** (this plan's default), or the
   whole worktree? The diff makes the bundle self-explanatory; the full tree
   makes it huge.
