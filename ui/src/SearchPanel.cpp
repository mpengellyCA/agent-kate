#include "SearchPanel.h"
#include "ipc/CoreClient.h"

#include <KHistoryComboBox>
#include <KLocalizedString>
#include <KSharedConfig>
#include <KConfigGroup>

#include <QAction>
#include <QApplication>
#include <QDir>
#include <QEvent>
#include <QFont>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QIcon>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QKeyEvent>
#include <QLabel>
#include <QLineEdit>
#include <QMenu>
#include <QMimeData>
#include <QPainter>
#include <QPointer>
#include <QRegularExpression>
#include <QSet>
#include <QStyledItemDelegate>
#include <QTextLayout>
#include <QTimer>
#include <QToolButton>
#include <QTreeWidget>
#include <QUrl>
#include <QVBoxLayout>

namespace {
constexpr int kDebounceMs = 220;
constexpr int kMaxResults = 2000;
constexpr int kMaxHistory = 30;
constexpr char kConfigGroup[] = "Search";
constexpr char kConfigKey[] = "history";

// Custom drag MIME carrying per-hit line ranges, mirrored in AgentPanel.cpp.
// Drops fall back to the standard text/uri-list when this type is absent.
constexpr char kAttachMime[] = "application/x-agentkate-attachment+json";

// Roles on result rows, beyond the default UserRole (path) and UserRole+1
// (0-based line). Column is 0-based; HiStart/HiLen address the highlighted
// span within the displayed row text.
constexpr int kRoleColumn = Qt::UserRole + 2;
constexpr int kRoleHiStart = Qt::UserRole + 3;
constexpr int kRoleHiLen = Qt::UserRole + 4;

QToolButton *makeToggle(const QString &label, const QString &tip,
                        const QString &accessible, const QString &icon)
{
    auto *b = new QToolButton;
    b->setCheckable(true);
    b->setText(label);
    b->setToolTip(tip);
    b->setAccessibleName(accessible);
    if (!icon.isEmpty()) {
        b->setIcon(QIcon::fromTheme(icon));
    }
    b->setAutoRaise(true);
    return b;
}

// HighlightDelegate paints a result row's match text with the matched span
// tinted using the user's color scheme (QPalette::Highlight), never a
// hardcoded color. Parent rows (no highlight span) fall back to the base
// painter so file headers render normally.
class HighlightDelegate : public QStyledItemDelegate
{
public:
    using QStyledItemDelegate::QStyledItemDelegate;

    void paint(QPainter *painter, const QStyleOptionViewItem &option,
               const QModelIndex &index) const override
    {
        const int hiLen = index.data(kRoleHiLen).toInt();
        if (hiLen <= 0) {
            QStyledItemDelegate::paint(painter, option, index);
            return;
        }

        QStyleOptionViewItem opt(option);
        initStyleOption(&opt, index);
        const QString text = opt.text;
        opt.text.clear(); // draw background/selection only; we paint the text

        QStyle *style = opt.widget ? opt.widget->style() : QApplication::style();
        style->drawControl(QStyle::CE_ItemViewItem, &opt, painter, opt.widget);

        const int hiStart = qBound(0, index.data(kRoleHiStart).toInt(), text.length());
        const int hiEnd = qMin(hiStart + hiLen, text.length());

        QRect textRect = style->subElementRect(QStyle::SE_ItemViewItemText,
                                               &opt, opt.widget);
        textRect.adjust(2, 0, -2, 0);

        const QPalette &pal = opt.palette;
        const bool selected = opt.state & QStyle::State_Selected;
        const QColor normalFg =
            selected ? pal.color(QPalette::HighlightedText) : pal.color(QPalette::Text);

        const QString elided =
            opt.fontMetrics.elidedText(text, opt.textElideMode, textRect.width());
        // Only highlight when the span survives elision (no ellipsis cut into it).
        const bool spanVisible = elided == text && hiEnd > hiStart;

        QTextLayout layout(elided, opt.font, painter->device());
        QList<QTextLayout::FormatRange> formats;
        if (spanVisible) {
            QTextLayout::FormatRange hi;
            hi.start = hiStart;
            hi.length = hiEnd - hiStart;
            // Match-highlight uses the scheme's selection colors so it tracks
            // the user's Breeze/custom palette.
            hi.format.setBackground(pal.color(QPalette::Highlight));
            hi.format.setForeground(pal.color(QPalette::HighlightedText));
            formats.append(hi);
        }
        layout.setFormats(formats);

        painter->save();
        painter->setPen(normalFg);
        layout.beginLayout();
        QTextLine line = layout.createLine();
        line.setLineWidth(textRect.width());
        const int y = textRect.y() + (textRect.height() - opt.fontMetrics.height()) / 2;
        line.setPosition(QPointF(textRect.x(), y));
        layout.endLayout();
        layout.draw(painter, QPointF(0, 0));
        painter->restore();
    }
};

// QTreeWidget's default mimeData() emits an internal-only
// application/x-qabstractitemmodeldatalist payload, so a drag to another widget
// carries nothing usable. This subclass supplies both a text/uri-list of the
// distinct files (for AgentPanel's URL drop path) and a custom JSON payload of
// per-hit line ranges (so ranged attaches preserve the spanned lines).
class SearchResultsTree : public QTreeWidget
{
public:
    using QTreeWidget::QTreeWidget;

protected:
    QMimeData *mimeData(const QList<QTreeWidgetItem *> &items) const override
    {
        auto *mime = new QMimeData;
        QList<QUrl> urls;
        QSet<QString> seenUrls;
        QJsonArray hits;
        for (const QTreeWidgetItem *item : items) {
            if (!item) {
                continue;
            }
            const QString path = item->data(0, Qt::UserRole).toString();
            if (path.isEmpty()) {
                continue;
            }
            if (!seenUrls.contains(path)) {
                seenUrls.insert(path);
                urls << QUrl::fromLocalFile(path);
            }
            QJsonObject hit{{QStringLiteral("path"), path}};
            // Match rows carry a 0-based line and have a parent; file (parent)
            // rows do not, and attach whole-file.
            const QVariant lineVar = item->data(0, Qt::UserRole + 1);
            if (lineVar.isValid() && item->parent()) {
                hit[QStringLiteral("line")] = lineVar.toInt();
                hit[QStringLiteral("endLine")] = lineVar.toInt();
            }
            hits.append(hit);
        }
        if (!urls.isEmpty()) {
            mime->setUrls(urls);
        }
        if (!hits.isEmpty()) {
            mime->setData(QLatin1String(kAttachMime),
                          QJsonDocument(hits).toJson(QJsonDocument::Compact));
        }
        return mime;
    }
};
} // namespace

SearchPanel::SearchPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    auto *outer = new QVBoxLayout(this);
    outer->setContentsMargins(6, 6, 6, 6);
    outer->setSpacing(4);

    // Row 1: query (with recall history) + toggles + stop.
    auto *row1 = new QHBoxLayout;
    row1->setSpacing(4);
    m_query = new KHistoryComboBox(this);
    m_query->setPlaceholderText(i18n("Search in project…"));
    m_query->lineEdit()->setClearButtonEnabled(true);
    m_query->setDuplicatesEnabled(false);
    m_query->setMaxCount(kMaxHistory);
    // Seed recall list from prior sessions.
    {
        const KConfigGroup cfg =
            KSharedConfig::openConfig()->group(QLatin1String(kConfigGroup));
        m_query->setHistoryItems(cfg.readEntry(QLatin1String(kConfigKey), QStringList{}));
    }
    m_query->lineEdit()->installEventFilter(this);
    row1->addWidget(m_query, 1);

    m_caseBtn = makeToggle(QStringLiteral("Aa"), i18n("Match Case"),
                           i18n("Match Case"), QStringLiteral("format-text-superscript"));
    m_wordBtn = makeToggle(QStringLiteral("❖"), i18n("Match Whole Word"),
                           i18n("Match Whole Word"), QStringLiteral("draw-text"));
    m_regexBtn = makeToggle(QStringLiteral(".*"), i18n("Use Regular Expression"),
                            i18n("Use Regular Expression"),
                            QStringLiteral("code-context"));
    // Glyph labels read better than the generic theme icons for these toggles,
    // so keep the text and drop the icon while retaining the accessible name.
    m_caseBtn->setIcon(QIcon());
    m_wordBtn->setIcon(QIcon());
    m_regexBtn->setIcon(QIcon());
    row1->addWidget(m_caseBtn);
    row1->addWidget(m_wordBtn);
    row1->addWidget(m_regexBtn);

    m_stopBtn = new QToolButton(this);
    m_stopBtn->setIcon(QIcon::fromTheme(QStringLiteral("process-stop")));
    m_stopBtn->setToolTip(i18n("Stop the running search"));
    m_stopBtn->setAccessibleName(i18n("Stop Search"));
    m_stopBtn->setAutoRaise(true);
    m_stopBtn->setEnabled(false);
    row1->addWidget(m_stopBtn);
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
    // Two side-by-side fields would otherwise pin the left pane's minimum width
    // to the sum of their natural widths. Ignore their width hint so the row
    // compresses with the pane (the fields stay usable and scroll internally).
    m_include->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Fixed);
    m_exclude->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Fixed);
    row2->addWidget(m_include, 1);
    row2->addWidget(m_exclude, 1);
    outer->addLayout(row2);

    // Results tree.
    m_results = new SearchResultsTree(this);
    m_results->setHeaderHidden(true);
    m_results->setUniformRowHeights(true);
    m_results->setRootIsDecorated(true);
    m_results->setAlternatingRowColors(true);
    m_results->setItemDelegate(new HighlightDelegate(m_results));
    // Drag results out to the chat (or any URL drop target); right-click to
    // attach the selection as context.
    m_results->setDragEnabled(true);
    m_results->setDragDropMode(QAbstractItemView::DragOnly);
    m_results->setSelectionMode(QAbstractItemView::ExtendedSelection);
    m_results->setContextMenuPolicy(Qt::CustomContextMenu);
    QFont mono = m_results->font();
    mono.setFamily(QStringLiteral("monospace"));
    mono.setStyleHint(QFont::TypeWriter);
    m_results->setFont(mono);
    outer->addWidget(m_results, 1);

    m_status = new QLabel(this);
    m_status->setForegroundRole(QPalette::PlaceholderText);
    outer->addWidget(m_status);

    m_debounce = new QTimer(this);
    m_debounce->setSingleShot(true);
    m_debounce->setInterval(kDebounceMs);
    connect(m_debounce, &QTimer::timeout, this, &SearchPanel::runSearch);

    auto bump = [this] { scheduleSearch(); };
    connect(m_query, &KHistoryComboBox::editTextChanged, this, bump);
    connect(m_include, &QLineEdit::textChanged, this, bump);
    connect(m_exclude, &QLineEdit::textChanged, this, bump);
    connect(m_caseBtn, &QToolButton::toggled, this, bump);
    connect(m_wordBtn, &QToolButton::toggled, this, bump);
    connect(m_regexBtn, &QToolButton::toggled, this, bump);
    connect(m_stopBtn, &QToolButton::clicked, this, &SearchPanel::stopSearch);

    // Enter commits the query to the recall history.
    connect(m_query->lineEdit(), &QLineEdit::returnPressed, this,
            &SearchPanel::commitHistory);

    connect(m_results, &QTreeWidget::itemActivated, this,
            [this](QTreeWidgetItem *item, int) { activateMatchRow(item); });

    connect(m_results, &QTreeWidget::customContextMenuRequested, this,
            &SearchPanel::onContextMenu);

    setProjectRoot(QString());
}

QString SearchPanel::queryText() const
{
    return m_query->currentText();
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
        if (!queryText().isEmpty()) {
            scheduleSearch();
        }
    }
}

void SearchPanel::focusQuery()
{
    m_query->setFocus();
    m_query->lineEdit()->selectAll();
}

void SearchPanel::search(const QString &query)
{
    const QString trimmed = query.trimmed();
    if (trimmed.isEmpty())
        return;
    // Setting the text drives editTextChanged → scheduleSearch() → runSearch(),
    // reusing the panel's debounce, toggles and workspace-scoped root.
    m_query->setEditText(query);
    m_query->setFocus();
}

void SearchPanel::commitHistory()
{
    const QString q = queryText().trimmed();
    if (q.isEmpty()) {
        return;
    }
    m_query->addToHistory(q);
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kConfigGroup));
    cfg.writeEntry(QLatin1String(kConfigKey), m_query->historyItems());
    cfg.sync();
}

bool SearchPanel::eventFilter(QObject *watched, QEvent *event)
{
    if (watched == m_query->lineEdit() && event->type() == QEvent::KeyPress) {
        auto *ke = static_cast<QKeyEvent *>(event);
        if (ke->key() == Qt::Key_Escape) {
            Q_EMIT escapeToEditor();
            return true;
        }
    }
    return QWidget::eventFilter(watched, event);
}

void SearchPanel::scheduleSearch()
{
    m_debounce->start();
}

void SearchPanel::clearResults()
{
    m_results->clear();
}

void SearchPanel::setBusy(bool busy)
{
    m_busy = busy;
    m_stopBtn->setEnabled(busy);
}

void SearchPanel::stopSearch()
{
    // Bump the generation so any in-flight reply is dropped, then reset state.
    ++m_seq;
    m_debounce->stop();
    setBusy(false);
    if (m_root.isEmpty()) {
        m_status->setText(i18n("No project selected."));
    } else {
        m_status->setText(i18n("Search stopped."));
    }
}

void SearchPanel::runSearch()
{
    if (!m_core || !m_core->isConnected() || m_root.isEmpty()) {
        clearResults();
        setBusy(false);
        return;
    }
    const QString q = queryText();
    if (q.isEmpty()) {
        clearResults();
        setBusy(false);
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
    setBusy(true);
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
    setBusy(false);
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
        const bool capped = f.value(QStringLiteral("capped")).toBool();
        const QJsonArray matches = f.value(QStringLiteral("matches")).toArray();
        auto *parent = new QTreeWidgetItem(m_results);
        // Surface per-file truncation: a capped file has more matches on disk
        // than the (200) listed, so mark the count as a lower bound.
        const QString count = capped ? i18n("%1+", matches.size())
                                     : QString::number(matches.size());
        parent->setText(0, QStringLiteral("%1   (%2)")
                               .arg(rootDir.relativeFilePath(path), count));
        parent->setToolTip(0, path);
        parent->setFirstColumnSpanned(true);
        // Carry the file path so selecting/dragging the file row attaches the
        // whole file; parent rows have no line role, marking them whole-file.
        parent->setData(0, Qt::UserRole, path);
        for (const QJsonValue &mv : matches) {
            const QJsonObject m = mv.toObject();
            // ripgrep returns 1-indexed line numbers and 1-indexed columns,
            // but EditorArea::openFile (KTextEditor::Cursor) is 0-indexed.
            const int lineOneBased = m.value(QStringLiteral("line")).toInt();
            const int lineZeroBased = lineOneBased > 0 ? lineOneBased - 1 : 0;
            const int colOneBased = m.value(QStringLiteral("column")).toInt();
            const int colZeroBased = colOneBased > 0 ? colOneBased - 1 : 0;
            const int matchLen = m.value(QStringLiteral("matchLen")).toInt();

            // Keep the raw preview (column offsets stay valid) but strip leading
            // whitespace for display, adjusting the highlight start to match.
            const QString rawPreview = m.value(QStringLiteral("preview")).toString();
            int lead = 0;
            while (lead < rawPreview.size() && rawPreview.at(lead).isSpace()) {
                ++lead;
            }
            const QString preview = rawPreview.mid(lead);
            const QString prefix = QStringLiteral("%1: ").arg(lineOneBased, 5);
            const QString display = prefix + preview;

            auto *row = new QTreeWidgetItem(parent);
            row->setText(0, display);
            row->setData(0, Qt::UserRole, path);
            row->setData(0, Qt::UserRole + 1, lineZeroBased);
            row->setData(0, kRoleColumn, colZeroBased);
            // Map the match span into display coordinates (prefix + de-indented
            // preview). Clamp so a span starting inside trimmed leading space
            // doesn't underflow.
            const int hiStart = prefix.size() + qMax(0, colZeroBased - lead);
            row->setData(0, kRoleHiStart, hiStart);
            row->setData(0, kRoleHiLen, matchLen);
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

void SearchPanel::activateMatchRow(QTreeWidgetItem *item)
{
    if (!item) {
        return;
    }
    const QString path = item->data(0, Qt::UserRole).toString();
    if (path.isEmpty()) {
        return;
    }
    const int line = item->data(0, Qt::UserRole + 1).toInt();
    const int column = item->data(0, kRoleColumn).toInt();
    Q_EMIT resultActivated(path, line, column);
}

QTreeWidgetItem *SearchPanel::stepMatchRow(int direction)
{
    // Flatten the file → match tree into the ordered list of match rows, then
    // step from the current selection with wraparound.
    QList<QTreeWidgetItem *> rows;
    for (int i = 0; i < m_results->topLevelItemCount(); ++i) {
        QTreeWidgetItem *file = m_results->topLevelItem(i);
        for (int j = 0; j < file->childCount(); ++j) {
            rows.append(file->child(j));
        }
    }
    if (rows.isEmpty()) {
        return nullptr;
    }
    const QList<QTreeWidgetItem *> selected = m_results->selectedItems();
    int cur = selected.isEmpty() ? -1 : rows.indexOf(selected.first());
    int next;
    if (cur < 0) {
        next = direction >= 0 ? 0 : rows.size() - 1;
    } else {
        next = (cur + direction + rows.size()) % rows.size();
    }
    return rows.at(next);
}

void SearchPanel::focusNextResult()
{
    if (QTreeWidgetItem *row = stepMatchRow(+1)) {
        m_results->setCurrentItem(row);
        activateMatchRow(row);
    }
}

void SearchPanel::focusPrevResult()
{
    if (QTreeWidgetItem *row = stepMatchRow(-1)) {
        m_results->setCurrentItem(row);
        activateMatchRow(row);
    }
}

QStringList SearchPanel::selectedResultPaths() const
{
    QStringList paths;
    QSet<QString> seen;
    auto add = [&](const QTreeWidgetItem *item) {
        if (!item) {
            return;
        }
        const QString path = item->data(0, Qt::UserRole).toString();
        if (path.isEmpty() || seen.contains(path)) {
            return;
        }
        seen.insert(path);
        paths << path;
    };
    const auto selected = m_results->selectedItems();
    for (const QTreeWidgetItem *item : selected) {
        add(item);
    }
    if (paths.isEmpty()) {
        add(m_results->currentItem());
    }
    return paths;
}

void SearchPanel::onContextMenu(const QPoint &pos)
{
    // Anchor the popup on the row under the cursor so a right-click on an
    // unselected row targets that row.
    if (QTreeWidgetItem *under = m_results->itemAt(pos)) {
        if (!under->isSelected()) {
            m_results->setCurrentItem(under);
        }
    }
    const QStringList paths = selectedResultPaths();
    if (paths.isEmpty()) {
        return;
    }
    QMenu menu(this);
    QAction *attach = menu.addAction(
        QIcon::fromTheme(QStringLiteral("mail-attachment")),
        i18n("Attach to Chat as Context"));
    connect(attach, &QAction::triggered, this,
            [this, paths] { Q_EMIT attachToChatRequested(paths); });
    menu.exec(m_results->viewport()->mapToGlobal(pos));
}
