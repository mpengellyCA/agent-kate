// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QFont>
#include <QHash>
#include <QJsonObject>
#include <QModelIndex>
#include <QPixmap>
#include <QPointer>
#include <QRect>
#include <QStyledItemDelegate>

class QTextDocument;

// TranscriptDelegate paints one transcript row (note / message / tool card) by
// drawing its pre-rendered HTML through a QTextDocument — the same engine the
// old per-message QLabel used, so markdown and code-block fidelity is preserved.
//
// The whole point of plan-10 phase 2 is that resize is O(visible rows): the
// delegate caches each row's measured height keyed by (stableId, width). The
// view only asks sizeHint() for the rows it actually shows, so a viewport-width
// change re-measures just those. A row's cache entry is bust when the model
// signals heightInvalidated for that row (on any data mutation) or when the
// width differs.
//
// It also caches the laid-out body QTextDocument per row. sizeHint() and paint()
// used to build one each, so a streaming message paid two full layouts of the
// whole accumulated text per 50ms tick; now paint reuses the document sizeHint
// laid out, halving the O(n²) a long stream costs.
class TranscriptDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    explicit TranscriptDelegate(QObject *parent = nullptr);
    ~TranscriptDelegate() override;

    void paint(QPainter *painter, const QStyleOptionViewItem &opt,
               const QModelIndex &idx) const override;
    QSize sizeHint(const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) const override;

    // True once a row's height has been measured at a width that differs from the
    // current viewport width — i.e. the cached height is a stale estimate and the
    // view should schedule a settle-time re-measure of its visible rows. The
    // panel polls this after a resize and clears it via measureExact().
    bool hasStaleHeights() const { return m_dirtyResize; }
    void clearStaleFlag() { m_dirtyResize = false; }

    // Force an exact (re-)measure of one row at `width`, updating the cache.
    // Returns the freshly measured height. Used by the settle pass to refine the
    // currently-visible rows after the cheap estimate phase of a resize.
    int measureExact(const QModelIndex &idx, int width,
                     const QStyleOptionViewItem &opt) const;

    // Drop everything cached for one row — its measured height and its laid-out
    // body document. Connected automatically to TranscriptModel::heightInvalidated
    // the first time this delegate measures a row of that model, so a mutation
    // busts exactly that row instead of the model minting a fresh identity (which
    // left one dead cache entry per streaming tick behind).
    void invalidateRow(quintptr stableId) const;

    // An attachment's resolved on-disk location plus the stat fields the
    // thumbnail cache keys on. `path` is empty when nothing readable was found.
    struct ResolvedAttachment {
        QString path;
        qint64 size = -1;
        qint64 mtime = -1;
    };
    // Resolve an attachment chip to a file, memoised with a short TTL. Painting
    // a chip used to run resolveAttachmentPath (up to three stats) plus a
    // QFileInfo for the pixmap key on EVERY paint of every scroll tick; now the
    // common case is one hash lookup. Public because the row painter is a free
    // function; not part of the delegate's contract with the view.
    ResolvedAttachment resolveAttachmentCached(const QJsonObject &att) const;

    // That attachment's chip thumbnail, decoded at icon size and memoised in
    // the same entry — so a repaint of an unchanged chip costs one hash lookup
    // instead of rebuilding a QPixmapCache key and taking its global mutex
    // (audit F18). Public for the same reason as above.
    QPixmap chipPixmap(const QJsonObject &att, int edge, qreal dpr) const;

    // The laid-out body document for a row, owned by the delegate's cache — the
    // caller must NOT delete it. `contentWidth` is the text width to wrap at.
    // Shares the document setup with the selection overlay (createEditor) via
    // configureBodyDoc() so glyph metrics match exactly. Public for the same
    // reason as above: the row painter is a free function.
    QTextDocument *bodyDoc(const QModelIndex &idx, int contentWidth,
                           const QStyleOptionViewItem &opt) const;

    // The body HTML a Message/Note row shows: the pre-rendered HtmlRole, with
    // the find highlight substituted in for a matching row. One routine for the
    // painted row and the selection overlay so both show identical content.
    // The highlighted form is cached per row keyed on (needle, current-row):
    // highlightedHtml escapes and scans the row's whole plain text, and it used
    // to be rebuilt on every paint of every matching row for the life of the
    // find bar. Same invalidation as the doc caches (invalidateRow), capped
    // like them too. Public for the same reason as bodyDoc.
    QString resolveBodyHtml(const QModelIndex &idx) const;

    // The two mono documents of an EXPANDED tool row: its input detail and its
    // result. They used to be heap-allocated, laid out and destroyed on every
    // single paint — two full layouts of up to the whole (unbounded, after
    // "Show full output") text per frame, which is what made scrolling past a
    // big expanded row jank (audit F18). Now they live in the same per-row cache
    // bodyDoc uses, invalidated through the same invalidateRow path. Owned by
    // the cache: callers must NOT delete them.
    enum class ToolSlot { Detail, Result };
    QTextDocument *toolDoc(const QModelIndex &idx, ToolSlot slot, const QString &plain,
                           const QFont &mono, int contentWidth) const;

    // --- in-place selectable overlay (plan 13 phase 1) -------------------
    // A Message row's body can be covered by a frameless read-only QTextBrowser
    // so the user can select and copy an arbitrary substring. The browser reuses
    // the exact document setup paint() uses, so glyph positions match the painted
    // row and opening the overlay causes no visual jump.
    QWidget *createEditor(QWidget *parent, const QStyleOptionViewItem &opt,
                          const QModelIndex &idx) const override;
    void setEditorData(QWidget *editor, const QModelIndex &idx) const override;
    void updateEditorGeometry(QWidget *editor, const QStyleOptionViewItem &opt,
                              const QModelIndex &idx) const override;

Q_SIGNALS:
    // A left click landed inside a Message row's body (and not on a link) — the
    // panel opens a persistent selection overlay for this row.
    void messageBodyClicked(const QModelIndex &idx) const;
    // A selection-overlay editor was just created; the panel takes a handle so it
    // can focus it and dismiss it on Esc / click-outside (the view offers no
    // getter for a persistent delegate editor).
    void editorCreated(QWidget *editor) const;
    // A link inside a Message body's selection overlay was activated.
    void anchorActivated(const QString &href) const;
    // An attachment chip under a You message was clicked; the payload is the
    // compact attachment object (name/kind/path/mediaType/outside).
    void attachmentActivated(const QJsonObject &att) const;
    // The tool card's "open in inspector" glyph was clicked — the panel opens the
    // ToolInspectorDialog for this row (plan 13 phase 5).
    void inspectToolRequested(const QModelIndex &idx) const;

protected:
    // Hit-testing for tool expand/collapse, the copy button, the "show full
    // output" link and message links; a body click asks the panel to open the
    // selection overlay.
    bool editorEvent(QEvent *event, QAbstractItemModel *model,
                     const QStyleOptionViewItem &opt,
                     const QModelIndex &idx) override;

    // Tooltip routing: an attachment chip shows its full path (plus an
    // "(outside workspace)" marker when flagged); the tool card's copy/inspect
    // glyphs show a short label.
    bool helpEvent(QHelpEvent *event, QAbstractItemView *view,
                   const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) override;

private:
    struct CacheEntry {
        int width = -1;  // width the height was last measured at
        int height = 0;  // measured height
    };
    // Keyed by the model's per-row stableId. On a width change sizeHint returns
    // the cached height *as an estimate* without rebuilding the QTextDocument —
    // so the view's O(N) total-height pass during an interactive resize costs
    // only hash lookups, not N text layouts. m_dirtyResize then signals the
    // panel to schedule an exact re-measure of just the visible rows once the
    // resize settles (measureExact), making steady-state cost O(visible rows).
    mutable QHash<quintptr, CacheEntry> m_heightCache;
    mutable bool m_dirtyResize = false;

    // One row's laid-out body document, kept so measure and paint share a single
    // layout. `html` and `width` are what it was laid out for: a mismatch means
    // the row's content or wrap width moved and the document is re-set (which is
    // where the layout cost is), otherwise it is reused as-is.
    struct DocEntry {
        int width = -1;
        QString html;
        QFont font;  // an app font change must re-lay the document, not repaint it stale
        // The palette generation the document was laid out under. The body HTML
        // carries inline `palette(highlight)` / `palette(text)` CSS, and Qt
        // resolves those to concrete QColors at setHtml() time — a cached
        // document therefore keeps painting the OLD theme's colours after a
        // theme switch, since neither the html nor the width nor the font moved.
        // Comparing the generation is what forces the re-lay. (AgentKate ships a
        // signature Midnight theme alongside Breeze, so this is user-visible.)
        int paletteGen = 0;
        QTextDocument *doc = nullptr;
    };
    mutable QHash<quintptr, DocEntry> m_docCache;
    // The same thing for the expanded tool row's two mono documents. Separate
    // hashes rather than a compound key so one row's three documents are still
    // dropped by one invalidateRow lookup each, and so the caps are independent
    // (a viewport full of expanded tool rows must not evict the body docs).
    // `html` holds the PLAIN text these were laid out from; paletteGen is
    // unused (plain text carries no palette CSS — the pen comes from the paint
    // context, so a theme switch needs a repaint, not a re-layout).
    mutable QHash<quintptr, DocEntry> m_detailCache;
    mutable QHash<quintptr, DocEntry> m_resultCache;

    // One row's find-highlighted body HTML, keyed on what it was built for. A
    // stale (needle, current) pair rebuilds in place, so the cache holds at
    // most one entry per row; invalidateRow drops it with the row's documents.
    struct HighlightEntry {
        QString needle;
        bool current = false;
        QString html;
    };
    mutable QHash<quintptr, HighlightEntry> m_highlightCache;

    // Bumped whenever the application/widget palette changes (ThemeManager theme
    // switch, or a system colour-scheme change arriving as
    // QEvent::ApplicationPaletteChange). Part of the document cache key above.
    mutable int m_paletteGen = 0;

    // The view widget this delegate has installed its palette watcher on, bound
    // lazily from the first measure/paint (like bindModel) so a bare delegate +
    // view pair needs no external wiring. QPointer: the view can outlive-or-not
    // the delegate in either order.
    mutable QPointer<QWidget> m_paletteWatched;
    void watchPalette(const QStyleOptionViewItem &opt) const;
    bool eventFilter(QObject *watched, QEvent *event) override;

    // Attachment path resolution + stat fields, memoised (see
    // resolveAttachmentCached). `checkedMs` is the monotonic-ish wall clock at
    // the last stat; entries older than the TTL are re-stat'ed.
    struct AttEntry {
        ResolvedAttachment r;
        qint64 checkedMs = 0;
        // The chip thumbnail this resolution decoded to, if any. Dropped with
        // the entry when the TTL expires and the file is re-stat'ed, which is
        // what lets a rewritten screenshot refresh.
        QPixmap thumb;
        int thumbEdge = -1;
        qreal thumbDpr = 0;
        bool thumbValid = false;
    };
    mutable QHash<QString, AttEntry> m_attCache;
    AttEntry &attEntry(const QString &key, const QJsonObject &att) const;

    // Models whose heightInvalidated signal this delegate has bound to. Bound
    // lazily from sizeHint (which is const) rather than by the panel, so a bare
    // delegate + model pair works with no external wiring.
    void bindModel(const QModelIndex &idx) const;


    // Geometry helpers shared by paint() and editorEvent(): given the row rect
    // and index, compute the sub-rects of the tool header / copy button.
    QRect toolHeaderRect(const QRect &row) const;
    QRect toolCopyRect(const QRect &row) const;
    // The "open in inspector" glyph sits immediately left of the copy glyph.
    QRect toolInspectRect(const QRect &row) const;

    // The body-text sub-rect of a Message row (excludes the card padding, the
    // role/timestamp line, and any attachment chip block) — the exact area the
    // selection overlay covers. `row` is the full row rect; `opt` supplies the
    // font; `idx` supplies the attachments so the chip block below the body is
    // excluded. Matches the translate the body draw uses in layoutRow().
    QRect messageBodyRect(const QRect &row, const QStyleOptionViewItem &opt,
                          const QModelIndex &idx) const;

    // Height of the attachment chip block for a Message row (0 if none),
    // including the gap above it — used by messageBodyRect and layoutRow so the
    // overlay never covers the chips.
    int attachmentsBlockHeight(const QModelIndex &idx,
                               const QStyleOptionViewItem &opt, int innerW) const;

    // The index of the attachment chip a point falls in (in the row's device
    // coordinates), or -1 — used by editorEvent to open the clicked attachment.
    int attachmentChipAt(const QRect &row, const QStyleOptionViewItem &opt,
                         const QModelIndex &idx, const QPoint &pos) const;
};
