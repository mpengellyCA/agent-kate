#pragma once

#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QObject>
#include <QPoint>
#include <QSet>
#include <QString>
#include <QTimer>
#include <QVariantList>
#include <QVariantMap>
#include <QVector>

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
    ~CoworkPortal() override;

private Q_SLOTS:
    void onNotification(const QString &method, const QJsonObject &params);

private:
    void handleScreenshot(const QJsonObject &req);
    void handleLaunchBrowser(const QJsonObject &req);
    // Chromium-family browsers only export their accessibility tree over AT-SPI when
    // org.a11y.Status reports accessibility enabled AT LAUNCH (Firefox activates
    // lazily on any AT-SPI client connect; Chromium does not). So before launching a
    // Chromium browser we announce ourselves as an assistive technology by flipping the
    // status flags true, and we restore the originals on teardown.
    void enableAtspiStatusForLaunch();
    void restoreAtspiStatus();
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
    // Run a batch of ops for corrId. If no op carries delayMs>0, runs synchronously and
    // returns (the caller replies). If any op has delayMs>0, converts the batch into a
    // non-blocking timed playback and replies success itself once playback drains —
    // signalled by m_playCorrId == corrId on return.
    void runInjectOps(const QString &corrId, const QJsonArray &ops);
    // Execute a single op now (no delay handling). Shared by the sync path and playback.
    void runOneOp(const QJsonObject &op);
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
    // Pointer ops for true cursor control. Absolute motion needs a screencast stream
    // (streamNodeId) bound to this session so the compositor can map global pixels.
    void notifyPointerMotionAbsolute(uint streamNodeId, double x, double y);
    void notifyAxis(double dx, double dy);
    void notifyAxisDiscrete(uint axis, int steps);
    // Map an absolute global desktop pixel to (streamNodeId, local x/y). Returns false
    // when no captured stream contains the point (the move is then skipped + logged).
    bool globalToStream(int gx, int gy, uint &outNode, double &outLx, double &outLy) const;
    // deviceTypesFor returns the RemoteDesktop SelectDevices bitmask the ops need:
    // keyboard (1) and/or pointer (2). Keyboard-only by default so the common case
    // never creates a virtual pointer (the cursor-freeze root cause).
    static uint deviceTypesFor(const QJsonArray &ops);
    // True iff a batch contains a move op — only then do we stand up screencast (lazy:
    // keyboard / button / scroll paths must never spin up frame capture).
    static bool needsScreencastFor(const QJsonArray &ops);
    // Parse the ScreenCast Start results["streams"] payload (a(ua{sv})) into m_streams.
    void parseStreams(const QVariant &v);
    // Open the PipeWire remote fd for the live session (plain method call, not a
    // Request). Returns the duped fd, or -1 on failure.
    int openPipeWireRemote();
    // Drive a timed (profiled-motion) playback: execute one op per QTimer tick.
    void playbackTick();
    void stopPlayback();
    // Invoke a portal method that returns a Request handle and deliver its async
    // Response to cb. options gains a fresh handle_token; args precede options.
    void portalRequest(const QString &iface, const QString &method, const QVariantList &args,
                       QVariantMap options, std::function<void(uint, const QVariantMap &)> cb);

    struct PendingInject {
        QString corrId;
        QJsonArray ops;
    };

    // One captured monitor stream: its PipeWire node id and its rect in global desktop
    // pixels. The vector is the coordinate map absolute motion resolves against.
    struct StreamInfo {
        uint nodeId = 0;
        int originX = 0;
        int originY = 0;
        int w = 0;
        int h = 0;
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
    // No idle timer: the session is kept alive for the whole app run after the one-time
    // portal approval; only the kill-switch / app exit tear it down.

    // --- ScreenCast (for absolute pointer motion) --------------------------------
    // Absolute motion (NotifyPointerMotionAbsolute) needs a screencast stream bound to
    // the same RemoteDesktop session. We only stand it up when a batch contains a move.
    bool m_scReady = false;         // screencast requested AND streams parsed this session
    QVector<StreamInfo> m_streams;  // captured monitor rects (the coordinate map)
    QPoint m_ptr;                   // last absolute pointer position we drove to
    int m_pwFd = -1;                // duped PipeWire remote fd (-1 = none)

    // --- Timed (profiled-motion) playback ----------------------------------------
    // A profiled move is many move ops each carrying delayMs. We must not block the Qt
    // event loop with sleeps, so a batch with any delayMs>0 is driven one op per tick.
    QTimer m_playTimer;             // singleShot, fires playbackTick()
    QJsonArray m_playOps;           // ops still to run in the current playback
    int m_playIdx = 0;              // index of the next op to execute
    QString m_playCorrId;           // corrId to reply success to when playback drains

    bool m_a11yStatusCaptured = false; // whether we recorded the user's original a11y status
    bool m_origIsEnabled = false;      // org.a11y.Status.IsEnabled before we forced it on
    bool m_origScreenReader = false;   // org.a11y.Status.ScreenReaderEnabled before we forced it on

#ifdef AK_HAVE_PIPEWIRE
    // SPIKE-1 defense: an opaque handle to the minimal libpipewire consumer that keeps
    // one stream "consumed" so KWin honours absolute motion. Defined in the .cpp.
    struct PwConsumer;
    PwConsumer *m_pwConsumer = nullptr;
    void startPwConsumer(uint nodeId);
    void stopPwConsumer();
#endif
};
