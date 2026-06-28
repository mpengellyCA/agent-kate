#pragma once

#include <QDialog>
#include <QMap>

#include "ProviderConfig.h"

class QLabel;
class QLineEdit;
class QListWidget;
class QPushButton;

// ProvidersDialog manages third-party API provider profiles (Fireworks,
// OpenRouter, …). The left list holds the profiles; the right form edits the
// selected one's non-secret fields and its API key. Non-secret fields persist to
// KConfig; the key is written to KWallet (or, when KWallet is unavailable,
// supplied through the named environment variable instead).
//
// See docs/plans/11-third-party-providers.md.
class ProvidersDialog : public QDialog
{
    Q_OBJECT
public:
    explicit ProvidersDialog(QWidget *parent = nullptr);

private:
    void rebuildList(int selectRow = -1);
    void loadIntoForm(int row);
    void commitForm();    // copy form fields back into m_profiles[m_current]
    void addProfile();
    void removeProfile();
    void saveAndAccept();
    void updateEditableState();

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
};
