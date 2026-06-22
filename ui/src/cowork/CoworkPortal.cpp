#include "CoworkPortal.h"

#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QBuffer>
#include <QDBusConnection>
#include <QDBusInterface>
#include <QDBusMessage>
#include <QDBusObjectPath>
#include <QDBusReply>
#include <QFile>
#include <QImage>
#include <QJsonValue>
#include <QRandomGenerator>
#include <QUrl>
#include <QWidget>

#include <utility>

namespace {
constexpr auto kPortalService = "org.freedesktop.portal.Desktop";
constexpr auto kPortalPath = "/org/freedesktop/portal/desktop";
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

namespace {
// Idle window after which a live RemoteDesktop session is torn down on its own, so
// the virtual input devices it holds never linger for the whole run.
constexpr int kInjectIdleMs = 60'000;
} // namespace

CoworkPortal::CoworkPortal(CoreClient *core, QWidget *topLevel, QObject *parent)
    : QObject(parent), m_core(core), m_topLevel(topLevel)
{
    connect(m_core, &CoreClient::notification, this, &CoworkPortal::onNotification);
    m_idleTimer.setSingleShot(true);
    m_idleTimer.setInterval(kInjectIdleMs);
    connect(&m_idleTimer, &QTimer::timeout, this, [this] {
        if (m_rdReady || m_rdStarting) {
            teardownRemoteDesktop();
        }
    });
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
    if (img.isNull()) {
        replyResult(corrId, QStringLiteral("screenshot"), false,
                    QStringLiteral("could not read the captured image"));
        if (!path.isEmpty()) {
            QFile::remove(path);
        }
        return;
    }
    if (maxDim > 0 && (img.width() > maxDim || img.height() > maxDim)) {
        img = img.scaled(maxDim, maxDim, Qt::KeepAspectRatio, Qt::SmoothTransformation);
    }

    const bool jpeg = (format == QLatin1String("jpeg"));
    QByteArray bytes;
    QBuffer buf(&bytes);
    buf.open(QIODevice::WriteOnly);
    img.save(&buf, jpeg ? "JPEG" : "PNG", jpeg ? 85 : -1);
    buf.close();

    // The portal saved a full-resolution PNG to disk; it may contain secrets, so we
    // do not keep it once the (downscaled) bytes are in hand.
    if (!path.isEmpty()) {
        QFile::remove(path);
    }

    QJsonObject extra{
        {QStringLiteral("pngB64"), QString::fromLatin1(bytes.toBase64())},
        {QStringLiteral("mime"), jpeg ? QStringLiteral("image/jpeg") : QStringLiteral("image/png")},
        {QStringLiteral("width"), img.width()},
        {QStringLiteral("height"), img.height()},
    };
    replyResult(corrId, QStringLiteral("screenshot"), true, QString(), extra);
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
        } else if (t == QLatin1String("btn")) {
            types |= 2u; // pointer
        }
    }
    return types ? types : 1u; // default to keyboard-only — never a bare virtual pointer
}

void CoworkPortal::handleInject(const QJsonObject &req)
{
    const QString corrId = req.value(QStringLiteral("corrId")).toString();
    const QJsonArray ops = req.value(QStringLiteral("ops")).toArray();
    m_idleTimer.start(); // (re)arm the idle teardown on any inject activity

    if (m_rdReady && !m_rdSession.isEmpty()) {
        const uint needed = deviceTypesFor(ops);
        if ((needed & ~m_rdTypes) == 0) {
            // The live session already owns every device these ops use.
            runInjectOps(ops);
            replyResult(corrId, QStringLiteral("inject"), true, QString());
            return;
        }
        // The batch needs a device the session was not started with (e.g. a click
        // arriving on a keyboard-only session). Drop the session — keeping the queue
        // intact — and rebuild it below with the wider device set.
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

    // Request only the device types the queued work actually needs. The common case
    // (typing / media / navigation keys) is keyboard-only, so no virtual pointer is
    // created in the compositor — the cursor cannot be wedged by a phantom pointer.
    uint types = 0;
    for (const auto &pi : std::as_const(m_injectQueue)) {
        types |= deviceTypesFor(pi.ops);
    }
    if (types == 0) {
        types = 1u;
    }
    const uint startTypes = types;

    QVariantMap createOpts;
    createOpts.insert(QStringLiteral("session_handle_token"), sessToken);

    portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("CreateSession"),
                  {}, createOpts, [this, startTypes](uint code, const QVariantMap &results) {
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
                      [this, startTypes](uint code2, const QVariantMap &) {
            if (code2 != 0) {
                failInjectQueue(i18n("input devices were not granted"));
                return;
            }
            portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("Start"),
                          {QVariant::fromValue(QDBusObjectPath(m_rdSession)), parentWindowHandle()},
                          QVariantMap(), [this, startTypes](uint code3, const QVariantMap &) {
                m_rdStarting = false;
                if (code3 != 0) {
                    failInjectQueue(i18n("remote control was declined"));
                    return;
                }
                m_rdReady = true;
                m_rdTypes = startTypes;
                flushInjectQueue();
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

void CoworkPortal::runInjectOps(const QJsonArray &ops)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    for (const QJsonValue &ov : ops) {
        const QJsonObject op = ov.toObject();
        const QString t = op.value(QStringLiteral("t")).toString();
        const uint state = uint(op.value(QStringLiteral("state")).toInt());
        if (t == QLatin1String("key")) {
            notifyKeysym(op.value(QStringLiteral("keysym")).toInt(), state);
        } else if (t == QLatin1String("btn")) {
            notifyButton(op.value(QStringLiteral("button")).toInt(), state);
        }
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
    const auto queued = m_injectQueue;
    m_injectQueue.clear();
    for (const auto &pi : queued) {
        runInjectOps(pi.ops);
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
    if (!m_rdSession.isEmpty()) {
        releaseHeld();
        QDBusMessage msg = QDBusMessage::createMethodCall(
            QString::fromLatin1(kPortalService), m_rdSession,
            QStringLiteral("org.freedesktop.portal.Session"), QStringLiteral("Close"));
        QDBusConnection::sessionBus().asyncCall(msg);
    }
    m_rdSession.clear();
    m_rdReady = false;
    m_rdStarting = false;
    m_rdTypes = 0;
}

void CoworkPortal::teardownRemoteDesktop()
{
    m_idleTimer.stop();
    closeSessionOnly();
    failInjectQueue(i18n("desktop control was stopped"));
}

void CoworkPortal::handleKillInject(const QJsonObject &req)
{
    teardownRemoteDesktop();
    replyResult(req.value(QStringLiteral("corrId")).toString(), QStringLiteral("killInject"), true, QString());
}
