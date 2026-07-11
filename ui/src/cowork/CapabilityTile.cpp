// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "CapabilityTile.h"

#include "shell/ElidingLabel.h"

#include <KLocalizedString>

#include <QEvent>
#include <QIcon>
#include <QKeyEvent>
#include <QLabel>
#include <QMouseEvent>
#include <QPainter>
#include <QPaintEvent>
#include <QVBoxLayout>

namespace {
// Warning accent for control-tier tiles: derived from the palette so it
// tracks light/dark schemes rather than a fixed amber.
QColor warnColor(const QPalette &pal)
{
    return QColor::fromHsv(35, 200, pal.color(QPalette::Base).value() > 128 ? 200 : 235);
}
} // namespace

namespace {
constexpr int kTileWidth = 148; // ~140px target incl. margins
constexpr int kIconPx = 28;
} // namespace

CapabilityTile::CapabilityTile(const QString &key, const QString &title,
                               const QString &description, const QString &iconName,
                               bool dangerous, QWidget *parent)
    : QFrame(parent), m_key(key), m_dangerous(dangerous)
{
    setFixedWidth(kTileWidth);
    setFrameShape(QFrame::NoFrame);
    setCursor(Qt::PointingHandCursor);
    setFocusPolicy(Qt::StrongFocus);
    setAttribute(Qt::WA_Hover, true);

    auto *v = new QVBoxLayout(this);
    v->setContentsMargins(10, 10, 10, 10);
    v->setSpacing(4);

    m_icon = new QLabel(this);
    m_icon->setPixmap(QIcon::fromTheme(iconName).pixmap(kIconPx, kIconPx));
    v->addWidget(m_icon, 0, Qt::AlignLeft);

    // The title carries the ⚠ marker for control-tier capabilities so the risk
    // reads even without hovering for the tooltip.
    m_title = new QLabel(dangerous ? i18n("⚠ %1", title) : title, this);
    m_title->setWordWrap(true);
    QFont tf = m_title->font();
    tf.setBold(true);
    m_title->setFont(tf);
    v->addWidget(m_title);

    m_desc = new ElidingLabel(description, this);
    m_desc->setToolTip(description);
    v->addWidget(m_desc);

    v->addStretch(1);

    restyle();
}

void CapabilityTile::setChecked(bool on)
{
    if (m_checked == on) {
        return;
    }
    m_checked = on;
    restyle();
}

void CapabilityTile::mousePressEvent(QMouseEvent *event)
{
    if (event->button() == Qt::LeftButton) {
        m_checked = !m_checked;
        restyle();
        Q_EMIT toggled(m_key, m_checked);
        event->accept();
        return;
    }
    QFrame::mousePressEvent(event);
}

void CapabilityTile::keyPressEvent(QKeyEvent *event)
{
    if (event->key() == Qt::Key_Space || event->key() == Qt::Key_Return
        || event->key() == Qt::Key_Enter) {
        m_checked = !m_checked;
        restyle();
        Q_EMIT toggled(m_key, m_checked);
        event->accept();
        return;
    }
    QFrame::keyPressEvent(event);
}

bool CapabilityTile::event(QEvent *event)
{
    if (event->type() == QEvent::PaletteChange) {
        restyle();
    }
    return QFrame::event(event);
}

// Palette-only styling: the on-state fills with Highlight and switches the text
// to HighlightedText; the off-state is a subtle Base card. Control-tier tiles
// keep a warning-coloured border in both states. The card itself is painted in
// paintEvent(); only the label foregrounds are set here, via the children's
// palettes. A per-widget stylesheet must not be used: setStyleSheet() delivers
// PaletteChange back to this widget, and restyle() runs on PaletteChange —
// infinite recursion (stack-overflow SIGSEGV on startup).
void CapabilityTile::restyle()
{
    const QPalette pal = palette();
    const QColor text = pal.color(QPalette::Text);
    const QColor hiText = pal.color(QPalette::HighlightedText);
    const QColor mid = pal.color(QPalette::Mid);

    const QColor fg = m_checked ? hiText : text;
    QPalette tp = m_title->palette();
    tp.setColor(QPalette::WindowText, fg);
    m_title->setPalette(tp);

    // Description is muted relative to the title; on Highlight we keep the
    // HighlightedText colour (a mid grey would vanish on the accent fill).
    QPalette dp = m_desc->palette();
    dp.setColor(QPalette::WindowText, m_checked ? hiText : mid);
    m_desc->setPalette(dp);

    update();
}

void CapabilityTile::paintEvent(QPaintEvent *event)
{
    Q_UNUSED(event);
    const QPalette pal = palette();
    const QColor base = pal.color(QPalette::Base);
    const QColor hi = pal.color(QPalette::Highlight);
    const QColor mid = pal.color(QPalette::Mid);

    const QColor bg = m_checked ? hi : base;
    QColor border;
    qreal borderW = 1.0;
    if (m_dangerous) {
        border = warnColor(pal);
        borderW = 2.0;
    } else {
        border = m_checked ? hi : mid;
    }

    QPainter p(this);
    p.setRenderHint(QPainter::Antialiasing);
    const qreal inset = borderW / 2.0;
    const QRectF r = QRectF(rect()).adjusted(inset, inset, -inset, -inset);
    p.setPen(QPen(border, borderW));
    p.setBrush(bg);
    p.drawRoundedRect(r, 8, 8);
}
