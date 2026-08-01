#pragma once

#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QObject>
#include <QPoint>
#include <QPointer>
#include <QSet>
#include <QString>
#include <QTimer>
#include <QVariantList>
#include <QVariantMap>
#include <QVector>

#include <functional>

class CoreClient;
class QWidget;
class QImage;

// PortalResponseWaiter listens for the org.freedesktop.portal.Request "Response"
// signal on one request object path, re-emits it as a Qt signal, then self-destructs.
// The XDG portal API is async Request/Response (a method returns a handle; the result
// arrives later on a signal), so we subscribe by predicted path BEFORE the call.
//
// A waiter ALWAYS finishes. The documented wedge — a portal that accepts the method call
// (so the pending-call watcher sees no error) and then never emits Response — would
// otherwise strand this object and its D-Bus signal match for the rest of the run, one
// per wedged attempt. So every waiter carries its own lifetime backstop: when it expires
// the waiter emits responded(2, {}) — the same "the portal will never answer" code the
// method-level failure path reports — and self-destructs, so no caller can leak one and
// no corrId is left without a reply. The backstop is a LAST resort, generous enough that
// a human sitting on an approval dialog is never cut short; the callers' own watchdogs
// are what react promptly.
class PortalResponseWaiter : public QObject
{
    Q_OBJECT
public:
    explicit PortalResponseWaiter(const QString &requestPath, int timeoutMs,
                                  QObject *parent = nullptr);

    // cancel marks this waiter as belonging to an ABANDONED session attempt without
    // silencing it. Destroying it outright is what it looks like it should do, and is
    // wrong: the continuation hanging off responded() is the ONLY code that knows how to
    // release what a late Response carries (an orphaned portal session handle), so it has
    // to survive long enough to run, bail on its generation check, and self-destruct.
    // All cancel does is shorten the backstop to graceMs, so an abandoned attempt cannot
    // hold this QObject and its D-Bus match for the full waiter lifetime. It NEVER emits
    // synchronously — callers bump the generation counter around it and rely on that.
    // Idempotent, and a no-op once the waiter has answered.
    void cancel(int graceMs);

Q_SIGNALS:
    void responded(uint code, const QVariantMap &results);

private Q_SLOTS:
    void onResponse(uint code, const QVariantMap &results);

private:
    // Emit once and self-destruct. Guarded, because deleteLater is deferred: a second
    // Response (or the backstop firing in the same event-loop pass) must not re-emit.
    void finish(uint code, const QVariantMap &results);

    QString m_requestPath; // kept so the match can be removed by the same signature it was added with
    QTimer m_lifetime;
    bool m_done = false;
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

    // Flip org.a11y.Status on for a browser the HUMAN is launching from the Cowork
    // panel, parking the pre-flip values first so teardown (and a crash-recovery run)
    // can put them back.
    //
    // This exists so that BrowserLaunch::launch does not do it itself. When the flip
    // lived inside the launch it ran for EVERY caller, which silently defeated the
    // agent-facing gate in handleLaunchBrowser (audit F8) and left the desktop in
    // accessibility mode with no parked originals to restore from. Routing the panel
    // through the portal's own park-then-flip keeps exactly one implementation of a
    // change this global — and keeps the human path working, since a browser launched
    // without it cannot expose its page to the agent at all.
    void enableAtspiForUserLaunch();

    // Whether the desktop-wide accessibility flip is CURRENTLY in effect because of us
    // (i.e. we hold the parked originals). The panel asks before deciding whether a click
    // it is about to service would make a new global permission change: if the flip is
    // already on, the human has already been told and consented; if it is not, they must
    // be told before it happens (audit F8). Never a claim about what the flags read right
    // now — only about whether WE turned them on and owe a restore.
    bool desktopAccessibilityFlipped() const { return m_a11yStatusCaptured; }

private Q_SLOTS:
    void onNotification(const QString &method, const QJsonObject &params);

private:
    void handleScreenshot(const QJsonObject &req);
    // KWin's native ScreenShot2 capture — the fast, no-dialog path that targets the exact
    // window/screen/region. Authorized via X-KDE-DBUS-Restricted-Interfaces in the
    // installed .desktop (which needs an ABSOLUTE Exec; build-dir dogfood builds are not
    // authorized). Returns false if it cannot even be dispatched (no bus / KWin absent /
    // pipe failure); on an async denial or a malformed/truncated buffer it falls back to
    // the XDG Screenshot portal itself. startPortalScreenshot is that fallback (the former
    // body of handleScreenshot). On the user's system the frontend xdg-desktop-portal may
    // not even expose org.freedesktop.portal.Screenshot, so ScreenShot2 is the path that
    // actually works — see docs/plans/08-kde-cowork/02-capture.md.
    bool startKWinScreenshot(const QJsonObject &req);
    void startPortalScreenshot(const QJsonObject &req);
    // Scale to maxDim, encode as png/jpeg, and reply — shared by both capture paths.
    void replyWithImage(const QString &corrId, int maxDim, const QString &format, const QImage &img);
    void handleLaunchBrowser(const QJsonObject &req);
    // Chromium-family browsers only export their accessibility tree over AT-SPI when
    // org.a11y.Status reports accessibility enabled AT LAUNCH (Firefox activates
    // lazily on any AT-SPI client connect; Chromium does not). So before launching a
    // Chromium browser we announce ourselves as an assistive technology by flipping the
    // status flags true, and we restore the originals on teardown.
    void enableAtspiStatusForLaunch();
    void restoreAtspiStatus();
    // The flip is DESKTOP-WIDE state that outlives this process: if we die between the
    // flip and the restore, the user is left in screen-reader mode with nothing left to
    // put it back. So the pre-flip values are written to the app config BEFORE the flip,
    // the record is dropped once they have been restored, and a parked record is replayed
    // shortly after startup — deferred off the constructor, since this object is built
    // inside MainWindow's, and skipped when the record's PID is a still-running foreign
    // process (that instance may have a live session relying on the flip).
    // aboutToQuit is a second restore path ahead of the destructor.
    //
    // The record has exactly ONE owner: the process whose PID it names. A second instance
    // that flips while the first still holds the record ADOPTS the parked originals (the
    // bus no longer holds them — they are already flipped) and never rewrites or deletes
    // the record, so the owner's crash safety net survives us.
    void persistA11yOriginals(bool isEnabled, bool screenReader);
    void clearPersistedA11yOriginals();
    void recoverStaleA11yStatus();
    // The flags are set with fire-and-forget writes, so "we asked for it" is not "it is
    // on": an absent or wedged a11y bus swallows them silently. Reads the status back
    // (bounded, see kA11yCallTimeoutMs) into m_a11yEnabled, which is what every
    // preflight/permission reply reports.
    void verifyAtspiEnabled();
    // The teardown the destructor used to own. MainWindow is heap-allocated and never
    // deleted, so ~CoworkPortal does not run on a real exit — aboutToQuit is the path that
    // does. Idempotent, so both may call it.
    void shutdownTeardown();
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
    // abortInjection cancels an in-flight timed playback, releases whatever it left held
    // down, and fails every batch queued behind it — WITHOUT tearing the approved portal
    // session down (a focus change is not a reason to make the human re-approve).
    //
    // SECURITY (audit F3): the two callers are the core's activation watch, which sees
    // focus leave the granted window mid-script, and our own focusWindowChanged hook,
    // which sees an Agent Kate window take focus. The second is the decisive one: it needs
    // no compositor round-trip and it fires exactly when a consent prompt could otherwise
    // be typed by the playback that raised it.
    void abortInjection(const QString &reason);
    // handlePreflight acquires the OS-level permissions UP FRONT, when the human
    // switches Cowork on for an agent — instead of on the agent's first action, which
    // is how a desktop-enabled agent used to sit there doing nothing while an
    // unanswered (or never-raised) portal dialog blocked it. It turns the
    // accessibility bus on and stands the RemoteDesktop + ScreenCast session up, so
    // the human answers one dialog while they are already looking at the screen, and
    // every later action reuses the approved session. It captures nothing: no
    // screenshot is taken, only the permission is obtained.
    void handlePreflight(const QJsonObject &req);
    void startRemoteDesktop();
    // Run a batch of ops for corrId. If no op carries delayMs>0, runs synchronously and
    // returns (the caller replies). If any op has delayMs>0, converts the batch into a
    // non-blocking timed playback and replies success itself once playback drains —
    // signalled by m_playCorrId == corrId on return.
    void runInjectOps(const QString &corrId, const QJsonArray &ops);
    // Execute a single op now (no delay handling). Shared by the sync path and playback.
    //
    // SECURITY (audit F3, absolute half): returns whether the op PROVABLY took effect.
    // An absolute move whose point lies inside no captured screen cannot be sent (there
    // is no stream node to address it to) and is dropped — and a dropped move used to be
    // swallowed here while the core's position mirror recorded the requested point anyway.
    // That desyncs the mirror from the true cursor exactly like the relative-motion bug,
    // with no relative motion involved, so the drop is COUNTED and reported back to the
    // core (see injectOutcome), which then destroys the mirror instead of trusting it.
    bool runOneOp(const QJsonObject &op);
    // Per-batch outcome fields attached to an inject reply: how many ops applied/dropped,
    // and the last absolute move that actually landed. The core cross-checks these against
    // what it asked for and invalidates its pointer mirror on any mismatch.
    QJsonObject injectOutcome() const;
    // Reset the per-batch counters. Called at the head of every batch (sync and timed).
    void beginInjectBatch();
    // Abandon the rest of a batch after an op could not be applied, counting the skipped
    // ops as dropped and releasing anything the batch left held.
    //
    // SECURITY: this is why a drop cannot be a warning. A batch is move→press→release; if
    // the move is dropped and the buttons still fire, the click lands wherever the cursor
    // happened to be — a position no guard ever cleared — and a dropped move mid-drag
    // would leave the button down. The batch stops, and its reply is a failure.
    void abandonBatch(int skipped);
    void flushInjectQueue();
    void failInjectQueue(const QString &err);
    // failPortalStep is the ONLY way a portalRequest failure branch should fail the
    // queue: it picks the wording from what actually happened and appends
    // portalFailureDetail(), so no branch can assert "you declined" when the real
    // fault was a missing portal backend. `declined` is the wording for a genuine
    // refusal, `faulted` for a portal/environment failure (empty = reuse `declined`).
    void failPortalStep(const QString &declined, const QString &faulted = QString());
    void teardownRemoteDesktop();
    // closeSessionOnly drops the live portal session (after force-releasing held
    // input) WITHOUT failing the queue — used to rebuild a session that lacks a
    // device type a new batch needs. notifyKeysym/notifyButton send one raw event;
    // releaseHeld synthesises a key/button-up for everything still logically down so
    // a torn-down session can never leave the compositor in a stuck-grab state.
    void closeSessionOnly();
    void releaseHeld();
    // Close a portal session object path that is NOT (or no longer) m_rdSession — used by
    // the stale-generation bail-outs, which must not orphan a session the portal created
    // for an attempt that has since been abandoned.
    static void closePortalSessionPath(const QString &sessionPath);
    void notifyKeysym(int keysym, uint state);
    void notifyButton(int button, uint state);
    // Pointer ops for true cursor control. Absolute motion needs a screencast stream
    // (streamNodeId) bound to this session so the compositor can map global pixels.
    void notifyPointerMotionAbsolute(uint streamNodeId, double x, double y);
    // Relative motion sends raw dx/dy deltas with NO stream — what pointer-grabbing games
    // (mouse-look) read, and it never needs a screencast map. Absolute is for UI targeting.
    void notifyPointerMotionRelative(double dx, double dy);
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
    // Request). Async like every other portal call — cb receives the duped fd, or -1 on
    // failure — because a wedged portal must never stall the GUI thread.
    void openPipeWireRemote(std::function<void(int)> cb);
    // Drive a timed (profiled-motion) playback: execute one op per QTimer tick.
    void playbackTick();
    // reason is what an in-flight batch is told; empty means the generic teardown wording.
    void stopPlayback(const QString &reason = QString());
    // Invoke a portal method that returns a Request handle and deliver its async
    // Response to cb. options gains a fresh handle_token; args precede options. Nothing
    // here blocks: the method call itself is an asyncCall whose watcher only reports a
    // method-level failure (cb(2,{})), the Response signal delivers the real result.
    void portalRequest(const QString &iface, const QString &method, const QVariantList &args,
                       QVariantMap options, std::function<void(uint, const QVariantMap &)> cb);
    // Release everything the session attempt that is ending still has in flight. Called
    // from closeSessionOnly() — the one place an attempt ends and m_rdGeneration is bumped —
    // and from startRemoteDesktop() as a backstop, so the generation bail-outs, the watchdog
    // and the kill-switch all reach it.
    //
    // It has to satisfy two requirements that pull in opposite directions, which is why it
    // does not simply delete the waiters:
    //   * nothing may be stranded — the wedge case m_rdWatchdog exists for would otherwise
    //     leave one waiter (QObject + D-Bus signal match) armed per abandoned attempt,
    //     because a generation guard only makes a continuation a no-op, it does not free
    //     what is waiting to run it;
    //   * nothing may be silenced — the CreateSession continuation's stale-generation branch
    //     is the only code that closes a session the portal minted for an attempt nobody
    //     owns any more, and a deleted waiter never runs it.
    // See the definition for the two layers that give both.
    void abandonPortalWaiters();
    // A trailing clause naming WHY the last portal call failed, appended to the
    // user-facing failure. Empty when the portal answered normally (a plain decline
    // needs no explanation); actionable when the failure is a fixable environment
    // fault rather than the user saying no.
    QString portalFailureDetail() const;

    struct PendingInject {
        QString corrId;
        QJsonArray ops;
    };

    // Preflight requests waiting on the same session hand-shake the inject queue waits
    // on. Kept apart from m_injectQueue because they carry no ops: they are satisfied
    // by the session coming up (or failed by it being declined), not by anything run.
    QStringList m_preflightCorrIds;

    // The last D-Bus error a portal call returned, so a failure can say what went
    // wrong instead of implying the user declined something. The name is kept
    // separately because it, not the (localisable, sometimes empty) message, is what
    // identifies a missing portal backend.
    QString m_lastPortalError;     // human-readable text (falls back to the error name)
    QString m_lastPortalErrorName; // D-Bus error name, e.g. org.freedesktop.DBus.Error.UnknownMethod

    // One captured monitor stream: its PipeWire node id and its rect in global desktop
    // pixels. The vector is the coordinate map absolute motion resolves against.
    struct StreamInfo {
        uint nodeId = 0;
        int originX = 0;
        int originY = 0;
        int w = 0;
        int h = 0;
    };

    // QPointer, not a raw pointer: m_core is a SIBLING child of the same parent, so on a
    // parent-driven teardown it may already be gone by the time anything here runs.
    QPointer<CoreClient> m_core;
    QWidget *m_topLevel = nullptr;

    QString m_rdSession;            // RemoteDesktop session object path ("" = none)
    // The session object path the IN-FLIGHT CreateSession was asked to mint, predicted from
    // the session_handle_token we chose (portalHandlePath). Set when CreateSession is armed,
    // cleared when its Response lands for the live attempt or when the attempt is abandoned.
    // It exists so an abandoned attempt can close a session the portal minted for it even in
    // the case where no Response ever arrives — see abandonPortalWaiters().
    QString m_rdPendingSessionPath;
    // Identity of the current session ATTEMPT. Every portal hand-shake step is an async
    // Response that can land long after the user has been sitting on the dialog, by which
    // time a kill-switch (or a failure) may have torn the attempt down and a new one may
    // already be in flight. Each continuation captures this at arm time and bails when it
    // has moved on, so a late reply can neither corrupt the live attempt nor orphan the
    // resources it carries. Bumped by startRemoteDesktop() and closeSessionOnly() — the
    // only two places an attempt begins or ends.
    int m_rdGeneration = 0;
    bool m_rdReady = false;         // session started, devices granted
    bool m_rdStarting = false;      // Create/Select/Start in flight
    uint m_rdTypes = 0;             // device types the live session was started with
    QList<PendingInject> m_injectQueue;
    // Hand-shake watchdog. The documented xdg-desktop-portal frontend race is a portal
    // that ACCEPTS the method (so the watcher sees no error) and then never emits
    // Response — which would leave m_rdStarting true forever and wedge every later
    // request behind a session that can never come up. Generation-guarded so a late fire
    // cannot fail an attempt that has already been replaced.
    QTimer m_rdWatchdog;
    int m_rdWatchdogGen = 0;
    // Waiters armed for the current session attempt, so the attempt can free them when it
    // ends instead of leaving them subscribed to a Response that will never come. QPointer
    // because a waiter that has already answered destroys itself.
    QList<QPointer<PortalResponseWaiter>> m_rdWaiters;

    QSet<int> m_heldKeys;           // keysyms currently pressed (state==1, no release yet)
    QSet<int> m_heldButtons;        // button codes currently pressed
    // No idle timer: the session is kept alive for the whole app run after the one-time
    // portal approval; only the kill-switch / app exit tear it down.

    // --- ScreenCast (for absolute pointer motion) --------------------------------
    // Absolute motion (NotifyPointerMotionAbsolute) needs a screencast stream bound to
    // the same RemoteDesktop session. We only stand it up when a batch contains a move.
    bool m_scReady = false;         // screencast requested AND streams parsed this session
    QVector<StreamInfo> m_streams;  // captured monitor rects (the coordinate map)
    // m_ptr / m_ptrKnown are EVIDENCE, not bookkeeping: they record the last absolute move
    // this process actually delivered to the compositor (m_ptrKnown false = no absolute
    // move has provably landed since the session came up). They are reported back to the
    // core with every inject reply so the core's own mirror can be checked against what
    // the desktop really did rather than against what was requested.
    QPoint m_ptr;                   // last absolute pointer position we drove to
    bool m_ptrKnown = false;        // ...and whether that move provably landed
    int m_batchApplied = 0;         // ops applied in the current batch
    int m_batchDropped = 0;         // ops the current batch could NOT apply
    QString m_batchError;           // why the current batch was abandoned (empty = fine)
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
    bool m_a11yEnabled = false;        // read back AFTER the flip — the value we report
    bool m_shutdownDone = false;       // shutdownTeardown() has run (aboutToQuit or dtor)

#ifdef AK_HAVE_PIPEWIRE
    // SPIKE-1 defense: an opaque handle to the minimal libpipewire consumer that keeps
    // one stream "consumed" so KWin honours absolute motion. Defined in the .cpp.
    struct PwConsumer;
    PwConsumer *m_pwConsumer = nullptr;
    void startPwConsumer(uint nodeId);
    void stopPwConsumer();
#endif
};
