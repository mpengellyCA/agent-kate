// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorkflowMonitor.h"

#include <QFile>
#include <QTemporaryDir>
#include <QTest>

// Audit F59: WorkflowMonitor used to re-open and line-parse the WHOLE journal
// on every 1.5 s poll. The fix follows the file incrementally — a byte offset
// plus carried parse state — so each poll touches only appended bytes. These
// tests pin the two properties that make that correct:
//   * appended-bytes-only: bytes before the offset are never re-parsed, even
//     when they change underneath us;
//   * shrink reset: a truncated/rotated journal drops all derived state
//     instead of blending two files' histories.
class WorkflowMonitorTest : public QObject
{
    Q_OBJECT

private Q_SLOTS:
    void appendedBytesOnly();
    void shrinkResetsState();
    void partialLineCarriedAcrossPolls();

private:
    static void appendTo(const QString &path, const QByteArray &bytes)
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly | QIODevice::Append));
        QCOMPARE(f.write(bytes), qint64(bytes.size()));
    }
};

void WorkflowMonitorTest::appendedBytesOnly()
{
    QTemporaryDir dir;
    QVERIFY(dir.isValid());
    const QString path = dir.filePath(QStringLiteral("journal.jsonl"));

    const QByteArray lineA = "{\"type\":\"started\",\"agentId\":\"agent-aaaa\"}\n";
    appendTo(path, lineA);

    WorkflowMonitor::JournalState st;
    QVERIFY(!WorkflowMonitor::pollJournal(st, path));
    QCOMPARE(st.order, QVector<QString>{QStringLiteral("agent-aaaa")});
    QCOMPARE(st.offset, qint64(lineA.size()));

    // Rewrite the ALREADY-CONSUMED first line in place (same length, so the
    // file never shrinks), then append a new one. Only the appended bytes may
    // be parsed: a full re-read would resurrect "agent-zzzz".
    const QByteArray lineZ = "{\"type\":\"started\",\"agentId\":\"agent-zzzz\"}\n";
    QCOMPARE(lineZ.size(), lineA.size());
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::ReadWrite));
        QCOMPARE(f.write(lineZ), qint64(lineZ.size()));
    }
    appendTo(path, "{\"type\":\"started\",\"agentId\":\"agent-bbbb\"}\n");

    QVERIFY(!WorkflowMonitor::pollJournal(st, path));
    const QVector<QString> want{QStringLiteral("agent-aaaa"),
                                QStringLiteral("agent-bbbb")};
    QCOMPARE(st.order, want);
    QVERIFY(!st.done.contains(QStringLiteral("agent-zzzz")));
}

void WorkflowMonitorTest::shrinkResetsState()
{
    QTemporaryDir dir;
    QVERIFY(dir.isValid());
    const QString path = dir.filePath(QStringLiteral("journal.jsonl"));

    appendTo(path,
             "{\"type\":\"started\",\"agentId\":\"agent-aaaa\"}\n"
             "{\"type\":\"result\",\"agentId\":\"agent-aaaa\"}\n"
             "{\"type\":\"started\",\"agentId\":\"agent-bbbb\"}\n");

    WorkflowMonitor::JournalState st;
    QVERIFY(!WorkflowMonitor::pollJournal(st, path));
    QCOMPARE(st.order.size(), 2);
    QVERIFY(st.done.value(QStringLiteral("agent-aaaa")));

    // Truncate and start a new, shorter journal: everything derived from the
    // old bytes must go — the new run's single agent is the whole state.
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly | QIODevice::Truncate));
        f.write("{\"type\":\"started\",\"agentId\":\"agent-cccc\"}\n");
    }

    QVERIFY(WorkflowMonitor::pollJournal(st, path));
    QCOMPARE(st.order, QVector<QString>{QStringLiteral("agent-cccc")});
    QVERIFY(!st.done.contains(QStringLiteral("agent-aaaa")));
    QVERIFY(!st.done.contains(QStringLiteral("agent-bbbb")));
}

void WorkflowMonitorTest::partialLineCarriedAcrossPolls()
{
    QTemporaryDir dir;
    QVERIFY(dir.isValid());
    const QString path = dir.filePath(QStringLiteral("journal.jsonl"));

    // A poll that lands mid-write sees half a record: it must not be parsed
    // (or lost) — it completes on the next poll.
    const QByteArray full = "{\"type\":\"result\",\"agentId\":\"agent-aaaa\"}\n";
    appendTo(path, full.left(20));

    WorkflowMonitor::JournalState st;
    QVERIFY(!WorkflowMonitor::pollJournal(st, path));
    QVERIFY(st.order.isEmpty());

    appendTo(path, full.mid(20));
    QVERIFY(!WorkflowMonitor::pollJournal(st, path));
    QCOMPARE(st.order, QVector<QString>{QStringLiteral("agent-aaaa")});
    QVERIFY(st.done.value(QStringLiteral("agent-aaaa")));
}

QTEST_GUILESS_MAIN(WorkflowMonitorTest)
#include "WorkflowMonitorTest.moc"
