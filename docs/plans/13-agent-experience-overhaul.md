# 13 — Agent Experience Overhaul (chat flow, lifecycle, panels)

> **Status: planned — not started.** Eleven phases for one targeted major release,
> ordered so regressions and workflow-blockers land first. Phases are largely
> independent; land them sequentially (1→11), one commit per phase, each built green
> (`scripts/build.sh`), `ctest --test-dir build` passing, and verified live before
> moving on.

## Goal

Fix the regressions and friction in the agent chat flow (copy/selection, interrupt,
save, attachments), add the missing power features (fork, tool inspector), and bring
the supporting panels (roster, Git Log, Worktrees, Cowork) up to the same card-based,
plain-language standard plan 12 set. Qt/KDE-native first: palette-driven delegates,
QDialogs, KF6 components; custom widgets only where no framework primitive fits.

## Working conventions (read once, applies to every phase)

- **Build/test:** `scripts/build.sh` (Debug into `./build`), `ctest --test-dir build`.
  End-to-end smokes in `scripts/smoke-*.py` (need built `./build/akcore` + authed `claude`).
- **Two processes:** `agentkate` (C++/Qt6/KF6, `ui/src`) ↔ `akcore` (Go, `core/`) over
  JSON-RPC on a Unix socket. New cross-process feature = RPC method registered in
  `core/cmd/akcore/handlers.go`, handler delegating to `core/internal/...`, and a
  `CoreClient::call(...)` in the UI. See `ARCHITECTURE.md`.
- **Async reply callbacks** from `CoreClient::call` must guard against the receiver
  being destroyed (capture `QPointer`, check before touching `this`) — unguarded
  callbacks are a known SIGSEGV class in this codebase.
- **Theming:** palette-only via `ThemeManager::palette()` / `ThemeManager::colors()`
  semantic colors (`ui/src/theme/`). Never Fusion style, never app-wide stylesheets.
  Delegates use QPalette roles so KDE schemes keep working.
- **Text into QTextDocument/setMarkdown:** route model-sourced text through
  `MarkdownUtil::neutralizeMarkdownRawHtml` — raw `<T>` in text otherwise eats content.
- **Responsive rows:** reuse `ui/src/shell/FlowLayout` and `ElidingLabel` for any chip
  row or long label.
- **Polled/refreshed state:** wrap in `Reactive<T>` with equality guards (see
  `WorktreeDashboard::m_snapshot`) so identical payloads don't repaint.
- All user-visible strings through `i18n()`; plain language per plan 12 (no jargon).

---

## Phase 1 — Chat text selection & copy (regression) — **M**

**Why.** Since transcript virtualization (commit `ab4cab7`) you cannot select a
substring of an agent response; only whole-block copy survives. This is the most-felt
daily regression.

**Now.** Transcript is a `QListView` with `NoSelection` (`ui/src/AgentPanel.cpp:265`)
painted by `TranscriptDelegate` via per-row QTextDocument (`ui/src/TranscriptDelegate.cpp`,
`buildBodyDoc()` ~line 92, hit-testing in `editorEvent()` ~line 394). Copy affordances:
double-click copies the whole message (delegate ~463), context menu "Copy message" /
"Copy N code blocks" (`AgentPanel.cpp:1443-1478`), tool-card copy glyph. Row HTML is in
`HtmlRole`, raw markdown in `PlainRole` (`ui/src/TranscriptModel.h:62-84`).

**Design — in-place selectable overlay (framework-native).** Give `TranscriptDelegate`
a `createEditor()` that returns a frameless, read-only `QTextBrowser` loaded with the
row's `HtmlRole` and the *same* document setup as `buildBodyDoc()` (same width, font,
default stylesheet) so glyph positions match the painted row exactly. On click inside a
Message row's body, `AgentPanel` calls `openPersistentEditor(index)` (closing any
previously open one); the overlay covers only the body-text rect (`updateEditorGeometry`),
`viewport()->setAutoFillBackground(false)` + `QFrame::NoFrame` so the painted card shows
through. Native selection, drag, double-click-word, Ctrl+C, and context-menu copy then
work for free. `setOpenLinks(false)` and forward `anchorClicked` to the existing link
handler. Close the editor on Esc, on click outside, and when the row's data changes
(streaming rows: don't open editors on the in-flight last row).

**Steps.**
1. Extract the QTextDocument configuration from `layoutRow()`/`buildBodyDoc()` into a
   shared helper both `paint()` and the editor use (single source of metrics).
2. Implement `createEditor`/`setEditorData`/`updateEditorGeometry`; wire open/close in
   `AgentPanel` (click on Message body opens; scroll keeps it attached — persistent
   editors move with the view; verify no height mismatch).
3. Retire double-click-copies-all (it conflicts with native word-select); keep the
   context-menu copy actions unchanged.
4. Manual verify: select mid-sentence in an old and a streaming-finished message; links
   still clickable; find-highlight still paints; resize while an editor is open.

**Accept.** Arbitrary substring of any user/agent message can be mouse-selected and
Ctrl+C'd with no visual jump when the overlay opens; existing copy paths still work.

---

## Phase 2 — Interrupt vs Stop semantics — **M–L** (core + UI)

**Why.** Interrupt today aborts the turn **and kills the process** (stdin close), so
"continue after interrupt" pays a full resume; Stop is graceful-but-slow and users reach
for it when they mean interrupt. Wanted: **Interrupt** = cancel the current request,
keep the session hot, keep typing. **Stop** = terminal — compact and close the agent.

**Now.** `Supervisor.Interrupt` (`core/internal/agent/agent.go:473-525`) writes the
stream-json `control_request{subtype:interrupt}` frame, **then closes stdin**, then
escalates SIGINT→SIGKILL on the process group after 2s+2s; `reap()` marks the thread
dormant ("interrupted (resumable)", agent.go:739). `agent.stop`
(`core/cmd/akcore/handlers.go:337-350`) runs hot compaction synchronously first
(`runHotCompactIfConfigured`, `agents.go:248-287`), then `Stop()` closes stdin with a 5s
kill backstop (agent.go:433-457) and leaves the thread dormant. UI: stop/interrupt
buttons `AgentPanel.cpp:699-712`, handlers at 2094/2107. Statuses:
running/dormant/archived (`core/internal/session/session.go:24-28`).

**Design.**
- **Interrupt keeps the process alive.** Drop the `t.stdin.Close()` from `Interrupt()`.
  Spike first (pattern of plan 04's spike): drive `claude -p --input/output stream-json`
  by hand, send the interrupt frame *without* closing stdin, confirm the CLI acks
  (`control_response`), emits a `result` for the aborted turn, stays resident, and
  accepts a subsequent user message. If the resident CLI misbehaves, fall back to
  close+auto-resume but keep the UI contract below.
- **New lifecycle phase `turn_aborted`** (process alive): emitted when the ack/result
  arrives after an interrupt. UI resets to idle, composer stays enabled, next
  `agent.send` goes down the same stdin — no resume cost.
- **Escalation backstop stays** for the hung-tool case (bad command): if no
  ack/result within ~3s, SIGINT the process group as today; `reap()` marks dormant.
  Pair this with **auto-resume on send**: `AgentPanel::deliverMessage` to a dormant
  thread triggers the existing resume path and queues the message, so even the
  escalated case feels like "interrupt and continue".
- **Stop becomes terminal.** Relabel the button "Stop & close" (tooltip: compacts the
  conversation and closes the agent). Flow: hot-compact (existing) → `Stop()` →
  on exit mark the thread **archived** (reversible via the Sessions browser) and remove
  it from the live roster — reuse the existing close-agent path in `AgentDock`. Confirm
  dialog only when a turn is in flight.
- **UI affordances:** interrupt is the prominent action while a turn runs (add Esc as
  shortcut when composer focused); stop moves to a less prominent position/menu.

**Steps.** Spike → core changes (`Interrupt()`, run-loop parsing of `control_response`,
`reap()` only reports "interrupted" when the process actually died, new lifecycle
event) → UI state machine (`m_idle`/`m_dormant` handling of `turn_aborted`,
auto-resume-on-send, button relabel/reorder) → **update `scripts/smoke-interrupt.py`**
(it currently asserts interrupt ⇒ dormant; new assertion: interrupt ⇒ alive + follow-up
answers without resume; keep an escalation test with a sleeping bash tool) → live
verify both paths.

**Accept.** Interrupt mid-generation halts <1s and a follow-up sends into the same
process; interrupt during a hung `sleep 600` bash call recovers via escalation and the
next send auto-resumes with context; Stop & close compacts, archives, and clears the
roster entry.

---

## Phase 3 — Editor: reliable Ctrl+S, autosave by default, auto-show editor — **S–M**

**Why.** Ctrl+S doesn't save when typing in the editor; user wants autosave on by
default with a toggle; opening a file while in chat-only layout opens it invisibly.

**Now.** Save is one window-level `KStandardAction::save` → `MainWindow::onSave`
(`ui/src/MainWindow.cpp:573`, 1770-1792) → `EditorArea::saveCurrent()` →
`documentSave()`. Two independent root-cause candidates, both real:
1. **Ambiguous shortcut.** KTextEditor views create their own internal `file_save`
   action with Ctrl+S; with focus in the editor both actions match and Qt disables the
   key ("Ambiguous shortcut overload" on the console). Views are created bare at
   `ui/src/EditorArea.cpp:492` with no shortcut reconciliation.
2. **Format-on-save trap.** `onSave` (MainWindow.cpp:1777-1784) goes async through
   `m_lsp->formatDocument(view, cb)` when the server "canFormat"; if the callback never
   fires (hung/dead server), **nothing saves, silently**.
No autosave exists (the only "autosave" is the composer draft). View modes are
"editor"/"chat"/"split" via `MainWindow::applyCentreMode` (MainWindow.cpp:1572-1612);
all file-opens funnel through `EditorArea::openFile` (EditorArea.cpp:366) and none
switch the mode, so in chat mode the file opens hidden.

**Design & steps.**
1. Reproduce with focus in a KTE view and confirm the console warning. Fix: after
   `doc->createView(...)` (EditorArea.cpp:492), clear the view's conflicting internal
   shortcuts (`view->actionCollection()->action("file_save")` at minimum; scan for
   other collisions with window actions) so the MainWindow action owns Ctrl+S; set the
   MainWindow save action to `Qt::ApplicationShortcut` context.
2. Make `onSave` non-swallowing: start a ~1.5s single-shot fallback that calls
   `saveCurrent()` directly if the format callback hasn't completed; always emit status
   feedback ("Saved x.cpp" / "Save failed: …").
3. **Autosave (default on).** In `EditorArea`: debounced save (~1s after last edit) per
   document with a local URL + modified + not read-only; also save on view focus-out
   and app deactivation. Autosave saves *without* the LSP format step (manual Ctrl+S
   still formats) so the cursor never jumps. Toggle: Options ▸ "Autosave files"
   checkable action, persisted in KConfig `[Editor] autosave` (default true). Skip
   documents showing the modified-on-disk reload banner.
4. **Auto-show editor.** `MainWindow::ensureEditorVisible()`: if centre mode is
   "chat", `applyCentreMode("split")` (keeps the conversation on screen while showing
   the file — Ctrl+E gets full editor). Call it from the user-initiated open
   connections only (ProjectTree, search, LSP, outline, problems — MainWindow.cpp:167,
   175, 357, 369, 457), *not* session restore or startup.

**Accept.** Ctrl+S with editor focus saves (with a dead LSP too); edits persist ~1s
after typing stops with autosave on and never with it off; opening a file from chat
layout makes it visible in split.

---

## Phase 4 — Attachments: named, visible, viewable — **M**

**Why.** A sent message renders only "📎 N attachment(s)"; you can't see what was
attached, its name, or its content.

**Now.** Composer chips already show name+thumbnail pre-send
(`AgentPanel.cpp:2055-2092` `rebuildAttachChips`). Each attachment is a QJsonObject
`{name, kind(image|text), path, mediaType?, dataB64?|text, outside?}` built in
`ui/src/AttachmentBuilder.cpp`. On send, `addYouCard()` bakes the count string into the
message HTML (`AgentPanel.cpp:1829-1831`) and the full array goes to `agent.send`;
TranscriptModel rows carry no attachment data.

**Design.**
- Add an `AttachmentsRole` (compact QJsonArray: name, kind, path, mediaType — never
  dataB64) to Message items in `TranscriptModel`; `appendMessage` overload; store it
  when sending.
- `TranscriptDelegate`: paint a chip row under the message body for messages with
  attachments (chip pattern from `AgentCardDelegate`; icon by kind); hit-test chips in
  `editorEvent` → signal with the attachment object.
- Open behavior: `kind=="image"` → lightweight preview dialog reusing `ImageView`
  (fall back to a "file moved" note if the path is gone); `kind=="text"` → emit a
  file-open request → `MainWindow` → `EditorArea::openFile` + `ensureEditorVisible()`
  (Phase 3). Tooltip shows full path + outside-workspace marker.
- **Replay:** investigate what `agent.transcript` JSONL preserves of a sent message's
  attachments (they become content blocks in the user message). If names/paths aren't
  recoverable, persist a small per-thread sidecar (thread-keyed JSON next to the
  session record in `core/internal/session`) written by the core on `agent.send`, and
  have the replay path consult it. Chips must survive restart/resume.

**Accept.** A sent message shows one named chip per attachment (icon per kind);
clicking an image shows it, clicking a text/file attachment opens it in the editor;
chips still there after closing and resuming the agent.

---

## Phase 5 — Tool-call inspector modal — **M**

**Why.** Tool rows expand to raw JSON + truncated plain text; there's no comfortable
way to *read* a big Edit, a long Bash output, or a Read result.

**Now.** Tool rows: collapsed header, expand shows input JSON (pretty-printed,
`AgentPanel.cpp:2438`) and result clipped to 4000 chars shown / 128 KB stored
(`kToolResultDisplayClip`/`kToolResultStoreCap`, AgentPanel.cpp:78-79). Full result in
`ToolFullResultRole`, input in `ToolDetailRole` (`ui/src/TranscriptModel.h:49-84`).
`DiffView` (`ui/src/DiffView.h`) is a reusable unified-diff renderer with syntax
highlighting and a side-by-side toggle.

**Design.** New `ui/src/ToolInspectorDialog.{h,cpp}` (resizable QDialog, remembers
size), opened from a new "open" glyph beside the tool card's copy glyph and from the
tool row context menu. `QTabWidget`:
- **Overview** — tool-aware, driven by a small registry keyed on tool name:
  `Bash` → command (mono, wrapped) + description + output in console styling;
  `Read` → clickable file path + content; `Edit`/`Write`/`MultiEdit` → synthesize a
  unified diff from old/new strings and render in **DiffView** (side-by-side works for
  free); `Grep`/`Glob` → pattern + hit list; fallback → QFormLayout of the input's
  top-level fields.
- **Input** — full JSON, monospace with KSyntaxHighlighting (JSON definition).
- **Result** — `ToolFullResultRole`, monospace, wrap toggle, inline find bar, copy
  button, and a note when the stored result was capped at 128 KB.
File paths in the overview open via the Phase 3/4 file-open path.

**Steps.** Dialog + registry → delegate glyph & hit-test → context-menu entry → verify
against a real session (Bash, Edit, Read, an MCP tool for the fallback).

**Accept.** Any tool row opens a modal where an Edit reads as a proper diff, Bash output
is scrollable/searchable, and unknown tools degrade to a clean key-value + JSON view.

---

## Phase 6 — Fork an agent (model/effort, keep context) — **M** (core + UI)

**Why.** No way to take a conversation and continue it on a different model or effort
without losing context.

**Now.** No fork anywhere. All needed per-agent state lives on the session `Record`
(sessionID, model, effort, permissionMode, project, provider snapshot, cowork flag —
`core/internal/session/session.go:32-38`). The installed CLI supports
`--resume <id> --fork-session` (verified: creates a new session ID from the old
context). Spawn args are built in `core/internal/agent/agent.go:289-339`; tier→id
mapping in `resolveModel` (`core/cmd/akcore/agents.go:457-470`); worktrees are created
per-thread in `core/internal/worktree` (branch `agentkate/<threadId>`).

**Design.**
- **Core:** RPC `agent.fork {threadId, model?, effort?, title?}` → read source Record
  (require a sessionID; interrupt/require-idle if a turn is in flight), mint a new
  threadID, create a new isolated worktree **branched from the source worktree's
  HEAD** (dirty uncommitted changes are not copied — state this in the UI subtext),
  then start via the existing start path with `--resume <sourceSessionID>
  --fork-session` plus the model/effort overrides; copy permissionMode, provider,
  cowork flag. Ensure the run loop captures the *new* session ID from the CLI init
  event into the fork's Record (mirror the resume wiring in `agents.go:130`).
- **UI:** "Fork…" in the AgentPanel header menu + roster context menu → small dialog
  (model tier picker + effort picker prefilled from the source, name defaulting to
  "Fork of <title>", reuse `NewAgentDialog` row patterns) → on reply activate the new
  agent (the started lifecycle event flows through the existing roster wiring).
- Add `scripts/smoke-fork.py` modeled on `smoke-resume.py`: tell agent A a secret,
  fork to a different tier, assert the fork recalls it and the original still works.

**Accept.** Forking a mid-conversation agent to another model yields a second roster
entry that remembers the conversation; the original agent is untouched.

---

## Phase 7 — Agent roster cards v2 — **M**

**Why.** Cards show one derived status line; wanted: two lines of actual chat preview,
a stronger card look, and status by color + symbol.

**Now.** `AgentCardDelegate` paints dot + bold title + one muted subtitle + #N badge +
tag chips on a QTreeWidget (`ui/src/AgentCardDelegate.cpp`, roles in the header at
12-26). Subtitle text is composed in AgentPanel (status words + branch + cost +
tokens) and wired through `AgentDock::wireAgentPanel` (`AgentDock.cpp:650-669`).
Status is a raw hex dot color; no icons, no preview, no timestamps.

**Design.**
- **New roles:** `Preview` (last chat line, prefixed "You: " for user messages —
  emitted by AgentPanel on every message append), `LastActivity` (epoch), and a proper
  `Status` enum (Working / Idle / NeedsInput / Dormant / Error) replacing the
  hex-string-only `Dot` role as the source of truth.
- **Card layout:** rounded card background (AlternateBase fill, 1px border, ~4px
  inter-card gap) — line 1: status badge (symbol + semantic color: Working ● animated
  in `agentRunning` green, Idle ○ muted, NeedsInput ⚠ amber, Dormant ⏸ grey+italic,
  Error ✖ negative) + bold title + relative time right-aligned; lines 2-3: two-line
  elided preview via QTextLayout (muted); line 4: existing #N badge + tag chips. The
  old composed subtitle (branch/cost/tokens) moves to the tooltip.
- Working animation: a roster-owned QTimer invalidating only Working rows (~10 fps arc
  sweep), stopped when none are working.
- Keep everything font-metric-relative and palette-driven; update `sizeHint`.

**Accept.** Each agent reads as a distinct card with a two-line preview of the latest
exchange; you can tell working/idle/needs-input/dormant at a glance from color+symbol
without reading text.

---

## Phase 8 — Git Log v2 (GitKraken-lean) — **L**

**Why.** The log is a flat table with a fixed detail pane; wanted: branch/path/text
filtering, a real commit modal with tabbed views, richer visuals.

**Now.** `ui/src/git/LogViewer` + `LogModel` + `LogGraphDelegate` (lanes) +
`RefChipDelegate`; 200/page, 5000 cap, selection feeds `CommitDetailPanel`
(`git.commit.detail` + `git.commit.diff`); copy sha/subject/patch in the context menu;
refresh via `git.log.invalidated`. Core plumbing already supports `branch` and `path`
params on `git.log` and per-file diffs via `git.commit.diff{path}`
(`core/cmd/akcore/handlers.go:1691-1789`). **Not plumbed:** branch list, search,
checkout/rebase/push/stash — keep this phase read-only; do not grow core git mutation
surface here.

**Design.**
- **Toolbar:** branch selector (new small RPC `git.branches {threadId|repoRoot} →
  [{name, current, isRemote}]` shelling `git branch -a --format`), path filter box
  (already-plumbed `path` param), and a search field — client-side filter over loaded
  subject/author first; add a `query` param mapping to `git log --grep` only if that
  proves insufficient.
- **Commit modal:** double-click (and context-menu "Open commit…") opens a new
  `CommitDetailDialog` — header (short sha copyable, author initials chip, absolute +
  relative date, ref chips), then tabs: **Changes** (file list with status colors and
  ±line counts; selecting a file loads its scoped diff) and **Patch** (full unified
  diff); both render through the existing `DiffView`, so inline/side-by-side toggle
  comes free. The embedded `CommitDetailPanel` stays for single-click browsing.
- **Visual pass:** lane colors from `ThemeManager::colors()`, slightly taller rows,
  relative dates with absolute tooltip, author initials chip, hover row highlight.

**Accept.** You can scope the log to a branch or path, search subjects, and double-click
any commit into a tabbed modal where per-file diffs render side-by-side.

---

## Phase 9 — Worktrees panel v2 — **M**

**Why.** Today it's a raw 8-column table; the useful signals (dirty, conflicts,
ahead/behind, which agent) don't pop, and there's no way to *see* a worktree's diff.

**Now.** `ui/src/WorktreeDashboard` renders WorktreeRow columns (#, branch, isolation,
ahead, behind, remote, dirty, path) from `git.snapshot`, 20s poll + `git.invalidated`
shortcut, with toolbar actions Commit/Land/PR/Discard/Cleanup and context-menu Remove
(all already wired to core RPCs). In-place merge model keyed by threadId
(`WorktreeDashboard.cpp:152-200`).

**Design.**
- Replace the table with a card list (QListView + new `WorktreeCardDelegate`, same
  pattern as the roster): line 1 — `#N branch` bold + agent title + status pills
  (↑ahead ↓behind vs base, ↑↓ remote, ✎ dirty count amber, ⚠ conflicts in negative
  red); line 2 — elided path + "updated Xs ago". Card border tint by state: conflicts
  negative, dirty amber, clean neutral. Pills via the chip-painting helper (share with
  Phases 4/7 — extract a small `ChipPainter` into `ui/src/shell/`).
- **Worktree diff modal:** double-click a card → read-only dialog assembling the
  worktree's current diff (same `git.diff` RPC `CommitDialog` uses) with a file list +
  `DiffView`, plus buttons handing off to the existing Commit…/Land/PR dialogs.
- Keep: toolbar actions, selection re-targeting, Reactive snapshot guard, in-place
  merge (adapt to roles instead of columns), project scoping via `setActiveProject`.
- Empty state: friendly "No agent worktrees in this project yet" note.

**Accept.** A glance shows which worktrees are dirty/conflicted/ahead; double-click
shows the actual diff; every previous action still works.

---

## Phase 10 — Cowork control centre — **M**

**Why.** The panel mixes basic controls with debug surfaces (audit stream, pointer
tuning) and renders permissions as a dense checkbox column — confusing for a basic
user. Wanted: device-control-center feel — large descriptive toggles, advanced stuff
behind buttons.

**Now.** `ui/src/cowork/CoworkPanel.cpp`: status KMessageWidget (~101), active-agent
row (~111), checkbox capability list built from `cowork.getPolicy` (~127-356), pointer
speed/accuracy/settle controls (~139-195), browser launcher (~201-239), grants
QTreeWidget (~242-256), kill-switch (~259), inline audit log (~265-272). Consent
dialogs and the core grant/policy/audit model are solid — **do not touch
`core/internal/cowork` or the consent dialogs**; this is a panel-layout phase.

**Design.**
- **Keep at top:** status banner; active-agent row; the kill-switch stays prominent
  (full-width, below the tiles).
- **Capability tiles:** replace the checkbox column with a FlowLayout grid of large
  checkable tiles (new `CapabilityTile` widget: theme icon, human title, one-line
  description, clear on/off state via palette Highlight; ~140px wide). Read-tier tiles
  plain; control-tier tiles get a warning accent border + ⚠ and the existing tooltip.
  Human copy per capability (e.g. `window_list` "See open windows", `screenshot`
  "Take screenshots", `a11y_read` "Read app contents", `screencast` "Watch the
  screen", `launch_browser` "Open a browser", `input_inject` "Type and press keys as
  you", `pointer_control` "Move and click the mouse as you"). Toggle → existing
  `cowork.setPolicy`.
- **Grants as sentences:** replace the table with a widget list — "**Agent X** can
  *take screenshots* on *whole screen* until *14:32* [Revoke]" — one revoke button per
  row; R2 rows carry the warning icon. Same `cowork.listGrants`/`cowork.revokeGrant`.
- **Advanced behind buttons:** a small toolbar — "Activity log…" (dialog wrapping the
  current audit view + kind filter), "Pointer settings…" (dialog with the
  speed/accuracy/settle controls), "Browser tools…" (dialog with the launcher). Panel
  body then contains only: status, agent row, tiles, grants, kill-switch.

**Accept.** A non-technical user can read the panel top-to-bottom and understand what
agents may do and how to stop them; no raw logs or tuning sliders visible by default;
all previous functions reachable through the three dialogs.

---

## Phase 11 — File Browser: Project / Worktree tabs — **S–M**

**Why.** The Files panel always shows the project root; there's no way to browse the
selected agent's isolated worktree, where the agent's actual edits live.

**Now.** `ProjectTree` is a single-root browser: `setRoot()`
(`ui/src/ProjectTree.cpp:311`) points a QFileSystemModel at one path, called with the
*project* path from `MainWindow::onAgentActivated` (`ui/src/MainWindow.cpp:1728`) and
the `projectFocused` connect (MainWindow.cpp:432). The worktree path is already on the
wire — every `_lifecycle` event carries `workdir`, `isolated`, `branch`
(`core/cmd/akcore/handlers.go:2083-2100`) — but the UI only reads `isolated`/`branch`
(`ui/src/AgentPanel.cpp:~2545`) and drops `workdir`. Git-status tinting comes from
`git.snapshot`, whose per-thread entries are keyed by worktree paths already.

**Design (UI-only).**
- **Scope tabs:** a compact `QTabBar` (document mode, no expanding) above the existing
  ProjectTree header with two tabs — **Project** and **Worktree**. ProjectTree gains
  `setRoots(projectPath, worktreePath)`; an empty `worktreePath` disables the Worktree
  tab (agent running non-isolated in the workspace, dormant without a worktree, or a
  project row selected in the roster).
- **Sticky + persistent, global:** the selected tab is one global preference persisted
  to KConfig `[Files] scope` (project|worktree), restored on startup. When scope is
  `worktree` but the current agent has none, *display* the project root with the tab
  disabled without overwriting the preference — selecting an agent that has a worktree
  snaps back to its worktree.
- **Plumbing the worktree path:** `AgentPanel` stores `workdir` from lifecycle events
  (`started`/`resumed`/`promoted`) and exposes it (accessor + change signal);
  `AgentDock` forwards it alongside `agentActivated` so
  `MainWindow::onAgentActivated` calls `m_tree->setRoots(projectPath, workdir)`. For
  dormant agents restored at startup, verify `session.listThreads` serializes the
  record's embedded worktree (path + isolated) and pass it through the same way; a
  mid-session promote updates the tree live via the `promoted` event's `workdir`.
- **Out of scope:** Terminal, Search, and Git Log scoping are unchanged (they have
  their own scoping rules); the tab only drives the file browser root.

**Steps.** `setRoots` + QTabBar in ProjectTree (preserve per-root expansion state when
flipping tabs if cheap — otherwise just re-root) → workdir plumbing
(AgentPanel/AgentDock/MainWindow, dormant-restore path included) → KConfig persistence
→ verify git-status emblems render for worktree files and `revealPath`/sync-with-editor
behaves in both scopes.

**Accept.** With an isolated agent selected, the Worktree tab shows its worktree files
(with git emblems); a workspace-mode agent leaves the tab disabled showing the project;
the chosen tab survives switching agents and a full app restart.

---

## Suggested landing order & sizing recap

| Phase | What | Size | Touches |
|---|---|---|---|
| 1 | Chat selection/copy | M | UI |
| 2 | Interrupt/Stop semantics | M–L | Core + UI + smoke |
| 3 | Save / autosave / auto-show editor | S–M | UI |
| 4 | Attachments | M | UI (+ small core sidecar if needed) |
| 5 | Tool inspector | M | UI |
| 6 | Fork agent | M | Core + UI + smoke |
| 7 | Roster cards v2 | M | UI |
| 8 | Git Log v2 | L | UI + 1 small RPC |
| 9 | Worktrees v2 | M | UI |
| 10 | Cowork control centre | M | UI only |
| 11 | File Browser project/worktree tabs | S–M | UI only |

Phases 1, 4, 5 all touch `TranscriptDelegate`/`TranscriptModel` — land them in order to
avoid conflicts. Phases 7 and 9 share the chip-painting helper (extract it in whichever
lands first). Everything else is independent (Phase 11 can land any time — it only
touches ProjectTree and the agent-activation wiring).
