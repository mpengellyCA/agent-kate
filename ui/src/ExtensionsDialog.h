#pragma once

#include <QDialog>
#include <QHash>

class CoreClient;
class QJsonArray;
class QJsonObject;
class QLabel;
class QLineEdit;
class QListWidget;
class QListWidgetItem;
class QProgressBar;
class QPushButton;
class QTabWidget;
class QTreeWidget;
class QTreeWidgetItem;

// ExtensionsDialog installs and lists VS Code language extensions reused from
// the Open VSX registry. The "Installed" tab manages what is already on disk
// and lets the user install by id; the "Popular" tab is a curated one-click
// catalog so users do not have to leave the app to discover extensions.
// All work goes through the core's vsix.install / vsix.list / vsix.catalog
// IPC methods.
class ExtensionsDialog : public QDialog
{
    Q_OBJECT
public:
    explicit ExtensionsDialog(CoreClient *core, QWidget *parent = nullptr);

Q_SIGNALS:
    // Emitted after the installed-extension set may have changed, so the main
    // window can re-register language servers.
    void extensionsChanged();

private:
    void refresh();
    void refreshCatalog();
    void installById(const QString &id);
    void installFromEdit();
    void uninstallSelected();
    void updateSelected();
    void runSearch();
    void populate(const QJsonArray &extensions);
    void populateCatalog(const QJsonArray &entries);
    void populateSearch(const QJsonArray &entries);
    void setBusy(bool busy, const QString &message);
    void onNotification(const QString &method, const QJsonObject &params);
    void updateInstalledButtons();

    CoreClient *m_core = nullptr;
    QTabWidget *m_tabs = nullptr;
    QLineEdit *m_idEdit = nullptr;
    QPushButton *m_installButton = nullptr;
    QListWidget *m_list = nullptr;
    QPushButton *m_uninstallButton = nullptr;
    QPushButton *m_updateButton = nullptr;
    QTreeWidget *m_catalog = nullptr;
    QHash<QString, QPushButton *> m_catalogButtons;
    QLineEdit *m_searchEdit = nullptr;
    QPushButton *m_searchButton = nullptr;
    QTreeWidget *m_searchResults = nullptr;
    QHash<QString, QPushButton *> m_searchButtons;
    QProgressBar *m_progress = nullptr;
    QLabel *m_status = nullptr;
    QString m_installingId; // id of the in-flight install, for progress filtering
};
