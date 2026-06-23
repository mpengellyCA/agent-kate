# 10 — Panel Responsiveness & Resize Performance

## Goal

Make Agent Kate's panel shell **shrink freely** and **resize smoothly**, even with a
long/heavy agent chat or a large document open. Two user-reported defects drive this:

1. **Rigidity** — "The Left and Right Pane won't shrink past the largest of the pane;
   they hold a very rigid amount of space." A small panel shown in a pane is still
   floored at the width of the *widest* panel that pane can ever hold.
2. **Resize lag** — dragging splitter handles (or the window edge) is heavy,
   "particularly if there is a long heavy chat rendered or a document in the editor."
   The editor itself is "a lot less noticeable."

This was investigated by five parallel sub-agents (shell/layout, AgentPanel chat,
left-pane panels, right+bottom panels, editor/doc-viewers). Their findings converge
on a small number of structural root causes with a clean, low-risk fix set, plus one
larger follow-up (chat virtualization) staged as Phase 2.

## Root causes (confirmed, with evidence)

### A. Rigidity — `QStackedWidget` aggregates the *max* of all pages' minimums

Every pane is a stock `QStackedWidget` placed in a `QSplitter`:

- Left / Right / Bottom panes: `SideBar::m_stack` (`ui/src/shell/SideBar.cpp:12`),
  added to the outer / centreV splitters in `ui/src/shell/ShellLayout.cpp:31,53,59`.
- The agent chat pane (editor | **agentPanel**) is *also* a `QStackedWidget`:
  `AgentDock::m_stack` (`ui/src/AgentDock.cpp:31`), holding one `AgentPanel` per agent.

`QStackedWidget::minimumSizeHint()` returns the element-wise **max over every page**
(by design — switching pages never forces the window to grow). So a pane's minimum
width is the widest minimum among *all* its panels, regardless of which is raised:

- **Left floor** dictated by **SearchPanel** (`SearchPanel.cpp:242-252`, toggles row +
  dual include/exclude `QLineEdit`s).
- **Right floor** dictated by **CoworkPanel** (`cowork/CoworkPanel.cpp:99,119,163,207`
  — long non-eliding `QGroupBox` titles, `KMessageWidget`, `QCheckBox` rows).
- **Centre/editor floor** dictated by whatever heavy view is open (Okular `KPartView`,
  `KPartView.cpp:102`) under the non-collapsible `m_centreH` (`ShellLayout.cpp:42`).

No panel sets an explicit `setMinimumWidth/Size` — the floor is **purely** the
implicit `QStackedLayout` max-aggregation. Confirmed: no `QStackedWidget` subclass,
no `setStackingMode`, no `minimumSizeHint` override anywhere in `ui/src`.

### B. Resize lag — live (opaque) splitter resize relayouts heavy, un-virtualized content every pixel

- The three shell splitters never call `setOpaqueResize(false)` (`ShellLayout.cpp:26-43`),
  so every pixel of a handle drag relayouts + repaints the child **synchronously**.
- Because the stack keeps *all* pages laid out, a drag re-measures **hidden** pages too
  (other chats, CoworkPanel) — not just the visible one.
- The visible chat is the worst payload: `AgentPanel` materializes the **entire**
  transcript as live widgets — one `QFrame` + word-wrapped `QLabel`(s) **per message**
  and ~9–10 widgets **per tool call** (`AgentPanel.cpp:456-466,1513-1605,178-310`),
  hundreds of them with `setWordWrap(true)` + `setHeightForWidth(true)` (`:1547,1561`).
  No height cache, no virtualization → **O(N) text re-layout per resize event**.
- Document viewers: **`ImageView`** smooth-rescales the **full-resolution** source on
  *every* resize event (`ImageView.cpp:137,132-133`, `Qt::SmoothTransformation`) — the
  classic doc-view resize stall. `CsvView` and `RichTextView` are already well-behaved
  (RichTextView even debounces image re-fit at `RichTextView.cpp:134-137` — the pattern
  to copy).

## Design

### Phase 1 — structural + content fixes (this pass, low-risk, high-impact)

**1. `PanelStack` — a `QStackedWidget` that sizes to its current page.**
New `ui/src/shell/PanelStack.{h,cpp}`. Overrides `sizeHint()` / `minimumSizeHint()` to
return only `currentWidget()`'s hints, and calls `updateGeometry()` on `currentChanged`
so the owning splitter re-queries. Swap it in for the four stocks:
`SideBar.cpp:12` (left/right/bottom) and `AgentDock.cpp:31` (chat). This single change
removes the max-aggregation floor (a small panel now shrinks the pane) **and** drops
hidden pages out of drag-time relayout (the multi-agent / multi-panel cost). Wired into
`ui/CMakeLists.txt`.

**2. Non-opaque splitter resize.** `setOpaqueResize(false)` on `m_outer`, `m_centreV`,
`m_centreH` (`ShellLayout.cpp`). Handle drags now rubber-band and relayout **once** on
release — smooth regardless of content weight. This is the highest reliability-to-effort
fix for the #1 complaint and is a standard, KDE-acceptable choice for heavy panes.

**3. `ImageView` debounced rescale.** Fast (nearest) scale for immediate feedback during
interactive resize, a single `Qt::SmoothTransformation` pass after an ~80 ms settle
timer, and a `m_lastTarget` guard to skip no-op rescales (`ImageView.cpp:117-143`).
Mirrors the existing `RichTextView` debounce.

**4. `KPartView` min-width clamp.** Clamp the embedded part widget's minimum width so
Okular can't pin the centre pane (`KPartView.cpp:102`). Combined with PanelStack the
editor pane regains drag freedom.

**5. Visible-panel floor polish** (so even the *raised* heavy panel can compress):
- `SearchPanel` — let the include/exclude row compress (`SearchPanel.cpp:244-251`).
- `CoworkPanel` — wrap the two non-wrapping labels and the long group title; drop the
  root min width (`CoworkPanel.cpp:119,163,207`).

### Phase 2 — chat virtualization (follow-up, separate focused pass)

Replace the `QScrollArea` + per-message-widget feed with a model/view design
(`QAbstractListModel` + `QListView` + a `QStyledItemDelegate` drawing the already-built
HTML via `QTextDocument`, with a per-(row,width) height cache). This makes resize cost
O(visible rows) and slashes memory for very long transcripts. It is a substantial
rewrite of a 2,800-line file that must preserve streaming append, tool-call expanders,
whole-message + code-block copy, in-conversation find, and sticky-bottom — so it is
deliberately staged after Phase 1 lands and is verified. Phase 1 already removes the
dominant *splitter-drag* lag; Phase 2 targets window-edge resize of very long chats and
memory.

## Test plan

- **New** `ui/tests/PanelStackTest` (QtTest, `QT_QPA_PLATFORM=offscreen`, first UI test
  target): a `PanelStack` holding a narrow + a wide widget asserts
  `minimumSizeHint().width()` tracks the **current** page (the regression guard for the
  rigidity bug), and that switching pages updates it. Wired via CTest.
- Build green (Debug) for the whole UI + core.
- Manual: drag each pane narrow with a heavy chat / large image open — confirm smooth
  drag and that a small raised panel shrinks the pane.

## File map

| Change | File |
|---|---|
| New PanelStack widget | `ui/src/shell/PanelStack.{h,cpp}` (+ `ui/CMakeLists.txt`) |
| Swap stacks | `ui/src/shell/SideBar.cpp:12`, `ui/src/AgentDock.cpp:31` |
| Non-opaque resize | `ui/src/shell/ShellLayout.cpp` |
| Image debounce | `ui/src/ImageView.{h,cpp}` |
| KPart min-width | `ui/src/KPartView.cpp` |
| Floor polish | `ui/src/SearchPanel.cpp`, `ui/src/cowork/CoworkPanel.cpp` |
| Test | `ui/tests/PanelStackTest.cpp` (+ CMake) |

Size: **M** (Phase 1). Phase 2 (chat virtualization): **L**, separate pass.

---

## Phase 2 implementation notes (chat virtualization)

Phase 1 killed the *splitter-drag* lag (non-opaque resize + current-page-only
sizing). Phase 2 targets the remaining cost: **window-edge resize of a long
chat** and the memory of materializing the whole transcript as live widgets.
It replaces the `QScrollArea` + per-message-widget feed with a model/view.

### Key fact that shapes the design

There is **no token-by-token streaming**. The core coalesces the stream-json
flood into ordered *batches* (`onNotification` → `agent.event` carries an
`events` array). Each `assistant` text block becomes a **whole new card**;
tool results arrive later as their own `user`/`tool_result` events keyed by
`tool_use_id`. So "streaming append" = *append new rows in batches* plus *mutate
the tool row whose result just arrived* — never a per-keystroke mutation of the
last row. This means a plain append-only model with a targeted `dataChanged`
for tool results is sufficient; no partial-text accumulation buffer is needed.

### Model — `TranscriptModel` (`QAbstractListModel`)

One row per feed entry. Row struct `TranscriptItem`:

| field | purpose |
|---|---|
| `kind` (Note / Message / Tool) | selects delegate paint path |
| `role` / `accentHex` | message header label + colour |
| `html` | pre-rendered body (markdown rendered **once** via `markdownToHtml`); for notes this is the note HTML |
| `plain` | raw markdown/source — copy + code-block extraction + search |
| `replayed` | suppress the live timestamp |
| `timestamp` | short local time string for live cards |
| tool: `toolName`, `summary`, `detail` (input JSON), `result`, `resultTruncated`, `fullResult` | tool card |
| tool: `expanded` | per-row collapse flag (default false) |
| tool: `done` | ✓ vs ⋯ marker |
| `toolVisible` | honours the `showTools` setting |
| `noteKind` | colour bucket for notes |

Public ops (mirror the old feed API): `appendMessage`, `appendNote`,
`appendTool` (returns row), `setToolResult(row, …)`, `setExpanded(row, on)`,
`setToolsVisible(bool)`, plus accessors for find/copy. Each mutation emits
`dataChanged` for exactly its row (or `begin/endInsertRows` for appends), so a
mutation re-measures **one** row, not the feed.

Tool rows are addressed by `tool_use id` via a `QHash<QString,int>` kept in the
panel (replaces `m_toolCards`).

### View — `QListView` + `TranscriptDelegate` (`QStyledItemDelegate`)

- `setUniformItemSizes(false)`, `setWordWrap(true)`, `setSelectionMode(NoSelection)`,
  horizontal scrollbar off, `setResizeMode(Adjust)` so a viewport-width change
  re-lays the visible rows.
- The delegate builds a `QTextDocument` from the row's cached HTML, sets its
  `textWidth` to the row's content width, and `documentLayout()->draw()`s it —
  same engine `QLabel` used, so markdown/code fidelity is preserved. The role
  header + timestamp + tool header line are drawn above the body.
- **Height cache**: `QHash<quintptr,CacheEntry>` keyed by a per-row stable id
  (model supplies a monotonic `idAt(row)`), storing `{width, height}`. `sizeHint`
  returns the cached height when the width matches; otherwise it measures the
  `QTextDocument` once and caches. → resize is **O(visible rows)**: only the
  rows the view actually asks `sizeHint` for (the visible window) get re-measured.
  The cache is invalidated for a row on `dataChanged` (model bumps that row's id)
  and wholesale on a model reset.
- The view connects `dataChanged`/`rowsInserted`/`modelReset` and on a width
  change calls `doItemsLayout()` so heights refresh.

### Feature mapping (each preserved feature → new mechanism)

1. **Streaming append** → `begin/endInsertRows`; sticky-bottom rides it.
2. **Sticky-bottom + jump button** → unchanged logic, but driven off the
   `QListView`'s own vertical scrollbar (rangeChanged/valueChanged). `m_jumpBtn`
   reparented to the view's viewport; `scrollToBottom()` via `scrollTo(lastIndex)`.
3. **Tool cards** (collapse/expand, copy, default-collapsed) → `expanded` flag
   toggled on click in the delegate's `editorEvent` (hit-test the header rect) →
   `setExpanded` → `dataChanged` re-measures that one row. A copy hit-rect in the
   header copies tool input+result. Permission affordances stay where they are
   (the `m_permBar` is **not** part of the feed — unchanged).
4. **Copy** → right-click context menu on the view: map position→index, offer
   "Copy message" (plain) and "Copy N code blocks" (same ```-fence regex over
   `plain`). Tool-row copy via the header copy hit-rect.
5. **Find** → iterate model rows; a row matches if `plain` contains the needle.
   Highlight = the delegate wraps matches in a highlight span when painting the
   current needle (model carries the active needle + current-hit row); scroll-to
   via `scrollTo(index)`. No re-render of stored HTML needed.
6. **Markdown/code fidelity** → `markdownToHtml` reused verbatim, rendered once
   into `html`, painted via `QTextDocument`. Never re-parsed on resize.
7. **Links external** → delegate `editorEvent` hit-tests anchors
   (`documentLayout()->anchorAt`) and opens via `QDesktopServices`.
8. **Notes / replay** → `appendNote`; `loadTranscript` replays through the same
   `renderEvent`, which now appends model rows.
9. **Selectable text** → per-row selection within a delegate is heavy; instead
   the row context-menu "Copy message" + the tool copy button cover copy needs,
   and double-click on a message row copies it. **Compromise noted**: live
   drag-select across the transcript is dropped (it never worked across cards
   anyway — each card was an independent QLabel). Copy paths fully preserved.

### Staging

1. Add `TranscriptModel` + `TranscriptDelegate` (new files), wired to CMake and
   the test target. Build.
2. Stand up the `QListView` in the panel **alongside** the old feed, route
   `addMessageCard`/`addNote`/tool creation to the model, hide the old scroll
   area. Build + smoke.
3. Remove the dead per-widget feed (`m_feed`, `m_feedLayout`, `addMessageCard`'s
   widget body, `ToolCard`, `m_searchables`/`Searchable`). Build + smoke.
4. Add `TranscriptModelTest` (row count, tool-result mutation, height-cache
   invalidation on width change, code-block extraction). `ctest`.

