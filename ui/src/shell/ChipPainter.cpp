// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "ChipPainter.h"

#include <QFontMetrics>
#include <QPainter>
#include <QStringList>

namespace ChipPainter {

int chipHeight(const QFont &font)
{
    return QFontMetrics(font).height() + kChipVPad * 2;
}

int chipWidth(const QFont &font, const QString &text)
{
    return QFontMetrics(font).horizontalAdvance(text) + kChipHPad * 2;
}

void drawChip(QPainter *painter, const QRect &rect, const QString &text,
              const QFont &font, const QColor &fill, const QColor &textColor,
              bool outline)
{
    painter->save();
    painter->setFont(font);
    if (outline) {
        painter->setBrush(Qt::NoBrush);
        painter->setPen(textColor);
    } else {
        painter->setPen(Qt::NoPen);
        painter->setBrush(fill);
    }
    painter->drawRoundedRect(rect, kChipRadius, kChipRadius);
    painter->setPen(textColor);
    painter->drawText(rect, Qt::AlignCenter, text);
    painter->restore();
}

int drawChipRow(QPainter *painter, int x, int top, int rightEdge,
                const QStringList &chips, const QFont &font, const QColor &fill,
                const QColor &textColor, bool outline)
{
    const QFontMetrics fm(font);
    const int h = chipHeight(font);
    for (int i = 0; i < chips.size(); ++i) {
        const QString chip = chips.at(i);
        const int w = chipWidth(font, chip);
        const bool fits = x + w <= rightEdge;
        // If this chip won't fit and there is more than one left, collapse the
        // remainder into a "+N" overflow chip and stop.
        if (!fits && i > 0) {
            const int remaining = chips.size() - i;
            const QString more = QStringLiteral("+%1").arg(remaining);
            const int moreW = chipWidth(font, more);
            int mx = x;
            if (mx + moreW > rightEdge) {
                mx = rightEdge - moreW; // pull it back so it stays visible
            }
            drawChip(painter, QRect(mx, top, moreW, h), more, font, fill,
                     textColor, outline);
            return mx + moreW;
        }
        drawChip(painter, QRect(x, top, w, h), chip, font, fill, textColor,
                 outline);
        x += w + kChipGap;
    }
    return x;
}

} // namespace ChipPainter
