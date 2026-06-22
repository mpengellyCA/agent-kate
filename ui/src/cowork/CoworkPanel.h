#pragma once

#include <QHash>
#include <QJsonObject>
#include <QString>
#include <QWidget>

class CoreClient;
class KMessageWidget;
class QTreeWidget;
class QPlainTextEdit;
class QPushButton;
class QToolButton;
class QMenu;
class QLabel;
class QCheckBox;
class QComboBox;
class QSlider;
class QSpinBox;
class QVBoxLayout;

// CoworkPanel is the user's control surface for the KDE Plasma Cowork feature: it
// shows what desktop access agents currently hold, answers consent prompts, lets the
// user revoke any grant, and provides a global kill-switch and an audit log. It is
// the security cockpit — the consent authority lives in the core; this panel renders
// it and relays the user's decisions (plan 06).
class CoworkPanel : public QWidget
{
    Q_OBJECT
public:
    explicit CoworkPanel(CoreClient *core, QWidget *parent = nullptr);

public Q_SLOTS:
    // Told by MainWindow which agent thread is active, so "Enable Cowork" targets it.
    void setActiveThread(const QString &threadId, const QString &title);

private Q_SLOTS:
    void onNotification(const QString &method, const QJsonObject &params);
    void refresh();

private:
    void refreshStatus();
    void refreshGrants();
    void refreshAudit();
    void refreshPolicy();
    void handleGrantRequested(const QJsonObject &params);
    void revokeSelected();
    void toggleKill();
    void enableForActiveThread();
    void rebuildBrowserMenu();
    void pickCustomBrowser();
    void launchBrowserAndReport(const QString &name, const QString &command, const QString &family);
    void refreshBrowserPrefCombo();
    void savePointerBounds(); // persist + push the user's pointer-motion defaults to core

    CoreClient *m_core = nullptr;
    QString m_activeThread;
    QString m_activeTitle;
    bool m_killed = false;
    bool m_available = false;

    KMessageWidget *m_status = nullptr;
    QLabel *m_activeLabel = nullptr;
    QPushButton *m_enableBtn = nullptr;
    QToolButton *m_browserBtn = nullptr;
    QMenu *m_browserMenu = nullptr;
    QComboBox *m_agentBrowserCombo = nullptr;
    QVBoxLayout *m_capsLayout = nullptr;                 // holds the capability toggles
    QHash<QString, QCheckBox *> m_policyChecks;          // capability key -> switch
    QComboBox *m_pointerSpeed = nullptr;                 // default agent pointer px/s
    QSlider *m_pointerAccuracy = nullptr;                // 0..100% path exactness
    QLabel *m_pointerAccuracyLabel = nullptr;           // "Accuracy: N%"
    QSpinBox *m_pointerSettle = nullptr;                 // ms to settle before a click
    QTreeWidget *m_grants = nullptr;
    QPushButton *m_revokeBtn = nullptr;
    QPushButton *m_killBtn = nullptr;
    QPlainTextEdit *m_audit = nullptr;
};
