// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AgentCardDelegate.h"

#include <QApplication>
#include <QColor>
#include <QFont>
#include <QFontMetrics>
#include <QIcon>
#include <QModelIndex>
#include <QPainter>
#include <QPalette>
#include <QStyle>

namespace {
// Card geometry, all kept in font-metric-relative terms at use-time so the row
// survives font scaling / HiDPI. These are the fixed paddings only.
constexpr int kPadH    = 8; // left/right inset from the row rect
constexpr int kPadV    = 6; // top/bottom inset
constexpr int kDotDiam = 10; // status-dot diameter
constexpr int kDotGap  = 8;  // gap between dot and text column
constexpr int kLineGap = 2;  // gap between title and subtitle lines
constexpr int kBadgeHPad = 6; // horizontal padding inside the "#N" badge
constexpr int kBadgeGap  = 6; // gap between title text and the badge
constexpr int kBadgeRadius = 4;

QFont titleFontFor(const QFont &base, bool dormant)
{
    QFont f = base;
    f.setBold(true);
    f.setItalic(dormant); // dormant agents read as resumable history
    return f;
}

QFont subtitleFontFor(const QFont &base, bool dormant)
{
    QFont f = base;
    if (f.pointSizeF() > 0) {
        f.setPointSizeF(f.pointSizeF() * 0.9);
    }
    f.setItalic(dormant);
    return f;
}
} // namespace

AgentCardDelegate::AgentCardDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
}

void AgentCardDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                              const QModelIndex &idx) const
{
    // Project rows keep the plain default look.
    if (!idx.parent().isValid()) {
        QStyledItemDelegate::paint(painter, opt, idx);
        return;
    }

    // Let the style paint background, hover and selection — but not the text,
    // which we lay out ourselves across two lines.
    QStyleOptionViewItem styled = opt;
    initStyleOption(&styled, idx);
    styled.text.clear();
    styled.icon = QIcon();
    styled.features &= ~QStyleOptionViewItem::HasDecoration;
    QStyle *style = styled.widget ? styled.widget->style() : QApplication::style();
    style->drawControl(QStyle::CE_ItemViewItem, &styled, painter, styled.widget);

    const bool selected = opt.state & QStyle::State_Selected;
    const bool current  = opt.state & QStyle::State_HasFocus;
    const bool dormant  = idx.data(AgentRoles::Dormant).toBool();
    const QString title    = idx.data(AgentRoles::Title).toString();
    const QString subtitle = idx.data(AgentRoles::Subtitle).toString();
    const int number       = idx.data(AgentRoles::Number).toInt();
    const QString dotHex   = idx.data(AgentRoles::Dot).toString();
    // A background agent that needs the user's input gets a palette-driven
    // marker — but only while it isn't the row the user is already looking at.
    const bool attention = idx.data(AgentRoles::Attention).toBool()
                           && !selected && !current;

    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);

    const QRect r = opt.rect.adjusted(kPadH, kPadV, -kPadH, -kPadV);

    const QFont titleFont = titleFontFor(opt.font, dormant);
    const QFont subFont = subtitleFontFor(opt.font, dormant);
    const QFontMetrics fmTitle(titleFont);
    const QFontMetrics fmSub(subFont);

    const QRect titleLine(r.left(), r.top(), r.width(), fmTitle.height());
    const QRect subLine(r.left(), titleLine.bottom() + kLineGap, r.width(), fmSub.height());

    // Text colours from the palette so we follow Breeze. Dormant rows read a
    // touch fainter; the subtitle is always the muted (placeholder) tone unless
    // the row is selected, where it rides the highlight foreground.
    QColor titleColor = opt.palette.color(selected ? QPalette::HighlightedText : QPalette::Text);
    QColor subColor = selected ? opt.palette.color(QPalette::HighlightedText)
                               : opt.palette.color(QPalette::PlaceholderText);
    if (dormant && !selected) {
        titleColor = opt.palette.color(QPalette::PlaceholderText);
    }

    // Status dot, vertically centred on the title line.
    const int dotY = titleLine.center().y() - kDotDiam / 2 + 1;
    if (!dotHex.isEmpty()) {
        painter->setPen(Qt::NoPen);
        painter->setBrush(QColor(dotHex));
        painter->drawEllipse(r.left(), dotY, kDotDiam, kDotDiam);
    }

    const int textX = r.left() + kDotDiam + kDotGap;
    int titleRight = r.right();

    // "Needs your input" marker, painted at the row's right edge in the
    // palette Highlight colour so it tracks Breeze and reads as actionable.
    // Drawn first so the "#N" badge (if any) tucks to its left.
    if (attention) {
        const int markD = kDotDiam;
        const int markX = r.right() - markD;
        const int markY = titleLine.center().y() - markD / 2 + 1;
        painter->setPen(Qt::NoPen);
        painter->setBrush(opt.palette.color(QPalette::Highlight));
        painter->drawEllipse(markX, markY, markD, markD);
        titleRight = markX - kBadgeGap;
    }

    // "#N" worktree badge on the right of the title line.
    if (number > 0) {
        const QString badge = QStringLiteral("#%1").arg(number);
        const int bw = fmTitle.horizontalAdvance(badge) + kBadgeHPad * 2;
        // Anchor to titleRight (the marker's left edge if a marker was drawn,
        // else the row's right edge) so the two never overlap.
        const QRect badgeRect(titleRight - bw, titleLine.top(), bw, titleLine.height());
        painter->setPen(Qt::NoPen);
        painter->setBrush(opt.palette.color(selected ? QPalette::Highlight
                                                     : QPalette::AlternateBase));
        // On a selected row Highlight==background, so outline instead of fill.
        if (selected) {
            painter->setBrush(Qt::NoBrush);
            painter->setPen(opt.palette.color(QPalette::HighlightedText));
        }
        painter->drawRoundedRect(badgeRect, kBadgeRadius, kBadgeRadius);
        painter->setPen(selected ? opt.palette.color(QPalette::HighlightedText)
                                 : opt.palette.color(QPalette::PlaceholderText));
        painter->setFont(titleFont);
        painter->drawText(badgeRect, Qt::AlignCenter, badge);
        titleRight = badgeRect.left() - kBadgeGap;
    }

    // Title line.
    painter->setFont(titleFont);
    painter->setPen(titleColor);
    const QRect titleRect(textX, titleLine.top(), titleRight - textX, titleLine.height());
    painter->drawText(titleRect, Qt::AlignVCenter | Qt::AlignLeft,
                      fmTitle.elidedText(title, Qt::ElideRight, titleRect.width()));

    // Subtitle line.
    if (!subtitle.isEmpty()) {
        painter->setFont(subFont);
        painter->setPen(subColor);
        const QRect subRect(textX, subLine.top(), r.right() - textX, subLine.height());
        painter->drawText(subRect, Qt::AlignVCenter | Qt::AlignLeft,
                          fmSub.elidedText(subtitle, Qt::ElideRight, subRect.width()));
    }

    painter->restore();
}

QSize AgentCardDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                  const QModelIndex &idx) const
{
    if (!idx.parent().isValid()) {
        return QStyledItemDelegate::sizeHint(opt, idx);
    }
    const QFontMetrics fmTitle(titleFontFor(opt.font, false));
    const QFontMetrics fmSub(subtitleFontFor(opt.font, false));
    const int h = kPadV * 2 + fmTitle.height() + kLineGap + fmSub.height();
    const int w = QStyledItemDelegate::sizeHint(opt, idx).width();
    return {w, h};
}
