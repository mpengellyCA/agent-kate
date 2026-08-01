#include "CoworkPortal.h"

#include "BrowserLaunch.h"
#include "ipc/CoreClient.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QBuffer>
#include <QCoreApplication>
#include <QDBusArgument>
#include <QDBusConnection>
#include <QDBusConnectionInterface>
#include <QDBusMessage>
#include <QDBusObjectPath>
#include <QDBusPendingCallWatcher>
#include <QDBusPendingReply>
#include <QDBusUnixFileDescriptor>
#include <QDBusVariant>
#include <QFile>
#include <QGuiApplication>
#include <QImage>
#include <QJsonValue>
#include <QPointer>
#include <QRandomGenerator>
#include <QSocketNotifier>
#include <QUrl>
#include <QWidget>
#include <QWindow>

#include <cerrno>
#include <fcntl.h>
#include <memory>
#include <unistd.h>

#ifdef AK_HAVE_PIPEWIRE
#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#endif

namespace {
constexpr auto kPortalService = "org.freedesktop.portal.Desktop";
constexpr auto kPortalPath = "/org/freedesktop/portal/desktop";

// Where the pre-flip org.a11y.Status values are parked while they are flipped, so a
// process that dies mid-session can still be undone by the next one.
constexpr auto kA11yGroup = "CoworkA11y";
constexpr auto kA11yPending = "Pending";
constexpr auto kA11yOrigIsEnabled = "OrigIsEnabled";
constexpr auto kA11yOrigScreenReader = "OrigScreenReaderEnabled";
// PID of the process that parked the record. s_a11yRecoveryDone only guards a SECOND
// CoworkPortal inside one process; a second AgentKate process would otherwise restore
// (and delete) the record while the first still holds the flags flipped for a live
// session. The PID makes the record self-describing across processes.
constexpr auto kA11yPid = "Pid";

// Only the first CoworkPortal in a process may replay a parked record: a second instance
// built while the first holds the flags flipped would "restore" over a live session.
bool s_a11yRecoveryDone = false;

// KWin's native screenshot interface (no portal dialog). Capture methods take a unix
// pipe write-fd; KWin streams the raw image into it and returns a {width,height,stride,
// format} results map (format is a QImage::Format value).
constexpr auto kKWinService = "org.kde.KWin";
constexpr auto kScreenShot2Path = "/org/kde/KWin/ScreenShot2";
constexpr auto kScreenShot2Iface = "org.kde.KWin.ScreenShot2";

// In-flight ScreenShot2 capture: the raw pixel bytes drained off the pipe plus the
// returned geometry. Both the pipe drain (QSocketNotifier) and the D-Bus reply
// (QDBusPendingCallWatcher) complete independently; we assemble the image once both land.
struct KWinShot {
    int readFd = -1;
    QByteArray buf;
    QVariantMap results;
    bool gotResults = false;
    bool gotEof = false;
    bool error = false;
    bool done = false;
    QSocketNotifier *notifier = nullptr;
    QDBusPendingCallWatcher *watcher = nullptr;
};

// Map a cowork Target to the ScreenShot2 method + its leading args (everything before the
// options map and the pipe fd). CaptureWindow takes the KWin internalId UUID exactly as
// listWindows reports it (braces and all); CaptureArea takes x,y (i) and w,h (u).
void kwinCaptureCall(const QJsonObject &req, QString &method, QVariantList &leading)
{
    const QJsonObject t = req.value(QStringLiteral("target")).toObject();
    const QString kind = t.value(QStringLiteral("kind")).toString();
    if (kind == QLatin1String("window")) {
        const QString wid = t.value(QStringLiteral("windowId")).toString();
        if (!wid.isEmpty()) {
            method = QStringLiteral("CaptureWindow");
            leading = {wid};
        } else {
            method = QStringLiteral("CaptureActiveWindow");
        }
    } else if (kind == QLatin1String("region")) {
        // cowork.Rect has no JSON tags, so it serialises as X/Y/W/H.
        const QJsonObject r = t.value(QStringLiteral("region")).toObject();
        const int x = r.value(QStringLiteral("X")).toInt();
        const int y = r.value(QStringLiteral("Y")).toInt();
        const int w = r.value(QStringLiteral("W")).toInt();
        const int h = r.value(QStringLiteral("H")).toInt();
        if (w > 0 && h > 0) {
            method = QStringLiteral("CaptureArea");
            leading = {x, y, uint(w), uint(h)};
        } else {
            method = QStringLiteral("CaptureActiveScreen");
        }
    } else if (kind == QLatin1String("screen")) {
        const QString name = t.value(QStringLiteral("screen")).toString();
        if (!name.isEmpty()) {
            method = QStringLiteral("CaptureScreen");
            leading = {name};
        } else {
            method = QStringLiteral("CaptureActiveScreen");
        }
    } else {
        // "any"/empty/unknown → the active screen (the default cowork screenshot target).
        method = QStringLiteral("CaptureActiveScreen");
    }
}

// org.a11y.Status lives on the session bus and reports whether accessibility is
// enabled; toolkits read it to decide whether to export an AT-SPI tree. Chromium
// checks it at launch, so we set it before launching a Chromium browser.
//
// Built as a raw QDBusMessage, never a QDBusInterface: constructing a QDBusInterface
// introspects the remote object SYNCHRONOUSLY, so a wedged a11y bus would stall
// whoever built it — including the startup path that replays a parked record.
QDBusMessage a11yStatusCall(const QString &method)
{
    return QDBusMessage::createMethodCall(QStringLiteral("org.a11y.Bus"),
                                          QStringLiteral("/org/a11y/bus"),
                                          QStringLiteral("org.freedesktop.DBus.Properties"), method);
}

// Short timeout: this only runs on user-driven enable, and an unresponsive a11y bus
// must cost a moment, not the default 25 s D-Bus timeout.
constexpr int kA11yCallTimeoutMs = 2000;

// Hand-shake watchdog budgets. The non-interactive steps raise no dialog, so a portal that
// has not answered them in kRdHandshakeTimeoutMs is not going to. Start DOES raise the
// approval dialog, and a human may sit on it for a while, so that step gets a budget long
// enough not to cut a real decision short while still bounding a wedged portal.
constexpr int kRdHandshakeTimeoutMs = 25000;
constexpr int kRdDialogTimeoutMs = 120000;

// Last-resort lifetime for a single PortalResponseWaiter. The watchdogs above are what
// react promptly to a wedged portal; this only exists so that no waiter can outlive the
// request it was created for, on ANY path (including the screenshot portal, which has no
// watchdog of its own). Comfortably longer than the longest legitimate wait — a human
// sitting on the approval dialog or picking a window in the interactive screenshot flow —
// so it never cuts a real decision short.
constexpr int kPortalWaiterLifetimeMs = 300000; // 5 min

// Lifetime a waiter gets once its session attempt has been ABANDONED. It is not
// destroyed on the spot: its continuation is the only code that knows how to release what
// a late Response hands back (an orphaned CreateSession session handle), so it has to stay
// subscribed long enough to run, bail on the generation check, and self-destruct. Generous
// next to the non-interactive step it covers (CreateSession raises no dialog and answers in
// milliseconds), yet far short of the 5-minute backstop, so an abandoned attempt cannot
// hold a QObject + D-Bus match for the rest of the run.
constexpr int kPortalCancelledGraceMs = 45000; // 45 s

// How long a KWin ScreenShot2 capture may take before it is abandoned in favour of the
// XDG portal fallback. Generous next to a real capture (milliseconds — no dialog, no
// user interaction) and comfortably inside akcore's own ~125 s portal-request timeout,
// so the agent gets a real answer instead of a timeout with nothing tried after it.
constexpr int kKWinShotLifetimeMs = 30000; // 30 s

// portalHandlePath builds the object path the portal will use for a handle WE name with a
// token: kind "request" for a Request, "session" for a Session. Both follow the same
// documented convention — the unique bus name with its leading ':' dropped and every '.'
// mangled to '_' — which is what lets a Response be subscribed BEFORE the method call
// returns, and what lets an abandoned CreateSession's session be closed even if its reply
// never arrives.
QString portalHandlePath(const QString &kind, const QString &token)
{
    QString sender = QDBusConnection::sessionBus().baseService();
    if (sender.startsWith(QLatin1Char(':'))) {
        sender.remove(0, 1);
    }
    sender.replace(QLatin1Char('.'), QLatin1Char('_'));
    return QStringLiteral("%1/%2/%3/%4")
        .arg(QString::fromLatin1(kPortalPath), kind, sender, token);
}

bool readA11yStatus(const QString &prop, bool fallback)
{
    QDBusMessage m = a11yStatusCall(QStringLiteral("Get"));
    m.setArguments({QStringLiteral("org.a11y.Status"), prop});
    const QDBusMessage r = QDBusConnection::sessionBus().call(m, QDBus::Block, kA11yCallTimeoutMs);
    if (r.type() != QDBusMessage::ReplyMessage || r.arguments().isEmpty()) {
        return fallback;
    }
    const QVariant v = r.arguments().constFirst();
    return v.canConvert<QDBusVariant>() ? v.value<QDBusVariant>().variant().toBool() : v.toBool();
}

// blocking=true is for the restore paths only (destructor / aboutToQuit): an asyncCall
// issued as the app tears down may never make it onto the wire, which would strand the
// desktop in screen-reader mode. Every other caller stays non-blocking.
void writeA11yStatus(const QString &prop, bool value, bool blocking = false)
{
    QDBusMessage m = a11yStatusCall(QStringLiteral("Set"));
    m.setArguments({QStringLiteral("org.a11y.Status"), prop, QVariant::fromValue(QDBusVariant(value))});
    if (blocking) {
        QDBusConnection::sessionBus().call(m, QDBus::Block, kA11yCallTimeoutMs);
    } else {
        QDBusConnection::sessionBus().asyncCall(m);
    }
}

// /proc/<pid>/comm, trimmed ("" when the pid is gone or unreadable). The kernel truncates
// comm to 15 chars, so callers must compare it against a comm — never against a full name.
QString procComm(qint64 pid)
{
    QFile f(QStringLiteral("/proc/%1/comm").arg(pid));
    if (!f.open(QIODevice::ReadOnly)) {
        return QString();
    }
    return QString::fromLocal8Bit(f.readAll()).trimmed();
}

// Our own comm, with a name-derived fallback for a system without procfs.
QString ownComm()
{
    QString c = procComm(QCoreApplication::applicationPid());
    if (!c.isEmpty()) {
        return c;
    }
    c = QCoreApplication::applicationName();
    if (c.isEmpty()) {
        c = QStringLiteral("agentkate");
    }
    c.truncate(15); // the kernel's comm length
    return c;
}

// Who owns the parked org.a11y.Status record. The record has exactly one owner — the
// process it names — and only that owner may rewrite or delete it.
enum class A11yRecordHolder {
    None,    // no record parked
    Ours,    // this process wrote it
    Foreign, // another process that is still alive and looks like us
    Stale,   // the writer is gone (or its pid was recycled by something else)
};

A11yRecordHolder a11yRecordHolder(const KConfigGroup &g)
{
    if (!g.readEntry(QLatin1String(kA11yPending), false)) {
        return A11yRecordHolder::None;
    }
    const qint64 pid = g.readEntry(QLatin1String(kA11yPid), qint64(0));
    if (pid == QCoreApplication::applicationPid()) {
        return A11yRecordHolder::Ours;
    }
    if (pid <= 0) {
        return A11yRecordHolder::Stale; // pre-PID record: nobody claims it
    }
    // Liveness alone is not identity: pids recycle, and a stranded record is exactly what a
    // crash or a reboot leaves behind — when the recorded pid is most likely to have been
    // handed to some unrelated process. So the holder must also LOOK like us.
    if (!QFile::exists(QStringLiteral("/proc/%1").arg(pid))) {
        return A11yRecordHolder::Stale; // no such process
    }
    const QString comm = procComm(pid);
    if (comm.isEmpty()) {
        // The process EXISTS but we cannot read its comm (a different uid, a hardened
        // procfs). Fail CLOSED: treating an unidentifiable live process as dead would
        // restore the desktop out from under a session that may still be using it.
        return A11yRecordHolder::Foreign;
    }
    return comm == ownComm() ? A11yRecordHolder::Foreign : A11yRecordHolder::Stale;
}
} // namespace

PortalResponseWaiter::PortalResponseWaiter(const QString &requestPath, int timeoutMs,
                                           QObject *parent)
    : QObject(parent), m_requestPath(requestPath)
{
    QDBusConnection::sessionBus().connect(
        QString::fromLatin1(kPortalService), requestPath,
        QStringLiteral("org.freedesktop.portal.Request"), QStringLiteral("Response"),
        this, SLOT(onResponse(uint, QVariantMap)));
    // Backstop: a portal that accepts the call and never emits Response must not strand
    // this object (and its signal match) for the rest of the run.
    m_lifetime.setSingleShot(true);
    connect(&m_lifetime, &QTimer::timeout, this, [this, requestPath] {
        qWarning("cowork: no portal Response on %s within the waiter lifetime; giving up",
                 qUtf8Printable(requestPath));
        finish(2, QVariantMap()); // 2 = the portal will not answer (same code as a method failure)
    });
    m_lifetime.start(timeoutMs);
}

void PortalResponseWaiter::cancel(int graceMs)
{
    if (m_done) {
        return; // already answered and self-destructing
    }
    // Only ever shortens: a waiter cancelled twice, or cancelled late in its life, must not
    // have its deadline pushed back out.
    const int remaining = m_lifetime.remainingTime();
    if (remaining < 0 || remaining > graceMs) {
        m_lifetime.start(graceMs);
    }
}

void PortalResponseWaiter::onResponse(uint code, const QVariantMap &results)
{
    finish(code, results);
}

void PortalResponseWaiter::finish(uint code, const QVariantMap &results)
{
    if (m_done) {
        return; // deleteLater is deferred; answer exactly once
    }
    m_done = true;
    m_lifetime.stop();
    // Drop the D-Bus match before the callback runs: the callback may tear the attempt
    // down (and delete us), and a late Response must find nothing subscribed.
    QDBusConnection::sessionBus().disconnect(
        QString::fromLatin1(kPortalService), m_requestPath,
        QStringLiteral("org.freedesktop.portal.Request"), QStringLiteral("Response"),
        this, SLOT(onResponse(uint, QVariantMap)));
    Q_EMIT responded(code, results);
    deleteLater();
}

#ifdef AK_HAVE_PIPEWIRE
// SPIKE-1 defense — a MINIMAL libpipewire consumer.
//
// NotifyPointerMotionAbsolute may require the screencast stream to be actively
// *consumed* (a downstream pulling frames), not merely *started*, before KWin will
// honour absolute motion. This consumer connects to the stream node via the PipeWire
// remote fd and DRAINS frames: every buffer is dequeued and immediately requeued.
// We never decode a frame, never copy pixels, never let an fd/frame cross the JSON
// bus (INV: no frames/FDs cross the JSON boundary) — it exists purely to satisfy the
// compositor that the stream is live.
//
// SPIKE-1: this must be verified against a live KWin session. If absolute motion turns
// out NOT to need a consumer on KWin, this whole struct + its call sites can be deleted
// (see the call site in startRemoteDesktop's Start callback). The libpipewire API
// details below follow the standard minimal pw_stream consumer pattern and should be
// re-checked against the installed libpipewire-0.3 headers.
struct CoworkPortal::PwConsumer {
    pw_thread_loop *loop = nullptr;
    pw_context *context = nullptr;
    pw_core *core = nullptr;
    pw_stream *stream = nullptr;
    spa_hook streamListener{};

    // process: a frame is ready — dequeue it and immediately requeue it (drain + drop).
    static void onProcess(void *userdata)
    {
        auto *self = static_cast<PwConsumer *>(userdata);
        pw_buffer *b = nullptr;
        // Drain everything currently queued so we never build a backlog.
        while ((b = pw_stream_dequeue_buffer(self->stream)) != nullptr) {
            pw_stream_queue_buffer(self->stream, b);
        }
    }

    static const pw_stream_events &events()
    {
        static const pw_stream_events ev = [] {
            pw_stream_events e{};
            e.version = PW_VERSION_STREAM_EVENTS;
            e.process = &PwConsumer::onProcess;
            return e;
        }();
        return ev;
    }

    // Connect to nodeId on the remote reached through fd. Returns true on success.
    bool start(int fd, uint nodeId)
    {
        static bool inited = false;
        if (!inited) {
            pw_init(nullptr, nullptr);
            inited = true;
        }
        loop = pw_thread_loop_new("ak-cowork-pw", nullptr);
        if (!loop) {
            return false;
        }
        context = pw_context_new(pw_thread_loop_get_loop(loop), nullptr, 0);
        if (!context) {
            return false;
        }
        // Order matters: pw_thread_loop_start() must run WITHOUT the lock held — it waits
        // for the loop thread to reach its running state, and the loop thread cannot make
        // progress while we hold the lock. Locking first deadlocks start() forever (the
        // pthread is created but start() never returns, so the portal callback never
        // replies → "desktop portal timed out"). Start first, THEN lock for the API calls.
        if (pw_thread_loop_start(loop) < 0) {
            return false;
        }
        pw_thread_loop_lock(loop);
        // pw_context_connect_fd takes ownership of the fd, so hand it a dup.
        core = pw_context_connect_fd(context, fcntl_dup(fd), nullptr, 0);
        if (!core) {
            pw_thread_loop_unlock(loop);
            return false;
        }
        stream = pw_stream_new(core, "ak-cowork-drain", nullptr);
        if (!stream) {
            pw_thread_loop_unlock(loop);
            return false;
        }
        pw_stream_add_listener(stream, &streamListener, &events(), this);

        // Minimal video param: accept any raw video format/size — we drop frames, so we
        // do not care about the actual format, only that the stream connects.
        uint8_t buffer[1024];
        spa_pod_builder pb = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
        const spa_pod *params[1];
        params[0] = reinterpret_cast<const spa_pod *>(spa_pod_builder_add_object(
            &pb, SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
            SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
            SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw)));

        const int rc = pw_stream_connect(
            stream, PW_DIRECTION_INPUT, nodeId,
            static_cast<pw_stream_flags>(PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS),
            params, 1);
        pw_thread_loop_unlock(loop);
        return rc >= 0;
    }

    void stop()
    {
        if (loop) {
            pw_thread_loop_lock(loop);
        }
        if (stream) {
            pw_stream_destroy(stream);
            stream = nullptr;
        }
        if (core) {
            pw_core_disconnect(core);
            core = nullptr;
        }
        if (loop) {
            pw_thread_loop_unlock(loop);
            pw_thread_loop_stop(loop);
        }
        if (context) {
            pw_context_destroy(context);
            context = nullptr;
        }
        if (loop) {
            pw_thread_loop_destroy(loop);
            loop = nullptr;
        }
    }

    // dup wrapper kept local so the only <fcntl.h>/<unistd.h> usage lives here.
    static int fcntl_dup(int fd) { return ::dup(fd); }
};
#endif // AK_HAVE_PIPEWIRE

CoworkPortal::CoworkPortal(CoreClient *core, QWidget *topLevel, QObject *parent)
    : QObject(parent), m_core(core), m_topLevel(topLevel)
{
    connect(m_core, &CoreClient::notification, this, &CoworkPortal::onNotification);
    // No idle teardown: once the user approves the remote-control + screen-share portal
    // once, the session is kept alive for the whole app run so we never re-prompt during
    // an interaction (a remote-desktop session cannot persist across restarts anyway).
    // The kill-switch (and app exit) are the only teardown points.
    // Timed (profiled-motion) playback: one op per tick, never blocking the event loop.
    m_playTimer.setSingleShot(true);
    connect(&m_playTimer, &QTimer::timeout, this, &CoworkPortal::playbackTick);
    // Hand-shake watchdog: a portal that accepts a step and never answers it would
    // otherwise leave m_rdStarting true forever, and every later request queued behind a
    // session that can never come up.
    m_rdWatchdog.setSingleShot(true);
    connect(&m_rdWatchdog, &QTimer::timeout, this, [this] {
        if (!m_rdStarting || m_rdWatchdogGen != m_rdGeneration) {
            return; // the attempt this timer was armed for is over
        }
        qWarning("cowork: the desktop portal never answered the remote-control hand-shake");
        failPortalStep(i18n("the desktop portal accepted the remote-control request but "
                            "never answered it. Restart it with:  systemctl --user restart "
                            "xdg-desktop-portal.service"));
    });
    // Put the desktop back if a previous run died with the a11y flags flipped — but off
    // the constructor: this runs inside MainWindow's constructor, before the event loop,
    // and recovery talks to the a11y bus. Deferred to the first event-loop pass so a
    // wedged (or activating) org.a11y.Bus can never delay the window coming up.
    QTimer::singleShot(0, this, [this] { recoverStaleA11yStatus(); });
    // SECURITY (audit F3): the last line of defence for a timed injection. A script may
    // play for up to 30 s, and keystrokes follow FOCUS — so the moment any Agent Kate
    // window becomes the focus window mid-playback, the rest of the script is aborted.
    // focusWindowChanged fires with a non-null QWindow only for OUR windows, which makes
    // this both exact and free: no compositor round-trip, no window list, no race with the
    // core's activation watch (they are independent and either one is sufficient). It is
    // what makes the consent prompt un-typeable by the very playback that raised it.
    //
    // Scoped to a LIVE timed playback (m_playCorrId): a synchronous batch runs inside a
    // single event-loop callback, so no focus change can interleave with it, and a batch
    // merely queued behind an unfinished portal hand-shake has not injected anything yet.
    // Aborting those would turn "the human clicked back into Agent Kate" into a failure
    // for work that was never at risk.
    connect(qApp, &QGuiApplication::focusWindowChanged, this, [this](QWindow *w) {
        if (w != nullptr && !m_playCorrId.isEmpty()) {
            abortInjection(i18n("an Agent Kate window took focus while a timed script was "
                                "playing — a script may never type into Agent Kate's own "
                                "interface, including its consent prompts"));
        }
    });
    // aboutToQuit is the teardown path that actually runs: the window that owns us is
    // heap-allocated and never deleted, so ~CoworkPortal does not fire on a real exit.
    connect(qApp, &QCoreApplication::aboutToQuit, this, [this] { shutdownTeardown(); });
}

void CoworkPortal::shutdownTeardown()
{
    if (m_shutdownDone) {
        return;
    }
    m_shutdownDone = true;
    // Release the screencast/PipeWire resources and cancel any playback timer (closeSessionOnly
    // does all of this and is safe with no live session), then put the desktop back.
    closeSessionOnly();
    restoreAtspiStatus();
}

CoworkPortal::~CoworkPortal()
{
    // Normally a no-op: aboutToQuit has already run shutdownTeardown(). If we ARE destroyed
    // (a test, or a future owner that deletes us), everything it touches is either local or
    // reached through a QPointer — m_core is a sibling child of the same parent and may
    // already be gone.
    shutdownTeardown();
}

void CoworkPortal::enableAtspiStatusForLaunch()
{
    // Record the user's original state once, so we can put it back on teardown.
    if (!m_a11yStatusCaptured) {
        const KConfigGroup g = KSharedConfig::openConfig()->group(QLatin1String(kA11yGroup));
        const A11yRecordHolder holder = a11yRecordHolder(g);
        if (holder == A11yRecordHolder::Foreign || holder == A11yRecordHolder::Ours) {
            // Someone has already flipped the flags and parked the pre-flip values. Reading
            // the bus now would capture the FLIPPED state as "the user's originals" and the
            // restore would leave the desktop in screen-reader mode forever. Adopt the
            // parked originals instead, and leave the record where it is: rewriting it with
            // our pid would let our exit delete the owner's crash safety net.
            m_origIsEnabled = g.readEntry(QLatin1String(kA11yOrigIsEnabled), false);
            m_origScreenReader = g.readEntry(QLatin1String(kA11yOrigScreenReader), false);
            m_a11yStatusCaptured = true;
        } else {
            m_origIsEnabled = readA11yStatus(QStringLiteral("IsEnabled"), false);
            m_origScreenReader = readA11yStatus(QStringLiteral("ScreenReaderEnabled"), false);
            m_a11yStatusCaptured = true;
            // Park the originals on disk BEFORE the flip: everything between this line and
            // restoreAtspiStatus() is the window in which a crash would otherwise strand the
            // user's desktop in screen-reader mode with no record of what it used to be.
            persistA11yOriginals(m_origIsEnabled, m_origScreenReader);
        }
    }
    writeA11yStatus(QStringLiteral("IsEnabled"), true);
    writeA11yStatus(QStringLiteral("ScreenReaderEnabled"), true);
    verifyAtspiEnabled();
}

void CoworkPortal::enableAtspiForUserLaunch()
{
    // The human clicked "launch browser" in the Cowork panel. That IS the consent for
    // the flip — but it must still go through the parking machinery above, or the
    // desktop is left in accessibility mode with nothing recorded to restore from.
    enableAtspiStatusForLaunch();
}

void CoworkPortal::verifyAtspiEnabled()
{
    // The writes above are async fire-and-forget, so they succeed even with no a11y bus at
    // all. Read the flag back — the session bus delivers our Set before this Get, so a bus
    // that is actually there answers true — and report THAT, never the intent. A wedged bus
    // costs kA11yCallTimeoutMs, not the 25 s D-Bus default.
    m_a11yEnabled = readA11yStatus(QStringLiteral("IsEnabled"), false);
    if (!m_a11yEnabled) {
        qWarning("cowork: org.a11y.Status did not come up enabled — reading windows and "
                 "clicking named elements will not work");
    }
}

void CoworkPortal::restoreAtspiStatus()
{
    if (!m_a11yStatusCaptured) {
        return;
    }
    writeA11yStatus(QStringLiteral("ScreenReaderEnabled"), m_origScreenReader, /*blocking=*/true);
    writeA11yStatus(QStringLiteral("IsEnabled"), m_origIsEnabled, /*blocking=*/true);
    m_a11yStatusCaptured = false;
    m_a11yEnabled = false;
    // Only now: the record exists to survive a failure to reach this point.
    clearPersistedA11yOriginals();
}

void CoworkPortal::persistA11yOriginals(bool isEnabled, bool screenReader)
{
    KConfigGroup g = KSharedConfig::openConfig()->group(QLatin1String(kA11yGroup));
    g.writeEntry(QLatin1String(kA11yOrigIsEnabled), isEnabled);
    g.writeEntry(QLatin1String(kA11yOrigScreenReader), screenReader);
    g.writeEntry(QLatin1String(kA11yPid), qint64(QCoreApplication::applicationPid()));
    g.writeEntry(QLatin1String(kA11yPending), true);
    g.sync(); // must be on disk before the flip, not at the next config flush
}

void CoworkPortal::clearPersistedA11yOriginals()
{
    KConfigGroup g = KSharedConfig::openConfig()->group(QLatin1String(kA11yGroup));
    // Only the owner may delete the record. If a live foreign instance parked it (we merely
    // adopted its originals), deleting it here would strip the crash protection of a process
    // that is still holding the flags flipped.
    if (a11yRecordHolder(g) == A11yRecordHolder::Foreign) {
        return;
    }
    g.deleteGroup();
    g.sync();
}

void CoworkPortal::recoverStaleA11yStatus()
{
    if (s_a11yRecoveryDone) {
        return;
    }
    s_a11yRecoveryDone = true;
    // If THIS process already holds the flags flipped, the parked record is our own live
    // safety net: restoring from it would undo a flip a running session depends on, and
    // clearing it would strip the crash protection. Today the singleShot(0) deferral makes
    // that ordering impossible; nothing enforces it, so the invariant is stated here.
    if (m_a11yStatusCaptured) {
        return;
    }
    const KConfigGroup g = KSharedConfig::openConfig()->group(QLatin1String(kA11yGroup));
    // The record names the process that wrote it. If that process is still alive (or is not
    // identifiable, which we treat the same way), it is a second AgentKate instance that may
    // be holding the flags flipped for a LIVE Cowork session — restoring would break it, and
    // clearing the record would rob it of its own safety net. Leave both alone; its own
    // restore path will run.
    const A11yRecordHolder holder = a11yRecordHolder(g);
    if (holder == A11yRecordHolder::None) {
        return;
    }
    if (holder == A11yRecordHolder::Foreign) {
        qInfo("cowork: leaving the parked org.a11y.Status record alone — pid %lld still runs",
              g.readEntry(QLatin1String(kA11yPid), qint64(0)));
        return;
    }
    // Otherwise the writer is gone (or is us): the flags were flipped and never put back,
    // and no Cowork session exists yet in this process, so nothing relies on them.
    const bool origIsEnabled = g.readEntry(QLatin1String(kA11yOrigIsEnabled), false);
    const bool origScreenReader = g.readEntry(QLatin1String(kA11yOrigScreenReader), false);
    qInfo("cowork: restoring org.a11y.Status left flipped by a previous run "
          "(IsEnabled=%d ScreenReaderEnabled=%d)",
          int(origIsEnabled), int(origScreenReader));
    writeA11yStatus(QStringLiteral("ScreenReaderEnabled"), origScreenReader);
    writeA11yStatus(QStringLiteral("IsEnabled"), origIsEnabled);
    clearPersistedA11yOriginals();
}

void CoworkPortal::onNotification(const QString &method, const QJsonObject &params)
{
    // The kill-switch tears down any live RemoteDesktop input session immediately.
    if (method == QLatin1String("cowork.killSwitch")) {
        if (params.value(QStringLiteral("on")).toBool()) {
            teardownRemoteDesktop();
            // "Stop ALL desktop access" has to include the desktop-wide accessibility flags
            // Cowork flipped (audit F8) — tearing down only the RemoteDesktop session would
            // leave every application on this session exporting its AT-SPI tree to any local
            // process after the human hit the panic button. The core asks for this
            // explicitly (cowork/consent.go Kill → restoreDesktopFlags) so the two halves of
            // the contract are visible from both sides.
            if (params.value(QStringLiteral("restoreDesktopFlags")).toBool()) {
                restoreAtspiStatus();
            }
        }
        return;
    }
    // The last agent with desktop access was switched off. The enable dialog promises the
    // accessibility flip lasts only "until desktop access is turned off — then your
    // original setting is restored", and until this existed only the kill-switch and app
    // exit honoured that, so the promise was false for the ordinary way people stop
    // (audit F8). No live thread can be relying on the flip at this point: the core only
    // sends it when NO record still has Cowork enabled.
    if (method == QLatin1String("cowork.restoreDesktopFlags")) {
        restoreAtspiStatus();
        return;
    }
    if (method != QLatin1String("cowork.portalRequest")) {
        return;
    }
    const QString kind = params.value(QStringLiteral("kind")).toString();
    if (kind == QLatin1String("screenshot")) {
        handleScreenshot(params);
        return;
    }
    if (kind == QLatin1String("launchBrowser")) {
        handleLaunchBrowser(params);
        return;
    }
    if (kind == QLatin1String("inject")) {
        handleInject(params);
        return;
    }
    if (kind == QLatin1String("killInject")) {
        handleKillInject(params);
        return;
    }
    if (kind == QLatin1String("abortInject")) {
        // The core's activation watch saw focus leave the window the running script was
        // approved for. It has already refused the call on its side and is not waiting for
        // an answer here — this is the stop order, not a round-trip.
        const QString reason = params.value(QStringLiteral("reason")).toString();
        abortInjection(reason.isEmpty() ? i18n("the focused window changed while a timed script was playing")
                                        : reason);
        return;
    }
    if (kind == QLatin1String("preflight")) {
        handlePreflight(params);
        return;
    }
    // Anything else is fail-closed so the core's staggered timeout resolves with a
    // clear error rather than hanging.
    replyResult(params.value(QStringLiteral("corrId")).toString(), kind, false,
                QStringLiteral("portal op '%1' is not supported in this version").arg(kind));
}

void CoworkPortal::handleScreenshot(const QJsonObject &req)
{
    // Prefer KWin's native ScreenShot2: it is fast, raises no dialog, and targets the
    // exact window/screen/region. interactive=true keeps the XDG portal's native picker;
    // every other case tries ScreenShot2 first and falls back to the portal if it is not
    // authorized/available (e.g. an un-installed build-dir binary, or — as on this
    // system — a frontend xdg-desktop-portal that doesn't expose the Screenshot portal).
    if (!req.value(QStringLiteral("interactive")).toBool() && startKWinScreenshot(req)) {
        return;
    }
    startPortalScreenshot(req);
}

bool CoworkPortal::startKWinScreenshot(const QJsonObject &req)
{
    QDBusConnection bus = QDBusConnection::sessionBus();
    if (!bus.isConnected()) {
        return false;
    }
    QDBusConnectionInterface *iface = bus.interface();
    if (!iface || !iface->isServiceRegistered(QString::fromLatin1(kKWinService))) {
        return false; // not a KWin session → let the portal path handle it
    }

    QString method;
    QVariantList leading;
    kwinCaptureCall(req, method, leading);
    qInfo("cowork: trying KWin ScreenShot2 %s", qUtf8Printable(method));

    int fds[2];
    if (::pipe2(fds, O_CLOEXEC) != 0) {
        return false;
    }
    // Only the READ end is non-blocking (we drain it from the Qt event loop). The write
    // end stays blocking so KWin's synchronous image write into the pipe always succeeds.
    ::fcntl(fds[0], F_SETFL, ::fcntl(fds[0], F_GETFL) | O_NONBLOCK);

    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kKWinService), QString::fromLatin1(kScreenShot2Path),
        QString::fromLatin1(kScreenShot2Iface), method);
    QVariantList args = leading;
    args << QVariant::fromValue(QVariantMap());                   // options: defaults
    args << QVariant::fromValue(QDBusUnixFileDescriptor(fds[1])); // dups fds[1] internally
    msg.setArguments(args);
    QDBusPendingCall pending = bus.asyncCall(msg);
    // The QDBusUnixFileDescriptor holds its own dup and asyncCall keeps the message alive
    // until sent, so close OUR write end now: the read end only sees EOF once EVERY write
    // end (ours + KWin's) is closed, and KWin closes its copy when the capture finishes.
    ::close(fds[1]);

    const QString corrId = req.value(QStringLiteral("corrId")).toString();
    const int maxDim = req.value(QStringLiteral("maxDim")).toInt(1568);
    const QString format = req.value(QStringLiteral("format")).toString(QStringLiteral("png"));

    auto ctx = std::make_shared<KWinShot>();
    ctx->readFd = fds[0];
    ctx->notifier = new QSocketNotifier(fds[0], QSocketNotifier::Read, this);
    ctx->watcher = new QDBusPendingCallWatcher(pending, this);

    // Assemble once the geometry reply has landed AND we have the pixels for it. The reply
    // carries {width,height,stride,format}; without it we cannot interpret the bytes.
    auto finalize = [this, ctx, req, corrId, maxDim, format]() {
        if (ctx->done || !ctx->gotResults) {
            return;
        }
        const uint w = ctx->results.value(QStringLiteral("width")).toUInt();
        const uint h = ctx->results.value(QStringLiteral("height")).toUInt();
        const uint stride = ctx->results.value(QStringLiteral("stride")).toUInt();
        const uint fmt = ctx->results.value(QStringLiteral("format")).toUInt();
        const bool badGeom = w == 0 || h == 0 || fmt < 1 || fmt > 30;
        const qint64 need = qint64(stride) * qint64(h);
        // On a SUCCESSFUL capture we still need the pixels: keep waiting until either the
        // pipe hits EOF or we have already buffered a full frame. We must NOT wait on EOF
        // when the reply errored (or the geometry is bogus): an unauthorized/denied call can
        // leave KWin holding its dup of the pipe write-end, so EOF never comes — blocking on
        // it would hang the whole portal round-trip until akcore times out (125 s).
        if (!ctx->error && !badGeom && !ctx->gotEof && qint64(ctx->buf.size()) < need) {
            return; // still streaming a valid capture
        }
        ctx->done = true;
        ctx->notifier->setEnabled(false);
        ctx->notifier->deleteLater();
        ctx->watcher->deleteLater();
        if (ctx->readFd >= 0) {
            ::close(ctx->readFd);
            ctx->readFd = -1;
        }
        // Denied (unauthorized exe), malformed, or truncated → fall back to the portal.
        if (ctx->error || badGeom || qint64(ctx->buf.size()) < need) {
            qWarning("cowork: KWin ScreenShot2 unusable (error=%d w=%u h=%u stride=%u fmt=%u "
                     "bytes=%lld need=%lld) → portal fallback",
                     int(ctx->error), w, h, stride, fmt, qint64(ctx->buf.size()), need);
            startPortalScreenshot(req);
            return;
        }
        qInfo("cowork: KWin ScreenShot2 captured %ux%u (fmt=%u, %lld bytes)", w, h, fmt,
              qint64(ctx->buf.size()));
        QImage img(reinterpret_cast<const uchar *>(ctx->buf.constData()), int(w), int(h),
                   int(stride), QImage::Format(fmt));
        // copy() detaches into QImage-owned memory before ctx->buf is freed.
        replyWithImage(corrId, maxDim, format, img.copy());
    };

    connect(ctx->notifier, &QSocketNotifier::activated, this, [ctx, finalize]() {
        char tmp[1 << 16];
        for (;;) {
            const ssize_t n = ::read(ctx->readFd, tmp, sizeof(tmp));
            if (n > 0) {
                ctx->buf.append(tmp, int(n));
                if (ctx->buf.size() > (256 << 20)) { // runaway-producer guard (~256 MiB)
                    ctx->error = true;
                    ctx->gotEof = true;
                    ctx->notifier->setEnabled(false);
                    break;
                }
                continue;
            }
            if (n == 0) { // EOF: KWin closed its write end → capture streamed in full
                ctx->gotEof = true;
                ctx->notifier->setEnabled(false);
                break;
            }
            if (errno == EINTR) {
                continue;
            }
            if (errno == EAGAIN || errno == EWOULDBLOCK) {
                break; // drained for now; wait for the next activation
            }
            ctx->error = true;
            ctx->gotEof = true;
            ctx->notifier->setEnabled(false);
            break;
        }
        finalize();
    });

    connect(ctx->watcher, &QDBusPendingCallWatcher::finished, this,
            [ctx, finalize](QDBusPendingCallWatcher *w) {
                QDBusPendingReply<QVariantMap> reply = *w;
                if (reply.isError()) {
                    qWarning("cowork: KWin ScreenShot2 reply error: %s: %s",
                             qUtf8Printable(reply.error().name()),
                             qUtf8Printable(reply.error().message()));
                    ctx->error = true;
                } else {
                    ctx->results = reply.value();
                }
                ctx->gotResults = true;
                finalize();
            });

    // Lifetime backstop (audit F24). Neither signal above is guaranteed to arrive in
    // the one case that matters: KWin answers the call SUCCESSFULLY and then stalls
    // without ever closing its end of the pipe. finalize() then keeps returning early
    // ("still streaming a valid capture") and this capture never ends — the read fd,
    // the notifier and the watcher live until the process does, and the agent's
    // request only resolves when akcore gives up 125 s later, with no fallback tried.
    // PortalResponseWaiter has carried the same guard from the start
    // (kPortalWaiterLifetimeMs); this path was the one without it.
    //
    // Marked as an error so finalize stops waiting for pixels and takes the portal
    // fallback, which is the right answer for a KWin that is not going to deliver.
    // `this` is the context object, so a destroyed portal simply drops the timer.
    QTimer::singleShot(kKWinShotLifetimeMs, this, [ctx, finalize] {
        if (ctx->done) {
            return; // finished normally; nothing to force
        }
        qWarning("cowork: KWin ScreenShot2 did not complete within %d ms → portal fallback",
                 kKWinShotLifetimeMs);
        ctx->error = true;
        ctx->gotResults = true;
        ctx->gotEof = true;
        finalize();
    });

    return true;
}

void CoworkPortal::startPortalScreenshot(const QJsonObject &req)
{
    const QString corrId = req.value(QStringLiteral("corrId")).toString();
    int maxDim = req.value(QStringLiteral("maxDim")).toInt(1568);
    const QString format = req.value(QStringLiteral("format")).toString(QStringLiteral("png"));

    QDBusConnection bus = QDBusConnection::sessionBus();
    if (!bus.isConnected()) {
        replyResult(corrId, QStringLiteral("screenshot"), false, QStringLiteral("no session bus"));
        return;
    }

    // Predict the Request object path so we can subscribe BEFORE the call (avoids a
    // race where Response fires before we connect). Path =
    // /…/request/<sender-with-dots-as-underscores>/<handle_token>.
    const QString token = QStringLiteral("ak%1").arg(QRandomGenerator::global()->generate());
    const QString requestPath = portalHandlePath(QStringLiteral("request"), token);

    auto *waiter = new PortalResponseWaiter(requestPath, kPortalWaiterLifetimeMs, this);
    connect(waiter, &PortalResponseWaiter::responded, this,
            [this, corrId, maxDim, format](uint code, const QVariantMap &results) {
                finishScreenshot(corrId, maxDim, format, code, results);
            });

    QVariantMap opts;
    opts.insert(QStringLiteral("handle_token"), token);
    // interactive=true lets the user pick a specific window/region in KDE's native
    // picker (a "share this window" flow); false captures the screen directly.
    opts.insert(QStringLiteral("interactive"), req.value(QStringLiteral("interactive")).toBool());

    // asyncCall: the Screenshot portal is the same service that can be wedged or still
    // activating, and a blocking call here would freeze the GUI thread for the D-Bus
    // timeout. (A QDBusInterface would also introspect synchronously on construction.)
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.Screenshot"), QStringLiteral("Screenshot"));
    msg.setArguments({parentWindowHandle(), opts});
    auto *watcher = new QDBusPendingCallWatcher(bus.asyncCall(msg), this);
    connect(watcher, &QDBusPendingCallWatcher::finished, this,
            [this, corrId, waiter = QPointer<PortalResponseWaiter>(waiter)](
                QDBusPendingCallWatcher *w) {
                w->deleteLater();
                QDBusPendingReply<QDBusObjectPath> reply = *w;
                if (!reply.isError()) {
                    return; // the waiter delivers the result asynchronously
                }
                if (waiter) {
                    waiter->deleteLater();
                }
                replyResult(corrId, QStringLiteral("screenshot"), false,
                            QStringLiteral("portal call failed: %1").arg(reply.error().message()));
            });
}

void CoworkPortal::finishScreenshot(const QString &corrId, int maxDim, const QString &format,
                                    uint code, const QVariantMap &results)
{
    if (code != 0) {
        replyResult(corrId, QStringLiteral("screenshot"), false,
                    code == 1 ? QStringLiteral("cancelled by the user")
                              : QStringLiteral("screenshot failed (portal code %1)").arg(code));
        return;
    }
    const QString uri = results.value(QStringLiteral("uri")).toString();
    const QString path = QUrl(uri).toLocalFile();
    QImage img(path);
    // The portal saved a full-resolution PNG to disk; it may contain secrets, so we do
    // not keep it once the pixels are decoded into memory (img is independent of the file).
    if (!path.isEmpty()) {
        QFile::remove(path);
    }
    if (img.isNull()) {
        replyResult(corrId, QStringLiteral("screenshot"), false,
                    QStringLiteral("could not read the captured image"));
        return;
    }
    replyWithImage(corrId, maxDim, format, img);
}

void CoworkPortal::replyWithImage(const QString &corrId, int maxDim, const QString &format,
                                  const QImage &src)
{
    QImage img = src;
    if (maxDim > 0 && (img.width() > maxDim || img.height() > maxDim)) {
        img = img.scaled(maxDim, maxDim, Qt::KeepAspectRatio, Qt::SmoothTransformation);
    }

    const bool jpeg = (format == QLatin1String("jpeg"));
    QByteArray bytes;
    QBuffer buf(&bytes);
    buf.open(QIODevice::WriteOnly);
    img.save(&buf, jpeg ? "JPEG" : "PNG", jpeg ? 85 : -1);
    buf.close();

    QJsonObject extra{
        {QStringLiteral("pngB64"), QString::fromLatin1(bytes.toBase64())},
        {QStringLiteral("mime"), jpeg ? QStringLiteral("image/jpeg") : QStringLiteral("image/png")},
        {QStringLiteral("width"), img.width()},
        {QStringLiteral("height"), img.height()},
    };
    replyResult(corrId, QStringLiteral("screenshot"), true, QString(), extra);
}

void CoworkPortal::handleLaunchBrowser(const QJsonObject &req)
{
    const QString corrId = req.value(QStringLiteral("corrId")).toString();
    const QString name = req.value(QStringLiteral("name")).toString();

    const QStringList allNames = BrowserLaunch::names();
    QJsonArray namesArr;
    for (const QString &n : allNames) {
        namesArr.append(n);
    }
    QJsonObject extra{{QStringLiteral("browsers"), namesArr}};

    // Resolve against the user's configured browsers only — the agent can name one but
    // never supply an arbitrary command. An empty name means "the user's default".
    const BrowserLaunch::Browser b =
        name.isEmpty() ? BrowserLaunch::preferred() : BrowserLaunch::find(name);
    if (b.command.isEmpty()) {
        QString err;
        if (allNames.isEmpty()) {
            err = i18n("no browser is configured — open one from the Cowork panel first");
        } else if (name.isEmpty()) {
            err = i18n("no default browser is set for agents");
        } else {
            err = i18n("no configured browser named “%1” (available: %2)", name,
                       allNames.join(QStringLiteral(", ")));
        }
        replyResult(corrId, QStringLiteral("launchBrowser"), false, err, extra);
        return;
    }

    // Chromium browsers only export their a11y tree if accessibility is enabled at
    // launch — announce ourselves as an AT first (Firefox doesn't need this).
    //
    // SECURITY (audit F8): this is an AGENT-triggered action, so it may only RE-ASSERT a
    // flip the human already consented to (m_a11yStatusCaptured means we own the parked
    // originals, i.e. the portal grant landed and becomeReady flipped them). It must never
    // be the thing that first switches the whole desktop into accessibility mode — that
    // would let an agent make a global permission change out of a declined preflight, or
    // after the kill-switch put the flags back.
    if (b.family == QLatin1String("chromium")) {
        if (m_a11yStatusCaptured) {
            enableAtspiStatusForLaunch();
        } else {
            qWarning("cowork: launching %s without switching accessibility on — desktop "
                     "access was never granted (or was stopped), so its page contents will "
                     "not be readable", qPrintable(b.name));
        }
    }

    QString launchErr;
    if (!BrowserLaunch::launch(b, &launchErr)) {
        replyResult(corrId, QStringLiteral("launchBrowser"), false, launchErr, extra);
        return;
    }
    extra.insert(QStringLiteral("browser"), b.name);
    replyResult(corrId, QStringLiteral("launchBrowser"), true, QString(), extra);
}

void CoworkPortal::replyResult(const QString &corrId, const QString &kind, bool ok,
                               const QString &error, const QJsonObject &extra)
{
    if (!m_core) {
        return; // destroyed sibling — nothing to reply to (teardown ordering)
    }
    QJsonObject params{
        {QStringLiteral("corrId"), corrId},
        {QStringLiteral("kind"), kind},
        {QStringLiteral("ok"), ok},
    };
    if (!error.isEmpty()) {
        params.insert(QStringLiteral("error"), error);
    }
    for (auto it = extra.begin(); it != extra.end(); ++it) {
        params.insert(it.key(), it.value());
    }
    m_core->call(QStringLiteral("cowork.portalResult"), params, nullptr, this);
}

QString CoworkPortal::parentWindowHandle() const
{
    // v1: pass an empty parent. xdg-desktop-portal-kde anchors the dialog loosely
    // when the handle is empty; proper Wayland xdg-foreign export (SPIKE-XDGEXPORT)
    // is a v2 refinement. On X11 a handle could be x11:<hex winId>, but empty works
    // there too for the screenshot portal.
    return QString();
}

// --- RemoteDesktop input injection ----------------------------------------------

void CoworkPortal::portalRequest(const QString &iface, const QString &method,
                                 const QVariantList &args, QVariantMap options,
                                 std::function<void(uint, const QVariantMap &)> cb)
{
    QDBusConnection bus = QDBusConnection::sessionBus();
    const QString token = QStringLiteral("ak%1").arg(QRandomGenerator::global()->generate());
    options.insert(QStringLiteral("handle_token"), token);

    const QString reqPath = portalHandlePath(QStringLiteral("request"), token);

    auto *waiter = new PortalResponseWaiter(reqPath, kPortalWaiterLifetimeMs, this);
    connect(waiter, &PortalResponseWaiter::responded, this,
            [cb](uint code, const QVariantMap &results) { cb(code, results); });
    // Every portalRequest belongs to the current session attempt (this is the RemoteDesktop
    // / ScreenCast hand-shake path). Track it so the attempt ending frees it — the generation
    // guards make a late continuation a no-op but cannot free what is waiting to run it.
    // Prune first: waiters that already answered have destroyed themselves.
    m_rdWaiters.removeIf([](const QPointer<PortalResponseWaiter> &w) { return w.isNull(); });
    m_rdWaiters.append(QPointer<PortalResponseWaiter>(waiter));

    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath), iface, method);
    QVariantList full = args;
    full.append(options);
    msg.setArguments(full);

    // asyncCall, never call(): a wedged or still-activating xdg-desktop-portal would
    // otherwise freeze the GUI thread — and with it the core IPC pump — for the 25 s
    // D-Bus timeout, on a path plan 18's auto-preflight reaches on ordinary session
    // starts. The watcher is parented to `this`, so a destroyed portal takes its
    // continuations with it (the async-callback crash class).
    //
    // Ordering is what makes this a drop-in: the portal sends the method reply BEFORE it
    // emits Response on the Request path, both arrive on this connection from the same
    // service, and Qt dispatches them in arrival order — so m_lastPortalError is settled
    // before any Response-driven cb can read it through portalFailureDetail().
    auto *watcher = new QDBusPendingCallWatcher(bus.asyncCall(msg), this);
    connect(watcher, &QDBusPendingCallWatcher::finished, this,
            [this, iface, method, cb, waiter = QPointer<PortalResponseWaiter>(waiter)](
                QDBusPendingCallWatcher *w) {
                w->deleteLater();
                const QDBusMessage reply = w->reply();
                if (reply.type() != QDBusMessage::ErrorMessage) {
                    m_lastPortalError.clear();
                    m_lastPortalErrorName.clear();
                    return;
                }
                // Log the concrete D-Bus error: a method-level rejection (e.g. an invalid
                // option) never reaches the Response signal, so without this it reads as a
                // silent "declined" with no clue why.
                qWarning("cowork: portal %s.%s failed: %s", qUtf8Printable(iface),
                         qUtf8Printable(method), qUtf8Printable(reply.errorMessage()));
                // Keep the reason for the failure path to quote. A generic "the session
                // could not be created" sends the user looking for a permission they never
                // denied. The message can be empty (some errors carry only a name), which
                // would leave the failure unexplained — fall back to the name.
                m_lastPortalErrorName = reply.errorName();
                m_lastPortalError =
                    reply.errorMessage().isEmpty() ? m_lastPortalErrorName : reply.errorMessage();
                if (waiter) {
                    waiter->deleteLater();
                }
                cb(2, QVariantMap()); // no Response will arrive; surface the failure now
            });
}

QString CoworkPortal::portalFailureDetail() const
{
    if (m_lastPortalError.isEmpty()) {
        return QString();
    }
    // The one failure that is NOT a decline and IS fixable: xdg-desktop-portal running
    // without the backend that owns RemoteDesktop/ScreenCast. It comes up this way often
    // enough (a startup race at login) that the message should carry the fix rather than
    // read like a refused permission the user has to hunt for. Matched on the error NAME:
    // the message is free-form and localisable, while these four names are exactly how the
    // bus reports "that interface/method/object/service is not there".
    static const QStringList missingBackend{
        QStringLiteral("org.freedesktop.DBus.Error.UnknownMethod"),
        QStringLiteral("org.freedesktop.DBus.Error.UnknownInterface"),
        QStringLiteral("org.freedesktop.DBus.Error.UnknownObject"),
        QStringLiteral("org.freedesktop.DBus.Error.ServiceUnknown"),
    };
    if (missingBackend.contains(m_lastPortalErrorName)) {
        return i18n(" — the desktop portal is running without its remote-control backend. "
                    "Restart it with:  systemctl --user restart xdg-desktop-portal.service");
    }
    return QStringLiteral(" — %1").arg(m_lastPortalError);
}

uint CoworkPortal::deviceTypesFor(const QJsonArray &ops)
{
    uint types = 0;
    for (const QJsonValue &ov : ops) {
        const QString t = ov.toObject().value(QStringLiteral("t")).toString();
        if (t == QLatin1String("key")) {
            types |= 1u; // keyboard
        } else if (t == QLatin1String("btn") || t == QLatin1String("move")
                   || t == QLatin1String("move_rel")
                   || t == QLatin1String("axis") || t == QLatin1String("axis_discrete")) {
            types |= 2u; // pointer
        }
    }
    return types ? types : 1u; // default to keyboard-only — never a bare virtual pointer
}

bool CoworkPortal::needsScreencastFor(const QJsonArray &ops)
{
    // Only absolute motion needs a screencast stream bound to the session; scroll and
    // relative button events do not. Lazy on purpose — keyboard/button/scroll paths must
    // never spin up frame capture.
    for (const QJsonValue &ov : ops) {
        if (ov.toObject().value(QStringLiteral("t")).toString() == QLatin1String("move")) {
            return true;
        }
    }
    return false;
}

void CoworkPortal::handleInject(const QJsonObject &req)
{
    const QString corrId = req.value(QStringLiteral("corrId")).toString();
    const QJsonArray ops = req.value(QStringLiteral("ops")).toArray();

    if (m_rdReady && !m_rdSession.isEmpty()) {
        // A timed playback in flight owns the session: queue this batch behind it so ops do
        // not interleave and the active playback's corrId is never clobbered; the queue is
        // re-flushed when playback drains (playbackTick → flushInjectQueue). This guard sits
        // AHEAD of the device check, because the widening path below tears the session down
        // — which would kill the live playback and answer it "desktop control was stopped".
        // flushInjectQueue re-checks the device set, so a queued batch that needs a wider
        // one still triggers the rebuild, just after the playback has finished.
        if (!m_playCorrId.isEmpty()) {
            m_injectQueue.append({corrId, ops});
            return;
        }
        const uint needed = deviceTypesFor(ops);
        const bool needSc = needsScreencastFor(ops);
        if ((needed & ~m_rdTypes) == 0 && (!needSc || m_scReady)) {
            // The live session already owns every device these ops use AND has the
            // screencast stream a move needs (if any).
            runInjectOps(corrId, ops);
            // runInjectOps replies itself when it starts a non-blocking timed playback;
            // otherwise (synchronous fast path) it returns having done nothing async, so
            // we reply here. It signals that by leaving m_playCorrId == corrId.
            if (m_playCorrId != corrId) {
                // m_batchError non-empty = the batch was abandoned mid-way; report the
                // failure rather than an "ok" the core would commit its mirror on.
                replyResult(corrId, QStringLiteral("inject"), m_batchError.isEmpty(),
                            m_batchError, injectOutcome());
            }
            return;
        }
        // The batch needs a device — or a screencast stream — the session was not started
        // with (e.g. a click or a move arriving on a keyboard-only session). Drop the
        // session, keeping the queue intact, and rebuild it below with the wider set.
        closeSessionOnly();
    }
    m_injectQueue.append({corrId, ops});
    if (!m_rdStarting) {
        startRemoteDesktop();
    }
}

void CoworkPortal::handlePreflight(const QJsonObject &req)
{
    const QString corrId = req.value(QStringLiteral("corrId")).toString();

    // NOTE (audit F8): the desktop-wide org.a11y.Status flip used to happen HERE, before
    // the portal's remote-control dialog was even raised — so declining it still left the
    // whole session in accessibility mode until app exit, a real global permission change
    // the human never agreed to. The flip now happens in becomeReady(), i.e. only once the
    // portal grant has actually landed, and the consent text discloses it. Everything the
    // flip is needed for (reading windows, clicking named elements, and Chromium-family
    // browsers, which only export their tree when accessibility was on AT THEIR LAUNCH)
    // still gets it at enable time, because enabling Cowork runs this preflight.

    if (m_rdReady && !m_rdSession.isEmpty()) {
        // Already approved earlier in this run; the grant is reused as-is.
        replyResult(corrId, QStringLiteral("preflight"), true, QString(),
                    {{QStringLiteral("remoteDesktop"), true},
                     {QStringLiteral("accessibility"), m_a11yEnabled}});
        return;
    }
    m_preflightCorrIds.append(corrId);
    if (!m_rdStarting) {
        startRemoteDesktop();
    }
}

void CoworkPortal::startRemoteDesktop()
{
    m_rdStarting = true;
    // A new attempt: every continuation armed by an earlier one is now stale. Anything still
    // waiting for the previous attempt belongs to nobody now — abandon it before bumping, or
    // the generation guard silently keeps it alive. (Callers only reach here with no
    // hand-shake in flight, so this is normally empty; it is the backstop for any path that
    // is not.) abandonPortalWaiters never emits, so nothing can observe the half-bumped state
    // between it and the increment below.
    abandonPortalWaiters();
    const int gen = ++m_rdGeneration;
    // Arm the hand-shake watchdog for the NON-interactive steps (CreateSession /
    // SelectDevices / SelectSources): these raise no dialog, so anything past a few seconds
    // is the portal frontend having dropped the interface rather than a human deciding.
    // doStart() re-arms it with the far longer dialog budget.
    m_rdWatchdogGen = gen;
    m_rdWatchdog.start(kRdHandshakeTimeoutMs);
    const QString sessToken = QStringLiteral("aks%1").arg(QRandomGenerator::global()->generate());

    // Stand up the FULL session up-front — keyboard | pointer + screencast — so the user
    // approves the portal's remote-control + screen-share dialog ONCE and every later
    // action (type, click, move, scroll, drag) reuses this session with no further
    // prompts, for the whole app run. This deliberately trades the old lazy/minimal set
    // (which re-prompted on every escalation: keyboard→+pointer→+screencast) for a single
    // up-front grant. A positioned pointer bound to a screencast stream is the safe,
    // intended usage (plan 09); the cursor-freeze risk was a *bare* unpositioned pointer,
    // which this path never creates. deviceTypesFor/needsScreencastFor remain for the
    // reuse check in handleInject.
    const uint startTypes = 3u;        // keyboard | pointer
    const bool wantScreencast = true;  // absolute motion needs a bound screencast stream

    QVariantMap createOpts;
    createOpts.insert(QStringLiteral("session_handle_token"), sessToken);
    // We named the session, so we know the path it will appear at before the portal tells
    // us — the escape hatch for closing it if this attempt is abandoned and CreateSession's
    // reply never lands (see abandonPortalWaiters()).
    m_rdPendingSessionPath = portalHandlePath(QStringLiteral("session"), sessToken);

    portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("CreateSession"),
                  {}, createOpts, [this, gen, startTypes, wantScreencast](uint code, const QVariantMap &results) {
        if (gen != m_rdGeneration) {
            // This attempt was abandoned while CreateSession was in flight. The portal may
            // still have created a session for it — close it here or it is orphaned, since
            // nothing else knows its path. Reachable precisely BECAUSE abandonPortalWaiters
            // cancels waiters instead of destroying them; do not "simplify" that back.
            // m_rdPendingSessionPath is NOT touched: it was cleared when this attempt was
            // abandoned, and any value in it now belongs to the attempt that replaced us.
            closePortalSessionPath(results.value(QStringLiteral("session_handle")).toString());
            return;
        }
        // The reply for the LIVE attempt landed: the predicted path has served its purpose,
        // and from here the session's fate is m_rdSession's (closeSessionOnly closes it).
        // Leaving it set would let a later abandonment close a session twice — harmless on
        // the bus, but it would also outlive the attempt it describes.
        m_rdPendingSessionPath.clear();
        if (code != 0) {
            failPortalStep(i18n("the remote-control session could not be created"));
            return;
        }
        m_rdSession = results.value(QStringLiteral("session_handle")).toString();
        if (m_rdSession.isEmpty()) {
            failInjectQueue(i18n("no remote-control session handle was returned"));
            return;
        }
        // SelectDevices bitmask: keyboard (1) | pointer (2).
        QVariantMap selOpts;
        selOpts.insert(QStringLiteral("types"), startTypes);
        portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("SelectDevices"),
                      {QVariant::fromValue(QDBusObjectPath(m_rdSession))}, selOpts,
                      [this, gen, startTypes, wantScreencast](uint code2, const QVariantMap &) {
            if (gen != m_rdGeneration) {
                return; // abandoned attempt; its session was closed by whoever bumped gen
            }
            if (code2 != 0) {
                failPortalStep(i18n("input devices were not granted"),
                               i18n("input devices could not be requested"));
                return;
            }
            // The Start step is shared by both paths; capture it so SelectSources can
            // chain into it (screencast path) or we can call it directly (input-only).
            auto doStart = [this, gen, startTypes, wantScreencast]() {
                // Start is the step that raises the portal's approval dialog, so the
                // watchdog now has to outlast a human reading it — but still bound the
                // wedge case, or m_rdStarting is stuck true for the rest of the run.
                m_rdWatchdogGen = gen;
                m_rdWatchdog.start(kRdDialogTimeoutMs);
                portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("Start"),
                              {QVariant::fromValue(QDBusObjectPath(m_rdSession)), parentWindowHandle()},
                              QVariantMap(),
                              [this, gen, startTypes, wantScreencast](uint code3, const QVariantMap &startResults) {
                    // The attempt may have been torn down — and a NEW one started — while
                    // the user sat on the portal dialog (kill-switch / failure). Bailing on
                    // generation rather than on "no session" is what keeps this reply from
                    // marking a different attempt's session ready, or failing a queue that
                    // the new attempt now owns.
                    if (gen != m_rdGeneration) {
                        return;
                    }
                    if (code3 != 0) {
                        m_rdStarting = false;
                        failPortalStep(i18n("remote control was declined"),
                                       i18n("remote control could not be started"));
                        return;
                    }
                    // Readiness is flipped in ONE place, at the very end of the hand-shake.
                    // handleInject's fast path gates only on m_rdReady/m_rdTypes/m_scReady,
                    // so flipping them while the OpenPipeWireRemote round trip is still in
                    // flight would advertise a session whose screencast side is half-built
                    // (streams mapped, no PipeWire consumer) and let a move op run against
                    // it. m_rdStarting also stays true until here, so a request arriving
                    // meanwhile queues instead of starting a second session.
                    auto becomeReady = [this, startTypes](bool screencastUsable) {
                        m_rdWatchdog.stop(); // the hand-shake landed
                        m_rdStarting = false;
                        m_rdReady = true;
                        m_rdTypes = startTypes;
                        m_scReady = screencastUsable;
                        // Only NOW switch the session's accessibility service on (audit F8):
                        // the human has answered the portal's remote-control dialog and said
                        // yes. Declining takes the code3 != 0 branch above and never reaches
                        // here, so a refusal leaves the desktop exactly as it was. It has to
                        // precede flushInjectQueue, which reports m_a11yEnabled back to the
                        // preflight caller.
                        enableAtspiStatusForLaunch();
                        flushInjectQueue(); // the single drain of everything queued behind us
                    };
                    if (!wantScreencast) {
                        becomeReady(false);
                        return;
                    }
                    // Parse the captured monitor streams (a(ua{sv})) — our coordinate
                    // map. No restore_token to stash: remote-desktop sessions can't
                    // persist (see SelectSources above), so each session is fresh.
                    parseStreams(startResults.value(QStringLiteral("streams")));
                    if (m_streams.isEmpty()) {
                        becomeReady(false); // no coordinate map → absolute motion stays off
                        return;
                    }
                    // Open the PipeWire remote and (SPIKE-1) keep one stream consumed so
                    // KWin honours absolute motion. Failing to get the fd is TOLERATED:
                    // the consumer is an unverified defence, the coordinate map is already
                    // built, and refusing the whole session over it would cost the user
                    // keyboard and click too. So we come up screencast-ready either way —
                    // but only once this round trip has landed.
                    openPipeWireRemote([this, gen, becomeReady](int fd) {
                        if (gen != m_rdGeneration || m_streams.isEmpty()) {
                            // Torn down — or already rebuilt as a different session — while
                            // the fd was in flight. The fd belongs to a session that is no
                            // longer the live one, and whoever replaced it owns the queue.
                            if (fd >= 0) {
                                ::close(fd);
                            }
                            return;
                        }
                        if (m_pwFd >= 0) {
                            ::close(m_pwFd); // never overwrite a live fd
                            m_pwFd = -1;
                        }
                        m_pwFd = fd;
#ifdef AK_HAVE_PIPEWIRE
                        if (m_pwFd >= 0) {
                            startPwConsumer(m_streams.first().nodeId);
                        }
#endif
                        if (m_pwFd < 0) {
                            qWarning("cowork: no PipeWire remote fd; absolute motion runs "
                                     "without a stream consumer");
                        }
                        becomeReady(true);
                    });
                });
            };

            if (!wantScreencast) {
                doStart();
                return;
            }
            // Screencast.SelectSources on the SAME session before Start, so the granted
            // streams arrive in Start's results bound to this RemoteDesktop session.
            QVariantMap scOpts;
            scOpts.insert(QStringLiteral("types"), 1u);            // MONITOR
            scOpts.insert(QStringLiteral("multiple"), true);       // all outputs → multi-monitor
            // cursor_mode 4 = METADATA: we never render frames, so we only want cursor
            // metadata, not it composited in. SPIKE-3: if the hardware cursor stops
            // following absolute motion, fall back to EMBEDDED (2).
            scOpts.insert(QStringLiteral("cursor_mode"), 4u);
            // Do NOT request persist_mode/restore_token: xdg-desktop-portal-kde rejects
            // SelectSources with InvalidArgument "Remote desktop sessions cannot persist"
            // — a combined RemoteDesktop+ScreenCast session is inherently non-persistent.
            // (Verified live: with persist_mode the call errors before any dialog, which
            // read as a silent decline.) We re-prompt per new session; once approved the
            // session is kept alive for the whole app run (there is no idle timer — see the
            // header), so a burst of pointer ops, or a long choreography that waits between
            // events (plan 10), reuses the approved session without re-prompting or being
            // reaped mid-script.
            portalRequest(QStringLiteral("org.freedesktop.portal.ScreenCast"), QStringLiteral("SelectSources"),
                          {QVariant::fromValue(QDBusObjectPath(m_rdSession))}, scOpts,
                          [this, gen, doStart](uint codeSc, const QVariantMap &) {
                if (gen != m_rdGeneration) {
                    return; // abandoned attempt; its session was closed by whoever bumped gen
                }
                if (codeSc != 0) {
                    failPortalStep(i18n("screen capture for cursor control was declined"),
                                   i18n("screen capture for cursor control could not be started"));
                    return;
                }
                doStart();
            });
        });
    });
}

void CoworkPortal::notifyKeysym(int keysym, uint state)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyKeyboardKeysym"));
    msg.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap()),
                      keysym, state});
    QDBusConnection::sessionBus().asyncCall(msg); // fire-and-forget; Notify* return nothing
    if (state) {
        m_heldKeys.insert(keysym);
    } else {
        m_heldKeys.remove(keysym);
    }
}

void CoworkPortal::notifyButton(int button, uint state)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyPointerButton"));
    msg.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap()),
                      button, state});
    QDBusConnection::sessionBus().asyncCall(msg);
    if (state) {
        m_heldButtons.insert(button);
    } else {
        m_heldButtons.remove(button);
    }
}

void CoworkPortal::notifyPointerMotionAbsolute(uint streamNodeId, double x, double y)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.RemoteDesktop"),
        QStringLiteral("NotifyPointerMotionAbsolute"));
    msg.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap()),
                      streamNodeId, x, y});
    QDBusConnection::sessionBus().asyncCall(msg);
}

void CoworkPortal::notifyPointerMotionRelative(double dx, double dy)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    // NotifyPointerMotion takes raw dx/dy deltas — no stream node, no global→stream map.
    // A pointer-grabbing game consumes these as mouse-look; the absolute path's recenter
    // fight does not apply.
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyPointerMotion"));
    msg.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap()),
                      dx, dy});
    QDBusConnection::sessionBus().asyncCall(msg);
}

void CoworkPortal::notifyAxis(double dx, double dy)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyPointerAxis"));
    msg.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap()),
                      dx, dy});
    QDBusConnection::sessionBus().asyncCall(msg);
}

void CoworkPortal::notifyAxisDiscrete(uint axis, int steps)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyPointerAxisDiscrete"));
    msg.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap()),
                      axis, steps});
    QDBusConnection::sessionBus().asyncCall(msg);
}

bool CoworkPortal::globalToStream(int gx, int gy, uint &outNode, double &outLx, double &outLy) const
{
    for (const StreamInfo &s : m_streams) {
        if (gx >= s.originX && gx < s.originX + s.w && gy >= s.originY && gy < s.originY + s.h) {
            outNode = s.nodeId;
            // Clamp local coordinates into [0, w-1] / [0, h-1].
            int lx = gx - s.originX;
            int ly = gy - s.originY;
            lx = qBound(0, lx, s.w > 0 ? s.w - 1 : 0);
            ly = qBound(0, ly, s.h > 0 ? s.h - 1 : 0);
            outLx = double(lx);
            outLy = double(ly);
            return true;
        }
    }
    return false;
}

void CoworkPortal::parseStreams(const QVariant &v)
{
    // streams is a(ua{sv}): an array of (uint node_id, a{sv} props) structs. Each props
    // map carries "position" (a (ii) struct → origin) and "size" (a (ii) struct → w,h).
    m_streams.clear();
    if (!v.canConvert<QDBusArgument>()) {
        return;
    }
    // MUST be const: QDBusArgument's begin/endStructure() etc. have both a non-const
    // (marshalling/write) and a const (demarshalling/read) overload taking no args, so on
    // a NON-const argument C++ picks the WRITE overload — which corrupts the read iterator
    // ("write from a read-only object") and then aborts in libdbus ("type struct not a
    // basic type"). A const argument selects the reading overloads.
    const QDBusArgument arg = v.value<QDBusArgument>();
    arg.beginArray();
    while (!arg.atEnd()) {
        uint nodeId = 0;
        QVariantMap props;
        arg.beginStructure();
        arg >> nodeId >> props;
        arg.endStructure();

        StreamInfo info;
        info.nodeId = nodeId;

        // position / size arrive as (ii) structs wrapped in a QDBusArgument.
        const auto readPair = [](const QVariant &pv, int &a, int &b) {
            if (!pv.canConvert<QDBusArgument>()) {
                return;
            }
            const QDBusArgument pa = pv.value<QDBusArgument>(); // const → reading overloads
            pa.beginStructure();
            pa >> a >> b;
            pa.endStructure();
        };
        if (props.contains(QStringLiteral("position"))) {
            readPair(props.value(QStringLiteral("position")), info.originX, info.originY);
        }
        if (props.contains(QStringLiteral("size"))) {
            readPair(props.value(QStringLiteral("size")), info.w, info.h);
        }
        m_streams.append(info);
    }
    arg.endArray();
}

void CoworkPortal::openPipeWireRemote(std::function<void(int)> cb)
{
    // Plain method call (NOT a Request) returning a unix fd — the PipeWire remote. Async
    // for the same reason as portalRequest: this runs on the preflight path, and a portal
    // that stops answering must not take the GUI thread down with it.
    QDBusMessage m = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.ScreenCast"), QStringLiteral("OpenPipeWireRemote"));
    m.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap())});
    auto *watcher =
        new QDBusPendingCallWatcher(QDBusConnection::sessionBus().asyncCall(m), this);
    connect(watcher, &QDBusPendingCallWatcher::finished, this, [cb](QDBusPendingCallWatcher *w) {
        w->deleteLater();
        QDBusPendingReply<QDBusUnixFileDescriptor> reply = *w;
        if (reply.isError() || !reply.value().isValid()) {
            cb(-1);
            return;
        }
        // QDBusUnixFileDescriptor owns its fd and closes it when destroyed; dup so the fd
        // outlives this reply.
        cb(::dup(reply.value().fileDescriptor()));
    });
}

#ifdef AK_HAVE_PIPEWIRE
void CoworkPortal::startPwConsumer(uint nodeId)
{
    // SPIKE-1: keep the screencast stream consumed so KWin honours absolute motion. If a
    // live KWin session proves this is unnecessary, drop this and stopPwConsumer().
    stopPwConsumer();
    if (m_pwFd < 0) {
        return;
    }
    m_pwConsumer = new PwConsumer();
    if (!m_pwConsumer->start(m_pwFd, nodeId)) {
        m_pwConsumer->stop();
        delete m_pwConsumer;
        m_pwConsumer = nullptr;
    }
}

void CoworkPortal::stopPwConsumer()
{
    if (m_pwConsumer) {
        m_pwConsumer->stop();
        delete m_pwConsumer;
        m_pwConsumer = nullptr;
    }
}
#endif // AK_HAVE_PIPEWIRE

bool CoworkPortal::runOneOp(const QJsonObject &op)
{
    if (m_rdSession.isEmpty()) {
        ++m_batchDropped;
        if (m_batchError.isEmpty()) {
            m_batchError = i18n("the desktop control session is gone");
        }
        return false;
    }
    const QString t = op.value(QStringLiteral("t")).toString();
    const uint state = uint(op.value(QStringLiteral("state")).toInt());
    bool applied = true;
    if (t == QLatin1String("key")) {
        notifyKeysym(op.value(QStringLiteral("keysym")).toInt(), state);
    } else if (t == QLatin1String("btn")) {
        // Back/forward etc. arrive as larger int codes (0x113/0x114); passed through
        // unchanged.
        notifyButton(op.value(QStringLiteral("button")).toInt(), state);
    } else if (t == QLatin1String("move")) {
        const int gx = op.value(QStringLiteral("x")).toInt();
        const int gy = op.value(QStringLiteral("y")).toInt();
        uint node = 0;
        double lx = 0.0, ly = 0.0;
        if (globalToStream(gx, gy, node, lx, ly)) {
            notifyPointerMotionAbsolute(node, lx, ly);
            m_ptr = QPoint(gx, gy);
            m_ptrKnown = true;
        } else {
            // No captured stream contains this point (or no screencast map at all): an
            // absolute move is impossible without a node id, so it cannot be sent.
            //
            // SECURITY (audit F3): this is a DROP, not a skip. The cursor stays wherever it
            // was — which is NOT where the core aimed it — so the core's mirror must not be
            // allowed to record the requested point. m_ptrKnown goes false and the drop is
            // counted; injectOutcome carries both back, and the core destroys the mirror.
            qWarning("cowork: move (%d,%d) has no containing screencast stream; dropped", gx, gy);
            m_ptrKnown = false;
            applied = false;
            if (m_batchError.isEmpty()) {
                m_batchError = i18n("the desktop could not move the pointer to (%1,%2): that point "
                                    "lies on no captured screen",
                                    gx, gy);
            }
        }
    } else if (t == QLatin1String("move_rel")) {
        // Raw relative delta — no stream map needed. Doubles so sub-pixel cadence survives.
        // It does NOT touch m_ptr/m_ptrKnown: those answer "did the last ABSOLUTE move
        // land where it was aimed", which is what the core cross-checks; the core does its
        // own (bounds-checked) accounting for relative drift on top of that.
        notifyPointerMotionRelative(op.value(QStringLiteral("dx")).toDouble(),
                                    op.value(QStringLiteral("dy")).toDouble());
    } else if (t == QLatin1String("axis")) {
        notifyAxis(double(op.value(QStringLiteral("dx")).toInt()),
                   double(op.value(QStringLiteral("dy")).toInt()));
    } else if (t == QLatin1String("axis_discrete")) {
        notifyAxisDiscrete(uint(op.value(QStringLiteral("axis")).toInt()),
                           op.value(QStringLiteral("steps")).toInt());
    } else {
        // An op kind this build does not know how to play. Fail closed: report it dropped
        // rather than let the core believe a batch ran in full.
        qWarning("cowork: unknown inject op %s; dropped", qPrintable(t));
        applied = false;
        if (m_batchError.isEmpty()) {
            m_batchError = i18n("the desktop could not play a \"%1\" action", t);
        }
    }
    if (applied) {
        ++m_batchApplied;
    } else {
        ++m_batchDropped;
    }
    return applied;
}

void CoworkPortal::beginInjectBatch()
{
    m_batchApplied = 0;
    m_batchDropped = 0;
    m_batchError.clear();
}

void CoworkPortal::abandonBatch(int skipped)
{
    if (skipped > 0) {
        m_batchDropped += skipped;
    }
    // A half-played batch must not leave a key or a mouse button logically down: the ops
    // that would have released them are exactly the ones being abandoned.
    releaseHeld();
}

QJsonObject CoworkPortal::injectOutcome() const
{
    return QJsonObject{
        {QStringLiteral("opsApplied"), m_batchApplied},
        {QStringLiteral("opsDropped"), m_batchDropped},
        {QStringLiteral("ptrKnown"), m_ptrKnown},
        {QStringLiteral("ptrX"), m_ptr.x()},
        {QStringLiteral("ptrY"), m_ptr.y()},
    };
}

void CoworkPortal::runInjectOps(const QString &corrId, const QJsonArray &ops)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    // Fast path: if no op carries delayMs>0, run the whole batch synchronously.
    bool anyDelay = false;
    for (const QJsonValue &ov : ops) {
        if (ov.toObject().value(QStringLiteral("delayMs")).toInt() > 0) {
            anyDelay = true;
            break;
        }
    }
    if (!anyDelay) {
        beginInjectBatch();
        for (int i = 0; i < ops.size(); ++i) {
            if (!runOneOp(ops.at(i).toObject())) {
                // Stop at the first op that could not be applied — see abandonBatch.
                abandonBatch(ops.size() - i - 1);
                break;
            }
        }
        return; // caller replies (m_playCorrId stays != corrId)
    }

    // Timed playback: a profiled move is many move ops each carrying delayMs. Drive one
    // op per QTimer tick so the Qt event loop never blocks. The reply is sent on drain.
    stopPlayback(); // a prior playback (if any) is superseded
    beginInjectBatch();
    m_playOps = ops;
    m_playIdx = 0;
    m_playCorrId = corrId;
    // Kick the first tick; playbackTick applies op[idx]'s own delayMs before executing.
    m_playTimer.start(0);
}

void CoworkPortal::playbackTick()
{
    if (m_playIdx >= m_playOps.size() || m_rdSession.isEmpty()) {
        // Drained (or session gone): reply once and clear playback state.
        const QString corrId = m_playCorrId;
        const bool sessionAlive = !m_rdSession.isEmpty();
        // A session that vanished with ops still unplayed is a MID-PLAY failure, not a
        // completed batch: the cursor is stranded somewhere along an interpolated path
        // (which is allowed to cross Agent Kate's windows, since motion alone is harmless)
        // and nobody can say where. Reporting success here would let the core commit its
        // mirror to the target the batch never reached — say so instead, and the core
        // destroys the mirror.
        const bool completed =
            sessionAlive && m_playIdx >= m_playOps.size() && m_batchError.isEmpty();
        const QString err = m_batchError.isEmpty()
            ? i18n("desktop control was stopped part-way through")
            : m_batchError;
        m_playOps = QJsonArray();
        m_playIdx = 0;
        m_playCorrId.clear();
        if (!corrId.isEmpty()) {
            replyResult(corrId, QStringLiteral("inject"), completed,
                        completed ? QString() : err, injectOutcome());
        }
        // Carry on with any batches that queued behind this timed one.
        if (sessionAlive && !m_injectQueue.isEmpty()) {
            flushInjectQueue();
        }
        return;
    }
    const QJsonObject op = m_playOps.at(m_playIdx).toObject();
    const bool applied = runOneOp(op);
    ++m_playIdx;
    if (!applied) {
        // The rest of the script is abandoned (see abandonBatch): jump to the drain branch,
        // which now replies FAILURE because m_batchError is set.
        abandonBatch(m_playOps.size() - m_playIdx);
        m_playIdx = m_playOps.size();
        m_playTimer.start(0);
        return;
    }
    // Schedule the next op after ITS delayMs (the pause applied BEFORE executing it).
    int nextDelay = 0;
    if (m_playIdx < m_playOps.size()) {
        nextDelay = m_playOps.at(m_playIdx).toObject().value(QStringLiteral("delayMs")).toInt();
    }
    m_playTimer.start(qMax(0, nextDelay));
}

void CoworkPortal::stopPlayback(const QString &reason)
{
    m_playTimer.stop();
    // If a timed playback is still in flight, its corrId is the ONLY reference to that
    // batch (it was already taken out of the inject queue). Reply now so the core's
    // runPortal wait resolves immediately instead of hanging until its timeout — this is
    // the teardown/rebuild path (kill-switch, idle, device/screencast growth). The
    // supersede caller (runInjectOps) only reaches here with no active playback, so this
    // never double-replies a live batch.
    if (!m_playCorrId.isEmpty()) {
        // Outcome fields go out even on this failure path: the batch DID play in part, and
        // the core needs "what actually landed" (not just "it failed") to reason about the
        // stranded cursor.
        replyResult(m_playCorrId, QStringLiteral("inject"), false,
                    reason.isEmpty() ? i18n("desktop control was stopped") : reason,
                    injectOutcome());
    }
    m_playOps = QJsonArray();
    m_playIdx = 0;
    m_playCorrId.clear();
}

void CoworkPortal::abortInjection(const QString &reason)
{
    const bool hadPlayback = !m_playCorrId.isEmpty();
    const bool hadQueue = !m_injectQueue.isEmpty();
    if (!hadPlayback && !hadQueue) {
        return; // nothing in flight — a focus change is only interesting during playback
    }
    qWarning("cowork: aborting injection — %s", qPrintable(reason));
    // stopPlayback answers the in-flight batch with THIS reason, so the agent (and the
    // audit log) learn why its script stopped rather than "desktop control was stopped".
    stopPlayback(reason);
    // Anything the aborted script left pressed must not stay pressed: a stuck modifier is
    // an implicit grab on the window that just took focus.
    releaseHeld();
    // Everything queued behind the aborted script was authorized under the same
    // now-invalid focus assumption. Fail it too rather than replaying it into whatever
    // took focus.
    const auto queued = m_injectQueue;
    m_injectQueue.clear();
    for (const auto &pi : queued) {
        replyResult(pi.corrId, QStringLiteral("inject"), false, reason);
    }
}

// releaseHeld synthesises a key/button-up for every input still logically pressed.
// Without this, tearing a session down between a press and its release would leave
// KWin in an implicit grab waiting for an up-event that never comes — which reads as
// a frozen, unresponsive cursor that only a compositor restart clears.
void CoworkPortal::releaseHeld()
{
    const auto keys = m_heldKeys;
    for (int k : keys) {
        notifyKeysym(k, 0);
    }
    const auto buttons = m_heldButtons;
    for (int b : buttons) {
        notifyButton(b, 0);
    }
    m_heldKeys.clear();
    m_heldButtons.clear();
}

void CoworkPortal::flushInjectQueue()
{
    // Preflights are satisfied the moment the session is up — they asked for the
    // permission, not for any ops — so they are answered before the queue drains.
    if (!m_preflightCorrIds.isEmpty()) {
        const QStringList waiting = m_preflightCorrIds;
        m_preflightCorrIds.clear();
        for (const QString &corrId : waiting) {
            replyResult(corrId, QStringLiteral("preflight"), true, QString(),
                        {{QStringLiteral("remoteDesktop"), true},
                         {QStringLiteral("accessibility"), m_a11yEnabled},
                         {QStringLiteral("screencast"), m_scReady}});
        }
    }
    // Drain queued batches. A batch with no delayMs runs synchronously and is replied to
    // here. A timed (profiled-motion) batch is handed to runInjectOps, which drives it
    // off a QTimer and replies on drain — we then stop and re-queue any remaining batches
    // so the next flush (triggered after playback, see below) carries on. Since timed and
    // synchronous batches are processed in order, this preserves ordering.
    while (!m_injectQueue.isEmpty()) {
        // A batch that queued behind a timed playback never went through handleInject's
        // device check against the LIVE session, so re-check it here: running it on a
        // session that lacks its devices would silently drop the ops. Only the device mask
        // triggers a rebuild — a fresh session always comes up with keyboard|pointer, so
        // this terminates; a missing screencast map would not be fixed by rebuilding, and
        // an absolute move without one is skipped with a warning in runOneOp.
        if ((deviceTypesFor(m_injectQueue.constFirst().ops) & ~m_rdTypes) != 0) {
            closeSessionOnly(); // leaves the queue intact
            startRemoteDesktop();
            return;
        }
        const PendingInject pi = m_injectQueue.takeFirst();
        runInjectOps(pi.corrId, pi.ops);
        if (m_playCorrId == pi.corrId) {
            // Timed playback started for this batch; reply happens on drain. Anything
            // still queued waits until playback finishes (re-flushed from playbackTick).
            return;
        }
        replyResult(pi.corrId, QStringLiteral("inject"), m_batchError.isEmpty(), m_batchError,
                    injectOutcome());
    }
}

void CoworkPortal::failPortalStep(const QString &declined, const QString &faulted)
{
    // An empty detail means the portal answered normally, i.e. the user really did decline.
    const QString detail = portalFailureDetail();
    const QString base = (detail.isEmpty() || faulted.isEmpty()) ? declined : faulted;
    failInjectQueue(base + detail);
}

void CoworkPortal::failInjectQueue(const QString &err)
{
    // A half-built session (CreateSession returned a handle, a later step failed) is
    // still a live portal session; drop it properly instead of just forgetting the
    // path, or every failed attempt — and preflight makes those routine — leaks one.
    closeSessionOnly();
    const auto queued = m_injectQueue;
    m_injectQueue.clear();
    for (const auto &pi : queued) {
        replyResult(pi.corrId, QStringLiteral("inject"), false, err);
    }
    // A declined or failed portal is a failed preflight too — the human is told
    // desktop control is not available, rather than finding out through an agent
    // action that dies minutes later.
    const QStringList waiting = m_preflightCorrIds;
    m_preflightCorrIds.clear();
    for (const QString &corrId : waiting) {
        replyResult(corrId, QStringLiteral("preflight"), false, err,
                    {{QStringLiteral("accessibility"), m_a11yEnabled}});
    }
}

// closeSessionOnly drops the live portal session (releasing any held input first)
// but leaves the inject queue untouched, so handleInject can rebuild a session with a
// wider device set without failing the work that triggered the rebuild.
void CoworkPortal::closePortalSessionPath(const QString &sessionPath)
{
    if (sessionPath.isEmpty()) {
        return;
    }
    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), sessionPath,
        QStringLiteral("org.freedesktop.portal.Session"), QStringLiteral("Close"));
    QDBusConnection::sessionBus().asyncCall(msg);
}

void CoworkPortal::abandonPortalWaiters()
{
    // Layer 1 — the session the in-flight CreateSession was told to mint. We chose its
    // token, so its object path is known WITHOUT waiting for the reply (portalHandlePath):
    // if the portal has already created it, this closes it now; if it has not, the Close
    // fails harmlessly on a path that does not exist yet, and layer 2 covers that case.
    const QString predicted = m_rdPendingSessionPath;
    m_rdPendingSessionPath.clear();
    closePortalSessionPath(predicted);

    // Layer 2 — the waiters. They are CANCELLED, not destroyed: a destroyed waiter can
    // never run its continuation, and for CreateSession that continuation is what closes
    // the session handle a late Response carries (the portal may mint the session after
    // layer 1's Close). Cancelling keeps the subscription alive on a short leash instead,
    // so the continuation still runs, still bails on the stale generation, and the waiter
    // still self-destructs — bounded by kPortalCancelledGraceMs.
    //
    // Dropping them from m_rdWaiters is what makes this safe to call repeatedly: a
    // cancelled waiter is nobody's to cancel again, and it frees itself.
    const auto waiters = m_rdWaiters;
    m_rdWaiters.clear();
    for (const QPointer<PortalResponseWaiter> &w : waiters) {
        if (w) {
            // Never emits synchronously (it only re-arms a timer), so callers that bump
            // m_rdGeneration immediately after this cannot be re-entered mid-bump.
            w->cancel(kPortalCancelledGraceMs);
        }
    }
}

void CoworkPortal::closeSessionOnly()
{
    // Cancel any in-flight timed playback so it cannot drive ops into a torn-down session,
    // and disarm the hand-shake watchdog: this attempt is over either way.
    stopPlayback();
    m_rdWatchdog.stop();
    // End of this attempt: every hand-shake continuation still in flight is now stale.
    ++m_rdGeneration;
    // ...and stale continuations are not just no-ops to be ignored, they are objects with a
    // live D-Bus subscription, and one of them may still be handed a session handle nobody
    // owns. Abandon them here — this is the single choke point every bail-out reaches
    // (watchdog fire and kill-switch both come through failInjectQueue).
    abandonPortalWaiters();
    if (!m_rdSession.isEmpty()) {
        releaseHeld();
        closePortalSessionPath(m_rdSession);
    }
    // Tear down the screencast side: stop the PipeWire consumer, close the remote fd, and
    // drop the stream map. (No restore token to keep — remote-desktop sessions can't
    // persist, so the next session re-prompts.)
#ifdef AK_HAVE_PIPEWIRE
    stopPwConsumer();
#endif
    if (m_pwFd >= 0) {
        ::close(m_pwFd);
        m_pwFd = -1;
    }
    m_streams.clear();
    // The stream map (and with it every coordinate this session could address) is gone, so
    // the "last absolute move landed" evidence no longer describes anything we can vouch
    // for. Drop it: the next batch re-establishes it, and until then the core is told the
    // pointer position is unproven rather than being handed a stale one.
    m_ptrKnown = false;
    m_scReady = false;
    m_rdSession.clear();
    m_rdReady = false;
    m_rdStarting = false;
    m_rdTypes = 0;
}

void CoworkPortal::teardownRemoteDesktop()
{
    failInjectQueue(i18n("desktop control was stopped")); // closes the session first
}

void CoworkPortal::handleKillInject(const QJsonObject &req)
{
    teardownRemoteDesktop();
    replyResult(req.value(QStringLiteral("corrId")).toString(), QStringLiteral("killInject"), true, QString());
}
