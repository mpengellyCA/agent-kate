// Regression guard for the chat/preview/hover markdown-rendering bug.
//
// QTextDocument::setMarkdown() runs md4c with raw-HTML enabled, and CommonMark
// treats "<T>" as an HTML open tag. Qt's importer then drops the surrounding
// text, so "Reactive<T> guards" rendered as "Reactive". neutralizeMarkdownRawHtml
// escapes those angle brackets first. These tests drive the *real* pipeline
// (neutralize -> setMarkdown -> toPlainText) so they pin the observable fix, not
// the escaping strategy, while also checking code/fences round-trip untouched.

#include "MarkdownUtil.h"

#include <QTextDocument>
#include <QtTest>

using agentkate::neutralizeMarkdownRawHtml;

namespace {
// Render markdown the way the app does and return the document's plain text.
QString rendered(const QString &md)
{
    QTextDocument doc;
    doc.setMarkdown(neutralizeMarkdownRawHtml(md), QTextDocument::MarkdownDialectGitHub);
    return doc.toPlainText();
}
} // namespace

class MarkdownUtilTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void keepsTextAroundAngleBrackets();
    void keepsComparisons();
    void preservesCodeSpan();
    void preservesFencedBlock();
    void preservesBlockquote();
    void honoursBackslashEscape();
    void leavesPlainMarkdownAlone();
};

void MarkdownUtilTest::keepsTextAroundAngleBrackets()
{
    // The exact bug from the report: everything after '<' used to vanish.
    const QString out = rendered(
        QStringLiteral("Reactive<T> with equality guards. Then `AKCORE_PPROF` end."));
    QVERIFY2(out.contains(QStringLiteral("with equality guards")),
             qPrintable(QStringLiteral("text after '<' was swallowed: ") + out));
    QVERIFY(out.contains(QStringLiteral("Reactive<T>")));
    QVERIFY(out.contains(QStringLiteral("AKCORE_PPROF")));
    QVERIFY(out.endsWith(QStringLiteral("end.")));
}

void MarkdownUtilTest::keepsComparisons()
{
    QCOMPARE(rendered(QStringLiteral("if a < b and c > d then x")),
             QStringLiteral("if a < b and c > d then x"));
}

void MarkdownUtilTest::preservesCodeSpan()
{
    // Inside a code span '<' is already literal; the span must survive verbatim.
    const QString src = QStringLiteral("a `code<T>span` b");
    QCOMPARE(neutralizeMarkdownRawHtml(src), src);
    QCOMPARE(rendered(src), QStringLiteral("a code<T>span b"));
}

void MarkdownUtilTest::preservesFencedBlock()
{
    const QString src = QStringLiteral("```\ntemplate<typename T> void f();\n```");
    QCOMPARE(neutralizeMarkdownRawHtml(src), src);
    QVERIFY(rendered(src).contains(QStringLiteral("template<typename T> void f();")));
}

void MarkdownUtilTest::preservesBlockquote()
{
    // '>' is left untouched so blockquotes still parse; '<' inside is escaped.
    QVERIFY(rendered(QStringLiteral("> quoted <T> line")).contains(QStringLiteral("quoted <T> line")));
}

void MarkdownUtilTest::honoursBackslashEscape()
{
    // A markdown-escaped "\<" must stay a backslash pair so md4c yields literal
    // "<thing>" rather than the stray entity "&lt;thing>".
    QCOMPARE(neutralizeMarkdownRawHtml(QStringLiteral("an escaped \\<thing> stays")),
             QStringLiteral("an escaped \\<thing> stays"));
    QCOMPARE(rendered(QStringLiteral("an escaped \\<thing> stays")),
             QStringLiteral("an escaped <thing> stays"));
}

void MarkdownUtilTest::leavesPlainMarkdownAlone()
{
    // No angle brackets -> the source is returned unchanged (no needless churn).
    const QString src = QStringLiteral("**bold** and *italic* and [link](http://x) ok");
    QCOMPARE(neutralizeMarkdownRawHtml(src), src);
}

QTEST_MAIN(MarkdownUtilTest)
#include "MarkdownUtilTest.moc"
