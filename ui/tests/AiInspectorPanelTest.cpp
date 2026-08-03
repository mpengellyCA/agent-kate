// The Agent Activity inspector's accounting honesty and memory bounds, on the
// real widget, driven through the CoreClient::notification wire it listens to.
//
// F60 (the F19b sibling): a "result" event's usage is a TURN's spend only on an
// engine that positively reports usage per turn. Kimi's is its /usage readout —
// a cumulative context fill that repeats most of itself every turn — and the
// panel was summing it into session totals that grew quadratically. The gate is
// HarnessTraits::usageReporting, the same one AgentPanel's `billed` reads.
//
// The same pass bounds the per-thread state: a tool call's m_rows entry is
// dropped when its result lands, and the per-thread timeline is a ring capped
// like the all-threads feed — this panel must never be the thing that grows
// forever.

#include "AiInspectorPanel.h"
#include "ipc/CoreClient.h"
#include "state/HarnessTraits.h"

#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QTreeWidget>
#include <QtTest>

namespace {

// One agent.event notification carrying a single event for the given thread.
QJsonObject threadEvent(const QString &threadId, const QJsonObject &event)
{
    return QJsonObject{
        {QStringLiteral("threadId"), threadId},
        {QStringLiteral("events"), QJsonArray{event}},
    };
}

// A "result" event with the given usage numbers (the CLI's field names).
QJsonObject resultEvent(qlonglong inTok, qlonglong outTok, qlonglong cacheRead,
                        qlonglong cacheCreate)
{
    return QJsonObject{
        {QStringLiteral("type"), QStringLiteral("result")},
        {QStringLiteral("usage"),
         QJsonObject{
             {QStringLiteral("input_tokens"), double(inTok)},
             {QStringLiteral("output_tokens"), double(outTok)},
             {QStringLiteral("cache_read_input_tokens"), double(cacheRead)},
             {QStringLiteral("cache_creation_input_tokens"), double(cacheCreate)},
         }},
    };
}

// An assistant event holding one tool_use block.
QJsonObject toolUseEvent(const QString &id, const QString &name)
{
    return QJsonObject{
        {QStringLiteral("type"), QStringLiteral("assistant")},
        {QStringLiteral("message"),
         QJsonObject{{QStringLiteral("content"),
                      QJsonArray{QJsonObject{
                          {QStringLiteral("type"), QStringLiteral("tool_use")},
                          {QStringLiteral("id"), id},
                          {QStringLiteral("name"), name},
                          {QStringLiteral("input"), QJsonObject{}},
                      }}}}},
    };
}

// A user event holding one tool_result block.
QJsonObject toolResultEvent(const QString &id, const QString &text)
{
    return QJsonObject{
        {QStringLiteral("type"), QStringLiteral("user")},
        {QStringLiteral("message"),
         QJsonObject{{QStringLiteral("content"),
                      QJsonArray{QJsonObject{
                          {QStringLiteral("type"), QStringLiteral("tool_result")},
                          {QStringLiteral("tool_use_id"), id},
                          {QStringLiteral("content"), text},
                      }}}}},
    };
}

// The totals label — the only word-wrapping label the panel has, so the test
// is not coupled to the widget tree.
QString totalsText(QWidget *panel)
{
    const auto labels = panel->findChildren<QLabel *>();
    for (QLabel *l : labels) {
        if (l->wordWrap()) {
            return l->text();
        }
    }
    return {};
}

// The per-thread timeline: the three-column tree (the all-threads feed has 5).
QTreeWidget *timelineTree(QWidget *panel)
{
    const auto trees = panel->findChildren<QTreeWidget *>();
    for (QTreeWidget *t : trees) {
        if (t->columnCount() == 3) {
            return t;
        }
    }
    return nullptr;
}

} // namespace

class AiInspectorPanelTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();

    void aCumulativeReadoutIsNeverSummed();
    void aPerTurnSpendAccumulates();
    void aResolvedToolCallIsLookedUpOnlyOnce();
    void theTimelineIsABoundedRing();
};

void AiInspectorPanelTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);

    // The production registry has no offline defaults: harness.list is its
    // authority. Install the two traits this test needs so its accounting
    // assertions exercise both supported result-usage semantics.
    HarnessTraits claude;
    claude.id = QStringLiteral("claude");
    claude.usageReporting = true;
    HarnessTraits kimi;
    kimi.id = QStringLiteral("kimi");
    HarnessRegistry::self()->replaceDescriptorsForTest({claude, kimi});
}

// The F60 gate. Kimi declares no usage reporting: its result usage is a
// cumulative context readout, and two snapshots must land as the LATEST one —
// never as their sum, which is the quadratic growth F19b killed in AgentPanel.
void AiInspectorPanelTest::aCumulativeReadoutIsNeverSummed()
{
    CoreClient core;
    AiInspectorPanel panel(&core);
    panel.setActiveThread(QStringLiteral("t-kimi"));
    // The backend resolution RPC cannot land (no core here); state the engine
    // through the seam the resolution callback itself uses.
    panel.setThreadBackend(QStringLiteral("kimi"));

    Q_EMIT core.notification(QStringLiteral("agent.event"),
                             threadEvent(QStringLiteral("t-kimi"),
                                         resultEvent(100, 10, 20, 5)));
    Q_EMIT core.notification(QStringLiteral("agent.event"),
                             threadEvent(QStringLiteral("t-kimi"),
                                         resultEvent(150, 15, 20, 5)));

    const QString totals = totalsText(&panel);
    // Latest snapshot: 150 + 20 + 5 prompt-side tokens. The sum would be 300.
    QVERIFY2(totals.contains(QStringLiteral("175")),
             qPrintable(QStringLiteral("latest readout missing: ") + totals));
    QVERIFY2(!totals.contains(QStringLiteral("300")),
             qPrintable(QStringLiteral("cumulative readout was summed: ") + totals));
    // And it is labelled as what it is — a context readout, not an in/out bill.
    QVERIFY2(totals.contains(QStringLiteral("context")), qPrintable(totals));
}

// The other half of the gate: an engine that bills per turn keeps summing.
void AiInspectorPanelTest::aPerTurnSpendAccumulates()
{
    CoreClient core;
    AiInspectorPanel panel(&core);
    panel.setActiveThread(QStringLiteral("t-claude"));
    panel.setThreadBackend(QStringLiteral("claude"));

    Q_EMIT core.notification(QStringLiteral("agent.event"),
                             threadEvent(QStringLiteral("t-claude"),
                                         resultEvent(100, 10, 0, 0)));
    Q_EMIT core.notification(QStringLiteral("agent.event"),
                             threadEvent(QStringLiteral("t-claude"),
                                         resultEvent(150, 15, 0, 0)));

    const QString totals = totalsText(&panel);
    QVERIFY2(totals.contains(QStringLiteral("in 250")),
             qPrintable(QStringLiteral("per-turn spend was not summed: ") + totals));
    QVERIFY2(totals.contains(QStringLiteral("out 25")), qPrintable(totals));
}

// F60(b): the tool_use → row lookup is consumed by the result that resolves it.
// A second result for the same id must find nothing — the observable face of
// the map no longer holding one entry per call ever made.
void AiInspectorPanelTest::aResolvedToolCallIsLookedUpOnlyOnce()
{
    CoreClient core;
    AiInspectorPanel panel(&core);
    const QString tid = QStringLiteral("t-rows");
    panel.setActiveThread(tid);

    Q_EMIT core.notification(
        QStringLiteral("agent.event"),
        threadEvent(tid, toolUseEvent(QStringLiteral("tu-1"), QStringLiteral("Read"))));
    Q_EMIT core.notification(
        QStringLiteral("agent.event"),
        threadEvent(tid, toolResultEvent(QStringLiteral("tu-1"), QStringLiteral("hello"))));

    QTreeWidget *tree = timelineTree(&panel);
    QVERIFY(tree != nullptr);
    QCOMPARE(tree->topLevelItemCount(), 1);
    const QString resolved = tree->topLevelItem(0)->text(2);
    QVERIFY(!resolved.isEmpty());

    // A duplicate result must not find the row again.
    Q_EMIT core.notification(
        QStringLiteral("agent.event"),
        threadEvent(tid, toolResultEvent(QStringLiteral("tu-1"),
                                         QStringLiteral("a much longer second result"))));
    QCOMPARE(tree->topLevelItem(0)->text(2), resolved);
}

// F60(c): the per-thread timeline is a ring with the same cap as the
// all-threads feed, and an evicted row's pending-result lookup goes with it —
// a result for an evicted id must land nowhere (not on freed memory).
void AiInspectorPanelTest::theTimelineIsABoundedRing()
{
    CoreClient core;
    AiInspectorPanel panel(&core);
    const QString tid = QStringLiteral("t-ring");
    panel.setActiveThread(tid);

    for (int i = 0; i < 520; ++i) {
        Q_EMIT core.notification(
            QStringLiteral("agent.event"),
            threadEvent(tid, toolUseEvent(QStringLiteral("tu-%1").arg(i),
                                          QStringLiteral("tool-%1").arg(i))));
    }

    QTreeWidget *tree = timelineTree(&panel);
    QVERIFY(tree != nullptr);
    QCOMPARE(tree->topLevelItemCount(), 500);
    // The ring dropped the OLDEST rows.
    QCOMPARE(tree->topLevelItem(0)->text(0), QStringLiteral("tool-20"));

    // A result for an evicted call: its lookup was purged with the row, so
    // nothing on screen may change.
    Q_EMIT core.notification(
        QStringLiteral("agent.event"),
        threadEvent(tid, toolResultEvent(QStringLiteral("tu-0"),
                                         QStringLiteral("late result"))));
    QCOMPARE(tree->topLevelItemCount(), 500);
    QCOMPARE(tree->topLevelItem(0)->text(0), QStringLiteral("tool-20"));
}

QTEST_MAIN(AiInspectorPanelTest)
#include "AiInspectorPanelTest.moc"
