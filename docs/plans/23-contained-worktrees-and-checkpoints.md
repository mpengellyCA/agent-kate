# 23 — Contained worktrees, and a checkpoint timeline on top of them

**Status: PLANNED.** Covers IDEAS #4 (checkpoints / rewind), reshaped by the
user's note into a worktree rework first and a timeline second. Program
context: [20-approved-features-program.md](20-approved-features-program.md).

**Size: XL** — the largest item in the program, and the only one that changes
where every agent process runs. Linux-only by construction.

> **User note:** *"This is an opportunity to improve and rework the Worktree
> system along with checkpoints. Currently it is escapable and agents have
> gotten confused. I think maybe we can run the agents inside a linux terminal
> container so it can't easily escape to higher directories without being given
> a deliberate path and hatch."*

## Why

**The worktree is a convention, not a boundary.** `worktree.Create`
(`core/internal/worktree/worktree.go:75`) runs `git worktree add` under
`<repoRoot>/.agentkate/worktrees/<threadID>` and the supervisor sets
`cmd.Dir = opts.WorkDir` (`core/internal/agent/agent.go:434`). That is the
entire isolation. The agent's `Bash` tool can `cd /`, read `~/.ssh`, edit the
main checkout, or — the reported failure — wander into a *sibling agent's*
worktree and start editing there, convinced it is in its own. There is no
kernel-level thing stopping any of it. `--add-dir` and `--disallowedTools` are
the CLI's own advisory permission layer, useful and not a boundary: a Bash
command is one string, and no allow-list survives contact with `cd ..`.

That has two costs. The obvious one is safety. The one the user actually
reported is **confusion**: an agent that can see six sibling worktrees, the main
checkout and the user's home has to *infer* which paths are its own from
context, and it gets it wrong. Making the wrong paths not exist is a correctness
fix before it is a security fix.

**And checkpoints are only honest inside a boundary.** IDEAS #4 asks for a
timeline rail where every turn boundary is a restorable point. Restoring the
worktree is meaningless if the agent also wrote to `~/.config` and the main
checkout — you would restore a third of the change and call it a rewind. So
containment is not a nice pairing with checkpoints; it is their **precondition**,
and this plan is ordered accordingly.

## Phase 1 — SPIKE: which containment mechanism?

**Produces a verdict and a throwaway prototype. Budget: one day.**

### The candidates, and what each actually is

| Candidate | Present on this box | What it gives | What it costs |
|---|---|---|---|
| **bubblewrap** (`bwrap` 0.11.2) | yes | Unprivileged user + mount + pid namespaces. Per-path `--ro-bind` / `--bind`; everything else simply is not in the tree. No daemon, no image, no root. | New mount namespace ⇒ every path the agent legitimately needs must be named. Wayland/D-Bus sockets need explicit binds for Cowork. |
| **systemd-nspawn** | yes | A full container with its own PID 1 and (usually) its own root filesystem. | Wants a root filesystem or an `--ephemeral` machine; needs root or a `machinectl` setup; heavy startup; the agent loses the user's PATH, toolchain and credentials unless painstakingly re-bound. |
| **podman** (6.0.1) | yes | Rootless OCI containers. Strong, familiar, image-based. | An image is a *different userspace*: the agent's `node`, `go`, `cargo`, language servers and `git` config are the image's, not the user's. Startup is hundreds of ms. Cowork (D-Bus/Wayland/AT-SPI) becomes a project of its own. |
| **landlock** (LSM enabled: `capability,landlock,lockdown,yama,bpf`) | yes | In-process, unprivileged filesystem access restriction on a process tree. No namespaces at all. | **Denies but does not hide.** `ls /home/mike/Dev` still shows the sibling worktrees; the agent still gets confused, it just gets EACCES afterwards. Also cannot express "this path is writable, that one read-only, the rest invisible" as a *tree*. |
| **unshare + manual mounts** | yes | The primitive bubblewrap wraps. | We would be writing bubblewrap, badly. |
| **The CLI's own `--add-dir`** | n/a | The engine's advisory tool-permission scope. | Not a boundary (Bash escapes it). Also not cross-engine: `kimi acp` accepts no flags at all (verified), so kimi threads would have nothing. |

### Standing recommendation (to be confirmed or overturned by the spike)

**Bubblewrap as the default `contained` tier**, with:

- **landlock-only** as a degraded fallback where user namespaces are unavailable
  (some hardened distros, some container hosts). It is worse — it denies rather
  than hides — but it is better than nothing and it needs no namespaces. Report
  the degradation in the UI; never claim containment we do not have.
- **podman** as an opt-in **`hard`** tier for the "I am pointing an agent at
  something I do not trust" case, accepting the toolchain and Cowork costs
  explicitly.
- **`--add-dir` retained** as the in-CLI advisory layer *inside* the sandbox
  (defence in depth), never as the boundary.

The argument for bubblewrap over the others, in one line each: it is the only
candidate that **hides** rather than denies (which is what fixes the reported
confusion), the only one that keeps the user's own toolchain and credentials
(which is what keeps agents useful), and the only one whose startup cost is
invisible next to spawning a CLI.

This box supports it: `/proc/sys/kernel/unprivileged_userns_clone` is `1` and
`/proc/sys/user/max_user_namespaces` is `127807`.

### Probe commands

```bash
# S1 — the core question: does `cd ..` stop working, and does git still work?
REPO=/home/mike/Dev/AgentKate
WT=$REPO/.agentkate/worktrees/t-spike
git -C $REPO worktree add -b agentkate/t-spike $WT HEAD

bwrap --unshare-all --share-net --die-with-parent \
  --ro-bind /usr /usr --ro-bind /etc /etc \
  --symlink usr/lib /lib --symlink usr/lib64 /lib64 --symlink usr/bin /bin \
  --proc /proc --dev /dev --tmpfs /tmp \
  --bind "$WT" "$WT" \
  --bind "$REPO/.git" "$REPO/.git" \
  --chdir "$WT" \
  -- bash -lc 'pwd; ls ..; ls '"$REPO"'; git status --short; git log --oneline -1'
#   EXPECT: `ls ..` is empty (no sibling worktrees), `ls $REPO` shows only
#   .git and .agentkate, and BOTH git commands succeed.

# S2 — the git indirection gotcha, stated explicitly.
#   A worktree's .git is a FILE: `gitdir: <repoRoot>/.git/worktrees/<id>`, and
#   `git rev-parse --git-common-dir` resolves back into <repoRoot>/.git.
#   Verified on this repo. Bind ONLY the worktree and git breaks completely.
#   S1 above is the minimal correct mount set; prove it, do not assume it.

# S3 — does the agent CLI itself still work inside?
bwrap … --bind ~/.claude ~/.claude --ro-bind ~/.claude.json ~/.claude.json \
  -- claude -p --output-format stream-json --verbose 'run `pwd` and tell me'
#   EXPECT: auth works, the tool runs, pwd is the worktree.
#   THEN re-run WITHOUT the ~/.claude bind and record the failure mode —
#   that is what a user with a mis-declared profile will see.

# S4 — does Cowork survive? (this is the one that can kill the design)
#   Add: --bind $XDG_RUNTIME_DIR/bus, --bind $XDG_RUNTIME_DIR/$WAYLAND_DISPLAY,
#        --setenv DBUS_SESSION_BUS_ADDRESS, --setenv WAYLAND_DISPLAY
#   Then run a thread with Cowork enabled and confirm desktop_list_windows works.
#   NOTE: the Cowork MCP bridge is a SEPARATE process the CLI spawns; it inherits
#   the sandbox. Its socket to akcore ($XDG_RUNTIME_DIR/agentkate/*.sock) must be
#   bound too or every desktop RPC fails.

# S5 — startup cost, measured not guessed.
hyperfine 'bwrap … -- true' 'podman run --rm alpine true' 'true'

# S6 — the fallback: landlock without namespaces.
#   Prototype with a tiny Go helper using landlock-lsm/go-landlock, confirm the
#   agent gets EACCES on a sibling worktree, and confirm it still SEES it.

# S7 — kimi inside the sandbox (KIMI_CODE_HOME must be bound rw).
```

### Success criteria

1. **`cd ..` shows nothing useful.** The sibling worktrees and the main
   checkout's source are not in the tree. (S1)
2. **git works** — status, log, diff, commit, and `git worktree` bookkeeping.
   (S1, S2)
3. **Both CLIs start and authenticate** inside, with a declared profile. (S3, S7)
4. **Cowork still works** with the sockets declared, or we accept that
   `contained` and `cowork` are mutually exclusive and the UI says so. (S4)
5. **Startup cost under ~30 ms**, i.e. invisible next to a CLI spawn. (S5)
6. **No root, no daemon, no setuid helper** required. (all)

**If criterion 4 fails**, the design still ships: `contained` and Cowork become
mutually exclusive, gated in the UI with an explanation, and the user picks per
agent. That is an acceptable outcome and must be planned for, not discovered.

## Phase 1 RESULT — spiked 2026-08-02: **bubblewrap, confirmed**

All six success criteria pass on this box (CachyOS, kernel 7.1.5). A real
`claude` 2.1.220 thread ran to completion inside the sandbox — authenticated,
used Bash, and reported `ls ..` as exactly `t-alpha` — for $0.092. Overhead is
**7 ms** per spawn against a 30 ms budget. `kimi` runs inside with
`KIMI_CODE_HOME` already under the containment root, so it costs no extra
binds. **Criterion 4 (Cowork) passed**, which this plan expected might fail —
so the mutually-exclusive fallback does not need building.

**But the mount set as drafted is wrong in ways that are worse than no
containment**, because each omission breaks the product rather than the
boundary, and a containment that breaks the product gets turned off:

- **`--ro-bind-try /run/systemd/resolve` is missing.** Without it there is no
  DNS at all: every agent fails to reach the API with curl code 000. This is
  the single most likely "containment broke everything" report.
- **`~/.claude` must be bound rw** or the agent reports `Not logged in`.
- **`--disable-userns --assert-userns-disabled` is missing.** Without it the
  agent can `unshare -Ur` to root of a nested namespace and hold
  CAP_SYS_ADMIN. This also means spelling `--unshare-user` explicitly rather
  than relying on `--unshare-all`, which rejects the flag.
- **`.git` needs masking, not just binding**: `--tmpfs` over `.git/hooks` and
  `.git/worktrees`, or a contained agent writes hooks that run unconfined.
- **Invariant 1 is inverted.** `--unshare-pid` does not break `Interrupt`;
  `kill(-pgid, SIGINT)` reaches the sandbox leader gracefully, and it *closes*
  an existing leak where a `setsid`'d tool survives the kill ladder entirely.

**The landlock-only fallback does not work and must not ship transparently.**
Deny-mode makes `git commit` fail outright (`fatal: unknown error occurred
while reading the configuration files`). If it ships at all it is an explicitly
labelled reduced tier with its own dotfile allow-list — and a thread that asked
for `contained` must fail rather than silently receive something weaker.

**The residual escape surface, stated plainly so no label overstates it:** the
shared `.git` object store. A contained agent can still write any object and
move any ref in the project's store. Containment bounds the *working tree*, not
the repository. Say exactly that in the UI — this plan exists partly because
the previous label ("sandbox") claimed more than a git worktree delivered.

**Cowork widens the boundary materially.** Binding `$XDG_RUNTIME_DIR/bus` hands
the agent the full KDE session bus — KWin, the portal, everything on it. That
is not a filesystem question and the consent text must say so.

**Recommendation:** ship bubblewrap as `TierContained` and make it the default
as planned, with the hardened mount set above. Keep podman as an opt-in `hard`
tier. Keep `--add-dir` as an advisory layer *inside* the sandbox, never as the
boundary.

## Phase 2 — The containment layer (core)

New package **`core/internal/contain/`**. It knows nothing about agents; it
turns a profile into an `*exec.Cmd` wrapper.

```go
// Tier is how hard the boundary is.
const (
    TierNone      = "none"      // today's behaviour: cmd.Dir and nothing else
    TierContained = "contained" // bubblewrap namespace (the new default)
    TierHard      = "hard"      // podman, opt-in
)

// Profile is one thread's containment, fully declared. Everything the agent
// can reach is in this struct — that is the point.
type Profile struct {
    Tier    string
    Root    string   // the worktree; the only rw path by default
    ReadOnly []string // /usr, /etc, the CLI's config dir …
    ReadWrite []string // <repoRoot>/.git, the thread's KIMI_CODE_HOME, …
    // Paths are the user's DELIBERATE additions (the note's "path"), persisted
    // on the record and replayed on resume.
    Paths []Path
    // Sockets the agent needs: the akcore IPC socket always; the session bus
    // and Wayland display only when Cowork is on.
    Sockets []string
    Network bool // default true; false is a real option for a review agent
}

type Path struct {
    Host      string `json:"host"`
    ReadOnly  bool   `json:"readOnly"`
    // GrantedBy: "user" (declared at launch) or "hatch" (agent asked, human
    // approved). Reason is the agent's verbatim justification for a hatch.
    GrantedBy string `json:"grantedBy"`
    Reason    string `json:"reason,omitempty"`
}

// Wrap returns the argv that runs `inner` under this profile, or inner
// unchanged for TierNone. Availability() reports why a tier is unusable on
// this machine (no bwrap binary, userns disabled) so callers can degrade
// LOUDLY rather than silently running uncontained.
func (p Profile) Wrap(inner []string) ([]string, error)
func Availability() map[string]string
```

**Wiring:** `core/internal/agent/agent.go:433` becomes
`exec.Command(argv[0], argv[1:]...)` where `argv = profile.Wrap(append([]string{s.claudeBin}, buildStartArgs(opts)...))`.
Kimi's equivalent in `core/internal/kimi/thread.go` gets the same treatment.
`StartOptions` / `kimi.StartOptions` gain `Contain contain.Profile`; the neutral
`harness.StartSpec` gains it too, and **it is not capability-gated** — this is
the core's boundary around the CLI, not a CLI feature, so every harness gets it
identically. That is the strongest argument for putting it in `contain` rather
than in either adapter.

**Three invariants that must be written into the code as comments, because each
one is a bug waiting to be reintroduced:**

1. `Setpgid` (`agent.go:448`) must survive. Interrupt signals the group
   (`agent.go:641`); a `--unshare-pid` sandbox gives the child its own PID
   namespace where our pgid means nothing. Either drop `--unshare-pid` or make
   `Interrupt` signal the bwrap leader and let `--die-with-parent` cascade.
   **Prove which in the spike (S1 extension).**
2. The provider env scrub (`buildEnv`, `provider.go:115`) runs *before*
   wrapping. A sandbox that passes `--setenv` for everything would resurrect
   scrubbed credentials. `Wrap` must take the already-built env, not rebuild it.
3. `contain.Availability()` failure is **never** a silent downgrade to
   `TierNone`. A thread the user asked to contain and that could not be
   contained fails to start, with the reason — the same applied-truth rule the
   rest of the system runs on.

## Phase 3 — Paths and hatches

The user's two nouns become two mechanisms.

**A path is declared.** `session.Record` gains
`Contain ContainSpec` (tier + `[]Path`), persisted and replayed on resume,
fork and promote — like `Env` and `DisallowedTools` before it
(`session.go:90-106`). Declared in the New Agent dialog's advanced section, and
editable on a dormant thread.

**A hatch is requested.** A new Cooperation MCP tool `request_path` (beside
`enable_cowork`, `core/cmd/akcore/mcp_cowork.go`):

- Mandatory `path` and `reason`; the reason is shown to the human **verbatim**
  in a consent dialog, exactly as `enable_cowork` does.
- Approval is unconditional — never auto-granted, never inherited from an
  orchestration approval. A worker asking for `/` is a prompt, not an error.
- **Granting re-attaches the thread.** A bind mount cannot be added to a live
  mount namespace from outside. Generalise `reattachForCowork`
  (`core/cmd/akcore/cowork_enable.go`) into
  `reattachThread(threadID, reason string)`: wait for the current turn
  (`TurnTracker.Wait` — never discard a turn in progress), stop, wait for the
  reap, relaunch with the new profile, resume the same session. Report
  `applied: "reattach"` with the same vocabulary plan 18 established. This is
  the third caller of that machinery (Cowork, plan 22's kimi components, hatches)
  which is a good sign the abstraction is right.
- The grant is recorded on the record with `GrantedBy: "hatch"` and its reason,
  so a resume months later still shows *why* this agent can see `/opt/toolchain`.

**Escape-hatch of last resort:** a per-thread "Uncontain this agent" action that
drops it to `TierNone` for the rest of the session, logged loudly in the
transcript. Users need a way out of a boundary that is wrong; the answer is a
visible lever, not a silent hole.

## Phase 4 — The worktree rework

With a boundary in place, several long-standing worktree annoyances become
fixable in the same pass:

- **Isolation tier replaces the isolation mode.** `worktree.Create`'s
  `ModeAuto|ModeIsolated|ModeWorkspace` (`worktree.go:59-63`) is orthogonal to
  the containment tier — you can be contained in the workspace itself. The New
  Agent dialog's single "Sandbox" checkbox (`NewAgentDialog.cpp:205-211`,
  which today writes `isolation = isolated|auto`) becomes two controls:
  *where it runs* (worktree / workspace) and *what it can reach*
  (none / contained / hard). Today's checkbox conflates them and its label
  already promises containment it does not deliver.
- **Worktrees move out of the repo.** `<repoRoot>/.agentkate/worktrees/<id>`
  (`worktree.go:96`) puts every agent's tree inside the repo, which is why
  binding one requires binding its parents and why `git status` in the main
  checkout has to ignore `.agentkate/`. Moving to
  `$XDG_DATA_HOME/agentkate/worktrees/<project-hash>/<threadID>` makes the mount
  set trivial and the main checkout clean. **This is a migration** — existing
  records carry absolute `Worktree.Path` values — so it needs a one-time
  relocation pass with the same care `session/relocate.go` already shows.
- **Sibling invisibility falls out for free.** Once each agent's tree is bound
  alone, no agent can see another's. That is the reported bug, closed by
  construction rather than by instruction.

## Phase 5 — The checkpoint store

**Mechanism: shadow refs, not branches, not stashes.**

Every turn boundary (the `result` event — already the UI's turn boundary and
already tracked by `TurnTracker`) creates a commit object recording the full
worktree state, written to `refs/agentkate/checkpoints/<threadID>` as a chain.
Implementation is `git write-tree` on a temporary index plus `git commit-tree`,
so it captures **staged, unstaged and untracked** files without touching the
agent's own index, branch or `git log`.

New `core/internal/checkpoint/checkpoint.go`:

```go
type Checkpoint struct {
    ID        string    `json:"id"`        // the commit sha
    ThreadID  string    `json:"threadId"`
    TurnIndex int       `json:"turnIndex"`
    At        time.Time `json:"at"`
    Label     string    `json:"label"`     // first line of the turn's user message
    Files     int       `json:"files"`     // changed vs. the previous checkpoint
    // Pinned checkpoints survive pruning. The user pins "before the refactor".
    Pinned bool `json:"pinned"`
}

func Capture(wt worktree.Worktree, threadID string, turn int, label string) (Checkpoint, error)
func List(wt worktree.Worktree, threadID string) ([]Checkpoint, error)
func Diff(wt worktree.Worktree, from, to string) (string, error)
func Restore(wt worktree.Worktree, id string, paths []string) error
func Prune(wt worktree.Worktree, threadID string, keep int) error
```

**Why shadow refs beat the alternatives:**

- *A branch per checkpoint* pollutes `git branch`, `git log --all` and the
  Worktrees panel. Refs outside `refs/heads/` are invisible to all of them.
- *`git stash`* is a stack with its own semantics and it mutates the working
  tree. Checkpoints must be pure observation.
- *Copying the tree* is what the CLI's `~/.claude/file-history/<sessionId>/`
  does (verified: per-session directories of `<hash>@v2` blobs). Fine for the
  CLI; wasteful when we already have an object store that dedups by content.

**The CLI's file history is a complement, not a competitor.** It records files
the agent touched *outside* the worktree — which, once Phase 2 lands, should be
approximately none. Surface it read-only in the timeline as "files changed
outside this worktree", which is also a useful containment-leak detector.

**Retention:** unpinned checkpoints older than the last N turns (default 50) or
14 days are pruned, and `git gc` reclaims them. The rule mirrors the attachment
cache prune already shipped in plan 19 (age + cap, with a floor that never
deletes something very recent).

## Phase 6 — The timeline rail (UI)

- A narrow rail down the left edge of the transcript view
  (`ui/src/AgentPanel.cpp`, the `m_view` `QListView` at `:372`), one marker per
  turn boundary, painted by a delegate alongside `TranscriptDelegate`. Marker
  shows changed-file count; pinned markers are filled.
- Hovering shows a summary; clicking opens a **diff preview** in the existing
  `ui/src/DiffView.cpp` (already used for worktree diffs), showing
  `checkpoint → now`.
- **Restore is a two-step with a preview, never a one-click.** The dialog shows
  what will change, offers whole-tree or selected-paths, and states plainly that
  restoring does **not** rewind the conversation — the agent still believes it
  made those edits. That mismatch is the single most confusing thing about
  rewind and it must be said in the UI, not in a doc.
  - Optional and recommended: offer "restore files **and** fork the conversation
    from this turn", which is [plan 25](25-session-portability-and-fork.md)'s
    fork pointed at a checkpoint. That combination is the *actually* coherent
    rewind, and it is the reason plan 25 is sequenced before this one.
- A "Checkpoints" section in the Worktrees panel
  (`ui/src/WorktreeDashboard.cpp`) for cross-agent browsing.
- Flicker rule: the marker list is a `Reactive<QList<Checkpoint>>`; capture
  happens once per turn, so a naive rebuild would repaint the rail on every
  transcript row. Equality on `Checkpoint` covers every painted field.

## Phase 7 — Containment status, made visible

Containment that the user cannot see is containment they will not trust.

- A chip in the agent panel header: **Contained · 1 path** / **Uncontained** /
  **Contained (landlock — reduced)**, using `ChipPainter`
  (`ui/src/shell/ChipPainter.h`), click to open the reach list.
- The Cowork panel states whether the desktop sockets are inside the boundary.
- `agent.capabilities` grows the `contain.Availability()` map so the New Agent
  dialog can grey the `hard` tier with "podman not installed" rather than
  failing at launch.

## Verify

| Phase | What proves it |
|---|---|
| 1 | A *Result* subsection appended here answering all six criteria individually, with the S1 and S4 transcripts kept. Criterion 4's answer decides whether Cowork and `contained` compose. |
| 2 | `go test ./internal/contain/…`: `TestWrapNoneIsIdentity`; `TestWrapDeclaresEveryPathExactlyOnce`; `TestWrapRefusesWhenUnavailable` (no silent `TierNone` downgrade); `TestWrapPreservesScrubbedEnv` (a scrubbed `ANTHROPIC_API_KEY` must not reappear via `--setenv`). |
| 2 | Integration `TestContainedThreadCannotSeeSibling` (skipped without `bwrap`): two worktrees, start a contained thread in one, its Bash `ls` of the other's path fails. This is the reported bug, as a test. |
| 2 | `TestInterruptStillReachesContainedGroup` — the pgid/PID-namespace invariant, because breaking Interrupt is the most likely regression. |
| 3 | `TestHatchGrantReattachesAndPersists`: request → approve → thread re-attaches → codeword from before the re-attach still known → record carries the Path with `GrantedBy: "hatch"` and the reason. Same shape as `scripts/smoke-cowork-kimi.py`. |
| 3 | Manual: an agent calls `request_path`, the consent dialog shows its reason verbatim, denial leaves the thread running and reports NOT APPLIED. |
| 4 | `TestWorktreeRelocationMigratesRecords` — old absolute paths under `<repo>/.agentkate` resolve to the new location; a record whose old worktree is missing degrades to workspace instead of failing to load. |
| 5 | `go test ./internal/checkpoint/…`: capture includes untracked files; capture does not touch the agent's index (`git status` identical before and after); restore of selected paths leaves others alone; prune keeps pinned and keeps the most recent N. |
| 5 | `TestCheckpointsInvisibleToBranchAndLog` — `git branch`, `git log --all` and `git worktree list` are byte-identical before and after 20 checkpoints. |
| 6 | Qt test `ui/tests/CheckpointRailTest.cpp`: re-publishing an identical checkpoint list causes zero repaints. |
| 6 | Manual: 10-turn session → rail shows 10 markers → restore turn 3 → diff preview matches → files match → the transcript shows a "restored to turn 3" marker and the agent is *told*, not left to discover it. |
| 7 | Manual on a machine with userns disabled (`sysctl -w kernel.unprivileged_userns_clone=0`): launching a `contained` agent fails with a named reason; the New Agent dialog greys the tier. |

## Non-goals

- **A security boundary against a hostile model.** This is containment against
  *confusion and accident*, and a substantial obstacle to casual escape. It is
  not a sandbox for adversarial code: the agent runs as the user's uid, shares
  the network by default, and can reach anything explicitly granted.
  `docs/security-model.md` must say so in these words.
- **Windows and macOS.** User namespaces, bind mounts and landlock are Linux.
  The tier is simply unavailable elsewhere and `Availability()` says so.
- **Rewinding the conversation.** Phase 6 restores *files*. Rewinding the
  agent's belief is fork-from-checkpoint, which belongs to plan 25 and is
  offered here as a combined action rather than reimplemented.
- **Checkpointing outside the worktree.** The CLI's own file history is
  surfaced read-only. We do not snapshot the user's home.
- **Network isolation by default.** `Network: true` is the default because an
  agent that cannot reach the API is not an agent. `false` is offered per thread.

## Open questions for the user

1. **Default tier (program open question 1).** ~~Should `contained` become the
   default for new agents — or ship as opt-in for one release?~~
   **RESOLVED 2026-08-01: `contained` ships as a fourth tier and is the
   DEFAULT for new agents** (auto / isolated / workspace remain selectable).
   The user also flagged a future fifth tier: a podman-backed hard-isolation
   tier — keep the tier enum open-ended for it.
2. **Cowork vs. containment**, if spike criterion 4 fails. Prefer (a) mutually
   exclusive with a clear explanation, or (b) a "Cowork requires a wider
   boundary" tier that binds the session bus and Wayland socket and is honestly
   labelled as weaker?
3. **Worktree relocation.** Moving trees out of the repo is the right shape but
   it is a migration touching every existing record. Do it in this plan, or
   leave trees where they are and pay a slightly more complex mount set forever?
4. **Checkpoint granularity.** Per turn is the obvious unit. Per *file-writing
   tool call* is finer and much noisier. Per turn, with an "also checkpoint
   before each Edit" option?
