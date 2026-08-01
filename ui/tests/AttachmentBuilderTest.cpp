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

#include <QDateTime>
#include <QElapsedTimer>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QImage>
#include <QJsonArray>
#include <QJsonDocument>
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
    void ingestsRawImages();
    void rawImagesShareTheBudget();
    void prunesCacheByAge();
    void prunesCacheToSizeCap();
    void neverPrunesRecentCacheFiles();
    void prunesDurableDirOnlyWhenOverCap();
    void resolvesStaleOriginToCache();
    void resolvesCopyMovedBetweenStoreDirs();
    void reportsUnattachableInput();
    void refusesHugeImageWithoutReadingIt();
    void truncatesHugeTextWithoutReadingIt();
    void refusesExcerptBeyondScanCap();

private:
    // Write a file of `bytes` length under the temp dir and return its path.
    QString makeFile(const QString &name, int bytes, char fill = 'x');
    // A file of `header` followed by a sparse hole out to `total` bytes. Sparse
    // so the test can name a multi-gigabyte file without writing one: the point
    // is what the builder READS, and QFileInfo::size() reports the full length.
    // Empty return means the filesystem would not do it — skip, don't fail.
    QString makeSparseFile(const QString &name, const QByteArray &header, qint64 total);
    // Write a file of `bytes` length into `dir`, backdated `ageSecs` seconds.
    QString makeAgedFile(const QString &dir, const QString &name, int bytes,
                         qint64 ageSecs);

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

QString AttachmentBuilderTest::makeSparseFile(const QString &name,
                                              const QByteArray &header, qint64 total)
{
    const QString path = m_dir.filePath(name);
    QFile f(path);
    if (!f.open(QIODevice::WriteOnly)) {
        return QString();
    }
    if (f.write(header) != header.size() || !f.flush() || !f.resize(total)) {
        f.close();
        QFile::remove(path);
        return QString();
    }
    f.close();
    return QFileInfo(path).size() == total ? path : QString();
}

QString AttachmentBuilderTest::makeAgedFile(const QString &dir, const QString &name,
                                            int bytes, qint64 ageSecs)
{
    QDir().mkpath(dir);
    const QString path = dir + QLatin1Char('/') + name;
    QFile f(path);
    if (!f.open(QIODevice::WriteOnly)) {
        return QString();
    }
    f.write(QByteArray(bytes, 'x'));
    // The handle must still be open: setFileTime on a closed QFile has no fd.
    // But the write is buffered, so it must be flushed here — otherwise close()
    // drains it after the stamp and re-dates the file to now.
    f.flush();
    const bool stamped = f.setFileTime(QDateTime::currentDateTime().addSecs(-ageSecs),
                                       QFileDevice::FileModificationTime);
    f.close();
    return stamped ? path : QString();
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
    // The copy is NOT derived data, however file-shaped its origin looked: it
    // exists precisely for the case this test goes on to exercise, the origin
    // being deleted. A cache-class dir would delete it after a fortnight and
    // leave the card with a chip that cannot be opened, so it is user data.
    QVERIFY2(cached.startsWith(
                 QStandardPaths::writableLocation(QStandardPaths::AppDataLocation)),
             "an attachment copy outlives its origin, so it is durable user data");
    QVERIFY2(!cached.startsWith(
                 QStandardPaths::writableLocation(QStandardPaths::CacheLocation)),
             "and must be out of reach of the cache-class sweep");

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

void AttachmentBuilderTest::ingestsRawImages()
{
    // A clipboard paste or a browser drag hands over PIXELS — there is no file
    // anywhere on disk. The bytes still have to end up in the durable cache dir,
    // because everything downstream (the chip thumbnail, chip-open, the You card
    // whose body is stripped) is path-keyed.
    QImage image(64, 48, QImage::Format_ARGB32);
    image.fill(Qt::red);

    QJsonArray atts;
    const QStringList skipped = agentkate::buildImageAttachments({image}, atts);
    QVERIFY(skipped.isEmpty());
    QCOMPARE(atts.size(), 1);

    const QJsonObject a = atts.at(0).toObject();
    QCOMPARE(a.value(QStringLiteral("kind")).toString(), QStringLiteral("image"));
    QCOMPARE(a.value(QStringLiteral("mediaType")).toString(), QStringLiteral("image/png"));

    const QByteArray raw =
        QByteArray::fromBase64(a.value(QStringLiteral("dataB64")).toString().toLatin1());
    QVERIFY2(raw.startsWith(QByteArray("\x89PNG\r\n\x1a\n", 8)), "must be encoded as PNG");
    QImage decoded;
    QVERIFY(decoded.loadFromData(raw));
    QCOMPARE(decoded.size(), image.size());

    // The stored copy exists, is byte-identical, and — unlike a file attachment
    // — IS the path, since there is no origin to preserve.
    const QString cached = a.value(QStringLiteral("cachePath")).toString();
    QVERIFY2(!cached.isEmpty(), "a raw image must be written to disk");
    QVERIFY(QFileInfo::exists(cached));
    QCOMPARE(a.value(QStringLiteral("path")).toString(), cached);
    // And it must NOT be in the cache: for pasted pixels this file is the only
    // copy in existence (compactAttachments strips dataB64 once sent), so the
    // prune sweep — which treats every cache dir as reclaimable — would be
    // deleting the user's screenshot, by age or under the size cap.
    const QString appData =
        QStandardPaths::writableLocation(QStandardPaths::AppDataLocation);
    QVERIFY(!appData.isEmpty());
    QVERIFY2(cached.startsWith(appData),
             "a pasted image is the only copy — it must live in durable app data");
    QVERIFY2(!cached.startsWith(
                 QStandardPaths::writableLocation(QStandardPaths::CacheLocation)),
             "a pasted image must be out of reach of the prune sweep");
    QFile c(cached);
    QVERIFY(c.open(QIODevice::ReadOnly));
    QCOMPARE(c.readAll(), raw);

    // Well inside the budget, and the same pixels pasted twice attach once —
    // the cache name is content-addressed, so a second copy would be a lie.
    QVERIFY(QJsonDocument(a).toJson(QJsonDocument::Compact).size() < 12 * 1024 * 1024);
    QVERIFY(agentkate::buildImageAttachments({image}, atts).isEmpty());
    QCOMPARE(atts.size(), 1);
}

void AttachmentBuilderTest::rawImagesShareTheBudget()
{
    // Pasted pixels ride the same frame as everything else the message carries,
    // so they are measured against what is already in the array — not zero.
    QJsonArray atts{
        QJsonObject{{QStringLiteral("kind"), QStringLiteral("text")},
                    {QStringLiteral("name"), QStringLiteral("seed.txt")},
                    {QStringLiteral("text"), QString(12 * 1024 * 1024 - 1024, u'a')}}};

    // Noise, so the PNG encoder cannot compress it down to nothing.
    QImage image(256, 256, QImage::Format_ARGB32);
    for (int y = 0; y < image.height(); ++y) {
        for (int x = 0; x < image.width(); ++x) {
            image.setPixel(x, y, uint((x * 2654435761u) ^ (y * 40503u)));
        }
    }

    const QStringList skipped = agentkate::buildImageAttachments({image}, atts);
    QCOMPARE(atts.size(), 1); // nothing appended
    QVERIFY2(!skipped.isEmpty(), "an over-budget image must be reported, not dropped silently");
    QVERIFY(skipped.first().contains(QStringLiteral("attachment limit")));
}

// Both image cache dirs were append-only: every attached screenshot and every
// tool-result image stayed forever. These cover the three rules the sweep runs
// on — the age sweep, the size cap, and the floor that outranks both.
void AttachmentBuilderTest::prunesCacheByAge()
{
    const QString dir = m_dir.filePath(QStringLiteral("prune-age"));
    const QString old1 = makeAgedFile(dir, QStringLiteral("old1.png"), 1024, 20 * 86400);
    const QString old2 = makeAgedFile(dir, QStringLiteral("old2.png"), 1024, 15 * 86400);
    const QString keep = makeAgedFile(dir, QStringLiteral("keep.png"), 1024, 3 * 86400);
    QVERIFY(!old1.isEmpty() && !old2.isEmpty() && !keep.isEmpty());

    // Roomy cap, so only the age rule can fire.
    QCOMPARE(agentkate::pruneCacheDir(dir, 14 * 86400, 100 * 1024 * 1024), 2);
    QVERIFY(!QFileInfo::exists(old1));
    QVERIFY(!QFileInfo::exists(old2));
    QVERIFY(QFileInfo::exists(keep));

    // Idempotent: a second sweep of a clean dir deletes nothing.
    QCOMPARE(agentkate::pruneCacheDir(dir, 14 * 86400, 100 * 1024 * 1024), 0);
}

void AttachmentBuilderTest::prunesCacheToSizeCap()
{
    const QString dir = m_dir.filePath(QStringLiteral("prune-size"));
    // All within the age limit, so only the cap can delete them; 4 KB total
    // against a 2 KB cap must give up the two oldest, oldest first.
    const QString a = makeAgedFile(dir, QStringLiteral("a.png"), 1024, 5 * 86400);
    const QString b = makeAgedFile(dir, QStringLiteral("b.png"), 1024, 4 * 86400);
    const QString c = makeAgedFile(dir, QStringLiteral("c.png"), 1024, 3 * 86400);
    const QString d = makeAgedFile(dir, QStringLiteral("d.png"), 1024, 2 * 86400);
    QVERIFY(!a.isEmpty() && !b.isEmpty() && !c.isEmpty() && !d.isEmpty());

    QCOMPARE(agentkate::pruneCacheDir(dir, 14 * 86400, 2048), 2);
    QVERIFY2(!QFileInfo::exists(a), "the oldest file must go first");
    QVERIFY(!QFileInfo::exists(b));
    QVERIFY2(QFileInfo::exists(c), "the cap must stop deleting once it fits");
    QVERIFY(QFileInfo::exists(d));
}

void AttachmentBuilderTest::neverPrunesRecentCacheFiles()
{
    const QString dir = m_dir.filePath(QStringLiteral("prune-floor"));
    // A file minutes old may be the thumbnail a visible card is about to redraw,
    // or a .tmp mid-rename. No rule may take it — not the age rule (a skewed
    // clock can make a fresh file look ancient) and not the cap.
    const QString fresh = makeAgedFile(dir, QStringLiteral("fresh.png"), 4096, 120);
    const QString skewed = makeAgedFile(dir, QStringLiteral("skewed.png"), 4096, 0);
    const QString stale = makeAgedFile(dir, QStringLiteral("stale.png"), 4096, 90 * 86400);
    QVERIFY(!fresh.isEmpty() && !skewed.isEmpty() && !stale.isEmpty());

    // Zero age limit and a zero cap: everything deletable, deleted.
    QCOMPARE(agentkate::pruneCacheDir(dir, 0, 0), 1);
    QVERIFY(QFileInfo::exists(fresh));
    QVERIFY(QFileInfo::exists(skewed));
    QVERIFY(!QFileInfo::exists(stale));

    // A missing directory is not an error — the dirs are created lazily.
    QCOMPARE(agentkate::pruneCacheDir(m_dir.filePath(QStringLiteral("nope")), 0, 0), 0);
}

// The attachment dir is user data, not cache: an attachment's copy is what makes
// it outlive its origin (and for pasted pixels it is the only copy there is), so
// age alone must never take one. Both rules have to agree.
void AttachmentBuilderTest::prunesDurableDirOnlyWhenOverCap()
{
    const QString dir = m_dir.filePath(QStringLiteral("prune-durable"));
    // Two years old — far past any age line — but the dir is tiny.
    const QString ancient = makeAgedFile(dir, QStringLiteral("ancient.png"), 1024,
                                         730 * 86400);
    const QString recent = makeAgedFile(dir, QStringLiteral("recent.png"), 1024,
                                        2 * 86400);
    QVERIFY(!ancient.isEmpty() && !recent.isEmpty());

    // Under the cap: the age rule alone gets nothing. This is the whole point of
    // the durable class — a screenshot pasted last year is still the user's.
    QCOMPARE(agentkate::pruneCacheDir(dir, 180 * 86400, 100 * 1024 * 1024,
                                      agentkate::PrunePolicy::Durable),
             0);
    QVERIFY(QFileInfo::exists(ancient));
    QVERIFY(QFileInfo::exists(recent));

    // Over the cap: now the oldest goes, but the sweep still stops at the age
    // line even though the dir is still over — a young file is never given up.
    QCOMPARE(agentkate::pruneCacheDir(dir, 180 * 86400, 512,
                                      agentkate::PrunePolicy::Durable),
             1);
    QVERIFY(!QFileInfo::exists(ancient));
    QVERIFY2(QFileInfo::exists(recent),
             "over the cap is not licence to delete a file that isn't old");

    // The same dir under the cache policy would have taken both on age alone —
    // which is exactly the bug this split exists to prevent.
    QCOMPARE(agentkate::pruneCacheDir(dir, 86400, 100 * 1024 * 1024), 1);
    QVERIFY(!QFileInfo::exists(recent));
}

// Capture tools reuse fixed names (/tmp/screenshot.png), so "the origin path
// exists" does not mean "the origin path still holds the bytes we sent". When
// it doesn't, the chip must show the cached copy, not the newer screenshot.
void AttachmentBuilderTest::resolvesStaleOriginToCache()
{
    const QString origin = makeFile(QStringLiteral("shot-fixed.png"), 2048);
    const QString cache = makeFile(QStringLiteral("shot-cached.png"), 2048);
    QVERIFY(!origin.isEmpty() && !cache.isEmpty());

    QJsonObject att{{QStringLiteral("kind"), QStringLiteral("image")},
                    {QStringLiteral("path"), origin},
                    {QStringLiteral("cachePath"), cache}};
    // Same size, inside the project: the origin is the copy worth opening.
    QCOMPARE(agentkate::resolveAttachmentPath(att), origin);

    // Outside the project is NOT on its own a reason to distrust the origin: an
    // untouched ~/Downloads image is still the file the user recognises, and the
    // content-addressed copy is an opaque name in a cache dir. Only the bytes
    // changing decides it — which is the same test run for an in-tree file.
    att[QStringLiteral("outside")] = true;
    QCOMPARE(agentkate::resolveAttachmentPath(att), origin);

    // Outside AND the size no longer matches: now the copy wins.
    QVERIFY(!makeFile(QStringLiteral("shot-fixed.png"), 3072).isEmpty());
    QCOMPARE(agentkate::resolveAttachmentPath(att), cache);
    att.remove(QStringLiteral("outside"));

    // Rewritten in place by the next capture: different size, so the cached
    // bytes are the ones the message actually carried.
    QVERIFY(!makeFile(QStringLiteral("shot-fixed.png"), 4096).isEmpty());
    QCOMPARE(agentkate::resolveAttachmentPath(att), cache);

    // Origin gone entirely: still the copy.
    QVERIFY(QFile::remove(origin));
    QCOMPARE(agentkate::resolveAttachmentPath(att), cache);

    // Neither survives: empty, so the caller reports it rather than opening
    // a path that isn't there.
    QVERIFY(QFile::remove(cache));
    QVERIFY(agentkate::resolveAttachmentPath(att).isEmpty());
}

// Copies used to be written to the cache dir and now live in app data. Cards
// from those sessions still record the old path, and the new store is empty of
// it — so a card that worked yesterday would show a broken chip. The recorded
// path is content-addressed, so the basename is enough to find the same bytes in
// whichever dir has them.
void AttachmentBuilderTest::resolvesCopyMovedBetweenStoreDirs()
{
    const QString legacyDir =
        QStandardPaths::writableLocation(QStandardPaths::CacheLocation)
        + QStringLiteral("/attachments");
    const QString storeDir =
        QStandardPaths::writableLocation(QStandardPaths::AppDataLocation)
        + QStringLiteral("/attachments");
    QVERIFY(QDir().mkpath(legacyDir));
    QVERIFY(QDir().mkpath(storeDir));

    const QString name = QStringLiteral("deadbeefcafe0001.png");
    QFile f(storeDir + QLatin1Char('/') + name);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(QByteArray(256, 'm'));
    f.close();

    // An old card: cachePath names the cache dir, which no longer has the file,
    // and the origin is long gone.
    QJsonObject att{{QStringLiteral("kind"), QStringLiteral("image")},
                    {QStringLiteral("path"), legacyDir + QLatin1Char('/') + name},
                    {QStringLiteral("cachePath"), legacyDir + QLatin1Char('/') + name}};
    QCOMPARE(agentkate::resolveAttachmentPath(att),
             storeDir + QLatin1Char('/') + name);

    // And the other direction, for a card written by a build that stored to app
    // data while the bytes only survive in the old dir.
    QVERIFY(QFile(storeDir + QLatin1Char('/') + name)
                .rename(legacyDir + QLatin1Char('/') + name));
    att[QStringLiteral("cachePath")] = storeDir + QLatin1Char('/') + name;
    att[QStringLiteral("path")] = storeDir + QLatin1Char('/') + name;
    QCOMPARE(agentkate::resolveAttachmentPath(att),
             legacyDir + QLatin1Char('/') + name);

    // Neither dir has it: empty, so the caller reports a missing file instead of
    // opening a path that isn't there.
    QVERIFY(QFile::remove(legacyDir + QLatin1Char('/') + name));
    QVERIFY(agentkate::resolveAttachmentPath(att).isEmpty());
}

// Every refusal has to come back as a reason. A builder that returns an empty
// skipped list has told the panel "nothing was offered", and the panel then says
// nothing at all — indistinguishable from a paste that never registered.
void AttachmentBuilderTest::reportsUnattachableInput()
{
    // A file URL whose file is gone: deleted since it was copied, or on a mount
    // that went away. Common enough that silence here reads as a broken paste.
    QJsonArray atts;
    QStringList skipped = agentkate::buildPathAttachments(
        {m_dir.filePath(QStringLiteral("ghost.txt"))}, QString(), atts);
    QVERIFY(atts.isEmpty());
    QCOMPARE(skipped.size(), 1);
    QVERIFY2(skipped.first().contains(QStringLiteral("ghost.txt")),
             "the reason must name the file, so it is actionable");

    // Pixels the platform handed over but Qt has no plugin to decode.
    skipped = agentkate::buildImageAttachments({QImage()}, atts);
    QVERIFY(atts.isEmpty());
    QCOMPARE(skipped.size(), 1);
}

// --- F11: the budget bounds what is ADMITTED; these bound what is READ -------
//
// Every path used to do file.readAll() before any size test, so dropping a
// multi-gigabyte log or ISO on the input allocated the whole thing in the GUI
// process just to discover it was too big — an OOM-kill of the UI from one
// drag, or from one "attach this file" an injected agent talked the user into.
//
// Three gigabytes, sparse: the file is that big to every size query and costs
// no disk. A regression here does not fail subtly — it tries to allocate 3 GB.
// The elapsed-time bound turns that into a failure rather than a hang, and the
// content assertions catch the other outcome (the allocation returning empty
// and an empty attachment being sent).
namespace {
constexpr qint64 kHuge = 3LL * 1024 * 1024 * 1024;
constexpr qint64 kSaneMs = 1000;

// Peak resident set in KiB, straight from the kernel. This is the finding's own
// wording made observable: "a 2 GB file must never be read into memory". Reading
// one touches every page, so the high-water mark moves by gigabytes; refusing it
// on QFileInfo::size() moves it by nothing. Zero means the platform has no
// /proc — the timing bound still applies there.
qint64 peakRssKb()
{
    QFile f(QStringLiteral("/proc/self/status"));
    if (!f.open(QIODevice::ReadOnly | QIODevice::Text)) {
        return 0;
    }
    const QList<QByteArray> lines = f.readAll().split('\n');
    for (const QByteArray &l : lines) {
        if (l.startsWith("VmHWM:")) {
            return QByteArray(l).replace("VmHWM:", "").replace("kB", "").trimmed().toLongLong();
        }
    }
    return 0;
}

// Assert the call did not pull the file in. 64 MiB of headroom covers the
// attachment itself, the JSON, and any allocator slack; a 3 GB read is 48x it.
//
// VmHWM is a HIGH-WATER mark, so it only ever reports the FIRST oversized read
// in a process — a later one hiding under an earlier peak reads as a delta of
// zero. That is why the elapsed bound is kept alongside it rather than replaced
// by it: between them, every one of these cases fails loudly if readAll returns.
void verifyNothingHuge(qint64 beforeKb, const char *what)
{
    const qint64 afterKb = peakRssKb();
    if (beforeKb == 0 || afterKb == 0) {
        return; // no /proc — the elapsed bound is the guard on this platform
    }
    QVERIFY2(afterKb - beforeKb < 64 * 1024,
             qPrintable(QStringLiteral("%1: peak RSS grew %2 MB — the file was read")
                            .arg(QString::fromLatin1(what))
                            .arg((afterKb - beforeKb) / 1024)));
}
} // namespace

void AttachmentBuilderTest::refusesHugeImageWithoutReadingIt()
{
    const QString path = makeSparseFile(QStringLiteral("huge.png"),
                                        QByteArray("\x89PNG\r\n\x1a\n", 8), kHuge);
    if (path.isEmpty()) {
        QSKIP("filesystem will not create a sparse file");
    }
    QJsonArray atts;
    const qint64 rssBefore = peakRssKb();
    QElapsedTimer t;
    t.start();
    const QStringList skipped = agentkate::buildPathAttachments({path}, QString(), atts);
    const qint64 ms = t.elapsed();
    verifyNothingHuge(rssBefore, "huge image");

    QVERIFY2(atts.isEmpty(), "a 3 GB image must be refused, not attached");
    QCOMPARE(skipped.size(), 1);
    QVERIFY2(skipped.first().contains(QStringLiteral("huge.png")),
             "the refusal must name the file");
    QVERIFY2(ms < kSaneMs,
             qPrintable(QStringLiteral("the file was read before it was refused (%1 ms)")
                            .arg(ms)));
}

void AttachmentBuilderTest::truncatesHugeTextWithoutReadingIt()
{
    // Real text well past the 256 KB text cap, then a sparse tail out to 3 GB.
    // The cap is what may be read; everything after it is unreachable by any
    // correct implementation, so the NULs out there never reach the NUL sniff.
    const QString path = makeSparseFile(QStringLiteral("huge.log"),
                                        QByteArray(512 * 1024, 'a'), kHuge);
    if (path.isEmpty()) {
        QSKIP("filesystem will not create a sparse file");
    }
    QJsonArray atts;
    const qint64 rssBefore = peakRssKb();
    QElapsedTimer t;
    t.start();
    const QStringList skipped = agentkate::buildPathAttachments({path}, QString(), atts);
    const qint64 ms = t.elapsed();
    verifyNothingHuge(rssBefore, "huge text");

    QVERIFY(skipped.isEmpty());
    QCOMPARE(atts.size(), 1);
    const QString text = atts.at(0).toObject().value(QStringLiteral("text")).toString();
    QVERIFY2(text.endsWith(QStringLiteral("(truncated)")),
             "the excess must be truncated and said so");
    // 256 KB of body plus the short truncation note, and nothing else.
    QVERIFY(text.size() >= 256 * 1024);
    QVERIFY(text.size() < 256 * 1024 + 64);
    QVERIFY2(ms < kSaneMs,
             qPrintable(QStringLiteral("the file was read before it was capped (%1 ms)")
                            .arg(ms)));
}

void AttachmentBuilderTest::refusesExcerptBeyondScanCap()
{
    // The ranged path is addressed by LINE, so it must walk from the start — but
    // only as far as its scan cap. A hit past that is a skip with a reason, not
    // a licence to pull gigabytes in to copy sixteen lines out.
    QByteArray header;
    while (header.size() < 16 * 1024) {
        header += "x\n";
    }
    const QString path = makeSparseFile(QStringLiteral("huge.txt"), header, kHuge);
    if (path.isEmpty()) {
        QSKIP("filesystem will not create a sparse file");
    }
    const QJsonArray items{QJsonObject{{QStringLiteral("path"), path},
                                       {QStringLiteral("line"), 100000}}};
    QJsonArray atts;
    QStringList wholeFile;
    const qint64 rssBefore = peakRssKb();
    QElapsedTimer t;
    t.start();
    const QStringList skipped = agentkate::buildItemAttachments(items, atts, wholeFile);
    const qint64 ms = t.elapsed();
    verifyNothingHuge(rssBefore, "huge excerpt");

    QVERIFY(atts.isEmpty());
    QCOMPARE(skipped.size(), 1);
    QVERIFY2(skipped.first().contains(QStringLiteral("huge.txt")),
             "a skipped excerpt must say which file and why");
    QVERIFY2(ms < kSaneMs,
             qPrintable(QStringLiteral("the file was read before it was refused (%1 ms)")
                            .arg(ms)));
}

QTEST_MAIN(AttachmentBuilderTest)
#include "AttachmentBuilderTest.moc"
