// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "ToolInspectorDialog.h"

#include "DiffView.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>
#include <KSyntaxHighlighting/Definition>
#include <KSyntaxHighlighting/Repository>
#include <KSyntaxHighlighting/SyntaxHighlighter>
#include <KSyntaxHighlighting/Theme>

#include <QCheckBox>
#include <QFontDatabase>
#include <QFormLayout>
#include <QGuiApplication>
#include <QClipboard>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QScrollArea>
#include <QTabWidget>
#include <QTextCursor>
#include <QToolButton>
#include <QVBoxLayout>
#include <QWidget>

namespace {

// The tool-call inspector paints its own body from model-supplied text; every
// content view here is a plain QPlainTextEdit fed via setPlainText, which is the
// safe path (no QTextDocument markdown parsing), so the MarkdownUtil neutralize
// step doesn't apply. DiffView owns its own syntax rendering.

// A read-only monospace text pane with the same tab-stop feel as the diff view.
QPlainTextEdit *monoView(const QString &text, QWidget *parent)
{
    auto *view = new QPlainTextEdit(parent);
    view->setReadOnly(true);
    view->setFont(QFontDatabase::systemFont(QFontDatabase::FixedFont));
    view->setLineWrapMode(QPlainTextEdit::NoWrap);
    view->setPlainText(text);
    return view;
}

// Resolve the app's chosen KSyntaxHighlighting theme (or a light/dark default),
// mirroring DiffView so highlighted panes track the user's colour scheme.
KSyntaxHighlighting::Theme resolveSyntaxTheme(KSyntaxHighlighting::Repository &repo,
                                              bool dark)
{
    KSyntaxHighlighting::Theme theme;
    const QString wanted = ThemeManager::instance()->syntaxTheme();
    if (!wanted.isEmpty()) {
        const KSyntaxHighlighting::Theme picked = repo.theme(wanted);
        if (picked.isValid()) {
            theme = picked;
        }
    }
    if (!theme.isValid()) {
        theme = repo.defaultTheme(dark ? KSyntaxHighlighting::Repository::DarkTheme
                                       : KSyntaxHighlighting::Repository::LightTheme);
    }
    return theme;
}

// Shared highlighting repository (one instance for the process, like DiffView).
KSyntaxHighlighting::Repository &sharedRepository()
{
    static KSyntaxHighlighting::Repository repo;
    return repo;
}

// Attach a KSyntaxHighlighting highlighter for `definitionName` to a view's
// document. The highlighter is parented to the view so it lives and dies with it.
void applyHighlighter(QPlainTextEdit *view, const QString &definitionName)
{
    KSyntaxHighlighting::Repository &repo = sharedRepository();
    const bool dark = view->palette().color(QPalette::Base).lightness() < 128;
    auto *hl = new KSyntaxHighlighting::SyntaxHighlighter(view->document());
    hl->setTheme(resolveSyntaxTheme(repo, dark));
    const KSyntaxHighlighting::Definition def = repo.definitionForName(definitionName);
    if (def.isValid()) {
        hl->setDefinition(def);
    }
}

// Build a unified diff between `oldText` and `newText` for a single `path` so
// DiffView (which parses git-style unified diffs) can render an Edit/Write as a
// real diff. A minimal whole-file hunk is enough: DiffView's parser only needs
// the file headers, an @@ line, and +/-/space-prefixed lines.
QString synthesizeUnifiedDiff(const QString &path, const QString &oldText,
                              const QString &newText)
{
    const QString safePath = path.isEmpty() ? QStringLiteral("file") : path;
    const QStringList oldLines =
        oldText.isEmpty() ? QStringList() : oldText.split(QLatin1Char('\n'));
    const QStringList newLines =
        newText.isEmpty() ? QStringList() : newText.split(QLatin1Char('\n'));

    QString diff;
    diff += QStringLiteral("diff --git a/%1 b/%1\n").arg(safePath);
    diff += QStringLiteral("--- a/%1\n").arg(oldText.isEmpty() ? QStringLiteral("/dev/null")
                                                               : safePath);
    diff += QStringLiteral("+++ b/%1\n").arg(newText.isEmpty() ? QStringLiteral("/dev/null")
                                                               : safePath);
    diff += QStringLiteral("@@ -%1,%2 +%3,%4 @@\n")
                .arg(oldLines.isEmpty() ? 0 : 1)
                .arg(oldLines.size())
                .arg(newLines.isEmpty() ? 0 : 1)
                .arg(newLines.size());
    for (const QString &l : oldLines) {
        diff += QLatin1Char('-') + l + QLatin1Char('\n');
    }
    for (const QString &l : newLines) {
        diff += QLatin1Char('+') + l + QLatin1Char('\n');
    }
    return diff;
}

} // namespace

ToolInspectorDialog::ToolInspectorDialog(const QString &toolName,
                                         const QString &inputJson,
                                         const QString &fullResult, bool resultCapped,
                                         QWidget *parent)
    : QDialog(parent)
{
    setAttribute(Qt::WA_DeleteOnClose);
    setWindowTitle(i18nc("@title:window tool-call inspector",
                         "Tool call — %1", toolName));

    const QJsonObject input =
        QJsonDocument::fromJson(inputJson.toUtf8()).object();

    auto *tabs = new QTabWidget(this);
    tabs->addTab(buildOverview(toolName, input, fullResult), i18n("Overview"));

    // --- Input tab: the full JSON, monospace + JSON highlighting -----------
    auto *inputView = monoView(inputJson.isEmpty()
                                   ? i18n("(no input)")
                                   : inputJson,
                               this);
    if (!inputJson.isEmpty()) {
        applyHighlighter(inputView, QStringLiteral("JSON"));
    }
    tabs->addTab(inputView, i18n("Input"));

    // --- Result tab --------------------------------------------------------
    tabs->addTab(buildResult(fullResult, resultCapped), i18n("Result"));

    auto *close = new QPushButton(i18n("Close"), this);
    connect(close, &QPushButton::clicked, this, &QDialog::accept);
    auto *btnRow = new QHBoxLayout;
    btnRow->addStretch(1);
    btnRow->addWidget(close);

    auto *root = new QVBoxLayout(this);
    root->addWidget(tabs, 1);
    root->addLayout(btnRow);

    // Remembered size (persisted in KConfig like the other dialogs).
    const KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("ToolInspector"));
    resize(cfg.readEntry("size", QSize(760, 620)));
}

ToolInspectorDialog::~ToolInspectorDialog()
{
    KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("ToolInspector"));
    cfg.writeEntry("size", size());
}

QWidget *ToolInspectorDialog::buildOverview(const QString &toolName,
                                            const QJsonObject &input,
                                            const QString &fullResult)
{
    auto *host = new QWidget(this);
    auto *lay = new QVBoxLayout(host);
    lay->setContentsMargins(8, 8, 8, 8);
    lay->setSpacing(8);

    // A titled section: a muted caption label above the given widget.
    const auto addSection = [&](const QString &caption, QWidget *w, int stretch = 0) {
        auto *cap = new QLabel(caption, host);
        QFont f = cap->font();
        f.setBold(true);
        cap->setFont(f);
        lay->addWidget(cap);
        lay->addWidget(w, stretch);
    };

    // A clickable file-path label that relays to the editor via openFile().
    const auto addPathRow = [&](const QString &path) {
        if (path.isEmpty()) {
            return;
        }
        auto *link = new QLabel(host);
        link->setText(QStringLiteral("<a href=\"open\">%1</a>").arg(path.toHtmlEscaped()));
        link->setTextInteractionFlags(Qt::TextBrowserInteraction);
        connect(link, &QLabel::linkActivated, this,
                [this, path] { emit openFile(path); });
        addSection(i18n("File"), link);
    };

    const QString name = toolName;

    if (name == QLatin1String("Bash")) {
        const QString cmd = input.value(QStringLiteral("command")).toString();
        const QString desc = input.value(QStringLiteral("description")).toString();
        if (!desc.isEmpty()) {
            auto *d = new QLabel(desc, host);
            d->setWordWrap(true);
            addSection(i18n("Description"), d);
        }
        auto *cmdView = monoView(cmd, host);
        cmdView->setLineWrapMode(QPlainTextEdit::WidgetWidth);
        cmdView->setMaximumHeight(140);
        applyHighlighter(cmdView, QStringLiteral("Bash"));
        addSection(i18n("Command"), cmdView);
        // Console-styled output.
        auto *out = monoView(fullResult, host);
        addSection(i18n("Output"), out, 1);

    } else if (name == QLatin1String("Read")) {
        addPathRow(input.value(QStringLiteral("file_path")).toString());
        auto *content = monoView(fullResult, host);
        addSection(i18n("Content"), content, 1);

    } else if (name == QLatin1String("Edit") || name == QLatin1String("Write")) {
        const QString path = input.value(QStringLiteral("file_path")).toString();
        addPathRow(path);
        QString oldText;
        QString newText;
        if (name == QLatin1String("Write")) {
            newText = input.value(QStringLiteral("content")).toString();
        } else {
            oldText = input.value(QStringLiteral("old_string")).toString();
            newText = input.value(QStringLiteral("new_string")).toString();
        }
        auto *diff =
            new DiffView(synthesizeUnifiedDiff(path, oldText, newText), host);
        addSection(i18n("Changes"), diff, 1);

    } else if (name == QLatin1String("MultiEdit")) {
        const QString path = input.value(QStringLiteral("file_path")).toString();
        addPathRow(path);
        // Concatenate each edit into one synthetic diff so a MultiEdit reads as
        // one review. Each edit is its own file section in DiffView.
        QString diff;
        const QJsonArray edits = input.value(QStringLiteral("edits")).toArray();
        int n = 0;
        for (const QJsonValue &ev : edits) {
            const QJsonObject e = ev.toObject();
            const QString label =
                QStringLiteral("%1 (edit %2)").arg(path.isEmpty() ? QStringLiteral("file")
                                                                  : path)
                    .arg(++n);
            diff += synthesizeUnifiedDiff(label,
                                          e.value(QStringLiteral("old_string")).toString(),
                                          e.value(QStringLiteral("new_string")).toString());
        }
        auto *view = new DiffView(diff, host);
        view->setEmptyMessage(i18n("No edits."));
        addSection(i18n("Changes"), view, 1);

    } else if (name == QLatin1String("Grep") || name == QLatin1String("Glob")) {
        const QString pattern = input.value(QStringLiteral("pattern")).toString();
        auto *pat = new QLabel(pattern, host);
        pat->setTextInteractionFlags(Qt::TextSelectableByMouse);
        pat->setFont(QFontDatabase::systemFont(QFontDatabase::FixedFont));
        addSection(i18n("Pattern"), pat);
        const QString path = input.value(QStringLiteral("path")).toString();
        if (!path.isEmpty()) {
            auto *p = new QLabel(path, host);
            p->setTextInteractionFlags(Qt::TextSelectableByMouse);
            addSection(i18n("In"), p);
        }
        auto *hits = monoView(fullResult, host);
        addSection(i18n("Matches"), hits, 1);

    } else {
        // Fallback: a key/value form of the input's top-level fields.
        auto *form = new QWidget(host);
        auto *fl = new QFormLayout(form);
        fl->setContentsMargins(0, 0, 0, 0);
        fl->setRowWrapPolicy(QFormLayout::WrapLongRows);
        const QStringList keys = input.keys();
        for (const QString &k : keys) {
            const QJsonValue v = input.value(k);
            QString text;
            if (v.isString()) {
                text = v.toString();
            } else if (v.isDouble()) {
                text = QString::number(v.toDouble());
            } else if (v.isBool()) {
                text = v.toBool() ? QStringLiteral("true") : QStringLiteral("false");
            } else {
                // Arrays/objects: compact JSON so the form stays readable.
                text = QString::fromUtf8(
                    QJsonDocument(v.isArray() ? QJsonDocument(v.toArray())
                                              : QJsonDocument(v.toObject()))
                        .toJson(QJsonDocument::Compact));
            }
            auto *val = new QLabel(text, form);
            val->setWordWrap(true);
            val->setTextInteractionFlags(Qt::TextSelectableByMouse);
            fl->addRow(k + QStringLiteral(":"), val);
        }
        if (keys.isEmpty()) {
            fl->addRow(new QLabel(i18n("(no input fields)"), form));
        }
        auto *scroll = new QScrollArea(host);
        scroll->setWidgetResizable(true);
        scroll->setFrameShape(QFrame::NoFrame);
        scroll->setWidget(form);
        addSection(i18n("Input"), scroll);
        // Also show the result for unknown tools so the Overview is useful.
        auto *out = monoView(fullResult, host);
        addSection(i18n("Result"), out, 1);
    }

    return host;
}

QWidget *ToolInspectorDialog::buildResult(const QString &fullResult, bool resultCapped)
{
    auto *host = new QWidget(this);
    auto *lay = new QVBoxLayout(host);
    lay->setContentsMargins(0, 0, 0, 0);
    lay->setSpacing(4);

    m_resultView = monoView(fullResult.isEmpty() ? i18n("(no output)") : fullResult,
                            host);

    // --- toolbar: wrap toggle, find bar, copy -----------------------------
    auto *bar = new QHBoxLayout;
    bar->setContentsMargins(6, 4, 6, 0);

    auto *wrap = new QCheckBox(i18n("Wrap"), host);
    connect(wrap, &QCheckBox::toggled, this, [this](bool on) {
        m_resultView->setLineWrapMode(on ? QPlainTextEdit::WidgetWidth
                                         : QPlainTextEdit::NoWrap);
    });
    bar->addWidget(wrap);

    m_findEdit = new QLineEdit(host);
    m_findEdit->setPlaceholderText(i18n("Find in output…"));
    m_findEdit->setClearButtonEnabled(true);
    connect(m_findEdit, &QLineEdit::returnPressed, this,
            [this] { runResultFind(true); });
    bar->addWidget(m_findEdit, 1);

    auto *findNext = new QToolButton(host);
    findNext->setText(i18n("Next"));
    connect(findNext, &QToolButton::clicked, this, [this] { runResultFind(true); });
    bar->addWidget(findNext);

    auto *findPrev = new QToolButton(host);
    findPrev->setText(i18n("Prev"));
    connect(findPrev, &QToolButton::clicked, this, [this] { runResultFind(false); });
    bar->addWidget(findPrev);

    auto *copy = new QToolButton(host);
    copy->setText(i18n("Copy"));
    connect(copy, &QToolButton::clicked, this, [fullResult] {
        QGuiApplication::clipboard()->setText(fullResult);
    });
    bar->addWidget(copy);

    lay->addLayout(bar);
    lay->addWidget(m_resultView, 1);

    if (resultCapped) {
        auto *note = new QLabel(
            i18n("Output was truncated to 128 KB for display — the full result is "
                 "preserved in the on-disk transcript."),
            host);
        note->setWordWrap(true);
        note->setForegroundRole(QPalette::Mid);
        lay->addWidget(note);
    }

    return host;
}

void ToolInspectorDialog::runResultFind(bool forward)
{
    if (!m_resultView || !m_findEdit) {
        return;
    }
    const QString needle = m_findEdit->text();
    if (needle.isEmpty()) {
        return;
    }
    QTextDocument::FindFlags flags;
    if (!forward) {
        flags |= QTextDocument::FindBackward;
    }
    if (!m_resultView->find(needle, flags)) {
        // Wrap around from the document edge and try once more.
        QTextCursor c = m_resultView->textCursor();
        c.movePosition(forward ? QTextCursor::Start : QTextCursor::End);
        m_resultView->setTextCursor(c);
        m_resultView->find(needle, flags);
    }
}
