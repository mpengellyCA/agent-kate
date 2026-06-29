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
        stop();
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

void LspClient::setState(State state)
{
    if (m_state == state) {
        return;
    }
    m_state = state;
    emit stateChanged(m_state);
}

void LspClient::flushPending()
{
    // The server is gone, so no response will ever arrive for these. Resolve each
    // live callback with a null result (callers already treat a non-array/empty
    // result as "no data"; format-on-save then proceeds to save unformatted rather
    // than dropping the save) and drop queued requests that were never sent.
    // Iterate over a copy: a callback may re-enter request() and touch m_callbacks.
    const QHash<int, PendingCallback> pending = m_callbacks;
    m_callbacks.clear();
    m_queue.clear();
    for (auto it = pending.constBegin(); it != pending.constEnd(); ++it) {
        const PendingCallback &p = it.value();
        if (!p.cb || (p.guarded && p.context.isNull())) {
            continue;
        }
        p.cb(QJsonValue());
    }
}

void LspClient::start(const QString &command, const QStringList &args, const QString &rootPath)
{
    m_command = command;
    m_rootPath = rootPath;
    m_proc = new QProcess(this);
    m_proc->setProcessChannelMode(QProcess::SeparateChannels);

    connect(m_proc, &QProcess::readyReadStandardOutput, this, &LspClient::onStdout);
    connect(m_proc, &QProcess::errorOccurred, this, [this, command](QProcess::ProcessError) {
        qWarning().noquote() << "[lsp]" << command << "unavailable:" << m_proc->errorString();
        setState(State::Crashed);
        flushPending();
    });
    connect(m_proc, &QProcess::finished, this,
            [this](int, QProcess::ExitStatus status) {
                if (status == QProcess::CrashExit) {
                    setState(State::Crashed);
                }
                // Whether it crashed or exited cleanly, no further responses will
                // arrive — resolve anything still pending so awaiting callers (e.g.
                // format-on-save) are not wedged.
                flushPending();
            });
    connect(m_proc, &QProcess::started, this, [this] {
        // Advertise the client features the richer code-intelligence relies on.
        const QJsonObject completionItem{
            {QStringLiteral("snippetSupport"), true},
            {QStringLiteral("documentationFormat"),
             QJsonArray{QStringLiteral("markdown"), QStringLiteral("plaintext")}},
            {QStringLiteral("resolveSupport"),
             QJsonObject{{QStringLiteral("properties"),
                          QJsonArray{QStringLiteral("documentation"),
                                     QStringLiteral("detail"),
                                     QStringLiteral("additionalTextEdits")}}}}};
        const QJsonObject textDocument{
            {QStringLiteral("synchronization"),
             QJsonObject{{QStringLiteral("didSave"), true}}},
            {QStringLiteral("publishDiagnostics"),
             QJsonObject{{QStringLiteral("relatedInformation"), true}}},
            {QStringLiteral("completion"),
             QJsonObject{{QStringLiteral("completionItem"), completionItem}}},
            {QStringLiteral("hover"),
             QJsonObject{{QStringLiteral("contentFormat"),
                          QJsonArray{QStringLiteral("markdown"),
                                     QStringLiteral("plaintext")}}}},
            {QStringLiteral("signatureHelp"), QJsonObject{}},
            {QStringLiteral("codeAction"),
             QJsonObject{{QStringLiteral("dynamicRegistration"), false}}},
            {QStringLiteral("rename"),
             QJsonObject{{QStringLiteral("prepareSupport"), true}}},
            {QStringLiteral("formatting"), QJsonObject{}},
            {QStringLiteral("rangeFormatting"), QJsonObject{}}};
        const QJsonObject capabilities{
            {QStringLiteral("textDocument"), textDocument},
            {QStringLiteral("window"),
             QJsonObject{{QStringLiteral("workDoneProgress"), true}}},
            {QStringLiteral("workspace"),
             QJsonObject{{QStringLiteral("applyEdit"), true},
                         {QStringLiteral("configuration"), true}}}};
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

void LspClient::stop()
{
    if (!m_proc || m_proc->state() != QProcess::Running) {
        return;
    }
    if (m_ready) {
        // Best-effort clean shutdown handshake; we do not wait on the reply.
        send(QJsonObject{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                         {QStringLiteral("id"), m_nextId++},
                         {QStringLiteral("method"), QStringLiteral("shutdown")},
                         {QStringLiteral("params"), QJsonValue::Null}});
        send(QJsonObject{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                         {QStringLiteral("method"), QStringLiteral("exit")},
                         {QStringLiteral("params"), QJsonValue::Null}});
    }
    m_ready = false;
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
            m_serverCaps = msg.value(QStringLiteral("result"))
                               .toObject()
                               .value(QStringLiteral("capabilities"))
                               .toObject();
            setState(State::Running);
            send(QJsonObject{{QStringLiteral("jsonrpc"), QStringLiteral("2.0")},
                             {QStringLiteral("method"), QStringLiteral("initialized")},
                             {QStringLiteral("params"), QJsonObject{}}});
            for (const QJsonObject &queued : m_queue) {
                send(queued);
            }
            m_queue.clear();
            return;
        }
        const PendingCallback pending = m_callbacks.take(id);
        if (pending.cb) {
            // Lifetime guard: if the registrant supplied a context that has since
            // been destroyed, drop the response rather than dereference freed state.
            if (pending.guarded && pending.context.isNull()) {
                return;
            }
            pending.cb(msg.value(QStringLiteral("result")));
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
        } else if (method == QLatin1String("workspace/applyEdit")) {
            const QJsonObject edit = msg.value(QStringLiteral("params"))
                                         .toObject()
                                         .value(QStringLiteral("edit"))
                                         .toObject();
            emit applyEditRequested(edit);
            result = QJsonObject{{QStringLiteral("applied"), true}};
        } else if (method == QLatin1String("window/workDoneProgress/create")) {
            // Acknowledge the progress token so $/progress can flow.
            result = QJsonValue::Null;
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
    } else if (method == QLatin1String("$/progress")) {
        const QJsonObject value = msg.value(QStringLiteral("params"))
                                      .toObject()
                                      .value(QStringLiteral("value"))
                                      .toObject();
        const QString kind = value.value(QStringLiteral("kind")).toString();
        if (kind == QLatin1String("end")) {
            emit progress(QString());
        } else {
            QString text = value.value(QStringLiteral("title")).toString();
            const QString message = value.value(QStringLiteral("message")).toString();
            if (!message.isEmpty()) {
                text = text.isEmpty() ? message : text + QLatin1String(": ") + message;
            }
            if (value.contains(QStringLiteral("percentage"))) {
                text += QStringLiteral(" %1%")
                            .arg(value.value(QStringLiteral("percentage")).toInt());
            }
            emit progress(text);
        }
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

void LspClient::didSave(const QString &path)
{
    notify(QStringLiteral("textDocument/didSave"),
           QJsonObject{{QStringLiteral("textDocument"),
                        QJsonObject{{QStringLiteral("uri"), uriFor(path)}}}});
}

void LspClient::didClose(const QString &path)
{
    m_versions.remove(path);
    notify(QStringLiteral("textDocument/didClose"),
           QJsonObject{{QStringLiteral("textDocument"),
                        QJsonObject{{QStringLiteral("uri"), uriFor(path)}}}});
}

void LspClient::request(const QString &method, const QJsonObject &params,
                        std::function<void(const QJsonValue &)> callback, QObject *context)
{
    const int id = m_nextId++;
    if (callback) {
        m_callbacks.insert(id, PendingCallback{std::move(callback),
                                               QPointer<QObject>(context),
                                               context != nullptr});
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
