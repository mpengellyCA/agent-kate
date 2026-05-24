// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#include "RefChipDelegate.h"
#include "LogModel.h"

#include <QApplication>
#include <QColor>
#include <QFontMetrics>
#include <QPainter>
#include <QPalette>
#include <QStringList>
#include <QStyle>

namespace {
enum class RefKind { Branch, Remote, Tag };

struct Chip {
    QString label;
    RefKind kind;
};

Chip classify(const QString &raw)
{
    if (raw.startsWith(QLatin1String("tag:"))) {
        return {raw.mid(4), RefKind::Tag};
    }
    if (raw.contains(QLatin1Char('/'))) {
        return {raw, RefKind::Remote};
    }
    return {raw, RefKind::Branch};
}

// Pull tints from the palette so chips track Breeze light/dark switching, but
// nudge them by kind so the three classes stay visually distinct.
QColor chipBackground(const QPalette &pal, RefKind kind)
{
    QColor base = pal.color(QPalette::Highlight);
    switch (kind) {
    case RefKind::Branch: return base;
    case RefKind::Remote: return base.darker(130);
    case RefKind::Tag:    return QColor(0xc9, 0x8a, 0x1f); // muted gold
    }
    return base;
}

QColor chipForeground(const QPalette &pal, RefKind kind)
{
    Q_UNUSED(kind);
    return pal.color(QPalette::HighlightedText);
}

constexpr int kChipHPad = 6;
constexpr int kChipVPad = 1;
constexpr int kChipGap = 4;
constexpr int kChipRadius = 4;
} // namespace

RefChipDelegate::RefChipDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
}

void RefChipDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                            const QModelIndex &idx) const
{
    QStyleOptionViewItem styled = opt;
    initStyleOption(&styled, idx);
    // We'll paint the text ourselves so we control the X offset after the
    // chips; clear it on the style option so the framework paints background
    // and selection but not the subject text.
    const QString text = styled.text;
    styled.text.clear();
    QStyle *style = styled.widget ? styled.widget->style() : QApplication::style();
    style->drawControl(QStyle::CE_ItemViewItem, &styled, painter, styled.widget);

    const QStringList refs = idx.data(LogModel::RefsRole).toStringList();

    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);

    const QRect r = opt.rect.adjusted(4, 0, -4, 0);
    const QFontMetrics fm(opt.font);
    int x = r.left();
    const int chipHeight = fm.height() + kChipVPad * 2;
    const int yChip = r.top() + (r.height() - chipHeight) / 2;

    for (const QString &raw : refs) {
        const Chip c = classify(raw);
        const int w = fm.horizontalAdvance(c.label) + kChipHPad * 2;
        QRect chipRect(x, yChip, w, chipHeight);
        QColor bg = chipBackground(opt.palette, c.kind);
        QColor fg = chipForeground(opt.palette, c.kind);
        painter->setPen(Qt::NoPen);
        painter->setBrush(bg);
        painter->drawRoundedRect(chipRect, kChipRadius, kChipRadius);
        painter->setPen(fg);
        painter->drawText(chipRect, Qt::AlignCenter, c.label);
        x += w + kChipGap;
        if (x > r.right()) {
            break; // out of room — drop the rest
        }
    }

    // Subject text, elided to whatever space is left.
    if (!text.isEmpty()) {
        QColor textColor = opt.palette.color((opt.state & QStyle::State_Selected)
                                                 ? QPalette::HighlightedText
                                                 : QPalette::Text);
        painter->setPen(textColor);
        const QRect textRect = QRect(x + (refs.isEmpty() ? 0 : kChipGap),
                                     r.top(),
                                     r.right() - x,
                                     r.height());
        const QString elided = fm.elidedText(text, Qt::ElideRight, textRect.width());
        painter->drawText(textRect, Qt::AlignVCenter | Qt::AlignLeft, elided);
    }

    painter->restore();
}

QSize RefChipDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                const QModelIndex &idx) const
{
    QSize base = QStyledItemDelegate::sizeHint(opt, idx);
    const QStringList refs = idx.data(LogModel::RefsRole).toStringList();
    if (refs.isEmpty()) {
        return base;
    }
    const QFontMetrics fm(opt.font);
    int extra = 0;
    for (const QString &raw : refs) {
        const Chip c = classify(raw);
        extra += fm.horizontalAdvance(c.label) + kChipHPad * 2 + kChipGap;
    }
    return {base.width() + extra, base.height()};
}
