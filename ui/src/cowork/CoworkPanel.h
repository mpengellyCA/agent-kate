#pragma once

#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QPointer>
#include <QString>
#include <QWidget>

class CoreClient;
class CoworkPortal;
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

    // The portal owns the ONLY correct implementation of the desktop-wide
    // org.a11y.Status flip: it parks the pre-flip values on disk first, so a crash
    // or a kill-switch can put them back (see CoworkPortal::enableAtspiForUserLaunch).
    // A Chromium browser launched without it cannot export its page over AT-SPI, so
    // the panel needs the portal to make its own launch button do anything useful —
    // but it must NOT re-implement the flip, which is how it ended up happening
    // ungated inside BrowserLaunch::launch (audit F8/F12).
    //
    // QPointer, not a raw pointer: MainWindow builds the panel before the portal and
    // both are its children, so their destruction order is not this class's to assume.
    // A null portal degrades to a launch with an honest message, never a crash.
    void setPortal(CoworkPortal *portal);

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
    // handleEnableRequested reads the live capability policy first (cowork.getPolicy) and
    // then raises the dialog, so the "what still asks for permission" sentence describes
    // the standing grants that are actually set rather than an assumption.
    void handleEnableRequested(const QJsonObject &params);
    void showEnableRequestDialog(const QJsonObject &params, const QStringList &standing,
                                 bool anyR2, bool policyKnown);
    void revokeGrant(const QString &id);
    void toggleKill();
    void enableForActiveThread();
    // Acquire the OS-level permissions (accessibility bus + the remote-control /
    // screen-share portal) on demand, so the dialog is answered now rather than
    // in the middle of an agent's work. Fired automatically when Cowork is
    // switched on; this is the manual re-run after a kill-switch or a decline.
    void requestPreflight();
    // SECURITY / honesty (audit F8): three panel buttons — Enable, "Grant desktop access
    // now", and the Chromium browser launcher — end in a DESKTOP-WIDE org.a11y.Status
    // flip: every application on the session starts exporting its accessibility tree to
    // any local process. That is a real global permission change, so the click that
    // causes it has to be an informed one. This raises the disclosure and returns whether
    // the human agreed; it returns true WITHOUT asking when the flip is already in effect
    // (they have already been told, and nothing new happens) or when the path cannot flip
    // anything. `what` names the action in the human's terms, e.g. "Enable desktop access
    // for this agent?".
    bool confirmDesktopAccessibilityFlip(const QString &what);
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
    QPointer<CoworkPortal> m_portal; // see setPortal
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
