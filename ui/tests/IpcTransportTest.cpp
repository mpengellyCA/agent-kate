#include "ipc/FrameReader.h"
#include "ipc/SocketPath.h"

#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QTemporaryDir>
#include <QTest>

#include <sys/stat.h>
#include <unistd.h>

// Gates for the two transport bounds the core also enforces:
//
//  * the socket path the UI hands akcore must satisfy ipc.assertPrivateDir, or
//    the core refuses to bind and the app never starts (the round-8 regression);
//  * the inbound frame buffer must be capped, or a single unterminated frame
//    grows the GUI process without bound (audit F10).
class IpcTransportTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    // Each path test rewrites XDG_RUNTIME_DIR/TMPDIR; put the process's real
    // ones back afterwards so no test depends on the order it ran in.
    void init()
    {
        m_runtime = qgetenv("XDG_RUNTIME_DIR");
        m_tmpdir = qgetenv("TMPDIR");
    }
    void cleanup()
    {
        m_runtime.isEmpty() ? qunsetenv("XDG_RUNTIME_DIR")
                            : qputenv("XDG_RUNTIME_DIR", m_runtime);
        m_tmpdir.isEmpty() ? qunsetenv("TMPDIR") : qputenv("TMPDIR", m_tmpdir);
    }

    void privateDirRules();
    void picksRuntimeDirWhenPrivate();
    void createsPrivateFallbackWhenNoRuntimeDir();
    void skipsRuntimeDirThatIsNotPrivate();
    void skipsDirectoriesThatWouldOverflowSunPath();
    void everyReturnedPathSatisfiesTheCoreRules();

    void splitsFrames();
    void skipsBlankLines();
    void dropsOversizeFrameKeepsFollowing();
    void unterminatedOversizeFrameDoesNotGrowBuffer();
    void clearForgetsDropInProgress();

private:
    QByteArray m_runtime;
    QByteArray m_tmpdir;
};

namespace {

// The four rules core/internal/ipc/server.go assertPrivateDir applies, restated
// here so the test asserts the CONTRACT and not the implementation.
bool coreWouldAcceptDir(const QString &dir)
{
    struct stat st {};
    if (::lstat(QFile::encodeName(dir).constData(), &st) != 0) {
        return false;
    }
    return S_ISDIR(st.st_mode) && st.st_uid == ::getuid()
        && (st.st_mode & (S_IRWXG | S_IRWXO)) == 0;
}

} // namespace

void IpcTransportTest::privateDirRules()
{
    QTemporaryDir tmp;
    QVERIFY(tmp.isValid());

    const QString priv = tmp.filePath(QStringLiteral("priv"));
    QVERIFY(::mkdir(QFile::encodeName(priv).constData(), 0700) == 0);
    QVERIFY(akipc::isPrivateDir(priv));

    // Group/other bits — the exact thing the core refuses.
    const QString loose = tmp.filePath(QStringLiteral("loose"));
    QVERIFY(::mkdir(QFile::encodeName(loose).constData(), 0755) == 0);
    QVERIFY(!akipc::isPrivateDir(loose));

    // A symlink to a private directory is still not a directory we may bind in:
    // its target can move under us between the check and the bind.
    const QString link = tmp.filePath(QStringLiteral("link"));
    QVERIFY(QFile::link(priv, link));
    QVERIFY(!akipc::isPrivateDir(link));

    // Missing, and a plain file, both answer "not private" rather than throw.
    QVERIFY(!akipc::isPrivateDir(tmp.filePath(QStringLiteral("nope"))));
    const QString file = tmp.filePath(QStringLiteral("file"));
    QFile f(file);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.close();
    QVERIFY(!akipc::isPrivateDir(file));

    QVERIFY(akipc::isPrivateDir(QString()) == false);

    // ensurePrivateDir creates 0700 and never "fixes" a loose one it finds.
    const QString made = tmp.filePath(QStringLiteral("made"));
    QVERIFY(akipc::ensurePrivateDir(made));
    QVERIFY(akipc::isPrivateDir(made));
    QVERIFY(!akipc::ensurePrivateDir(loose));
}

void IpcTransportTest::picksRuntimeDirWhenPrivate()
{
    QTemporaryDir tmp;
    QVERIFY(tmp.isValid());
    const QString runtime = tmp.filePath(QStringLiteral("rt"));
    QVERIFY(::mkdir(QFile::encodeName(runtime).constData(), 0700) == 0);

    qputenv("XDG_RUNTIME_DIR", QFile::encodeName(runtime));
    QString err;
    const QString path = akipc::privateSocketPath(&err);
    qunsetenv("XDG_RUNTIME_DIR");

    QVERIFY2(!path.isEmpty(), qPrintable(err));
    QCOMPARE(QFileInfo(path).absolutePath(), QFileInfo(runtime).absoluteFilePath());
}

void IpcTransportTest::createsPrivateFallbackWhenNoRuntimeDir()
{
    QTemporaryDir tmp;
    QVERIFY(tmp.isValid());
    qunsetenv("XDG_RUNTIME_DIR");
    qputenv("TMPDIR", QFile::encodeName(tmp.path()));

    QString err;
    const QString path = akipc::privateSocketPath(&err);
    qunsetenv("TMPDIR");

    QVERIFY2(!path.isEmpty(), qPrintable(err));
    const QString dir = QFileInfo(path).absolutePath();
    // Not the bare temp root: a private subdirectory the helper made itself.
    QVERIFY(dir != QFileInfo(tmp.path()).absoluteFilePath());
    QVERIFY(coreWouldAcceptDir(dir));
}

void IpcTransportTest::skipsRuntimeDirThatIsNotPrivate()
{
    QTemporaryDir tmp;
    QVERIFY(tmp.isValid());
    const QString loose = tmp.filePath(QStringLiteral("loose-rt"));
    QVERIFY(::mkdir(QFile::encodeName(loose).constData(), 0755) == 0);

    qputenv("XDG_RUNTIME_DIR", QFile::encodeName(loose));
    qputenv("TMPDIR", QFile::encodeName(tmp.path()));
    QString err;
    const QString path = akipc::privateSocketPath(&err);
    qunsetenv("XDG_RUNTIME_DIR");
    qunsetenv("TMPDIR");

    QVERIFY2(!path.isEmpty(), qPrintable(err));
    // Fell through to the fallback rather than handing the core a path it would
    // refuse to bind.
    QVERIFY(!path.startsWith(loose + QLatin1Char('/')));
    QVERIFY(coreWouldAcceptDir(QFileInfo(path).absolutePath()));
}

void IpcTransportTest::skipsDirectoriesThatWouldOverflowSunPath()
{
    QTemporaryDir tmp;
    QVERIFY(tmp.isValid());
    // A runtime dir whose path alone is nearly the whole sun_path budget: the
    // directory is perfectly private, but no socket fits inside it.
    QString deep = tmp.path();
    while (QFile::encodeName(deep).size() < akipc::kMaxUnixSocketPath - 12) {
        deep += QStringLiteral("/dddddddddd");
        if (::mkdir(QFile::encodeName(deep).constData(), 0700) != 0) {
            break;
        }
    }
    QVERIFY(akipc::isPrivateDir(deep));

    qputenv("XDG_RUNTIME_DIR", QFile::encodeName(deep));
    qputenv("TMPDIR", QFile::encodeName(deep));
    QString err;
    const QString path = akipc::privateSocketPath(&err);
    qunsetenv("XDG_RUNTIME_DIR");
    qunsetenv("TMPDIR");

    // Either it found the short /tmp candidate, or it refused outright — never
    // a path the kernel would reject with ENAMETOOLONG.
    if (path.isEmpty()) {
        QVERIFY(!err.isEmpty());
    } else {
        QVERIFY(QFile::encodeName(path).size() <= akipc::kMaxUnixSocketPath);
        QVERIFY(coreWouldAcceptDir(QFileInfo(path).absolutePath()));
    }
}

void IpcTransportTest::everyReturnedPathSatisfiesTheCoreRules()
{
    // The real environment, unmodified: whatever this box hands us, the path we
    // would pass to `akcore --socket` must pass the core's own gate.
    QString err;
    const QString path = akipc::privateSocketPath(&err);
    QVERIFY2(!path.isEmpty(), qPrintable(err));
    QVERIFY(QFile::encodeName(path).size() <= akipc::kMaxUnixSocketPath);
    QVERIFY(coreWouldAcceptDir(QFileInfo(path).absolutePath()));
}

void IpcTransportTest::splitsFrames()
{
    akipc::FrameReader r;
    r.append(QByteArray("{\"a\":1}\n{\"b\":"));
    akipc::FrameReader::Frame f;
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray("{\"a\":1}"));
    QCOMPARE(f.oversize, 0LL);
    QVERIFY(!r.next(&f)); // second frame is still partial
    r.append(QByteArray("2}\n"));
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray("{\"b\":2}"));
    QVERIFY(!r.next(&f));
}

void IpcTransportTest::skipsBlankLines()
{
    akipc::FrameReader r;
    r.append(QByteArray("\n  \n{\"a\":1}\n"));
    akipc::FrameReader::Frame f;
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray("{\"a\":1}"));
    QVERIFY(!r.next(&f));
}

void IpcTransportTest::dropsOversizeFrameKeepsFollowing()
{
    akipc::FrameReader r;
    QByteArray big = QByteArray("{\"id\":42,\"result\":\"");
    big.append(QByteArray(akipc::kMaxInboundFrameBytes + 1024, 'x'));
    big.append("\"}\n");
    r.append(big);
    r.append(QByteArray("{\"a\":1}\n"));

    akipc::FrameReader::Frame f;
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray());
    QVERIFY(f.oversize >= akipc::kMaxInboundFrameBytes);
    // The head is retained so the pending call can be failed rather than leaked.
    QVERIFY(f.probe.contains("\"id\":42"));
    QVERIFY(f.probe.size() <= akipc::kIdProbeBytes);

    // The connection survives: the very next frame still parses.
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray("{\"a\":1}"));
    QCOMPARE(f.oversize, 0LL);
}

void IpcTransportTest::unterminatedOversizeFrameDoesNotGrowBuffer()
{
    // The DoS shape: bytes with no newline, forever. Before the cap this grew
    // m_buf without bound.
    akipc::FrameReader r;
    const QByteArray chunk(1 << 20, 'x');
    for (int i = 0; i < 64; ++i) { // 64 MiB, four times the cap
        r.append(chunk);
        akipc::FrameReader::Frame f;
        while (r.next(&f)) {
            // A discard is only reported once its terminating newline arrives;
            // none does here, so nothing should come out.
            QFAIL("unterminated frame must not yield a frame");
        }
        QVERIFY(r.buffered() <= akipc::kMaxInboundFrameBytes + chunk.size());
    }
    // Once the line finally ends, the discard is reported and the stream resyncs.
    r.append(QByteArray("\n{\"a\":1}\n"));
    akipc::FrameReader::Frame f;
    QVERIFY(r.next(&f));
    QVERIFY(f.oversize > akipc::kMaxInboundFrameBytes);
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray("{\"a\":1}"));
}

void IpcTransportTest::clearForgetsDropInProgress()
{
    akipc::FrameReader r;
    r.append(QByteArray(akipc::kMaxInboundFrameBytes + 16, 'x'));
    akipc::FrameReader::Frame f;
    QVERIFY(!r.next(&f)); // now discarding, waiting for the newline
    r.clear();            // connection dropped

    // The next connection's first frame must not be eaten by the old discard.
    r.append(QByteArray("{\"a\":1}\n"));
    QVERIFY(r.next(&f));
    QCOMPARE(f.line, QByteArray("{\"a\":1}"));
    QCOMPARE(f.oversize, 0LL);
}

QTEST_MAIN(IpcTransportTest)
#include "IpcTransportTest.moc"
