#include "ipc/CoreClient.h"

#include <QCoreApplication>
#include <QDir>
#include <QJsonDocument>
#include <QLocalSocket>
#include <QProcess>
#include <QTimer>

namespace {
constexpr int kMaxConnectAttempts = 30; // ~6s at 200ms spacing
constexpr int kConnectRetryMs = 200;

// A JSON-RPC error object delivered to a reply callback that can never be
// answered by the core (send failed, or the connection dropped while pending).
QJsonObject localError(const QString &message)
{
    return QJsonObject{
        {QStringLiteral("code"), -32000},
        {QStringLiteral("message"), message},
    };
}

// Mirrors akcore's default socket location, made unique per UI process.
QString runtimeSocketPath()
{
    QString dir = qEnvironmentVariable("XDG_RUNTIME_DIR");
    if (dir.isEmpty()) {
        dir = QDir::tempPath();
    }
    return QDir(dir).filePath(
        QStringLiteral("agentkate-%1.sock").arg(QCoreApplication::applicationPid()));
}
} // namespace

CoreClient::CoreClient(QObject *parent)
    : QObject(parent)
    , m_socket(new QLocalSocket(this))
{
    connect(m_socket, &QLocalSocket::connected, this, &CoreClient::onSocketConnected);
    connect(m_socket, &QLocalSocket::readyRead, this, &CoreClient::onReadyRead);
    connect(m_socket, &QLocalSocket::disconnected, this, &CoreClient::onDisconnected);
    connect(m_socket, &QLocalSocket::errorOccurred, this,
            [this](QLocalSocket::LocalSocketError) {
                if (m_socket->state() == QLocalSocket::ConnectedState) {
                    return;
                }
                if (++m_connectAttempts > kMaxConnectAttempts) {
                    emit failed(QStringLiteral("could not connect to akcore at %1")
                                    .arg(m_socketPath));
                    return;
                }
                QTimer::singleShot(kConnectRetryMs, this, &CoreClient::tryConnect);
            });
}

CoreClient::~CoreClient()
{
    if (m_proc && m_proc->state() != QProcess::NotRunning) {
        // SIGTERM starts the core's graceful shutdown, which drains any pending
        // cold-exit compactions before exiting. We MUST out-wait that drain, or
        // the SIGKILL below cuts the core off mid-compaction and the summary
        // goes missing. This grace stays a few seconds above the core's
        // exitCompactCap (akcore main.go); keep the two in sync if either moves.
        m_proc->terminate();
        if (!m_proc->waitForFinished(18000)) {
            m_proc->kill();
            m_proc->waitForFinished(1000);
        }
    }
}

void CoreClient::start(const QString &coreBinaryPath)
{
    m_socketPath = runtimeSocketPath();

    m_proc = new QProcess(this);
    m_proc->setProgram(coreBinaryPath);
    m_proc->setArguments({QStringLiteral("--socket"), m_socketPath});

    connect(m_proc, &QProcess::readyReadStandardError, this, [this] {
        const QString text = QString::fromUtf8(m_proc->readAllStandardError());
        const auto lines = text.split(QLatin1Char('\n'), Qt::SkipEmptyParts);
        for (const QString &line : lines) {
            emit coreLog(line);
        }
    });
    connect(m_proc, &QProcess::errorOccurred, this, [this](QProcess::ProcessError) {
        emit failed(QStringLiteral("akcore process error: %1").arg(m_proc->errorString()));
    });

    m_proc->start();
    if (!m_proc->waitForStarted(3000)) {
        emit failed(QStringLiteral("failed to launch akcore (%1)").arg(coreBinaryPath));
        return;
    }

    m_connectAttempts = 0;
    QTimer::singleShot(kConnectRetryMs, this, &CoreClient::tryConnect);
}

void CoreClient::tryConnect()
{
    if (m_socket->state() != QLocalSocket::UnconnectedState) {
        return;
    }
    m_socket->connectToServer(m_socketPath);
}

void CoreClient::onSocketConnected()
{
    m_connectAttempts = 0;
    // Claim the UI role and only announce the connection once it is established. The core
    // dispatches each request on its own goroutine, so merely sending the handshake first
    // does not guarantee it is *processed* before the panels' UI-only queries (getPolicy,
    // listGrants, listAudit, …) that fire on connected(). Waiting for the handshake reply
    // does: the reply is sent after the connection is marked "ui", so every connected()
    // consumer runs after the role is set. Without this, those queries race the handshake,
    // are rejected, and panels are left empty until some later notification refreshes them
    // — which for the Cowork capability toggles never happened (only a policy change
    // re-fetches them), so the switchboard stayed blank. The callback fires on both success
    // and error, so a handshake failure still surfaces the connection rather than hanging.
    call(QStringLiteral("handshake"), {}, [this](const QJsonObject &, const QJsonObject &) {
        emit connected();
    });
}

bool CoreClient::isConnected() const
{
    return m_socket->state() == QLocalSocket::ConnectedState;
}

void CoreClient::call(const QString &method, const QJsonObject &params, ReplyCallback cb,
                      QObject *context)
{
    const int id = m_nextId++;
    const bool hadCb = static_cast<bool>(cb);
    if (cb) {
        m_pending.insert(id, PendingReply{std::move(cb), QPointer<QObject>(context),
                                          context != nullptr});
    }
    const bool sent = send(QJsonObject{
        {QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
        {QStringLiteral("id"), id},
        {QStringLiteral("method"), method},
        {QStringLiteral("params"), params},
    });
    if (!sent && hadCb) {
        // The frame never left the process (socket not connected). Resolve the
        // pending callback now with an error rather than leak the closure until a
        // reply that can never arrive — every caller already handles the error arg.
        const PendingReply pending = m_pending.take(id);
        if (pending.cb && !(pending.guarded && pending.context.isNull())) {
            pending.cb({}, localError(QStringLiteral("not connected to core")));
        }
    }
}

bool CoreClient::send(const QJsonObject &frame)
{
    if (m_socket->state() != QLocalSocket::ConnectedState) {
        return false;
    }
    QByteArray data = QJsonDocument(frame).toJson(QJsonDocument::Compact);
    data.append('\n');
    m_socket->write(data);
    m_socket->flush();
    return true;
}

void CoreClient::onDisconnected()
{
    // Drain every outstanding reply so closures are not leaked and no caller is
    // left awaiting a reply that can never come. Guarded callbacks whose context
    // has already been destroyed are dropped (their captured state is gone); the
    // rest are invoked once with a synthetic error. Iterate over a copy: a callback
    // may re-enter call() and insert into m_pending. Also reset the read buffer so a
    // half-frame left mid-stream cannot corrupt the first frame after a reconnect.
    const QHash<int, PendingReply> pending = m_pending;
    m_pending.clear();
    m_buf.clear();
    for (auto it = pending.constBegin(); it != pending.constEnd(); ++it) {
        const PendingReply &p = it.value();
        if (!p.cb || (p.guarded && p.context.isNull())) {
            continue;
        }
        p.cb({}, localError(QStringLiteral("disconnected from core")));
    }
    emit disconnected();
}

void CoreClient::onReadyRead()
{
    m_buf.append(m_socket->readAll());
    int nl;
    while ((nl = m_buf.indexOf('\n')) >= 0) {
        const QByteArray line = m_buf.left(nl);
        m_buf.remove(0, nl + 1);
        if (line.trimmed().isEmpty()) {
            continue;
        }
        QJsonParseError err{};
        const QJsonDocument doc = QJsonDocument::fromJson(line, &err);
        if (err.error != QJsonParseError::NoError || !doc.isObject()) {
            emit failed(QStringLiteral("malformed frame from core: %1").arg(err.errorString()));
            continue;
        }
        handleFrame(doc.object());
    }
}

void CoreClient::handleFrame(const QJsonObject &frame)
{
    // Response: carries an id plus a result or error.
    if (frame.contains(QStringLiteral("id"))
        && (frame.contains(QStringLiteral("result"))
            || frame.contains(QStringLiteral("error")))) {
        const int id = frame.value(QStringLiteral("id")).toInt();
        const PendingReply pending = m_pending.take(id);
        if (pending.cb) {
            // Lifetime guard: if the registrant supplied a context that has since
            // been destroyed, drop the reply rather than dereference freed state.
            if (pending.guarded && pending.context.isNull()) {
                return;
            }
            pending.cb(frame.value(QStringLiteral("result")).toObject(),
                       frame.value(QStringLiteral("error")).toObject());
        }
        return;
    }
    // Notification: carries a method and no id.
    if (frame.contains(QStringLiteral("method"))) {
        emit notification(frame.value(QStringLiteral("method")).toString(),
                          frame.value(QStringLiteral("params")).toObject());
    }
}
