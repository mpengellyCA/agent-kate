// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AgentCardDelegate.h"

#include "shell/ChipPainter.h"
#include "theme/ThemeManager.h"

#include <QApplication>
#include <QColor>
#include <QDateTime>
#include <QFont>
#include <QFontMetrics>
#include <QModelIndex>
#include <QPainter>
#include <QPainterPath>
#include <QPalette>
#include <QStringList>
#include <QStyle>
#include <QTextLayout>
#include <QTextLine>
#include <QTextOption>

#include <KFormat>
#include <KLocalizedString>

namespace {
// Card geometry, all kept in font-metric-relative terms at use-time so the row
// survives font scaling / HiDPI. These are the fixed paddings only.
constexpr int kCardGap  = 4;  // inter-card vertical gap (breathing room)
constexpr int kPadH     = 9;  // left/right inset inside the card
constexpr int kPadV     = 7;  // top/bottom inset inside the card
constexpr int kBadgeD   = 11; // status-badge glyph box (square)
constexpr int kBadgeGap = 8;  // gap between badge and title column
constexpr int kLineGap  = 2;  // gap between stacked text lines
constexpr int kNumHPad  = 6;  // horizontal padding inside the "#N" badge
constexpr int kNumGap   = 6;  // gap between title and the "#N" badge
constexpr int kChipRowGap = 3; // gap above the chip row
constexpr int kCardRadius = 7; // card corner radius
constexpr int kPreviewLines = 2; // preview is elided across two lines

QFont chipFontFor(const QFont &base)
{
    QFont f = base;
    if (f.pointSizeF() > 0) {
        f.setPointSizeF(f.pointSizeF() * 0.85);
    }
    return f;
}

QFont titleFontFor(const QFont &base, bool dormant)
{
    QFont f = base;
    f.setBold(true);
    f.setItalic(dormant); // dormant agents read as resumable history
    return f;
}

QFont previewFontFor(const QFont &base, bool dormant)
{
    QFont f = base;
    if (f.pointSizeF() > 0) {
        f.setPointSizeF(f.pointSizeF() * 0.9);
    }
    f.setItalic(dormant);
    return f;
}

QFont timeFontFor(const QFont &base)
{
    QFont f = base;
    if (f.pointSizeF() > 0) {
        f.setPointSizeF(f.pointSizeF() * 0.82);
    }
    return f;
}

// The status badge for a state: the glyph to paint and its semantic colour.
// Colours come from ThemeManager (agentRunning / neutral / negative) and the
// live palette (idle/dormant muted), never hardcoded per light/dark.
struct Badge {
    QString glyph;
    QColor color;
    bool animated = false;
};

Badge badgeFor(AgentRoles::AgentStatus s, const QPalette &pal)
{
    const AkColors &ak = ThemeManager::palette();
    switch (s) {
    case AgentRoles::AgentStatus::Working:
        return {QStringLiteral("●"), ak.agentRunning, true}; // ● animated
    case AgentRoles::AgentStatus::NeedsInput:
        return {QStringLiteral("⚠"), ak.neutral, false};     // ⚠ amber
    case AgentRoles::AgentStatus::Error:
        return {QStringLiteral("✖"), ak.negative, false};    // ✖ negative
    case AgentRoles::AgentStatus::Dormant:
        return {QStringLiteral("⏸"), pal.color(QPalette::PlaceholderText),
                false}; // ⏸ muted
    case AgentRoles::AgentStatus::Idle:
    default:
        return {QStringLiteral("○"), pal.color(QPalette::PlaceholderText),
                false}; // ○ muted
    }
}

// Relative "Xm ago" string in plain language, via KFormat so it localises and
// keeps its phrasing consistent with the rest of the app.
QString relativeTime(qint64 epochSecs)
{
    if (epochSecs <= 0) {
        return QString();
    }
    const QDateTime when = QDateTime::fromSecsSinceEpoch(epochSecs);
    if (!when.isValid()) {
        return QString();
    }
    const qint64 delta = when.secsTo(QDateTime::currentDateTime());
    if (delta < 45) {
        return i18nc("@item roster card, very recent activity", "just now");
    }
    return KFormat().formatRelativeDateTime(when, QLocale::NarrowFormat);
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

    const bool selected = opt.state & QStyle::State_Selected;
    const bool current  = opt.state & QStyle::State_HasFocus;
    const bool hover    = opt.state & QStyle::State_MouseOver;
    const bool dormant  = idx.data(AgentRoles::Dormant).toBool();
    const QString title    = idx.data(AgentRoles::Title).toString();
    const QString subtitle = idx.data(AgentRoles::Subtitle).toString();
    const QString preview  = idx.data(AgentRoles::Preview).toString();
    const int number       = idx.data(AgentRoles::Number).toInt();
    const auto status      = AgentRoles::AgentStatus(idx.data(AgentRoles::StatusRole).toInt());
    const qint64 activity  = idx.data(AgentRoles::LastActivity).toLongLong();
    const QStringList tags = idx.data(AgentRoles::Tags).toStringList();
    // A background agent that needs the user's input gets a palette-driven
    // marker — but only while it isn't the row the user is already looking at.
    const bool attention = idx.data(AgentRoles::Attention).toBool()
                           && !selected && !current;

    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);

    // The card body: inset from the row rect so cards sit apart with a gap.
    const QRect card = opt.rect.adjusted(kPadH / 2, kCardGap / 2, -kPadH / 2,
                                         -kCardGap / 2);

    // Rounded card background: AlternateBase fill, 1px border. Selection rides
    // Highlight; hover lifts a touch toward the base tone.
    QColor fill = opt.palette.color(QPalette::AlternateBase);
    QColor border = opt.palette.color(QPalette::Mid);
    if (selected) {
        fill = opt.palette.color(QPalette::Highlight);
        border = fill;
    } else if (hover) {
        fill = opt.palette.color(QPalette::Base);
    }
    QPainterPath cardPath;
    cardPath.addRoundedRect(QRectF(card), kCardRadius, kCardRadius);
    painter->fillPath(cardPath, fill);
    painter->setPen(QPen(border, 1));
    painter->setBrush(Qt::NoBrush);
    painter->drawPath(cardPath);

    const QRect r = card.adjusted(kPadH, kPadV, -kPadH, -kPadV);

    const QFont titleFont = titleFontFor(opt.font, dormant);
    const QFont previewFont = previewFontFor(opt.font, dormant);
    const QFont timeFont = timeFontFor(opt.font);
    const QFontMetrics fmTitle(titleFont);
    const QFontMetrics fmPreview(previewFont);
    const QFontMetrics fmTime(timeFont);

    // Text colours from the palette so we follow Breeze. On selection everything
    // rides the highlight foreground; otherwise dormant rows read a touch fainter
    // and the preview is always the muted (placeholder) tone.
    QColor titleColor = opt.palette.color(selected ? QPalette::HighlightedText
                                                   : QPalette::Text);
    QColor previewColor = selected ? opt.palette.color(QPalette::HighlightedText)
                                   : opt.palette.color(QPalette::PlaceholderText);
    QColor timeColor = previewColor;
    if (dormant && !selected) {
        titleColor = opt.palette.color(QPalette::PlaceholderText);
    }

    // --- line 1: status badge + title + relative time -----------------------
    const QRect titleLine(r.left(), r.top(), r.width(), fmTitle.height());

    // Status badge, vertically centred on the title line.
    const Badge badge = badgeFor(status, opt.palette);
    const QRect badgeRect(r.left(), titleLine.center().y() - kBadgeD / 2, kBadgeD,
                          kBadgeD);
    if (badge.animated && !selected) {
        // Working: a sweeping arc in the agentRunning colour. The phase is
        // derived from wall-clock time so the roster's ~10fps timer only needs
        // to trigger repaints (the delegate stays stateless).
        const qint64 ms = QDateTime::currentMSecsSinceEpoch();
        const int start = int((ms / 3) % 360); // ~ one revolution / 1.1s
        painter->save();
        painter->setBrush(Qt::NoBrush);
        QPen arcPen(badge.color, 2);
        arcPen.setCapStyle(Qt::RoundCap);
        painter->setPen(arcPen);
        const QRectF arc = QRectF(badgeRect).adjusted(1.5, 1.5, -1.5, -1.5);
        painter->drawArc(arc, -start * 16, 270 * 16);
        painter->restore();
    } else {
        painter->save();
        painter->setPen(selected ? opt.palette.color(QPalette::HighlightedText)
                                 : badge.color);
        painter->setFont(opt.font);
        painter->drawText(badgeRect, Qt::AlignCenter, badge.glyph);
        painter->restore();
    }

    const int textX = r.left() + kBadgeD + kBadgeGap;
    int titleRight = r.right();

    // Relative time, right-aligned on the title line.
    const QString when = relativeTime(activity);
    if (!when.isEmpty()) {
        const int tw = fmTime.horizontalAdvance(when);
        const QRect timeRect(r.right() - tw, titleLine.top(), tw,
                             titleLine.height());
        painter->setFont(timeFont);
        painter->setPen(timeColor);
        painter->drawText(timeRect, Qt::AlignVCenter | Qt::AlignRight, when);
        titleRight = timeRect.left() - kNumGap;
    }

    // "Needs your input" marker, painted just left of the time in the palette
    // Highlight colour so it tracks Breeze and reads as actionable.
    if (attention) {
        const int markD = kBadgeD;
        const int markX = titleRight - markD;
        const int markY = titleLine.center().y() - markD / 2;
        painter->setPen(Qt::NoPen);
        painter->setBrush(opt.palette.color(QPalette::Highlight));
        painter->drawEllipse(markX, markY, markD, markD);
        titleRight = markX - kNumGap;
    }

    // Title line.
    painter->setFont(titleFont);
    painter->setPen(titleColor);
    const QRect titleRect(textX, titleLine.top(), qMax(0, titleRight - textX),
                          titleLine.height());
    painter->drawText(titleRect, Qt::AlignVCenter | Qt::AlignLeft,
                      fmTitle.elidedText(title, Qt::ElideRight, titleRect.width()));

    // --- lines 2-3: two-line elided chat preview via QTextLayout ------------
    int y = titleLine.bottom() + kLineGap;
    if (!preview.isEmpty()) {
        painter->setFont(previewFont);
        painter->setPen(previewColor);
        QTextOption topt;
        topt.setWrapMode(QTextOption::WrapAtWordBoundaryOrAnywhere);
        // Collapse newlines so a multi-line message doesn't blow the layout.
        QString flat = preview;
        flat.replace(QLatin1Char('\n'), QLatin1Char(' '));
        QTextLayout layout(flat, previewFont);
        layout.setTextOption(topt);
        const int lineWidth = qMax(0, r.width());
        layout.beginLayout();
        int line = 0;
        while (line < kPreviewLines) {
            QTextLine tl = layout.createLine();
            if (!tl.isValid()) {
                break;
            }
            tl.setLineWidth(lineWidth);
            const bool lastAllowed = (line == kPreviewLines - 1);
            // On the last allowed line, if more text remains, elide it.
            if (lastAllowed) {
                const int start = tl.textStart();
                QString rest = flat.mid(start);
                // Does the remainder overflow one line? If so, elide the whole
                // remainder onto this line and stop.
                if (fmPreview.horizontalAdvance(rest) > lineWidth) {
                    painter->drawText(
                        QRect(r.left(), y, lineWidth, fmPreview.height()),
                        Qt::AlignLeft | Qt::AlignVCenter,
                        fmPreview.elidedText(rest, Qt::ElideRight, lineWidth));
                    y += fmPreview.height();
                    line = kPreviewLines; // consumed the last line
                    break;
                }
            }
            tl.draw(painter, QPointF(r.left(), y));
            y += qRound(tl.height());
            ++line;
        }
        layout.endLayout();
    }

    // --- line 4: "#N" badge + tag chips -------------------------------------
    // The card body is AlternateBase, so chips fill from Base (a step away) to
    // read against it; on a selected row they outline (a filled Highlight chip
    // on the Highlight card would vanish). Text stays the muted placeholder tone
    // except on selection where it rides the highlight foreground.
    const bool chipOutline = selected;
    const QColor chipFill = opt.palette.color(QPalette::Base);
    const QColor chipText = selected ? opt.palette.color(QPalette::HighlightedText)
                                     : opt.palette.color(QPalette::PlaceholderText);

    const QFont chipFont = chipFontFor(opt.font);
    if (number > 0 || !tags.isEmpty()) {
        const int chipH = ChipPainter::chipHeight(chipFont);
        int x = r.left();
        if (number > 0) {
            const QString num = QStringLiteral("#%1").arg(number);
            const int nw = QFontMetrics(chipFont).horizontalAdvance(num)
                           + kNumHPad * 2;
            ChipPainter::drawChip(painter, QRect(x, y, nw, chipH), num, chipFont,
                                  chipFill, chipText, chipOutline);
            x += nw + ChipPainter::kChipGap;
        }
        if (!tags.isEmpty()) {
            ChipPainter::drawChipRow(painter, x, y, r.right(), tags, chipFont,
                                     chipFill, chipText, chipOutline);
        }
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
    const QFontMetrics fmPreview(previewFontFor(opt.font, false));
    // Card content: title line + two preview lines. The card border/fill adds
    // the top+bottom inset; the inter-card gap is added on top so cards separate.
    int h = kCardGap + kPadV * 2 + fmTitle.height()
            + kLineGap + kPreviewLines * fmPreview.height();
    // A "#N" badge or tags add the chip row.
    const bool hasChips = idx.data(AgentRoles::Number).toInt() > 0
                          || !idx.data(AgentRoles::Tags).toStringList().isEmpty();
    if (hasChips) {
        h += kChipRowGap + ChipPainter::chipHeight(chipFontFor(opt.font));
    }
    const int w = QStyledItemDelegate::sizeHint(opt, idx).width();
    return {w, h};
}
