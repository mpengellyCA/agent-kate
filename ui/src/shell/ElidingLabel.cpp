// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "shell/ElidingLabel.h"

#include <QPainter>
#include <QPaintEvent>

ElidingLabel::ElidingLabel(QWidget *parent)
    : ElidingLabel(QString(), parent)
{
}

ElidingLabel::ElidingLabel(const QString &text, QWidget *parent)
    : QLabel(parent)
{
    // Ignore the natural text width when distributing space: the label takes
    // whatever it is given and elides to fit, so it never pins a row wider.
    setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Preferred);
    setText(text);
}

void ElidingLabel::setText(const QString &text)
{
    m_full = text;
    setToolTip(text);
    // Keep the base QLabel text in sync for accessibility / QLabel::text(), but
    // our paintEvent does the actual (elided) drawing and our size hints keep
    // the base implementation from demanding the full width.
    QLabel::setText(text);
    update();
}

void ElidingLabel::setElideMode(Qt::TextElideMode mode)
{
    if (m_mode == mode)
        return;
    m_mode = mode;
    update();
}

QSize ElidingLabel::minimumSizeHint() const
{
    // Just enough for an ellipsis — so the label can shrink freely in a row.
    const QFontMetrics fm = fontMetrics();
    const QMargins m = contentsMargins();
    return QSize(fm.horizontalAdvance(QStringLiteral("…")) + m.left() + m.right(),
                 fm.height() + m.top() + m.bottom());
}

QSize ElidingLabel::sizeHint() const
{
    const QFontMetrics fm = fontMetrics();
    const QMargins m = contentsMargins();
    return QSize(fm.horizontalAdvance(m_full) + m.left() + m.right(),
                 fm.height() + m.top() + m.bottom());
}

void ElidingLabel::paintEvent(QPaintEvent *event)
{
    if (m_full.isEmpty()) {
        QLabel::paintEvent(event);
        return;
    }
    QPainter painter(this);
    const QRect r = contentsRect();
    const QString elided = fontMetrics().elidedText(m_full, m_mode, r.width());
    painter.drawText(r, int(alignment()), elided);
}
