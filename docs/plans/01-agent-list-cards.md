# 01 — Agent List → Visual Card List

## Goal

Turn the agent selector from a one-line-per-agent tree into a richer **card list**:
each agent shown as a multi-line card with title, a longer description/subtitle,
status, worktree number, and room for future metadata (model, last activity). More
visual, more legible, easier to pick the right agent at a glance.

## Current state

The list is `AgentRoster`, a `QWidget` wrapping a **`QTreeWidget`**, hierarchical
**Projects → Agents**. It lives in the left sidebar, owned by `AgentDock`.

- `ui/src/AgentRoster.h` / `ui/src/AgentRoster.cpp` — the widget.
  - Tree built in `AgentRoster.cpp:47-133`.
  - Per-agent data is stored in `QTreeWidgetItem` user roles
    (`AgentRoster.cpp:16-24`): `Qt::UserRole` = agentId, `+1` = dormant, `RoleTitle`
    (`+2`) = raw title, `RoleNumber` (`+3`) = worktree number.
  - Display per agent today is just: a 14×14 status dot icon (`dotIcon()`,
    `AgentRoster.cpp:33-44`) + a `#N  Title` label (`AgentRoster.cpp:160,169,183`).
  - Public API to keep working: `addAgent` (`:150`), `setAgentTitle` (`:165`),
    `setAgentStatus` (`:186`), `setAgentNumber` (`:173`), `setAgentDormant` (`:193`),
    `removeAgent` (`:203`), `setCurrentAgent` (`:217`).
- **Selection signal** (must preserve): `currentItemChanged` →
  `agentActivated(int)` for agent rows / `projectFocused(QString)` for project rows
  (`AgentRoster.cpp:64-73`). `AgentDock` consumes these to switch the panel stack
  (`AgentDock.cpp:36-40`) and re-emits outward to `MainWindow`.
- **Where state changes come from**: `AgentDock::wireAgentPanel` (`AgentDock.cpp:343`)
  connects each `AgentPanel`'s `titleChanged` / `stateChanged` / `dormantChanged`
  signals into the roster setters.

### The data gap

There is currently **no description/subtitle field** for an agent. Titles are
user-set or auto-`"Agent N"`. So the card needs a second text line that we must
*populate from somewhere*. Options, cheapest first:

1. **Derive locally** — subtitle = worktree branch / isolation mode + relative path +
   last-activity ("idle 3m" from the existing idle state), composed in the UI. No
   protocol change. Recommended for v1.
2. **First-prompt summary** — store the agent's opening prompt (truncated) as the
   description. `AgentPanel` already has the first user message; persist it into the
   session record and surface via `session.listThreads`.
3. **Model + token meter** — once #5 lands, show the per-agent model and a compact
   usage figure (data already exists in `toolmeter`/`usagemeter`).

## Proposed design

Replace the rendering, **keep the model and the public API**. Two viable routes:

### Route A (recommended) — custom delegate on the existing tree

Keep `QTreeWidget` (so projects-as-sections and all the existing setters/lookup
helpers survive untouched) and attach an `AgentCardDelegate : QStyledItemDelegate`
to draw agent rows as cards. Project rows keep default rendering (or a thin section
header style).

- Model after the existing delegates: `ui/src/git/RefChipDelegate.{h,cpp}` and
  `ui/src/git/LogGraphDelegate.{h,cpp}` — both override `paint()` + `sizeHint()`,
  call `initStyleOption()` then `drawControl(CE_ItemViewItem, …)` for the
  background/selection, then custom-paint on top. This is the established pattern in
  the repo.
- `sizeHint()` returns a taller fixed height (e.g. ~48–56px) for agent rows so two
  text lines + status fit.
- `paint()` draws: status dot (reuse the color currently fed to `setAgentStatus`),
  bold title line (`RoleTitle`), muted subtitle line (new role, e.g. `RoleSubtitle`
  = `Qt::UserRole + 4`), worktree badge `#N`, and a dormant/idle affordance
  (dim + italic when `dormant`).
- Add `setAgentSubtitle(int id, const QString&)` to `AgentRoster` and a matching
  `subtitleChanged` wire-up in `AgentDock::wireAgentPanel`.

**Why A:** smallest diff, zero risk to selection/lookup logic, all done in one new
delegate file + a couple of setters. Uses Breeze palette via `QStyleOption`
(stays KDE-native per the project's design rule).

### Route B — `QListView` + custom model

Flatten to a single list with project section separators, backed by a small
`QAbstractListModel`. More flexible long-term (virtualized, easy filtering/sorting,
search box over agents) but a larger rewrite that must re-implement the project
grouping and every setter as model mutations. Defer unless we want agent
filtering/search in the same pass.

## Implementation steps (Route A)

1. Add `RoleSubtitle = Qt::UserRole + 4` and a `setAgentSubtitle` setter +
   `agentItem(id)` write in `AgentRoster.cpp`.
2. Create `ui/src/AgentCardDelegate.{h,cpp}` (copy structure from `RefChipDelegate`):
   `sizeHint()` → tall rows for items with a parent (agents); default for projects.
   `paint()` → dot + title + subtitle + `#N` badge + dormant styling, all via
   `option.palette` (no hard-coded colors except the status dot, which is already a
   hex string supplied by `setAgentStatus`).
3. In `AgentRoster` ctor, `m_tree->setItemDelegate(new AgentCardDelegate(...))` and
   bump `setUniformRowHeights(false)`.
4. Wire the subtitle source: in `AgentDock::wireAgentPanel` compose a subtitle from
   isolation mode + worktree + idle state and call `setAgentSubtitle`; refresh it on
   `stateChanged`/`dormantChanged`. (v1 = locally derived, per the data-gap note.)
5. Add the file to `ui/CMakeLists.txt`.
6. Verify selection, dormant restore, and worktree-number refresh
   (`AgentDock::refreshAgentNumbers`) still behave.

## Risks / considerations

- Keep it **Breeze-native** — paint with `option.palette` roles
  (`Highlight`, `HighlightedText`, `Text`, `PlaceholderText`), no custom theming
  (project memory: KDE-native design).
- Don't regress keyboard navigation or the project/agent dual-role of
  `currentItemChanged`.
- `sizeHint` height should scale with `option.fontMetrics`, not a magic pixel count,
  so it survives font-scaling/HiDPI.

## Out of scope (follow-ups)

Drag-to-reorder agents, inline rename in the card, per-agent context menu actions,
agent search/filter (would favor Route B).
