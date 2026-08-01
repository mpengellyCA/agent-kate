// Plan 10 phase 2 — the virtualized chat transcript (model/view) replaces the
// per-message-widget feed. These tests pin the contracts the panel relies on:
//  * append-only growth + row count,
//  * a tool row's result mutating in place (done flag, dataChanged for one row),
//  * the delegate's per-(row,width) height cache: same width is cached, a width
//    change re-measures, and a model mutation busts the entry (via
//    heightInvalidated) — which is what makes window-edge resize O(visible rows),
//  * a mutation keeps the row's stableId: identity is stable, invalidation is
//    explicit, so a streamed message no longer strands one dead cache entry per
//    flush tick.

#include "AgentChatHelpers.h"
#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "state/HarnessTraits.h"

#include <QApplication>
#include <QJsonArray>
#include <QJsonObject>
#include <QListView>
#include <QSignalSpy>
#include <QStyleOptionViewItem>
#include <QTextDocument>
#include <QtTest>

class TranscriptModelTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void appendsGrowRowCount();
    void toolResultMutatesInPlace();
    void toolsVisibilityToggles();
    void findStatePropagates();
    void widthChangeEstimatesThenMeasuresExact();
    void sizeHintMeasuresAtViewportWidth();
    void themeChangeRelaysCachedDocuments();
    void heightCacheInvalidatesOnMutation();
    void stableIdSurvivesInPlaceUpdates();
    void evictionBoundsRamAndKeysResolve();
    void attachmentsRoleRoundTrips();
    void thinkingRowExpands();
    void checklistUpdatesInPlace();
    void toolAttachmentsAddChips();
    void mcpToolsSummarizeTheirArguments();
    void compactionCapabilitySplitsHotFromCold();
    void permissionModeDefaultIsNamedNotPositional();
    void expandedToolDocsAreCachedPerRow();
};

// Audit F18: an expanded tool row used to build (and destroy) two whole
// QTextDocuments on every paint, so scrolling past one big expanded row paid
// two full text layouts per frame. They now come from the same per-row cache
// the body documents use — and, like those, are dropped when the row mutates.
void TranscriptModelTest::expandedToolDocsAreCachedPerRow()
{
    TranscriptModel m;
    const int row = m.appendTool(QStringLiteral("Bash"), QStringLiteral("ls"),
                                 QStringLiteral("{\"command\":\"ls\"}"), true);
    m.setToolResult(row, QStringLiteral("a\nb\nc"), QStringLiteral("a\nb\nc"),
                    false);
    m.setExpanded(0, true);

    TranscriptDelegate d;
    QFont mono;
    mono.setFamily(QStringLiteral("monospace"));
    const QModelIndex idx = m.index(0);
    QTextDocument *first =
        d.toolDoc(idx, TranscriptDelegate::ToolSlot::Detail, QStringLiteral("{}"), mono, 300);
    QVERIFY(first);
    // Same row, same content, same width: the very same laid-out document.
    QCOMPARE(d.toolDoc(idx, TranscriptDelegate::ToolSlot::Detail, QStringLiteral("{}"),
                       mono, 300),
             first);
    // The result slot is its own document — never the detail one.
    QVERIFY(d.toolDoc(idx, TranscriptDelegate::ToolSlot::Result, QStringLiteral("out"),
                      mono, 300)
            != first);
    // New content re-lays the SAME document rather than leaking a new one, and
    // the new text is what it holds.
    QTextDocument *relaid =
        d.toolDoc(idx, TranscriptDelegate::ToolSlot::Detail,
                  QStringLiteral("{\"a\":1}"), mono, 300);
    QCOMPARE(relaid, first);
    QCOMPARE(relaid->toPlainText(), QStringLiteral("{\"a\":1}"));
    // A row mutation drops the cached documents (invalidateRow), so the next
    // fetch cannot paint stale text.
    const quintptr id = idx.data(TranscriptModel::StableIdRole).value<quintptr>();
    d.invalidateRow(id);
    QCOMPARE(d.toolDoc(idx, TranscriptDelegate::ToolSlot::Detail, QStringLiteral("{}"),
                       mono, 300)
                 ->toPlainText(),
             QStringLiteral("{}"));
}

void TranscriptModelTest::appendsGrowRowCount()
{
    TranscriptModel m;
    QCOMPARE(m.rowCount(), 0);
    m.appendNote(QStringLiteral("session started"), QStringLiteral("sys"));
    m.appendMessage(QStringLiteral("Agent Kate"), QStringLiteral("#1a7f6b"),
                    QStringLiteral("hello <b>world</b>"), QStringLiteral("hello world"),
                    false, QStringLiteral("10:00"));
    const int tool = m.appendTool(QStringLiteral("Bash"), QStringLiteral("ls -la"),
                                  QStringLiteral("{\"command\":\"ls -la\"}"), true);
    QCOMPARE(m.rowCount(), 3);
    QCOMPARE(tool, 2);
    QCOMPARE(TranscriptModel::Kind(m.data(m.index(0), TranscriptModel::KindRole).toInt()),
             TranscriptModel::Note);
    QCOMPARE(TranscriptModel::Kind(m.data(m.index(1), TranscriptModel::KindRole).toInt()),
             TranscriptModel::Message);
    QCOMPARE(m.data(m.index(2), TranscriptModel::ToolNameRole).toString(),
             QStringLiteral("Bash"));
    // A fresh tool row is not yet done.
    QVERIFY(!m.data(m.index(2), TranscriptModel::ToolDoneRole).toBool());
}

void TranscriptModelTest::toolResultMutatesInPlace()
{
    TranscriptModel m;
    m.appendNote(QStringLiteral("n"), QStringLiteral("sys"));
    const int row = m.appendTool(QStringLiteral("Read"), QStringLiteral("file.cpp"),
                                 QStringLiteral("{}"), true);
    QSignalSpy spy(&m, &QAbstractItemModel::dataChanged);
    m.setToolResult(row, QStringLiteral("the output"), QStringLiteral("the output"), false);
    QCOMPARE(m.rowCount(), 2); // no new rows — mutation in place
    QVERIFY(m.data(m.index(row), TranscriptModel::ToolDoneRole).toBool());
    QCOMPARE(m.data(m.index(row), TranscriptModel::ToolResultRole).toString(),
             QStringLiteral("the output"));
    QCOMPARE(spy.count(), 1);
    const auto args = spy.takeFirst();
    QCOMPARE(args.at(0).toModelIndex().row(), row); // only that row changed
    QCOMPARE(args.at(1).toModelIndex().row(), row);
}

void TranscriptModelTest::toolsVisibilityToggles()
{
    TranscriptModel m;
    m.appendMessage(QStringLiteral("You"), QStringLiteral("#888"),
                    QStringLiteral("hi"), QStringLiteral("hi"), false, QString());
    const int t = m.appendTool(QStringLiteral("Bash"), QStringLiteral("x"),
                               QStringLiteral("{}"), true);
    m.setToolsVisible(false);
    QVERIFY(!m.data(m.index(t), TranscriptModel::ToolVisibleRole).toBool());
    m.setToolsVisible(true);
    QVERIFY(m.data(m.index(t), TranscriptModel::ToolVisibleRole).toBool());
}

void TranscriptModelTest::findStatePropagates()
{
    TranscriptModel m;
    m.appendMessage(QStringLiteral("Agent Kate"), QStringLiteral("#1a7f6b"),
                    QStringLiteral("find the needle here"),
                    QStringLiteral("find the needle here"), false, QString());
    QSignalSpy spy(&m, &QAbstractItemModel::dataChanged);
    m.setFind(QStringLiteral("needle"), 0);
    QCOMPARE(m.findNeedle(), QStringLiteral("needle"));
    QCOMPARE(m.findCurrentRow(), 0);
    QCOMPARE(spy.count(), 1); // a repaint was requested
}

void TranscriptModelTest::widthChangeEstimatesThenMeasuresExact()
{
    TranscriptModel m;
    // A long body so wrapping at different widths yields different heights.
    QString body;
    for (int i = 0; i < 40; ++i) {
        body += QStringLiteral("word%1 ").arg(i);
    }
    m.appendMessage(QStringLiteral("Agent Kate"), QStringLiteral("#1a7f6b"), body, body,
                    false, QStringLiteral("10:00"));

    TranscriptDelegate d;
    QStyleOptionViewItem opt;
    opt.font = QFont();
    opt.palette = QPalette();

    opt.rect = QRect(0, 0, 200, 0);
    const int hNarrow = d.sizeHint(opt, m.index(0)).height();
    QVERIFY(hNarrow > 0);
    QVERIFY(!d.hasStaleHeights());
    // Same width again — served from cache, identical, still not stale.
    QCOMPARE(d.sizeHint(opt, m.index(0)).height(), hNarrow);
    QVERIFY(!d.hasStaleHeights());

    // The virtualization contract: on a WIDTH CHANGE sizeHint returns the cached
    // height as a cheap estimate (no QTextDocument rebuild) and flags that a
    // settle-time exact re-measure is due. This is what keeps an interactive
    // resize O(N hash lookups) instead of O(N text layouts).
    opt.rect = QRect(0, 0, 600, 0);
    const int hEstimate = d.sizeHint(opt, m.index(0)).height();
    QCOMPARE(hEstimate, hNarrow);         // estimate == old height (not re-laid out)
    QVERIFY(d.hasStaleHeights());         // re-measure is queued

    // The settle pass measures the visible rows exactly: now the wider row wraps
    // to fewer lines and is strictly shorter, and the cache is refreshed.
    const int hExact = d.measureExact(m.index(0), 600, opt);
    QVERIFY2(hExact < hNarrow, "wider row must be shorter once measured exactly");
    d.clearStaleFlag();
    // After the exact measure, asking again at 600 is a cache hit at the right
    // height with no new stale flag.
    QCOMPARE(d.sizeHint(opt, m.index(0)).height(), hExact);
    QVERIFY(!d.hasStaleHeights());
}

// The width trap: QListView hands sizeHint() an option whose rect can be EMPTY,
// and the old fallback then used the view's own width — which includes the
// vertical scrollbar. paint() always gets the viewport width, so measure and
// paint missed each other by the scrollbar's width and the shared body document
// was laid out twice per streaming tick. sizeHint must report viewport width.
void TranscriptModelTest::sizeHintMeasuresAtViewportWidth()
{
    TranscriptModel m;
    QString body;
    for (int i = 0; i < 60; ++i) {
        body += QStringLiteral("word%1 ").arg(i);
    }
    m.appendMessage(QStringLiteral("Agent Kate"), QStringLiteral("#1a7f6b"), body, body,
                    false, QStringLiteral("10:00"));

    QListView view;
    TranscriptDelegate d;
    view.setModel(&m);
    view.setItemDelegate(&d);
    // Always-on keeps the viewport strictly narrower than the view, which is the
    // situation a streaming transcript is permanently in.
    view.setVerticalScrollBarPolicy(Qt::ScrollBarAlwaysOn);
    view.resize(600, 400);
    // The viewport only takes its real geometry once the resize event is
    // delivered, which for a hidden widget never happens.
    view.show();
    qApp->processEvents();

    QVERIFY2(view.viewport()->width() < view.width(),
             "test needs a visible scrollbar to tell the two widths apart");

    QStyleOptionViewItem opt;
    opt.widget = &view;
    opt.font = view.font();
    opt.palette = view.palette();
    opt.rect = QRect(); // what QListView's layout pass actually passes
    QCOMPARE(d.sizeHint(opt, m.index(0)).width(), view.viewport()->width());

    // And the height it cached must be the one paint() (viewport width) needs:
    // asking again at the viewport width is a cache hit, not a stale estimate.
    d.clearStaleFlag();
    opt.rect = QRect(0, 0, view.viewport()->width(), 0);
    const int h = d.sizeHint(opt, m.index(0)).height();
    QVERIFY(h > 0);
    QVERIFY2(!d.hasStaleHeights(),
             "measure and paint widths disagree — the row was measured twice");
}

// The body HTML carries inline palette(...) CSS that Qt resolves to concrete
// colours at setHtml() time, so a cached document keeps painting the previous
// theme's colours unless the cache key notices the palette moved.
void TranscriptModelTest::themeChangeRelaysCachedDocuments()
{
    TranscriptModel m;
    m.appendMessage(QStringLiteral("Agent Kate"), QStringLiteral("#1a7f6b"),
                    QStringLiteral("hello <b>world</b>"), QStringLiteral("hello world"),
                    false, QStringLiteral("10:00"));

    QListView view;
    TranscriptDelegate d;
    view.setModel(&m);
    view.setItemDelegate(&d);
    view.resize(600, 400);

    QStyleOptionViewItem opt;
    opt.widget = &view;
    opt.font = view.font();
    opt.palette = view.palette();
    opt.rect = QRect(0, 0, 400, 0);

    QTextDocument *doc = d.bodyDoc(m.index(0), 360, opt);
    QVERIFY(doc);
    const int rev = doc->revision();
    // Same row, width, html and font: a cache hit, no re-layout.
    QCOMPARE(d.bodyDoc(m.index(0), 360, opt)->revision(), rev);

    QPalette p = qApp->palette();
    p.setColor(QPalette::Highlight, QColor(255, 0, 128));
    qApp->setPalette(p);
    qApp->processEvents();

    QVERIFY2(d.bodyDoc(m.index(0), 360, opt)->revision() > rev,
             "a theme change must re-lay the cached body document, not repaint it stale");
}

void TranscriptModelTest::heightCacheInvalidatesOnMutation()
{
    TranscriptModel m;
    const int row = m.appendTool(QStringLiteral("Bash"), QStringLiteral("echo hi"),
                                 QStringLiteral("{\"command\":\"echo hi\"}"), true);
    TranscriptDelegate d;
    QStyleOptionViewItem opt;
    opt.font = QFont();
    opt.palette = QPalette();
    opt.rect = QRect(0, 0, 500, 0);

    const int collapsed = d.sizeHint(opt, m.index(row)).height();
    // Expanding the tool row must grow it — proves the mutation's
    // heightInvalidated busts the cached collapsed height.
    m.setExpanded(row, true);
    m.setToolResult(row, QStringLiteral("a\nb\nc\nd"), QStringLiteral("a\nb\nc\nd"), false);
    const int expanded = d.sizeHint(opt, m.index(row)).height();
    QVERIFY2(expanded > collapsed, "expanded tool row must be taller than collapsed");
}

// A row's stable id is an identity, not a change counter: streaming rewrites a
// message body every 50ms flush tick, and minting a fresh id each time left the
// delegate's (id -> height) cache holding a dead entry per tick. The model now
// keeps the id and emits heightInvalidated for exactly that row instead.
void TranscriptModelTest::stableIdSurvivesInPlaceUpdates()
{
    TranscriptModel m;
    const int key = m.appendMessage(QStringLiteral("Agent Kate"),
                                    QStringLiteral("#1a7f6b"), QStringLiteral("a"),
                                    QStringLiteral("a"), false, QString());
    const quintptr id0 =
        m.data(m.index(key), TranscriptModel::StableIdRole).value<quintptr>();
    QVERIFY(id0 != 0);

    QSignalSpy invalidated(&m, &TranscriptModel::heightInvalidated);
    for (int i = 0; i < 5; ++i) {
        m.setMessageBody(key, QStringLiteral("a<b>%1</b>").arg(i),
                         QStringLiteral("a%1").arg(i));
    }
    // Every in-place update invalidates the row...
    QCOMPARE(invalidated.count(), 5);
    QCOMPARE(invalidated.takeFirst().at(0).value<quintptr>(), id0);
    // ...and none of them changes its identity.
    QCOMPARE(m.data(m.index(key), TranscriptModel::StableIdRole).value<quintptr>(), id0);

    // A NEW row still gets a distinct id.
    const int other = m.appendMessage(QStringLiteral("You"), QStringLiteral("#888"),
                                      QStringLiteral("b"), QStringLiteral("b"), false,
                                      QString());
    QVERIFY(m.data(m.index(other), TranscriptModel::StableIdRole).value<quintptr>() != id0);
}

// The in-RAM feed is capped (kMaxRows = 5000) so a long session can't grow the
// model without bound. The contract that protects correctness across eviction:
// deferred references (a tool_result landing after a round-trip) use the stable
// key from appendTool, which must (a) resolve to the right row even after the
// front has been evicted and m_base has moved, and (b) become a safe no-op once
// its own row is gone — never a write to a wrongly-shifted row.
void TranscriptModelTest::evictionBoundsRamAndKeysResolve()
{
    TranscriptModel m;
    // A tool appended early, before the feed overflows its cap.
    const int earlyKey = m.appendTool(QStringLiteral("Bash"), QStringLiteral("old"),
                                      QStringLiteral("{}"), true);
    // Push well past the cap so the early row is evicted off the front.
    for (int i = 0; i < 6000; ++i) {
        m.appendNote(QStringLiteral("n"), QStringLiteral("sys"));
    }
    QVERIFY2(m.rowCount() <= 5000, "in-RAM feed must be capped, not grow without bound");
    QVERIFY(m.rowCount() > 0);

    // Delivering a result to the now-evicted tool is a safe no-op.
    m.setToolResult(earlyKey, QStringLiteral("late"), QStringLiteral("late"), false);

    // A tool appended after eviction still resolves by key (m_base != 0 now).
    const int liveKey = m.appendTool(QStringLiteral("Read"), QStringLiteral("live"),
                                     QStringLiteral("{}"), true);
    const int liveRow = m.rowCount() - 1;
    QSignalSpy spy(&m, &QAbstractItemModel::dataChanged);
    m.setToolResult(liveKey, QStringLiteral("done"), QStringLiteral("done"), false);
    QCOMPARE(spy.count(), 1);
    QCOMPARE(spy.takeFirst().at(0).toModelIndex().row(), liveRow); // exactly the live row
    QCOMPARE(m.data(m.index(liveRow), TranscriptModel::ToolResultRole).toString(),
             QStringLiteral("done"));
    QVERIFY(m.data(m.index(liveRow), TranscriptModel::ToolDoneRole).toBool());
}

// A You message can carry compact attachment metadata (plan 13 phase 4). It must
// round-trip through AttachmentsRole so the delegate can draw one chip per file;
// a message with no attachments returns an empty array (never garbage), and the
// chip block grows the row's measured height (proving the delegate lays it out).
void TranscriptModelTest::attachmentsRoleRoundTrips()
{
    TranscriptModel m;
    // A plain message: no attachments.
    m.appendMessage(QStringLiteral("You"), QStringLiteral("#1a5fb4"),
                    QStringLiteral("plain"), QStringLiteral("plain"), false,
                    QStringLiteral("10:00"));
    QVERIFY(m.data(m.index(0), TranscriptModel::AttachmentsRole).toJsonArray().isEmpty());

    QJsonArray atts{
        QJsonObject{{QStringLiteral("name"), QStringLiteral("a.png")},
                    {QStringLiteral("kind"), QStringLiteral("image")},
                    {QStringLiteral("path"), QStringLiteral("/tmp/a.png")},
                    {QStringLiteral("mediaType"), QStringLiteral("image/png")}},
        QJsonObject{{QStringLiteral("name"), QStringLiteral("notes.txt")},
                    {QStringLiteral("kind"), QStringLiteral("text")},
                    {QStringLiteral("path"), QStringLiteral("/tmp/notes.txt")},
                    {QStringLiteral("outside"), true}}};
    m.appendMessage(QStringLiteral("You"), QStringLiteral("#1a5fb4"),
                    QStringLiteral("with files"), QStringLiteral("with files"), false,
                    QStringLiteral("10:01"), atts);
    const QJsonArray got =
        m.data(m.index(1), TranscriptModel::AttachmentsRole).toJsonArray();
    QCOMPARE(got.size(), 2);
    QCOMPARE(got.at(0).toObject().value(QStringLiteral("name")).toString(),
             QStringLiteral("a.png"));
    QCOMPARE(got.at(1).toObject().value(QStringLiteral("kind")).toString(),
             QStringLiteral("text"));
    QVERIFY(got.at(1).toObject().value(QStringLiteral("outside")).toBool());

    // The attachment chip block makes the with-files row taller than the plain one
    // (same body font/width) — the delegate lays the chips out under the body.
    TranscriptDelegate d;
    QStyleOptionViewItem opt;
    opt.font = QFont();
    opt.palette = QPalette();
    opt.rect = QRect(0, 0, 500, 0);
    const int plainH = d.sizeHint(opt, m.index(0)).height();
    const int withAttH = d.sizeHint(opt, m.index(1)).height();
    QVERIFY2(withAttH > plainH, "attachment chips must add to the message row height");
}

// A thinking row (plan 14 P2) starts collapsed to its one-line preview and
// grows when expanded — same collapse contract as a tool row, distinct kind.
void TranscriptModelTest::thinkingRowExpands()
{
    TranscriptModel m;
    const int key = m.appendThinking(
        QStringLiteral("<p>long reasoning<br>line two<br>line three</p>"),
        QStringLiteral("long reasoning\nline two\nline three"),
        QStringLiteral("long reasoning"));
    QCOMPARE(TranscriptModel::Kind(m.data(m.index(key), TranscriptModel::KindRole).toInt()),
             TranscriptModel::Thinking);
    QCOMPARE(m.data(m.index(key), TranscriptModel::ToolSummaryRole).toString(),
             QStringLiteral("long reasoning"));

    TranscriptDelegate d;
    QStyleOptionViewItem opt;
    opt.font = QFont();
    opt.palette = QPalette();
    opt.rect = QRect(0, 0, 500, 0);
    const int collapsed = d.sizeHint(opt, m.index(key)).height();
    QVERIFY(collapsed > 0);
    m.setExpanded(key, true);
    const int expanded = d.sizeHint(opt, m.index(key)).height();
    QVERIFY2(expanded > collapsed, "expanded thinking row must be taller than collapsed");
}

// The plan checklist (plan 14 P2) is ONE card updated in place: each TodoWrite
// replaces the items rather than appending a stale copy; an evicted card
// reports false so the caller appends anew.
void TranscriptModelTest::checklistUpdatesInPlace()
{
    TranscriptModel m;
    const QJsonArray v1{
        QJsonObject{{QStringLiteral("content"), QStringLiteral("read code")},
                    {QStringLiteral("status"), QStringLiteral("in_progress")}}};
    const int key = m.appendChecklist(v1);
    QCOMPARE(m.rowCount(), 1);
    QCOMPARE(TranscriptModel::Kind(m.data(m.index(key), TranscriptModel::KindRole).toInt()),
             TranscriptModel::Checklist);

    const QJsonArray v2{
        QJsonObject{{QStringLiteral("content"), QStringLiteral("read code")},
                    {QStringLiteral("status"), QStringLiteral("completed")}},
        QJsonObject{{QStringLiteral("content"), QStringLiteral("fix bug")},
                    {QStringLiteral("status"), QStringLiteral("pending")}}};
    QSignalSpy spy(&m, &QAbstractItemModel::dataChanged);
    QVERIFY(m.setChecklist(key, v2));
    QCOMPARE(m.rowCount(), 1); // updated in place, no second card
    QCOMPARE(spy.count(), 1);
    QCOMPARE(m.data(m.index(0), TranscriptModel::ChecklistRole).toJsonArray().size(), 2);

    // More items → taller card (the delegate lays out one line per item).
    TranscriptDelegate d;
    QStyleOptionViewItem opt;
    opt.font = QFont();
    opt.palette = QPalette();
    opt.rect = QRect(0, 0, 500, 0);
    const int twoItems = d.sizeHint(opt, m.index(0)).height();
    QJsonArray v3 = v2;
    v3.append(QJsonObject{{QStringLiteral("content"), QStringLiteral("add test")},
                          {QStringLiteral("status"), QStringLiteral("pending")}});
    QVERIFY(m.setChecklist(key, v3));
    const int threeItems = d.sizeHint(opt, m.index(0)).height();
    QVERIFY2(threeItems > twoItems, "an extra checklist item must add a line");

    // Evict the card off the front; the stale key then reports false.
    for (int i = 0; i < 6000; ++i) {
        m.appendNote(QStringLiteral("n"), QStringLiteral("sys"));
    }
    QVERIFY(!m.setChecklist(key, v1));
}

// A tool row that received image blocks in its result (plan 14 P4) carries
// attachment chips: they round-trip through the role and grow the collapsed
// row's measured height (the delegate lays the chip row under the header).
void TranscriptModelTest::toolAttachmentsAddChips()
{
    TranscriptModel m;
    const int key = m.appendTool(QStringLiteral("desktop_screenshot"),
                                 QStringLiteral("whole screen"), QStringLiteral("{}"),
                                 true);
    TranscriptDelegate d;
    QStyleOptionViewItem opt;
    opt.font = QFont();
    opt.palette = QPalette();
    opt.rect = QRect(0, 0, 500, 0);
    const int bare = d.sizeHint(opt, m.index(0)).height();

    m.setToolResult(key, QStringLiteral("captured"), QStringLiteral("captured"), false);
    m.setToolAttachments(key,
                         QJsonArray{QJsonObject{
                             {QStringLiteral("name"), QStringLiteral("shot-1.png")},
                             {QStringLiteral("kind"), QStringLiteral("image")},
                             {QStringLiteral("path"), QStringLiteral("/tmp/none.png")}}});
    const QJsonArray got =
        m.data(m.index(0), TranscriptModel::AttachmentsRole).toJsonArray();
    QCOMPARE(got.size(), 1);
    QCOMPARE(got.at(0).toObject().value(QStringLiteral("kind")).toString(),
             QStringLiteral("image"));
    const int withChips = d.sizeHint(opt, m.index(0)).height();
    QVERIFY2(withChips > bare, "image chips must add to the collapsed tool row height");

    // A non-tool row refuses tool attachments (guarded setter).
    const int note = m.appendNote(QStringLiteral("n"), QStringLiteral("sys"));
    m.setToolAttachments(note, got);
    QVERIFY(m.data(m.index(1), TranscriptModel::AttachmentsRole).toJsonArray().isEmpty());
}

// Cooperation and Cowork tool rows (plan 16 P2 / Feature 4b) read as sentences
// instead of raw JSON: each verb's summary names what it did, long bodies are
// reduced to their first line, and payloads that may hold secrets (a typed
// value) never appear. Unknown mcp__ servers keep the compact-JSON fallback.
void TranscriptModelTest::mcpToolsSummarizeTheirArguments()
{
    using agentkate::permSummary;
    const auto coop = [](const char *verb) {
        return QStringLiteral("mcp__cooperation__") + QLatin1String(verb);
    };

    QCOMPARE(permSummary(coop("post_note"),
                         QJsonObject{{QStringLiteral("text"),
                                      QStringLiteral("claiming the parser\nthen editing")}}),
             QStringLiteral("claiming the parser"));
    QCOMPARE(permSummary(coop("claim_file"),
                         QJsonObject{{QStringLiteral("path"), QStringLiteral("src/main.go")}}),
             QStringLiteral("src/main.go"));
    QCOMPARE(permSummary(coop("request_review"),
                         QJsonObject{{QStringLiteral("summary"), QStringLiteral("rewired the relay")}}),
             QStringLiteral("rewired the relay"));
    QCOMPARE(permSummary(coop("launch_agent"),
                         QJsonObject{{QStringLiteral("backend"), QStringLiteral("kimi")},
                                     {QStringLiteral("model"), QStringLiteral("kimi-code/k3")},
                                     {QStringLiteral("title"), QStringLiteral("pong worker")},
                                     {QStringLiteral("prompt"), QStringLiteral("the briefing")}}),
             QStringLiteral("kimi/kimi-code/k3: pong worker"));
    QCOMPARE(permSummary(coop("send_agent"),
                         QJsonObject{{QStringLiteral("thread_id"), QStringLiteral("t-w")},
                                     {QStringLiteral("message"), QStringLiteral("do this\nand that")}}),
             QStringLiteral("t-w: do this"));
    QCOMPARE(permSummary(coop("wait_agent"),
                         QJsonObject{{QStringLiteral("thread_id"), QStringLiteral("t-w")}}),
             QStringLiteral("t-w"));
    // The core's cross-subtree approval prompt for the same verb names the
    // target "targetThreadId"; the ask must read like the tool row.
    QCOMPARE(permSummary(coop("send_agent"),
                         QJsonObject{{QStringLiteral("targetThreadId"), QStringLiteral("t-x")},
                                     {QStringLiteral("message"), QStringLiteral("please stop")}}),
             QStringLiteral("t-x: please stop"));
    QCOMPARE(permSummary(coop("close_agent"),
                         QJsonObject{{QStringLiteral("thread_id"), QStringLiteral("t-w")}}),
             QStringLiteral("t-w"));
    QCOMPARE(permSummary(coop("discard_agent"),
                         QJsonObject{{QStringLiteral("thread_id"), QStringLiteral("t-w")},
                                     {QStringLiteral("force"), true}}),
             QStringLiteral("t-w"));
    // Fixed labels for the parameterless verbs — never "{}".
    for (const char *verb : {"read_notes", "get_presence", "list_open_files", "whoami"}) {
        const QString s = permSummary(coop(verb), QJsonObject{});
        QVERIFY2(!s.isEmpty() && !s.startsWith(QLatin1Char('{')),
                 qPrintable(QStringLiteral("%1 -> %2").arg(QLatin1String(verb), s)));
    }
    QCOMPARE(permSummary(coop("list_agents"),
                         QJsonObject{{QStringLiteral("all_workspaces"), true}}),
             QStringLiteral("every workspace"));

    // The permission gate carries the RAW ARGUMENTS of the tool it is gating —
    // the most secret-bearing payload in the catalogue. The row must name the
    // gated tool and nothing else, in either arg spelling the bridge accepts,
    // and must never fall through to the generic JSON dump.
    const QJsonObject gateInput{
        {QStringLiteral("command"), QStringLiteral("deploy --token=hunter2")}};
    for (const QString &key : {QStringLiteral("tool_name"), QStringLiteral("toolName")}) {
        const QString s = permSummary(coop("request_permission"),
                                      QJsonObject{{key, QStringLiteral("Bash")},
                                                  {QStringLiteral("input"), gateInput}});
        QCOMPARE(s, QStringLiteral("Bash"));
        QVERIFY2(!s.contains(QStringLiteral("hunter2")),
                 "the gated tool's input must never reach the transcript row");
    }
    // Even with no tool name at all, the input is not dumped.
    const QString unnamed =
        permSummary(coop("request_permission"), QJsonObject{{QStringLiteral("input"), gateInput}});
    QVERIFY2(!unnamed.contains(QStringLiteral("hunter2")) && !unnamed.isEmpty(),
             qPrintable(QStringLiteral("nameless gate leaked: %1").arg(unnamed)));

    // Cowork: the element, never the text being typed into it.
    const QString typed =
        permSummary(QStringLiteral("mcp__cowork__desktop_set_text"),
                    QJsonObject{{QStringLiteral("elementId"), QStringLiteral("el-7")},
                                {QStringLiteral("text"), QStringLiteral("hunter2")}});
    QCOMPARE(typed, QStringLiteral("el-7"));
    QCOMPARE(permSummary(QStringLiteral("mcp__cowork__desktop_click"),
                         QJsonObject{{QStringLiteral("x"), 100}, {QStringLiteral("y"), 250}}),
             QStringLiteral("100, 250"));
    // Every Cowork verb has a digest — none may fall through to raw JSON.
    QCOMPARE(permSummary(QStringLiteral("mcp__cowork__desktop_scroll"),
                         QJsonObject{{QStringLiteral("dx"), 0}, {QStringLiteral("dy"), -3}}),
             QStringLiteral("+0,-3"));
    QCOMPARE(permSummary(QStringLiteral("mcp__cowork__desktop_move_pointer_relative"),
                         QJsonObject{{QStringLiteral("dx"), 12}, {QStringLiteral("dy"), 0}}),
             QStringLiteral("+12,+0"));
    QCOMPARE(permSummary(QStringLiteral("mcp__cowork__desktop_drag"),
                         QJsonObject{{QStringLiteral("fromX"), 1}, {QStringLiteral("fromY"), 2},
                                     {QStringLiteral("toX"), 3}, {QStringLiteral("toY"), 4}}),
             QStringLiteral("1,2 → 3,4"));
    QCOMPARE(permSummary(QStringLiteral("mcp__cowork__desktop_screenshot"), QJsonObject{}),
             QStringLiteral("the active screen"));
    QCOMPARE(permSummary(QStringLiteral("mcp__cowork__desktop_screenshot"),
                         QJsonObject{{QStringLiteral("target"),
                                      QJsonObject{{QStringLiteral("kind"), QStringLiteral("window")},
                                                  {QStringLiteral("windowId"), QStringLiteral("w-9")}}}}),
             QStringLiteral("w-9"));
    for (const char *verb : {"desktop_set_pointer_profile", "desktop_screenshot",
                             "desktop_scroll", "desktop_drag",
                             "desktop_move_pointer_relative"}) {
        const QString s = permSummary(QStringLiteral("mcp__cowork__") + QLatin1String(verb),
                                      QJsonObject{});
        QVERIFY2(!s.isEmpty() && !s.startsWith(QLatin1Char('{')),
                 qPrintable(QStringLiteral("%1 -> %2").arg(QLatin1String(verb), s)));
    }

    // A third-party MCP server keeps today's behaviour (the generic fallback).
    QCOMPARE(permSummary(QStringLiteral("mcp__other__do_thing"),
                         QJsonObject{{QStringLiteral("q"), QStringLiteral("v")}}),
             QStringLiteral("{\"q\":\"v\"}"));

    // The activity line distinguishes orchestration from board chatter and
    // from desktop work.
    QCOMPARE(agentkate::activityFor(coop("launch_agent")),
             QStringLiteral("Agent Kate is directing its team…"));
    QCOMPARE(agentkate::activityFor(coop("post_note")),
             QStringLiteral("Agent Kate is coordinating with the team…"));
    QCOMPARE(agentkate::activityFor(QStringLiteral("mcp__cowork__desktop_click")),
             QStringLiteral("Agent Kate is working at the desktop…"));
}

// The built-in traits must mirror the core's two-field compaction capability
// (harness.Capabilities Compaction/ColdCompact). Kimi compacts HOT only, and
// the panel's pre-resume summary-recovery prompt ends in a COLD compaction — so
// a UI that gates that prompt on `compaction` offers a dormant kimi thread a
// modal whose every choice the core refuses.
void TranscriptModelTest::compactionCapabilitySplitsHotFromCold()
{
    const HarnessTraits claude = HarnessRegistry::self()->traits(QStringLiteral("claude"));
    QVERIFY(claude.compaction);
    QVERIFY(claude.coldCompact);

    const HarnessTraits kimi = HarnessRegistry::self()->traits(QStringLiteral("kimi"));
    QVERIFY(kimi.compaction);
    QVERIFY(!kimi.coldCompact);
}

// permissionModes is the engine's own vocabulary in the CLI's order, so the
// default mode must be named, never "whatever is at index 0" — reordering the
// list upstream must not change what a fresh profile starts on.
void TranscriptModelTest::permissionModeDefaultIsNamedNotPositional()
{
    QCOMPARE(HarnessRegistry::self()->traits(QStringLiteral("claude")).defaultPermissionMode(),
             QStringLiteral("acceptEdits"));

    HarnessTraits reordered;
    reordered.permissionModes = {QStringLiteral("bypassPermissions"),
                                 QStringLiteral("manual"),
                                 QStringLiteral("acceptEdits")};
    QCOMPARE(reordered.defaultPermissionMode(), QStringLiteral("acceptEdits"));

    // No acceptEdits in the vocabulary: fall to "default", then to the first
    // entry, then to an empty string for a discovered-vocabulary harness.
    HarnessTraits noAccept;
    noAccept.permissionModes = {QStringLiteral("plan"), QStringLiteral("default")};
    QCOMPARE(noAccept.defaultPermissionMode(), QStringLiteral("default"));

    HarnessTraits foreign;
    foreign.permissionModes = {QStringLiteral("careful"), QStringLiteral("bold")};
    QCOMPARE(foreign.defaultPermissionMode(), QStringLiteral("careful"));

    QCOMPARE(HarnessTraits().defaultPermissionMode(), QString());
}

QTEST_MAIN(TranscriptModelTest)
#include "TranscriptModelTest.moc"
