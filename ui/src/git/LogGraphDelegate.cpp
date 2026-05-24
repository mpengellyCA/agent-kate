// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#include "LogGraphDelegate.h"
#include "LogModel.h"

#include <QApplication>
#include <QColor>
#include <QPainter>
#include <QPalette>
#include <QStyle>

namespace {
// Six perceptually distinct hues, chosen to read on both light and dark
// Breeze palettes. We don't pull from QPalette here because the lane palette
// needs to stay stable as the user scrolls — palette roles cycle hue with
// theme and would scramble lane identity.
QColor laneColor(int lane)
{
    static const QColor kPalette[6] = {
        QColor(0x35, 0x8c, 0xe6), // azure
        QColor(0xe6, 0x7e, 0x22), // amber
        QColor(0x2e, 0xc4, 0x71), // emerald
        QColor(0xe7, 0x4c, 0x3c), // vermilion
        QColor(0x9b, 0x59, 0xb6), // amethyst
        QColor(0x1a, 0xbc, 0x9c), // teal
    };
    return kPalette[((lane % 6) + 6) % 6];
}
} // namespace

LogGraphDelegate::LogGraphDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
}

void LogGraphDelegate::setMaxLane(int maxLane)
{
    if (maxLane > m_maxLane) {
        m_maxLane = maxLane;
    }
}

QSize LogGraphDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                 const QModelIndex &idx) const
{
    QSize base = QStyledItemDelegate::sizeHint(opt, idx);
    int width = (m_maxLane + 1) * kLaneStep + kLanePad * 2;
    if (base.width() > width) {
        width = base.width();
    }
    return {width, base.height()};
}

void LogGraphDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                             const QModelIndex &idx) const
{
    // Draw the standard row background (selection, alternation, hover) first
    // so the lane lines sit on top of it without us reimplementing styling.
    QStyleOptionViewItem styled = opt;
    initStyleOption(&styled, idx);
    styled.text.clear();
    QStyle *style = styled.widget ? styled.widget->style() : QApplication::style();
    style->drawControl(QStyle::CE_ItemViewItem, &styled, painter, styled.widget);

    const int lane = idx.data(LogModel::LaneRole).toInt();
    const QList<int> lanesIn = idx.data(LogModel::LanesInRole).value<QList<int>>();
    const QList<int> lanesOut = idx.data(LogModel::LanesOutRole).value<QList<int>>();

    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);

    const QRect r = opt.rect;
    const int yMid = r.top() + r.height() / 2;

    auto laneX = [&](int l) { return r.left() + kLanePad + l * kLaneStep; };

    QPen pen;
    pen.setWidthF(1.6);
    pen.setCapStyle(Qt::FlatCap);

    // Half-lines descending from above (this row's "lanesIn"): vertical from
    // top edge down to the node, then horizontal across to this commit's lane.
    for (int l : lanesIn) {
        pen.setColor(laneColor(l));
        painter->setPen(pen);
        const int x = laneX(l);
        painter->drawLine(x, r.top(), x, yMid);
        if (l != lane) {
            painter->drawLine(x, yMid, laneX(lane), yMid);
        }
    }

    // Half-lines ascending into the row below (this row's "lanesOut").
    for (int l : lanesOut) {
        pen.setColor(laneColor(l));
        painter->setPen(pen);
        const int x = laneX(l);
        painter->drawLine(x, yMid, x, r.bottom());
        if (l != lane) {
            painter->drawLine(laneX(lane), yMid, x, yMid);
        }
    }

    // Node circle at this commit's own lane. A subtle ring border picks up
    // the row background so the node always reads, even on a selected row.
    const int nx = laneX(lane);
    const QColor c = laneColor(lane);
    painter->setBrush(c);
    QColor ring = opt.palette.color(QPalette::Base);
    if ((opt.state & QStyle::State_Selected) != 0) {
        ring = opt.palette.color(QPalette::Highlight);
    }
    QPen nodePen(ring);
    nodePen.setWidthF(1.5);
    painter->setPen(nodePen);
    painter->drawEllipse(QPointF(nx, yMid), kNodeRadius, kNodeRadius);

    painter->restore();
}
