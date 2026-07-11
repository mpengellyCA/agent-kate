// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStyledItemDelegate>

// LogGraphDelegate paints the lane-graph column of the log table. Each row
// gets:
//   - half-lines descending from the row above for every lane in LanesIn;
//   - half-lines ascending into the row below for every lane in LanesOut;
//   - a filled circle marking this commit's own Lane.
//
// Colors cycle through six palette-derived hues by `lane % 6` so the same
// branch keeps the same color from page to page. The column's preferred width
// scales with the busiest lane seen so far.
class LogGraphDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    explicit LogGraphDelegate(QObject *parent = nullptr);

    // The viewer calls this whenever the model's maxLane grows so the column
    // can resize. We don't watch the model directly — the viewer already
    // listens for rowsInserted and is the single source of truth for layout.
    void setMaxLane(int maxLane);
    int maxLane() const { return m_maxLane; }

    // While a search filter hides rows, the surviving rows are non-contiguous,
    // so the lane rails would connect through hidden commits and look corrupt.
    // The viewer sets this true while filtering; the delegate then paints only
    // the row background (no rails, no node). The caller triggers a repaint.
    void setSuppressLanes(bool suppress) { m_suppressLanes = suppress; }

    QSize sizeHint(const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) const override;
    void paint(QPainter *painter, const QStyleOptionViewItem &opt,
               const QModelIndex &idx) const override;

private:
    static constexpr int kLaneStep = 14; // horizontal spacing between lanes
    static constexpr int kLanePad = 8;   // left padding before lane 0
    static constexpr int kNodeRadius = 4;

    int m_maxLane = 0;
    bool m_suppressLanes = false;
};
