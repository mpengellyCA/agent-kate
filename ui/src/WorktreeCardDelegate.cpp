// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorktreeCardDelegate.h"

#include "WorktreeDashboard.h"
#include "shell/ChipPainter.h"
#include "theme/ThemeManager.h"

#include <QDateTime>
#include <QFont>
#include <QFontMetrics>
#include <QModelIndex>
#include <QPainter>
#include <QPainterPath>
#include <QPalette>

#include <KFormat>
#include <KLocalizedString>

namespace {
// Card geometry, kept font-metric-relative at use-time so the card survives
// font scaling / HiDPI. These are the fixed paddings only.
constexpr int kCardGap    = 4; // inter-card vertical gap
constexpr int kPadH       = 9; // left/right inset inside the card
constexpr int kPadV       = 7; // top/bottom inset inside the card
constexpr int kLineGap    = 3; // gap between the two text lines
constexpr int kPillGap    = 4; // gap before the pill run
constexpr int kCardRadius = 7; // card corner radius

QFont titleFontFor(const QFont &base)
{
    QFont f = base;
    f.setBold(true);
    return f;
}

QFont subFontFor(const QFont &base)
{
    QFont f = base;
    if (f.pointSizeF() > 0) {
        f.setPointSizeF(f.pointSizeF() * 0.9);
    }
    return f;
}

QFont pillFontFor(const QFont &base)
{
    QFont f = base;
    if (f.pointSizeF() > 0) {
        f.setPointSizeF(f.pointSizeF() * 0.85);
    }
    return f;
}

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
        return i18nc("@item worktree card, just refreshed", "updated just now");
    }
    return i18nc("@item worktree card, e.g. 'updated 3 minutes ago'",
                 "updated %1",
                 KFormat().formatRelativeDateTime(when, QLocale::NarrowFormat));
}

// A single status pill: its text and its two colours (fill + text). Built
// left-to-right in the order they should read; the caller paints them.
struct Pill {
    QString text;
    QColor fill;
    QColor textColor;
};

// Readable text colour for a saturated pill fill: black on a light fill, white
// on a dark one, chosen by perceived luminance so amber and red both read.
QColor readableOn(const QColor &fill)
{
    const double lum = 0.299 * fill.redF() + 0.587 * fill.greenF()
                       + 0.114 * fill.blueF();
    return lum > 0.55 ? QColor(Qt::black) : QColor(Qt::white);
}

// Assemble the status pills for a row, in reading order (ahead/behind first,
// remote, dirty, conflicts). Colours come from ThemeManager semantic colours.
QList<Pill> pillsFor(const WorktreeRow &r, const QPalette &pal, bool selected)
{
    const AkColors &ak = ThemeManager::palette();
    QList<Pill> pills;

    // Neutral (informational) pill background/text — a step off the card base so
    // it reads. On selection everything rides the highlight foreground.
    const QColor neutralFill = selected ? pal.color(QPalette::HighlightedText)
                                        : pal.color(QPalette::Base);
    const QColor neutralText = selected ? pal.color(QPalette::Highlight)
                                        : pal.color(QPalette::PlaceholderText);

    // "not isolated" first, so it reads furthest from the counts: this row is
    // the user's own checkout, not a private copy (audit F50). It is a fact
    // about the row rather than an alarm, so it takes the informational hue —
    // but it must be legible, because it is the property that decides what
    // "Discard changes…" destroys (audit F29).
    if (!r.isolated) {
        const QColor info = selected ? pal.color(QPalette::HighlightedText) : ak.info;
        const QColor infoText = selected ? ak.info : readableOn(info);
        pills << Pill{WorktreeCopy::notIsolatedPill(), info, infoText};
    }
    // Ahead / behind vs the fork base — only when non-zero, so a clean, fully
    // merged worktree carries no clutter.
    if (r.ahead > 0 || r.behindBase > 0) {
        pills << Pill{QStringLiteral("↑%1 ↓%2").arg(r.ahead).arg(r.behindBase),
                      neutralFill, neutralText};
    }
    // Remote (origin) tracking, only when the branch has an upstream.
    if (r.hasUpstream && (r.remoteAhead > 0 || r.remoteBehind > 0)) {
        pills << Pill{
            i18nc("worktree pill: ahead/behind the origin remote", "origin ↑%1 ↓%2")
                .arg(r.remoteAhead)
                .arg(r.remoteBehind),
            neutralFill, neutralText};
    }
    // Dirty count in amber (neutral/caution semantic colour).
    if (r.dirty > 0) {
        const QColor amber = selected ? pal.color(QPalette::HighlightedText) : ak.neutral;
        const QColor amberText = selected ? ak.neutral : readableOn(amber);
        pills << Pill{QStringLiteral("✎ %1").arg(r.dirty), amber, amberText};
    }
    // Conflicts in negative red — the loudest signal, so it reads last (nearest
    // the title).
    if (r.conflicts) {
        const QColor red = selected ? pal.color(QPalette::HighlightedText) : ak.negative;
        const QColor redText = selected ? ak.negative : readableOn(red);
        pills << Pill{i18nc("worktree pill: has merge conflicts", "⚠ conflicts"),
                      red, redText};
    }
    return pills;
}
} // namespace

WorktreeCardDelegate::WorktreeCardDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
}

void WorktreeCardDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                                 const QModelIndex &idx) const
{
    const auto *rp = static_cast<const WorktreeRow *>(
        idx.data(WorktreeRoles::Row).value<void *>());
    if (!rp) {
        QStyledItemDelegate::paint(painter, opt, idx);
        return;
    }
    const WorktreeRow &r = *rp;

    const bool selected = opt.state & QStyle::State_Selected;
    const bool hover = opt.state & QStyle::State_MouseOver;
    const AkColors &ak = ThemeManager::palette();

    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);

    // The card body: inset from the row rect so cards sit apart with a gap.
    const QRect card =
        opt.rect.adjusted(kPadH / 2, kCardGap / 2, -kPadH / 2, -kCardGap / 2);

    // Fill rides the palette; hover lifts a touch, selection uses Highlight.
    QColor fill = opt.palette.color(QPalette::AlternateBase);
    if (selected) {
        fill = opt.palette.color(QPalette::Highlight);
    } else if (hover) {
        fill = opt.palette.color(QPalette::Base);
    }
    // Border tint by state: conflicts negative, dirty amber, clean neutral.
    // On selection the border matches the fill so the card reads as one block.
    QColor border = opt.palette.color(QPalette::Mid);
    int borderWidth = 1;
    if (selected) {
        border = fill;
    } else if (r.conflicts) {
        border = ak.negative;
        borderWidth = 2;
    } else if (r.dirty > 0) {
        border = ak.neutral;
        borderWidth = 2;
    }

    QPainterPath cardPath;
    cardPath.addRoundedRect(QRectF(card), kCardRadius, kCardRadius);
    painter->fillPath(cardPath, fill);
    painter->setPen(QPen(border, borderWidth));
    painter->setBrush(Qt::NoBrush);
    painter->drawPath(cardPath);

    const QRect content = card.adjusted(kPadH, kPadV, -kPadH, -kPadV);

    const QFont titleFont = titleFontFor(opt.font);
    const QFont subFont = subFontFor(opt.font);
    const QFont pillFont = pillFontFor(opt.font);
    const QFontMetrics fmTitle(titleFont);
    const QFontMetrics fmSub(subFont);

    const QColor titleColor = opt.palette.color(
        selected ? QPalette::HighlightedText : QPalette::Text);
    const QColor mutedColor = selected ? opt.palette.color(QPalette::HighlightedText)
                                       : opt.palette.color(QPalette::PlaceholderText);

    // --- line 1: "#N branch" + agent title + status pills -------------------
    const int lineH = qMax(fmTitle.height(), ChipPainter::chipHeight(pillFont));
    const QRect line1(content.left(), content.top(), content.width(), lineH);

    // Pills first (right-aligned): measure the run, reserve space, then paint
    // from the right edge inward.
    const QList<Pill> pills = pillsFor(r, opt.palette, selected);
    int pillsLeft = line1.right();
    if (!pills.isEmpty()) {
        int totalW = 0;
        for (const Pill &p : pills) {
            totalW += ChipPainter::chipWidth(pillFont, p.text) + ChipPainter::kChipGap;
        }
        totalW -= ChipPainter::kChipGap; // no trailing gap
        const int pillH = ChipPainter::chipHeight(pillFont);
        const int pillY = line1.center().y() - pillH / 2;
        int x = line1.right() - totalW;
        pillsLeft = x - ChipPainter::kChipGap;
        for (const Pill &p : pills) {
            const int w = ChipPainter::chipWidth(pillFont, p.text);
            ChipPainter::drawChip(painter, QRect(x, pillY, w, pillH), p.text, pillFont,
                                  p.fill, p.textColor, /*outline*/ selected);
            x += w + ChipPainter::kChipGap;
        }
    }

    // Title: "#N branch" bold, then the agent title muted, elided to fit the
    // space the pills left.
    const QString branchLabel =
        r.branch.isEmpty() ? i18nc("git branch state", "(detached)") : r.branch;
    const QString head = r.number > 0
                             ? QStringLiteral("#%1 %2").arg(r.number).arg(branchLabel)
                             : branchLabel;
    const int titleAvail = qMax(0, (pillsLeft - kPillGap) - line1.left());
    painter->setFont(titleFont);
    painter->setPen(titleColor);
    const int headW = qMin(fmTitle.horizontalAdvance(head), titleAvail);
    const QRect headRect(line1.left(), line1.top(), headW, line1.height());
    painter->drawText(headRect, Qt::AlignVCenter | Qt::AlignLeft,
                      fmTitle.elidedText(head, Qt::ElideRight, titleAvail));

    int afterHead = headRect.right();
    if (!r.title.isEmpty() && afterHead + fmSub.averageCharWidth() * 3 < pillsLeft) {
        const int titleX = afterHead + fmTitle.averageCharWidth();
        const int avail = qMax(0, (pillsLeft - kPillGap) - titleX);
        painter->setFont(subFont);
        painter->setPen(mutedColor);
        painter->drawText(QRect(titleX, line1.top(), avail, line1.height()),
                          Qt::AlignVCenter | Qt::AlignLeft,
                          fmSub.elidedText(r.title, Qt::ElideRight, avail));
    }

    // --- line 2: elided path + "updated Xs ago" -----------------------------
    const int y2 = line1.bottom() + kLineGap;
    const QRect line2(content.left(), y2, content.width(), fmSub.height());
    painter->setFont(subFont);
    painter->setPen(mutedColor);

    const QString when = relativeTime(r.updatedAt);
    int pathRight = line2.right();
    if (!when.isEmpty()) {
        const int ww = fmSub.horizontalAdvance(when);
        painter->drawText(QRect(line2.right() - ww, line2.top(), ww, line2.height()),
                          Qt::AlignVCenter | Qt::AlignRight, when);
        pathRight = line2.right() - ww - fmSub.averageCharWidth() * 2;
    }
    const int pathW = qMax(0, pathRight - line2.left());
    painter->drawText(QRect(line2.left(), line2.top(), pathW, line2.height()),
                      Qt::AlignVCenter | Qt::AlignLeft,
                      fmSub.elidedText(r.path, Qt::ElideMiddle, pathW));

    painter->restore();
}

QSize WorktreeCardDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                     const QModelIndex &idx) const
{
    const QFontMetrics fmTitle(titleFontFor(opt.font));
    const QFontMetrics fmSub(subFontFor(opt.font));
    const int line1 =
        qMax(fmTitle.height(), ChipPainter::chipHeight(pillFontFor(opt.font)));
    const int h = kCardGap + kPadV * 2 + line1 + kLineGap + fmSub.height();
    const int w = QStyledItemDelegate::sizeHint(opt, idx).width();
    return {w, h};
}
