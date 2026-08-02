// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "AttachmentBuilder.h"
#include "SafeContent.h"
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

// The single source of a body document's metrics — font, zero margin, HTML and
// wrap width. paint()/measure use it through bodyDoc(); the selection
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
    watchPalette(opt);
    auto it = m_docCache.find(id);
    if (it != m_docCache.end() && it->doc) {
        if (it->width == contentWidth && it->html == html && it->font == opt.font
            && it->paletteGen == m_paletteGen) {
            return it->doc; // already laid out for exactly this
        }
        // Same row, new content / width / theme: re-set this document rather than
        // allocating another (QTextDocument reuses its internal structures).
        configureBodyDoc(it->doc, opt.font, contentWidth, html);
        it->width = contentWidth;
        it->html = html;
        it->font = opt.font;
        it->paletteGen = m_paletteGen;
        return it->doc;
    }
    if (m_docCache.size() >= kDocCacheCap) {
        for (auto &e : m_docCache) {
            delete e.doc;
        }
        m_docCache.clear();
    }
    auto *doc = new agentkate::GuardedTextDocument;
    configureBodyDoc(doc, opt.font, contentWidth, html);
    m_docCache.insert(id, DocEntry{contentWidth, html, opt.font, m_paletteGen, doc});
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
    const auto configure = [&](QTextDocument *doc) {
        doc->setDefaultFont(mono);
        doc->setDocumentMargin(0);
        doc->setPlainText(plain);
        doc->setTextWidth(qMax(1, contentWidth));
    };
    auto it = cache.find(id);
    if (it != cache.end() && it->doc) {
        if (it->width == contentWidth && it->html == plain && it->font == mono) {
            return it->doc; // already laid out for exactly this
        }
        configure(it->doc);
        it->width = contentWidth;
        it->html = plain;
        it->font = mono;
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
    cache.insert(id, DocEntry{contentWidth, plain, mono, 0, doc});
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

    const int contentLeft = kOuterMarginX;
    const int contentWidth = width - 2 * kOuterMarginX;
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
        QFont small = opt.font;
        small.setPointSizeF(opt.font.pointSizeF() * 0.85);
        const int tsW =
            ts.isEmpty() ? 0 : QFontMetrics(small).horizontalAdvance(ts) + kNotePadX;
        const int textW = contentWidth - 2 * kNotePadX - tsW;
        QTextDocument *doc = self->bodyDoc(idx, qMax(1, textW), opt);
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
            if (tsW > 0) {
                painter->save();
                painter->setFont(small);
                painter->setPen(opt.palette.color(QPalette::Mid));
                painter->drawText(QRect(rowRect.left() + contentLeft + kNotePadX + textW,
                                        rowRect.top() + kNotePadY, tsW, lineH),
                                  Qt::AlignRight | Qt::AlignVCenter, ts);
                painter->restore();
            }
        }
        return h;
    }

    if (kind == TranscriptModel::Message) {
        const int innerW = contentWidth - 2 * kCardPadX;
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
        QList<ChipLayout> chips;
        if (!atts.isEmpty()) {
            chips = layoutAttachmentChips(atts, opt.font, QPoint(0, 0), qMax(1, innerW),
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
                painter->save();
                painter->setRenderHint(QPainter::Antialiasing, true);
                QFont chipFont = opt.font;
                for (int i = 0; i < chips.size(); ++i) {
                    ChipLayout c = chips.at(i);
                    c.rect.translate(origin);
                    const QJsonObject att = atts.at(i).toObject();
                    // Chip background + border (palette-only).
                    painter->setPen(opt.palette.color(QPalette::Mid));
                    painter->setBrush(opt.palette.color(QPalette::Base));
                    painter->drawRoundedRect(c.rect.adjusted(0, 0, -1, -1), 6, 6);
                    int textLeft = c.rect.left() + kChipPadX;
                    if (c.image) {
                        // Chips on replayed cards have only a path (no dataB64),
                        // so the thumbnail comes from a file. Which file is
                        // resolveAttachmentPath's call: the origin unless our
                        // cached copy is the one still holding the sent bytes.
                        const QPixmap pm = self->chipPixmap(
                            att, kChipIcon, painter->device()->devicePixelRatioF());
                        const QRect iconR(textLeft, c.rect.top() + (kChipH - kChipIcon) / 2,
                                          kChipIcon, kChipIcon);
                        if (!pm.isNull()) {
                            drawChipThumbnail(painter, iconR, pm);
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
        return total;
    }

    if (kind == TranscriptModel::Thinking) {
        // A quiet, collapsible reasoning row: no card frame, just a dim header
        // line ("▸ 💭 Thinking · preview") that expands to the dim body.
        const bool expanded = idx.data(TranscriptModel::ToolExpandedRole).toBool();
        int total = kToolHeaderH;
        QTextDocument *thinkDoc = nullptr;
        int bodyH = 0;
        const int bodyW = contentWidth - 2 * kDetailPadX;
        if (expanded) {
            thinkDoc = self->bodyDoc(idx, qMax(1, bodyW), opt);
            bodyH = int(thinkDoc->size().height());
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
            if (thinkDoc) {
                painter->save();
                painter->translate(rowRect.left() + contentLeft + kDetailPadX,
                                   rowRect.top() + kToolHeaderH + kToolPad);
                QAbstractTextDocumentLayout::PaintContext ctx;
                ctx.palette = opt.palette;
                ctx.palette.setColor(QPalette::Text, ThemeManager::palette().agentIdle);
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
    // Image chips (a tool_result carrying image blocks, e.g. a screenshot)
    // sit directly under the header in both states — the image usually IS the
    // result, so it reads first; the expanded detail shifts below it.
    const QJsonArray toolAtts =
        idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
    int toolChipsH = 0;
    QList<ChipLayout> toolChips; // laid out once; translated at paint time (F18)
    if (!toolAtts.isEmpty()) {
        toolChips = layoutAttachmentChips(toolAtts, opt.font, QPoint(0, 0),
                                          qMax(1, contentWidth - 2 * kDetailPadX),
                                          toolChipsH);
        total += toolChipsH + kToolPad;
    }
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
        // Cached per row (audit F18): measure and paint now share one layout,
        // and a repaint of an unchanged row costs a hash lookup instead of two
        // fresh QTextDocuments over up to the whole output.
        const QString detail = idx.data(TranscriptModel::ToolDetailRole).toString();
        if (!detail.isEmpty()) {
            detailDoc = self->toolDoc(idx, TranscriptDelegate::ToolSlot::Detail, detail,
                                      mono, detailW);
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
                                      mono, detailW);
            resultH = int(resultDoc->size().height());
        }
        if (idx.data(TranscriptModel::ToolTruncatedRole).toBool()) {
            extraH = lineH + kToolPad;
        }
        total += kToolPad + detailH + (detailH && resultH ? kToolPad : 0) + resultH
               + extraH + kToolPad;
    }

    if (painter) {
        // A failed tool used to render exactly like a successful one — same ✓,
        // same frame — so the only way to find the failure in a long turn was
        // to expand every row (audit F40). It now carries a ✗ in the theme's
        // negative colour and a negative card border.
        const bool failed = idx.data(TranscriptModel::ToolErrorRole).toBool();
        const QColor errColor = ThemeManager::palette().negative;
        const QRect card(rowRect.left() + contentLeft, rowRect.top(),
                         contentWidth, total);
        painter->save();
        painter->setRenderHint(QPainter::Antialiasing, true);
        painter->setPen(failed ? errColor : opt.palette.color(QPalette::Mid));
        painter->setBrush(Qt::NoBrush);
        painter->drawRoundedRect(card.adjusted(0, 0, -1, -1), 7, 7);
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
        QString header = QStringLiteral("%1 %2   %3").arg(kind, name, summary);
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
        const QRect hdr(card.left() + 8, card.top(),
                        card.width() - 8 - kToolCopyW - kToolInspectW, kToolHeaderH);
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
        const QRect inspectR(card.right() - kToolCopyW - kToolInspectW, card.top(),
                             kToolInspectW, kToolHeaderH);
        painter->drawText(inspectR, Qt::AlignCenter, QStringLiteral("⤢"));
        const QRect copyR(card.right() - kToolCopyW, card.top(), kToolCopyW,
                          kToolHeaderH);
        painter->drawText(copyR, Qt::AlignCenter, QStringLiteral("⧉"));
        painter->restore();

        if (toolChipsH > 0) {
            const QPoint origin(card.left() + kDetailPadX, card.top() + kToolHeaderH);
            painter->save();
            painter->setRenderHint(QPainter::Antialiasing, true);
            for (int i = 0; i < toolChips.size(); ++i) {
                ChipLayout c = toolChips.at(i);
                c.rect.translate(origin);
                const QJsonObject att = toolAtts.at(i).toObject();
                painter->setPen(opt.palette.color(QPalette::Mid));
                painter->setBrush(opt.palette.color(QPalette::Base));
                painter->drawRoundedRect(c.rect.adjusted(0, 0, -1, -1), 6, 6);
                int textLeft = c.rect.left() + kChipPadX;
                if (c.image) {
                    const QPixmap pm = self->chipPixmap(
                        att, kChipIcon, painter->device()->devicePixelRatioF());
                    const QRect iconR(textLeft, c.rect.top() + (kChipH - kChipIcon) / 2,
                                      kChipIcon, kChipIcon);
                    if (!pm.isNull()) {
                        drawChipThumbnail(painter, iconR, pm);
                    } else {
                        painter->setPen(opt.palette.color(QPalette::Mid));
                        painter->drawText(iconR, Qt::AlignCenter,
                                          QStringLiteral("\U0001f5bc"));
                    }
                    textLeft += kChipIcon + kChipHGap;
                }
                painter->setFont(opt.font);
                painter->setPen(opt.palette.color(QPalette::Link));
                painter->drawText(QRect(textLeft, c.rect.top(),
                                        c.rect.right() - textLeft - kChipPadX + 1,
                                        c.rect.height()),
                                  Qt::AlignLeft | Qt::AlignVCenter, c.label);
            }
            painter->restore();
        }

        if (expanded) {
            int y = card.top() + kToolHeaderH
                    + (toolChipsH > 0 ? toolChipsH + kToolPad : 0) + kToolPad;
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
                if (!done) {
                    // Provisional output of a still-running tool: dimmed so it
                    // is legible as "so far", not as the result (audit F39).
                    ctx.palette.setColor(QPalette::Text,
                                         opt.palette.color(QPalette::Mid));
                }
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
    const int h = layoutRow(idx, width, opt, nullptr, QRect(), this);
    if (m_heightCache.size() >= kHeightCacheCap) {
        m_heightCache.clear();
    }
    m_heightCache.insert(id, CacheEntry{width, h});
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
    m_heightCache.insert(id, CacheEntry{width, h});
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
            // An image chip (screenshot result) opens its preview.
            const QJsonArray toolAtts =
                idx.data(TranscriptModel::AttachmentsRole).toJsonArray();
            if (!toolAtts.isEmpty()) {
                const int availW =
                    opt.rect.width() - 2 * kOuterMarginX - 2 * kDetailPadX;
                const QPoint origin(opt.rect.left() + kOuterMarginX + kDetailPadX,
                                    opt.rect.top() + kToolHeaderH);
                int dummy = 0;
                const QList<ChipLayout> laid = layoutAttachmentChips(
                    toolAtts, opt.font, origin, qMax(1, availW), dummy);
                for (int i = 0; i < laid.size(); ++i) {
                    if (laid.at(i).rect.contains(pos)) {
                        emit attachmentActivated(toolAtts.at(i).toObject());
                        return true;
                    }
                }
            }
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
            QTextDocument *doc = bodyDoc(idx, qMax(1, innerW), opt);
            const QFontMetrics fm(opt.font);
            const QPointF rel(pos.x() - (opt.rect.left() + kOuterMarginX + kCardPadX),
                              pos.y() - (opt.rect.top() + kCardPadTop + fm.height()
                                         + kRoleRowGap));
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
