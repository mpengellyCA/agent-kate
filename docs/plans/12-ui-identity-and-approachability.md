# 12 — UI Identity & Approachability (broaden the audience beyond CS)

> **Status: phases 1–3 implemented and merged to local `main`** (branch
> `agentkate/t-7bb53c7e24`, commits `2d0ed9d`, `c5ee29f`, `c045e3f`). Phases 4–8
> below are the remaining roadmap — grounded, sequenced, not yet built.

## Goal

Agent Kate reads like an "advanced Linux developer IDE". The goal is to make it
approachable to **advanced and intermediate knowledge workers across disciplines**
— people who want to *work alongside an agent*, not operate an IDE — **without
losing** the deep-dive power for those who want it. Strong defaults, an obvious
primary path, advanced functions one keystroke away, and a real visual identity.

Two product principles drive every item here:

1. **Progressive disclosure.** The front door is simple; depth is opt-in (the
   Experience level, the command palette, "Advanced…" sections in dialogs).
2. **Plain language.** Developer jargon (compaction, isolation, worktree, perspective,
   LSP) is renamed or explained in human terms; the git/dev machinery stays, relabelled.

---

## Shipped (phases 1–3)

### Phase 1 — Identity: signature theme + theme override (`2d0ed9d`)
- `ui/src/theme/ThemeManager.{h,cpp}` + `Theme.h` — a single owner of appearance that
  builds a `QPalette` in code and applies it with `qApp->setPalette()` while keeping the
  **native Breeze style and no app-wide stylesheet** (palette-only — this is what avoids
  the historic Fusion+QSS breakage of KTextEditor/tree widgets).
- Built-ins: **Agent Kate Midnight** (default — navy `#0b0e24`/`#12152e`, purple selection
  `#7c3aed`, neon-pink accent `#ff2d8e`, grey text), **Agent Kate Daylight** (light), **Follow
  System**, plus **every installed KDE colour scheme** discovered via
  `KColorScheme::createApplicationPalette` — so the app can wear a *different* look than the
  rest of the desktop. Persisted to `agentkaterc [Appearance] Theme=`, applied in `main.cpp`
  before any window.
- `ui/src/AppearanceDialog.{h,cpp}` — grouped picker, swatches, live preview, revert-on-cancel
  (Options ▸ Appearance).
- The ad-hoc hardcoded light/dark colour pairs (gutter, git graph, transcript, diff chips,
  roster dot, working spinner) now read `ThemeManager::colors()` semantic colours.

### Phase 2 — Discoverability: command palette (`2d0ed9d`)
- `ui/src/CommandPalette.{h,cpp}` — VS-Code-style fuzzy palette over every window action with
  shortcuts (`Ctrl+Shift+P` / `Ctrl+P`). `MainWindow::showCommandPalette()` walks `menuBar()`,
  honouring hidden menus (so Simple mode hides those commands too).

### Phase 3 — Experience modes, layouts, responsiveness (`c5ee29f`, `c045e3f`)
- **Simple/Advanced experience level** — status-bar toggle + Options ▸ Experience Level,
  persisted to `[Experience] level`. Simple hides the Code menu, the dev-only View actions,
  and the developer panels; keeps Projects & Agents, Files, Search, Cowork and the chat.
  First-run heuristic: a brand-new profile starts Simple. `MainWindow::setupExperience` /
  `applyExperienceLevel` / `setPanelTabVisible`.
- **Workspace layouts** — friendly **Converse / Build / Review / Side by Side** replacing the
  "Perspective" jargon; a visible top-toolbar **Layout** switcher + View ▸ Layout +
  `Ctrl+Shift+1..4`, routed through `applyCentreMode`.
- **In-panel responsiveness** — `ui/src/shell/FlowLayout.{h,cpp}` (wrapping toolbar/chip rows;
  unit-tested) + `ui/src/shell/ElidingLabel.{h,cpp}` (eliding long labels). Converted
  AgentPanel, ProjectTree, AgentRoster, WorktreeDashboard, DiffView, LogViewer, CoworkPanel,
  MainWindow search. See `12`'s sibling note [10-panel-responsiveness.md](10-panel-responsiveness.md)
  for the earlier *shell*-level (pane sizing) work this complements.

---

## Remaining roadmap

### Phase 4 — First-class Agent actions (Agent menu) — size **M**
**Why:** the agent lifecycle actions (new, rename, stop/resume, attach, show changes, commit,
PR, merge, discard worktree, tags) live **only** in the roster right-click menus
(`AgentRoster.cpp`) and the composer toolbar (`AgentPanel.cpp`). A newcomer never discovers
them. They should also be a top-level **Agent** menu (→ menu bar, hamburger, command palette).

**Grounded blocker:** `AgentDock`'s agent operations are **private** (`ui/src/AgentDock.h:79-119`:
`addAgent`, `closeAgent`, `renameAgent`, `editTags`, `autoOrganize`, …). The menu needs a thin
**public, context-aware surface** on `AgentDock` that operates on the *active* agent.

**Steps:**
1. Add public methods to `AgentDock` for the active agent: `newAgentForActiveProject()`,
   `renameActiveAgent()`, `stopActiveAgent()`, `attachToActiveAgent()`, `showActiveAgentChanges()`,
   plus `bool hasActiveAgent()` / signals so the menu can enable/disable. Each forwards to the
   existing private impl using the current selection.
2. In `MainWindow::setupActions`, add an **`&Agent`** menu between Options and View with those
   actions; wire enable-state to `AgentDock` signals; give every action an icon + plain-language
   tooltip. They flow into the command palette automatically.
3. Mark the developer-only subset (e.g. *Discard worktree*, *Merge into local main*) so Simple
   mode hides them (add to `m_advancedActions`).

### Phase 5 — Plain-language agent panel — size **M**
**Why:** the composer's **Setup** and **Compaction** dropdowns are dense, jargon-heavy forms
("Permission / Isolation / Effort / Compact on Stop (Hot Opus)…") — `ui/src/AgentPanel.cpp`
(~`710` Setup, ~`740` Compaction). This is the single biggest "too much nuance" surface.

**Design (relabel + explain; keep the mechanics):**
- **Compaction → "Memory"** — "How this agent remembers a long conversation." Strategy names in
  human terms ("Summarize when it pauses", "Summarize cheaply on resume"); the model tiers move
  behind an "Advanced…" disclosure.
- **Isolation → "Sandbox"** — "Work on a private copy so changes don't touch your files until you
  approve." **Worktree → "working copy"**, **Promote → "Save to a branch"** (the promote bar text
  in `AgentPanel.cpp`).
- **Effort → "Thinking effort"**; **Permission modes** → "Ask first / Work freely / Expert".
- Add a small **"?" affordance** per row (a `QLabel` with a what's-this tooltip) instead of
  burying meaning in the option text.

### Phase 6 — Custom navy editor theme (close the canvas seam) — size **S–M**
**Why:** the app chrome is navy but the code editor / DiffView canvas is **Breeze Dark grey**
(`ThemeManager` currently sets `syntaxTheme = "Breeze Dark"` for Midnight). The seam is visible
in screenshots.

**Steps:**
1. Author a KSyntaxHighlighting theme JSON ("Agent Kate Midnight") with a navy editor background
   and purple/pink/mint syntax, bundle it in the Qt resource under
   `:/org.kde.syntax-highlighting/themes/` (KSyntaxHighlighting auto-discovers resource themes).
2. Point Midnight's `syntaxTheme` at it; DiffView already honours `ThemeManager::syntaxTheme()`.
   Wire the main **KTextEditor** view theme too (set the editor's colour theme on open).
3. Ship a matching Daylight editor theme. Finish migrating the few remaining `KColorScheme`-reading
   call sites (e.g. `CleanupDialog`, `ControlConsentDialog`) to the theme for full coherence.

### Phase 7 — Guided "New Agent" — size **M**
**Why:** the front-door action (start an agent to do X) is a blank starter + the dense Setup
dropdown. Replace with a friendly **New Agent** dialog: a big task field, plain-language choices
(model as "Smartest / Balanced / Fastest", sandbox on/off), and an **"Advanced"** disclosure for
the full Setup form. Reuses `AgentPanel`'s existing option model; new dialog only.

### Phase 8 — Welcome / first-run redesign — size **S–M**
**Why:** `ui/src/WelcomeDialog.cpp` is functional but plain. Make first-run set the tone: recent
projects, "open a folder", "start something new", a one-line theme/Experience intro, and land the
newcomer in **Simple + Converse**. Pairs with the Phase 3 first-run heuristic already in
`setupExperience`.

### Lower-priority cleanups (slot in opportunistically)
- Remaining responsiveness spots the audit flagged as low severity: breadcrumb middle-segment
  elide/collapse (`MainWindow::updateBreadcrumb`); CoworkPanel label+control rows → `QFormLayout`
  with `WrapLongRows`; `CommitDetailPanel` file-list path elide.
- An ElidingLabel **rich-text** variant (current one is plain-text only) so colored/`<b>` labels
  can elide too — would let DiffView's summary and Cowork's active label elide instead of the
  current QLabel+size-policy workaround.

---

## Sequencing

Phase **4** (Agent menu) first — it's the biggest discoverability gap and unblocks nothing else
but composes with the palette. Then **5** (plain language) and **6** (editor theme) in parallel
(disjoint files: `AgentPanel` vs `theme/` + resources). **7** (guided New Agent) builds on 5's
relabelled option model. **8** (Welcome) last — it ties the first-run story together. The
lower-priority cleanups can land anytime; none gate the others.

Size key (matches `README.md`): **S** ≈ <½ day, **M** ≈ 1–2 days, **L** ≈ 3–5 days.
