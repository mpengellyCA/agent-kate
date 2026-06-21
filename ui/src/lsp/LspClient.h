#pragma once

#include <QByteArray>
#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QList>
#include <QObject>
#include <QPointer>
#include <QString>
#include <QStringList>

#include <functional>

class QProcess;

// LspClient drives one language-server process: JSON-RPC over stdio with
// Content-Length framing, the initialize handshake, document synchronisation,
// publishDiagnostics, and server→client requests (workspace/applyEdit,
// workspace/configuration, $/progress). It retains the server's advertised
// capabilities so the manager can gate features.
class LspClient : public QObject
{
    Q_OBJECT
public:
    explicit LspClient(QObject *parent = nullptr);
    ~LspClient() override;

    // Lifecycle state of the underlying server process.
    enum class State { Starting, Running, Crashed };

    void start(const QString &command, const QStringList &args, const QString &rootPath);
    // Ask the server to shut down cleanly (shutdown + exit), then terminate.
    void stop();
    void didOpen(const QString &path, const QString &languageId, const QString &text);
    void didChange(const QString &path, const QString &text);
    void didSave(const QString &path);
    void didClose(const QString &path);

    // Issue an LSP request; the callback receives the response "result".
    //
    // If context is non-null, the callback is lifetime-guarded: should the
    // context QObject be destroyed before the response arrives, the callback is
    // DROPPED (never invoked). Pass the object that owns the captured state so a
    // late response cannot touch freed memory. A null context (the default)
    // keeps the legacy behaviour: always invoked.
    void request(const QString &method, const QJsonObject &params,
                 std::function<void(const QJsonValue &)> callback,
                 QObject *context = nullptr);

    // The server's advertised capabilities (from the initialize result).
    const QJsonObject &capabilities() const { return m_serverCaps; }

    State state() const { return m_state; }
    QString uriFor(const QString &path) const;

Q_SIGNALS:
    // Diagnostics for one file, as the raw LSP diagnostics array.
    void diagnostics(const QString &path, const QJsonArray &items);
    // A workspace/applyEdit arrived from the server; the manager applies it.
    void applyEditRequested(const QJsonObject &workspaceEdit);
    // Server lifecycle changed (started / crashed).
    void stateChanged(State state);
    // Latest $/progress report — a one-line human title/message ("" when done).
    void progress(const QString &text);

private:
    void send(const QJsonObject &msg);
    void notify(const QString &method, const QJsonObject &params);
    void onStdout();
    void handleMessage(const QJsonObject &msg);
    void setState(State state);

    QProcess *m_proc = nullptr;
    QByteArray m_buf;
    int m_nextId = 1;
    int m_initializeId = 0;
    bool m_ready = false;
    State m_state = State::Starting;
    QString m_command;
    QString m_rootPath;
    QJsonObject m_serverCaps;          // initialize result capabilities
    QHash<QString, int> m_versions;   // path -> document version
    QList<QJsonObject> m_queue;       // messages held until initialised

    // A pending request slot. When guarded, the callback is invoked only if its
    // context is still alive; a null context after destruction means DROP.
    struct PendingCallback {
        std::function<void(const QJsonValue &)> cb;
        QPointer<QObject> context;
        bool guarded = false;
    };
    QHash<int, PendingCallback> m_callbacks; // pending requests
};
