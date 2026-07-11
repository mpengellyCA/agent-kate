// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AuthorChipDelegate.h"
#include "shell/ChipPainter.h"
#include "theme/ThemeManager.h"

#include <QApplication>
#include <QFontMetrics>
#include <QPainter>
#include <QPalette>
#include <QStringList>
#include <QStyle>

namespace {
// Up to two uppercase initials from an author name.
QString initials(const QString &name)
{
    const QStringList parts = name.split(QLatin1Char(' '), Qt::SkipEmptyParts);
    if (parts.isEmpty()) {
        return QStringLiteral("?");
    }
    QString out = parts.first().left(1).toUpper();
    if (parts.size() > 1) {
        out += parts.last().left(1).toUpper();
    }
    return out;
}
} // namespace

AuthorChipDelegate::AuthorChipDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
}

void AuthorChipDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                               const QModelIndex &idx) const
{
    // Standard row background (selection / alternation / hover) first.
    QStyleOptionViewItem styled = opt;
    initStyleOption(&styled, idx);
    const QString name = styled.text;
    styled.text.clear();
    QStyle *style = styled.widget ? styled.widget->style() : QApplication::style();
    style->drawControl(QStyle::CE_ItemViewItem, &styled, painter, styled.widget);

    if (name.isEmpty()) {
        return;
    }

    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);

    const QRect r = opt.rect.adjusted(4, 0, -4, 0);
    const AkColors &ak = ThemeManager::palette();
    const int chipH = ChipPainter::chipHeight(opt.font);
    const int chipW = ChipPainter::chipWidth(opt.font, initials(name));
    const int yChip = r.top() + (r.height() - chipH) / 2;
    ChipPainter::drawChip(painter, QRect(r.left(), yChip, chipW, chipH),
                          initials(name), opt.font, ak.accent, ak.accentText);

    // Author name after the chip, elided to the remaining width.
    const int textX = r.left() + chipW + ChipPainter::kChipGap;
    const QColor textColor = opt.palette.color(
        (opt.state & QStyle::State_Selected) ? QPalette::HighlightedText
                                             : QPalette::Text);
    painter->setPen(textColor);
    const QFontMetrics fm(opt.font);
    const QRect textRect(textX, r.top(), r.right() - textX, r.height());
    painter->drawText(textRect, Qt::AlignVCenter | Qt::AlignLeft,
                      fm.elidedText(name, Qt::ElideRight, textRect.width()));

    painter->restore();
}

QSize AuthorChipDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                   const QModelIndex &idx) const
{
    QSize base = QStyledItemDelegate::sizeHint(opt, idx);
    const QString name = idx.data(Qt::DisplayRole).toString();
    if (name.isEmpty()) {
        return base;
    }
    const int extra =
        ChipPainter::chipWidth(opt.font, initials(name)) + ChipPainter::kChipGap;
    return {base.width() + extra, base.height()};
}
