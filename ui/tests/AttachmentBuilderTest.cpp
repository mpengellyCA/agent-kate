// Plan 19: attaching a screenshot is routinely attaching a TEMP file, and the
// capture tool reaps it moments later. The bytes are safe (they are read and
// base64'd at attach time), but everything path-keyed afterwards is not: the
// You card redraws its chip thumbnail from a path, and clicking a chip opens
// one. So the builder keeps its own copy, and these assert the two properties
// that copy exists for:
//
//   * it survives the origin being deleted, and
//   * it does NOT displace the origin path, because for a real workspace file
//     that is the copy worth opening — edits there count.
//
// Also guards the total attachment budget. There was a per-image cap but no cap
// on the sum, against a hard 16 MB IPC frame limit whose overflow is not a
// clean error: the core's scanner stops and the UI connection is dropped. The
// budget spans the whole message — every attach action appends to one array and
// they all ride one frame — and it is measured on the serialised object, which
// is what the frame actually carries.

#include "AttachmentBuilder.h"

#include <QJsonArray>
#include <QJsonObject>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QtTest>

class AttachmentBuilderTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void cachesImageBytesAgainstOriginDeletion();
    void keepsOriginPathForWorkspaceFiles();
    void textAttachmentsAreNotCached();
    void repairsTruncatedCacheCopy();
    void enforcesTotalBudget();
    void budgetAccumulatesAcrossCalls();
    void boundsItemExcerpts();

private:
    // Write a file of `bytes` length under the temp dir and return its path.
    QString makeFile(const QString &name, int bytes, char fill = 'x');

    QTemporaryDir m_dir;
};

void AttachmentBuilderTest::initTestCase()
{
    QVERIFY(m_dir.isValid());
    // Keep the cache copies out of the real user cache.
    QStandardPaths::setTestModeEnabled(true);
}

QString AttachmentBuilderTest::makeFile(const QString &name, int bytes, char fill)
{
    const QString path = m_dir.filePath(name);
    QFile f(path);
    if (!f.open(QIODevice::WriteOnly)) {
        return QString();
    }
    f.write(QByteArray(bytes, fill));
    f.close();
    return path;
}

void AttachmentBuilderTest::cachesImageBytesAgainstOriginDeletion()
{
    // A PNG header so the extension isn't the only thing making it an image.
    const QString path = m_dir.filePath(QStringLiteral("shot.png"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    const QByteArray body = QByteArray("\x89PNG\r\n\x1a\n", 8) + QByteArray(4096, 'p');
    f.write(body);
    f.close();

    QJsonArray atts;
    const QStringList skipped = agentkate::buildPathAttachments({path}, QString(), atts);
    QVERIFY(skipped.isEmpty());
    QCOMPARE(atts.size(), 1);

    const QJsonObject a = atts.at(0).toObject();
    QCOMPARE(a.value(QStringLiteral("kind")).toString(), QStringLiteral("image"));
    const QString cached = a.value(QStringLiteral("cachePath")).toString();
    QVERIFY2(!cached.isEmpty(), "an image attachment must carry a durable copy");
    QVERIFY(QFileInfo::exists(cached));

    // The temp screenshot goes away, exactly as a capture tool would reap it.
    QVERIFY(QFile::remove(path));
    QVERIFY2(!QFileInfo::exists(a.value(QStringLiteral("path")).toString()),
             "origin should now be gone");
    QVERIFY2(QFileInfo::exists(cached), "the cached copy must outlive the origin");

    QFile c(cached);
    QVERIFY(c.open(QIODevice::ReadOnly));
    QCOMPARE(c.readAll(), body); // and be byte-identical, not a truncated write
}

void AttachmentBuilderTest::keepsOriginPathForWorkspaceFiles()
{
    const QString path = m_dir.filePath(QStringLiteral("in-tree.png"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(QByteArray("\x89PNG\r\n\x1a\n", 8) + QByteArray(64, 'q'));
    f.close();

    QJsonArray atts;
    agentkate::buildPathAttachments({path}, m_dir.path(), atts);
    QCOMPARE(atts.size(), 1);
    const QJsonObject a = atts.at(0).toObject();

    // The cache copy is ALONGSIDE the origin, never a replacement for it —
    // clicking this chip must still open the real file in the editor.
    QCOMPARE(a.value(QStringLiteral("path")).toString(), path);
    QVERIFY(a.value(QStringLiteral("cachePath")).toString() != path);
    // Inside the workspace root, so not flagged as outside.
    QVERIFY(!a.value(QStringLiteral("outside")).toBool());
}

void AttachmentBuilderTest::textAttachmentsAreNotCached()
{
    const QString path = m_dir.filePath(QStringLiteral("notes.txt"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("hello world\n");
    f.close();

    QJsonArray atts;
    agentkate::buildPathAttachments({path}, QString(), atts);
    QCOMPARE(atts.size(), 1);
    const QJsonObject a = atts.at(0).toObject();
    QCOMPARE(a.value(QStringLiteral("kind")).toString(), QStringLiteral("text"));
    // A text file's content is inlined in the attachment itself, so there is
    // nothing a copy would save — the chip has no thumbnail to redraw.
    QVERIFY(!a.contains(QStringLiteral("cachePath")));
    QVERIFY(a.value(QStringLiteral("text")).toString().contains(QStringLiteral("hello")));
}

void AttachmentBuilderTest::enforcesTotalBudget()
{
    // Four 4 MB images: each is under the 5 MB per-image cap, but base64 makes
    // the set ~21 MB — past the 12 MB budget and past the 16 MB frame limit.
    QStringList paths;
    for (int i = 0; i < 4; ++i) {
        paths << makeFile(QStringLiteral("big%1.png").arg(i), 4 * 1024 * 1024, 'z');
    }
    QJsonArray atts;
    const QStringList skipped = agentkate::buildPathAttachments(paths, QString(), atts);

    QVERIFY2(!skipped.isEmpty(), "over-budget files must be reported, not dropped silently");
    qsizetype total = 0;
    for (const QJsonValue &v : std::as_const(atts)) {
        total += v.toObject().value(QStringLiteral("dataB64")).toString().size();
    }
    QVERIFY2(total <= 12 * 1024 * 1024, "accepted attachments must fit the budget");
    QVERIFY2(atts.size() < paths.size(), "not everything can have been accepted");
    // The reason must name the file and the limit, so it is actionable.
    QVERIFY(skipped.first().contains(QStringLiteral("big")));
}

void AttachmentBuilderTest::repairsTruncatedCacheCopy()
{
    const QString path = m_dir.filePath(QStringLiteral("repair.png"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    const QByteArray body = QByteArray("\x89PNG\r\n\x1a\n", 8) + QByteArray(8192, 'r');
    f.write(body);
    f.close();

    QJsonArray first;
    agentkate::buildPathAttachments({path}, QString(), first);
    QCOMPARE(first.size(), 1);
    const QString cached = first.at(0).toObject().value(QStringLiteral("cachePath")).toString();
    QVERIFY(!cached.isEmpty());

    // Simulate the copy a crash (or a full disk) left half-written. The cache
    // name is content-addressed, so existence alone would poison this digest for
    // the life of the cache dir: every later attach of the same image would
    // reuse a file that draws as a broken thumbnail.
    {
        QFile c(cached);
        QVERIFY(c.open(QIODevice::WriteOnly | QIODevice::Truncate));
        c.write(body.left(16));
    }
    QCOMPARE(QFileInfo(cached).size(), qint64(16));

    QJsonArray second;
    agentkate::buildPathAttachments({path}, QString(), second);
    QCOMPARE(second.size(), 1);
    QCOMPARE(second.at(0).toObject().value(QStringLiteral("cachePath")).toString(), cached);

    QFile c(cached);
    QVERIFY(c.open(QIODevice::ReadOnly));
    QCOMPARE(c.readAll(), body); // rewritten, not reused on existence alone
}

void AttachmentBuilderTest::budgetAccumulatesAcrossCalls()
{
    // The budget belongs to the MESSAGE, not to one attach action: a drop, then
    // a file dialog, then another drop all append to the same array and share
    // the one frame. A per-call budget would let three 10 MB actions through.
    QStringList first;
    for (int i = 0; i < 3; ++i) {
        first << makeFile(QStringLiteral("acc%1.png").arg(i), 4 * 1024 * 1024, 'z');
    }
    QJsonArray atts;
    agentkate::buildPathAttachments(first, QString(), atts);
    QVERIFY2(atts.size() == 2, "two 4 MB images base64 to ~10.7 MB; a third cannot fit");

    const QString more = makeFile(QStringLiteral("acc-late.png"), 4 * 1024 * 1024, 'y');
    const QStringList skipped = agentkate::buildPathAttachments({more}, QString(), atts);
    QCOMPARE(atts.size(), 2); // nothing added by the second call
    QVERIFY2(!skipped.isEmpty(), "the second call must see the first call's total");
    QVERIFY(skipped.first().contains(QStringLiteral("acc-late")));
}

void AttachmentBuilderTest::boundsItemExcerpts()
{
    // A minified file is a single line that is the entire file, so "line 0 plus
    // eight lines of context" is the whole 2 MB — a line range bounds nothing.
    const QString path = m_dir.filePath(QStringLiteral("bundle.min.js"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(QByteArray(2 * 1024 * 1024, 'a'));
    f.close();

    const QJsonArray items{
        QJsonObject{{QStringLiteral("path"), path}, {QStringLiteral("line"), 0}}};
    QJsonArray atts;
    QStringList wholeFile;
    QStringList skipped = agentkate::buildItemAttachments(items, atts, wholeFile);
    QVERIFY(wholeFile.isEmpty());
    QVERIFY(skipped.isEmpty());
    QCOMPARE(atts.size(), 1);
    const QString text = atts.at(0).toObject().value(QStringLiteral("text")).toString();
    QVERIFY2(text.size() <= 256 * 1024 + 64,
             "the excerpt must be truncated, not inlined whole");
    QVERIFY(text.endsWith(QStringLiteral("(truncated)")));

    // And excerpts are budgeted against what the message already carries: with
    // the frame nearly full the next one is skipped with an actionable reason,
    // instead of pushing the send past the core's frame cap.
    QJsonArray full{
        QJsonObject{{QStringLiteral("kind"), QStringLiteral("text")},
                    {QStringLiteral("name"), QStringLiteral("seed.txt")},
                    {QStringLiteral("text"), QString(12 * 1024 * 1024 - 1024, u'a')}}};
    QStringList wholeFile2;
    skipped = agentkate::buildItemAttachments(items, full, wholeFile2);
    QCOMPARE(full.size(), 1); // nothing appended
    QVERIFY(!skipped.isEmpty());
    QVERIFY(skipped.first().contains(QStringLiteral("bundle.min.js")));
}

QTEST_MAIN(AttachmentBuilderTest)
#include "AttachmentBuilderTest.moc"
