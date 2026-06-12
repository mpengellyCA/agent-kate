# 07 — Document & Media Viewing (Okular-style) in the Editor

## Goal

View non-code files **inside** Agent Kate's editor tabs — PDFs (full, Okular-style,
not a static first-page image), CSVs, images, video, audio, and Office documents
(Word/Excel/PowerPoint, ODF) — so Agent Kate is useful for office-like tasks. Scope
to **what's achievable without hoops**: lean on existing KDE/Qt facilities and
gracefully degrade when a format needs something not installed.

## Current state

- Dispatch lives in `EditorArea::openFile` (`EditorArea.cpp:108-176`): the only
  non-text branch is `if (ImageView::canDisplay(abs))` (`:142`) → host `ImageView`;
  everything else goes to a `KTextEditor` view.
- `ImageView` (`ui/src/ImageView.{h,cpp}`) decodes via **`QImageReader`** only
  (`ImageView.cpp:14-25,31-33`). It is a static raster viewer (fit / 1:1 / zoom).
- **Why a PDF "previews" today**: the system has `libqpdf.so` installed as a Qt image
  plugin, so `QImageReader::supportedImageFormats()` includes `pdf`. `ImageView`
  therefore renders **only page 1, as a flat image** — no scrolling, search, or
  multi-page. Same would apply to TIFF.
- **KParts embedding is already proven in this codebase.** `TerminalPanel::createSession`
  (`TerminalPanel.cpp:130-164`) does exactly what we need for documents:
  `KPluginFactory::loadFactory(KPluginMetaData("kf6/parts/konsolepart"))` →
  `factory->create<KParts::ReadOnlyPart>(container, container)` → add `part->widget()`
  to a layout, with a clean "couldn't load — install X" fallback label. `KF6::Parts`
  is already linked (`ui/CMakeLists.txt`).
- **Installed parts on this machine** (`/usr/lib/qt6/plugins/kf6/parts/`): `gvpart`
  (Gwenview, images), `arkpart` (archives), `katepart` (text), `kfontviewpart`
  (fonts), `konsolepart`, `dolphinpart`, plus **`okularpart` (okular + okularparts
  now installed)** and **Calligra** (Words/Sheets/Stage) parts. Calligra **Plan**
  is also installed (project-management part) if ever useful. This means the full
  PDF/ODF/MS-Office viewing path is available locally today — verify the exact part
  names before coding (`kreadconfig6`/`ls` the parts dir).

## Key idea: a generic MIME-dispatched KPart host

The non-hoops, future-proof backbone is a single new widget that, given a file, asks
KDE "**is there a viewer KPart for this MIME type?**" and embeds it — automatically
adopting whatever parts the user has installed, with no per-format code.

Use **`KParts::PartLoader`** (KF6) rather than hardcoded plugin ids:

- `KParts::PartLoader::partsForMimeType(mime)` → is anything available?
- `KParts::PartLoader::createPartInstanceForMimeType<KParts::ReadOnlyPart>(mime, parentWidget, parent)`
  → instantiate the user's preferred viewer part.
- then `part->openUrl(QUrl::fromLocalFile(path))` and host `part->widget()` exactly as
  `TerminalPanel` hosts the konsole widget.

This one widget (`KPartView`) covers PDF (Okular), archives (Ark), fonts (KFontView),
ODF/Office (Calligra/Okular generators), and more — **whatever is installed** — for
free. When nothing is available it shows the existing "install X to view this" message
and an **Open externally** button (KIO/`QDesktopServices`).

## Format-by-format plan (tiered by effort/availability)

### Tier 1 — easy, high value (generic KPart + one small native viewer)

| Type | Approach | Notes |
|------|----------|-------|
| **PDF** | `KPartView` → `okularpart` | Full Okular: multi-page, scroll, zoom, search, TOC, text selection, annotations (view). **Requires the `okular` package.** Recommend as a packaging dependency/Recommends. |
| **Images** | keep `ImageView` (or optionally `gvpart`) | `ImageView` is fine and native; Gwenview part is an optional upgrade. |
| **CSV / TSV** | **new native `CsvView`** = `QTableView` + tiny `QAbstractTableModel` | Better than any part: sortable, selectable, frozen header. ~1 small file. No dependency. |
| **Fonts** | `KPartView` → `kfontviewpart` | Already installed here; free via the generic loader. |
| **Archives (zip/tar/…)** | `KPartView` → `arkpart` | Already installed; nice bonus for inspecting `.zip` etc. |

Okular's part also covers, when okular is installed: **ePub, DjVu, CHM, comic books
(cbz/cbr), TIFF, Markdown, and — via its office generators — ODT/ODS/ODP and MS Office**
(the latter by delegating to a LibreOffice/Calligra backend). So "install okular"
unlocks most of the document story in one move.

### Tier 2 — moderate, optional dependency

| Type | Approach | Notes |
|------|----------|-------|
| **Video / Audio** | `Qt6::Multimedia` (`QMediaPlayer` + `QVideoWidget`, `QAudioOutput`) in a new `MediaView` | Cleanest, but **adds a Qt component** to the build + a working multimedia backend (FFmpeg/GStreamer). Gate behind a CMake `find_package(Qt6 OPTIONAL_COMPONENTS Multimedia)` so the build degrades gracefully. Alternative: a media KPart if present — but QtMultimedia is more predictable. |

### Tier 3 — the "hoops" zone (be honest, don't over-build)

| Type | Approach | Notes |
|------|----------|-------|
| **Word / Excel / PowerPoint (.docx/.xlsx/.pptx)** | Prefer Okular's office generator (needs a LibreOffice backend) **or** Calligra parts if installed; otherwise **Open externally** | There is **no painless pure-Qt/KDE renderer** for MS Office formats. Do **not** write a custom docx/xlsx/pptx renderer. The generic `KPartView` already picks up Calligra/Okular-office automatically when present; when absent, fall back to "Open externally." |
| **ODF (.odt/.ods/.odp)** | same as above via `KPartView` | Works automatically if Okular-office or Calligra is installed. |

For Excel specifically, a *lightweight* future option is to treat `.xlsx`/`.ods`
read-only via a spreadsheet library into the same `CsvView` table — but that's a
follow-up, not v1.

## Dispatch design (EditorArea)

Replace the single `ImageView::canDisplay` branch with an ordered resolver. Proposed
precedence in `EditorArea::openFile` (around `EditorArea.cpp:142`):

1. **Markdown** → `MarkdownView` (see [02-markdown-preview.md]).
2. **CSV/TSV** → `CsvView`.
3. **Raster images** → `ImageView` — but **narrow `ImageView::canDisplay` to true
   raster formats** and explicitly **exclude `pdf`** (and arguably `tiff`/multi-page),
   so PDFs no longer get hijacked into the static first-page path.
4. **Generic `KPartView`** if `PartLoader::partsForMimeType(mime)` is non-empty
   (covers PDF→Okular, archives, fonts, ODF/Office when their parts exist). Resolve
   MIME via `QMimeDatabase` (by content+name), not just extension.
5. **Media** → `MediaView` if `Qt6::Multimedia` was built in and the type is audio/video.
6. **Else** → existing KTextEditor view (text), or for clearly-binary unknown types a
   small "no viewer — Open externally / Reveal in file manager" placeholder reusing
   the KIO actions already used elsewhere.

All hosts follow the established tab contract: a `QWidget` added via
`tabs->addTab(widget, name)`, deferred-deleted on close like `ImageView`
(`EditorArea.cpp:237-253`). `KPartView` must delete its part on close (the part owns
its widget), mirroring `TerminalPanel`'s `deleteLater()` teardown
(`TerminalPanel.cpp:32-38`).

## Implementation steps

1. **`KPartView` (`ui/src/KPartView.{h,cpp}`)** — generic host:
   - `static bool canDisplay(const QString &path)` → resolve MIME via `QMimeDatabase`,
     return `!KParts::PartLoader::partsForMimeType(mime).isEmpty()` (and exclude types
     we handle better natively — text, plain images, csv, md).
   - ctor: `createPartInstanceForMimeType<KParts::ReadOnlyPart>(mime, this, this)`,
     `openUrl(...)`, host `part->widget()`; on failure show the install/open-externally
     fallback (copy `TerminalPanel`'s label pattern).
   - expose `path()` for `EditorArea`'s polymorphic loops; delete the part on dtor.
2. **`CsvView` (`ui/src/CsvView.{h,cpp}`)** — `QTableView` + streaming CSV model
   (handle quoting/embedded commas/newlines; detect `,` vs `\t`). Read-only v1.
3. **Narrow `ImageView::canDisplay`** to exclude `pdf` (and let `KPartView` claim it).
4. **Rework `EditorArea::openFile` dispatch** into the ordered resolver above; update
   the polymorphic spots (`emitCurrentFile` `:255-267`, `openFilePaths` `:216-235`,
   `closeTabIn` `:237-253`, existing-tab reuse `:120-140`) to recognize the new widgets.
5. **Optional `MediaView`** behind `find_package(Qt6 OPTIONAL_COMPONENTS Multimedia)`;
   compile out cleanly when absent.
6. **CMake**: add new sources to `ui/CMakeLists.txt`; link `Qt6::Multimedia` only if
   found.
7. **Packaging** (`packaging/`): add **okular** as a Recommends/optional runtime dep so
   the PDF/office story works out of the box; document that media needs a QtMultimedia
   backend. (Per [kde-native-design] these are all native KDE components — no bundling.)

## Risks / considerations

- **Read-only by design.** `KParts::ReadOnlyPart` views; we are not building editors
  for these formats. Okular allows view-time annotations but we won't wire save paths
  in v1. Keep it a *viewer*, matching the user's "Okular-like abilities."
- **Missing parts must degrade, not crash.** Always check `partsForMimeType`/factory
  result and fall back to the message + "Open externally" (the `TerminalPanel`
  `m_konsoleMissing` pattern is the template). No hard dependency on okular at build
  time.
- **MIME resolution by content**, not extension alone (`QMimeDatabase`), so
  extension-less or mislabeled files route correctly — and so we don't regress the
  current extension-based image path.
- **Don't double-claim PDFs.** Until `ImageView::canDisplay` is narrowed, both
  ImageView (via libqpdf) and KPartView could claim `.pdf`; fix the precedence in the
  same change.
- **Office (MS) is genuinely environment-dependent.** Set expectations in UI copy:
  when no office-capable part is found, the tab offers "Open externally" rather than
  pretending to render. This honors "support what is possible, don't over-complicate."
- **Security/perf**: large PDFs/media load in-process via the part; that's how Okular
  itself behaves, acceptable. Archives via arkpart are read-only listings.

## Acceptance

- With `okular` installed, opening a multi-page PDF gives a full scrollable, searchable
  document (not a single static page).
- A `.csv` opens as a sortable table.
- A `.zip`/`.ttf` opens in its KPart when available; an unsupported/`.docx`-without-backend
  file shows a clear "Open externally" tab instead of garbled text or a crash.
- Images still open as today; nothing that currently works regresses.
