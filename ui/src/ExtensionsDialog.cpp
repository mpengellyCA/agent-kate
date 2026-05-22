#include "ExtensionsDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QPointer>
#include <QPushButton>
#include <QVBoxLayout>

ExtensionsDialog::ExtensionsDialog(CoreClient *core, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
{
    setWindowTitle(i18n("Language Extensions"));
    resize(540, 440);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Install VS Code language extensions from the Open VSX registry. "
             "The language server each one bundles is reused for code "
             "intelligence in the editor."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    // Install-by-id row.
    auto *installRow = new QHBoxLayout;
    m_idEdit = new QLineEdit(this);
    m_idEdit->setPlaceholderText(
        i18n("Extension id, e.g. bmewburn.vscode-intelephense-client"));
    m_installButton = new QPushButton(i18n("Install"), this);
    m_installButton->setDefault(true);
    installRow->addWidget(m_idEdit);
    installRow->addWidget(m_installButton);
    layout->addLayout(installRow);

    m_list = new QListWidget(this);
    m_list->setSelectionMode(QAbstractItemView::NoSelection);
    m_list->setFocusPolicy(Qt::NoFocus);
    layout->addWidget(m_list, 1);

    m_status = new QLabel(this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    layout->addWidget(buttons);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::accept);

    connect(m_installButton, &QPushButton::clicked, this, &ExtensionsDialog::install);
    connect(m_idEdit, &QLineEdit::returnPressed, this, &ExtensionsDialog::install);

    refresh();
}

void ExtensionsDialog::refresh()
{
    if (!m_core || !m_core->isConnected()) {
        m_status->setText(i18n("The core is not connected."));
        return;
    }
    QPointer<ExtensionsDialog> self(this);
    m_core->call(QStringLiteral("vsix.list"), {},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Could not list extensions: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->populate(
                         result.value(QStringLiteral("extensions")).toArray());
                 });
}

void ExtensionsDialog::install()
{
    const QString id = m_idEdit->text().trimmed();
    if (id.isEmpty()) {
        return;
    }
    if (!m_core || !m_core->isConnected()) {
        m_status->setText(i18n("The core is not connected."));
        return;
    }
    setBusy(true, i18n("Installing %1 — downloading from Open VSX…", id));

    QPointer<ExtensionsDialog> self(this);
    m_core->call(QStringLiteral("vsix.install"),
                 QJsonObject{{QStringLiteral("extensionId"), id}},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     self->setBusy(false, QString());
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Install failed: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     const QString name = result.value(QStringLiteral("name")).toString();
                     const bool hasServer =
                         !result.value(QStringLiteral("server")).toObject().isEmpty();
                     self->m_status->setText(
                         hasServer
                             ? i18n("Installed %1 — its language server is ready.", name)
                             : i18n("Installed %1, but no language server was detected.",
                                    name));
                     self->m_idEdit->clear();
                     self->refresh();
                     Q_EMIT self->extensionsChanged();
                 });
}

void ExtensionsDialog::populate(const QJsonArray &extensions)
{
    m_list->clear();
    if (extensions.isEmpty()) {
        new QListWidgetItem(i18n("No extensions installed yet."), m_list);
        return;
    }
    for (const QJsonValue &value : extensions) {
        const QJsonObject ext = value.toObject();
        const QString id = ext.value(QStringLiteral("id")).toString();
        const QString name = ext.value(QStringLiteral("name")).toString();
        const QString version = ext.value(QStringLiteral("version")).toString();
        const QJsonObject server = ext.value(QStringLiteral("server")).toObject();

        QString serverLine;
        if (server.isEmpty()) {
            serverLine = i18n("    no language server detected");
        } else {
            QStringList langs;
            const QJsonArray ids = server.value(QStringLiteral("languageIds")).toArray();
            for (const QJsonValue &l : ids) {
                langs << l.toString();
            }
            serverLine = i18n("    language server: %1  (%2)",
                              langs.join(QStringLiteral(", ")),
                              server.value(QStringLiteral("source")).toString());
        }
        auto *item = new QListWidgetItem(
            QStringLiteral("%1  —  v%2\n%3")
                .arg(name.isEmpty() ? id : name, version, serverLine),
            m_list);
        item->setToolTip(id);
    }
}

void ExtensionsDialog::setBusy(bool busy, const QString &message)
{
    m_installButton->setEnabled(!busy);
    m_idEdit->setEnabled(!busy);
    if (!message.isEmpty()) {
        m_status->setText(message);
    }
}
