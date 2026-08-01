#include "ipc/CoreClient.h"

#include "ipc/SocketPath.h"

#include <QCoreApplication>
#include <QDir>
#include <QJsonArray>
#include <QJsonDocument>
#include <QLocalSocket>
#include <QProcess>
#include <QRegularExpression>
#include <QTimer>

namespace {
constexpr int kMaxConnectAttempts = 30; // ~6s at 200ms spacing
constexpr int kConnectRetryMs = 200;
// Per recovery round the connect loop is kept short (~2s): giving up here hands
// back to the outer ladder, which can respawn a core that is gone for good —
// spinning the socket for six seconds against a dead process only delays that.
constexpr int kReconnectConnectAttempts = 10;
constexpr int kMaxReconnectAttempts = 5;
constexpr int kReconnectBackoffMs = 500; // doubled each round: 0.5s … 8s
// How long a recovery round waits for its handshake reply before treating the
// connection as dead again. Generous next to a fresh core's startup, and far
// short of leaving the ladder — and the banner — hanging for good.
constexpr int kHandshakeTimeoutMs = 5000;

// The core survives an over-long frame: it discards the line and answers it
// with an error rather than dropping the connection (16 MiB cap,
// core/internal/ipc/server.go). Crossing this line therefore costs the request,
// not every agent's link — but it costs it only after the whole payload has
// been serialised, written and read for nothing, so a frame that would cross it
// is never written. Refusing up front beats spending the round trip to be told
// the same thing. A megabyte of slack keeps us clear of the core counting the
// same bytes slightly differently.
constexpr qsizetype kMaxFrameBytes = 15 * 1024 * 1024;

// A JSON-RPC error object delivered to a reply callback that can never be
// answered by the core (send failed, or the connection dropped while pending).
QJsonObject localError(const QString &message)
{
    return QJsonObject{
        {QStringLiteral("code"), -32000},
        {QStringLiteral("message"), message},
    };
}

// Recovers the JSON-RPC id from the retained head of a frame that was too large
// to keep. Deliberately a text probe and not a JSON parse: the head is a
// truncated object, so it never parses — but the id is written near the front
// of every reply the core sends, so this finds it in practice. No id found
// means the pending call is simply left to the connection's own teardown.
int probeFrameId(const QByteArray &head)
{
    static const QRegularExpression re(QStringLiteral("\"id\"\\s*:\\s*(\\d+)"));
    const auto m = re.match(QString::fromUtf8(head));
    if (!m.hasMatch()) {
        return -1;
    }
    bool ok = false;
    const int id = m.captured(1).toInt(&ok);
    return ok ? id : -1;
}
} // namespace

CoreClient::CoreClient(QObject *parent)
    : QObject(parent)
    , m_socket(new QLocalSocket(this))
    , m_reconnectTimer(new QTimer(this))
{
    m_reconnectTimer->setSingleShot(true);
    connect(m_reconnectTimer, &QTimer::timeout, this, &CoreClient::attemptReconnect);

    connect(m_socket, &QLocalSocket::connected, this, &CoreClient::onSocketConnected);
    connect(m_socket, &QLocalSocket::readyRead, this, &CoreClient::onReadyRead);
    connect(m_socket, &QLocalSocket::disconnected, this, &CoreClient::onDisconnected);
    connect(m_socket, &QLocalSocket::errorOccurred, this,
            [this](QLocalSocket::LocalSocketError) {
                // Same first-statement gate as the ladder functions: once the
                // app is going down the socket is meant to fail, and spending
                // six seconds of retries on it only ends in a failed() nobody
                // asked for.
                if (m_shuttingDown) {
                    return;
                }
                if (m_socket->state() == QLocalSocket::ConnectedState) {
                    return;
                }
                const int cap =
                    m_reconnecting ? kReconnectConnectAttempts : kMaxConnectAttempts;
                if (++m_connectAttempts > cap) {
                    if (m_reconnecting) {
                        scheduleReconnect();
                        return;
                    }
                    emit failed(QStringLiteral("could not connect to akcore at %1")
                                    .arg(m_socketPath));
                    return;
                }
                QTimer::singleShot(kConnectRetryMs, this, &CoreClient::tryConnect);
            });

    // The quit path tears the core down on purpose; from aboutToQuit onwards a
    // dropped socket is the expected outcome, not something to recover from.
    if (QCoreApplication *app = QCoreApplication::instance()) {
        connect(app, &QCoreApplication::aboutToQuit, this,
                [this] { m_shuttingDown = true; });
    }
}

CoreClient::~CoreClient()
{
    m_shuttingDown = true;
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
    // The core refuses to bind a socket in a directory it cannot vouch for
    // (ipc.assertPrivateDir). Picking the path here with the SAME rules means a
    // rejection surfaces as one clear message now, rather than as a core that
    // exits on startup and a UI that spends six seconds connecting to nothing.
    QString pathError;
    m_socketPath = akipc::privateSocketPath(&pathError);
    if (m_socketPath.isEmpty()) {
        emit failed(QStringLiteral("cannot start the core: %1").arg(pathError));
        return;
    }

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

    if (!launchCore()) {
        return;
    }

    m_connectAttempts = 0;
    QTimer::singleShot(kConnectRetryMs, this, &CoreClient::tryConnect);
}

bool CoreClient::launchCore()
{
    if (!m_proc) {
        return false;
    }
    m_proc->start();
    if (!m_proc->waitForStarted(3000)) {
        emit failed(
            QStringLiteral("failed to launch akcore (%1)").arg(m_proc->program()));
        return false;
    }
    return true;
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
    //
    // A recovery round is over only when that reply lands, so during one it is
    // timed: a fresh core that accepts the socket and then never answers would
    // otherwise leave m_reconnecting set for good — the banner stuck, neither
    // reconnected() nor reconnectFailed() ever emitted, in breach of the
    // contract this class documents. Armed BEFORE the call, so a send that fails
    // synchronously disarms it through the same reply path.
    if (m_reconnecting) {
        m_handshakeWatchdog = true;
        m_reconnectTimer->start(kHandshakeTimeoutMs);
    }
    call(QStringLiteral("handshake"), {}, [this](const QJsonObject &, const QJsonObject &) {
        // However the reply lands — answered, errored, or drained by a drop —
        // the handshake is settled, so the watchdog must not outlive it.
        if (m_handshakeWatchdog) {
            m_handshakeWatchdog = false;
            m_reconnectTimer->stop();
        }
        // Except when the "error" is the drop drain: a socket that died with the
        // handshake still in flight must not announce a connection that is gone —
        // it would fire ahead of the recovery banner and contradict it. (The drain
        // runs before beginReconnect, so stopping the timer above hands that path
        // an idle timer to re-arm rather than one it would decline to touch.)
        if (!isConnected()) {
            return;
        }
        emit connected();
        if (m_reconnecting) {
            m_reconnecting = false;
            m_reconnectAttempts = 0;
            const bool respawned = m_coreRespawned;
            m_coreRespawned = false;
            emit reconnected(respawned);
        }
    });
}

void CoreClient::deliverLocalEvent(const QString &threadId, const QJsonObject &event)
{
    if (threadId.isEmpty() || event.isEmpty()) {
        return;
    }
    emit notification(QStringLiteral("agent.event"),
                      QJsonObject{
                          {QStringLiteral("threadId"), threadId},
                          {QStringLiteral("events"), QJsonArray{event}},
                      });
}

bool CoreClient::isConnected() const
{
    return m_socket->state() == QLocalSocket::ConnectedState;
}

void CoreClient::call(const QString &method, const QJsonObject &params, ReplyCallback cb,
                      QObject *context)
{
    // app.shutdown is the app asking the core to stop: everything that follows —
    // the reply, the drained agents, the socket dying — is intentional, so no
    // recovery may start from here on.
    if (method == QLatin1String("app.shutdown")) {
        m_shuttingDown = true;
    }

    const int id = m_nextId++;
    const bool hadCb = static_cast<bool>(cb);
    if (cb) {
        m_pending.insert(id, PendingReply{std::move(cb), QPointer<QObject>(context),
                                          context != nullptr});
    }
    QString sendError;
    const bool sent = send(QJsonObject{
                              {QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                              {QStringLiteral("id"), id},
                              {QStringLiteral("method"), method},
                              {QStringLiteral("params"), params},
                          },
                          &sendError);
    if (!sent && hadCb) {
        // The frame never left the process (socket down, or too large to send).
        // Resolve the pending callback now with an error rather than leak the
        // closure until a reply that can never arrive — every caller already
        // handles the error arg.
        const PendingReply pending = m_pending.take(id);
        if (pending.cb && !(pending.guarded && pending.context.isNull())) {
            pending.cb({}, localError(sendError));
        }
    }
}

bool CoreClient::send(const QJsonObject &frame, QString *error)
{
    if (m_socket->state() != QLocalSocket::ConnectedState) {
        if (error) {
            *error = QStringLiteral("not connected to core");
        }
        return false;
    }
    QByteArray data = QJsonDocument(frame).toJson(QJsonDocument::Compact);
    data.append('\n');
    if (data.size() > kMaxFrameBytes) {
        const QString message =
            QStringLiteral("message too large for the core connection (%1 MB); "
                           "the request was not sent")
                .arg(data.size() / (1024.0 * 1024.0), 0, 'f', 1);
        if (error) {
            *error = message;
        }
        // Surfaces the drop in the window: a fire-and-forget send has no reply
        // callback to carry the error back to its caller, and even one that does
        // has just lost content the human wrote — a qWarning is not where that
        // belongs. Not failed(): nothing is wrong with the connection.
        emit sendRefused(frame.value(QStringLiteral("method")).toString(), message);
        return false;
    }
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
    m_reader.clear();
    for (auto it = pending.constBegin(); it != pending.constEnd(); ++it) {
        const PendingReply &p = it.value();
        if (!p.cb || (p.guarded && p.context.isNull())) {
            continue;
        }
        p.cb({}, localError(QStringLiteral("disconnected from core")));
    }
    emit disconnected();
    beginReconnect();
}

void CoreClient::beginReconnect()
{
    // Nothing to recover to before start() built the process, and nothing worth
    // recovering while the app is quitting.
    if (m_shuttingDown || !m_proc) {
        return;
    }
    if (!m_reconnecting) {
        m_reconnecting = true;
        m_reconnectAttempts = 0;
        m_coreRespawned = false; // a fresh ladder has respawned nothing yet
        emit reconnecting();
    }
    // The armed timer — not m_reconnecting — is the re-entrancy guard, so a drop
    // that lands mid-ladder joins the round already in flight instead of running
    // a second one, while a drop with nothing armed (the socket had come back but
    // the handshake never answered) still re-arms rather than wedging.
    if (!m_reconnectTimer->isActive()) {
        scheduleReconnect();
    }
}

void CoreClient::scheduleReconnect()
{
    if (m_shuttingDown || !m_reconnecting) {
        return;
    }
    if (++m_reconnectAttempts > kMaxReconnectAttempts) {
        // Out of rounds: fall back into the plain dead state, where every call
        // fails locally, rather than retrying forever behind the user's back.
        m_reconnecting = false;
        emit reconnectFailed();
        return;
    }
    m_reconnectTimer->start(kReconnectBackoffMs << (m_reconnectAttempts - 1));
}

void CoreClient::attemptReconnect()
{
    if (m_shuttingDown || !m_reconnecting) {
        return;
    }
    if (isConnected()) {
        if (!m_handshakeWatchdog) {
            // The socket is back and the handshake is still in flight; its reply
            // ends the ladder. Leaving the timer idle is safe — another drop
            // re-arms it.
            return;
        }
        // The watchdog expired: something is holding the socket open without
        // serving us. Drop it and spend another round rather than wait forever.
        // abort() takes the disconnect path, which re-arms the ladder itself —
        // so only arm here if it did not, or the round would be counted twice.
        m_handshakeWatchdog = false;
        m_socket->abort();
        if (!m_reconnectTimer->isActive()) {
            scheduleReconnect();
        }
        return;
    }
    if (m_proc->state() == QProcess::NotRunning) {
        // The core is gone, not merely unreachable — a fresh one is the only way
        // back. Its agents died with it; what is being restored is the UI's
        // ability to act at all. QProcess keeps program, arguments and the log
        // wiring from start(), so a bare start() is the whole respawn.
        if (!launchCore()) {
            scheduleReconnect();
            return;
        }
        // The fresh core has never heard of the threads the UI is showing, and
        // will never report their end. Only the reconnected() that closes this
        // ladder can tell the window that, so the fact has to survive until then.
        m_coreRespawned = true;
    }
    // Hand back to the connect loop, which re-runs the handshake through
    // onSocketConnected and closes the ladder with reconnected().
    m_connectAttempts = 0;
    tryConnect();
}

void CoreClient::onReadyRead()
{
    m_reader.append(m_socket->readAll());
    akipc::FrameReader::Frame f;
    while (m_reader.next(&f)) {
        if (f.oversize > 0) {
            // Over the inbound cap: the frame is gone, the connection is not.
            // A caller waiting on it would otherwise hold its closure forever,
            // so resolve it here when the retained head still names it.
            const QString message =
                QStringLiteral("the core sent a message too large to receive "
                               "(%1 MB, cap %2 MB); it was discarded")
                    .arg(f.oversize / (1024.0 * 1024.0), 0, 'f', 1)
                    .arg(akipc::kMaxInboundFrameBytes / (1024 * 1024));
            const int id = probeFrameId(f.probe);
            if (id >= 0 && m_pending.contains(id)) {
                const PendingReply pending = m_pending.take(id);
                if (pending.cb && !(pending.guarded && pending.context.isNull())) {
                    pending.cb({}, localError(message));
                }
            }
            emit failed(message);
            continue;
        }
        QJsonParseError err{};
        const QJsonDocument doc = QJsonDocument::fromJson(f.line, &err);
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
