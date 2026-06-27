// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TranscriptDelegate.h"
#include "TranscriptModel.h"

#include <QAbstractItemView>
#include <QAbstractTextDocumentLayout>
#include <QApplication>
#include <QDesktopServices>
#include <QGuiApplication>
#include <QClipboard>
#include <QFontMetrics>
#include <QMouseEvent>
#include <QPainter>
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
constexpr int kToolPad = 6;       // tool card inner padding
constexpr int kToolHeaderH = 28;  // clickable header height
constexpr int kToolCopyW = 26;    // copy hit zone on the right of the header
constexpr int kDetailPadX = 10;

// The height cache is keyed by the model's per-row stableId, which the model
// bumps on every mutation (so the old entry is never looked up again) — left
// unchecked it grows for the life of the panel. Cap it; when full we drop the
// whole cache and the currently-visible rows re-measure lazily (cheap).
constexpr int kHeightCacheCap = 16384;

// noteColor mirrors AgentPanel.cpp's palette-aware dim/ok/err colours.
QColor noteColor(const QString &kind, bool dark)
{
    if (kind == QLatin1String("ok")) {
        return dark ? QColor(0x5f, 0xd3, 0x8a) : QColor(0x1a, 0x7f, 0x37);
    }
    if (kind == QLatin1String("err")) {
        return dark ? QColor(0xff, 0x8a, 0x80) : QColor(0xc0, 0x1c, 0x28);
    }
    return dark ? QColor(0x9a, 0x9a, 0xa3) : QColor(0x6b, 0x6b, 0x72);
}

// Wrap matches of `needle` in `plain` with a highlight span, escaping first.
// Used by the find overlay so the highlighted body matches the QLabel days.
QString highlightedHtml(const QString &plain, const QString &needle)
{
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
        out += QStringLiteral("<span style='background:palette(highlight); "
                              "color:palette(highlighted-text)'>")
               + escaped.mid(at, escNeedle.length()) + QStringLiteral("</span>");
        from = at + escNeedle.length();
    }
    return out.replace(QLatin1Char('\n'), QStringLiteral("<br>"));
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
    const auto kind = TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());
    auto *doc = new QTextDocument;
    doc->setDefaultFont(opt.font);
    doc->setDocumentMargin(0);

    QString html = idx.data(TranscriptModel::HtmlRole).toString();
    // Apply the find highlight to the matching message body only.
    if (kind == TranscriptModel::Message) {
        const auto *m = qobject_cast<const TranscriptModel *>(idx.model());
        if (m && !m->findNeedle().isEmpty()) {
            const QString plain = idx.data(TranscriptModel::PlainRole).toString();
            if (plain.contains(m->findNeedle(), Qt::CaseInsensitive)) {
                html = highlightedHtml(plain, m->findNeedle());
            }
        }
    }
    doc->setHtml(html);
    doc->setTextWidth(contentWidth);
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
        const int total = kCardPadTop + lineH + kRoleRowGap + bodyH + kCardPadBottom;
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
            painter->save();
            painter->translate(card.left() + kCardPadX,
                               rrTop + lineH + kRoleRowGap);
            QAbstractTextDocumentLayout::PaintContext ctx;
            ctx.palette = opt.palette;
            doc->documentLayout()->draw(painter, ctx);
            painter->restore();
        }
        delete doc;
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
                        card.width() - 8 - kToolCopyW, kToolHeaderH);
        painter->save();
        painter->setPen(opt.palette.color(QPalette::WindowText));
        painter->drawText(hdr, Qt::AlignLeft | Qt::AlignVCenter,
                          fm.elidedText(header, Qt::ElideRight, hdr.width()));
        // Copy glyph.
        const QRect copyR(card.right() - kToolCopyW, card.top(), kToolCopyW,
                          kToolHeaderH);
        painter->setPen(opt.palette.color(QPalette::Mid));
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
                 row.width() - 2 * kOuterMarginX - kToolCopyW, kToolHeaderH);
}

QRect TranscriptDelegate::toolCopyRect(const QRect &row) const
{
    const int right = row.right() - kOuterMarginX;
    return QRect(right - kToolCopyW, row.top(), kToolCopyW, kToolHeaderH);
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

        if (kind == TranscriptModel::Message) {
            // Open an anchor if the click landed on a link.
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
        }
        return false;
    }

    if (event->type() == QEvent::MouseButtonDblClick && kind == TranscriptModel::Message) {
        // Double-click copies the whole message (selection across rows isn't
        // available in the virtualized feed; this covers the common copy path).
        const QString plain = idx.data(TranscriptModel::PlainRole).toString();
        if (!plain.isEmpty()) {
            QGuiApplication::clipboard()->setText(plain);
            return true;
        }
    }

    return QStyledItemDelegate::editorEvent(event, model, opt, idx);
}
