#pragma once

#include <QByteArray>
#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QList>
#include <QObject>
#include <QString>
#include <QStringList>

#include <functional>

class QProcess;

// LspClient drives one language-server process: JSON-RPC over stdio with
// Content-Length framing, the initialize handshake, document synchronisation,
// and publishDiagnostics. M3 increment 1 covers diagnostics only.
class LspClient : public QObject
{
    Q_OBJECT
public:
    explicit LspClient(QObject *parent = nullptr);
    ~LspClient() override;

    void start(const QString &command, const QStringList &args, const QString &rootPath);
    void didOpen(const QString &path, const QString &languageId, const QString &text);
    void didChange(const QString &path, const QString &text);
    void didClose(const QString &path);

    // Issue an LSP request; the callback receives the response "result".
    void request(const QString &method, const QJsonObject &params,
                 std::function<void(const QJsonValue &)> callback);

Q_SIGNALS:
    // Diagnostics for one file, as the raw LSP diagnostics array.
    void diagnostics(const QString &path, const QJsonArray &items);

private:
    void send(const QJsonObject &msg);
    void notify(const QString &method, const QJsonObject &params);
    void onStdout();
    void handleMessage(const QJsonObject &msg);
    QString uriFor(const QString &path) const;

    QProcess *m_proc = nullptr;
    QByteArray m_buf;
    int m_nextId = 1;
    int m_initializeId = 0;
    bool m_ready = false;
    QString m_rootPath;
    QHash<QString, int> m_versions;   // path -> document version
    QList<QJsonObject> m_queue;       // messages held until initialised
    QHash<int, std::function<void(const QJsonValue &)>> m_callbacks; // pending requests
};
