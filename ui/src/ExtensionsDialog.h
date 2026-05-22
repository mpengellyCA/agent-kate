#pragma once

#include <QDialog>

class CoreClient;
class QJsonArray;
class QLabel;
class QLineEdit;
class QListWidget;
class QPushButton;

// ExtensionsDialog installs and lists VS Code language extensions reused from
// the Open VSX registry. Installing an extension makes the language server it
// bundles available to the editor (see LspManager::registerExtensionServer).
// All work goes through the core's vsix.install / vsix.list IPC methods.
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
    void install();
    void populate(const QJsonArray &extensions);
    void setBusy(bool busy, const QString &message);

    CoreClient *m_core = nullptr;
    QLineEdit *m_idEdit = nullptr;
    QPushButton *m_installButton = nullptr;
    QListWidget *m_list = nullptr;
    QLabel *m_status = nullptr;
};
