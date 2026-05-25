#include "ExtensionsDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QPointer>
#include <QPushButton>
#include <QTabWidget>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QVBoxLayout>
#include <QWidget>

ExtensionsDialog::ExtensionsDialog(CoreClient *core, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
{
    setWindowTitle(i18n("Language Extensions"));
    resize(620, 520);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Install VS Code language extensions from the Open VSX registry. "
             "The language server each one bundles is reused for code "
             "intelligence in the editor."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    m_tabs = new QTabWidget(this);
    layout->addWidget(m_tabs, 1);

    // --- Installed tab ----------------------------------------------------
    auto *installedTab = new QWidget(m_tabs);
    auto *installedLayout = new QVBoxLayout(installedTab);
    installedLayout->setContentsMargins(0, 6, 0, 0);

    auto *installRow = new QHBoxLayout;
    m_idEdit = new QLineEdit(installedTab);
    m_idEdit->setPlaceholderText(
        i18n("Extension id, e.g. bmewburn.vscode-intelephense-client"));
    m_installButton = new QPushButton(i18n("Install"), installedTab);
    m_installButton->setDefault(true);
    installRow->addWidget(m_idEdit);
    installRow->addWidget(m_installButton);
    installedLayout->addLayout(installRow);

    m_list = new QListWidget(installedTab);
    m_list->setSelectionMode(QAbstractItemView::NoSelection);
    m_list->setFocusPolicy(Qt::NoFocus);
    installedLayout->addWidget(m_list, 1);

    m_tabs->addTab(installedTab, i18n("Installed"));

    // --- Popular tab ------------------------------------------------------
    auto *popularTab = new QWidget(m_tabs);
    auto *popularLayout = new QVBoxLayout(popularTab);
    popularLayout->setContentsMargins(0, 6, 0, 0);

    auto *popularIntro = new QLabel(
        i18n("Curated extensions known to ship a working language server. "
             "Click Install to fetch one from Open VSX."),
        popularTab);
    popularIntro->setWordWrap(true);
    popularLayout->addWidget(popularIntro);

    m_catalog = new QTreeWidget(popularTab);
    m_catalog->setColumnCount(2);
    m_catalog->setHeaderHidden(true);
    m_catalog->setRootIsDecorated(false);
    m_catalog->setSelectionMode(QAbstractItemView::NoSelection);
    m_catalog->setFocusPolicy(Qt::NoFocus);
    m_catalog->setIndentation(0);
    m_catalog->header()->setStretchLastSection(false);
    m_catalog->header()->setSectionResizeMode(0, QHeaderView::Stretch);
    m_catalog->header()->setSectionResizeMode(1, QHeaderView::ResizeToContents);
    popularLayout->addWidget(m_catalog, 1);

    m_tabs->addTab(popularTab, i18n("Popular"));

    // --- Status + close ---------------------------------------------------
    m_status = new QLabel(this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    layout->addWidget(buttons);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::accept);

    connect(m_installButton, &QPushButton::clicked,
            this, &ExtensionsDialog::installFromEdit);
    connect(m_idEdit, &QLineEdit::returnPressed,
            this, &ExtensionsDialog::installFromEdit);

    refresh();
    refreshCatalog();
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

void ExtensionsDialog::refreshCatalog()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    QPointer<ExtensionsDialog> self(this);
    m_core->call(QStringLiteral("vsix.catalog"), {},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Could not load the catalog: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->populateCatalog(
                         result.value(QStringLiteral("entries")).toArray());
                 });
}

void ExtensionsDialog::installFromEdit()
{
    const QString id = m_idEdit->text().trimmed();
    if (id.isEmpty()) {
        return;
    }
    installById(id);
}

void ExtensionsDialog::installById(const QString &id)
{
    if (!m_core || !m_core->isConnected()) {
        m_status->setText(i18n("The core is not connected."));
        return;
    }
    setBusy(true, i18n("Installing %1 — downloading from Open VSX…", id));

    QPointer<ExtensionsDialog> self(this);
    const QString requestedId = id;
    m_core->call(QStringLiteral("vsix.install"),
                 QJsonObject{{QStringLiteral("extensionId"), id}},
                 [self, requestedId](const QJsonObject &result,
                                     const QJsonObject &error) {
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
                     if (self->m_idEdit->text().trimmed() == requestedId) {
                         self->m_idEdit->clear();
                     }
                     self->refresh();
                     self->refreshCatalog();
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

void ExtensionsDialog::populateCatalog(const QJsonArray &entries)
{
    m_catalog->clear();
    m_catalogButtons.clear();
    if (entries.isEmpty()) {
        auto *empty = new QTreeWidgetItem(m_catalog);
        empty->setText(0, i18n("Catalog is empty."));
        empty->setFirstColumnSpanned(true);
        return;
    }
    for (const QJsonValue &value : entries) {
        const QJsonObject e = value.toObject();
        const QString id = e.value(QStringLiteral("id")).toString();
        const QString name = e.value(QStringLiteral("displayName")).toString();
        const QString summary = e.value(QStringLiteral("summary")).toString();
        const QString category = e.value(QStringLiteral("category")).toString();
        const bool installed = e.value(QStringLiteral("installed")).toBool();

        auto *item = new QTreeWidgetItem(m_catalog);
        item->setText(0, QStringLiteral("%1  —  %2\n%3")
                             .arg(name, category, summary));
        item->setToolTip(0, id);

        auto *button = new QPushButton(
            installed ? i18n("Installed") : i18n("Install"), m_catalog);
        button->setEnabled(!installed);
        connect(button, &QPushButton::clicked, this,
                [this, id]() { installById(id); });
        m_catalog->setItemWidget(item, 1, button);
        m_catalogButtons.insert(id, button);
    }
}

void ExtensionsDialog::setBusy(bool busy, const QString &message)
{
    m_installButton->setEnabled(!busy);
    m_idEdit->setEnabled(!busy);
    for (auto *b : m_catalogButtons) {
        if (b) {
            // Keep "Installed" entries disabled; only re-enable real install
            // buttons when leaving the busy state.
            if (busy) {
                b->setEnabled(false);
            } else {
                b->setEnabled(b->text() != i18n("Installed"));
            }
        }
    }
    if (!message.isEmpty()) {
        m_status->setText(message);
    }
}
