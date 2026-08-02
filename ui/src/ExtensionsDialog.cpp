#include "ExtensionsDialog.h"
#include "ipc/CoreClient.h"

#include <KGuiItem>
#include <KLocalizedString>
#include <KMessageBox>
#include <KStandardGuiItem>

#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QPointer>
#include <QProgressBar>
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
    m_list->setSelectionMode(QAbstractItemView::SingleSelection);
    installedLayout->addWidget(m_list, 1);

    auto *installedActions = new QHBoxLayout;
    installedActions->addStretch(1);
    m_updateButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("system-software-update")),
        i18n("Update"), installedTab);
    m_updateButton->setEnabled(false);
    m_uninstallButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("edit-delete")),
        i18n("Uninstall"), installedTab);
    m_uninstallButton->setEnabled(false);
    installedActions->addWidget(m_updateButton);
    installedActions->addWidget(m_uninstallButton);
    installedLayout->addLayout(installedActions);

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

    // --- Search tab -------------------------------------------------------
    auto *searchTab = new QWidget(m_tabs);
    auto *searchLayout = new QVBoxLayout(searchTab);
    searchLayout->setContentsMargins(0, 6, 0, 0);

    auto *searchRow = new QHBoxLayout;
    m_searchEdit = new QLineEdit(searchTab);
    m_searchEdit->setPlaceholderText(
        i18n("Search the Open VSX registry, e.g. python"));
    m_searchEdit->setClearButtonEnabled(true);
    m_searchButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("search")), i18n("Search"), searchTab);
    searchRow->addWidget(m_searchEdit);
    searchRow->addWidget(m_searchButton);
    searchLayout->addLayout(searchRow);

    m_searchResults = new QTreeWidget(searchTab);
    m_searchResults->setColumnCount(2);
    m_searchResults->setHeaderHidden(true);
    m_searchResults->setRootIsDecorated(false);
    m_searchResults->setSelectionMode(QAbstractItemView::NoSelection);
    m_searchResults->setFocusPolicy(Qt::NoFocus);
    m_searchResults->setIndentation(0);
    m_searchResults->header()->setStretchLastSection(false);
    m_searchResults->header()->setSectionResizeMode(0, QHeaderView::Stretch);
    m_searchResults->header()->setSectionResizeMode(1, QHeaderView::ResizeToContents);
    searchLayout->addWidget(m_searchResults, 1);

    m_tabs->addTab(searchTab, i18n("Search"));

    // --- Progress + status + close ----------------------------------------
    m_progress = new QProgressBar(this);
    m_progress->setVisible(false);
    layout->addWidget(m_progress);

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
    connect(m_list, &QListWidget::itemSelectionChanged,
            this, &ExtensionsDialog::updateInstalledButtons);
    connect(m_uninstallButton, &QPushButton::clicked,
            this, &ExtensionsDialog::uninstallSelected);
    connect(m_updateButton, &QPushButton::clicked,
            this, &ExtensionsDialog::updateSelected);
    connect(m_searchButton, &QPushButton::clicked,
            this, &ExtensionsDialog::runSearch);
    connect(m_searchEdit, &QLineEdit::returnPressed,
            this, &ExtensionsDialog::runSearch);
    if (m_core) {
        connect(m_core, &CoreClient::notification,
                this, &ExtensionsDialog::onNotification);
    }

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
    m_installingId = id;
    m_progress->setRange(0, 0); // indeterminate until first progress arrives
    m_progress->setVisible(true);

    QPointer<ExtensionsDialog> self(this);
    const QString requestedId = id;
    m_core->call(QStringLiteral("vsix.install"),
                 QJsonObject{{QStringLiteral("extensionId"), id}},
                 [self, requestedId](const QJsonObject &result,
                                     const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     self->m_installingId.clear();
                     self->m_progress->setVisible(false);
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
                     // Re-run the active search so a just-installed result flips
                     // to "Installed" rather than offering a redundant install.
                     if (!self->m_searchEdit->text().trimmed().isEmpty()
                         && self->m_searchButtons.contains(requestedId)) {
                         self->runSearch();
                     }
                     Q_EMIT self->extensionsChanged();
                 });
}

void ExtensionsDialog::populate(const QJsonArray &extensions)
{
    m_list->clear();
    if (extensions.isEmpty()) {
        auto *empty =
            new QListWidgetItem(i18n("No extensions installed yet."), m_list);
        empty->setFlags(empty->flags() & ~Qt::ItemIsSelectable);
    } else {
        for (const QJsonValue &value : extensions) {
            const QJsonObject ext = value.toObject();
            const QString id = ext.value(QStringLiteral("id")).toString();
            const QString name = ext.value(QStringLiteral("name")).toString();
            const QString version = ext.value(QStringLiteral("version")).toString();
            const QJsonObject server = ext.value(QStringLiteral("server")).toObject();
            const bool updateAvailable =
                ext.value(QStringLiteral("updateAvailable")).toBool();
            const QString latest = ext.value(QStringLiteral("latest")).toString();

            const QString hint = ext.value(QStringLiteral("serverHint")).toString();

            QString serverLine;
            if (server.isEmpty()) {
                serverLine = hint.isEmpty()
                    ? i18n("    no language server detected")
                    : i18n("    no language server detected — %1", hint);
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
            QString versionLine = i18n("v%1", version);
            if (updateAvailable && !latest.isEmpty()) {
                versionLine = i18n("v%1 → v%2 available", version, latest);
            }
            auto *item = new QListWidgetItem(
                QStringLiteral("%1  —  %2\n%3")
                    .arg(name.isEmpty() ? id : name, versionLine, serverLine),
                m_list);
            item->setToolTip(id);
            item->setData(Qt::UserRole, id);
            item->setData(Qt::UserRole + 1, updateAvailable);
        }
    }
    updateInstalledButtons();
}

void ExtensionsDialog::updateInstalledButtons()
{
    QListWidgetItem *item = m_list->currentItem();
    const QString id = item ? item->data(Qt::UserRole).toString() : QString();
    const bool hasSelection = item != nullptr && !id.isEmpty();
    m_uninstallButton->setEnabled(hasSelection);
    m_updateButton->setEnabled(hasSelection
                               && item->data(Qt::UserRole + 1).toBool());
}

void ExtensionsDialog::uninstallSelected()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item) {
        return;
    }
    const QString id = item->data(Qt::UserRole).toString();
    if (id.isEmpty() || !m_core || !m_core->isConnected()) {
        return;
    }
    // SECURITY/UX (audit F31): warningTwoActions, not questionTwoActions — per the KF6
    // header contract questionTwoActions defaults to the PRIMARY button, so Enter here
    // uninstalled the extension. Uninstalling is destructive and irreversible from this
    // dialog, and it is raised by a click the user has already made, which is exactly
    // the shape where Enter gets pressed through. warningTwoActions defaults to the
    // secondary (Cancel) button. This is the last questionTwoActions in ui/src.
    if (KMessageBox::warningTwoActions(
            this,
            i18n("Uninstall the extension \"%1\"? Its language server will stop "
                 "being used in the editor.", id),
            i18n("Uninstall Extension"),
            KGuiItem(i18n("Uninstall"), QStringLiteral("edit-delete")),
            KStandardGuiItem::cancel())
        != KMessageBox::PrimaryAction) {
        return;
    }
    setBusy(true, i18n("Uninstalling %1…", id));
    QPointer<ExtensionsDialog> self(this);
    m_core->call(QStringLiteral("vsix.uninstall"),
                 QJsonObject{{QStringLiteral("extensionId"), id}},
                 [self, id](const QJsonObject &, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     self->setBusy(false, QString());
                     if (!error.isEmpty()) {
                         self->m_status->setText(
                             i18n("Uninstall failed: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->m_status->setText(i18n("Uninstalled %1.", id));
                     self->refresh();
                     self->refreshCatalog();
                     Q_EMIT self->extensionsChanged();
                 });
}

void ExtensionsDialog::updateSelected()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item) {
        return;
    }
    const QString id = item->data(Qt::UserRole).toString();
    if (!id.isEmpty()) {
        installById(id);
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

void ExtensionsDialog::runSearch()
{
    const QString query = m_searchEdit->text().trimmed();
    if (query.isEmpty() || !m_core || !m_core->isConnected()) {
        return;
    }
    // Clearing the tree deletes the per-row buttons; drop their (now dangling)
    // pointers from the hash in lockstep so setBusy() never dereferences them.
    m_searchResults->clear();
    m_searchButtons.clear();
    auto *loading = new QTreeWidgetItem(m_searchResults);
    loading->setText(0, i18n("Searching Open VSX…"));
    loading->setFirstColumnSpanned(true);
    m_searchButton->setEnabled(false);

    QPointer<ExtensionsDialog> self(this);
    m_core->call(QStringLiteral("vsix.search"),
                 QJsonObject{{QStringLiteral("query"), query}},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     self->m_searchButton->setEnabled(true);
                     if (!error.isEmpty()) {
                         self->m_searchResults->clear();
                         self->m_searchButtons.clear();
                         auto *err = new QTreeWidgetItem(self->m_searchResults);
                         err->setText(0, i18n("Search failed: %1",
                                              error.value(QStringLiteral("message")).toString()));
                         err->setFirstColumnSpanned(true);
                         return;
                     }
                     self->populateSearch(
                         result.value(QStringLiteral("entries")).toArray());
                 });
}

void ExtensionsDialog::populateSearch(const QJsonArray &entries)
{
    m_searchResults->clear();
    m_searchButtons.clear();
    if (entries.isEmpty()) {
        auto *empty = new QTreeWidgetItem(m_searchResults);
        empty->setText(0, i18n("No matching extensions found."));
        empty->setFirstColumnSpanned(true);
        return;
    }
    for (const QJsonValue &value : entries) {
        const QJsonObject e = value.toObject();
        const QString id = e.value(QStringLiteral("id")).toString();
        const QString name = e.value(QStringLiteral("displayName")).toString();
        const QString summary = e.value(QStringLiteral("summary")).toString();
        const bool installed = e.value(QStringLiteral("installed")).toBool();

        auto *item = new QTreeWidgetItem(m_searchResults);
        item->setText(0, QStringLiteral("%1  —  %2\n%3").arg(name, id, summary));
        item->setToolTip(0, id);

        auto *button = new QPushButton(
            installed ? i18n("Installed") : i18n("Install"), m_searchResults);
        button->setEnabled(!installed);
        connect(button, &QPushButton::clicked, this,
                [this, id]() { installById(id); });
        m_searchResults->setItemWidget(item, 1, button);
        m_searchButtons.insert(id, button);
    }
}

void ExtensionsDialog::onNotification(const QString &method,
                                      const QJsonObject &params)
{
    if (method != QStringLiteral("vsix.installProgress")) {
        return;
    }
    if (m_installingId.isEmpty()
        || params.value(QStringLiteral("extensionId")).toString() != m_installingId) {
        return;
    }
    if (params.value(QStringLiteral("indeterminate")).toBool()) {
        m_progress->setRange(0, 0);
        return;
    }
    const int pct =
        qBound(0, int(params.value(QStringLiteral("fraction")).toDouble() * 100.0), 100);
    m_progress->setRange(0, 100);
    m_progress->setValue(pct);
}

void ExtensionsDialog::setBusy(bool busy, const QString &message)
{
    m_installButton->setEnabled(!busy);
    m_idEdit->setEnabled(!busy);
    m_searchButton->setEnabled(!busy);
    if (busy) {
        m_uninstallButton->setEnabled(false);
        m_updateButton->setEnabled(false);
    } else {
        updateInstalledButtons();
    }
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
    for (auto *b : m_searchButtons) {
        if (b) {
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
