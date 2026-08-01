# 27 — KDE presence: one action model, a tray item, and global shortcuts

**Status: PLANNED.** Covers IDEAS #12 (system tray + close-to-tray) and #13
(global shortcuts), plus the KActionCollection / KXmlGui refactor #13 depends on
and the whole program benefits from. Program context:
[20-approved-features-program.md](20-approved-features-program.md).

**Size: M–L.** §1 is S–M, §2 is M, §3 is S.

**Phase 1 of this plan is the first thing the whole program should do.** Not
because global shortcuts are urgent, but because five of the seven clusters add
new user-visible actions, and every action born outside the collection is one
more shortcut nobody can reconfigure and one more command the palette cannot see.

## Why

**AgentKate is a good KDE citizen in its shell and absent from the desktop.** It
uses `KMainWindow`, `KHamburgerMenu`, `KStandardAction`, `KHelpMenu`,
`KMessageWidget`, KConfig-backed state, `KIO`, `KParts`, `KColorScheme`, and its
signature theme is correctly palette-only on Breeze. `KNotification`,
`KDBusService` and window-geometry restore all land with the current work. What
is left is the part that matters most for an app whose premise is *parallel
background agents*:

**1. Closing the window kills everything.** `MainWindow::closeEvent`
(`ui/src/MainWindow.cpp:2276-2309`) runs `ShutdownDialog`, which stops and
compacts every running agent — because the app has nowhere to live without a
window. For a tool whose entire pitch is "start a long refactor and go do
something else", the only way to get the window out of the way is to end the
work. That is the single biggest missing Plasma surface.

**2. Nothing brings you back.** The workflow is: *I am in Dolphin/Firefox/
Konsole, an agent just pinged me, I need to get back and answer.* Today that is
alt-tab archaeology. `grep -rn KGlobalAccel ui/` returns nothing.

**3. Shortcuts are hardcoded and unreconfigurable.** `MainWindow` builds ~30
raw `QAction`s with literal `setShortcut` calls (`:718, 756, 877, 886, 914, 933,
948, 956, 972, 983, 994, 1003, 1015, 1039, 1046, 1055, 1071, 1079, 1087, 1106,
1114, 1122, 2341`) plus raw `QShortcut`s (`:1413, 1423, 1434, 1440`). There is
no `KActionCollection` outside `EditorArea.cpp:74` (the embedded KTextEditor
view's own), no `KShortcutsDialog`, no `KEditToolBar`, no `.rc` file. Settings ▸
Configure Shortcuts — which Kate itself provides — is absent. `Ctrl+Alt+T`
(`:1015`) is commonly the system terminal shortcut and cannot be changed.
`EditorArea.cpp:64-83` already documents an *"Ambiguous shortcut"* collision
with the KTextEditor Save action, worked around by blanking the part's shortcut
— a workaround that will recur with every new action.

**4. The command palette only sees the menu bar.** `showCommandPalette`
(`MainWindow.cpp:1164`) walks `menuBar()->actions()` and nothing else, and skips
anything hidden by Simple mode. Panels build their own actions outside the menu
bar — `TerminalPanel`, `DiffView`, `ProjectTree`, `SearchPanel`, `JobsPanel`,
`CoworkPanel`, `git/LogViewer` — and none of them appear. `CommandPalette.h`'s
own header comment claims it lists *"every command in the application"*. It
lists a fraction, and least of all in Simple mode, which is exactly the mode
where finding things by name matters most.

All four are one problem wearing four hats: **there is no single registry of
what this application can do.**

## Phase 1 — One `KActionCollection` (the prerequisite)

`KF6::XmlGui` is already linked (`ui/CMakeLists.txt`), so this is a refactor,
not a dependency.

- Add `KActionCollection *m_actions` to `MainWindow`.
- Every action routes through
  `m_actions->addAction(QStringLiteral("<stable-name>"), act)` and
  `KActionCollection::setDefaultShortcut(act, seq)` instead of `setShortcut`.
  **The stable name is the contract** — it is the KConfig key a user's
  customised binding is stored under, so renaming one silently resets that
  user's shortcut. Name them once, deliberately, and write that rule in a
  comment at the top of the collection.
- Convert the four raw `QShortcut`s (`:1413, 1423, 1434, 1440`) into named
  actions. A `QShortcut` is invisible to every configuration surface.
- Add `KStandardAction::keyBindings(this, &MainWindow::configureShortcuts,
  m_actions)` opening `KShortcutsDialog::showDialog(m_actions)`. Feed it
  `EditorArea`'s collection too — that resolves the documented Save ambiguity
  *properly* instead of by blanking, and lets the dialog show the conflict.
- **Panel action registry.** `MainWindow::registerCommands(const QString &group,
  const QList<QAction*> &)`, called by each panel at construction. Merge the
  registered actions with the menu-bar walk in `showCommandPalette`. Include
  Simple-mode-hidden actions but **tag** them (a dimmed "Advanced" suffix in
  `CommandPalette::rebuildList`) so the palette is the escape hatch from Simple
  mode rather than a second wall. `CommandPalette` already `QPointer`-guards its
  actions, so a panel being destroyed is safe.
- **Full `setupGUI()` + `agentkateui.rc` is explicitly deferred.** The collection
  plus the keyBindings dialog is the 80% win and is source-compatible with the
  existing menu-building code; converting to a `.rc` file means moving every menu
  definition into XML, which is a large change with its own review surface. Do
  it later, or not at all. `KEditToolBar` follows the `.rc` file, so toolbar
  configuration is deferred with it — say so here so it is a decision, not an
  omission.

**Every plan in this program adds its actions here.** Plan 21's fleet actions,
plan 22's extension actions, plan 23's checkpoint actions, plan 24's
answer-question action, plan 25's export/visualize — all of them.

## Phase 2 — System tray presence

New `ui/src/shell/TrayPresence.{h,cpp}` owning a `KStatusNotifierItem`. Requires
adding `StatusNotifierItem` to the `find_package(KF6 …)` list in
`ui/CMakeLists.txt` (Notifications, DBusAddons and WindowSystem are already
there).

**What the tray knows, and where it gets it.** Nowhere does `TrayPresence`
compute state. It consumes what `AgentNotifier` already consumes:
`AgentPanel::statusChanged` and `AgentPanel::attentionChanged`, forwarded by
`AgentDock`. That is deliberate: `AgentNotifier`'s header states the rule that
*"it can never claim a state the card does not show"*, and the tray must obey
the same rule or the roster, the notification and the tray will disagree.

- **Icon status:** `Passive` when nothing is running, `Active` when agents are
  running, **`NeedsAttention`** when any agent is blocked — which makes the
  Plasma tray icon pulse. The attention signal already exists; this is a second
  consumer, not new plumbing.
- **Tooltip / overlay:** "3 running · 1 waiting on you".
- **Context menu**, built from `m_actions` (Phase 1's collection — this is why
  Phase 1 comes first):
  - a per-agent submenu, each entry raising the window and selecting that agent
    (`AgentDock`'s existing `selectAgent`),
  - "Answer next question" / "Answer pending permission" when the queue is
    non-empty (plan 24 supplies the question half),
  - New Agent, Show/Hide, Quit.
- **Attention counts questions**, once plan 24 lands. Until then it counts
  permissions only, which is still correct — just narrower.

**Close-to-tray**, the actual feature:

- A preference (`[Behaviour] closeToTray`, default **off** — changing what the
  close button does without asking is hostile).
- When on, `closeEvent` hides to tray instead of running `ShutdownDialog`.
  `ShutdownDialog` is reserved for a **genuine quit**: File ▸ Quit, the tray's
  Quit, session logout.
- **The trap to avoid, stated plainly:** `KMainWindow::closeEvent` and Qt's
  `quitOnLastWindowClosed` interact badly with a hidden main window — hide it
  and the app can exit anyway. Set `qApp->setQuitOnLastWindowClosed(false)`
  when the tray is active and **only** then, or a tray-less environment
  (a bare WM with no StatusNotifier host) becomes unquittable. Guard on
  `KStatusNotifierItem`'s availability, and if there is no host, fall back to
  today's behaviour with a one-time `KMessageWidget` explaining why.
- **First hide-to-tray shows a one-shot notification** ("Agent Kate is still
  running — 2 agents are working"). A window that vanishes with work still
  running, and no sign of where it went, is a support ticket.
- The existing `persistEditorSession` / `persistShellState` / `saveSession` /
  `persistLastActiveSessions` calls at the top of `closeEvent` (`:2280-2293`)
  must **still run on hide**, not only on quit — otherwise a crash while hidden
  loses the session state that the current code is careful to snapshot early.

## Phase 3 — Global shortcuts (KGlobalAccel)

Three bindings, no more. Every global shortcut is taken from the user's whole
desktop; three is a budget, not a starting point.

| Action | Suggested default | What it does |
|---|---|---|
| **Show/Hide Agent Kate** | `Meta+A` | Raise, activate, focus the active agent's composer. Toggles to hide when already focused. |
| **Quick-ask the active agent** | `Meta+Shift+A` | A small always-on-top prompt line — KRunner for your agent — that sends to the active agent **without switching windows**. |
| **Answer pending attention** | `Meta+Ctrl+A` | Raise and jump to the first blocked agent (question or permission). |

- All three are actions in `m_actions` with
  `KGlobalAccel::self()->setGlobalShortcut(act, seq)`. They therefore appear in
  **System Settings ▸ Shortcuts ▸ Agent Kate**, which is where KDE users
  discover what an app can do — the real reason this feature is worth having.
- **Defaults ship empty.** Register the actions with no default sequence and let
  the user assign them, or ship the three above and accept collisions. This
  plan's position: ship `Meta+A` and `Meta+Shift+A` (both commonly free on
  Plasma), and ship "Answer pending attention" **unbound** because it is the
  least frequent and the most likely to collide. See open question 1.
- **Raising correctly on Wayland** needs an xdg-activation token, which the
  `KDBusService` work landing now already established for the
  relaunch-raises-us path. Reuse it; do not write a second raise path.
- **Quick-ask** is a new small frameless `QDialog` (`ui/src/QuickAskDialog.*`):
  one `ElidingLabel` naming the target agent, one line edit, Enter sends, Escape
  closes. It reuses `AgentPanel::sendMessage` through `AgentDock` and adds no
  protocol. If no agent is active it opens the New Agent dialog instead of doing
  nothing.

## Phase 4 — The small KDE-citizen wins in the same pass

Cheap, adjacent, and each one is a separate chore if not done here:

- **`Keywords=` in `ui/org.kde.agentkate.desktop.in`** —
  `AI;agent;coding;assistant;LLM;claude;kimi;IDE;editor;git;worktree;` so
  KRunner/Kickoff find it by what people actually type. Today only "Agent Kate"
  matches, which collides with Kate.
- **Re-enable the status bar size grip** (`MainWindow.cpp:1767`
  `setSizeGripEnabled(false)`), and promote the *state* messages (layout reset,
  core disconnect/reconnect, save failures) from 6-second status text to the
  existing `KMessageWidget` banner. A user who steps away currently misses all
  of them.
- **AppStream metainfo** — `appstreamcli validate
  ui/org.kde.agentkate.metainfo.xml` currently fails with one warning and two
  infos: unreachable homepage URL, deprecated `<developer_name>`, missing
  developer info, and no `<screenshots>` block at all. Fix all four and wire
  `appstreamcli validate` into the CMake test suite so it stays green.
- **Accessibility, the two changes that matter most.** The transcript
  `QListView` is `setFocusPolicy(Qt::NoFocus)` (`AgentPanel.cpp:372`) — the core
  content of the app cannot be reached by Tab, scrolled by keyboard, or read by
  Orca. Give it `Qt::StrongFocus` with `NoSelection` retained (focus enables
  PgUp/PgDn/arrows without changing the read-only interaction model), and have
  `TranscriptModel` serve `Qt::AccessibleTextRole` from the plain-text form the
  copy path already assembles in `TranscriptDelegate`. Same for
  `CooperationPanel.cpp:35`. A full a11y sweep is out of scope; **these two are
  the difference between "unusable with a screen reader" and "usable"**.

## Verify

| Phase | What proves it |
|---|---|
| 1 | Every action has a stable name: a test that walks `m_actions` and asserts no empty `objectName()` and no duplicates. Renaming one later then shows up as a failing test, which is the point. |
| 1 | Manual: Settings ▸ Configure Shortcuts opens, lists both collections, rebinding `Ctrl+Alt+T` persists across restart. |
| 1 | Manual: the documented `EditorArea` Save ambiguity — open a document, press `Ctrl+S`, confirm no "Ambiguous shortcut" warning and the right handler runs. |
| 1 | Manual: open the palette, confirm a panel-local command (e.g. Terminal ▸ New Terminal, `TerminalPanel.cpp:143`) is now listed; confirm an Advanced-only command appears tagged while in Simple mode. |
| 2 | `ui/tests/TrayPresenceTest.cpp` — the decision layer only, no D-Bus: given N running / M attention, the expected status enum and tooltip come out. Same split `AgentNotifier` uses (`evaluateStatus` / `evaluateAttention` separated from `emitAlert`) and for the same reason. |
| 2 | Manual: enable close-to-tray, start an agent, close the window → the window hides, the agent keeps working, the tray shows 1 running, and the one-shot notification appears. Reopen from the tray → the transcript is intact. |
| 2 | Manual: File ▸ Quit with agents running → `ShutdownDialog` still runs. The distinction between hide and quit is the whole feature. |
| 2 | Manual negative: run under a session with no StatusNotifier host → the app still quits normally and does not become unclosable. This is the failure that would be catastrophic and is easy to miss. |
| 2 | Manual: agent hits a permission prompt while hidden → the tray icon enters `NeedsAttention` and Plasma pulses it. |
| 3 | Manual: the three actions appear in System Settings ▸ Shortcuts ▸ Agent Kate. |
| 3 | Manual on Wayland: with Firefox focused, `Meta+A` raises Agent Kate **and focuses the composer** — activation without a token silently fails on Wayland, so verify the caret, not just the window. |
| 3 | Manual: `Meta+Shift+A` from another app opens Quick-ask, Enter sends, the reply arrives in the panel. |
| 4 | `appstreamcli validate ui/org.kde.agentkate.metainfo.xml` exits 0, enforced by ctest. |
| 4 | `desktop-file-validate` on the built `.desktop`; KRunner finds the app by typing "agent" and by typing "claude". |
| 4 | Manual with Orca: Tab reaches the transcript, arrows scroll it, and the screen reader speaks the rows. |

## Non-goals

- **Full KXmlGui `setupGUI()` + `.rc` conversion**, and therefore
  `KEditToolBar`. Deferred deliberately (Phase 1); the collection is the win.
- **A tray-only mode.** The tray is presence for a running app, not a headless
  daemon. Quitting still quits, and `akcore` still dies with the UI.
- **More than three global shortcuts.** They are taken from the user's entire
  desktop.
- **A complete accessibility pass.** Phase 4 does the transcript and the roster.
  Custom-painted delegates (`AgentCardDelegate`, `WorktreeCardDelegate`,
  `ChipPainter`) exposing accessible text is a separate, larger piece of work.
- **Translations.** The KDE audit found no `po/`, no `Messages.sh`, no
  `ki18n_install()`, and 57 `tr()` calls outside the declared catalogue. That is
  a real gap and it is **not** this plan — except for the 22 in
  `ProvidersDialog.cpp`, which [plan 26](26-engine-services.md) converts because
  it is editing that file anyway.

## Open questions for the user

1. **Global shortcut defaults.** Ship `Meta+A` / `Meta+Shift+A` bound and the
   third unbound (this plan's position), or ship all three unbound so nothing can
   possibly collide with the user's existing setup?
2. **Close-to-tray default.** Off (this plan's position — do not change what the
   close button does without asking), or on, given that closing-kills-everything
   is the behaviour the feature exists to fix?
3. **Quick-ask target.** "The active agent" is ambiguous when the window is
   hidden. Prefer (a) last-focused agent, (b) the agent that most recently
   needed attention, or (c) a picker in the quick-ask line itself?
4. **Tray icon when idle.** `Passive` hides the icon in some Plasma
   configurations. Always show (so the app is findable), or go passive when
   nothing is running (so the tray stays clean)?
