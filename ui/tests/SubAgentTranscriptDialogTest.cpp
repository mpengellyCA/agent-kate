// The dialog's document bounds.
//
// This view tails a file a SUB-AGENT writes, so its size, its line lengths and
// its growth rate are all attacker-influenced. It caps the document, and the cap
// used to be maximumBlockCount alone — which bounds how many PARAGRAPHS the
// document holds and says nothing about how long one paragraph is. A sub-agent
// that writes few-but-enormous messages grew the QTextDocument without limit
// while the block count sat far below its cap, so the stated claim ("a sub-agent
// that keeps writing cannot grow the QTextDocument without bound") was false.
//
// These tests pin both shapes: many-and-small (blocks) and few-and-huge
// (characters), including the degenerate case of a single block that is over the
// whole budget on its own.

#include "SubAgentTranscriptDialog.h"

#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QScopedPointer>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QTextBrowser>
#include <QTextDocument>
#include <QtTest>

namespace {
// LOCKSTEP with kMaxDocChars in SubAgentTranscriptDialog.cpp. If that moves,
// this moves with it — there is no reason to widen it silently.
constexpr int kMaxDocChars = 200000;

// One transcript line in the shape both engines' logs share.
QByteArray assistantLine(const QString &text)
{
    QJsonObject block{{QStringLiteral("type"), QStringLiteral("text")},
                      {QStringLiteral("text"), text}};
    QJsonObject msg{{QStringLiteral("role"), QStringLiteral("assistant")},
                    {QStringLiteral("content"), QJsonArray{block}}};
    QJsonObject line{{QStringLiteral("message"), msg}};
    return QJsonDocument(line).toJson(QJsonDocument::Compact) + '\n';
}

// A single paragraph of `chars` ordinary words, tagged so a test can tell which
// message survived the trim.
QString paragraph(const QString &marker, int chars)
{
    QString s = marker + QLatin1Char(' ');
    while (s.size() < chars) {
        s += QStringLiteral("lorem ipsum dolor sit amet ");
    }
    return s.left(chars);
}

QTextBrowser *browserOf(SubAgentTranscriptDialog *dlg)
{
    return dlg->findChild<QTextBrowser *>();
}
} // namespace

class SubAgentTranscriptDialogTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void fewHugeBlocksAreBounded();
    void oneOversizeBlockCannotSurvive();
    void ordinaryTranscriptIsNotTrimmed();

private:
    QTemporaryDir m_dir;
};

void SubAgentTranscriptDialogTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true); // keep KConfig out of the real profile
    QVERIFY(m_dir.isValid());
}

void SubAgentTranscriptDialogTest::fewHugeBlocksAreBounded()
{
    // Six messages, 60k characters each: 360k characters in ~18 blocks. The
    // block cap (4000) is nowhere near tripping — only the character cap can
    // stop this.
    const QString path = m_dir.filePath(QStringLiteral("huge.jsonl"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(assistantLine(paragraph(QStringLiteral("OLDESTMARKER"), 60000)));
    for (int i = 0; i < 4; ++i) {
        f.write(assistantLine(paragraph(QStringLiteral("MIDDLE"), 60000)));
    }
    f.write(assistantLine(paragraph(QStringLiteral("NEWESTMARKER"), 60000)));
    f.close();

    QScopedPointer<SubAgentTranscriptDialog> dlg(
        new SubAgentTranscriptDialog(path, QStringLiteral("worker")));
    QTextBrowser *b = browserOf(dlg.data());
    QVERIFY(b);
    const QTextDocument *doc = b->document();
    QVERIFY2(doc->characterCount() <= kMaxDocChars,
             qPrintable(QStringLiteral("document grew to %1 characters")
                            .arg(doc->characterCount())));
    QVERIFY(doc->blockCount() < 4000); // the block cap never bound: this is the hole
    // A tail, not a truncation: the newest output is what is kept.
    const QString text = doc->toPlainText();
    QVERIFY(text.contains(QStringLiteral("NEWESTMARKER")));
    QVERIFY(!text.contains(QStringLiteral("OLDESTMARKER")));
}

void SubAgentTranscriptDialogTest::oneOversizeBlockCannotSurvive()
{
    // The degenerate shape: ONE paragraph over the whole budget. There is
    // nothing to drop off the front, so the document is dropped instead — the
    // file on disk stays the record.
    const QString path = m_dir.filePath(QStringLiteral("one-huge.jsonl"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(assistantLine(paragraph(QStringLiteral("MONOLITH"), 300000)));
    f.close();

    QScopedPointer<SubAgentTranscriptDialog> dlg(
        new SubAgentTranscriptDialog(path, QString()));
    QTextBrowser *b = browserOf(dlg.data());
    QVERIFY(b);
    QVERIFY2(b->document()->characterCount() <= kMaxDocChars,
             qPrintable(QStringLiteral("document grew to %1 characters")
                            .arg(b->document()->characterCount())));
}

void SubAgentTranscriptDialogTest::ordinaryTranscriptIsNotTrimmed()
{
    // The cap must not be so tight that a normal conversation loses its head:
    // an ordinary transcript renders whole, oldest message included.
    const QString path = m_dir.filePath(QStringLiteral("normal.jsonl"));
    QFile f(path);
    QVERIFY(f.open(QIODevice::WriteOnly));
    f.write(assistantLine(QStringLiteral("OLDESTMARKER first thing I did")));
    for (int i = 0; i < 50; ++i) {
        f.write(assistantLine(QStringLiteral("looked at the file and moved on")));
    }
    f.write(assistantLine(QStringLiteral("NEWESTMARKER done")));
    f.close();

    QScopedPointer<SubAgentTranscriptDialog> dlg(
        new SubAgentTranscriptDialog(path, QString()));
    QTextBrowser *b = browserOf(dlg.data());
    QVERIFY(b);
    const QString text = b->document()->toPlainText();
    QVERIFY(text.contains(QStringLiteral("OLDESTMARKER")));
    QVERIFY(text.contains(QStringLiteral("NEWESTMARKER")));
}

QTEST_MAIN(SubAgentTranscriptDialogTest)
#include "SubAgentTranscriptDialogTest.moc"
