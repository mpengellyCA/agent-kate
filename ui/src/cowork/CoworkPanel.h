#pragma once

#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QString>
#include <QWidget>

class CoreClient;
class CapabilityTile;
class KMessageWidget;
class QPlainTextEdit;
class QPushButton;
class QToolButton;
class QMenu;
class QLabel;
class QComboBox;
class QSlider;
class QSpinBox;
class QVBoxLayout;

// CoworkPanel is the user's control surface for the KDE Plasma Cowork feature: it
// shows what desktop access agents currently hold, answers consent prompts, lets the
// user revoke any grant, and provides a global kill-switch and an audit log. It is
// the security cockpit — the consent authority lives in the core; this panel renders
// it and relays the user's decisions (plan 06).
//
// Plan 13 phase 10 reshapes it into a device-control-centre: the panel body carries
// only the plain-language essentials (status, active agent, capability tiles, active
// grants as sentences, kill-switch). The debug/tuning surfaces (activity log, pointer
// motion, browser tools) move behind an "Advanced" toolbar into their own dialogs.
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
    // An agent asked (via the enable_cowork MCP tool) for desktop access — for
    // itself or for a worker it launched. The human decides here; nothing is
    // switched on until they do.
    void handleEnableRequested(const QJsonObject &params);
    void revokeGrant(const QString &id);
    void toggleKill();
    void enableForActiveThread();
    // Acquire the OS-level permissions (accessibility bus + the remote-control /
    // screen-share portal) on demand, so the dialog is answered now rather than
    // in the middle of an agent's work. Fired automatically when Cowork is
    // switched on; this is the manual re-run after a kill-switch or a decline.
    void requestPreflight();
    void rebuildBrowserMenu();
    void pickCustomBrowser();
    void launchBrowserAndReport(const QString &name, const QString &command, const QString &family);
    void refreshBrowserPrefCombo();
    void savePointerBounds(); // persist + push the user's pointer-motion defaults to core

    // Advanced dialogs (built lazily; the tuning/debug widgets are hosted in the
    // dialog and rebuilt from persisted config each open).
    void showActivityLog();
    void showPointerSettings();
    void showBrowserTools();

    CoreClient *m_core = nullptr;
    QString m_activeThread;
    QString m_activeTitle;
    bool m_killed = false;
    bool m_available = false;

    KMessageWidget *m_status = nullptr;
    QLabel *m_activeLabel = nullptr;
    QPushButton *m_enableBtn = nullptr;
    QPushButton *m_preflightBtn = nullptr;

    // Capability tiles (control-centre grid). Keyed by capability key.
    QVBoxLayout *m_capsLayout = nullptr;         // the tile grid's FlowLayout lives inside
    class FlowLayout *m_tilesFlow = nullptr;     // holds the CapabilityTiles
    QHash<QString, CapabilityTile *> m_tiles;    // capability key -> tile
    QLabel *m_capsEmpty = nullptr;               // shown until the first policy arrives

    // Active grants rendered as sentences (widget list).
    QVBoxLayout *m_grantsLayout = nullptr;
    QLabel *m_grantsEmpty = nullptr;

    QPushButton *m_killBtn = nullptr;

    // --- Advanced surfaces, hosted inside their dialogs on demand ---
    // Pointer motion controls (rebuilt per dialog open, saved to KConfig + core).
    QComboBox *m_pointerSpeed = nullptr;
    QSlider *m_pointerAccuracy = nullptr;
    QLabel *m_pointerAccuracyLabel = nullptr;
    QSpinBox *m_pointerSettle = nullptr;
    // Browser tools.
    QToolButton *m_browserBtn = nullptr;
    QMenu *m_browserMenu = nullptr;
    QComboBox *m_agentBrowserCombo = nullptr;
    // Activity log view + its cached data.
    QPlainTextEdit *m_audit = nullptr;
    QComboBox *m_auditFilter = nullptr;
    QJsonArray m_auditEntries; // last fetched entries, so the filter can re-render offline
    void renderAudit();        // apply m_auditFilter over m_auditEntries into m_audit
};
