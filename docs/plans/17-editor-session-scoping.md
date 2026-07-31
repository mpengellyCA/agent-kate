# 17 — Editor Session Scoping (cross-project file leak on startup)

## Status: ✅ LANDED

Implemented as sketched, with one structural deviation and one scope call. See
"How the sketch met the real code" at the bottom for the full retrospective,
the live A/B dogfood evidence, and the probed KConfig facts.

## Goal

Fix the user-reported defect: **on startup, Agent Kate reopens files that were open
in unrelated projects.** Only files belonging to the currently open agent/project
should be restored. This is a bug-fix plan — investigation is complete, root cause
confirmed with live evidence, no code has been changed yet.

## Symptom

Launch Agent Kate into project A, and editor tabs from project B (last used under a
completely different project) pop open. The only filter applied on restore is "does
the file still exist on disk", so the foreign tabs always replay.

## Root cause (confirmed, with evidence)

### A. Session keys are ephemeral and global in tabs-by-agent mode — the primary bug

Open editor tabs are persisted per *group key*:

- `MainWindow::groupKey()` — `ui/src/MainWindow.cpp:1853-1859`:

  ```cpp
  QString MainWindow::groupKey() const
  {
      if (m_tabsByAgent) {
          return QStringLiteral("agent-%1").arg(m_activeAgentId);
      }
      return m_activeProject;
  }
  ```

- `m_activeAgentId` is an `int` handed out by `++m_counter` in `AgentDock`
  (`ui/src/AgentDock.h:210`, incremented at `AgentDock.cpp:694`, `:719`, `:1135`).
  The counter **resets to 0 every app run**, is **shared across all projects in the
  run**, and has **no relation to any stable thread identity**. Dormant threads get
  ids in async callback order (`AgentDock::restoreThreads`,
  `AgentDock.cpp:791-845`), so `agent-3` last run is not `agent-3` this run.

- Persistence writes to `KSharedConfig::openConfig()` (= `~/.config/agentkaterc`),
  group `[Editor][Sessions][<groupKey>]`, entries `openFiles` / `active` —
  `MainWindow::persistEditorSession()` at `ui/src/MainWindow.cpp:1939-1956`, fed by
  `EditorArea::groupKeys()` / `openFilePathsForGroup()` / `currentPathForGroup()`
  (`ui/src/EditorArea.cpp:847`, `:852`, `:866`).

- Restore: `MainWindow::restoreEditorSession(const QString &projectPath)` at
  `ui/src/MainWindow.cpp:1962-2001`, called from `onAgentActivated()`
  (`MainWindow.cpp:1849`). Note `projectPath` is `Q_UNUSED` at line 1964 — restore
  drifted away from project identity entirely. The only filter is
  `QFileInfo::exists(path)` (line 1991): **no project-containment check**.

**Live evidence from this machine's `~/.config/agentkaterc`:**

```ini
[Editor]
tabsByAgent=true
[Editor][Sessions][agent-1]
active=/home/mike/Dev/BDC-Kia/README.md
openFiles=/home/mike/Dev/BDC-Kia/README.md,/home/mike/Dev/BDC-Kia/.mcp.json,...
[Editor][Sessions][agent-10]
active=/home/mike/Dev/AgentKate/ARCHITECTURE.md
```

The user runs with `tabsByAgent=true`. On every launch, `MainWindow`'s ctor calls
`m_agent->addProject(project)` (`MainWindow.cpp:124`) → `AgentDock::addProject` →
`addAgent` (`AgentDock.cpp:535-548`), which always creates a starter agent with id
**1** → `restoreEditorSession` replays `[agent-1]`'s files — currently BDC-Kia's —
into whatever project was opened. That is the reported bug, exactly.

### B. Secondary defects (fix in the same pass)

1. **ID instability within one project** — even when all agents belong to one
   project, `agent-N` ids shift between runs (async restore order, later-created
   agents bump the counter), so tabs migrate between agents of the same project.
2. **Stale session groups are never GC'd** — `persistEditorSession` only writes live
   `groupKeys()`; obsolete `agent-N` groups (the rc file already has `agent-32` and
   several empties) persist forever and are eligible for replay by any future agent
   that lands the same id.
3. **Crash path** — persistence happens only in `closeEvent`
   (`MainWindow.cpp:2019`). A crash leaves the previous run's mismatched `agent-N`
   data as the next restore source.
4. **No path normalization** — `AgentDock::addProject` (`AgentDock.cpp:535`) does no
   `cleanPath`/canonicalization; the rc file already shows `[Agent][LastActive]`
   keys with and without trailing slashes. In project mode the same project can
   split into two session groups.
5. **No containment filter on restore** — even in project mode, any surviving path
   is reopened regardless of whether it belongs to the project.
6. **Worktrees not distinguished** — `groupKey()` never uses the worktree path
   (worktrees live under the project, e.g. `.agentkate/worktrees/t-*`; see
   `AgentDock::worktreePathForAgent` at `AgentDock.cpp:607`). All agents of a
   project share one tab group in project mode, or are keyed by the volatile int in
   agent mode.

## Test coverage

None today. `ui/tests/` covers FlowLayout, MarkdownUtil, PanelStack,
TranscriptModel, WidgetsPreview only — nothing exercises editor session
persistence/restore. The fix should add coverage (see step 5).

## Proposed fix

Two layers, both cheap; neither requires core (Go) changes.

### 1. Stable, scoped session keys

Replace the ephemeral `"agent-%1"` int with a key derived from stable identity:

- Base the key on the **normalized project path** always (project containment is
  the invariant the user cares about).
- In tabs-by-agent mode, suffix with the agent's **core threadId** (stable across
  runs, unlike the counter) — e.g.
  `agent:<cleanProjectPath>:<threadId>`. Sanitize for use as a KConfig group name.
- In project mode, keep `<cleanProjectPath>` (normalize with
  `QDir::cleanPath` + strip trailing slash, fixing B4).
- Thread identity must come from the core thread (the id used by
  `session.listThreads` in `AgentDock::restoreThreads`), not the UI counter.

### 2. Containment filter on restore

In `restoreEditorSession`, drop any path not under the restored agent's root
(project path, or the agent's worktree path when it has one). This makes restore
safe even against stale/foreign groups that were never cleaned up, and fixes B5.

### 3. Housekeeping

- On persist, delete `[Editor][Sessions]` groups that no longer correspond to a
  live key (fixes B2).
- One-time migration: wipe or ignore legacy `agent-N` groups (schema-version the
  group, e.g. write `version=2`; groups without it are skipped and removed). Old
  `agent-N` data is unrecoverable garbage anyway — the ints don't map to anything.
- Persist on agent switch / tab close in addition to `closeEvent` (mitigates B3;
  cheap — the data is small).

## Implementation steps

1. **`AgentDock`**: expose the stable threadId for the active agent alongside
   `m_activeAgentId` (`ui/src/AgentDock.h`, `AgentDock.cpp`). Confirm which existing
   member already carries the core thread id (roster items are restored from
   `session.listThreads`, so it exists somewhere around `AgentDock.cpp:791-845`).
2. **`MainWindow::groupKey()`** (`ui/src/MainWindow.cpp:1853-1859`): build the new
   scoped key; add a small helper `normalizedProjectPath()` used here and by any
   other rc-key call sites (`[Agent][LastActive]` keys benefit too — check
   call sites before widening scope; keep this plan's diff limited to editor
   sessions unless the same helper touches them trivially).
3. **`MainWindow::restoreEditorSession()`** (`ui/src/MainWindow.cpp:1962-2001`):
   use the (now meaningful) `projectPath` parameter — remove the `Q_UNUSED` — and
   filter `openFiles`/`active` to paths under the project or active worktree root.
4. **`MainWindow::persistEditorSession()`** (`ui/src/MainWindow.cpp:1939-1956`):
   prune stale groups; write `version=2`; skip/remove legacy `agent-N` groups.
5. **Tests** (`ui/tests/`): add a Qt test covering (a) same-project restore works,
   (b) foreign-project paths are filtered out, (c) legacy `agent-N` groups are
   ignored, (d) project paths with/without trailing slash map to the same key.
6. **Dogfood check**: launch with the current rc file (repro above: `agent-1` →
   BDC-Kia) into the AgentKate project; confirm zero foreign tabs and that real
   AgentKate tabs restore under the new key.

## Risks / notes

- KConfig group names have character restrictions — verify the chosen key format
  survives `KConfigGroup` naming (slashes are used for nesting; encode if needed,
  e.g. hash the path or percent-encode).
- First run after the fix restores nothing (old groups ignored) — acceptable
  one-time tab loss; far better than restoring the wrong project's files.
- Keep the change confined to `MainWindow` + `AgentDock` + a new test; no core
  (Go) work, no wire-protocol changes.

Size: **S–M**.

---

## How the sketch met the real code

### What shipped

| Step | Landed as |
| --- | --- |
| 1 — stable thread id | No `AgentDock` change was needed: `AgentDock::currentThreadId()` (`AgentDock.h:47`) already returns the shown panel's **core** thread id, and the roster switches the stack *before* it emits `agentActivated` (`AgentDock.cpp:78-88`), so the id is correct by the time `MainWindow::onAgentActivated` runs. |
| 2 — scoped keys | New unit `ui/src/EditorSession.{h,cpp}` owns key derivation; `MainWindow::groupKey()` is now three lines of policy over it. |
| 3 — containment filter | `EditorSession::read()` filters against the agent's roots; `restoreEditorSession(projectPath, worktreePath)` passes them and the `Q_UNUSED` is gone. |
| 4 — housekeeping | `EditorSession::write()` stamps `version=2` + the owning `project`; `EditorSession::sweep()` deletes legacy/unversioned, empty, and vanished-project groups. |
| 5 — tests | `ui/tests/EditorSessionTest.cpp`, 10 cases, registered in `ui/CMakeLists.txt`. |
| 6 — dogfood | Done as a live A/B against a copy of the real `agentkaterc` — see below. |

### Deviation: the rules live in their own unit, not in `MainWindow`

The plan put the logic in `MainWindow`, but `ui/tests/` builds each test from a
small list of real sources and `MainWindow.cpp` transitively pulls in the whole
app — it cannot be linked into a test. Since step 5 asked for coverage of
exactly these rules, they moved into `EditorSession`, a free-function namespace
in the shape of the existing `RecentProjects`. `MainWindow` keeps only the
wiring; `EditorSession` takes the `KConfigGroup` as a parameter, so the tests
drive the real read/write/sweep path against a throwaway config file.

### Deviation: pending keys + a group rename (defect the sketch didn't cover)

A freshly created agent has **no thread id** until its session starts, so there
is no stable key to put its tabs under. Keying it on the counter would reopen
the leak. So a thread-less agent gets a `pending:<project>:<agentId>` key that
`isPersistable()` rejects — it can never reach the config — and
`MainWindow::adoptPendingEditorGroup()` re-keys the group to
`agent:<project>:<threadId>` the moment the id lands, via the new
`EditorArea::renameGroup()`.

That hook is called from **both** `activeThreadChanged` and `onAgentActivated`.
Only the first was obvious; the second is required because
`activeThreadChanged` fires for the *shown* agent only, so an agent whose
session started while another agent was on screen would otherwise come back to
an empty tab group with its tabs stranded under a key nothing reads — an in-run
tab loss the sketch would have shipped.

### Scope call: `[Agent][LastActive]` keys were left alone

The plan floated reusing the normalizer for the `[Agent][LastActive]` groups
(which really do exist both with and without trailing slashes). Not done: those
keys are the *same strings* the UI sends to the core as `project`, and
`session.listThreads` matches them **exactly** (`core/internal/session/session.go:320`).
Normalizing the project path at the `AgentDock::addProject` source would orphan
every thread recorded under a trailing-slash path. Normalizing only the editor
key (as done here) is safe because that key is UI-local. Splitting the LastActive
keys needs a core-side path-normalization migration and is out of scope for a
UI-only bug fix.

### Probed facts

- **KConfig group names tolerate more than expected.** With `sanitize()` stubbed
  to the identity function, `groupNamesSurviveKConfig` still passed for a group
  named after a path containing `[`, `]`, `=` and spaces — KConfig escapes group
  headers itself. The percent-encoding was kept anyway: it makes the key format
  independent of KConfig's escaping rules, and it encodes `:` so the
  `agent:<project>:<threadId>` split stays unambiguous for a project path that
  contains a colon (covered by `projectForKeyRoundTrips`).
- **The bug reproduces exactly as documented**, and the fix kills it — live A/B
  below.

### Dogfood evidence (step 6), live A/B on the real rc file

Both runs: the *real* `~/.config/agentkaterc` copied into an isolated
`XDG_CONFIG_HOME` (`tabsByAgent=true`, `[Editor][Sessions][agent-1]` holding four
BDC-Kia files), app launched offscreen into `AgentKate/ARCHITECTURE.md`, then the
core asked `coop.listOpenFiles` — i.e. the files the UI actually had open.

```
pre-fix build (HEAD, stashed diff):
  /home/mike/Dev/AgentKate/ARCHITECTURE.md
  /home/mike/Dev/BDC-Kia/.claude/settings.local.json   ← the reported bug
  /home/mike/Dev/BDC-Kia/.mcp.json                     ←
  /home/mike/Dev/BDC-Kia/README.md                     ←
  /home/mike/Dev/BDC-Kia/config.json                   ←

fixed build:
  /home/mike/Dev/AgentKate/ARCHITECTURE.md             ← only this
```

After the fixed run the isolated rc held **0** of the 33 legacy
`[Editor][Sessions][agent-N]` groups (swept), and no BDC-Kia path anywhere under
`[Editor]`. As predicted in "Risks / notes", the first run after the fix restores
nothing — the one-time cost of retiring the legacy corpus.

### Verification

- `ctest --test-dir build` — 5/5 pass, including the new `EditorSessionTest`.
- Mutation-checked, so the guards are not decorative: dropping the containment
  filter fails `filtersForeignProjectFiles`; dropping the schema-version check
  fails `ignoresLegacyAgentGroups`.
- `go build ./... && go vet ./... && go test ./...` green (untouched by this
  diff — it contains no `core/` changes), plus `scripts/smoke-agent.py` PASSED as
  a binary sanity check. The rest of the smoke suite drives core/harness paths
  this diff does not reach and was deferred to the plan 16 phases.
