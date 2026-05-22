#pragma once

#include <QByteArray>
#include <QHash>
#include <QJsonObject>
#include <QObject>
#include <QString>

#include <functional>

class QLocalSocket;
class QProcess;

// CoreClient owns the akcore subprocess and the JSON-RPC connection to it.
// It spawns the core, connects over a Unix domain socket with retry, and
// exposes request/reply calls plus broadcast notifications from the core.
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
    void call(const QString &method, const QJsonObject &params, ReplyCallback cb = nullptr);

    bool isConnected() const;

Q_SIGNALS:
    void connected();
    void disconnected();
    void notification(const QString &method, const QJsonObject &params);
    void coreLog(const QString &line);
    void failed(const QString &message);

private:
    void tryConnect();
    void onSocketConnected();
    void onReadyRead();
    void handleFrame(const QJsonObject &frame);
    void send(const QJsonObject &frame);

    QProcess *m_proc = nullptr;
    QLocalSocket *m_socket = nullptr;
    QString m_socketPath;
    QByteArray m_buf;
    int m_nextId = 1;
    int m_connectAttempts = 0;
    QHash<int, ReplyCallback> m_pending;
};
