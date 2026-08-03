#pragma once

#include <QDialog>
#include <QMap>

#include "ProviderConfig.h"

class QComboBox;
class QGroupBox;
class QJsonArray;
class QLabel;
class QLineEdit;
class QListWidget;
class QPushButton;
class QVBoxLayout;
class CoreClient;

// ProvidersDialog manages both provider mechanisms, one section each:
//
//   - "API providers (Claude Code)": third-party API provider profiles
//     (Fireworks, OpenRouter, …) routed by per-launch env injection (plan
//     11). The left list holds the profiles; the right form edits the
//     selected one's non-secret fields and its API key. Non-secret fields
//     persist to KConfig; the key is written to KWallet (or, when KWallet is
//     unavailable, supplied through the named environment variable instead).
//
//   - "Kimi provider registry" (plan 26): the engine's OWN persistent
//     registry in its home directory, edited through the kimiProvider.* RPCs
//     (which shell out to `kimi provider …`). Keys are held by kimi's
//     credential store — Agent Kate never sees or stores them, and the
//     section says so. A home selector chooses WHICH registry: the user's
//     default, or a thread's private KIMI_CODE_HOME. The section hides when
//     no registered engine declares providerRegistry (HarnessTraits).
//
// See docs/plans/11-third-party-providers.md and 26-engine-services.md.
class ProvidersDialog : public QDialog
{
    Q_OBJECT
public:
    // core drives the kimi registry section; without it (the legacy call
    // site) that section still lists per the traits but says it needs a core
    // connection. The claude-side profile editor never needs the core.
    explicit ProvidersDialog(QWidget *parent = nullptr, CoreClient *core = nullptr);

private:
    void rebuildList(int selectRow = -1);
    void loadIntoForm(int row);
    void commitForm();    // copy form fields back into m_profiles[m_current]
    void addProfile();
    void removeProfile();
    void saveAndAccept();
    void updateEditableState();

    // --- the kimi registry section -------------------------------------
    void buildKimiSection(QVBoxLayout *outer);
    void refreshKimiHomes();     // "User default" + threads with a KIMI_CODE_HOME
    void refreshKimiProviders(); // kimiProvider.list for the selected home
    void renderKimiProviders(const QJsonArray &providers);
    void kimiImportCatalog();
    void kimiAddFromUrl();
    void kimiRemoveSelected();
    // The selected home's threadId ("" = the user's default home).
    QString kimiHomeThreadId() const;
    void setKimiBusy(bool busy, const QString &status = QString());

    QList<ProviderProfile> m_profiles;
    int m_current = -1;
    QMap<QString, QString> m_pendingKeys; // id -> new key text to write on save

    QListWidget *m_list = nullptr;
    QLineEdit *m_name = nullptr;
    QLineEdit *m_baseUrl = nullptr;
    QLineEdit *m_envVar = nullptr;
    QLineEdit *m_key = nullptr;
    QLabel *m_keyStatus = nullptr;
    QLabel *m_walletNote = nullptr;
    QPushButton *m_removeBtn = nullptr;
    QMap<QString, QLineEdit *> m_modelEdits; // slot -> edit

    CoreClient *m_core = nullptr;
    QGroupBox *m_kimiSection = nullptr;
    QComboBox *m_kimiHome = nullptr;
    QListWidget *m_kimiList = nullptr;
    QLabel *m_kimiStatus = nullptr;
    QPushButton *m_kimiRefresh = nullptr;
    QPushButton *m_kimiImport = nullptr;
    QPushButton *m_kimiAdd = nullptr;
    QPushButton *m_kimiRemove = nullptr;
};
