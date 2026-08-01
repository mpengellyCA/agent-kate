#pragma once

#include <QByteArray>
#include <QHash>
#include <QJsonObject>
#include <QObject>
#include <QPointer>
#include <QString>

#include <functional>

class QLocalSocket;
class QProcess;
class QTimer;

// CoreClient owns the akcore subprocess and the JSON-RPC connection to it.
// It spawns the core, connects over a Unix domain socket with retry, and
// exposes request/reply calls plus broadcast notifications from the core.
//
// A drop that was not asked for is recovered from — the core is respawned if it
// died — over a bounded ladder, because without it every later action in the
// window fails with "disconnected from core" until the app is restarted.
// connected() is therefore emitted once per connection, not once per run.
class CoreClient : public QObject
{
    Q_OBJECT
public:
    // Invoked with (result, error); exactly one is non-empty.
    using ReplyCallback =
        std::function<void(const QJsonObject &result, const QJsonObject &error)>;

    explicit CoreClient(QObject *parent = nullptr);
    ~CoreClient() override;

    // Spawn akcore (binary at coreBinaryPath) and connect to it.
    void start(const QString &coreBinaryPath);

    // Issue a JSON-RPC request. cb may be null for fire-and-forget.
    //
    // If context is non-null, the reply callback is lifetime-guarded: should the
    // context QObject be destroyed before the reply arrives, the callback is
    // DROPPED (never invoked). Pass the object that owns the captured state — a
    // transient widget, model, etc. — so a late reply cannot touch freed memory.
    // A null context (the default) keeps the legacy behaviour: always invoked.
    void call(const QString &method, const QJsonObject &params, ReplyCallback cb = nullptr,
              QObject *context = nullptr);

    bool isConnected() const;

    // Deliver an agent event to the notification consumers as if the core had
    // sent it. Strictly for facts the UI holds and the core cannot state: after
    // a respawn the fresh core has never heard of the previous one's threads, so
    // nothing would ever report that they ended. Never use it for anything the
    // core is the authority on.
    void deliverLocalEvent(const QString &threadId, const QJsonObject &event);

Q_SIGNALS:
    void connected();
    void disconnected();
    void notification(const QString &method, const QJsonObject &params);
    void coreLog(const QString &line);
    void failed(const QString &message);

    // One request refused before it was written, because the frame would exceed
    // the core's inbound cap. NOT a failed(): the connection is unaffected — a
    // message the human composed was dropped, which is worth saying out loud.
    void sendRefused(const QString &method, const QString &reason);

    // Recovery lifecycle after an *unexpected* drop (never during an intentional
    // shutdown). reconnecting() is followed by exactly one of reconnected() —
    // which arrives after connected(), the connection being fully live again —
    // or reconnectFailed(), after which the client stays dead until app restart.
    //
    // coreRespawned separates the two recoveries, which are not the same event:
    // false = the same core was there all along, so everything it was running
    // still is; true = the core process was gone and a fresh one was launched,
    // so every thread the UI is showing died with the old one and now exists
    // only as a session on disk. Consumers MUST reconcile on true — the fresh
    // core will never report an exit for a thread it never started.
    void reconnecting();
    void reconnected(bool coreRespawned);
    void reconnectFailed();

private:
    void tryConnect();
    void onSocketConnected();
    void onDisconnected();
    void onReadyRead();
    void handleFrame(const QJsonObject &frame);
    // Serialises and writes frame. Returns false without writing when the socket
    // is down or the frame is over the core's inbound cap; *error (if given) then
    // holds the caller-facing reason, and an oversize frame also raises
    // sendRefused so the window can say a message was dropped.
    bool send(const QJsonObject &frame, QString *error = nullptr);

    // Start (or restart) the already-configured akcore process. False means the
    // binary would not launch; failed() has been emitted.
    bool launchCore();
    // Recovery ladder: begin arms it (once), schedule waits out the backoff for
    // the next round, attempt respawns the core if it died and re-drives the
    // connect loop. The handshake reply closes the loop with reconnected(), and
    // a handshake that never comes is timed out by attempt into another round.
    void beginReconnect();
    void scheduleReconnect();
    void attemptReconnect();

    QProcess *m_proc = nullptr;
    QLocalSocket *m_socket = nullptr;
    QString m_socketPath;
    QByteArray m_buf;
    int m_nextId = 1;
    int m_connectAttempts = 0;

    // Set once the app is on its way out (app.shutdown issued, qApp quitting, or
    // this client being destroyed). The core dropping after that is expected, so
    // it must not trigger a recovery that would fight the quit.
    bool m_shuttingDown = false;
    // True from the drop until reconnected()/reconnectFailed(). The armed timer,
    // not this flag, is what keeps a second drop from starting a rival ladder.
    bool m_reconnecting = false;
    int m_reconnectAttempts = 0;
    // True once THIS recovery has had to launch a fresh core, carried until the
    // reconnected() that closes the ladder can report it. Reset per ladder, not
    // per round: a respawn in round 1 is still true of a round 3 that lands.
    bool m_coreRespawned = false;
    // True while a recovery round's handshake is outstanding and the timer below
    // is watching it rather than counting a backoff.
    bool m_handshakeWatchdog = false;
    QTimer *m_reconnectTimer = nullptr; // single-shot: one backoff, or one handshake

    // A pending reply slot. When guarded, the callback is invoked only if its
    // context is still alive; a null context after destruction means DROP.
    struct PendingReply {
        ReplyCallback cb;
        QPointer<QObject> context;
        bool guarded = false;
    };
    QHash<int, PendingReply> m_pending;
};
