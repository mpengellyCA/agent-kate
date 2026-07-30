#include "SessionBrowserDialog.h"
#include "ipc/CoreClient.h"
#include "state/HarnessTraits.h"

#include <KConfigGroup>
#include <KGuiItem>
#include <KLocalizedString>
#include <KMessageBox>
#include <KSharedConfig>
#include <KStandardGuiItem>

#include <QAction>
#include <QComboBox>
#include <QDateTime>
#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QMenu>
#include <QPointer>
#include <QPushButton>
#include <QSplitter>
#include <QTextBrowser>
#include <QTimer>
#include <QVBoxLayout>

#include <algorithm>

namespace {
// session.preview / session.forget read a harness's on-disk transcript store.
// A harness whose transcript lives only in the core's event log (e.g. Kimi) has
// none — so those rows show metadata instead of a preview, and can't be
// forgotten from here. Bound to a dedicated capability, never inferred from the
// model vocabulary (which conflates two unrelated affordances).
bool backendHasTranscript(const QString &backend)
{
    return HarnessRegistry::self()->traits(backend).transcriptPreview;
}

// relativeTime renders a timestamp as a short, human "… ago" string.
QString relativeTime(const QDateTime &when)
{
    if (!when.isValid()) {
        return QString();
    }
    const qint64 secs = when.secsTo(QDateTime::currentDateTime());
    if (secs < 90) {
        return QStringLiteral("just now");
    }
    if (secs < 3600) {
        return QStringLiteral("%1 min ago").arg(secs / 60);
    }
    if (secs < 86400) {
        return QStringLiteral("%1 h ago").arg(secs / 3600);
    }
    if (secs < 86400 * 30) {
        return QStringLiteral("%1 d ago").arg(secs / 86400);
    }
    return when.date().toString(Qt::ISODate);
}
} // namespace

SessionBrowserDialog::SessionBrowserDialog(CoreClient *core, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
{
    setWindowTitle(i18n("Resume a Session"));
    resize(640, 500);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Pick a past conversation to continue in Agent Kate. Sessions from "
             "every engine are listed — including ones started in the CLI "
             "directly."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    auto *filterRow = new QHBoxLayout;
    m_search = new QLineEdit(this);
    m_search->setPlaceholderText(i18n("Filter by title or project…"));
    m_search->setClearButtonEnabled(true);
    filterRow->addWidget(m_search, 1);

    m_sort = new QComboBox(this);
    m_sort->addItem(i18n("Recent"));
    m_sort->addItem(i18n("Project"));
    m_sort->addItem(i18n("Title"));
    filterRow->addWidget(new QLabel(i18n("Sort:"), this));
    filterRow->addWidget(m_sort);
    layout->addLayout(filterRow);

    m_splitter = new QSplitter(Qt::Horizontal, this);

    m_list = new QListWidget(m_splitter);
    m_list->setAlternatingRowColors(true);
    m_list->setContextMenuPolicy(Qt::CustomContextMenu);
    m_splitter->addWidget(m_list);

    m_preview = new QTextBrowser(m_splitter);
    m_preview->setReadOnly(true);
    m_preview->setPlaceholderText(i18n("Select a session to preview"));
    m_splitter->addWidget(m_preview);
    m_splitter->setStretchFactor(0, 1);
    m_splitter->setStretchFactor(1, 1);
    layout->addWidget(m_splitter, 1);

    m_previewTimer = new QTimer(this);
    m_previewTimer->setSingleShot(true);
    m_previewTimer->setInterval(150);

    m_status = new QLabel(i18n("Loading sessions…"), this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    m_forgetButton =
        buttons->addButton(i18n("Forget"), QDialogButtonBox::DestructiveRole);
    m_forgetButton->setIcon(QIcon::fromTheme(QStringLiteral("edit-delete")));
    m_forgetButton->setEnabled(false);
    m_attachButton =
        buttons->addButton(i18n("Resume Session"), QDialogButtonBox::AcceptRole);
    m_attachButton->setEnabled(false);
    layout->addWidget(buttons);

    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(m_attachButton, &QPushButton::clicked, this,
            &SessionBrowserDialog::attachSelected);
    connect(m_forgetButton, &QPushButton::clicked, this,
            &SessionBrowserDialog::forgetSelected);
    connect(m_list, &QListWidget::itemDoubleClicked, this,
            &SessionBrowserDialog::attachSelected);
    connect(m_list, &QListWidget::itemSelectionChanged, this, [this] {
        updateActionButtons();
        m_previewTimer->start();
    });
    connect(m_list, &QListWidget::customContextMenuRequested, this,
            &SessionBrowserDialog::showContextMenu);
    connect(m_previewTimer, &QTimer::timeout, this,
            &SessionBrowserDialog::loadPreview);
    connect(m_search, &QLineEdit::textChanged, this, &SessionBrowserDialog::applyFilter);
    connect(m_sort, qOverload<int>(&QComboBox::currentIndexChanged), this,
            &SessionBrowserDialog::applySort);

    // Restore persisted filter, sort, and splitter sizes.
    KConfigGroup cfg(KSharedConfig::openConfig(), QStringLiteral("SessionBrowser"));
    m_search->setText(cfg.readEntry("filter", QString()));
    m_sort->setCurrentIndex(cfg.readEntry("sort", 0));
    const QList<int> sizes = cfg.readEntry("splitterSizes", QList<int>());
    if (sizes.size() == 2) {
        m_splitter->setSizes(sizes);
    }

    refresh();
}

SessionBrowserDialog::~SessionBrowserDialog()
{
    KConfigGroup cfg(KSharedConfig::openConfig(), QStringLiteral("SessionBrowser"));
    cfg.writeEntry("filter", m_search->text());
    cfg.writeEntry("sort", m_sort->currentIndex());
    cfg.writeEntry("splitterSizes", m_splitter->sizes());
    cfg.sync();
}

void SessionBrowserDialog::refresh()
{
    if (!m_core || !m_core->isConnected()) {
        m_status->setText(i18n("The core is not connected."));
        return;
    }
    QPointer<SessionBrowserDialog> self(this);
    m_core->call(QStringLiteral("session.browse"), {},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Could not list sessions: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->populate(result.value(QStringLiteral("sessions")).toArray());
                 });
}

void SessionBrowserDialog::populate(const QJsonArray &sessions)
{
    m_list->clear();
    for (const QJsonValue &v : sessions) {
        const QJsonObject s = v.toObject();
        const QString sessionId = s.value(QStringLiteral("sessionId")).toString();
        const QString project = s.value(QStringLiteral("project")).toString();
        const QString title = s.value(QStringLiteral("title")).toString();
        const QString backend = s.value(QStringLiteral("backend")).toString();
        const bool attached = s.value(QStringLiteral("attached")).toBool();
        const QString updatedStr = s.value(QStringLiteral("updated")).toString();
        const QDateTime updated =
            QDateTime::fromString(updatedStr, Qt::ISODate);

        QString meta = project;
        const QString rel = relativeTime(updated);
        if (!rel.isEmpty()) {
            meta += QStringLiteral("  ·  ") + rel;
        }
        // Mark non-default engines (Claude is the unmarked default: empty badge).
        const QString badge = HarnessRegistry::self()->traits(backend).badge;
        if (!badge.isEmpty()) {
            meta += QStringLiteral("  ·  ") + badge;
        }
        if (attached) {
            meta += QStringLiteral("  ·  already in Agent Kate");
        }
        auto *item =
            new QListWidgetItem(QStringLiteral("%1\n%2").arg(title, meta), m_list);
        item->setData(Qt::UserRole, sessionId);
        item->setData(Qt::UserRole + 1, project);
        item->setData(Qt::UserRole + 2, title);
        item->setData(Qt::UserRole + 3, updatedStr);
        item->setData(Qt::UserRole + 4, attached);
        item->setData(Qt::UserRole + 5, backend);
        item->setToolTip(sessionId);
    }
    m_status->setText(sessions.isEmpty()
                          ? i18n("No sessions found.")
                          : i18n("%1 session(s).", sessions.size()));
    applySort();
    applyFilter();
}

void SessionBrowserDialog::applySort()
{
    const int mode = m_sort->currentIndex();
    const QString currentId =
        m_list->currentItem() ? m_list->currentItem()->data(Qt::UserRole).toString()
                              : QString();

    // Detach every row, sort the pointers, and re-insert. The list is capped at
    // 500 server-side, so an in-memory client sort is trivial.
    QList<QListWidgetItem *> items;
    items.reserve(m_list->count());
    while (m_list->count() > 0) {
        items.append(m_list->takeItem(0));
    }
    std::sort(items.begin(), items.end(),
              [mode](QListWidgetItem *a, QListWidgetItem *b) {
                  switch (mode) {
                  case 1: // Project
                      return a->data(Qt::UserRole + 1).toString().compare(
                                 b->data(Qt::UserRole + 1).toString(),
                                 Qt::CaseInsensitive)
                          < 0;
                  case 2: // Title
                      return a->data(Qt::UserRole + 2).toString().compare(
                                 b->data(Qt::UserRole + 2).toString(),
                                 Qt::CaseInsensitive)
                          < 0;
                  default: // Recent — newest first by ISO modified string
                      return a->data(Qt::UserRole + 3).toString()
                          > b->data(Qt::UserRole + 3).toString();
                  }
              });
    for (QListWidgetItem *it : items) {
        m_list->addItem(it);
        if (!currentId.isEmpty()
            && it->data(Qt::UserRole).toString() == currentId) {
            m_list->setCurrentItem(it);
        }
    }
    applyFilter();
}

void SessionBrowserDialog::applyFilter()
{
    const QString needle = m_search->text().trimmed();
    for (int i = 0; i < m_list->count(); ++i) {
        QListWidgetItem *item = m_list->item(i);
        bool match = needle.isEmpty();
        if (!match) {
            // Match against the structured title/project fields only, so the
            // rendered "x d ago" suffix never pollutes the filter.
            const QString title = item->data(Qt::UserRole + 2).toString();
            const QString project = item->data(Qt::UserRole + 1).toString();
            match = title.contains(needle, Qt::CaseInsensitive)
                || project.contains(needle, Qt::CaseInsensitive);
        }
        item->setHidden(!match);
    }
    // If the selected row was just filtered out, its preview is stale.
    QListWidgetItem *current = m_list->currentItem();
    if (!current || current->isHidden()) {
        m_preview->clear();
        m_previewSessionId.clear();
    }
    updateActionButtons();
}

void SessionBrowserDialog::updateActionButtons()
{
    QListWidgetItem *current = m_list->currentItem();
    const bool valid = current != nullptr && !current->isHidden();
    m_attachButton->setEnabled(valid);
    // Forget is refused for attached sessions (remove the agent first) and for
    // engines with no on-disk transcript to delete.
    const bool attached = valid && current->data(Qt::UserRole + 4).toBool();
    const bool forgettable =
        valid && backendHasTranscript(current->data(Qt::UserRole + 5).toString());
    m_forgetButton->setEnabled(forgettable && !attached);
}

void SessionBrowserDialog::attachSelected()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item || item->isHidden()) {
        return;
    }
    if (!m_core || !m_core->isConnected()) {
        m_status->setText(i18n("The core is not connected."));
        return;
    }
    const QString sessionId = item->data(Qt::UserRole).toString();
    const QString project = item->data(Qt::UserRole + 1).toString();
    const QString title = item->data(Qt::UserRole + 2).toString();
    const QString backend = item->data(Qt::UserRole + 5).toString();

    m_attachButton->setEnabled(false);
    m_status->setText(i18n("Attaching session…"));

    QPointer<SessionBrowserDialog> self(this);
    m_core->call(QStringLiteral("session.attach"),
                 QJsonObject{{QStringLiteral("sessionId"), sessionId},
                             {QStringLiteral("project"), project},
                             {QStringLiteral("title"), title},
                             {QStringLiteral("backend"), backend}},
                 [self, project, title](const QJsonObject &result,
                                        const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Could not attach: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         self->m_attachButton->setEnabled(true);
                         return;
                     }
                     Q_EMIT self->attachRequested(
                         project,
                         result.value(QStringLiteral("threadId")).toString(), title);
                     self->accept();
                 });
}

void SessionBrowserDialog::forgetSelected()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item || item->isHidden()) {
        return;
    }
    if (item->data(Qt::UserRole + 4).toBool()) {
        m_status->setText(
            i18n("This session is attached as an agent; remove the agent first."));
        return;
    }
    if (!m_core || !m_core->isConnected()) {
        m_status->setText(i18n("The core is not connected."));
        return;
    }
    const QString sessionId = item->data(Qt::UserRole).toString();
    const QString title = item->data(Qt::UserRole + 2).toString();
    if (KMessageBox::questionTwoActions(
            this,
            i18n("Permanently delete the on-disk transcript for \"%1\"? "
                 "This cannot be undone.", title),
            i18n("Forget Session"),
            KGuiItem(i18n("Forget"), QStringLiteral("edit-delete")),
            KStandardGuiItem::cancel())
        != KMessageBox::PrimaryAction) {
        return;
    }
    QPointer<SessionBrowserDialog> self(this);
    m_core->call(QStringLiteral("session.forget"),
                 QJsonObject{{QStringLiteral("sessionId"), sessionId}},
                 [self, sessionId](const QJsonObject &, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Could not forget session: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     // Remove the matching row and refresh the count.
                     for (int i = 0; i < self->m_list->count(); ++i) {
                         QListWidgetItem *it = self->m_list->item(i);
                         if (it->data(Qt::UserRole).toString() == sessionId) {
                             delete self->m_list->takeItem(i);
                             break;
                         }
                     }
                     self->m_preview->clear();
                     self->m_previewSessionId.clear();
                     self->m_status->setText(
                         i18n("%1 session(s).", self->m_list->count()));
                     self->updateActionButtons();
                 });
}

void SessionBrowserDialog::showContextMenu(const QPoint &pos)
{
    QListWidgetItem *item = m_list->itemAt(pos);
    if (!item) {
        return;
    }
    m_list->setCurrentItem(item);
    QMenu menu(this);
    QAction *resume = menu.addAction(i18n("Resume Session"));
    QAction *forget = menu.addAction(
        QIcon::fromTheme(QStringLiteral("edit-delete")), i18n("Forget…"));
    forget->setEnabled(!item->data(Qt::UserRole + 4).toBool()
                       && backendHasTranscript(item->data(Qt::UserRole + 5).toString()));
    QAction *chosen = menu.exec(m_list->viewport()->mapToGlobal(pos));
    if (chosen == resume) {
        attachSelected();
    } else if (chosen == forget) {
        forgetSelected();
    }
}

void SessionBrowserDialog::loadPreview()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item || item->isHidden()) {
        m_preview->clear();
        m_previewSessionId.clear();
        return;
    }
    const QString sessionId = item->data(Qt::UserRole).toString();
    if (sessionId == m_previewSessionId) {
        return; // already showing this one
    }
    m_previewSessionId = sessionId;
    const QString backend = item->data(Qt::UserRole + 5).toString();
    if (!backendHasTranscript(backend)) {
        // No on-disk transcript to preview — show the row's metadata instead.
        const QString title =
            item->data(Qt::UserRole + 2).toString().toHtmlEscaped();
        const QString project =
            item->data(Qt::UserRole + 1).toString().toHtmlEscaped();
        const QString engine =
            HarnessRegistry::self()->traits(backend).displayName.toHtmlEscaped();
        const QDateTime updated = QDateTime::fromString(
            item->data(Qt::UserRole + 3).toString(), Qt::ISODate);
        QString html = QStringLiteral("<p><b>%1</b></p>").arg(title);
        html += i18n("<p>Engine: %1<br>Project: %2<br>Last active: %3</p>", engine,
                     project, relativeTime(updated).toHtmlEscaped());
        html += i18n("<p><i>No preview for this engine — resume the session to "
                     "see the conversation.</i></p>");
        m_preview->setHtml(html);
        return;
    }
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    m_preview->setPlainText(i18n("Loading preview…"));

    QPointer<SessionBrowserDialog> self(this);
    m_core->call(QStringLiteral("session.preview"),
                 QJsonObject{{QStringLiteral("sessionId"), sessionId},
                             {QStringLiteral("maxMessages"), 20}},
                 [self, sessionId](const QJsonObject &result,
                                   const QJsonObject &error) {
                     if (!self || self->m_previewSessionId != sessionId) {
                         return; // selection moved on; ignore stale reply
                     }
                     if (!error.isEmpty()) {
                         self->m_preview->setPlainText(
                             i18n("Could not load preview: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->renderPreview(
                         result.value(QStringLiteral("messages")).toArray(),
                         result.value(QStringLiteral("truncated")).toBool());
                 });
}

void SessionBrowserDialog::renderPreview(const QJsonArray &messages, bool truncated)
{
    if (messages.isEmpty()) {
        m_preview->setPlainText(i18n("This session has no preview-able messages."));
        return;
    }
    QString html;
    if (truncated) {
        html += i18n("<p><i>… earlier messages not shown</i></p>");
    }
    for (const QJsonValue &v : messages) {
        const QJsonObject m = v.toObject();
        const QString role = m.value(QStringLiteral("role")).toString();
        const QString label = role == QStringLiteral("assistant")
                                  ? i18n("Assistant")
                                  : i18n("You");
        QString text = m.value(QStringLiteral("text")).toString().toHtmlEscaped();
        text.replace(QStringLiteral("\n"), QStringLiteral("<br>"));
        html += QStringLiteral("<p><b>%1</b><br>%2</p>").arg(label, text);
    }
    m_preview->setHtml(html);
}
