#include "LspClient.h"

#include <QCoreApplication>
#include <QDebug>
#include <QJsonDocument>
#include <QProcess>
#include <QUrl>

LspClient::LspClient(QObject *parent)
    : QObject(parent)
{
}

LspClient::~LspClient()
{
    if (m_proc && m_proc->state() != QProcess::NotRunning) {
        m_proc->terminate();
        if (!m_proc->waitForFinished(1500)) {
            m_proc->kill();
        }
    }
}

QString LspClient::uriFor(const QString &path) const
{
    return QUrl::fromLocalFile(path).toString();
}

void LspClient::start(const QString &command, const QStringList &args, const QString &rootPath)
{
    m_rootPath = rootPath;
    m_proc = new QProcess(this);
    m_proc->setProcessChannelMode(QProcess::SeparateChannels);

    connect(m_proc, &QProcess::readyReadStandardOutput, this, &LspClient::onStdout);
    connect(m_proc, &QProcess::errorOccurred, this, [this, command](QProcess::ProcessError) {
        qWarning().noquote() << "[lsp]" << command << "unavailable:" << m_proc->errorString();
    });
    connect(m_proc, &QProcess::started, this, [this] {
        QJsonObject capabilities{
            {QStringLiteral("textDocument"),
             QJsonObject{
                 {QStringLiteral("synchronization"), QJsonObject{{QStringLiteral("didSave"), false}}},
                 {QStringLiteral("publishDiagnostics"),
                  QJsonObject{{QStringLiteral("relatedInformation"), true}}}}},
            {QStringLiteral("workspace"), QJsonObject{}}};
        m_initializeId = m_nextId++;
        send(QJsonObject{
            {QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
            {QStringLiteral("id"), m_initializeId},
            {QStringLiteral("method"), QStringLiteral("initialize")},
            {QStringLiteral("params"),
             QJsonObject{{QStringLiteral("processId"),
                          static_cast<int>(QCoreApplication::applicationPid())},
                         {QStringLiteral("rootUri"), uriFor(m_rootPath)},
                         {QStringLiteral("capabilities"), capabilities}}}});
    });

    m_proc->start(command, args);
}

void LspClient::send(const QJsonObject &msg)
{
    if (!m_proc || m_proc->state() != QProcess::Running) {
        return;
    }
    const QByteArray json = QJsonDocument(msg).toJson(QJsonDocument::Compact);
    m_proc->write("Content-Length: " + QByteArray::number(json.size()) + "\r\n\r\n" + json);
}

void LspClient::notify(const QString &method, const QJsonObject &params)
{
    const QJsonObject msg{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                          {QStringLiteral("method"), method},
                          {QStringLiteral("params"), params}};
    if (m_ready) {
        send(msg);
    } else {
        m_queue.append(msg);
    }
}

void LspClient::onStdout()
{
    m_buf += m_proc->readAllStandardOutput();
    while (true) {
        const int headerEnd = m_buf.indexOf("\r\n\r\n");
        if (headerEnd < 0) {
            break;
        }
        int contentLength = -1;
        const QList<QByteArray> headers = m_buf.left(headerEnd).split('\n');
        for (const QByteArray &line : headers) {
            const QByteArray h = line.trimmed();
            if (h.toLower().startsWith("content-length:")) {
                contentLength = h.mid(h.indexOf(':') + 1).trimmed().toInt();
            }
        }
        const int bodyStart = headerEnd + 4;
        if (contentLength < 0) {
            m_buf.remove(0, bodyStart);
            continue;
        }
        if (m_buf.size() < bodyStart + contentLength) {
            break; // wait for the rest of the body
        }
        const QByteArray body = m_buf.mid(bodyStart, contentLength);
        m_buf.remove(0, bodyStart + contentLength);

        QJsonParseError err{};
        const QJsonDocument doc = QJsonDocument::fromJson(body, &err);
        if (err.error == QJsonParseError::NoError && doc.isObject()) {
            handleMessage(doc.object());
        }
    }
}

void LspClient::handleMessage(const QJsonObject &msg)
{
    const QString method = msg.value(QStringLiteral("method")).toString();

    // Response to one of our requests (id present, no method).
    if (method.isEmpty() && msg.contains(QStringLiteral("id"))) {
        const int id = msg.value(QStringLiteral("id")).toInt();
        if (!m_ready && id == m_initializeId) {
            m_ready = true;
            send(QJsonObject{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                             {QStringLiteral("method"), QStringLiteral("initialized")},
                             {QStringLiteral("params"), QJsonObject{}}});
            for (const QJsonObject &queued : m_queue) {
                send(queued);
            }
            m_queue.clear();
            return;
        }
        if (const auto callback = m_callbacks.take(id)) {
            callback(msg.value(QStringLiteral("result")));
        }
        return;
    }

    // Server-to-client request (id + method): it expects a reply.
    if (msg.contains(QStringLiteral("id"))) {
        QJsonValue result = QJsonValue::Null;
        if (method == QLatin1String("workspace/configuration")) {
            const QJsonArray items = msg.value(QStringLiteral("params"))
                                         .toObject()
                                         .value(QStringLiteral("items"))
                                         .toArray();
            QJsonArray nulls;
            for (int i = 0; i < items.size(); ++i) {
                nulls.append(QJsonValue::Null);
            }
            result = nulls;
        }
        send(QJsonObject{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                         {QStringLiteral("id"), msg.value(QStringLiteral("id"))},
                         {QStringLiteral("result"), result}});
        return;
    }

    // Notification.
    if (method == QLatin1String("textDocument/publishDiagnostics")) {
        const QJsonObject params = msg.value(QStringLiteral("params")).toObject();
        const QString path = QUrl(params.value(QStringLiteral("uri")).toString()).toLocalFile();
        emit diagnostics(path, params.value(QStringLiteral("diagnostics")).toArray());
    }
}

void LspClient::didOpen(const QString &path, const QString &languageId, const QString &text)
{
    m_versions[path] = 1;
    notify(QStringLiteral("textDocument/didOpen"),
           QJsonObject{{QStringLiteral("textDocument"),
                        QJsonObject{{QStringLiteral("uri"), uriFor(path)},
                                    {QStringLiteral("languageId"), languageId},
                                    {QStringLiteral("version"), 1},
                                    {QStringLiteral("text"), text}}}});
}

void LspClient::didChange(const QString &path, const QString &text)
{
    const int version = ++m_versions[path];
    notify(QStringLiteral("textDocument/didChange"),
           QJsonObject{
               {QStringLiteral("textDocument"),
                QJsonObject{{QStringLiteral("uri"), uriFor(path)},
                            {QStringLiteral("version"), version}}},
               // Full-document sync — the whole text on every change.
               {QStringLiteral("contentChanges"),
                QJsonArray{QJsonObject{{QStringLiteral("text"), text}}}}});
}

void LspClient::didClose(const QString &path)
{
    m_versions.remove(path);
    notify(QStringLiteral("textDocument/didClose"),
           QJsonObject{{QStringLiteral("textDocument"),
                        QJsonObject{{QStringLiteral("uri"), uriFor(path)}}}});
}

void LspClient::request(const QString &method, const QJsonObject &params,
                        std::function<void(const QJsonValue &)> callback)
{
    const int id = m_nextId++;
    if (callback) {
        m_callbacks.insert(id, std::move(callback));
    }
    const QJsonObject msg{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                          {QStringLiteral("id"), id},
                          {QStringLiteral("method"), method},
                          {QStringLiteral("params"), params}};
    if (m_ready) {
        send(msg);
    } else {
        m_queue.append(msg);
    }
}
