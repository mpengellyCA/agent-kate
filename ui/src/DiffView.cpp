#include "DiffView.h"

#include <KSyntaxHighlighting/AbstractHighlighter>
#include <KSyntaxHighlighting/Definition>
#include <KSyntaxHighlighting/Format>
#include <KSyntaxHighlighting/Repository>
#include <KSyntaxHighlighting/State>
#include <KSyntaxHighlighting/Theme>

#include <QComboBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QPalette>
#include <QRegularExpression>
#include <QTextBrowser>
#include <QVBoxLayout>

namespace {

// One parsed diff line.
struct DiffLine {
    enum Kind { Context, Added, Removed, Hunk } kind = Context;
    int oldNo = -1;
    int newNo = -1;
    QString text; // code without the marker; for Hunk, the @@ line
};

// One changed file in the diff.
struct DiffFile {
    QString path;
    int added = 0;
    int removed = 0;
    QList<DiffLine> lines;
};

QString escapeHtml(const QString &s)
{
    return s.toHtmlEscaped();
}

// HtmlHighlighter renders one source line to coloured HTML spans, threading
// KSyntaxHighlighting state across lines within a hunk.
class HtmlHighlighter : public KSyntaxHighlighting::AbstractHighlighter
{
public:
    QString render(const QString &line)
    {
        m_line = line;
        m_html.clear();
        m_pos = 0;
        m_state = highlightLine(line, m_state);
        if (m_pos < m_line.size()) {
            m_html += escapeHtml(m_line.mid(m_pos));
        }
        return m_html;
    }

    void resetState() { m_state = KSyntaxHighlighting::State(); }

protected:
    void applyFormat(int offset, int length,
                     const KSyntaxHighlighting::Format &format) override
    {
        if (length <= 0) {
            return;
        }
        if (offset > m_pos) {
            m_html += escapeHtml(m_line.mid(m_pos, offset - m_pos));
        }
        const QString token = escapeHtml(m_line.mid(offset, length));
        if (format.hasTextColor(theme())) {
            m_html += QStringLiteral("<span style=\"color:%1\">%2</span>")
                          .arg(format.textColor(theme()).name(), token);
        } else {
            m_html += token;
        }
        m_pos = offset + length;
    }

private:
    QString m_line;
    QString m_html;
    int m_pos = 0;
    KSyntaxHighlighting::State m_state;
};

KSyntaxHighlighting::Repository &sharedRepository()
{
    static KSyntaxHighlighting::Repository repo;
    return repo;
}

QList<DiffFile> parseDiff(const QString &diff)
{
    QList<DiffFile> files;
    DiffFile *cur = nullptr;
    int oldNo = 0;
    int newNo = 0;
    static const QRegularExpression hunkRe(
        QStringLiteral("^@@ -(\\d+)(?:,\\d+)? \\+(\\d+)(?:,\\d+)? @@"));

    const QStringList lines = diff.split(QLatin1Char('\n'));
    for (const QString &raw : lines) {
        if (raw.startsWith(QLatin1String("diff --git "))) {
            files.append(DiffFile{});
            cur = &files.last();
            continue;
        }
        if (raw.startsWith(QLatin1String("+++ "))) {
            QString p = raw.mid(4).trimmed();
            if (p.startsWith(QLatin1String("b/"))) {
                p = p.mid(2);
            }
            if (!cur) {
                files.append(DiffFile{});
                cur = &files.last();
            }
            cur->path = p;
            continue;
        }
        if (raw.startsWith(QLatin1String("--- "))) {
            continue;
        }
        if (raw.startsWith(QLatin1String("@@"))) {
            const QRegularExpressionMatch m = hunkRe.match(raw);
            if (m.hasMatch()) {
                oldNo = m.captured(1).toInt();
                newNo = m.captured(2).toInt();
            }
            if (cur) {
                cur->lines.append(DiffLine{DiffLine::Hunk, -1, -1, raw});
            }
            continue;
        }
        if (!cur) {
            continue;
        }
        const QString text = raw.mid(1).replace(QLatin1Char('\t'), QStringLiteral("    "));
        if (raw.startsWith(QLatin1Char('+'))) {
            cur->lines.append(DiffLine{DiffLine::Added, -1, newNo, text});
            ++newNo;
            ++cur->added;
        } else if (raw.startsWith(QLatin1Char('-'))) {
            cur->lines.append(DiffLine{DiffLine::Removed, oldNo, -1, text});
            ++oldNo;
            ++cur->removed;
        } else if (raw.startsWith(QLatin1Char(' '))) {
            cur->lines.append(DiffLine{DiffLine::Context, oldNo, newNo, text});
            ++oldNo;
            ++newNo;
        }
        // Other lines ("index …", "new file mode …", "\ No newline …") are skipped.
    }
    return files;
}

QString numCell(int n)
{
    return n >= 0 ? QString::number(n).rightJustified(5) : QStringLiteral("     ");
}

} // namespace

DiffView::DiffView(const QString &unifiedDiff, QWidget *parent)
    : QWidget(parent)
{
    const bool dark = palette().color(QPalette::Base).lightness() < 128;
    const QString addBg    = dark ? QStringLiteral("#16331d") : QStringLiteral("#e6ffec");
    const QString delBg    = dark ? QStringLiteral("#3a1e1f") : QStringLiteral("#ffebe9");
    const QString hunkBg   = dark ? QStringLiteral("#1f2733") : QStringLiteral("#ddf4ff");
    const QString hunkFg   = dark ? QStringLiteral("#7c9cc0") : QStringLiteral("#3b5b7a");
    const QString gutterFg = dark ? QStringLiteral("#6b7280") : QStringLiteral("#9aa0a8");
    const QString headBg   = dark ? QStringLiteral("#23272f") : QStringLiteral("#eef0f3");
    const QString addFg    = dark ? QStringLiteral("#5fd38a") : QStringLiteral("#1a7f37");
    const QString delFg    = dark ? QStringLiteral("#ff8a80") : QStringLiteral("#c01c28");

    const QList<DiffFile> files = parseDiff(unifiedDiff);

    KSyntaxHighlighting::Repository &repo = sharedRepository();
    const KSyntaxHighlighting::Theme theme = repo.defaultTheme(
        dark ? KSyntaxHighlighting::Repository::DarkTheme
             : KSyntaxHighlighting::Repository::LightTheme);
    const QColor codeBg(theme.editorColor(KSyntaxHighlighting::Theme::BackgroundColor));

    int totalAdded = 0;
    int totalRemoved = 0;
    QString body;

    for (int fi = 0; fi < files.size(); ++fi) {
        const DiffFile &file = files.at(fi);
        totalAdded += file.added;
        totalRemoved += file.removed;

        body += QStringLiteral(
                    "<a name=\"file%1\"></a>"
                    "<div style=\"background-color:%2;padding:7px 10px;margin-top:%3;\">"
                    "<b>%4</b> &nbsp; <span style=\"color:%5\">+%6</span> "
                    "<span style=\"color:%7\">-%8</span></div>")
                    .arg(QString::number(fi), headBg, fi == 0 ? QStringLiteral("0") : QStringLiteral("14px"),
                         escapeHtml(file.path.isEmpty() ? QStringLiteral("(file)") : file.path),
                         addFg, QString::number(file.added),
                         delFg, QString::number(file.removed));

        HtmlHighlighter hl;
        hl.setDefinition(repo.definitionForFileName(file.path));
        hl.setTheme(theme);

        for (const DiffLine &line : file.lines) {
            if (line.kind == DiffLine::Hunk) {
                hl.resetState();
                body += QStringLiteral(
                            "<div style=\"background-color:%1;color:%2;white-space:pre-wrap;"
                            "font-family:monospace;padding:1px 4px;\">%3</div>")
                            .arg(hunkBg, hunkFg, escapeHtml(line.text));
                continue;
            }
            QString bg = QStringLiteral("transparent");
            QString marker = QStringLiteral(" ");
            if (line.kind == DiffLine::Added) {
                bg = addBg;
                marker = QStringLiteral("+");
            } else if (line.kind == DiffLine::Removed) {
                bg = delBg;
                marker = QStringLiteral("-");
            }
            body += QStringLiteral(
                        "<div style=\"background-color:%1;white-space:pre-wrap;"
                        "font-family:monospace;padding:0 4px;\">"
                        "<span style=\"color:%2\">%3 %4 </span>%5 %6</div>")
                        .arg(bg, gutterFg,
                             escapeHtml(numCell(line.oldNo)), escapeHtml(numCell(line.newNo)),
                             marker, hl.render(line.text));
        }
    }

    // --- top bar ----------------------------------------------------------
    auto *summary = new QLabel(this);
    summary->setText(QStringLiteral("%1 file%2 changed · ")
                         .arg(files.size())
                         .arg(files.size() == 1 ? QString() : QStringLiteral("s"))
                     + QStringLiteral("<span style=\"color:%1\">+%2</span> "
                                      "<span style=\"color:%3\">-%4</span>")
                           .arg(addFg, QString::number(totalAdded),
                                delFg, QString::number(totalRemoved)));
    summary->setTextFormat(Qt::RichText);

    auto *jump = new QComboBox(this);
    for (const DiffFile &file : files) {
        jump->addItem(file.path.isEmpty() ? QStringLiteral("(file)") : file.path);
    }

    auto *topBar = new QHBoxLayout;
    topBar->setContentsMargins(10, 6, 10, 6);
    topBar->addWidget(summary, 1);
    topBar->addWidget(new QLabel(QStringLiteral("Jump to:"), this));
    topBar->addWidget(jump);

    auto *browser = new QTextBrowser(this);
    browser->setOpenExternalLinks(false);
    browser->setStyleSheet(QStringLiteral("QTextBrowser { background-color:%1; border:none; }")
                               .arg(codeBg.name()));
    if (files.isEmpty()) {
        browser->setHtml(QStringLiteral("<p style=\"padding:16px;\">No changes.</p>"));
    } else {
        browser->setHtml(body);
    }

    connect(jump, &QComboBox::activated, this, [browser](int index) {
        browser->scrollToAnchor(QStringLiteral("file%1").arg(index));
    });

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addLayout(topBar);
    layout->addWidget(browser, 1);
}
