// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "theme/ThemeManager.h"

#include <QAbstractItemView>
#include <QAbstractTextDocumentLayout>
#include <QApplication>
#include <QDesktopServices>
#include <QGuiApplication>
#include <QClipboard>
#include <QFontMetrics>
#include <QHelpEvent>
#include <QToolTip>
#include <KLocalizedString>
#include <QJsonArray>
#include <QJsonObject>
#include <QMouseEvent>
#include <QPixmap>
#include <QPainter>
#include <QTextBrowser>
#include <QTextDocument>
#include <QUrl>

namespace {
// Geometry constants for a transcript row. All paint/measure/hit-test code goes
// through these so the height cache, the painter and editorEvent stay in sync.
constexpr int kOuterMarginX = 4;  // matches the old feed layout contentsMargins
constexpr int kRowSpacing = 8;    // vertical gap between rows (old layout spacing)
constexpr int kCardPadX = 12;     // message card horizontal padding
constexpr int kCardPadTop = 9;
constexpr int kCardPadBottom = 11;
constexpr int kRoleRowGap = 3;    // gap under the role row
constexpr int kNotePadX = 8;
constexpr int kNotePadY = 1;
// Attachment chip row under a You message body (plan 13 phase 4).
constexpr int kChipGapTop = 8;  // gap between the body and the chip row
constexpr int kChipH = 24;      // chip height
constexpr int kChipVGap = 4;    // vertical gap between wrapped chip rows
constexpr int kChipHGap = 6;    // horizontal gap between chips
constexpr int kChipPadX = 9;    // text padding inside a chip
constexpr int kChipIcon = 16;   // image-preview icon edge inside a chip
constexpr int kChipMaxW = 220;  // max chip width before the name elides
constexpr int kToolPad = 6;       // tool card inner padding
constexpr int kToolHeaderH = 28;  // clickable header height
constexpr int kToolCopyW = 26;    // copy hit zone on the right of the header
constexpr int kToolInspectW = 26; // "open in inspector" glyph, left of the copy glyph
constexpr int kDetailPadX = 10;

// The height cache is keyed by the model's per-row stableId, which the model
// bumps on every mutation (so the old entry is never looked up again) — left
// unchecked it grows for the life of the panel. Cap it; when full we drop the
// whole cache and the currently-visible rows re-measure lazily (cheap).
constexpr int kHeightCacheCap = 16384;

// noteColor maps a note's kind to the active theme's semantic colours.
QColor noteColor(const QString &kind, bool dark)
{
    Q_UNUSED(dark);
    const AkColors &c = ThemeManager::palette();
    if (kind == QLatin1String("ok")) {
        return c.positive;
    }
    if (kind == QLatin1String("err")) {
        return c.negative;
    }
    return c.agentIdle;
}

// Wrap matches of `needle` in `plain` with a highlight span, escaping first.
// Used by the find overlay so the highlighted body matches the QLabel days. When
// `current` is set this is the row holding the active match, so its hits use the
// strong accent highlight; other matching rows use the muted `mid` role so the
// current match stands out as the focus ring as the user cycles through hits.
QString highlightedHtml(const QString &plain, const QString &needle, bool current)
{
    const QString span = current
        ? QStringLiteral("<span style='background:palette(highlight); "
                         "color:palette(highlighted-text)'>")
        : QStringLiteral("<span style='background:palette(mid); color:palette(text)'>");
    QString escaped = plain.toHtmlEscaped();
    const QString escNeedle = needle.toHtmlEscaped();
    QString out;
    int from = 0;
    for (;;) {
        const int at = escaped.indexOf(escNeedle, from, Qt::CaseInsensitive);
        if (at < 0) {
            out += escaped.mid(from);
            break;
        }
        out += escaped.mid(from, at - from);
        out += span + escaped.mid(at, escNeedle.length()) + QStringLiteral("</span>");
        from = at + escNeedle.length();
    }
    return out.replace(QLatin1Char('\n'), QStringLiteral("<br>"));
}

// Resolve the body HTML for a Message/Note row: the pre-rendered HtmlRole, with
// the in-conversation find highlight substituted in for a matching Message. Kept
// as one routine so the painted row and the selection overlay show identical
// content (same substring highlighting, same glyph flow).
QString resolveBodyHtml(const QModelIndex &idx)
{
    const auto kind = TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());
    QString html = idx.data(TranscriptModel::HtmlRole).toString();
    if (kind == TranscriptModel::Message) {
        const auto *m = qobject_cast<const TranscriptModel *>(idx.model());
        if (m && !m->findNeedle().isEmpty()) {
            const QString plain = idx.data(TranscriptModel::PlainRole).toString();
            if (plain.contains(m->findNeedle(), Qt::CaseInsensitive)) {
                html = highlightedHtml(plain, m->findNeedle(),
                                       idx.row() == m->findCurrentRow());
            }
        }
    }
    return html;
}

// The single source of a body document's metrics — font, zero margin, HTML and
// wrap width. paint()/measure use it through buildBodyDoc(); the selection
// overlay (a QTextBrowser) applies the same setup to its own document so the
// overlay's glyph positions line up with the painted row exactly.
void configureBodyDoc(QTextDocument *doc, const QFont &font, int contentWidth,
                      const QString &html)
{
    doc->setDefaultFont(font);
    doc->setDocumentMargin(0);
    doc->setHtml(html);
    doc->setTextWidth(contentWidth);
}

// One laid-out attachment chip: its rect (in the coordinate space of the origin
// passed to layoutAttachmentChips) plus the elided label and whether it carries
// an image thumbnail.
struct ChipLayout {
    QRect rect;
    QString label;   // already elided to fit
    bool image = false;
};

// Flow-lay the attachment chips of a You message across `availW`, wrapping onto
// further rows. `origin` is the top-left of the chip area. Returns the chips and,
// via `outHeight`, the total height the chip block occupies (0 for no chips).
// Shared by measure, paint and hit-test so all three agree exactly.
QList<ChipLayout> layoutAttachmentChips(const QJsonArray &atts, const QFont &font,
                                        const QPoint &origin, int availW, int &outHeight)
{
    QList<ChipLayout> chips;
    outHeight = 0;
    if (atts.isEmpty() || availW <= 0) {
        return chips;
    }
    const QFontMetrics fm(font);
    int x = origin.x();
    int y = origin.y();
    int rowH = kChipH;
    for (const QJsonValue &av : atts) {
        const QJsonObject att = av.toObject();
        const bool image =
            att.value(QStringLiteral("kind")).toString() == QLatin1String("image");
        const QString name = att.value(QStringLiteral("name")).toString();
        const int iconW = image ? kChipIcon + kChipHGap : 0;
        const int textAvail = qMax(1, kChipMaxW - 2 * kChipPadX - iconW);
        const QString label = fm.elidedText(name, Qt::ElideMiddle, textAvail);
        const int chipW = qMin(kChipMaxW,
                               2 * kChipPadX + iconW + fm.horizontalAdvance(label));
        // Wrap to the next row when this chip would overflow the available width
        // (but always place at least one chip per row).
        if (x > origin.x() && x + chipW > origin.x() + availW) {
            x = origin.x();
            y += rowH + kChipVGap;
        }
        chips.append(ChipLayout{QRect(x, y, chipW, kChipH), label, image});
        x += chipW + kChipHGap;
    }
    outHeight = (y - origin.y()) + rowH;
    return chips;
}
} // namespace

TranscriptDelegate::TranscriptDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
}

TranscriptDelegate::~TranscriptDelegate() = default;

QTextDocument *TranscriptDelegate::buildBodyDoc(const QModelIndex &idx,
                                                int contentWidth,
                                                const QStyleOptionViewItem &opt) const
{
    auto *doc = new QTextDocument;
    configureBodyDoc(doc, opt.font, contentWidth, resolveBodyHtml(idx));
    return doc;
}

// Measure / paint share this: returns the total row height for `width` and, when
// `painter` is non-null, draws the row into `rowRect`. Keeping one routine for
// both guarantees the height cache and the painting never disagree.
namespace {
int layoutRow(const QModelIndex &idx, int width, const QStyleOptionViewItem &opt,
              QPainter *painter, const QRect &rowRect, const TranscriptDelegate *self,
              QTextDocument *(TranscriptDelegate::*buildDoc)(const QModelIndex &, int,
                                                             const QStyleOptionViewItem &) const)
{
    const auto kind = TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());
    const bool dark = opt.palette.color(QPalette::Base).lightness() < 128;
    const QFontMetrics fm(opt.font);
    const int lineH = fm.height();

    // Tool rows hidden by the showTools setting collapse to nothing.
    if (kind == TranscriptModel::Tool
        && !idx.data(TranscriptModel::ToolVisibleRole).toBool()) {
        return 0;
    }

    const int contentLeft = kOuterMarginX;
    const int contentWidth = width - 2 * kOuterMarginX;
    if (contentWidth <= 0) {
        return lineH;
    }

    if (kind == TranscriptModel::Note) {
        const int textW = contentWidth - 2 * kNotePadX;
        auto *doc = (self->*buildDoc)(idx, qMax(1, textW), opt);
        const int h = int(doc->size().height()) + 2 * kNotePadY;
        if (painter) {
            painter->save();
            painter->translate(rowRect.left() + contentLeft + kNotePadX,
                               rowRect.top() + kNotePadY);
            QAbstractTextDocumentLayout::PaintContext ctx;
            ctx.palette = opt.palette;
            ctx.palette.setColor(QPalette::Text,
                                 noteColor(idx.data(TranscriptModel::NoteKindRole).toString(),
                                           dark));
            doc->documentLayout()->draw(painter, ctx);
            painter->restore();
        }
        delete doc;
        return h;
    }

    if (kind == TranscriptModel::Message) {
        const int innerW = contentWidth - 2 * kCardPadX;
        auto *doc = (self->*buildDoc)(idx, qMax(1, innerW), opt);
        const int bodyH = int(doc->size().height());
        // Attachment chip block under the body (You messages with attachments).
        const QJsonArray atts =
            idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
        int chipsH = 0;
        if (!atts.isEmpty()) {
            layoutAttachmentChips(atts, opt.font, QPoint(0, 0), qMax(1, innerW),
                                  chipsH);
        }
        const int chipsBlock = chipsH > 0 ? kChipGapTop + chipsH : 0;
        const int total =
            kCardPadTop + lineH + kRoleRowGap + bodyH + chipsBlock + kCardPadBottom;
        if (painter) {
            const QRect card(rowRect.left() + contentLeft, rowRect.top(),
                             contentWidth, total);
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            painter->setPen(Qt::NoPen);
            painter->setBrush(opt.palette.color(QPalette::AlternateBase));
            painter->drawRoundedRect(card, 8, 8);
            painter->restore();

            // Role row: accent label left, dim timestamp right.
            const int rrTop = rowRect.top() + kCardPadTop;
            const QString role = idx.data(TranscriptModel::RoleTextRole).toString();
            QFont bold = opt.font;
            bold.setBold(true);
            painter->save();
            painter->setFont(bold);
            painter->setPen(QColor(idx.data(TranscriptModel::AccentRole).toString()));
            painter->drawText(QRect(card.left() + kCardPadX, rrTop, innerW, lineH),
                              Qt::AlignLeft | Qt::AlignVCenter, role);
            painter->restore();
            if (!idx.data(TranscriptModel::ReplayedRole).toBool()) {
                const QString ts = idx.data(TranscriptModel::TimestampRole).toString();
                painter->save();
                QFont small = opt.font;
                small.setPointSizeF(opt.font.pointSizeF() * 0.85);
                painter->setFont(small);
                painter->setPen(opt.palette.color(QPalette::Mid));
                painter->drawText(QRect(card.left() + kCardPadX, rrTop, innerW, lineH),
                                  Qt::AlignRight | Qt::AlignVCenter, ts);
                painter->restore();
            }

            // Body HTML.
            const int bodyTop = rrTop + lineH + kRoleRowGap;
            painter->save();
            painter->translate(card.left() + kCardPadX, bodyTop);
            QAbstractTextDocumentLayout::PaintContext ctx;
            ctx.palette = opt.palette;
            doc->documentLayout()->draw(painter, ctx);
            painter->restore();

            // Attachment chips, laid out in the coordinate space of the card so
            // the chip rects here match the hit-test rects in editorEvent().
            if (chipsH > 0) {
                const QPoint origin(card.left() + kCardPadX,
                                    bodyTop + bodyH + kChipGapTop);
                int dummy = 0;
                const QList<ChipLayout> laid = layoutAttachmentChips(
                    atts, opt.font, origin, qMax(1, innerW), dummy);
                painter->save();
                painter->setRenderHint(QPainter::Antialiasing, true);
                QFont chipFont = opt.font;
                for (int i = 0; i < laid.size(); ++i) {
                    const ChipLayout &c = laid.at(i);
                    const QJsonObject att = atts.at(i).toObject();
                    // Chip background + border (palette-only).
                    painter->setPen(opt.palette.color(QPalette::Mid));
                    painter->setBrush(opt.palette.color(QPalette::Base));
                    painter->drawRoundedRect(c.rect.adjusted(0, 0, -1, -1), 6, 6);
                    int textLeft = c.rect.left() + kChipPadX;
                    if (c.image) {
                        // Decode the stored path into a small thumbnail. Chips on
                        // replayed cards have only a path (no dataB64), so load
                        // from the file; a moved file just falls back to no icon.
                        const QString path =
                            att.value(QStringLiteral("path")).toString();
                        QPixmap pm(path);
                        const QRect iconR(textLeft, c.rect.top() + (kChipH - kChipIcon) / 2,
                                          kChipIcon, kChipIcon);
                        if (!pm.isNull()) {
                            painter->drawPixmap(
                                iconR,
                                pm.scaled(kChipIcon, kChipIcon, Qt::KeepAspectRatio,
                                          Qt::SmoothTransformation));
                        } else {
                            painter->setPen(opt.palette.color(QPalette::Mid));
                            painter->drawText(iconR, Qt::AlignCenter,
                                              QStringLiteral("\U0001f5bc"));
                        }
                        textLeft += kChipIcon + kChipHGap;
                    }
                    painter->setFont(chipFont);
                    painter->setPen(opt.palette.color(
                        att.value(QStringLiteral("outside")).toBool()
                            ? QPalette::LinkVisited
                            : QPalette::Link));
                    painter->drawText(
                        QRect(textLeft, c.rect.top(),
                              c.rect.right() - textLeft - kChipPadX + 1, c.rect.height()),
                        Qt::AlignLeft | Qt::AlignVCenter, c.label);
                }
                painter->restore();
            }
        }
        delete doc;
        return total;
    }

    if (kind == TranscriptModel::Thinking) {
        // A quiet, collapsible reasoning row: no card frame, just a dim header
        // line ("▸ 💭 Thinking · preview") that expands to the dim body.
        const bool expanded = idx.data(TranscriptModel::ToolExpandedRole).toBool();
        int total = kToolHeaderH;
        QTextDocument *bodyDoc = nullptr;
        int bodyH = 0;
        const int bodyW = contentWidth - 2 * kDetailPadX;
        if (expanded) {
            bodyDoc = (self->*buildDoc)(idx, qMax(1, bodyW), opt);
            bodyH = int(bodyDoc->size().height());
            total += kToolPad + bodyH + kToolPad;
        }
        if (painter) {
            const QString arrow = expanded ? QStringLiteral("▾") : QStringLiteral("▸");
            const QString preview = idx.data(TranscriptModel::ToolSummaryRole).toString();
            const QString header = QStringLiteral("%1  \U0001f4ad %2   %3")
                                       .arg(arrow, i18n("Thinking"), preview);
            const QRect hdr(rowRect.left() + contentLeft + 8, rowRect.top(),
                            contentWidth - 16, kToolHeaderH);
            painter->save();
            painter->setPen(opt.palette.color(QPalette::Mid));
            painter->drawText(hdr, Qt::AlignLeft | Qt::AlignVCenter,
                              fm.elidedText(header, Qt::ElideRight, hdr.width()));
            painter->restore();
            if (bodyDoc) {
                painter->save();
                painter->translate(rowRect.left() + contentLeft + kDetailPadX,
                                   rowRect.top() + kToolHeaderH + kToolPad);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                ctx.palette.setColor(QPalette::Text, ThemeManager::palette().agentIdle);
                bodyDoc->documentLayout()->draw(painter, ctx);
                painter->restore();
            }
        }
        delete bodyDoc;
        return total;
    }

    if (kind == TranscriptModel::Checklist) {
        // The agent's plan: a bordered card listing every item with a status
        // glyph. Always fully visible — the plan is the thing to watch.
        const QJsonArray items = idx.data(TranscriptModel::ChecklistRole).toJsonArray();
        const int itemH = lineH + 4;
        const int total = kToolHeaderH + items.size() * itemH + kToolPad;
        if (painter) {
            const QRect card(rowRect.left() + contentLeft, rowRect.top(),
                             contentWidth, total);
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            painter->setPen(opt.palette.color(QPalette::Mid));
            painter->setBrush(Qt::NoBrush);
            painter->drawRoundedRect(card.adjusted(0, 0, -1, -1), 7, 7);
            painter->restore();

            int done = 0;
            for (const QJsonValue &v : items) {
                if (v.toObject().value(QStringLiteral("status")).toString()
                    == QLatin1String("completed")) {
                    ++done;
                }
            }
            const QString header =
                QStringLiteral("☑ %1   %2")
                    .arg(i18n("Plan"),
                         i18nc("checklist progress", "%1 of %2 done", done, items.size()));
            painter->save();
            painter->setPen(opt.palette.color(QPalette::WindowText));
            QFont bold = opt.font;
            bold.setBold(true);
            painter->setFont(bold);
            painter->drawText(QRect(card.left() + 8, card.top(), card.width() - 16,
                                    kToolHeaderH),
                              Qt::AlignLeft | Qt::AlignVCenter,
                              fm.elidedText(header, Qt::ElideRight, card.width() - 16));
            painter->restore();

            int y = card.top() + kToolHeaderH;
            painter->save();
            for (const QJsonValue &v : items) {
                const QJsonObject item = v.toObject();
                const QString status =
                    item.value(QStringLiteral("status")).toString();
                const QString text = item.value(QStringLiteral("content")).toString();
                QString glyph = QStringLiteral("☐"); // ☐ pending
                QFont f = opt.font;
                QColor pen = opt.palette.color(QPalette::WindowText);
                if (status == QLatin1String("completed")) {
                    glyph = QStringLiteral("✓"); // ✓
                    f.setStrikeOut(true);
                    pen = opt.palette.color(QPalette::Mid);
                } else if (status == QLatin1String("in_progress")) {
                    glyph = QStringLiteral("▸"); // ▸
                    f.setBold(true);
                    pen = ThemeManager::palette().agentRunning;
                }
                painter->setFont(f);
                painter->setPen(pen);
                const QRect line(card.left() + kDetailPadX, y,
                                 card.width() - 2 * kDetailPadX, itemH);
                painter->drawText(line, Qt::AlignLeft | Qt::AlignVCenter,
                                  QFontMetrics(f).elidedText(
                                      glyph + QStringLiteral("  ") + text,
                                      Qt::ElideRight, line.width()));
                y += itemH;
            }
            painter->restore();
        }
        return total;
    }

    // --- Tool card -------------------------------------------------------
    const QString name = idx.data(TranscriptModel::ToolNameRole).toString();
    const QString summary = idx.data(TranscriptModel::ToolSummaryRole).toString();
    const bool expanded = idx.data(TranscriptModel::ToolExpandedRole).toBool();
    const bool done = idx.data(TranscriptModel::ToolDoneRole).toBool();

    int total = kToolHeaderH;
    // Detail (input JSON + result) measured with the mono document only when open.
    QTextDocument *detailDoc = nullptr;
    QTextDocument *resultDoc = nullptr;
    int detailH = 0;
    int resultH = 0;
    int extraH = 0; // "show full output" link line
    const int detailW = contentWidth - 2 * kDetailPadX;
    if (expanded) {
        QFont mono = opt.font;
        mono.setFamily(QStringLiteral("monospace"));
        mono.setPointSizeF(opt.font.pointSizeF() * 0.9);
        const QString detail = idx.data(TranscriptModel::ToolDetailRole).toString();
        if (!detail.isEmpty()) {
            detailDoc = new QTextDocument;
            detailDoc->setDefaultFont(mono);
            detailDoc->setDocumentMargin(0);
            detailDoc->setPlainText(detail);
            detailDoc->setTextWidth(qMax(1, detailW));
            detailH = int(detailDoc->size().height());
        }
        QString result = idx.data(TranscriptModel::ToolResultRole).toString();
        if (done) {
            if (result.isEmpty()) {
                result = QStringLiteral("(no output)");
            }
            resultDoc = new QTextDocument;
            resultDoc->setDefaultFont(mono);
            resultDoc->setDocumentMargin(0);
            resultDoc->setPlainText(result);
            resultDoc->setTextWidth(qMax(1, detailW));
            resultH = int(resultDoc->size().height());
        }
        if (idx.data(TranscriptModel::ToolTruncatedRole).toBool()) {
            extraH = lineH + kToolPad;
        }
        total += kToolPad + detailH + (detailH && resultH ? kToolPad : 0) + resultH
               + extraH + kToolPad;
    }

    if (painter) {
        const QRect card(rowRect.left() + contentLeft, rowRect.top(),
                         contentWidth, total);
        painter->save();
        painter->setRenderHint(QPainter::Antialiasing, true);
        painter->setPen(opt.palette.color(QPalette::Mid));
        painter->setBrush(Qt::NoBrush);
        painter->drawRoundedRect(card.adjusted(0, 0, -1, -1), 7, 7);
        painter->restore();

        // Header line: arrow + mark + 🔧 name + summary, with a copy glyph right.
        const QString arrow = expanded ? QStringLiteral("▾") : QStringLiteral("▸");
        const QString mark = done ? QStringLiteral("✓") : QStringLiteral("⋯");
        const QString header = QStringLiteral("%1  %2  \U0001f527 %3   %4")
                                   .arg(arrow, mark, name, summary);
        const QRect hdr(card.left() + 8, card.top(),
                        card.width() - 8 - kToolCopyW - kToolInspectW, kToolHeaderH);
        painter->save();
        painter->setPen(opt.palette.color(QPalette::WindowText));
        painter->drawText(hdr, Qt::AlignLeft | Qt::AlignVCenter,
                          fm.elidedText(header, Qt::ElideRight, hdr.width()));
        painter->setPen(opt.palette.color(QPalette::Mid));
        // "Open in inspector" glyph, then the copy glyph on the far right.
        const QRect inspectR(card.right() - kToolCopyW - kToolInspectW, card.top(),
                             kToolInspectW, kToolHeaderH);
        painter->drawText(inspectR, Qt::AlignCenter, QStringLiteral("⤢"));
        const QRect copyR(card.right() - kToolCopyW, card.top(), kToolCopyW,
                          kToolHeaderH);
        painter->drawText(copyR, Qt::AlignCenter, QStringLiteral("⧉"));
        painter->restore();

        if (expanded) {
            int y = card.top() + kToolHeaderH + kToolPad;
            QFont mono = opt.font;
            mono.setFamily(QStringLiteral("monospace"));
            painter->save();
            painter->setPen(opt.palette.color(QPalette::WindowText));
            if (detailDoc) {
                painter->save();
                painter->translate(card.left() + kDetailPadX, y);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                detailDoc->documentLayout()->draw(painter, ctx);
                painter->restore();
                y += detailH + kToolPad;
            }
            if (resultDoc) {
                painter->save();
                painter->translate(card.left() + kDetailPadX, y);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                resultDoc->documentLayout()->draw(painter, ctx);
                painter->restore();
                y += resultH;
            }
            if (idx.data(TranscriptModel::ToolTruncatedRole).toBool()) {
                y += kToolPad;
                painter->setPen(opt.palette.color(QPalette::Link));
                painter->drawText(QRect(card.left() + kDetailPadX, y, detailW, lineH),
                                  Qt::AlignLeft | Qt::AlignVCenter,
                                  QStringLiteral("Show full output"));
            }
            painter->restore();
        }
    }

    delete detailDoc;
    delete resultDoc;
    return total;
}
} // namespace

QSize TranscriptDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                   const QModelIndex &idx) const
{
    const int width = opt.rect.width() > 0 ? opt.rect.width()
                                           : (opt.widget ? opt.widget->width() : 400);
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    auto cit = m_heightCache.constFind(id);
    if (cit != m_heightCache.constEnd()) {
        if (cit->width == width) {
            return QSize(width, cit->height); // exact — width unchanged
        }
        // Width changed: hand back the cached height as an estimate WITHOUT
        // rebuilding the (expensive) QTextDocument, so the view's full-height
        // pass over every row during a drag is O(N) hash lookups, not N layouts.
        // Flag for a settle-time exact re-measure of the visible rows.
        m_dirtyResize = true;
        return QSize(width, cit->height);
    }
    // First sight of this row — measure it exactly (this is O(visible rows): the
    // view only asks for rows it is about to show).
    const int h = layoutRow(idx, width, opt, nullptr, QRect(), this,
                            &TranscriptDelegate::buildBodyDoc);
    if (m_heightCache.size() >= kHeightCacheCap) {
        m_heightCache.clear();
    }
    m_heightCache.insert(id, CacheEntry{width, h});
    return QSize(width, h);
}

int TranscriptDelegate::measureExact(const QModelIndex &idx, int width,
                                     const QStyleOptionViewItem &opt) const
{
    const int h = layoutRow(idx, width, opt, nullptr, QRect(), this,
                            &TranscriptDelegate::buildBodyDoc);
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    if (m_heightCache.size() >= kHeightCacheCap) {
        m_heightCache.clear();
    }
    m_heightCache.insert(id, CacheEntry{width, h});
    return h;
}

void TranscriptDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                               const QModelIndex &idx) const
{
    painter->save();
    painter->setClipRect(opt.rect);
    layoutRow(idx, opt.rect.width(), opt, painter, opt.rect, this,
              &TranscriptDelegate::buildBodyDoc);
    painter->restore();
}

QRect TranscriptDelegate::toolHeaderRect(const QRect &row) const
{
    return QRect(row.left() + kOuterMarginX, row.top(),
                 row.width() - 2 * kOuterMarginX - kToolCopyW - kToolInspectW,
                 kToolHeaderH);
}

QRect TranscriptDelegate::toolCopyRect(const QRect &row) const
{
    const int right = row.right() - kOuterMarginX;
    return QRect(right - kToolCopyW, row.top(), kToolCopyW, kToolHeaderH);
}

QRect TranscriptDelegate::toolInspectRect(const QRect &row) const
{
    const int right = row.right() - kOuterMarginX;
    return QRect(right - kToolCopyW - kToolInspectW, row.top(), kToolInspectW,
                 kToolHeaderH);
}

int TranscriptDelegate::attachmentsBlockHeight(const QModelIndex &idx,
                                               const QStyleOptionViewItem &opt,
                                               int innerW) const
{
    const QJsonArray atts = idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    if (atts.isEmpty()) {
        return 0;
    }
    int chipsH = 0;
    layoutAttachmentChips(atts, opt.font, QPoint(0, 0), qMax(1, innerW), chipsH);
    return chipsH > 0 ? kChipGapTop + chipsH : 0;
}

QRect TranscriptDelegate::messageBodyRect(const QRect &row,
                                          const QStyleOptionViewItem &opt,
                                          const QModelIndex &idx) const
{
    const int lineH = QFontMetrics(opt.font).height();
    const int left = row.left() + kOuterMarginX + kCardPadX;
    const int top = row.top() + kCardPadTop + lineH + kRoleRowGap;
    const int innerW = row.width() - 2 * kOuterMarginX - 2 * kCardPadX;
    const int chipsBlock = attachmentsBlockHeight(idx, opt, innerW);
    const int bodyH = row.height() - kCardPadTop - lineH - kRoleRowGap
                      - chipsBlock - kCardPadBottom;
    return QRect(left, top, qMax(1, innerW), qMax(0, bodyH));
}

// The chip a point falls in, or -1. Recomputes the chip layout in the card's
// coordinate space (matching paint) and tests each rect.
int TranscriptDelegate::attachmentChipAt(const QRect &row,
                                         const QStyleOptionViewItem &opt,
                                         const QModelIndex &idx, const QPoint &pos) const
{
    const QJsonArray atts = idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    if (atts.isEmpty()) {
        return -1;
    }
    const int lineH = QFontMetrics(opt.font).height();
    const int innerW = row.width() - 2 * kOuterMarginX - 2 * kCardPadX;
    const int bodyTop = row.top() + kCardPadTop + lineH + kRoleRowGap;
    const int chipsBlock = attachmentsBlockHeight(idx, opt, innerW);
    const int bodyH = row.height() - kCardPadTop - lineH - kRoleRowGap
                      - chipsBlock - kCardPadBottom;
    const QPoint origin(row.left() + kOuterMarginX + kCardPadX,
                        bodyTop + bodyH + kChipGapTop);
    int dummy = 0;
    const QList<ChipLayout> laid =
        layoutAttachmentChips(atts, opt.font, origin, qMax(1, innerW), dummy);
    for (int i = 0; i < laid.size(); ++i) {
        if (laid.at(i).rect.contains(pos)) {
            return i;
        }
    }
    return -1;
}

// --- in-place selectable overlay (plan 13 phase 1) -----------------------

QWidget *TranscriptDelegate::createEditor(QWidget *parent,
                                          const QStyleOptionViewItem &opt,
                                          const QModelIndex &idx) const
{
    Q_UNUSED(opt);
    Q_UNUSED(idx);
    // A frameless, read-only text browser laid over the row's body. It shares the
    // exact document setup of the painted body (font, margin, wrap width, HTML) so
    // opening it causes no visual jump — it just makes the same glyphs selectable.
    auto *browser = new QTextBrowser(parent);
    browser->setFrameShape(QFrame::NoFrame);
    browser->setReadOnly(true);
    browser->setContextMenuPolicy(Qt::DefaultContextMenu); // native copy menu
    browser->setTextInteractionFlags(Qt::TextSelectableByMouse
                                     | Qt::TextSelectableByKeyboard
                                     | Qt::LinksAccessibleByMouse);
    browser->setHorizontalScrollBarPolicy(Qt::ScrollBarAlwaysOff);
    browser->setVerticalScrollBarPolicy(Qt::ScrollBarAlwaysOff);
    browser->document()->setDocumentMargin(0);
    // Let the painted card show through: transparent frame + viewport, no opaque
    // fill. The painted body underneath stays visible so opening the overlay is
    // seamless; only the selection highlight is drawn by the browser.
    QPalette pal = browser->palette();
    pal.setColor(QPalette::Base, Qt::transparent);
    browser->setPalette(pal);
    browser->viewport()->setAutoFillBackground(false);
    browser->setAutoFillBackground(false);
    // Links open through the panel's handler, never QTextBrowser navigation.
    browser->setOpenLinks(false);
    browser->setOpenExternalLinks(false);
    connect(browser, &QTextBrowser::anchorClicked, this,
            [this](const QUrl &url) { emit anchorActivated(url.toString()); });
    emit editorCreated(browser);
    return browser;
}

void TranscriptDelegate::setEditorData(QWidget *editor, const QModelIndex &idx) const
{
    auto *browser = qobject_cast<QTextBrowser *>(editor);
    if (!browser) {
        return;
    }
    // Match paint()'s document setup exactly (see configureBodyDoc). The browser
    // is a child of the view's viewport, so it inherits the same font paint()
    // draws with (opt.font). The wrap width is re-applied by updateEditorGeometry
    // once the geometry is known.
    configureBodyDoc(browser->document(), browser->font(),
                     browser->viewport()->width(), resolveBodyHtml(idx));
}

void TranscriptDelegate::updateEditorGeometry(QWidget *editor,
                                              const QStyleOptionViewItem &opt,
                                              const QModelIndex &idx) const
{
    if (TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt())
        != TranscriptModel::Message) {
        return;
    }
    const QRect body = messageBodyRect(opt.rect, opt, idx);
    editor->setGeometry(body);
    if (auto *browser = qobject_cast<QTextBrowser *>(editor)) {
        // Re-wrap the document to the body width now the geometry is known.
        browser->document()->setTextWidth(body.width());
    }
}

bool TranscriptDelegate::editorEvent(QEvent *event, QAbstractItemModel *model,
                                     const QStyleOptionViewItem &opt,
                                     const QModelIndex &idx)
{
    auto *tm = qobject_cast<TranscriptModel *>(model);
    if (!tm) {
        return QStyledItemDelegate::editorEvent(event, model, opt, idx);
    }
    const auto kind = TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());

    if (event->type() == QEvent::MouseButtonRelease) {
        auto *me = static_cast<QMouseEvent *>(event);
        if (me->button() != Qt::LeftButton) {
            return false;
        }
        const QPoint pos = me->pos();

        if (kind == TranscriptModel::Tool
            && idx.data(TranscriptModel::ToolVisibleRole).toBool()) {
            // "Open in inspector" glyph — opens the full tool-call modal.
            if (toolInspectRect(opt.rect).contains(pos)) {
                emit inspectToolRequested(idx);
                return true;
            }
            // Copy button.
            if (toolCopyRect(opt.rect).contains(pos)) {
                QString out = idx.data(TranscriptModel::ToolNameRole).toString();
                const QString detail = idx.data(TranscriptModel::ToolDetailRole).toString();
                const QString full = idx.data(TranscriptModel::ToolFullResultRole).toString();
                if (!detail.isEmpty()) {
                    out += QStringLiteral("\n\n") + detail;
                }
                if (!full.isEmpty()) {
                    out += QStringLiteral("\n\n") + full;
                }
                QGuiApplication::clipboard()->setText(out);
                return true;
            }
            // "Show full output" link, when expanded + truncated. It sits on the
            // last text line, so hit-test the bottom band of the row.
            if (idx.data(TranscriptModel::ToolExpandedRole).toBool()
                && idx.data(TranscriptModel::ToolTruncatedRole).toBool()) {
                if (pos.y() > opt.rect.bottom() - QFontMetrics(opt.font).height()
                                  - kToolPad) {
                    tm->expandToolResult(idx.row());
                    return true;
                }
            }
            // Header toggles expand/collapse.
            if (toolHeaderRect(opt.rect).contains(pos)) {
                tm->setExpanded(idx.row(), !idx.data(TranscriptModel::ToolExpandedRole).toBool());
                return true;
            }
            return false;
        }

        if (kind == TranscriptModel::Thinking) {
            // The header line toggles the reasoning body open/closed.
            if (QRect(opt.rect.left(), opt.rect.top(), opt.rect.width(), kToolHeaderH)
                    .contains(pos)) {
                tm->setExpanded(idx.row(),
                                !idx.data(TranscriptModel::ToolExpandedRole).toBool());
                return true;
            }
            return false;
        }

        if (kind == TranscriptModel::Message) {
            // An attachment chip click opens that file (image preview / editor).
            const int chip = attachmentChipAt(opt.rect, opt, idx, pos);
            if (chip >= 0) {
                const QJsonArray atts =
                    idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
                if (chip < atts.size()) {
                    emit attachmentActivated(atts.at(chip).toObject());
                    return true;
                }
            }
            // A click on a link opens it directly (the overlay isn't up on the
            // first click). Otherwise a click on the body asks the panel to open
            // the persistent selection overlay so the text becomes selectable.
            const int innerW = opt.rect.width() - 2 * kOuterMarginX - 2 * kCardPadX;
            QTextDocument *doc = buildBodyDoc(idx, qMax(1, innerW), opt);
            const QFontMetrics fm(opt.font);
            const QPointF rel(pos.x() - (opt.rect.left() + kOuterMarginX + kCardPadX),
                              pos.y() - (opt.rect.top() + kCardPadTop + fm.height()
                                         + kRoleRowGap));
            const QString anchor = doc->documentLayout()->anchorAt(rel);
            delete doc;
            if (!anchor.isEmpty()) {
                QDesktopServices::openUrl(QUrl(anchor));
                return true;
            }
            if (messageBodyRect(opt.rect, opt, idx).contains(pos)) {
                emit messageBodyClicked(idx);
                return true;
            }
        }
        return false;
    }

    return QStyledItemDelegate::editorEvent(event, model, opt, idx);
}

bool TranscriptDelegate::helpEvent(QHelpEvent *event, QAbstractItemView *view,
                                   const QStyleOptionViewItem &opt,
                                   const QModelIndex &idx)
{
    if (event->type() != QEvent::ToolTip) {
        return QStyledItemDelegate::helpEvent(event, view, opt, idx);
    }
    const QPoint pos = event->pos();
    const auto kind = TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());

    if (kind == TranscriptModel::Message) {
        // An attachment chip shows its full path, marking files outside the
        // workspace so the user knows the reference reaches beyond the project.
        const int chip = attachmentChipAt(opt.rect, opt, idx, pos);
        if (chip >= 0) {
            const QJsonArray atts =
                idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
            if (chip < atts.size()) {
                const QJsonObject att = atts.at(chip).toObject();
                QString tip = att.value(QStringLiteral("path")).toString();
                if (tip.isEmpty()) {
                    tip = att.value(QStringLiteral("name")).toString();
                }
                if (att.value(QStringLiteral("outside")).toBool()) {
                    tip += QStringLiteral("\n") + i18n("(outside workspace)");
                }
                QToolTip::showText(event->globalPos(), tip, view);
                return true;
            }
        }
    }

    if (kind == TranscriptModel::Tool
        && idx.data(TranscriptModel::ToolVisibleRole).toBool()) {
        if (toolInspectRect(opt.rect).contains(pos)) {
            QToolTip::showText(event->globalPos(), i18n("Open in inspector"), view);
            return true;
        }
        if (toolCopyRect(opt.rect).contains(pos)) {
            QToolTip::showText(event->globalPos(), i18n("Copy tool call"), view);
            return true;
        }
    }

    QToolTip::hideText();
    event->ignore();
    return QStyledItemDelegate::helpEvent(event, view, opt, idx);
}
