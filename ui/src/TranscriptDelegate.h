// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QHash>
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
// bumps that row's stableId (on any data mutation) or when the width differs.
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

protected:
    // Hit-testing for tool expand/collapse, the copy button, the "show full
    // output" link, message links and double-click-to-copy.
    bool editorEvent(QEvent *event, QAbstractItemModel *model,
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

    // Build (and configure widths on) the text document for a row's body.
    // Returns a freshly-built document the caller owns. `contentWidth` is the
    // text width to wrap at.
    QTextDocument *buildBodyDoc(const QModelIndex &idx, int contentWidth,
                                const QStyleOptionViewItem &opt) const;

    // Geometry helpers shared by paint() and editorEvent(): given the row rect
    // and index, compute the sub-rects of the tool header / copy button.
    QRect toolHeaderRect(const QRect &row) const;
    QRect toolCopyRect(const QRect &row) const;
};
