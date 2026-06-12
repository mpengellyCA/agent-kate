# 02 — Markdown Preview / Raw / Split Toggle

## Goal

When a Markdown file (`.md`, `.markdown`) opens in the editor, let the user toggle
between three modes:

- **Raw** — the normal `KTextEditor` editing view (what we have today).
- **Preview** — rendered Markdown.
- **Split** — raw editor on the left, live preview on the right.

## Current state

The editor is `EditorArea` (`ui/src/EditorArea.{h,cpp}`), a container of grouped
`QTabWidget`s (`m_groups`, `EditorArea.h:51`). File opening and dispatch live in
`EditorArea::openFile` (`EditorArea.cpp:108-176`):

- Existing-tab reuse for both `KTextEditor::View` and `ImageView`
  (`EditorArea.cpp:120-140`).
- **File-type dispatch point** (`EditorArea.cpp:142`): `if (ImageView::canDisplay(abs))`
  → host a custom `ImageView` widget; **else** build a `KTextEditor` document + view.
- Custom widgets are hosted as tabs exactly like text views:
  `tabs->addTab(widget, name)` (`EditorArea.cpp:144` for image, `:166` for text,
  `:185` for diff).
- Tab teardown distinguishes the two: `KTextEditor::View` docs get deleted, other
  widgets get `removeTab()` + `deleteLater()` (`EditorArea.cpp:237-253`).
- Current-file tracking casts polymorphically to find the path
  (`emitCurrentFile`, `EditorArea.cpp:255-267`; `openFilePaths`, `:216-235`).

### Precedents already in the tree

- **`ImageView`** (`ui/src/ImageView.{h,cpp}`) is the exact pattern for "a non-Kate
  custom `QWidget` hosted in an editor tab, chosen by file extension." It even has a
  toolbar of **checkable mode actions** (`ImageView.cpp:45-81`) — the model for our
  raw/preview/split toggle.
- **Markdown→HTML already exists**: `AgentPanel.cpp:65-80` uses
  `QTextDocument::setMarkdown(md, QTextDocument::MarkdownDialectGitHub)` then
  `toHtml()`. Reuse this verbatim.
- **`QTextBrowser` host pattern**: `DiffView.cpp:258-266` builds HTML and
  `setHtml()`s it into a `QTextBrowser` tab. Same approach for the preview pane.
- **Linked libs** (`ui/CMakeLists.txt`): Qt6 Core/Gui/Widgets/Network, `KF6::TextEditor`,
  `KF6::SyntaxHighlighting`, KIO, etc. **No QtWebEngine, no external markdown lib** —
  and none needed.

## Proposed design

New widget **`MarkdownView : QWidget`** (`ui/src/MarkdownView.{h,cpp}`) modeled on
`ImageView`, hosting:

- a small `QToolBar` with three checkable, mutually-exclusive actions
  (Raw / Preview / Split) in a `QActionGroup`;
- a `KTextEditor::View` (the real editable Kate view over the file's document);
- a `QTextBrowser` preview pane;
- a `QSplitter` that holds editor and/or preview depending on mode.

Mode switching just shows/hides the two panes inside the splitter — the document is
the **same** `KTextEditor::Document` in all modes, so edits in raw/split are real
edits with full Kate features (highlighting, LSP, save).

**Live preview:** connect the document's `textChanged` signal to a debounced
re-render (reuse the `setMarkdown`→HTML helper). Add a short `QTimer` debounce
(~150–250ms) like `SearchPanel`'s search debounce so typing stays smooth.

**Code blocks:** for v1, `QTextDocument::setMarkdown` renders fenced code as
monospace blocks — acceptable. A later pass can syntax-highlight them with the
already-linked `KSyntaxHighlighting::Repository` (same engine `DiffView` uses).

### Hosting decision: dedicated view vs. KTextEditor's own preview

`KF6::TextEditor` does **not** ship a markdown preview; the Kate app's preview is a
separate plugin (`KTextEditorPreview`/KParts) that isn't linked here. Building our
own `MarkdownView` is simpler than pulling in that plugin and matches the existing
`ImageView` precedent. Recommended.

## Implementation steps

1. Add a dispatch helper `isMarkdownFile(path)` and a branch in `EditorArea::openFile`
   **before** the `ImageView::canDisplay` check (`EditorArea.cpp:142`) →
   `auto *md = new MarkdownView(m_editor, abs, tabs); tabs->addTab(md, name);`.
   `MarkdownView` should take the shared `KTextEditor::Editor*` so it can
   `createDocument`/`createView` the same way `EditorArea.cpp:154-165` does.
2. Create `ui/src/MarkdownView.{h,cpp}`:
   - ctor: create document, `openUrl`, create view; build `QTextBrowser`; assemble
     `QSplitter` + toolbar; default mode = Preview (configurable).
   - `setMode(Raw|Preview|Split)`: toggle pane visibility; on entering a preview-bearing
     mode, render once.
   - `render()`: `setMarkdown` → `toHtml` → `m_preview->setHtml(...)` (lift the helper
     from `AgentPanel.cpp:65-80` into a shared util or duplicate locally).
   - debounce timer on `document()->textChanged`.
   - expose `path()` and the underlying `KTextEditor::View*`/`Document*` so EditorArea's
     polymorphic loops keep working.
3. Update `EditorArea`'s polymorphic spots to recognize `MarkdownView`:
   - existing-tab reuse (`:120-140`), `closeTabIn` teardown (`:237-253` — it owns a
     real document, so delete the doc then the widget), `emitCurrentFile` (`:255-267`),
     `openFilePaths` (`:216-235`), and `documentOpened`/`documentClosed` emission so
     LSP/outline still see the doc.
4. Persist last-used mode per file (or globally) in `KSharedConfig`, like the other
   sticky UI prefs.
5. Add `MarkdownView.{cpp,h}` to `ui/CMakeLists.txt`.

## Risks / considerations

- **Don't lose Kate document integration.** Because raw/split use a genuine
  `KTextEditor::Document`, the file must still emit `documentOpened` so the rest of
  the app (LSP, Problems, Outline, Git gutter) treats it like any other open file.
  This is the main subtlety vs. `ImageView` (which has no document).
- **Scroll sync** in split mode is a nice-to-have, not v1 — `QTextBrowser` and the
  Kate view have different layout models; approximate by proportion later.
- **Relative links / images** in the preview: set the `QTextBrowser` search paths to
  the file's directory so embedded image links resolve; keep external link opening
  off (or route through `QDesktopServices`) to avoid surprises.
- Keep styling Breeze-native: derive `QTextBrowser` colors from the palette
  (`DiffView.cpp:260-261` sets background from palette — follow that).

## Out of scope (follow-ups)

Syntax-highlighted code fences, scroll-sync, math/mermaid, export-to-PDF/HTML,
editing in the preview pane.
