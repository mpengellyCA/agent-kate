// Regression guard for the two model-content gates (audit F14, F15).
//
// F14: a markdown link in an assistant message is attacker-shapeable and its
// text says nothing about its target, so only http/https/mailto may reach the
// OS handler without a human decision.
// F15: rendered rich text resolves image names through a synchronous, unbounded
// local read — `![x](/dev/zero)` hung the GUI thread, and any absolute path
// rendered a private file into the transcript. Resources must come from an
// allowed root, be regular files, and be bounded.
//
// These are security gates: every case here asserts a REFUSAL. Refusal has two
// shapes and neither of them is "no bytes" (see SafeContent.h): an image comes
// back as a 1x1 transparent PNG, anything else as an empty-but-not-null
// QByteArray. The rendering tests at the bottom are why — they show that
// handing back nothing lets Qt load the file anyway.

#include "SafeContent.h"
#include "MarkdownUtil.h"

#include <QDir>
#include <QDirIterator>
#include <QFileInfo>
#include <QFile>
#include <QImage>
#include <QPainter>
#include <QRegularExpression>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QTextDocument>
#include <QUrl>
#include <QtTest>

using namespace agentkate;

namespace {
// ui/src, found from wherever the test binary was run (mirrors MarkdownUtilTest:
// the source scan at the bottom needs the tree, not the build dir).
QString uiSrcDir()
{
    for (const QString &from :
         {QCoreApplication::applicationDirPath(), QDir::currentPath()}) {
        QDir d(from);
        for (int i = 0; i < 8; ++i) {
            if (d.exists(QStringLiteral("ui/src/SafeContent.cpp"))) {
                return d.absoluteFilePath(QStringLiteral("ui/src"));
            }
            if (!d.cdUp()) {
                break;
            }
        }
    }
    const QFileInfo self(QString::fromUtf8(__FILE__));
    if (self.isAbsolute()) {
        QDir t = self.absoluteDir(); // ui/tests
        if (t.cdUp() && t.exists(QStringLiteral("src/SafeContent.cpp"))) {
            return t.absoluteFilePath(QStringLiteral("src"));
        }
    }
    return QString();
}

// Refused = "the file's bytes did not come back", in either refusal shape.
bool refused(const QVariant &v)
{
    if (!v.isValid() || v.typeId() != QMetaType::QByteArray) {
        return false;
    }
    const QByteArray b = v.toByteArray();
    if (b == blockedImageBytes()) {
        return true; // an image refusal: decodable, but shows nothing
    }
    return b.isEmpty() && !b.isNull();
}

// The colour of the file a test is trying to leak. Counting it in a rendered
// document is the only end-to-end way to ask "did Qt read that path?".
constexpr QRgb kSecret = qRgb(255, 0, 255);

// Write a solid kSecret PNG at `path`.
bool writeSecretPng(const QString &path)
{
    QImage img(40, 40, QImage::Format_ARGB32);
    img.fill(QColor::fromRgb(kSecret));
    return img.save(path, "PNG");
}

// Lay out `doc` and count the secret pixels it painted.
int secretPixels(QTextDocument &doc)
{
    doc.setTextWidth(300);
    QImage canvas(300, 200, QImage::Format_ARGB32);
    canvas.fill(Qt::white);
    QPainter p(&canvas);
    doc.drawContents(&p);
    p.end();
    int n = 0;
    for (int y = 0; y < canvas.height(); ++y) {
        for (int x = 0; x < canvas.width(); ++x) {
            if (canvas.pixel(x, y) == kSecret) {
                ++n;
            }
        }
    }
    return n;
}

// A document whose guard refuses by handing back NOTHING — the shape the F15
// remediation originally used. Kept here to prove it is not a refusal at all.
class EmptyBytesDoc : public QTextDocument
{
public:
    int calls = 0;

protected:
    QVariant loadResource(int type, const QUrl &name) override
    {
        Q_UNUSED(type);
        Q_UNUSED(name);
        ++calls;
        return QVariant(QByteArray(""));
    }
};
} // namespace

class SafeContentTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void schemePolicy_data();
    void schemePolicy();
    void servesFileUnderRoot();
    void refusesOutsideRoot();
    void refusesSpecialFile();
    void refusesOversizeFile();
    void refusesSymlinkEscape();
    void refusesRemoteScheme();
    void passesInlineSchemesThrough();
    void resolvesRelativeAgainstRootsOnly();
    void tailReadsIncrementally();
    void tailCapsOneRead();
    void tailSkipsForwardWhenOutrun();
    void tailRestartsOnTruncate();
    void tailRefusesSpecialAndMissingFiles();
    void blockedImageIsATransparentPixel();
    void emptyBytesIsNotARefusalForImages();
    void markdownImageOutsideRootsIsNotLoaded();
    void markdownImageUnderRootStillRenders();
    void guardedBrowserAlsoRefuses();
    void noUnguardedRenderedDocument();

private:
    QTemporaryDir m_root;
    QTemporaryDir m_outside;
};

void SafeContentTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    QVERIFY(m_root.isValid());
    QVERIFY(m_outside.isValid());
    allowMediaRoot(m_root.path());
}

void SafeContentTest::schemePolicy_data()
{
    QTest::addColumn<QString>("url");
    QTest::addColumn<bool>("safe");
    QTest::newRow("http") << QStringLiteral("http://example.com/x") << true;
    QTest::newRow("https") << QStringLiteral("https://example.com/x") << true;
    QTest::newRow("mailto") << QStringLiteral("mailto:a@b.c") << true;
    QTest::newRow("file") << QStringLiteral("file:///etc/passwd") << false;
    QTest::newRow("smb") << QStringLiteral("smb://host/share") << false;
    QTest::newRow("custom handler") << QStringLiteral("zoommtg://join?x=1") << false;
    QTest::newRow("scheme-less path") << QStringLiteral("/etc/passwd") << false;
    QTest::newRow("empty") << QString() << false;
}

void SafeContentTest::schemePolicy()
{
    QFETCH(QString, url);
    QFETCH(bool, safe);
    QCOMPARE(isSafeExternalScheme(QUrl(url)), safe);
}

void SafeContentTest::servesFileUnderRoot()
{
    const QString p = m_root.filePath(QStringLiteral("ok.png"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("PNGBYTES");
    f.close();
    const QVariant v = loadGuardedResource(QUrl::fromLocalFile(p), {});
    QCOMPARE(v.toByteArray(), QByteArray("PNGBYTES"));
}

void SafeContentTest::refusesOutsideRoot()
{
    const QString p = m_outside.filePath(QStringLiteral("private.png"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("SECRET");
    f.close();
    QVERIFY(refused(loadGuardedResource(QUrl::fromLocalFile(p), {})));
    // ...and allowed once the caller passes that directory as an extra root
    // (the RichTextView case: the document's own folder).
    QCOMPARE(loadGuardedResource(QUrl::fromLocalFile(p), {m_outside.path()}).toByteArray(),
             QByteArray("SECRET"));
}

void SafeContentTest::refusesSpecialFile()
{
    // THE HANG: /dev/zero is readable and inside a root we deliberately allow
    // here, so only the regular-file test can stop it. If this ever regresses,
    // this test does not fail — it hangs, which is exactly the user-visible bug.
    if (!QFile::exists(QStringLiteral("/dev/zero"))) {
        QSKIP("no /dev/zero on this platform");
    }
    QVERIFY(refused(loadGuardedResource(QUrl::fromLocalFile(QStringLiteral("/dev/zero")),
                                        {QStringLiteral("/dev")})));
}

void SafeContentTest::refusesOversizeFile()
{
    const QString p = m_root.filePath(QStringLiteral("huge.png"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    QVERIFY(f.resize(kMaxResourceBytes + 1)); // sparse: no 8 MiB of I/O
    f.close();
    QVERIFY(refused(loadGuardedResource(QUrl::fromLocalFile(p), {})));
}

void SafeContentTest::refusesSymlinkEscape()
{
    const QString target = m_outside.filePath(QStringLiteral("target.png"));
    QFile f(target);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("ESCAPED");
    f.close();
    const QString link = m_root.filePath(QStringLiteral("link.png"));
    if (!QFile::link(target, link)) {
        QSKIP("cannot create symlinks here");
    }
    // The path is under the root, the file it names is not: canonicalisation is
    // what decides, so this must be refused.
    QVERIFY(refused(loadGuardedResource(QUrl::fromLocalFile(link), {})));
}

void SafeContentTest::refusesRemoteScheme()
{
    QVERIFY(refused(loadGuardedResource(QUrl(QStringLiteral("http://evil.example/x.png")),
                                        {})));
    QVERIFY(refused(loadGuardedResource(QUrl(QStringLiteral("smb://host/share/x.png")),
                                        {})));
    // An unparseable URL is a refusal, not a "not ours": answering "not ours"
    // hands the string to a base implementation that would resolve it itself.
    const QUrl bad(QStringLiteral("http://[unclosed"));
    QVERIFY(!bad.isValid());
    QVERIFY(refused(loadGuardedResource(bad, {})));
}

void SafeContentTest::passesInlineSchemesThrough()
{
    // data:/qrc: carry no filesystem access; "not ours" is an invalid variant so
    // the browser's base implementation still decodes them.
    QVERIFY(!loadGuardedResource(QUrl(QStringLiteral("data:image/png;base64,AAAA")), {})
                 .isValid());
    QVERIFY(!loadGuardedResource(QUrl(QStringLiteral("qrc:/icons/x.png")), {}).isValid());
}

void SafeContentTest::resolvesRelativeAgainstRootsOnly()
{
    const QString p = m_root.filePath(QStringLiteral("rel.png"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("REL");
    f.close();
    QCOMPARE(loadGuardedResource(QUrl(QStringLiteral("rel.png")), {}).toByteArray(),
             QByteArray("REL"));
    // A relative name that climbs out of every root resolves to nothing.
    QVERIFY(refused(loadGuardedResource(QUrl(QStringLiteral("../rel.png")), {})));
}

// --- readBoundedTail: bound what is READ, not just what is shown -------------
//
// The F11 class outside AttachmentBuilder. A live view that follows a file an
// AGENT writes must never turn "the agent wrote a lot" into "the GUI process
// allocated a lot". Every case below is one way that used to be possible with a
// seek + readAll().
void SafeContentTest::tailReadsIncrementally()
{
    const QString p = m_root.filePath(QStringLiteral("tail.jsonl"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("one\n");
    f.flush();

    qint64 off = 0;
    TailRead r = readBoundedTail(p, off, 1024);
    QCOMPARE(r.bytes, QByteArray("one\n"));
    QCOMPARE(off, 4);
    QVERIFY(!r.gap);
    QVERIFY(!r.restarted);

    // Nothing new: no bytes, offset untouched.
    r = readBoundedTail(p, off, 1024);
    QVERIFY(r.bytes.isEmpty());
    QCOMPARE(off, 4);

    f.write("two\n");
    f.flush();
    r = readBoundedTail(p, off, 1024);
    QCOMPARE(r.bytes, QByteArray("two\n")); // only the delta
    QCOMPARE(off, 8);
}

void SafeContentTest::tailCapsOneRead()
{
    // The whole point: a huge file must not become a huge allocation. Reading
    // starts at the END of the window, not at byte 0.
    const QString p = m_root.filePath(QStringLiteral("big.jsonl"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(QByteArray(100000, 'x'));
    f.write("\ntail\n");
    f.close();

    qint64 off = 0;
    const TailRead r = readBoundedTail(p, off, 4096);
    QCOMPARE(r.bytes.size(), 4096);
    QVERIFY2(r.gap, "a file bigger than the cap must be reported as a gap");
    QCOMPARE(off, QFileInfo(p).size());
    // The window is the END of the file, so the newest lines are what shows.
    QVERIFY(r.bytes.endsWith("\ntail\n"));
}

void SafeContentTest::tailSkipsForwardWhenOutrun()
{
    // A writer faster than the reader must not make the reader fall further
    // behind on every poll — it skips forward and says so.
    const QString p = m_root.filePath(QStringLiteral("fast.jsonl"));
    QFile f(p);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write("a\n");
    f.flush();

    qint64 off = 0;
    TailRead r = readBoundedTail(p, off, 64);
    QCOMPARE(r.bytes, QByteArray("a\n"));
    QVERIFY(!r.gap);

    f.write(QByteArray(5000, 'y'));
    f.write("\nlast\n");
    f.flush();
    r = readBoundedTail(p, off, 64);
    QVERIFY(r.gap);
    QCOMPARE(r.bytes.size(), 64);
    QCOMPARE(off, QFileInfo(p).size()); // caught up, in one bounded step
}

void SafeContentTest::tailRestartsOnTruncate()
{
    const QString p = m_root.filePath(QStringLiteral("rotate.jsonl"));
    {
        QFile f(p);
        QVERIFY(f.open(QIODevice::WriteOnly));
        f.write("first line\n");
    }
    qint64 off = 0;
    TailRead r = readBoundedTail(p, off, 1024);
    QCOMPARE(off, 11);

    { // rewritten shorter: the old offset now points past the end
        QFile f(p);
        QVERIFY(f.open(QIODevice::WriteOnly | QIODevice::Truncate));
        f.write("new\n");
    }
    r = readBoundedTail(p, off, 1024);
    QVERIFY2(r.restarted, "a shrunk file must tell the caller to drop its state");
    QCOMPARE(r.bytes, QByteArray("new\n"));
    QCOMPARE(off, 4);
}

void SafeContentTest::tailRefusesSpecialAndMissingFiles()
{
    // Same fail-closed rule as the resource loader: a FIFO or device node would
    // park the GUI thread inside read(), and its size() reports 0 so no bound
    // computed from it would mean anything.
    qint64 off = 0;
    QVERIFY(readBoundedTail(QStringLiteral("/dev/zero"), off, 1024).bytes.isEmpty());
    QCOMPARE(off, 0);
    QVERIFY(readBoundedTail(m_root.path(), off, 1024).bytes.isEmpty()); // a directory
    QVERIFY(readBoundedTail(m_root.filePath(QStringLiteral("nope")), off, 1024)
                .bytes.isEmpty());
    QVERIFY(readBoundedTail(QString(), off, 1024).bytes.isEmpty());
    // A non-positive cap reads nothing rather than everything.
    const QString p = m_root.filePath(QStringLiteral("tail.jsonl"));
    qint64 z = 0;
    QVERIFY(readBoundedTail(p, z, 0).bytes.isEmpty());
    QCOMPARE(z, 0);
}

// --- what a refusal has to LOOK like, and what it has to stop ---------------
//
// The F15 remediation guarded the selection overlay and left the painted
// transcript rows on bare QTextDocuments, on the premise that a parentless
// document cannot load files. It can. Worse, the guard itself did not stop the
// load: QTextImageHandler re-reads the path in the markup whenever the resource
// did not decode, so refusing with "no bytes" refused nothing. These four tests
// pin the corrected behaviour end to end — the assertion is on RENDERED PIXELS,
// because that is the only way to observe the fallback the loader cannot see.
void SafeContentTest::blockedImageIsATransparentPixel()
{
    // Hand-written bytes (they must not depend on an image plugin), so their
    // meaning is asserted rather than assumed.
    QImage img;
    QVERIFY(img.loadFromData(blockedImageBytes(), "PNG"));
    QCOMPARE(img.size(), QSize(1, 1));
    QCOMPARE(qAlpha(img.pixel(0, 0)), 0);
}

void SafeContentTest::emptyBytesIsNotARefusalForImages()
{
    const QString p = m_outside.filePath(QStringLiteral("empty-bytes.png"));
    QVERIFY(writeSecretPng(p));
    EmptyBytesDoc doc;
    doc.setHtml(QStringLiteral("<img src=\"%1\">").arg(QUrl::fromLocalFile(p).toString()));
    const int painted = secretPixels(doc);
    QCOMPARE(doc.calls, 1); // the guard WAS consulted...
    // ...and the file rendered anyway. This is the reason loadGuardedResource
    // returns a decodable placeholder instead of an empty QByteArray; if Qt ever
    // stops doing this, that decision can be revisited — deliberately, not by
    // accident.
    QVERIFY2(painted > 0,
             "Qt no longer falls back to reading the image path itself — "
             "re-check why blockedImageBytes() exists before changing it");
}

void SafeContentTest::markdownImageOutsideRootsIsNotLoaded()
{
    // The whole threat in one line of markdown: an assistant message that names
    // a file the human never attached. It must render, and it must not read.
    const QString secret = m_outside.filePath(QStringLiteral("private.png"));
    QVERIFY(writeSecretPng(secret));
    const QString md =
        QStringLiteral("here you go\n\n![x](%1)").arg(QUrl::fromLocalFile(secret).toString());

    // Control: this is what the painted transcript rows used to do.
    QTextDocument unguarded;
    setMarkdownSafe(unguarded, md);
    QVERIFY2(secretPixels(unguarded) > 0,
             "a bare QTextDocument no longer loads local images — the guard "
             "below would then be untested, so fix this test deliberately");

    // The guard: same markdown, same layout, nothing read.
    GuardedTextDocument guarded;
    setMarkdownSafe(guarded, md);
    QCOMPARE(secretPixels(guarded), 0);
    // The text around the image still renders — refusing an image is not
    // refusing the message.
    QVERIFY(guarded.toPlainText().contains(QStringLiteral("here you go")));
    // And the loader agrees, at its own level.
    QVERIFY(refused(loadGuardedResource(QUrl::fromLocalFile(secret), {})));

    // Scheme-less absolute path: the same markdown without file://.
    GuardedTextDocument bare;
    setMarkdownSafe(bare, QStringLiteral("![x](%1)").arg(secret));
    QCOMPARE(secretPixels(bare), 0);
}

void SafeContentTest::markdownImageUnderRootStillRenders()
{
    // The guard is an allowlist, not a blanket block: a thumbnail the human
    // attached (the attachment store is an allowed root, m_root stands in for
    // it here) must still appear, or the feature is broken rather than safe.
    const QString ok = m_root.filePath(QStringLiteral("attached.png"));
    QVERIFY(writeSecretPng(ok));
    GuardedTextDocument guarded;
    setMarkdownSafe(guarded,
                    QStringLiteral("![x](%1)").arg(QUrl::fromLocalFile(ok).toString()));
    QVERIFY(secretPixels(guarded) > 0);
}

void SafeContentTest::guardedBrowserAlsoRefuses()
{
    // The surface F15 originally guarded (the transcript's selection overlay,
    // the sub-agent transcript dialog, the file previewer) — never actually
    // asserted end to end until now, which is how the "no bytes" refusal went
    // unnoticed.
    const QString secret = m_outside.filePath(QStringLiteral("browser.png"));
    QVERIFY(writeSecretPng(secret));
    GuardedTextBrowser browser;
    browser.setHtml(
        QStringLiteral("<img src=\"%1\">").arg(QUrl::fromLocalFile(secret).toString()));
    QCOMPARE(secretPixels(*browser.document()), 0);

    // Same browser, a root it was told about: served.
    const QString ok = m_root.filePath(QStringLiteral("browser-ok.png"));
    QVERIFY(writeSecretPng(ok));
    browser.setHtml(
        QStringLiteral("<img src=\"%1\">").arg(QUrl::fromLocalFile(ok).toString()));
    QVERIFY(secretPixels(*browser.document()) > 0);
}

// The guard is only worth having if there is no way around it. Every
// heap-allocated QTextDocument in ui/src is one that outlives its statement —
// i.e. one that gets laid out and painted — so it must be a
// GuardedTextDocument. Stack-local QTextDocuments are held to the same bar
// unless they are on the exempt list below: those only call
// setMarkdown()+toHtml(), nothing lays them out, so no image handler ever runs
// against them. Probed. If one of them ever grows a
// setTextWidth()/drawContents(), it needs the guard too — and any NEW
// stack-local either takes the guard or argues its way onto the list here.
void SafeContentTest::noUnguardedRenderedDocument()
{
    const QString src = uiSrcDir();
    if (src.isEmpty()) {
        QSKIP("source tree not reachable from the test binary");
    }
    // The probed setMarkdown()+toHtml()-only documents (both markdownToHtml
    // helpers). LspHoverProvider.cpp used to be the third; it now takes the
    // guard (plan 30 carried item), which is what this ratchet preserves.
    const QStringList exempt{QStringLiteral("RichTextView.cpp"),
                             QStringLiteral("AgentChatHelpers.cpp")};
    const QRegularExpression stackLocal(QStringLiteral("\\bQTextDocument\\s+\\w+;"));
    QStringList offenders;
    QDirIterator it(src, {QStringLiteral("*.cpp"), QStringLiteral("*.h")}, QDir::Files,
                    QDirIterator::Subdirectories);
    int scanned = 0;
    while (it.hasNext()) {
        const QString path = it.next();
        QFile f(path);
        if (!f.open(QIODevice::ReadOnly | QIODevice::Text)) {
            continue;
        }
        ++scanned;
        const QString name = QFileInfo(path).fileName();
        const QString text = QString::fromUtf8(f.readAll());
        if (text.contains(QStringLiteral("new QTextDocument"))
            || (!exempt.contains(name) && text.contains(stackLocal))) {
            offenders << name;
        }
    }
    QVERIFY2(scanned > 10, "source scan found almost nothing — wrong directory?");
    QVERIFY2(offenders.isEmpty(),
             qPrintable(QStringLiteral("unguarded QTextDocument allocated in: ")
                        + offenders.join(QStringLiteral(", "))
                        + QStringLiteral(" — use agentkate::GuardedTextDocument")));
}

QTEST_MAIN(SafeContentTest)
#include "SafeContentTest.moc"
