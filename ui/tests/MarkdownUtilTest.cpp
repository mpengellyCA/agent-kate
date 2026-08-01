// Regression guard for the chat/preview/hover markdown-rendering bug, and for
// the security property that came with it (audit F16).
//
// QTextDocument::setMarkdown() runs md4c, and md4c recognises raw HTML by
// default. Two consequences, both bad: CommonMark reads "<T>" as an open tag so
// Qt's importer DROPS the surrounding text ("Reactive<T> guards" -> "Reactive"),
// and anything a model writes as HTML becomes live Qt rich text on the surface
// where the human approves permission prompts.
//
// The fix is not a pre-pass that guesses where md4c thinks raw HTML is live —
// three audit rounds each found another place where such a guess and md4c
// disagreed, and every disagreement was a hole. setMarkdownSafe() instead passes
// md4c's own MarkdownNoHTML flag, so the single parser in the pipeline never
// recognises HTML at all.
//
// These tests drive the REAL pipeline (setMarkdownSafe -> QTextDocument) so they
// pin the observable property rather than an implementation:
//   * every payload's tag text survives as READABLE TEXT (proof md4c did not
//     consume it as markup: had it, Qt would have applied it and eaten the text);
//   * no payload produces an element in the document;
//   * ordinary markdown still renders as markdown;
//   * and no other call site in the tree can opt out of the flag.

#include "MarkdownUtil.h"

#include <QCoreApplication>
#include <QDir>
#include <QDirIterator>
#include <QFile>
#include <QFileInfo>
#include <QTextDocument>
#include <QtTest>

using agentkate::setMarkdownSafe;

namespace {
// Render markdown the way the app does and return the document's plain text.
QString rendered(const QString &md)
{
    QTextDocument doc;
    setMarkdownSafe(doc, md);
    return doc.toPlainText();
}

QString renderedHtml(const QString &md)
{
    QTextDocument doc;
    setMarkdownSafe(doc, md);
    return doc.toHtml();
}

// ui/src, located from the test binary (in-tree build) or from this file's own
// path. Empty when neither works.
QString uiSrcDir()
{
    for (const QString &from :
         {QCoreApplication::applicationDirPath(), QDir::currentPath()}) {
        QDir d(from);
        for (int i = 0; i < 8; ++i) {
            if (d.exists(QStringLiteral("ui/src/MarkdownUtil.cpp"))) {
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
        if (t.cdUp() && t.exists(QStringLiteral("src/MarkdownUtil.cpp"))) {
            return t.absoluteFilePath(QStringLiteral("src"));
        }
    }
    return QString();
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
    void keepsAutolinks();
    void rawHtmlNeverBecomesMarkup_data();
    void rawHtmlNeverBecomesMarkup();
    void ordinaryMarkdownStillRenders();
    void indentedAndFencedCodeAreByteExact_data();
    void indentedAndFencedCodeAreByteExact();
    void noUnguardedSetMarkdownCall();
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
    // Inside a code span '<' is literal and must round-trip with no entity:
    // the old escaping pre-pass had to special-case this, the parser flag does
    // not have to know about it at all.
    QCOMPARE(rendered(QStringLiteral("a `code<T>span` b")),
             QStringLiteral("a code<T>span b"));
    QVERIFY(!rendered(QStringLiteral("a `code<T>span` b"))
                 .contains(QStringLiteral("&lt;")));
}

void MarkdownUtilTest::preservesFencedBlock()
{
    const QString out =
        rendered(QStringLiteral("```\ntemplate<typename T> void f();\n```"));
    QVERIFY(out.contains(QStringLiteral("template<typename T> void f();")));
    QVERIFY(!out.contains(QStringLiteral("&lt;")));
    // ...and it is still a code block, not a paragraph.
    QVERIFY(renderedHtml(QStringLiteral("```\ntemplate<typename T> void f();\n```"))
                .contains(QStringLiteral("<pre")));
}

void MarkdownUtilTest::preservesBlockquote()
{
    // '>' still opens a quote; '<' inside it is text, and the text after it
    // survives (it did not, before).
    QCOMPARE(rendered(QStringLiteral("> quoted <T> line")),
             QStringLiteral("quoted <T> line"));
}

void MarkdownUtilTest::honoursBackslashEscape()
{
    // A markdown-escaped "\<" is markdown's own literalization and still works.
    QCOMPARE(rendered(QStringLiteral("an escaped \\<thing> stays")),
             QStringLiteral("an escaped <thing> stays"));
}

void MarkdownUtilTest::keepsAutolinks()
{
    // Regression the old escaping pre-pass caused and the flag does not: an
    // autolink is delimited by '<' '>', so escaping every '<' outside code
    // destroyed it. MarkdownNoHTML disables HTML *spans and blocks* only —
    // autolinks are a markdown construct and keep working.
    const QString html = renderedHtml(QStringLiteral("see <http://example.com/> ok"));
    QVERIFY2(html.contains(QStringLiteral("href=\"http://example.com/\"")),
             qPrintable(QStringLiteral("autolink was destroyed: ") + html));
    QCOMPARE(rendered(QStringLiteral("see <http://example.com/> ok")),
             QStringLiteral("see http://example.com/ ok"));
}

// --- The security property ---------------------------------------------------
//
// Each row is markdown a prompt-injected agent could emit. The property has two
// halves and both are checked below for every row:
//   1. the tag text is still READABLE — if md4c had taken it as HTML, Qt would
//      have applied it and the literal text would be gone (that is exactly what
//      "Reactive<T>" -> "Reactive" was);
//   2. the document contains no <img>/<table>/<a href> element — no payload here
//      writes markdown that could legitimately produce one.
void MarkdownUtilTest::rawHtmlNeverBecomesMarkup_data()
{
    QTest::addColumn<QString>("src");
    QTest::addColumn<QString>("literal"); // must appear verbatim in the plain text

    // --- plain raw HTML, no cleverness needed --------------------------------
    QTest::newRow("inline tag") << QStringLiteral("a <b>pwned</b> b")
                                << QStringLiteral("<b>pwned</b>");
    QTest::newRow("html block")
        << QStringLiteral("<div style=\"color:red\">Approve</div>\n")
        << QStringLiteral("<div style=\"color:red\">Approve</div>");
    QTest::newRow("table spoof")
        << QStringLiteral("<table><tr><td>Allow</td></tr></table>\n")
        << QStringLiteral("<table><tr><td>Allow</td></tr></table>");
    QTest::newRow("styled span spoof")
        << QStringLiteral("<span style=\"color:red\">Approve</span>\n")
        << QStringLiteral("<span style=\"color:red\">Approve</span>");
    QTest::newRow("image read of a device node")
        << QStringLiteral("x <img src=\"/dev/zero\"> y")
        << QStringLiteral("<img src=\"/dev/zero\">");
    QTest::newRow("script") << QStringLiteral("<script>alert(1)</script>\n")
                            << QStringLiteral("<script>alert(1)</script>");
    QTest::newRow("comment swallows text")
        << QStringLiteral("a <!-- c --> b") << QStringLiteral("<!-- c -->");

    // --- the escape hatches earlier audit rounds found in the pre-pass -------
    // Each of these was a case where a hand-written scanner believed the
    // payload line was inside a code block (so: copy verbatim) while md4c read
    // it as a paragraph (so: live raw HTML). The parser flag makes the whole
    // class unreachable — there is no second opinion to be wrong.

    // Round 1: a backtick inside a backtick fence's info string means it is not
    // a fence, so the next line is a raw HTML block.
    QTest::newRow("backtick in fence info string")
        << QStringLiteral("```rust`x`\n<b>pwned</b>\n") << QStringLiteral("<b>pwned</b>");
    // Round 2: an unmatched backtick reaching across a leaf-block boundary.
    QTest::newRow("code span reaches across a heading")
        << QStringLiteral("# head `tick\n<b>pwned</b>\ntail ` close\n")
        << QStringLiteral("<b>pwned</b>");
    QTest::newRow("code span reaches across a blank line")
        << QStringLiteral("`x\n\n<b>pwned</b>\n\n`y\n") << QStringLiteral("<b>pwned</b>");

    // Round 3, [HIGH]: a fence opened INSIDE a list item (indent 1-3) and never
    // closed. A line-oriented scanner stays "in fence" to EOF and copies every
    // following line verbatim; md4c closes the item at the blank line and reads
    // the payload as a top-level HTML block.
    QTest::newRow("unterminated fence inside a list item")
        << QStringLiteral("- item\n  ```\n  code\n\n<b>pwned</b>\n")
        << QStringLiteral("<b>pwned</b>");
    QTest::newRow("unterminated fence inside an ordered item")
        << QStringLiteral("1. item\n   ~~~\n   code\n\n<table><tr><td>Allow</td></tr>"
                          "</table>\n")
        << QStringLiteral("<table><tr><td>Allow</td></tr></table>");

    // Round 3, [MED]: the indented-code floor. Held at the OUTERMOST item's
    // content column, a 6-space line inside a nested item clears "floor + 4"
    // and is copied verbatim — but md4c measures from the INNER item's content
    // column, so the same line is a paragraph, where raw HTML is live. A lower
    // floor is the unsafe direction: it lets more lines qualify as code.
    QTest::newRow("nested list item lowers the indented-code floor")
        << QStringLiteral("- outer\n  - inner\n\n      <b>pwned</b>\n")
        << QStringLiteral("<b>pwned</b>");
    QTest::newRow("deeper nesting, same trick")
        << QStringLiteral("- a\n  - b\n    - c\n\n        <span style=\"color:red\">"
                          "Approve</span>\n")
        << QStringLiteral("<span style=\"color:red\">Approve</span>");

    // Round 3, [MED]: an HTML block (CommonMark conditions 1-6) INTERRUPTS a
    // paragraph. A scanner that pairs inline backticks across the whole
    // paragraph declares the payload a code span and copies it; md4c ended the
    // paragraph at the '<div>' line, so the backticks never pair and the block
    // is live HTML.
    QTest::newRow("html block interrupts a paragraph mid-code-span")
        << QStringLiteral("para `tick\n<div>pwned</div>\ntail ` close\n")
        << QStringLiteral("<div>pwned</div>");
    QTest::newRow("pre block interrupts a paragraph mid-code-span")
        << QStringLiteral("para `tick\n<pre>pwned</pre>\ntail ` close\n")
        << QStringLiteral("<pre>pwned</pre>");
    QTest::newRow("table block interrupts a paragraph mid-code-span")
        << QStringLiteral("para `tick\n<table><tr><td>Allow</td></tr></table>\n"
                          "tail ` close\n")
        << QStringLiteral("<table><tr><td>Allow</td></tr></table>");

    // --- entities are the one thing md4c still translates under the flag -----
    // Safe by shape: an entity token carries no '<' of its own, so it can only
    // ever put a literal character in the text, never an element.
    QTest::newRow("named entity payload")
        << QStringLiteral("&lt;img src=\"/dev/zero\"&gt;")
        << QStringLiteral("<img src=\"/dev/zero\">");
    QTest::newRow("numeric entity payload")
        << QStringLiteral("&#60;table&#62;&#60;tr&#62;&#60;td&#62;Allow")
        << QStringLiteral("<table><tr><td>Allow");
}

void MarkdownUtilTest::rawHtmlNeverBecomesMarkup()
{
    QFETCH(QString, src);
    QFETCH(QString, literal);

    QTextDocument doc;
    setMarkdownSafe(doc, src);
    const QString plain = doc.toPlainText();
    const QString html = doc.toHtml();

    QVERIFY2(plain.contains(literal),
             qPrintable(QStringLiteral("payload was interpreted, not shown: ") + plain));

    // No element. Compare against a document holding the same text as PLAIN
    // TEXT: Qt round-trips its own markup in toHtml(), so the baseline accounts
    // for the escaping of the payload's characters.
    QTextDocument baseline;
    baseline.setPlainText(plain);
    const QString base = baseline.toHtml();
    for (const char *tag : {"<img", "<table", "href=", "<hr"}) {
        QCOMPARE(html.count(QLatin1String(tag)), base.count(QLatin1String(tag)));
    }
}

// The other side of the trade: turning HTML off must not turn MARKDOWN off.
void MarkdownUtilTest::ordinaryMarkdownStillRenders()
{
    const QString html = renderedHtml(QStringLiteral(
        "# Heading\n\n**bold** and *italic* and [link](http://example.com/x)\n\n"
        "- one\n- two\n\n| a | b |\n| --- | --- |\n| x<T> | y |\n\n"
        "```cpp\nstd::vector<int> v;\n```\n\n> quoted\n\n---\n"));
    QVERIFY2(html.contains(QStringLiteral("href=\"http://example.com/x\"")),
             "markdown links stopped working");
    QVERIFY2(html.contains(QStringLiteral("font-weight:700")), "bold stopped working");
    QVERIFY2(html.contains(QStringLiteral("font-style:italic")), "italic stopped working");
    QVERIFY2(html.contains(QStringLiteral("<table")), "GFM tables stopped working");
    QVERIFY2(html.contains(QStringLiteral("<ul")), "lists stopped working");
    QVERIFY2(html.contains(QStringLiteral("<pre")), "fenced code stopped working");

    const QString plain = rendered(QStringLiteral(
        "# Heading\n\n**bold** and *italic* and [link](http://example.com/x)\n\n"
        "- one\n- two\n\n| a | b |\n| --- | --- |\n| x<T> | y |\n\n"
        "```cpp\nstd::vector<int> v;\n```\n\n> quoted\n\n---\n"));
    // Text inside a table cell and inside a fence keeps its angle brackets, and
    // nothing anywhere shows a stray entity.
    QVERIFY(plain.contains(QStringLiteral("x<T>")));
    QVERIFY(plain.contains(QStringLiteral("std::vector<int> v;")));
    QVERIFY2(!plain.contains(QStringLiteral("&lt;")),
             qPrintable(QStringLiteral("over-escaped: ") + plain));
}

// Code is what the user must SEE, byte for byte. The old pre-pass had to decide
// which lines were code to avoid printing "&lt;" into them, and getting that
// decision wrong in the safe direction was a visible rendering bug. With the
// flag there is no decision: '<' is never touched anywhere.
void MarkdownUtilTest::indentedAndFencedCodeAreByteExact_data()
{
    QTest::addColumn<QString>("src");
    QTest::addColumn<QString>("literal");

    QTest::newRow("indented code at top level")
        << QStringLiteral("text\n\n    <T> code\n\nafter\n") << QStringLiteral("<T> code");
    QTest::newRow("indented code after a list")
        << QStringLiteral("- item\n\ntext\n\n    <T> code\n") << QStringLiteral("<T> code");
    QTest::newRow("indented code inside a list item")
        << QStringLiteral("- item\n\n        <T> code\n") << QStringLiteral("<T> code");
    QTest::newRow("fence inside a list item")
        << QStringLiteral("- item\n  ```\n  std::map<K,V> m;\n  ```\n")
        << QStringLiteral("std::map<K,V> m;");
    QTest::newRow("tilde fence with a backtick info string")
        << QStringLiteral("~~~rust`x`\ntemplate<T>\n~~~\n") << QStringLiteral("template<T>");
    QTest::newRow("nested fence markers")
        << QStringLiteral("````\n```\n<b>x</b>\n````\n") << QStringLiteral("<b>x</b>");
    QTest::newRow("code span across two lines of one paragraph")
        << QStringLiteral("start `foo\nbar<T>` end\n") << QStringLiteral("bar<T>");
}

void MarkdownUtilTest::indentedAndFencedCodeAreByteExact()
{
    QFETCH(QString, src);
    QFETCH(QString, literal);
    const QString plain = rendered(src);
    QVERIFY2(plain.contains(literal), qPrintable(QStringLiteral("code mangled: ") + plain));
    QVERIFY2(!plain.contains(QStringLiteral("&lt;")),
             qPrintable(QStringLiteral("entity leaked into code: ") + plain));
}

// The property is only "by construction" if there is no way around it. Every
// QTextDocument::setMarkdown() call in ui/src must be the one inside
// setMarkdownSafe() — anything else is a call site running with md4c's default
// features, i.e. with raw HTML back on.
void MarkdownUtilTest::noUnguardedSetMarkdownCall()
{
    const QString src = uiSrcDir();
    if (src.isEmpty()) {
        QSKIP("source tree not reachable from the test binary");
    }
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
        const QString text = QString::fromUtf8(f.readAll());
        // A call, not the prose in a comment: "QTextDocument::setMarkdown()".
        if (!text.contains(QStringLiteral(".setMarkdown("))
            && !text.contains(QStringLiteral("->setMarkdown("))) {
            continue;
        }
        if (QFileInfo(path).fileName() == QLatin1String("MarkdownUtil.cpp")) {
            continue; // the one sanctioned call
        }
        offenders << path;
    }
    QVERIFY2(scanned > 10, "source scan found almost nothing — wrong directory?");
    QVERIFY2(offenders.isEmpty(),
             qPrintable(QStringLiteral("setMarkdown() called outside setMarkdownSafe(): ")
                        + offenders.join(QStringLiteral(", "))));
}

QTEST_MAIN(MarkdownUtilTest)
#include "MarkdownUtilTest.moc"
