# 03 — Wire the Toolbar Search Box

## Goal

The Search box on the main toolbar currently does nothing. Wire it so typing a query
and pressing Enter drives the real, working **Search panel** (ripgrep-backed code
search) — reveals the panel, fills its query, and focuses the results.

## Current state

### The dead widget

`MainWindow.cpp:1177-1185`:

```cpp
// Placeholder global-symbol search (wiring lives in Phase 4).
auto *search = new QLineEdit(toolbar);
search->setPlaceholderText(i18n("Search…  (Ctrl+T)"));
search->setClearButtonEnabled(true);
search->addAction(QIcon::fromTheme("search"), QLineEdit::LeadingPosition);
search->setFixedWidth(260);
search->setEnabled(false); // disabled until Phase 4 wires global symbol search
toolbar->addWidget(search);
```

It is a **local stack variable**, **disabled**, with **no signals connected**. The
placeholder mentions `Ctrl+T` and "global-symbol search," but nothing is wired.

### The working search path

`SearchPanel` (`ui/src/SearchPanel.{h,cpp}`) is fully functional:

- `m_query` `QLineEdit` (`SearchPanel.h:43`) → `textChanged` → `scheduleSearch()`
  (220ms debounce) → `runSearch()` (`SearchPanel.cpp:160`).
- Calls IPC `m_core->call("search.code", params, cb)` (`SearchPanel.cpp:197`), params:
  `query, root, regex, caseSensitive, wholeWord, includes, excludes, maxResults`.
- Core handler `search.code` (`core/cmd/akcore/main.go:1499-1530`) → `search.Run(...)`
  (`core/internal/search/search.go`), which shells out to ripgrep and returns
  `{files:[{path,matches:[{line,column,preview}]}], truncated, total}`.
- Results render into a tree; activating a row emits `resultActivated(path,line)`.
- `SearchPanel::focusQuery()` (`SearchPanel.cpp:144-148`) focuses + selects-all the
  query field.

### Panel reveal plumbing (already exists)

- The panel is registered under the stable key `m_keySearch` ("search") on the left
  strip (`MainWindow.cpp:177-178`).
- `MainWindow::raisePanelByKey(key)` (`MainWindow.cpp:716`) raises/expands a panel.
- There's already a **Ctrl+Shift+F** action ("Find in project") that does exactly
  `raisePanelByKey(m_keySearch); m_search->focusQuery();` (`MainWindow.cpp:505-514`).

So the entire backend + reveal mechanism exists — only the toolbar box is unhooked.

## Design decision — which "Search tool"?

The placeholder said "global-symbol search" (an LSP workspace-symbol idea), but the
user's intent is "the Search tool that works" = the **ripgrep code Search panel**.
Wire it to `SearchPanel`. (A symbol-search mode can be a later toggle; LSP plumbing
already exists under `ui/src/lsp/` and `core` LSP host, but it's a separate feature.)

## Proposed implementation

Minimal, self-contained — no protocol changes.

1. **Promote the widget to a member.** Replace the local `auto *search` with
   `m_toolbarSearch` (add to `MainWindow.h`). Drop `setEnabled(false)`. Update the
   placeholder to `i18n("Search project…  (Ctrl+Shift+F)")` (match the real shortcut,
   or wire `Ctrl+T` too — see step 4).
2. **Connect Enter.** `connect(m_toolbarSearch, &QLineEdit::returnPressed, this, [this]{ ... })`:
   - `const QString q = m_toolbarSearch->text(); if (q.isEmpty()) return;`
   - `raisePanelByKey(m_keySearch);`
   - set the panel's query and run it. Add a small public method
     `SearchPanel::search(const QString &query)` that sets `m_query` text (which already
     triggers the debounced search) and `focusQuery()` / focuses results. Prefer this
     over reaching into `m_query` from `MainWindow` (keep encapsulation).
3. **Optional: live-as-you-type.** Also connect `textChanged` to forward into the panel
   if it's already visible, so the toolbar box and panel stay in sync. Keep it simple:
   Enter-to-search is enough for v1; add live forwarding only if desired.
4. **Shortcut.** Either repoint the placeholder text to the existing **Ctrl+Shift+F**,
   or add a dedicated **Ctrl+T** action that focuses `m_toolbarSearch`
   (`m_toolbarSearch->setFocus(); selectAll();`). Pick one and make the placeholder
   match what actually works (avoid the current mismatch).

## Risks / considerations

- **Keep one source of truth.** Forward the query *into* `SearchPanel` and let its
  existing debounce/RPC path run; don't duplicate the `search.code` call from
  `MainWindow`. This avoids divergent behavior and double requests.
- **Root scoping.** `SearchPanel::runSearch` already computes `root` from the active
  workspace (and the project per [workspace-scoping]); driving it through the panel
  inherits that automatically. Calling `search.code` directly from the toolbar would
  bypass it — another reason to route through the panel.
- **Empty/whitespace queries** should no-op (don't reveal the panel on a stray Enter).
- Trivial, low-risk, no core changes — good first item to ship.

## Acceptance

Type in the toolbar box, press Enter → Search panel opens on the left, shows ripgrep
results for the active project, query text carried over; results clickable as today.
