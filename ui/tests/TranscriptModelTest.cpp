// Plan 10 phase 2 — the virtualized chat transcript (model/view) replaces the
// per-message-widget feed. These tests pin the contracts the panel relies on:
//  * append-only growth + row count,
//  * a tool row's result mutating in place (done flag, dataChanged for one row),
//  * the delegate's per-(row,width) height cache: same width is cached, a width
//    change re-measures, and a model mutation (stableId bump) busts the entry —
//    which is what makes window-edge resize O(visible rows).

#include "TranscriptDelegate.h"
#include "TranscriptModel.h"

#include <QJsonArray>
#include <QJsonObject>
#include <QSignalSpy>
#include <QStyleOptionViewItem>
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
    void heightCacheInvalidatesOnMutation();
    void evictionBoundsRamAndKeysResolve();
    void attachmentsRoleRoundTrips();
    void thinkingRowExpands();
    void checklistUpdatesInPlace();
};

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
    // Expanding the tool row must grow it — proves the stableId bump on mutation
    // busts the cached collapsed height.
    m.setExpanded(row, true);
    m.setToolResult(row, QStringLiteral("a\nb\nc\nd"), QStringLiteral("a\nb\nc\nd"), false);
    const int expanded = d.sizeHint(opt, m.index(row)).height();
    QVERIFY2(expanded > collapsed, "expanded tool row must be taller than collapsed");
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

QTEST_MAIN(TranscriptModelTest)
#include "TranscriptModelTest.moc"
