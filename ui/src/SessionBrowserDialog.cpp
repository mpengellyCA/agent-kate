#include "SessionBrowserDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDateTime>
#include <QDialogButtonBox>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QPointer>
#include <QPushButton>
#include <QVBoxLayout>

namespace {
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
    setWindowTitle(i18n("Resume a Claude Code Session"));
    resize(640, 500);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Pick a past Claude Code conversation to continue in AgentKate. "
             "Every session on disk is listed — including ones started in the "
             "claude CLI directly."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    m_search = new QLineEdit(this);
    m_search->setPlaceholderText(i18n("Filter by title or project…"));
    m_search->setClearButtonEnabled(true);
    layout->addWidget(m_search);

    m_list = new QListWidget(this);
    m_list->setAlternatingRowColors(true);
    layout->addWidget(m_list, 1);

    m_status = new QLabel(i18n("Loading sessions…"), this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    m_attachButton =
        buttons->addButton(i18n("Resume Session"), QDialogButtonBox::AcceptRole);
    m_attachButton->setEnabled(false);
    layout->addWidget(buttons);

    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(m_attachButton, &QPushButton::clicked, this,
            &SessionBrowserDialog::attachSelected);
    connect(m_list, &QListWidget::itemDoubleClicked, this,
            &SessionBrowserDialog::attachSelected);
    connect(m_list, &QListWidget::itemSelectionChanged, this, [this] {
        QListWidgetItem *item = m_list->currentItem();
        m_attachButton->setEnabled(item != nullptr && !item->isHidden());
    });
    connect(m_search, &QLineEdit::textChanged, this, &SessionBrowserDialog::applyFilter);

    refresh();
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
        const bool attached = s.value(QStringLiteral("attached")).toBool();
        const QDateTime modified = QDateTime::fromString(
            s.value(QStringLiteral("modified")).toString(), Qt::ISODate);

        QString meta = project;
        const QString rel = relativeTime(modified);
        if (!rel.isEmpty()) {
            meta += QStringLiteral("  ·  ") + rel;
        }
        if (attached) {
            meta += QStringLiteral("  ·  already in AgentKate");
        }
        auto *item =
            new QListWidgetItem(QStringLiteral("%1\n%2").arg(title, meta), m_list);
        item->setData(Qt::UserRole, sessionId);
        item->setData(Qt::UserRole + 1, project);
        item->setData(Qt::UserRole + 2, title);
        item->setToolTip(sessionId);
    }
    m_status->setText(sessions.isEmpty()
                          ? i18n("No Claude Code sessions found.")
                          : i18n("%1 session(s) — newest first.", sessions.size()));
    applyFilter();
}

void SessionBrowserDialog::applyFilter()
{
    const QString needle = m_search->text().trimmed();
    for (int i = 0; i < m_list->count(); ++i) {
        QListWidgetItem *item = m_list->item(i);
        item->setHidden(!needle.isEmpty()
                        && !item->text().contains(needle, Qt::CaseInsensitive));
    }
    QListWidgetItem *current = m_list->currentItem();
    m_attachButton->setEnabled(current != nullptr && !current->isHidden());
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

    m_attachButton->setEnabled(false);
    m_status->setText(i18n("Attaching session…"));

    QPointer<SessionBrowserDialog> self(this);
    m_core->call(QStringLiteral("session.attach"),
                 QJsonObject{{QStringLiteral("sessionId"), sessionId},
                             {QStringLiteral("project"), project},
                             {QStringLiteral("title"), title}},
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
