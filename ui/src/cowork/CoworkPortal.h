#pragma once

#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QObject>
#include <QSet>
#include <QString>
#include <QTimer>
#include <QVariantList>
#include <QVariantMap>

#include <functional>

class CoreClient;
class QWidget;

// PortalResponseWaiter listens for the org.freedesktop.portal.Request "Response"
// signal on one request object path, re-emits it as a Qt signal, then self-destructs.
// The XDG portal API is async Request/Response (a method returns a handle; the result
// arrives later on a signal), so we subscribe by predicted path BEFORE the call.
class PortalResponseWaiter : public QObject
{
    Q_OBJECT
public:
    explicit PortalResponseWaiter(const QString &requestPath, QObject *parent = nullptr);

Q_SIGNALS:
    void responded(uint code, const QVariantMap &results);

private Q_SLOTS:
    void onResponse(uint code, const QVariantMap &results);
};

// CoworkPortal services the core's `cowork.portalRequest` notifications by running
// XDG Desktop Portal operations — the Qt UI is the only process with a Wayland
// surface (plan INV-1). v1 implements `screenshot`; it returns ONLY a base64 PNG via
// `cowork.portalResult`. File descriptors / raw frames never cross the JSON bus.
class CoworkPortal : public QObject
{
    Q_OBJECT
public:
    CoworkPortal(CoreClient *core, QWidget *topLevel, QObject *parent = nullptr);

private Q_SLOTS:
    void onNotification(const QString &method, const QJsonObject &params);

private:
    void handleScreenshot(const QJsonObject &req);
    void handleLaunchBrowser(const QJsonObject &req);
    void finishScreenshot(const QString &corrId, int maxDim, const QString &format,
                          uint code, const QVariantMap &results);
    void replyResult(const QString &corrId, const QString &kind, bool ok,
                     const QString &error, const QJsonObject &extra = QJsonObject());
    QString parentWindowHandle() const;

    // --- RemoteDesktop input injection -------------------------------------------
    // A single RemoteDesktop session is created lazily on the first inject (the user
    // approves the portal's remote-control dialog once) and reused for the rest of
    // the run; the kill-switch tears it down.
    void handleInject(const QJsonObject &req);
    void handleKillInject(const QJsonObject &req);
    void startRemoteDesktop();
    void runInjectOps(const QJsonArray &ops);
    void flushInjectQueue();
    void failInjectQueue(const QString &err);
    void teardownRemoteDesktop();
    // closeSessionOnly drops the live portal session (after force-releasing held
    // input) WITHOUT failing the queue — used to rebuild a session that lacks a
    // device type a new batch needs. notifyKeysym/notifyButton send one raw event;
    // releaseHeld synthesises a key/button-up for everything still logically down so
    // a torn-down session can never leave the compositor in a stuck-grab state.
    void closeSessionOnly();
    void releaseHeld();
    void notifyKeysym(int keysym, uint state);
    void notifyButton(int button, uint state);
    // deviceTypesFor returns the RemoteDesktop SelectDevices bitmask the ops need:
    // keyboard (1) and/or pointer (2). Keyboard-only by default so the common case
    // never creates a virtual pointer (the cursor-freeze root cause).
    static uint deviceTypesFor(const QJsonArray &ops);
    // Invoke a portal method that returns a Request handle and deliver its async
    // Response to cb. options gains a fresh handle_token; args precede options.
    void portalRequest(const QString &iface, const QString &method, const QVariantList &args,
                       QVariantMap options, std::function<void(uint, const QVariantMap &)> cb);

    struct PendingInject {
        QString corrId;
        QJsonArray ops;
    };

    CoreClient *m_core = nullptr;
    QWidget *m_topLevel = nullptr;

    QString m_rdSession;            // RemoteDesktop session object path ("" = none)
    bool m_rdReady = false;         // session started, devices granted
    bool m_rdStarting = false;      // Create/Select/Start in flight
    uint m_rdTypes = 0;             // device types the live session was started with
    QList<PendingInject> m_injectQueue;

    QSet<int> m_heldKeys;           // keysyms currently pressed (state==1, no release yet)
    QSet<int> m_heldButtons;        // button codes currently pressed
    QTimer m_idleTimer;             // tears the session down after a spell of no injects
};
