#include "SearchPanel.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDir>
#include <QFont>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QPointer>
#include <QRegularExpression>
#include <QTimer>
#include <QToolButton>
#include <QTreeWidget>
#include <QVBoxLayout>

namespace {
constexpr int kDebounceMs = 220;
constexpr int kMaxResults = 2000;

QToolButton *makeToggle(const QString &label, const QString &tip, const QString &icon)
{
    auto *b = new QToolButton;
    b->setCheckable(true);
    b->setText(label);
    b->setToolTip(tip);
    if (!icon.isEmpty()) {
        b->setIcon(QIcon::fromTheme(icon));
    }
    b->setAutoRaise(true);
    return b;
}
} // namespace

SearchPanel::SearchPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    auto *outer = new QVBoxLayout(this);
    outer->setContentsMargins(6, 6, 6, 6);
    outer->setSpacing(4);

    // Row 1: query + toggles.
    auto *row1 = new QHBoxLayout;
    row1->setSpacing(4);
    m_query = new QLineEdit(this);
    m_query->setPlaceholderText(i18n("Search in project…"));
    m_query->setClearButtonEnabled(true);
    row1->addWidget(m_query, 1);

    m_caseBtn = makeToggle(QStringLiteral("Aa"),
                           i18n("Match Case"), QString());
    m_wordBtn = makeToggle(QStringLiteral("❖"),
                           i18n("Match Whole Word"), QString());
    m_regexBtn = makeToggle(QStringLiteral(".*"),
                            i18n("Use Regular Expression"), QString());
    row1->addWidget(m_caseBtn);
    row1->addWidget(m_wordBtn);
    row1->addWidget(m_regexBtn);
    outer->addLayout(row1);

    // Row 2: include / exclude globs.
    auto *row2 = new QHBoxLayout;
    row2->setSpacing(4);
    m_include = new QLineEdit(this);
    m_include->setPlaceholderText(i18n("Files to include (e.g. *.cpp, src/**)"));
    m_include->setClearButtonEnabled(true);
    m_exclude = new QLineEdit(this);
    m_exclude->setPlaceholderText(i18n("Files to exclude (e.g. build/**, *.min.js)"));
    m_exclude->setClearButtonEnabled(true);
    row2->addWidget(m_include, 1);
    row2->addWidget(m_exclude, 1);
    outer->addLayout(row2);

    // Results tree.
    m_results = new QTreeWidget(this);
    m_results->setHeaderHidden(true);
    m_results->setUniformRowHeights(true);
    m_results->setRootIsDecorated(true);
    m_results->setAlternatingRowColors(true);
    QFont mono = m_results->font();
    mono.setFamily(QStringLiteral("monospace"));
    mono.setStyleHint(QFont::TypeWriter);
    m_results->setFont(mono);
    outer->addWidget(m_results, 1);

    m_status = new QLabel(this);
    m_status->setStyleSheet(QStringLiteral("color: palette(mid);"));
    outer->addWidget(m_status);

    m_debounce = new QTimer(this);
    m_debounce->setSingleShot(true);
    m_debounce->setInterval(kDebounceMs);
    connect(m_debounce, &QTimer::timeout, this, &SearchPanel::runSearch);

    auto bump = [this] { scheduleSearch(); };
    connect(m_query, &QLineEdit::textChanged, this, bump);
    connect(m_include, &QLineEdit::textChanged, this, bump);
    connect(m_exclude, &QLineEdit::textChanged, this, bump);
    connect(m_caseBtn, &QToolButton::toggled, this, bump);
    connect(m_wordBtn, &QToolButton::toggled, this, bump);
    connect(m_regexBtn, &QToolButton::toggled, this, bump);

    connect(m_results, &QTreeWidget::itemActivated, this,
            [this](QTreeWidgetItem *item, int) {
                if (!item) {
                    return;
                }
                const QString path = item->data(0, Qt::UserRole).toString();
                if (path.isEmpty()) {
                    return;
                }
                const int line = item->data(0, Qt::UserRole + 1).toInt();
                Q_EMIT resultActivated(path, line);
            });

    setProjectRoot(QString());
}

void SearchPanel::setProjectRoot(const QString &root)
{
    if (m_root == root) {
        return;
    }
    m_root = root;
    clearResults();
    if (root.isEmpty()) {
        m_status->setText(i18n("No project selected."));
        m_query->setEnabled(false);
    } else {
        m_status->setText(i18n("Searching in %1", QDir(root).dirName()));
        m_query->setEnabled(true);
        if (!m_query->text().isEmpty()) {
            scheduleSearch();
        }
    }
}

void SearchPanel::focusQuery()
{
    m_query->setFocus();
    m_query->selectAll();
}

void SearchPanel::search(const QString &query)
{
    const QString trimmed = query.trimmed();
    if (trimmed.isEmpty())
        return;
    // Setting the text drives textChanged → scheduleSearch() → runSearch(),
    // reusing the panel's debounce, toggles and workspace-scoped root.
    m_query->setText(query);
    m_query->setFocus();
}

void SearchPanel::scheduleSearch()
{
    m_debounce->start();
}

void SearchPanel::clearResults()
{
    m_results->clear();
}

void SearchPanel::runSearch()
{
    if (!m_core || !m_core->isConnected() || m_root.isEmpty()) {
        clearResults();
        return;
    }
    const QString q = m_query->text();
    if (q.isEmpty()) {
        clearResults();
        m_status->setText(i18n("Searching in %1", QDir(m_root).dirName()));
        return;
    }

    auto splitGlobs = [](const QString &s) {
        QJsonArray a;
        const QStringList parts = s.split(QRegularExpression(QStringLiteral("[,\\s]+")),
                                          Qt::SkipEmptyParts);
        for (const QString &p : parts) {
            a.append(p);
        }
        return a;
    };

    QJsonObject params{
        {QStringLiteral("query"), q},
        {QStringLiteral("root"), m_root},
        {QStringLiteral("regex"), m_regexBtn->isChecked()},
        {QStringLiteral("caseSensitive"), m_caseBtn->isChecked()},
        {QStringLiteral("wholeWord"), m_wordBtn->isChecked()},
        {QStringLiteral("includes"), splitGlobs(m_include->text())},
        {QStringLiteral("excludes"), splitGlobs(m_exclude->text())},
        {QStringLiteral("maxResults"), kMaxResults},
    };

    m_status->setText(i18n("Searching…"));
    const quint64 seq = ++m_seq;
    QPointer<SearchPanel> self(this);
    m_core->call(QStringLiteral("search.code"), params,
                 [self, seq](const QJsonObject &result, const QJsonObject &error) {
                     if (!self || seq != self->m_seq) {
                         return;
                     }
                     self->onReply(result, error);
                 });
}

void SearchPanel::onReply(const QJsonObject &result, const QJsonObject &error)
{
    clearResults();
    if (!error.isEmpty()) {
        m_status->setText(i18n("Search failed: %1",
                               error.value(QStringLiteral("message")).toString()));
        return;
    }
    const QJsonArray files = result.value(QStringLiteral("files")).toArray();
    int total = 0;
    const QDir rootDir(m_root);
    for (const QJsonValue &fv : files) {
        const QJsonObject f = fv.toObject();
        const QString path = f.value(QStringLiteral("path")).toString();
        const QJsonArray matches = f.value(QStringLiteral("matches")).toArray();
        auto *parent = new QTreeWidgetItem(m_results);
        parent->setText(0,
                        QStringLiteral("%1   (%2)")
                            .arg(rootDir.relativeFilePath(path))
                            .arg(matches.size()));
        parent->setToolTip(0, path);
        parent->setFirstColumnSpanned(true);
        for (const QJsonValue &mv : matches) {
            const QJsonObject m = mv.toObject();
            // ripgrep returns 1-indexed line numbers, but EditorArea::openFile
            // (and KTextEditor::Cursor) is 0-indexed. Store the 0-indexed
            // value for the result row and display the 1-indexed value.
            const int lineOneBased = m.value(QStringLiteral("line")).toInt();
            const int lineZeroBased = lineOneBased > 0 ? lineOneBased - 1 : 0;
            const QString preview =
                m.value(QStringLiteral("preview")).toString().trimmed();
            auto *row = new QTreeWidgetItem(parent);
            row->setText(0, QStringLiteral("%1: %2").arg(lineOneBased, 5).arg(preview));
            row->setData(0, Qt::UserRole, path);
            row->setData(0, Qt::UserRole + 1, lineZeroBased);
            ++total;
        }
        parent->setExpanded(true);
    }
    const bool truncated = result.value(QStringLiteral("truncated")).toBool();
    if (total == 0) {
        m_status->setText(i18n("No matches."));
    } else if (truncated) {
        m_status->setText(
            i18n("%1 matches in %2 files (truncated at %3).",
                 total, files.size(), kMaxResults));
    } else {
        m_status->setText(i18n("%1 matches in %2 files.", total, files.size()));
    }
}
