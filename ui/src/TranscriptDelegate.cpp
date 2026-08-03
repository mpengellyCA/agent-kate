// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "AttachmentBuilder.h"
#include "SafeContent.h"
#include "state/ChatAppearance.h"
#include "theme/ThemeManager.h"

#include <QAbstractItemView>
#include <QAbstractTextDocumentLayout>
#include <QApplication>
#include <QGuiApplication>
#include <QClipboard>
#include <QFontMetrics>
#include <QHelpEvent>
#include <QToolTip>
#include <KLocalizedString>
#include <QJsonArray>
#include <QJsonObject>
#include <QDateTime>
#include <QFileInfo>
#include <QImageReader>
#include <QMouseEvent>
#include <QPixmap>
#include <QPixmapCache>
#include <QPainter>
#include <QTextBrowser>
#include <QTextDocument>
#include <QUrl>

namespace {
// Remaining tool-local geometry. Transcript-wide geometry belongs to
// TranscriptMetrics so density changes remain coherent across measure, paint
// and hit testing.
constexpr int kToolPad = 6;       // tool card inner padding
constexpr int kToolCopyW = 26;    // copy hit zone on the right of the header
constexpr int kToolInspectW = 26; // "open in inspector" glyph, left of the copy glyph
constexpr int kDetailPadX = 10;

// The height cache is keyed by the model's per-row stableId. A mutation drops
// that row's entry (TranscriptModel::heightInvalidated → invalidateRow), so the
// cache no longer accumulates a dead entry per streaming tick; what it still
// accumulates is one entry per row EVICTED off the front of the feed, whose id
// is never queried again. Cap it as the backstop; when full we drop the whole
// cache and the currently-visible rows re-measure lazily (cheap).
constexpr int kHeightCacheCap = 16384;

// Laid-out body documents kept for reuse between sizeHint() and paint(). Only
// the rows the view is currently showing are asked for repeatedly, so a cap a
// little above a viewport's worth covers the working set. It is kept small on
// purpose: a laid-out document of a 128KB row is not cheap in RAM, and this
// cache exists to save layouts, not to hold the transcript. Full means "drop
// them all" — they rebuild lazily, at most one layout each. (Safe because a
// document is used within the single call that fetched it, never across one.)
constexpr int kDocCacheCap = 32;

// Attachment path resolutions kept between paints, and how long one is trusted
// before the file is stat'ed again. A second of staleness is invisible (the
// thumbnail of a file rewritten under us refreshes on the next paint after the
// TTL) and it takes the syscalls out of the paint path entirely.
constexpr int kAttCacheCap = 512;
constexpr qint64 kAttStatTtlMs = 1000;

// Find-highlighted body HTML kept between paints (one entry per matching row on
// screen plus slack). Like the doc caches it exists to save rebuilds, not to
// hold the transcript: full means "drop them all", they rebuild lazily.
constexpr int kHighlightCacheCap = 64;

// chipThumbnail returns an attachment's preview icon, decoded AT icon size and
// kept in QPixmapCache.
//
// Chips repaint on every scroll tick, and this used to construct a QPixmap from
// the path each time: a 4K screenshot was fully decoded, then scaled to 16px,
// then thrown away, per chip per paint. QImageReader::setScaledSize lets the
// decoder do the downscale, and the cache means a still transcript decodes once.
//
// The key carries size + mtime so a fixed-name capture file that changed bytes
// re-decodes instead of redrawing the previous screenshot; eviction is
// QPixmapCache's business. Returns a null pixmap for anything unreadable, which
// the caller draws as the generic glyph.
//
// `size`/`mtime` are supplied by the caller (see
// TranscriptDelegate::resolveAttachmentCached) rather than stat'ed here: this
// runs once per image chip per paint, and the stat it used to do was pure
// overhead on every scroll tick of an unchanged file.
//
// Failures are cached too, under a sibling key. A truncated or non-image file
// otherwise re-ran the full decode on every paint of every scroll tick, forever
// — the one case where the cache bought nothing. The entry is a 1x1 pixmap
// because QPixmapCache cannot store a null one, and it is kept under its own key
// so a genuinely 1x1 image is not mistaken for it. Since the key carries size
// and mtime, repairing the file retries the decode.
QPixmap chipThumbnail(const TranscriptDelegate::ResolvedAttachment &att, int edge,
                      qreal dpr)
{
    const QString &path = att.path;
    if (path.isEmpty() || att.size < 0 || edge <= 0) {
        return {};
    }
    const QString key = QStringLiteral("ak:chip:%1|%2|%3|%4|%5")
                            .arg(path)
                            .arg(att.size)
                            .arg(att.mtime)
                            .arg(edge)
                            .arg(qRound(dpr * 100));
    const QString failKey = key + QStringLiteral("|!");
    const auto cacheFailure = [&failKey] {
        QPixmap sentinel(1, 1);
        sentinel.fill(Qt::transparent);
        QPixmapCache::insert(failKey, sentinel);
        return QPixmap();
    };
    QPixmap pm;
    if (QPixmapCache::find(key, &pm)) {
        return pm;
    }
    if (QPixmapCache::find(failKey, &pm)) {
        return {}; // decoded before and failed; don't pay for it again
    }
    const int target = qMax(1, qRound(edge * dpr));
    QImageReader reader(path);
    reader.setAutoTransform(true);
    QSize src = reader.size();
    if (src.isValid() && !src.isEmpty()) {
        src.scale(target, target, Qt::KeepAspectRatio);
        reader.setScaledSize(src.expandedTo(QSize(1, 1)));
    }
    QImage img = reader.read();
    if (img.isNull()) {
        return cacheFailure();
    }
    // reader.size() is invalid for any format whose header the plugin cannot
    // pre-parse, so setScaledSize above never ran and this decoded at full size.
    // Without the fallback a 4K screenshot is cached and painted 3840px wide.
    if (img.width() > target || img.height() > target) {
        img = img.scaled(target, target, Qt::KeepAspectRatio, Qt::SmoothTransformation);
    }
    pm = QPixmap::fromImage(img);
    pm.setDevicePixelRatio(dpr);
    QPixmapCache::insert(key, pm);
    return pm;
}

// drawChipThumbnail centres an already-sized thumbnail in the chip's icon box,
// so a non-square image keeps its aspect ratio instead of being stretched.
void drawChipThumbnail(QPainter *painter, const QRect &iconR, const QPixmap &pm)
{
    QSizeF sz = pm.deviceIndependentSize();
    // Clamped regardless of how the pixmap was sized: a thumbnail wider than its
    // box would paint straight over the chip's label and its neighbours.
    if (sz.width() > iconR.width() || sz.height() > iconR.height()) {
        sz.scale(QSizeF(iconR.size()), Qt::KeepAspectRatio);
    }
    const QRect fit(iconR.left() + (iconR.width() - qRound(sz.width())) / 2,
                    iconR.top() + (iconR.height() - qRound(sz.height())) / 2,
                    qRound(sz.width()), qRound(sz.height()));
    painter->drawPixmap(fit, pm);
}

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

QString cssColor(const QColor &color)
{
    return color.name(QColor::HexRgb);
}

// The single source of transcript typography. Qt supports this subset on its
// rich-text engine; installing it before setHtml is important because the safe
// markdown HTML then inherits it both in paint and in the selection overlay.
void configureTranscriptDocument(QTextDocument *doc, const TranscriptMetrics &metrics,
                                 const QPalette &palette, int contentWidth,
                                 const QString &html)
{
    const QString family = metrics.bodyFont.family().replace(QLatin1Char('\''),
                                                               QStringLiteral("\\'"));
    const QString mono = metrics.codeFont.family().replace(QLatin1Char('\''),
                                                            QStringLiteral("\\'"));
    const qreal bodyPt = metrics.bodyFont.pointSizeF();
    const qreal codePt = metrics.codeFont.pointSizeF();
    const AkColors &colors = ThemeManager::palette();
    doc->setDefaultFont(metrics.bodyFont);
    doc->setDocumentMargin(0);
    doc->setDefaultStyleSheet(QStringLiteral(
        "body { font-family: '%1'; font-size: %2pt; color: %3; }"
        "p { margin-top: 0px; margin-bottom: 8px; }"
        "h1 { font-size: 1.45em; margin: 10px 0px 6px 0px; }"
        "h2 { font-size: 1.25em; margin: 10px 0px 6px 0px; }"
        "h3 { font-size: 1.1em; margin: 8px 0px 4px 0px; }"
        "ul, ol { margin: 4px 0px 8px 20px; }"
        "li { margin-bottom: 3px; }"
        "blockquote { margin: 6px 0px; padding-left: 8px; color: %4; }"
        "code { font-family: '%5'; font-size: %6pt; background-color: %7; }"
        "pre { font-family: '%5'; font-size: %6pt; background-color: %7;"
        " padding: 7px; white-space: pre-wrap; }"
        "table { border-collapse: collapse; margin: 6px 0px; }"
        "th, td { border: 1px solid %8; padding: 4px; }"
        "a { color: %9; text-decoration: underline; }")
        .arg(family, QString::number(bodyPt), cssColor(palette.color(QPalette::Text)),
             cssColor(colors.chatMetadata), mono, QString::number(codePt),
             cssColor(colors.chatCodeSurface), cssColor(colors.chatBorder),
             cssColor(palette.color(QPalette::Link))));
    doc->setHtml(html);
    doc->setTextWidth(contentWidth);
}

// A compact durable attachment object. Its details are intentionally derived
// only from fields already in the model: resolving a path or stat'ing a file
// while scrolling would turn a rich visual into a scroll-path regression.
struct AttachmentTileLayout {
    QRect rect;
    QString name;    // already elided to fit
    QString detail;  // type-only metadata, never synchronous file metadata
    bool image = false;
    QString glyph;
};

QString attachmentDetail(const QJsonObject &att)
{
    const QString mediaType = att.value(QStringLiteral("mediaType")).toString();
    if (!mediaType.isEmpty()) {
        const int slash = mediaType.indexOf(QLatin1Char('/'));
        const QString subtype = slash >= 0 ? mediaType.mid(slash + 1).toUpper()
                                           : mediaType.toUpper();
        return i18nc("attachment type", "%1 file", subtype);
    }
    const QString kind = att.value(QStringLiteral("kind")).toString();
    if (kind == QLatin1String("image"))
        return i18n("Image");
    if (kind == QLatin1String("text"))
        return i18n("Text file");
    return i18n("Attachment");
}

QString attachmentGlyph(const QJsonObject &att, bool image)
{
    if (image)
        return QStringLiteral("▧");
    if (att.value(QStringLiteral("kind")).toString() == QLatin1String("text"))
        return QStringLiteral("▤");
    return QStringLiteral("▱");
}

// Shared by message and tool-result attachments. It has no file-system work:
// thumbnails are resolved only at paint through the delegate's bounded cache.
QList<AttachmentTileLayout> layoutAttachmentTiles(const QJsonArray &atts,
                                                   const TranscriptMetrics &metrics,
                                                   const QPoint &origin, int availW,
                                                   int &outHeight)
{
    QList<AttachmentTileLayout> tiles;
    outHeight = 0;
    if (atts.isEmpty() || availW <= 0) {
        return tiles;
    }
    const QFontMetrics nameFm(metrics.bodyFont);
    const QFontMetrics detailFm(metrics.metadataFont);
    int x = origin.x();
    int y = origin.y();
    const int iconEdge = qMax(16, metrics.attachmentTileHeight - 10);
    const int pad = qMax(5, metrics.activityPaddingY);
    const int minW = qMin(availW, qMax(112, iconEdge + pad * 3 + 48));
    for (const QJsonValue &av : atts) {
        const QJsonObject att = av.toObject();
        const QString media = att.value(QStringLiteral("mediaType")).toString();
        const bool image = att.value(QStringLiteral("kind")).toString() == QLatin1String("image")
            || media.startsWith(QLatin1String("image/"), Qt::CaseInsensitive);
        const QString detail = attachmentDetail(att);
        const int maximumText = qMax(1, metrics.attachmentTileMaxWidth - iconEdge - 3 * pad);
        const QString name = nameFm.elidedText(att.value(QStringLiteral("name")).toString(),
                                                Qt::ElideMiddle, maximumText);
        const int textW = qMax(nameFm.horizontalAdvance(name), detailFm.horizontalAdvance(detail));
        const int tileW = qMin(metrics.attachmentTileMaxWidth,
                               qMax(minW, iconEdge + 3 * pad + textW));
        if (x > origin.x() && x + tileW > origin.x() + availW) {
            x = origin.x();
            y += metrics.attachmentTileHeight + metrics.attachmentTileGap;
        }
        tiles.append(AttachmentTileLayout{QRect(x, y, tileW, metrics.attachmentTileHeight),
                                          name, detail, image, attachmentGlyph(att, image)});
        x += tileW + metrics.attachmentTileGap;
    }
    outHeight = (y - origin.y()) + metrics.attachmentTileHeight;
    return tiles;
}

void paintAttachmentTiles(QPainter *painter, const TranscriptDelegate *delegate,
                          const QJsonArray &atts,
                          const QList<AttachmentTileLayout> &tiles,
                          const QPoint &origin, const QStyleOptionViewItem &opt,
                          const TranscriptMetrics &metrics)
{
    const int pad = qMax(5, metrics.activityPaddingY);
    const int iconEdge = qMax(16, metrics.attachmentTileHeight - 10);
    const QFontMetrics nameFm(metrics.bodyFont);
    painter->save();
    painter->setRenderHint(QPainter::Antialiasing, true);
    for (int i = 0; i < tiles.size(); ++i) {
        AttachmentTileLayout tile = tiles.at(i);
        tile.rect.translate(origin);
        const QJsonObject att = atts.at(i).toObject();
        painter->setPen(ThemeManager::palette().chatBorder);
        painter->setBrush(ThemeManager::palette().chatAttachmentSurface);
        painter->drawRoundedRect(tile.rect.adjusted(0, 0, -1, -1), 7, 7);
        const QRect iconR(tile.rect.left() + pad,
                          tile.rect.top() + (tile.rect.height() - iconEdge) / 2,
                          iconEdge, iconEdge);
        if (tile.image) {
            const QPixmap pm = delegate->chipPixmap(att, iconEdge,
                                                      painter->device()->devicePixelRatioF());
            if (!pm.isNull()) {
                drawChipThumbnail(painter, iconR, pm);
            } else {
                painter->setPen(ThemeManager::palette().chatMetadata);
                painter->drawText(iconR, Qt::AlignCenter, tile.glyph);
            }
        } else {
            painter->setPen(ThemeManager::palette().chatMetadata);
            painter->drawText(iconR, Qt::AlignCenter, tile.glyph);
        }
        const int textLeft = iconR.right() + pad + 1;
        const QRect nameR(textLeft, tile.rect.top() + 4,
                          tile.rect.right() - textLeft - pad + 1,
                          qMax(1, tile.rect.height() / 2 - 2));
        const QRect detailR(textLeft, nameR.bottom() + 1, nameR.width(),
                            tile.rect.bottom() - nameR.bottom() - 3);
        painter->setFont(metrics.bodyFont);
        painter->setPen(opt.palette.color(QPalette::Text));
        painter->drawText(nameR, Qt::AlignLeft | Qt::AlignVCenter,
                          nameFm.elidedText(tile.name, Qt::ElideMiddle, nameR.width()));
        painter->setFont(metrics.metadataFont);
        painter->setPen(att.value(QStringLiteral("outside")).toBool()
                            ? opt.palette.color(QPalette::LinkVisited)
                            : ThemeManager::palette().chatMetadata);
        QString detail = tile.detail;
        if (att.value(QStringLiteral("outside")).toBool())
            detail += QStringLiteral(" · ") + i18n("outside workspace");
        painter->drawText(detailR, Qt::AlignLeft | Qt::AlignVCenter,
                          QFontMetrics(metrics.metadataFont).elidedText(
                              detail, Qt::ElideRight, detailR.width()));
    }
    painter->restore();
}

// THE WIDTH TRAP: sizeHint() and paint() must measure at the SAME width, or the
// shared body document is laid out twice per streaming tick and the cached row
// height is subtly wrong.
//
// QAbstractItemView::initViewItemOption() clears option->rect, so the option
// sizeHint() gets carries an EMPTY rect and the old fallback used
// opt.widget->width() — the QListView's own width, which includes the vertical
// scrollbar (~14px with Breeze). paint(), by contrast, gets a real opt.rect
// spanning the VIEWPORT. Measure at W, paint at W-14: bodyDoc()'s width key
// misses on every paint and re-lays the whole accumulated text, exactly the
// double layout the document cache exists to remove.
//
// So: always prefer the viewport width, which is also what AgentPanel's
// settle-time pass hands measureExact(). Falls back to opt.rect (paint's own
// geometry), then the widget, then a constant for a delegate with no view.
int rowMeasureWidth(const QStyleOptionViewItem &opt)
{
    if (const auto *view = qobject_cast<const QAbstractItemView *>(opt.widget)) {
        const int vw = view->viewport()->width();
        if (vw > 0) {
            return vw;
        }
    }
    if (opt.rect.width() > 0) {
        return opt.rect.width();
    }
    return opt.widget && opt.widget->width() > 0 ? opt.widget->width() : 400;
}

bool isPassiveActivity(const TranscriptModel::Item &item)
{
    // Errors deliberately break the quiet stream: their stronger treatment is
    // an attention boundary rather than a continuation of routine activity.
    return item.kind == TranscriptModel::Tool || item.kind == TranscriptModel::Thinking
        || (item.kind == TranscriptModel::Note && item.noteKind != QLatin1String("err"));
}

struct ActivityNeighbours {
    bool previous = false;
    bool next = false;
};

ActivityNeighbours activityNeighbours(const QModelIndex &idx)
{
    const auto *model = qobject_cast<const TranscriptModel *>(idx.model());
    if (!model || idx.row() < 0 || idx.row() >= model->count()
        || !isPassiveActivity(model->itemAt(idx.row()))) {
        return {};
    }
    return {idx.row() > 0 && isPassiveActivity(model->itemAt(idx.row() - 1)),
            idx.row() + 1 < model->count() && isPassiveActivity(model->itemAt(idx.row() + 1))};
}

void paintActivityRail(QPainter *painter, const QRect &row, const TranscriptMetrics &metrics,
                       const ActivityNeighbours &neighbours, const QColor &color)
{
    const int x = row.left() + metrics.outerInsetX;
    const int top = row.top() + (neighbours.previous ? 0 : metrics.activityPaddingY);
    const int bottom = row.bottom() - (neighbours.next ? 0 : metrics.activityPaddingY);
    if (bottom < top) {
        return;
    }
    painter->fillRect(QRect(x, top, metrics.activityRailWidth, bottom - top + 1), color);
}
} // namespace

TranscriptDelegate::TranscriptDelegate(QObject *parent)
    : QStyledItemDelegate(parent)
{
    // No ThemeManager::changed hookup here on purpose: ThemeManager::applyTheme
    // ends in qApp->setPalette(), which Qt delivers to every widget as
    // ApplicationPaletteChange — the view's event filter below catches that, and
    // a system colour-scheme change that never goes through ThemeManager too.
    // Touching ThemeManager::instance() from a constructor would also drag the
    // singleton (and its KColorScheme/config read) into every delegate.
    connect(ChatAppearance::instance(), &ChatAppearance::changed, this, [this] {
        // Do not clear m_heightCache: old heights are exactly the cheap estimate
        // the virtual view needs during its all-row layout pass. The per-entry
        // appearance generation below makes them stale, then AgentPanel's
        // existing settle pass measures only the visible rows exactly.
        m_dirtyResize = true;
        Q_EMIT appearanceChanged();
    });
}

// Watch the view for palette changes. Installed lazily (like bindModel) from the
// first measure/paint, so the delegate needs no wiring from the panel.
// QEvent::ApplicationPaletteChange is delivered to every widget, so filtering the
// view catches both an app-wide theme switch and a palette set on the view alone.
void TranscriptDelegate::watchPalette(const QStyleOptionViewItem &opt) const
{
    auto *w = const_cast<QWidget *>(opt.widget);
    if (!w || m_paletteWatched == w) {
        return;
    }
    if (m_paletteWatched) {
        m_paletteWatched->removeEventFilter(const_cast<TranscriptDelegate *>(this));
    }
    w->installEventFilter(const_cast<TranscriptDelegate *>(this));
    m_paletteWatched = w;
}

bool TranscriptDelegate::eventFilter(QObject *watched, QEvent *event)
{
    if (watched && watched == m_paletteWatched) {
        switch (event->type()) {
        case QEvent::PaletteChange:
        case QEvent::ApplicationPaletteChange:
        case QEvent::StyleChange:
            ++m_paletteGen;
            break;
        default:
            break;
        }
        // Deliberately NOT chained to QStyledItemDelegate::eventFilter: that one
        // assumes every watched QWidget is an open editor and would emit
        // commitData/closeEditor for the VIEW on Tab/Esc/FocusOut.
        return QObject::eventFilter(watched, event);
    }
    return QStyledItemDelegate::eventFilter(watched, event);
}

TranscriptDelegate::~TranscriptDelegate()
{
    for (auto &e : m_docCache) {
        delete e.doc;
    }
    for (auto &e : m_detailCache) {
        delete e.doc;
    }
    for (auto &e : m_resultCache) {
        delete e.doc;
    }
}

// resolveBodyHtml — see the header. Notes highlight like messages (audit F48):
// a note is where every error, rate-limit and compaction line lives, so a find
// that reaches them but cannot show WHERE the hit is only half-works. Whether
// the row matches comes from the model's cached flag, not a fresh scan of the
// row's plain text per paint.
QString TranscriptDelegate::resolveBodyHtml(const QModelIndex &idx) const
{
    const auto kind = TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());
    if (kind != TranscriptModel::Message && kind != TranscriptModel::Note) {
        return idx.data(TranscriptModel::HtmlRole).toString();
    }
    const auto *m = qobject_cast<const TranscriptModel *>(idx.model());
    if (!m || m->findNeedle().isEmpty() || !m->findMatch(idx.row())) {
        return idx.data(TranscriptModel::HtmlRole).toString();
    }
    const bool current = idx.row() == m->findCurrentRow();
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    const auto it = m_highlightCache.constFind(id);
    if (it != m_highlightCache.constEnd() && it->needle == m->findNeedle()
        && it->current == current) {
        return it->html;
    }
    if (m_highlightCache.size() >= kHighlightCacheCap) {
        m_highlightCache.clear();
    }
    const QString html = highlightedHtml(idx.data(TranscriptModel::PlainRole).toString(),
                                         m->findNeedle(), current);
    m_highlightCache.insert(id, HighlightEntry{m->findNeedle(), current, html});
    return html;
}

// bodyDoc hands back the row's laid-out body document from the cache, laying it
// out only when the row's HTML or wrap width actually changed. sizeHint() and
// paint() both go through here: they used to build a QTextDocument each, so one
// 50ms flush tick of a streaming message re-laid the whole accumulated text
// TWICE. The document is owned by the cache — callers must not delete it.
//
// GuardedTextDocument, not QTextDocument: this HTML comes from an assistant
// message, `![x](/home/you/.ssh/id_rsa)` is ordinary markdown, and the row that
// PAINTS it resolves that image through the same loader a QTextBrowser would
// (audit F15). Being widget-less buys nothing — see SafeContent.h.
QTextDocument *TranscriptDelegate::bodyDoc(const QModelIndex &idx, int contentWidth,
                                           const QStyleOptionViewItem &opt) const
{
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    const QString html = resolveBodyHtml(idx);
    const int appearanceGen = ChatAppearance::instance()->generation();
    const qreal dpr = opt.widget ? opt.widget->devicePixelRatioF() : 1.0;
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, rowMeasureWidth(opt), dpr);
    watchPalette(opt);
    auto it = m_docCache.find(id);
    if (it != m_docCache.end() && it->doc) {
        if (it->width == contentWidth && it->html == html && it->font == metrics.bodyFont
            && it->paletteGen == m_paletteGen && it->appearanceGen == appearanceGen) {
            return it->doc; // already laid out for exactly this
        }
        // Same row, new content / width / theme: re-set this document rather than
        // allocating another (QTextDocument reuses its internal structures).
        configureTranscriptDocument(it->doc, metrics, opt.palette, contentWidth, html);
        it->width = contentWidth;
        it->html = html;
        it->font = metrics.bodyFont;
        it->paletteGen = m_paletteGen;
        it->appearanceGen = appearanceGen;
        return it->doc;
    }
    if (m_docCache.size() >= kDocCacheCap) {
        for (auto &e : m_docCache) {
            delete e.doc;
        }
        m_docCache.clear();
    }
    auto *doc = new agentkate::GuardedTextDocument;
    configureTranscriptDocument(doc, metrics, opt.palette, contentWidth, html);
    m_docCache.insert(id, DocEntry{contentWidth, html, metrics.bodyFont, m_paletteGen,
                                   appearanceGen, doc});
    return doc;
}

// toolDoc is bodyDoc for the expanded tool row's two mono documents: same cache
// shape, same invalidation, but the content is PLAIN text (an input JSON blob, a
// command's output) so it is set with setPlainText and needs no palette
// generation in the key. Still a GuardedTextDocument: setPlainText cannot make
// an <img> today, but "the delegate builds no unguarded document" is a property
// worth keeping true by construction rather than by reading this function.
QTextDocument *TranscriptDelegate::toolDoc(const QModelIndex &idx, ToolSlot slot,
                                           const QString &plain, const QFont &mono,
                                           int contentWidth) const
{
    QHash<quintptr, DocEntry> &cache =
        slot == ToolSlot::Detail ? m_detailCache : m_resultCache;
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    const int appearanceGen = ChatAppearance::instance()->generation();
    const auto configure = [&](QTextDocument *doc) {
        doc->setDefaultFont(mono);
        doc->setDocumentMargin(0);
        doc->setPlainText(plain);
        doc->setTextWidth(qMax(1, contentWidth));
    };
    auto it = cache.find(id);
    if (it != cache.end() && it->doc) {
        if (it->width == contentWidth && it->html == plain && it->font == mono
            && it->appearanceGen == appearanceGen) {
            return it->doc; // already laid out for exactly this
        }
        configure(it->doc);
        it->width = contentWidth;
        it->html = plain;
        it->font = mono;
        it->appearanceGen = appearanceGen;
        return it->doc;
    }
    if (cache.size() >= kDocCacheCap) {
        for (auto &e : cache) {
            delete e.doc;
        }
        cache.clear();
    }
    auto *doc = new agentkate::GuardedTextDocument;
    configure(doc);
    cache.insert(id, DocEntry{contentWidth, plain, mono, 0, appearanceGen, doc});
    return doc;
}

void TranscriptDelegate::invalidateRow(quintptr stableId) const
{
    m_heightCache.remove(stableId);
    m_highlightCache.remove(stableId);
    for (QHash<quintptr, DocEntry> *cache : {&m_docCache, &m_detailCache, &m_resultCache}) {
        const auto it = cache->constFind(stableId);
        if (it != cache->constEnd()) {
            delete it->doc;
            cache->erase(it);
        }
    }
}

void TranscriptDelegate::bindModel(const QModelIndex &idx) const
{
    const auto *model = qobject_cast<const TranscriptModel *>(idx.model());
    if (!model) {
        return;
    }
    // UniqueConnection makes this idempotent, so it can run on every measure
    // without bookkeeping; the connection dies with either object.
    connect(model, &TranscriptModel::heightInvalidated, this,
            &TranscriptDelegate::invalidateRow, Qt::UniqueConnection);
}

TranscriptDelegate::ResolvedAttachment
TranscriptDelegate::resolveAttachmentCached(const QJsonObject &att) const
{
    // The chip's identity is the pair of paths it was recorded with; the
    // resolution between them is what costs syscalls.
    const QString key = att.value(QStringLiteral("path")).toString()
                        + QLatin1Char('\x1f')
                        + att.value(QStringLiteral("cachePath")).toString();
    return attEntry(key, att).r;
}

// The cache entry behind resolveAttachmentCached, re-stat'ed when its TTL has
// run out. Returned by reference so the thumbnail can be memoised INTO it (see
// chipPixmap); the reference is used before any further insertion, so a rehash
// cannot invalidate it.
TranscriptDelegate::AttEntry &TranscriptDelegate::attEntry(const QString &key,
                                                          const QJsonObject &att) const
{
    const qint64 now = QDateTime::currentMSecsSinceEpoch();
    const auto hit = m_attCache.find(key);
    if (hit != m_attCache.end() && now - hit->checkedMs < kAttStatTtlMs) {
        return *hit;
    }
    AttEntry entry;
    entry.checkedMs = now;
    entry.r.path = agentkate::resolveAttachmentPath(att);
    if (!entry.r.path.isEmpty()) {
        const QFileInfo fi(entry.r.path);
        if (fi.isFile()) {
            entry.r.size = fi.size();
            entry.r.mtime = fi.lastModified().toMSecsSinceEpoch();
        } else {
            entry.r.path.clear();
        }
    }
    if (m_attCache.size() >= kAttCacheCap) {
        m_attCache.clear();
    }
    // A re-stat drops the memoised thumbnail with it: the file may be the next
    // screenshot written over the same name.
    return *m_attCache.insert(key, entry);
}

// chipPixmap is chipThumbnail memoised in the attachment entry. chipThumbnail
// itself is cheap only in the QPixmapCache-hit case, and that hit costs ~6
// QString allocations to build the key plus a global-mutex lookup — per image
// chip, per paint, per scroll tick (audit F18). Here a still transcript pays
// one hash lookup.
QPixmap TranscriptDelegate::chipPixmap(const QJsonObject &att, int edge, qreal dpr) const
{
    const QString key = att.value(QStringLiteral("path")).toString()
                        + QLatin1Char('\x1f')
                        + att.value(QStringLiteral("cachePath")).toString();
    AttEntry &e = attEntry(key, att);
    if (e.thumbValid && e.thumbEdge == edge && qFuzzyCompare(e.thumbDpr, dpr)) {
        return e.thumb;
    }
    e.thumb = chipThumbnail(e.r, edge, dpr);
    e.thumbEdge = edge;
    e.thumbDpr = dpr;
    e.thumbValid = true;
    return e.thumb;
}

// Measure / paint share this: returns the total row height for `width` and, when
// `painter` is non-null, draws the row into `rowRect`. Keeping one routine for
// both guarantees the height cache and the painting never disagree.
namespace {
int layoutRow(const QModelIndex &idx, int width, const QStyleOptionViewItem &opt,
              QPainter *painter, const QRect &rowRect, const TranscriptDelegate *self)
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

    const qreal dpr = painter ? painter->device()->devicePixelRatioF()
                              : (opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, width, dpr);
    const int contentLeft = metrics.outerInsetX;
    const int contentWidth = width - 2 * metrics.outerInsetX;
    if (contentWidth <= 0) {
        return lineH;
    }

    if (kind == TranscriptModel::Note) {
        // Notes carry a live timestamp like message cards do: an error or a
        // rate-limit line the user reads ten minutes later says nothing about
        // WHEN it happened otherwise (audit F50). Its column is reserved out of
        // the note's own wrap width so the two never overlap. Replayed notes
        // have no timestamp — and get no reserved column.
        const QString ts = idx.data(TranscriptModel::TimestampRole).toString();
        const bool error = idx.data(TranscriptModel::NoteKindRole).toString()
            == QLatin1String("err");
        const ActivityNeighbours neighbours = activityNeighbours(idx);
        QFont small = metrics.metadataFont;
        const int tsW =
            ts.isEmpty() ? 0 : QFontMetrics(small).horizontalAdvance(ts) + metrics.activityPaddingX;
        const int textW = contentWidth - 2 * metrics.activityPaddingX - tsW
            - metrics.activityRailWidth - metrics.activityPaddingX;
        QTextDocument *doc = self->bodyDoc(idx, qMax(1, textW), opt);
        const int h = int(doc->size().height()) + 2 * metrics.activityPaddingY;
        if (painter) {
            const QRect surface(rowRect.left() + contentLeft + metrics.activityRailWidth
                                    + metrics.activityPaddingX,
                                rowRect.top(),
                                qMax(1, contentWidth - metrics.activityRailWidth
                                           - metrics.activityPaddingX), h);
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            QColor background = error ? ThemeManager::palette().negative
                                      : ThemeManager::palette().chatActivitySurface;
            background.setAlpha(error ? 48 : 255);
            painter->setPen(error ? ThemeManager::palette().negative : Qt::NoPen);
            painter->setBrush(background);
            painter->drawRoundedRect(surface.adjusted(0, 0, -1, -1), 6, 6);
            if (!error)
                paintActivityRail(painter, rowRect, metrics, neighbours,
                                  ThemeManager::palette().chatRail);
            painter->translate(surface.left() + metrics.activityPaddingX,
                               surface.top() + metrics.activityPaddingY);
            QAbstractTextDocumentLayout::PaintContext ctx;
            ctx.palette = opt.palette;
            ctx.palette.setColor(QPalette::Text,
                                 noteColor(idx.data(TranscriptModel::NoteKindRole).toString(),
                                           dark));
            doc->documentLayout()->draw(painter, ctx);
            painter->restore();
            if (tsW > 0) {
                painter->save();
                painter->setFont(small);
                painter->setPen(ThemeManager::palette().chatMetadata);
                painter->drawText(QRect(surface.left() + metrics.activityPaddingX + textW,
                                        surface.top() + metrics.activityPaddingY, tsW, lineH),
                                  Qt::AlignRight | Qt::AlignVCenter, ts);
                painter->restore();
            }
        }
        return h;
    }

    if (kind == TranscriptModel::Message) {
        const auto speaker = TranscriptModel::Speaker(
            idx.data(TranscriptModel::SpeakerRole).toInt());
        const auto position = TranscriptModel::MessageRunPosition(
            idx.data(TranscriptModel::MessageRunPositionRole).toInt());
        const bool showHeader = position == TranscriptModel::MessageRunPosition::Single
            || position == TranscriptModel::MessageRunPosition::First;
        const int bubbleW = qMax(1, speaker == TranscriptModel::Speaker::User
            ? metrics.userMaxWidth : metrics.assistantMaxWidth);
        const int innerW = qMax(1, bubbleW - 2 * metrics.messagePaddingX);
        QTextDocument *doc = self->bodyDoc(idx, qMax(1, innerW), opt);
        const int bodyH = int(doc->size().height());
        // Attachment chip block under the body (You messages with attachments).
        const QJsonArray atts =
            idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
        int chipsH = 0;
        // Laid out once per row pass, at the origin-relative coordinates, and
        // translated when painting — this used to run twice per paint (once to
        // measure, once to place), rebuilding every elided label string (audit
        // F18).
        QList<AttachmentTileLayout> tiles;
        if (!atts.isEmpty()) {
            tiles = layoutAttachmentTiles(atts, metrics, QPoint(0, 0), innerW, chipsH);
        }
        const int chipsBlock = chipsH > 0 ? metrics.attachmentGap + chipsH : 0;
        const int headerH = showHeader ? metrics.messageHeaderHeight
                                       + metrics.messageHeaderGap : 0;
        const int bubbleH = metrics.messagePaddingY + headerH + bodyH + chipsBlock
                            + metrics.messagePaddingY;
        const int gap = position == TranscriptModel::MessageRunPosition::Middle
                || position == TranscriptModel::MessageRunPosition::First
            ? metrics.groupedMessageGap : metrics.messageGap;
        const int total = bubbleH + gap;
        if (painter) {
            const QRect card = self->messageBubbleRect(rowRect, opt, idx);
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            painter->setPen(Qt::NoPen);
            painter->setBrush(speaker == TranscriptModel::Speaker::User
                                  ? ThemeManager::palette().chatUserSurface
                                  : ThemeManager::palette().chatAssistantSurface);
            painter->drawRoundedRect(card, metrics.bubbleRadius, metrics.bubbleRadius);
            painter->restore();

            const QRect bodyRect = self->messageBodyRect(rowRect, opt, idx);
            if (showHeader) {
                const QRect header = self->messageHeaderRect(rowRect, opt, idx);
                painter->save();
                QFont label = metrics.metadataFont;
                label.setBold(true);
                painter->setFont(label);
                painter->setPen(speaker == TranscriptModel::Speaker::User
                                    ? opt.palette.color(QPalette::Link)
                                    : ThemeManager::palette().chatMetadata);
                painter->drawText(header, Qt::AlignLeft | Qt::AlignVCenter,
                                  idx.data(TranscriptModel::RoleTextRole).toString());
                painter->restore();
            }
            if (showHeader && !idx.data(TranscriptModel::ReplayedRole).toBool()) {
                const QString ts = idx.data(TranscriptModel::TimestampRole).toString();
                painter->save();
                painter->setFont(metrics.metadataFont);
                painter->setPen(ThemeManager::palette().chatMetadata);
                painter->drawText(self->messageHeaderRect(rowRect, opt, idx),
                                  Qt::AlignRight | Qt::AlignVCenter, ts);
                painter->restore();
            }

            // Body HTML.
            painter->save();
            painter->translate(bodyRect.left(), bodyRect.top());
            QAbstractTextDocumentLayout::PaintContext ctx;
            ctx.palette = opt.palette;
            doc->documentLayout()->draw(painter, ctx);
            painter->restore();

            // Typed attachment tiles are laid out in the coordinate space of the
            // card so their paint and click targets remain identical.
            if (chipsH > 0) {
                const QPoint origin(bodyRect.left(), bodyRect.bottom() + 1
                                                    + metrics.attachmentGap);
                paintAttachmentTiles(painter, self, atts, tiles, origin, opt, metrics);
            }
        }
        return total;
    }

    if (kind == TranscriptModel::Thinking) {
        // Reasoning is passive activity, not a second chat speaker. Its rail
        // joins adjacent tools/notes while the disclosure keeps the body on
        // demand.
        const bool expanded = idx.data(TranscriptModel::ToolExpandedRole).toBool();
        const ActivityNeighbours neighbours = activityNeighbours(idx);
        int total = metrics.activityHeaderHeight;
        QTextDocument *thinkDoc = nullptr;
        int bodyH = 0;
        const int bodyW = contentWidth - metrics.activityRailWidth
            - metrics.activityPaddingX - kDetailPadX;
        if (expanded) {
            thinkDoc = self->bodyDoc(idx, qMax(1, bodyW), opt);
            bodyH = int(thinkDoc->size().height());
            total += kToolPad + bodyH + kToolPad;
        }
        if (painter) {
            const QString arrow = expanded ? QStringLiteral("▾") : QStringLiteral("▸");
            const QString preview = idx.data(TranscriptModel::ToolSummaryRole).toString();
            const QString header = QStringLiteral("%1  %2  —  %3")
                                       .arg(arrow, i18n("Thinking"), preview);
            const QRect surface(rowRect.left() + contentLeft + metrics.activityRailWidth + 8,
                                rowRect.top(), contentWidth - metrics.activityRailWidth - 8,
                                metrics.activityHeaderHeight);
            const QRect hdr(surface.left() + metrics.activityPaddingX, surface.top(),
                            surface.width() - 2 * metrics.activityPaddingX, surface.height());
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            painter->setPen(Qt::NoPen);
            painter->setBrush(ThemeManager::palette().chatActivitySurface);
            painter->drawRoundedRect(surface.adjusted(0, 0, -1, -1), 6, 6);
            paintActivityRail(painter, rowRect, metrics, neighbours,
                              ThemeManager::palette().chatRail);
            painter->setFont(metrics.metadataFont);
            painter->setPen(ThemeManager::palette().chatMetadata);
            painter->drawText(hdr, Qt::AlignLeft | Qt::AlignVCenter,
                              QFontMetrics(metrics.metadataFont).elidedText(
                                  header, Qt::ElideRight, hdr.width()));
            painter->restore();
            if (thinkDoc) {
                painter->save();
                painter->translate(rowRect.left() + contentLeft + metrics.activityRailWidth
                                       + metrics.activityPaddingX,
                                   rowRect.top() + metrics.activityHeaderHeight + kToolPad);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                ctx.palette.setColor(QPalette::Text, ThemeManager::palette().chatMetadata);
                thinkDoc->documentLayout()->draw(painter, ctx);
                painter->restore();
            }
        }
        return total;
    }

    if (kind == TranscriptModel::Checklist) {
        // The agent's plan: a bordered card listing every item with a status
        // glyph. Always fully visible — the plan is the thing to watch.
        const QJsonArray items = idx.data(TranscriptModel::ChecklistRole).toJsonArray();
        const int itemH = QFontMetrics(metrics.bodyFont).height() + metrics.activityGap + 3;
        const int total = metrics.activityHeaderHeight + items.size() * itemH + kToolPad;
        if (painter) {
            const QRect card(rowRect.left() + contentLeft, rowRect.top(),
                             contentWidth, total);
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            painter->setPen(ThemeManager::palette().chatBorder);
            painter->setBrush(ThemeManager::palette().chatActivitySurface);
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
            QFont bold = metrics.bodyFont;
            bold.setBold(true);
            painter->setFont(bold);
            painter->drawText(QRect(card.left() + 8, card.top(), card.width() - 16,
                                    metrics.activityHeaderHeight),
                              Qt::AlignLeft | Qt::AlignVCenter,
                              QFontMetrics(bold).elidedText(header, Qt::ElideRight,
                                                            card.width() - 16));
            painter->restore();

            int y = card.top() + metrics.activityHeaderHeight;
            painter->save();
            for (const QJsonValue &v : items) {
                const QJsonObject item = v.toObject();
                const QString status =
                    item.value(QStringLiteral("status")).toString();
                const QString text = item.value(QStringLiteral("content")).toString();
                QString glyph = QStringLiteral("☐"); // ☐ pending
                QFont f = metrics.bodyFont;
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

    int total = metrics.activityHeaderHeight;
    // Image chips (a tool_result carrying image blocks, e.g. a screenshot)
    // sit directly under the header in both states — the image usually IS the
    // result, so it reads first; the expanded detail shifts below it.
    const QJsonArray toolAtts =
        idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    int toolChipsH = 0;
    QList<AttachmentTileLayout> toolTiles; // laid out once; translated at paint time
    if (!toolAtts.isEmpty()) {
        toolTiles = layoutAttachmentTiles(toolAtts, metrics, QPoint(0, 0),
                                          qMax(1, contentWidth - 2 * kDetailPadX), toolChipsH);
        total += toolChipsH + kToolPad;
    }
    // Detail (input JSON + result) measured with the mono document only when open.
    QTextDocument *detailDoc = nullptr;
    QTextDocument *resultDoc = nullptr;
    int detailH = 0;
    int resultH = 0;
    int extraH = 0; // "show full output" link line
    const int detailW = contentWidth - 2 * kDetailPadX;
    const int detailTextW = qMax(1, detailW - 2 * kToolPad);
    if (expanded) {
        QFont mono = opt.font;
        mono.setFamily(QStringLiteral("monospace"));
        mono.setPointSizeF(opt.font.pointSizeF() * 0.9);
        // Cached per row (audit F18): measure and paint now share one layout,
        // and a repaint of an unchanged row costs a hash lookup instead of two
        // fresh QTextDocuments over up to the whole output.
        const QString detail = idx.data(TranscriptModel::ToolDetailRole).toString();
        if (!detail.isEmpty()) {
            detailDoc = self->toolDoc(idx, TranscriptDelegate::ToolSlot::Detail, detail,
                                      mono, detailTextW);
            detailH = int(detailDoc->size().height());
        }
        QString result = idx.data(TranscriptModel::ToolResultRole).toString();
        // A RUNNING row shows whatever partial text it has — a helper's
        // forwarded output under a Task row, live command output. Measuring
        // and painting this only `if (done)` meant the whole live-subagent
        // pipeline (buffered, bounded, coalesced, repainted) was never visible
        // and the row sat at "⋯" for the entire run (audit F39). Only the
        // "(no output)" placeholder is done-only: a running tool with nothing
        // yet has produced no output, it has not produced none.
        if (done && result.isEmpty()) {
            result = QStringLiteral("(no output)");
        }
        if (!result.isEmpty()) {
            // The cache key carries the text, so streaming partial output
            // re-lays this row's own document rather than defeating the cache
            // or serving a stale height (audit F18).
            resultDoc = self->toolDoc(idx, TranscriptDelegate::ToolSlot::Result, result,
                                      mono, detailTextW);
            resultH = int(resultDoc->size().height());
        }
        if (idx.data(TranscriptModel::ToolTruncatedRole).toBool()) {
            extraH = lineH + kToolPad;
        }
        const int detailBlock = detailH > 0 ? detailH + 2 * kToolPad : 0;
        const int resultBlock = resultH > 0 ? resultH + 2 * kToolPad : 0;
        total += kToolPad + detailBlock + (detailBlock && resultBlock ? kToolPad : 0)
               + resultBlock + extraH + kToolPad;
    }

    if (painter) {
        // Tools are an activity stream: a slim status rail, sentence-like
        // primary action and quiet surface. An expanded tool earns a card for
        // its code/result detail; a failure remains an explicit, high-contrast
        // boundary instead of relying on its red mark alone.
        const bool failed = idx.data(TranscriptModel::ToolErrorRole).toBool();
        const ActivityNeighbours neighbours = activityNeighbours(idx);
        const QColor errColor = ThemeManager::palette().negative;
        const QRect card(rowRect.left() + contentLeft, rowRect.top(),
                         contentWidth, total);
        const QRect surface(card.left() + metrics.activityRailWidth + 8, card.top(),
                            card.width() - metrics.activityRailWidth - 8,
                            expanded ? card.height() : metrics.activityHeaderHeight);
        painter->save();
        painter->setRenderHint(QPainter::Antialiasing, true);
        painter->setPen(expanded ? (failed ? errColor : ThemeManager::palette().chatBorder)
                                 : Qt::NoPen);
        painter->setBrush(ThemeManager::palette().chatActivitySurface);
        painter->drawRoundedRect(surface.adjusted(0, 0, -1, -1), 7, 7);
        paintActivityRail(painter, rowRect, metrics, neighbours,
                          failed ? errColor : ThemeManager::palette().chatRail);
        painter->restore();

        // Header line: arrow + mark + kind glyph + name + summary, with a copy
        // glyph right. Cooperation calls (agent-to-agent coordination and
        // orchestration) carry ⇄ instead of the 🔧 file/shell wrench, so a
        // controller's traffic stands out from its own tool use at a glance.
        // The arrow+mark prefix is drawn separately so the failure mark can
        // take the negative colour while the rest of the line stays readable.
        const QString arrow = expanded ? QStringLiteral("▾") : QStringLiteral("▸");
        const QString mark = !done ? QStringLiteral("⋯")
                                   : (failed ? QStringLiteral("✗") : QStringLiteral("✓"));
        const QString kind = name.startsWith(QLatin1String("mcp__cooperation__"))
                                 ? QStringLiteral("⇄")
                                 : QStringLiteral("\U0001f527");
        const QString prefix = QStringLiteral("%1  %2  ").arg(arrow, mark);
        const QString state = !done ? i18n("Running")
            : failed ? i18n("Failed") : i18n("Done");
        QString header = summary.isEmpty()
            ? QStringLiteral("%1  ·  %2").arg(name, state)
            : QStringLiteral("%1  —  %2  ·  %3").arg(name, summary, state);
        // A RUNNING row carrying partial output shows its latest line right in
        // the header. Tool rows are collapsed by default, so painting live text
        // only inside the expanded body would still leave a subagent's whole
        // run reading "⋯" for anyone who did not open the row (audit F39).
        if (!done) {
            const QString partial = idx.data(TranscriptModel::ToolResultRole).toString();
            const QString tail =
                partial.section(QLatin1Char('\n'), -1, -1).simplified();
            if (!tail.isEmpty()) {
                header += QStringLiteral("   · ") + tail;
            }
        }
        const QRect hdr(surface.left() + 8, surface.top(),
                        surface.width() - 8 - kToolCopyW - kToolInspectW,
                        metrics.activityHeaderHeight);
        painter->save();
        const int prefixW = fm.horizontalAdvance(prefix);
        painter->setPen(failed ? errColor : opt.palette.color(QPalette::WindowText));
        painter->drawText(hdr, Qt::AlignLeft | Qt::AlignVCenter, prefix);
        const QRect restR(hdr.left() + prefixW, hdr.top(),
                          qMax(0, hdr.width() - prefixW), hdr.height());
        painter->setPen(opt.palette.color(QPalette::WindowText));
        painter->drawText(restR, Qt::AlignLeft | Qt::AlignVCenter,
                          fm.elidedText(header, Qt::ElideRight, restR.width()));
        painter->setPen(opt.palette.color(QPalette::Mid));
        // "Open in inspector" glyph, then the copy glyph on the far right.
        const QRect inspectR(surface.right() - kToolCopyW - kToolInspectW + 1, card.top(),
                             kToolInspectW, metrics.activityHeaderHeight);
        painter->drawText(inspectR, Qt::AlignCenter, QStringLiteral("⤢"));
        const QRect copyR(surface.right() - kToolCopyW + 1, card.top(), kToolCopyW,
                          metrics.activityHeaderHeight);
        painter->drawText(copyR, Qt::AlignCenter, QStringLiteral("⧉"));
        painter->restore();

        if (toolChipsH > 0) {
            const QPoint origin(card.left() + kDetailPadX,
                                card.top() + metrics.activityHeaderHeight);
            paintAttachmentTiles(painter, self, toolAtts, toolTiles, origin, opt, metrics);
        }

        if (expanded) {
            int y = surface.top() + metrics.activityHeaderHeight
                    + (toolChipsH > 0 ? toolChipsH + kToolPad : 0) + kToolPad;
            QFont mono = opt.font;
            mono.setFamily(QStringLiteral("monospace"));
            painter->save();
            painter->setPen(opt.palette.color(QPalette::WindowText));
            if (detailDoc) {
                painter->setPen(Qt::NoPen);
                painter->setBrush(ThemeManager::palette().chatCodeSurface);
                painter->drawRoundedRect(QRect(card.left() + kDetailPadX, y, detailW,
                                               detailH + 2 * kToolPad), 5, 5);
                painter->save();
                painter->translate(card.left() + kDetailPadX + kToolPad, y + kToolPad);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                detailDoc->documentLayout()->draw(painter, ctx);
                painter->restore();
                y += detailH + 2 * kToolPad;
            }
            if (resultDoc) {
                if (detailDoc) {
                    y += kToolPad;
                }
                painter->setPen(Qt::NoPen);
                painter->setBrush(ThemeManager::palette().chatCodeSurface);
                painter->drawRoundedRect(QRect(card.left() + kDetailPadX, y, detailW,
                                               resultH + 2 * kToolPad), 5, 5);
                painter->save();
                painter->translate(card.left() + kDetailPadX + kToolPad, y + kToolPad);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                if (!done) {
                    // Provisional output of a still-running tool: dimmed so it
                    // is legible as "so far", not as the result (audit F39).
                    ctx.palette.setColor(QPalette::Text,
                                         opt.palette.color(QPalette::Mid));
                }
                resultDoc->documentLayout()->draw(painter, ctx);
                painter->restore();
                y += resultH + 2 * kToolPad;
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

    // detailDoc/resultDoc are owned by the delegate's cache — nothing to free.
    return total;
}
} // namespace

QSize TranscriptDelegate::sizeHint(const QStyleOptionViewItem &opt,
                                   const QModelIndex &idx) const
{
    // Same width source as paint() — see rowMeasureWidth's "WIDTH TRAP" note.
    const int width = rowMeasureWidth(opt);
    watchPalette(opt);
    // Cheap and idempotent: the model's per-row invalidation is what keeps this
    // cache honest, and this is the first place we see the model.
    bindModel(idx);
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    const int appearanceGen = ChatAppearance::instance()->generation();
    auto cit = m_heightCache.constFind(id);
    if (cit != m_heightCache.constEnd()) {
        if (cit->width == width && cit->appearanceGen == appearanceGen) {
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
    const int h = layoutRow(idx, width, opt, nullptr, QRect(), this);
    if (m_heightCache.size() >= kHeightCacheCap) {
        m_heightCache.clear();
    }
    m_heightCache.insert(id, CacheEntry{width, h, appearanceGen});
    return QSize(width, h);
}

int TranscriptDelegate::measureExact(const QModelIndex &idx, int width,
                                     const QStyleOptionViewItem &opt) const
{
    const int h = layoutRow(idx, width, opt, nullptr, QRect(), this);
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    if (m_heightCache.size() >= kHeightCacheCap) {
        m_heightCache.clear();
    }
    m_heightCache.insert(id, CacheEntry{width, h, ChatAppearance::instance()->generation()});
    return h;
}

void TranscriptDelegate::paint(QPainter *painter, const QStyleOptionViewItem &opt,
                               const QModelIndex &idx) const
{
    painter->save();
    painter->setClipRect(opt.rect);
    layoutRow(idx, opt.rect.width(), opt, painter, opt.rect, this);
    painter->restore();
}

QRect TranscriptDelegate::toolHeaderRect(const QRect &row,
                                         const QStyleOptionViewItem &opt) const
{
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    return QRect(row.left() + metrics.outerInsetX + metrics.activityRailWidth + 16, row.top(),
                 row.width() - 2 * metrics.outerInsetX - metrics.activityRailWidth - 16
                     - kToolCopyW - kToolInspectW,
                 metrics.activityHeaderHeight);
}

QRect TranscriptDelegate::toolCopyRect(const QRect &row,
                                       const QStyleOptionViewItem &opt) const
{
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const int right = row.right() - metrics.outerInsetX;
    return QRect(right - kToolCopyW, row.top(), kToolCopyW, metrics.activityHeaderHeight);
}

QRect TranscriptDelegate::toolInspectRect(const QRect &row,
                                          const QStyleOptionViewItem &opt) const
{
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const int right = row.right() - metrics.outerInsetX;
    return QRect(right - kToolCopyW - kToolInspectW, row.top(), kToolInspectW,
                 metrics.activityHeaderHeight);
}

int TranscriptDelegate::attachmentsBlockHeight(const QModelIndex &idx,
                                               const QStyleOptionViewItem &opt,
                                               int innerW) const
{
    const QJsonArray atts = idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    if (atts.isEmpty()) {
        return 0;
    }
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, rowMeasureWidth(opt),
        opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    int chipsH = 0;
    layoutAttachmentTiles(atts, metrics, QPoint(0, 0), qMax(1, innerW), chipsH);
    return chipsH > 0 ? metrics.attachmentGap + chipsH : 0;
}

QRect TranscriptDelegate::messageBubbleRect(const QRect &row,
                                             const QStyleOptionViewItem &opt,
                                             const QModelIndex &idx) const
{
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const auto speaker = TranscriptModel::Speaker(
        idx.data(TranscriptModel::SpeakerRole).toInt());
    const auto position = TranscriptModel::MessageRunPosition(
        idx.data(TranscriptModel::MessageRunPositionRole).toInt());
    const int gap = position == TranscriptModel::MessageRunPosition::First
            || position == TranscriptModel::MessageRunPosition::Middle
        ? metrics.groupedMessageGap : metrics.messageGap;
    const int bubbleW = qMax(1, speaker == TranscriptModel::Speaker::User
        ? metrics.userMaxWidth : metrics.assistantMaxWidth);
    const int left = speaker == TranscriptModel::Speaker::User
        ? row.right() - metrics.outerInsetX - bubbleW + 1 : row.left() + metrics.outerInsetX;
    return QRect(left, row.top(), bubbleW, qMax(0, row.height() - gap));
}

QRect TranscriptDelegate::messageHeaderRect(const QRect &row,
                                             const QStyleOptionViewItem &opt,
                                             const QModelIndex &idx) const
{
    const auto position = TranscriptModel::MessageRunPosition(
        idx.data(TranscriptModel::MessageRunPositionRole).toInt());
    if (position != TranscriptModel::MessageRunPosition::Single
        && position != TranscriptModel::MessageRunPosition::First) {
        return {};
    }
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const QRect bubble = messageBubbleRect(row, opt, idx);
    return QRect(bubble.left() + metrics.messagePaddingX,
                 bubble.top() + metrics.messagePaddingY,
                 qMax(1, bubble.width() - 2 * metrics.messagePaddingX),
                 metrics.messageHeaderHeight);
}

QRect TranscriptDelegate::messageBodyRect(const QRect &row,
                                          const QStyleOptionViewItem &opt,
                                          const QModelIndex &idx) const
{
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const QRect bubble = messageBubbleRect(row, opt, idx);
    const QRect header = messageHeaderRect(row, opt, idx);
    const int innerW = bubble.width() - 2 * metrics.messagePaddingX;
    const int chipsBlock = attachmentsBlockHeight(idx, opt, innerW);
    const int headerH = header.isEmpty() ? 0 : metrics.messageHeaderHeight
                                             + metrics.messageHeaderGap;
    const int bodyH = bubble.height() - 2 * metrics.messagePaddingY - headerH - chipsBlock;
    return QRect(bubble.left() + metrics.messagePaddingX,
                 bubble.top() + metrics.messagePaddingY + headerH,
                 qMax(1, innerW), qMax(0, bodyH));
}

QList<QRect> TranscriptDelegate::attachmentRects(const QRect &row,
                                                  const QStyleOptionViewItem &opt,
                                                  const QModelIndex &idx) const
{
    const QJsonArray atts = idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    if (atts.isEmpty()) {
        return {};
    }
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    const QRect body = messageBodyRect(row, opt, idx);
    int ignored = 0;
    const QList<AttachmentTileLayout> tiles = layoutAttachmentTiles(
        atts, metrics, QPoint(body.left(), body.bottom() + 1 + metrics.attachmentGap),
        body.width(), ignored);
    QList<QRect> rects;
    rects.reserve(tiles.size());
    for (const AttachmentTileLayout &tile : tiles) {
        rects.append(tile.rect);
    }
    return rects;
}

QList<QRect> TranscriptDelegate::toolAttachmentRects(const QRect &row,
                                                      const QStyleOptionViewItem &opt,
                                                      const QModelIndex &idx) const
{
    const QJsonArray atts = idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    if (atts.isEmpty()) {
        return {};
    }
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        opt.font, opt.palette, row.width(), opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
    int ignored = 0;
    const QList<AttachmentTileLayout> tiles = layoutAttachmentTiles(
        atts, metrics,
        QPoint(row.left() + metrics.outerInsetX + kDetailPadX,
               row.top() + metrics.activityHeaderHeight),
        qMax(1, row.width() - 2 * metrics.outerInsetX - 2 * kDetailPadX), ignored);
    QList<QRect> rects;
    rects.reserve(tiles.size());
    for (const AttachmentTileLayout &tile : tiles) {
        rects.append(tile.rect);
    }
    return rects;
}

// The tile a point falls in, or -1. Recomputes the tile layout in the card's
// coordinate space (matching paint) and tests each rect.
int TranscriptDelegate::attachmentChipAt(const QRect &row,
                                         const QStyleOptionViewItem &opt,
                                         const QModelIndex &idx, const QPoint &pos) const
{
    const QJsonArray atts = idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    if (atts.isEmpty()) {
        return -1;
    }
    const QList<QRect> laid = attachmentRects(row, opt, idx);
    for (int i = 0; i < laid.size(); ++i) {
        if (laid.at(i).contains(pos)) {
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
    // GuardedTextBrowser, not a bare QTextBrowser: this document holds the same
    // model-authored HTML the row paints, and a QTextBrowser resolves image
    // names through an unbounded synchronous QFile read of ANY local path
    // (audit F15).
    //
    // This comment used to claim the PAINTED rows were safe because their
    // documents are parentless. They were not: a parentless QTextDocument reads
    // arbitrary local paths too (probed — `![x](file:///…/secret.png)` in an
    // assistant message rendered the file), so bodyDoc/toolDoc build
    // GuardedTextDocuments and the guard covers every document in this class,
    // not just this one.
    auto *browser = new agentkate::GuardedTextBrowser(parent);
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
    // Match paint()'s document setup exactly (see configureTranscriptDocument).
    // is a child of the view's viewport, so it inherits the same font paint()
    // draws with (opt.font). The wrap width is re-applied by updateEditorGeometry
    // once the geometry is known.
    const QPalette palette = browser->palette();
    const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
        browser->font(), palette, browser->viewport()->width(), browser->devicePixelRatioF());
    configureTranscriptDocument(browser->document(), metrics, palette,
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
        // Reapply the same appearance-aware setup paint uses. This covers an
        // overlay opened immediately after a density/theme update as well as a
        // width change, without ever letting its document retain stale glyph
        // metrics behind the painted row.
        const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
            opt.font, opt.palette, opt.rect.width(), browser->devicePixelRatioF());
        configureTranscriptDocument(browser->document(), metrics, opt.palette,
                                    body.width(), resolveBodyHtml(idx));
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
            // An image chip (screenshot result) opens its preview.
            const QJsonArray toolAtts =
                idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
            if (!toolAtts.isEmpty()) {
                const QList<QRect> laid = toolAttachmentRects(opt.rect, opt, idx);
                for (int i = 0; i < laid.size(); ++i) {
                    if (laid.at(i).contains(pos)) {
                        emit attachmentActivated(toolAtts.at(i).toObject());
                        return true;
                    }
                }
            }
            // "Open in inspector" glyph — opens the full tool-call modal.
            if (toolInspectRect(opt.rect, opt).contains(pos)) {
                emit inspectToolRequested(idx);
                return true;
            }
            // Copy button.
            if (toolCopyRect(opt.rect, opt).contains(pos)) {
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
            if (toolHeaderRect(opt.rect, opt).contains(pos)) {
                tm->setExpanded(idx.row(), !idx.data(TranscriptModel::ToolExpandedRole).toBool());
                return true;
            }
            return false;
        }

        if (kind == TranscriptModel::Thinking) {
            // The header line toggles the reasoning body open/closed.
            const TranscriptMetrics metrics = ChatAppearance::instance()->metrics(
                opt.font, opt.palette, opt.rect.width(),
                opt.widget ? opt.widget->devicePixelRatioF() : 1.0);
            if (QRect(opt.rect.left(), opt.rect.top(), opt.rect.width(),
                      metrics.activityHeaderHeight)
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
            const QRect body = messageBodyRect(opt.rect, opt, idx);
            QTextDocument *doc = bodyDoc(idx, qMax(1, body.width()), opt);
            const QPointF rel(pos.x() - body.left(), pos.y() - body.top());
            const QString anchor = doc->documentLayout()->anchorAt(rel);
            if (!anchor.isEmpty()) {
                // A link in an assistant message is model-authored: its text
                // says nothing about its target, so it goes through the scheme
                // policy rather than straight to the OS handler (audit F14).
                agentkate::openModelLink(const_cast<QWidget *>(opt.widget),
                                         QUrl(anchor));
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
        if (toolInspectRect(opt.rect, opt).contains(pos)) {
            QToolTip::showText(event->globalPos(), i18n("Open in inspector"), view);
            return true;
        }
        if (toolCopyRect(opt.rect, opt).contains(pos)) {
            QToolTip::showText(event->globalPos(), i18n("Copy tool call"), view);
            return true;
        }
    }

    QToolTip::hideText();
    event->ignore();
    return QStyledItemDelegate::helpEvent(event, view, opt, idx);
}
