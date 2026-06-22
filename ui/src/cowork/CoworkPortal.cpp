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

CoworkPortal::CoworkPortal(CoreClient *core, QWidget *topLevel, QObject *parent)
    : QObject(parent), m_core(core), m_topLevel(topLevel)
{
    connect(m_core, &CoreClient::notification, this, &CoworkPortal::onNotification);
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

void CoworkPortal::handleInject(const QJsonObject &req)
{
    const QString corrId = req.value(QStringLiteral("corrId")).toString();
    const QJsonArray ops = req.value(QStringLiteral("ops")).toArray();

    if (m_rdReady && !m_rdSession.isEmpty()) {
        runInjectOps(ops);
        replyResult(corrId, QStringLiteral("inject"), true, QString());
        return;
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

    QVariantMap createOpts;
    createOpts.insert(QStringLiteral("session_handle_token"), sessToken);

    portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("CreateSession"),
                  {}, createOpts, [this](uint code, const QVariantMap &results) {
        if (code != 0) {
            failInjectQueue(i18n("the remote-control session could not be created"));
            return;
        }
        m_rdSession = results.value(QStringLiteral("session_handle")).toString();
        if (m_rdSession.isEmpty()) {
            failInjectQueue(i18n("no remote-control session handle was returned"));
            return;
        }
        // SelectDevices: keyboard (1) | pointer (2) = 3.
        QVariantMap selOpts;
        selOpts.insert(QStringLiteral("types"), uint(3));
        portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("SelectDevices"),
                      {QVariant::fromValue(QDBusObjectPath(m_rdSession))}, selOpts,
                      [this](uint code2, const QVariantMap &) {
            if (code2 != 0) {
                failInjectQueue(i18n("input devices were not granted"));
                return;
            }
            portalRequest(QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("Start"),
                          {QVariant::fromValue(QDBusObjectPath(m_rdSession)), parentWindowHandle()},
                          QVariantMap(), [this](uint code3, const QVariantMap &) {
                m_rdStarting = false;
                if (code3 != 0) {
                    failInjectQueue(i18n("remote control was declined"));
                    return;
                }
                m_rdReady = true;
                flushInjectQueue();
            });
        });
    });
}

void CoworkPortal::runInjectOps(const QJsonArray &ops)
{
    if (m_rdSession.isEmpty()) {
        return;
    }
    QDBusConnection bus = QDBusConnection::sessionBus();
    const QDBusObjectPath session(m_rdSession);
    for (const QJsonValue &ov : ops) {
        const QJsonObject op = ov.toObject();
        const QString t = op.value(QStringLiteral("t")).toString();
        QDBusMessage msg;
        if (t == QLatin1String("key")) {
            msg = QDBusMessage::createMethodCall(
                QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
                QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyKeyboardKeysym"));
            msg.setArguments({QVariant::fromValue(session), QVariant::fromValue(QVariantMap()),
                              op.value(QStringLiteral("keysym")).toInt(),
                              uint(op.value(QStringLiteral("state")).toInt())});
        } else if (t == QLatin1String("btn")) {
            msg = QDBusMessage::createMethodCall(
                QString::fromLatin1(kPortalService), QString::fromLatin1(kPortalPath),
                QStringLiteral("org.freedesktop.portal.RemoteDesktop"), QStringLiteral("NotifyPointerButton"));
            msg.setArguments({QVariant::fromValue(session), QVariant::fromValue(QVariantMap()),
                              op.value(QStringLiteral("button")).toInt(),
                              uint(op.value(QStringLiteral("state")).toInt())});
        } else {
            continue;
        }
        bus.asyncCall(msg); // fire-and-forget; Notify* return nothing meaningful
    }
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
    const auto queued = m_injectQueue;
    m_injectQueue.clear();
    for (const auto &pi : queued) {
        replyResult(pi.corrId, QStringLiteral("inject"), false, err);
    }
}

void CoworkPortal::teardownRemoteDesktop()
{
    if (!m_rdSession.isEmpty()) {
        QDBusMessage msg = QDBusMessage::createMethodCall(
            QString::fromLatin1(kPortalService), m_rdSession,
            QStringLiteral("org.freedesktop.portal.Session"), QStringLiteral("Close"));
        QDBusConnection::sessionBus().asyncCall(msg);
    }
    failInjectQueue(i18n("desktop control was stopped"));
}

void CoworkPortal::handleKillInject(const QJsonObject &req)
{
    teardownRemoteDesktop();
    replyResult(req.value(QStringLiteral("corrId")).toString(), QStringLiteral("killInject"), true, QString());
}
