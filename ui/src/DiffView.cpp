#include "DiffView.h"

#include "shell/FlowLayout.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>
#include <KSyntaxHighlighting/AbstractHighlighter>
#include <KSyntaxHighlighting/Definition>
#include <KSyntaxHighlighting/Format>
#include <KSyntaxHighlighting/Repository>
#include <KSyntaxHighlighting/State>
#include <KSyntaxHighlighting/Theme>

#include <QComboBox>
#include <QIcon>
#include <QLabel>
#include <QPalette>
#include <QRegularExpression>
#include <QScrollBar>
#include <QSplitter>
#include <QStackedWidget>
#include <QTextBrowser>
#include <QToolButton>
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
    QString oldPath; // set when the file was renamed
    bool binary = false;
    bool renamed = false;
    int added = 0;
    int removed = 0;
    QList<DiffLine> lines;
};

// Colours used by both the inline and side-by-side renderers. Derived from the
// active palette so the diff tracks the user's Breeze colour scheme.
struct DiffPalette {
    QString addBg;
    QString delBg;
    QString hunkBg;
    QString hunkFg;
    QString gutterFg;
    QString headBg;
    QString addFg;
    QString delFg;
    QColor codeBg;
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
        // "Binary files a/foo and b/foo differ" — git's marker for a binary
        // change. No textual content follows.
        if (raw.startsWith(QLatin1String("Binary files "))
            || raw.startsWith(QLatin1String("GIT binary patch"))) {
            if (!cur) {
                files.append(DiffFile{});
                cur = &files.last();
            }
            cur->binary = true;
            continue;
        }
        // Rename headers carry the old/new path even when there is no hunk
        // (a pure rename has zero content lines).
        if (raw.startsWith(QLatin1String("rename from "))) {
            if (!cur) {
                files.append(DiffFile{});
                cur = &files.last();
            }
            cur->renamed = true;
            cur->oldPath = raw.sliced(12).trimmed(); // len("rename from ")
            continue;
        }
        if (raw.startsWith(QLatin1String("rename to "))) {
            if (!cur) {
                files.append(DiffFile{});
                cur = &files.last();
            }
            cur->renamed = true;
            if (cur->path.isEmpty()) {
                cur->path = raw.sliced(10).trimmed(); // len("rename to ")
            }
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
            if (p != QLatin1String("/dev/null")) {
                cur->path = p;
            }
            continue;
        }
        if (raw.startsWith(QLatin1String("--- "))) {
            // Capture the old path for the side-by-side header / rename label
            // when no explicit rename header was present.
            QString p = raw.mid(4).trimmed();
            if (p.startsWith(QLatin1String("a/"))) {
                p = p.mid(2);
            }
            if (cur && cur->oldPath.isEmpty() && p != QLatin1String("/dev/null")) {
                cur->oldPath = p;
            }
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

QString fileHeaderHtml(const DiffFile &file, int index, const DiffPalette &c, bool first)
{
    QString label;
    if (file.renamed && !file.oldPath.isEmpty() && file.oldPath != file.path) {
        label = i18nc("renamed file header in a diff, old → new",
                      "Renamed: %1 → %2",
                      escapeHtml(file.oldPath), escapeHtml(file.path));
    } else {
        label = QStringLiteral("<b>%1</b>")
                    .arg(escapeHtml(file.path.isEmpty()
                                        ? i18nc("placeholder for an unnamed file in a diff",
                                                "(file)")
                                        : file.path));
    }
    return QStringLiteral(
               "<a name=\"file%1\"></a>"
               "<div style=\"background-color:%2;padding:7px 10px;margin-top:%3;\">"
               "%4 &nbsp; <span style=\"color:%5\">+%6</span> "
               "<span style=\"color:%7\">-%8</span></div>")
        .arg(QString::number(index), c.headBg,
             first ? QStringLiteral("0") : QStringLiteral("14px"),
             label,
             c.addFg, QString::number(file.added),
             c.delFg, QString::number(file.removed));
}

// Render the full inline body (per-file sections, old+new gutters).
QString renderInline(const QList<DiffFile> &files, const DiffPalette &c,
                     KSyntaxHighlighting::Repository &repo,
                     const KSyntaxHighlighting::Theme &theme)
{
    QString body;
    for (int fi = 0; fi < files.size(); ++fi) {
        const DiffFile &file = files.at(fi);
        body += fileHeaderHtml(file, fi, c, fi == 0);

        if (file.binary) {
            body += QStringLiteral(
                        "<div style=\"padding:6px 10px;font-style:italic;\">%1</div>")
                        .arg(escapeHtml(i18n("Binary file — no textual diff")));
            continue;
        }

        HtmlHighlighter hl;
        hl.setDefinition(repo.definitionForFileName(file.path));
        hl.setTheme(theme);

        for (const DiffLine &line : file.lines) {
            if (line.kind == DiffLine::Hunk) {
                hl.resetState();
                body += QStringLiteral(
                            "<div style=\"background-color:%1;color:%2;white-space:pre-wrap;"
                            "font-family:monospace;padding:1px 4px;\">%3</div>")
                            .arg(c.hunkBg, c.hunkFg, escapeHtml(line.text));
                continue;
            }
            QString bg = QStringLiteral("transparent");
            QString marker = QStringLiteral(" ");
            if (line.kind == DiffLine::Added) {
                bg = c.addBg;
                marker = QStringLiteral("+");
            } else if (line.kind == DiffLine::Removed) {
                bg = c.delBg;
                marker = QStringLiteral("-");
            }
            body += QStringLiteral(
                        "<div style=\"background-color:%1;white-space:pre-wrap;"
                        "font-family:monospace;padding:0 4px;\">"
                        "<span style=\"color:%2\">%3 %4 </span>%5 %6</div>")
                        .arg(bg, c.gutterFg,
                             escapeHtml(numCell(line.oldNo)), escapeHtml(numCell(line.newNo)),
                             marker, hl.render(line.text));
        }
    }
    return body;
}

// Render one side of the split view. side==Removed builds the "old" pane
// (context + removed lines); side==Added builds the "new" pane (context +
// added lines). Hunk separators and file headers stay aligned across panes.
QString renderSide(const QList<DiffFile> &files, const DiffPalette &c,
                   KSyntaxHighlighting::Repository &repo,
                   const KSyntaxHighlighting::Theme &theme, DiffLine::Kind side)
{
    QString body;
    for (int fi = 0; fi < files.size(); ++fi) {
        const DiffFile &file = files.at(fi);
        body += fileHeaderHtml(file, fi, c, fi == 0);

        if (file.binary) {
            body += QStringLiteral(
                        "<div style=\"padding:6px 10px;font-style:italic;\">%1</div>")
                        .arg(escapeHtml(i18n("Binary file — no textual diff")));
            continue;
        }

        HtmlHighlighter hl;
        hl.setDefinition(repo.definitionForFileName(file.path));
        hl.setTheme(theme);

        for (const DiffLine &line : file.lines) {
            if (line.kind == DiffLine::Hunk) {
                hl.resetState();
                body += QStringLiteral(
                            "<div style=\"background-color:%1;color:%2;white-space:pre-wrap;"
                            "font-family:monospace;padding:1px 4px;\">%3</div>")
                            .arg(c.hunkBg, c.hunkFg, escapeHtml(line.text));
                continue;
            }
            // Context lines appear on both sides. A change line appears only on
            // its own side; the opposite side gets a blank filler so rows stay
            // vertically aligned.
            const bool isContext = line.kind == DiffLine::Context;
            const bool mine = isContext || line.kind == side;
            QString bg = QStringLiteral("transparent");
            QString marker = QStringLiteral(" ");
            int no = (side == DiffLine::Removed) ? line.oldNo : line.newNo;
            QString content;
            if (mine) {
                if (line.kind == DiffLine::Added) {
                    bg = c.addBg;
                    marker = QStringLiteral("+");
                } else if (line.kind == DiffLine::Removed) {
                    bg = c.delBg;
                    marker = QStringLiteral("-");
                }
                content = hl.render(line.text);
            } else {
                // Filler row on the opposite side.
                no = -1;
                content = QString();
            }
            body += QStringLiteral(
                        "<div style=\"background-color:%1;white-space:pre-wrap;"
                        "font-family:monospace;padding:0 4px;\">"
                        "<span style=\"color:%2\">%3 </span>%4 %5</div>")
                        .arg(bg, c.gutterFg,
                             escapeHtml(numCell(no)), marker, content);
        }
    }
    return body;
}

} // namespace

DiffView::DiffView(const QString &unifiedDiff, QWidget *parent)
    : QWidget(parent)
    , m_unifiedDiff(unifiedDiff)
    , m_emptyMessage(i18n("No changes."))
{
    // Persisted preference for inline vs side-by-side.
    const KConfigGroup grp =
        KSharedConfig::openConfig()->group(QStringLiteral("Git"));
    m_sideBySide = grp.readEntry("DiffSideBySide", false);

    const QList<DiffFile> files = parseDiff(m_unifiedDiff);

    // --- top bar ----------------------------------------------------------
    const AkColors &ak = ThemeManager::palette();
    const QString addFg = ak.positive.name();
    const QString delFg = ak.negative.name();

    int totalAdded = 0;
    int totalRemoved = 0;
    for (const DiffFile &file : files) {
        totalAdded += file.added;
        totalRemoved += file.removed;
    }

    // Plain QLabel (not ElidingLabel): the summary is short and carries colored
    // rich text (+N / -N), which an eliding plain-text label can't render. It
    // stays in the FlowLayout so the controls after it wrap when narrow.
    auto *summary = new QLabel(this);
    summary->setText(
        i18np("%1 file changed · ", "%1 files changed · ", files.size())
        + QStringLiteral("<span style=\"color:%1\">+%2</span> "
                         "<span style=\"color:%3\">-%4</span>")
              .arg(addFg, QString::number(totalAdded),
                   delFg, QString::number(totalRemoved)));
    summary->setTextFormat(Qt::RichText);

    auto *jump = new QComboBox(this);
    for (const DiffFile &file : files) {
        jump->addItem(file.path.isEmpty()
                          ? i18nc("placeholder for an unnamed file in a diff", "(file)")
                          : file.path);
    }

    m_splitBtn = new QToolButton(this);
    m_splitBtn->setIcon(QIcon::fromTheme(QStringLiteral("view-split-left-right")));
    m_splitBtn->setCheckable(true);
    m_splitBtn->setChecked(m_sideBySide);
    m_splitBtn->setToolTip(i18n("Toggle side-by-side view"));
    m_splitBtn->setAutoRaise(true);

    // FlowLayout so the toggle, "Jump to:" label and file combo wrap below the
    // (eliding) summary when the panel is dragged narrow instead of clipping.
    auto *topBar = new FlowLayout;
    topBar->setContentsMargins(10, 6, 10, 6);
    topBar->addWidget(summary);
    topBar->addWidget(m_splitBtn);
    topBar->addWidget(new QLabel(i18nc("@label jump-to-file selector", "Jump to:"), this));
    topBar->addWidget(jump);

    // --- panes ------------------------------------------------------------
    m_inline = new QTextBrowser(this);
    m_inline->setOpenExternalLinks(false);

    auto *splitHost = new QSplitter(Qt::Horizontal, this);
    m_leftPane = new QTextBrowser(splitHost);
    m_rightPane = new QTextBrowser(splitHost);
    m_leftPane->setOpenExternalLinks(false);
    m_rightPane->setOpenExternalLinks(false);
    splitHost->addWidget(m_leftPane);
    splitHost->addWidget(m_rightPane);

    m_stack = new QStackedWidget(this);
    m_stack->addWidget(m_inline);   // index 0
    m_stack->addWidget(splitHost);  // index 1

    // Keep the two side-by-side panes scrolled together so changed lines line
    // up as the user reads down.
    connect(m_leftPane->verticalScrollBar(), &QScrollBar::valueChanged,
            m_rightPane->verticalScrollBar(), &QScrollBar::setValue);
    connect(m_rightPane->verticalScrollBar(), &QScrollBar::valueChanged,
            m_leftPane->verticalScrollBar(), &QScrollBar::setValue);

    connect(jump, &QComboBox::activated, this, [this](int index) {
        const QString anchor = QStringLiteral("file%1").arg(index);
        m_inline->scrollToAnchor(anchor);
        m_leftPane->scrollToAnchor(anchor);
        m_rightPane->scrollToAnchor(anchor);
    });

    connect(m_splitBtn, &QToolButton::toggled, this, [this](bool on) {
        m_sideBySide = on;
        KConfigGroup g = KSharedConfig::openConfig()->group(QStringLiteral("Git"));
        g.writeEntry("DiffSideBySide", on);
        m_stack->setCurrentIndex(on ? 1 : 0);
    });

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addLayout(topBar);
    layout->addWidget(m_stack, 1);

    // Re-render when the interface or editor syntax theme changes so an already
    // open diff tracks the new colours live, rather than only on its next open.
    connect(ThemeManager::instance(), &ThemeManager::changed, this, &DiffView::rebuild);

    m_stack->setCurrentIndex(m_sideBySide ? 1 : 0);
    rebuild();
}

void DiffView::setEmptyMessage(const QString &message)
{
    if (message == m_emptyMessage) {
        return;
    }
    m_emptyMessage = message;
    rebuild();
}

void DiffView::rebuild()
{
    const bool dark = palette().color(QPalette::Base).lightness() < 128;
    const AkColors &ak = ThemeManager::palette();
    DiffPalette c;
    c.addBg    = ak.addedBg.name();
    c.delBg    = ak.removedBg.name();
    c.hunkBg   = ak.hunkBg.name();
    c.hunkFg   = dark ? QStringLiteral("#7c9cc0") : QStringLiteral("#3b5b7a");
    c.gutterFg = dark ? QStringLiteral("#6b7280") : QStringLiteral("#9aa0a8");
    c.headBg   = dark ? QStringLiteral("#23272f") : QStringLiteral("#eef0f3");
    c.addFg    = ak.positive.name();
    c.delFg    = ak.negative.name();

    const QList<DiffFile> files = parseDiff(m_unifiedDiff);

    KSyntaxHighlighting::Repository &repo = sharedRepository();
    // Honour the app's chosen syntax theme when it names one; otherwise fall
    // back to a sensible default by light/dark.
    KSyntaxHighlighting::Theme theme;
    const QString wanted = ThemeManager::instance()->editorSyntaxTheme();
    if (!wanted.isEmpty()) {
        const KSyntaxHighlighting::Theme picked = repo.theme(wanted);
        if (picked.isValid()) {
            theme = picked;
        }
    }
    if (!theme.isValid()) {
        theme = repo.defaultTheme(
            dark ? KSyntaxHighlighting::Repository::DarkTheme
                 : KSyntaxHighlighting::Repository::LightTheme);
    }
    c.codeBg = QColor(theme.editorColor(KSyntaxHighlighting::Theme::BackgroundColor));

    const QString sheet =
        QStringLiteral("QTextBrowser { background-color:%1; border:none; }")
            .arg(c.codeBg.name());
    m_inline->setStyleSheet(sheet);
    m_leftPane->setStyleSheet(sheet);
    m_rightPane->setStyleSheet(sheet);

    if (files.isEmpty()) {
        const QString html = QStringLiteral("<p style=\"padding:16px;\">%1</p>")
                                 .arg(escapeHtml(m_emptyMessage));
        m_inline->setHtml(html);
        m_leftPane->setHtml(html);
        m_rightPane->setHtml(QString());
        return;
    }

    m_inline->setHtml(renderInline(files, c, repo, theme));
    m_leftPane->setHtml(renderSide(files, c, repo, theme, DiffLine::Removed));
    m_rightPane->setHtml(renderSide(files, c, repo, theme, DiffLine::Added));
}
