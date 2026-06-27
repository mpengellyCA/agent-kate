#include "CoworkPortal.h"

#include "BrowserLaunch.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QBuffer>
#include <QDBusArgument>
#include <QDBusConnection>
#include <QDBusConnectionInterface>
#include <QDBusInterface>
#include <QDBusMessage>
#include <QDBusObjectPath>
#include <QDBusPendingCallWatcher>
#include <QDBusPendingReply>
#include <QDBusReply>
#include <QDBusUnixFileDescriptor>
#include <QDBusVariant>
#include <QFile>
#include <QImage>
#include <QJsonValue>
#include <QRandomGenerator>
#include <QSocketNotifier>
#include <QUrl>
#include <QWidget>

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
QDBusInterface a11yStatusProps()
{
    return QDBusInterface(QStringLiteral("org.a11y.Bus"), QStringLiteral("/org/a11y/bus"),
                          QStringLiteral("org.freedesktop.DBus.Properties"),
                          QDBusConnection::sessionBus());
}

bool readA11yStatus(const QString &prop, bool fallback)
{
    QDBusInterface props = a11yStatusProps();
    if (!props.isValid()) {
        return fallback;
    }
    QDBusReply<QDBusVariant> r = props.call(QStringLiteral("Get"), QStringLiteral("org.a11y.Status"), prop);
    return r.isValid() ? r.value().variant().toBool() : fallback;
}

void writeA11yStatus(const QString &prop, bool value)
{
    QDBusInterface props = a11yStatusProps();
    if (props.isValid()) {
        props.call(QStringLiteral("Set"), QStringLiteral("org.a11y.Status"), prop,
                   QVariant::fromValue(QDBusVariant(value)));
    }
}
} // namespace

PortalResponseWaiter::PortalResponseWaiter(const QString &requestPath, QObject *parent)
    : QObject(parent)
{
    QDBusConnection::sessionBus().connect(
        QString::fromLatin1(kPortalService), requestPath,
        QStringLiteral("org.freedesktop.portal.Request"), QStringLiteral("Response"),
        this, SLOT(onResponse(uint, QVariantMap)));
}

void PortalResponseWaiter::onResponse(uint code, const QVariantMap &results)
{
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
}

CoworkPortal::~CoworkPortal()
{
    // Release the screencast/PipeWire resources and cancel any playback timer before we
    // go (closeSessionOnly does all of this and is safe with no live session).
    closeSessionOnly();
    restoreAtspiStatus();
}

void CoworkPortal::enableAtspiStatusForLaunch()
{
    // Record the user's original state once, so we can put it back on teardown.
    if (!m_a11yStatusCaptured) {
        m_origIsEnabled = readA11yStatus(QStringLiteral("IsEnabled"), false);
        m_origScreenReader = readA11yStatus(QStringLiteral("ScreenReaderEnabled"), false);
        m_a11yStatusCaptured = true;
    }
    writeA11yStatus(QStringLiteral("IsEnabled"), true);
    writeA11yStatus(QStringLiteral("ScreenReaderEnabled"), true);
}

void CoworkPortal::restoreAtspiStatus()
{
    if (!m_a11yStatusCaptured) {
        return;
    }
    writeA11yStatus(QStringLiteral("ScreenReaderEnabled"), m_origScreenReader);
    writeA11yStatus(QStringLiteral("IsEnabled"), m_origIsEnabled);
    m_a11yStatusCaptured = false;
}

void CoworkPortal::onNotification(const QString &method, const QJsonObject &params)
{
    // The kill-switch tears down any live RemoteDesktop input session immediately.
    if (method == QLatin1String("cowork.killSwitch")) {
        if (params.value(QStringLiteral("on")).toBool()) {
            teardownRemoteDesktop();
        }
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
    QString sender = bus.baseService();
    if (sender.startsWith(QLatin1Char(':'))) {
        sender.remove(0, 1);
    }
    sender.replace(QLatin1Char('.'), QLatin1Char('_'));
    const QString requestPath =
        QStringLiteral("%1/request/%2/%3").arg(QString::fromLatin1(kPortalPath), sender, token);

    auto *waiter = new PortalResponseWaiter(requestPath, this);
    connect(waiter, &PortalResponseWaiter::responded, this,
            [this, corrId, maxDim, format](uint code, const QVariantMap &results) {
                finishScreenshot(corrId, maxDim, format, code, results);
            });

    QDBusInterface portal(QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
                          QStringLiteral("org.freedesktop.portal.Screenshot"), bus);
    QVariantMap opts;
    opts.insert(QStringLiteral("handle_token"), token);
    // interactive=true lets the user pick a specific window/region in KDE's native
    // picker (a "share this window" flow); false captures the screen directly.
    opts.insert(QStringLiteral("interactive"), req.value(QStringLiteral("interactive")).toBool());

    QDBusReply<QDBusObjectPath> reply = portal.call(QStringLiteral("Screenshot"),
                                                    parentWindowHandle(), opts);
    if (!reply.isValid()) {
        waiter->deleteLater();
        replyResult(corrId, QStringLiteral("screenshot"), false,
                    QStringLiteral("portal call failed: %1").arg(reply.error().message()));
    }
    // Otherwise the waiter delivers the result asynchronously.
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
    if (b.family == QLatin1String("chromium")) {
        enableAtspiStatusForLaunch();
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

    QString sender = bus.baseService();
    if (sender.startsWith(QLatin1Char(':'))) {
        sender.remove(0, 1);
    }
    sender.replace(QLatin1Char('.'), QLatin1Char('_'));
    const QString reqPath =
        QStringLiteral("%1/request/%2/%3").arg(QString::fromLatin1(kPortalPath), sender, token);

    auto *waiter = new PortalResponseWaiter(reqPath, this);
    connect(waiter, &PortalResponseWaiter::responded, this,
            [cb](uint code, const QVariantMap &results) { cb(code, results); });

    QDBusMessage msg = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath), iface, method);
    QVariantList full = args;
    full.append(options);
    msg.setArguments(full);

    QDBusMessage reply = bus.call(msg);
    if (reply.type() == QDBusMessage::ErrorMessage) {
        // Log the concrete D-Bus error: a method-level rejection (e.g. an invalid option)
        // never reaches the Response signal, so without this it reads as a silent
        // "declined" with no clue why.
        qWarning("cowork: portal %s.%s failed: %s", qUtf8Printable(iface), qUtf8Printable(method),
                 qUtf8Printable(reply.errorMessage()));
        waiter->deleteLater();
        cb(2, QVariantMap()); // no Response will arrive; surface the failure now
    }
}

uint CoworkPortal::deviceTypesFor(const QJsonArray &ops)
{
    uint types = 0;
    for (const QJsonValue &ov : ops) {
        const QString t = ov.toObject().value(QStringLiteral("t")).toString();
        if (t == QLatin1String("key")) {
            types |= 1u; // keyboard
        } else if (t == QLatin1String("btn") || t == QLatin1String("move")
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
        const uint needed = deviceTypesFor(ops);
        const bool needSc = needsScreencastFor(ops);
        if ((needed & ~m_rdTypes) == 0 && (!needSc || m_scReady)) {
            // If a timed playback is in flight, queue this batch behind it so ops do not
            // interleave and the active playback's corrId is never clobbered; the queue is
            // re-flushed when playback drains (playbackTick → flushInjectQueue).
            if (!m_playCorrId.isEmpty()) {
                m_injectQueue.append({corrId, ops});
                return;
            }
            // The live session already owns every device these ops use AND has the
            // screencast stream a move needs (if any).
            runInjectOps(corrId, ops);
            // runInjectOps replies itself when it starts a non-blocking timed playback;
            // otherwise (synchronous fast path) it returns having done nothing async, so
            // we reply here. It signals that by leaving m_playCorrId == corrId.
            if (m_playCorrId != corrId) {
                replyResult(corrId, QStringLiteral("inject"), true, QString());
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

void CoworkPortal::startRemoteDesktop()
{
    m_rdStarting = true;
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

    portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("CreateSession"),
                  {}, createOpts, [this, startTypes, wantScreencast](uint code, const QVariantMap &results) {
        if (code != 0) {
            failInjectQueue(i18n("the remote-control session could not be created"));
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
                      [this, startTypes, wantScreencast](uint code2, const QVariantMap &) {
            if (code2 != 0) {
                failInjectQueue(i18n("input devices were not granted"));
                return;
            }
            // The Start step is shared by both paths; capture it so SelectSources can
            // chain into it (screencast path) or we can call it directly (input-only).
            auto doStart = [this, startTypes, wantScreencast]() {
                portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("Start"),
                              {QVariant::fromValue(QDBusObjectPath(m_rdSession)), parentWindowHandle()},
                              QVariantMap(),
                              [this, startTypes, wantScreencast](uint code3, const QVariantMap &startResults) {
                    m_rdStarting = false;
                    // The session may have been torn down while the user sat on the portal
                    // dialog (idle-timeout / kill-switch). Don't parse streams or stand up a
                    // PipeWire consumer against a dead session — that would leak an fd and a
                    // consumer thread bound to nothing.
                    if (m_rdSession.isEmpty()) {
                        failInjectQueue(i18n("desktop control was stopped"));
                        return;
                    }
                    if (code3 != 0) {
                        failInjectQueue(i18n("remote control was declined"));
                        return;
                    }
                    m_rdReady = true;
                    m_rdTypes = startTypes;
                    if (wantScreencast) {
                        // Parse the captured monitor streams (a(ua{sv})) — our coordinate
                        // map. No restore_token to stash: remote-desktop sessions can't
                        // persist (see SelectSources above), so each session is fresh.
                        parseStreams(startResults.value(QStringLiteral("streams")));
                        m_scReady = !m_streams.isEmpty();
                        if (m_scReady) {
                            // Open the PipeWire remote and (SPIKE-1) keep one stream
                            // consumed so KWin honours absolute motion.
                            m_pwFd = openPipeWireRemote();
#ifdef AK_HAVE_PIPEWIRE
                            if (m_pwFd >= 0) {
                                startPwConsumer(m_streams.first().nodeId);
                            }
#endif
                        }
                    }
                    flushInjectQueue();
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
                          [this, doStart](uint codeSc, const QVariantMap &) {
                if (codeSc != 0) {
                    failInjectQueue(i18n("screen capture for cursor control was declined"));
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

int CoworkPortal::openPipeWireRemote()
{
    // Plain method call (NOT a Request) returning a unix fd — the PipeWire remote.
    QDBusMessage m = QDBusMessage::createMethodCall(
        QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
        QStringLiteral("org.freedesktop.portal.ScreenCast"), QStringLiteral("OpenPipeWireRemote"));
    m.setArguments({QVariant::fromValue(QDBusObjectPath(m_rdSession)), QVariant::fromValue(QVariantMap())});
    QDBusReply<QDBusUnixFileDescriptor> reply = QDBusConnection::sessionBus().call(m);
    if (!reply.isValid() || !reply.value().isValid()) {
        return -1;
    }
    // QDBusUnixFileDescriptor owns its fd and closes it when destroyed; dup so the fd
    // outlives this reply.
    return ::dup(reply.value().fileDescriptor());
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

void CoworkPortal::runOneOp(const QJsonObject &op)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    const QString t = op.value(QStringLiteral("t")).toString();
    const uint state = uint(op.value(QStringLiteral("state")).toInt());
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
        } else {
            // No captured stream contains this point (or no screencast map at all): an
            // absolute move is impossible without a node id, so skip it.
            qWarning("cowork: move (%d,%d) has no containing screencast stream; skipped", gx, gy);
        }
    } else if (t == QLatin1String("axis")) {
        notifyAxis(double(op.value(QStringLiteral("dx")).toInt()),
                   double(op.value(QStringLiteral("dy")).toInt()));
    } else if (t == QLatin1String("axis_discrete")) {
        notifyAxisDiscrete(uint(op.value(QStringLiteral("axis")).toInt()),
                           op.value(QStringLiteral("steps")).toInt());
    }
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
        for (const QJsonValue &ov : ops) {
            runOneOp(ov.toObject());
        }
        return; // caller replies (m_playCorrId stays != corrId)
    }

    // Timed playback: a profiled move is many move ops each carrying delayMs. Drive one
    // op per QTimer tick so the Qt event loop never blocks. The reply is sent on drain.
    stopPlayback(); // a prior playback (if any) is superseded
    m_playOps = ops;
    m_playIdx = 0;
    m_playCorrId = corrId;
    // Kick the first tick; playbackTick applies op[idx]'s own delayMs before executing.
    m_playTimer.start(0);
}

void CoworkPortal::playbackTick()
{
    if (m_playIdx >= m_playOps.size() || m_rdSession.isEmpty()) {
        // Drained (or session gone): reply success once and clear playback state.
        const QString corrId = m_playCorrId;
        const bool sessionAlive = !m_rdSession.isEmpty();
        m_playOps = QJsonArray();
        m_playIdx = 0;
        m_playCorrId.clear();
        if (!corrId.isEmpty()) {
            replyResult(corrId, QStringLiteral("inject"), true, QString());
        }
        // Carry on with any batches that queued behind this timed one.
        if (sessionAlive && !m_injectQueue.isEmpty()) {
            flushInjectQueue();
        }
        return;
    }
    const QJsonObject op = m_playOps.at(m_playIdx).toObject();
    runOneOp(op);
    ++m_playIdx;
    // Schedule the next op after ITS delayMs (the pause applied BEFORE executing it).
    int nextDelay = 0;
    if (m_playIdx < m_playOps.size()) {
        nextDelay = m_playOps.at(m_playIdx).toObject().value(QStringLiteral("delayMs")).toInt();
    }
    m_playTimer.start(qMax(0, nextDelay));
}

void CoworkPortal::stopPlayback()
{
    m_playTimer.stop();
    // If a timed playback is still in flight, its corrId is the ONLY reference to that
    // batch (it was already taken out of the inject queue). Reply now so the core's
    // runPortal wait resolves immediately instead of hanging until its timeout — this is
    // the teardown/rebuild path (kill-switch, idle, device/screencast growth). The
    // supersede caller (runInjectOps) only reaches here with no active playback, so this
    // never double-replies a live batch.
    if (!m_playCorrId.isEmpty()) {
        replyResult(m_playCorrId, QStringLiteral("inject"), false, i18n("desktop control was stopped"));
    }
    m_playOps = QJsonArray();
    m_playIdx = 0;
    m_playCorrId.clear();
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
    // Drain queued batches. A batch with no delayMs runs synchronously and is replied to
    // here. A timed (profiled-motion) batch is handed to runInjectOps, which drives it
    // off a QTimer and replies on drain — we then stop and re-queue any remaining batches
    // so the next flush (triggered after playback, see below) carries on. Since timed and
    // synchronous batches are processed in order, this preserves ordering.
    while (!m_injectQueue.isEmpty()) {
        const PendingInject pi = m_injectQueue.takeFirst();
        runInjectOps(pi.corrId, pi.ops);
        if (m_playCorrId == pi.corrId) {
            // Timed playback started for this batch; reply happens on drain. Anything
            // still queued waits until playback finishes (re-flushed from playbackTick).
            return;
        }
        replyResult(pi.corrId, QStringLiteral("inject"), true, QString());
    }
}

void CoworkPortal::failInjectQueue(const QString &err)
{
    m_rdStarting = false;
    m_rdReady = false;
    m_rdSession.clear();
    m_rdTypes = 0;
    const auto queued = m_injectQueue;
    m_injectQueue.clear();
    for (const auto &pi : queued) {
        replyResult(pi.corrId, QStringLiteral("inject"), false, err);
    }
}

// closeSessionOnly drops the live portal session (releasing any held input first)
// but leaves the inject queue untouched, so handleInject can rebuild a session with a
// wider device set without failing the work that triggered the rebuild.
void CoworkPortal::closeSessionOnly()
{
    // Cancel any in-flight timed playback so it cannot drive ops into a torn-down session.
    stopPlayback();
    if (!m_rdSession.isEmpty()) {
        releaseHeld();
        QDBusMessage msg = QDBusMessage::createMethodCall(
            QString::fromLatin1(kPortalService), m_rdSession,
            QStringLiteral("org.freedesktop.portal.Session"), QStringLiteral("Close"));
        QDBusConnection::sessionBus().asyncCall(msg);
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
    m_scReady = false;
    m_rdSession.clear();
    m_rdReady = false;
    m_rdStarting = false;
    m_rdTypes = 0;
}

void CoworkPortal::teardownRemoteDesktop()
{
    closeSessionOnly();
    failInjectQueue(i18n("desktop control was stopped"));
}

void CoworkPortal::handleKillInject(const QJsonObject &req)
{
    teardownRemoteDesktop();
    replyResult(req.value(QStringLiteral("corrId")).toString(), QStringLiteral("killInject"), true, QString());
}
