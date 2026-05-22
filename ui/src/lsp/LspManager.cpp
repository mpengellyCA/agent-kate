#include "LspManager.h"
#include "LspClient.h"
#include "LspCompletionModel.h"
#include "LspHoverProvider.h"

#include <KTextEditor/Attribute>
#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/MovingRange>
#include <KTextEditor/Range>
#include <KTextEditor/View>

#include <QColor>
#include <QFileInfo>
#include <QJsonObject>
#include <QTextCharFormat>
#include <QTimer>
#include <QUrl>

namespace {
QColor severityColor(int severity)
{
    switch (severity) {
    case 1:  return QColor(0xe3, 0x6b, 0x5f); // error  — red
    case 2:  return QColor(0xe0, 0xa5, 0x3a); // warning — amber
    case 3:  return QColor(0x6f, 0x9b, 0xd6); // info   — blue
    default: return QColor(0x8b, 0x91, 0xa0); // hint   — grey
    }
}

QJsonObject positionParams(const QString &path, const KTextEditor::Cursor &pos)
{
    return QJsonObject{
        {QStringLiteral("textDocument"),
         QJsonObject{{QStringLiteral("uri"), QUrl::fromLocalFile(path).toString()}}},
        {QStringLiteral("position"),
         QJsonObject{{QStringLiteral("line"), pos.line()},
                     {QStringLiteral("character"), pos.column()}}}};
}

// toLocation reads a path + line from an LSP Location or LocationLink.
bool toLocation(const QJsonObject &o, Location &out)
{
    QString uri = o.value(QStringLiteral("uri")).toString();
    QJsonObject range = o.value(QStringLiteral("range")).toObject();
    if (uri.isEmpty()) {
        uri = o.value(QStringLiteral("targetUri")).toString();
        range = o.value(QStringLiteral("targetRange")).toObject();
    }
    if (uri.isEmpty()) {
        return false;
    }
    out.path = QUrl(uri).toLocalFile();
    out.line = range.value(QStringLiteral("start")).toObject().value(QStringLiteral("line")).toInt();
    return true;
}

// parseSymbols handles both DocumentSymbol (nested) and SymbolInformation (flat).
void parseSymbols(const QJsonArray &items, std::vector<Symbol> &out)
{
    for (const QJsonValue &v : items) {
        const QJsonObject o = v.toObject();
        Symbol s;
        s.name = o.value(QStringLiteral("name")).toString();
        s.detail = o.value(QStringLiteral("detail")).toString();
        s.kind = o.value(QStringLiteral("kind")).toInt();
        if (o.contains(QStringLiteral("range"))) {
            s.line = o.value(QStringLiteral("range")).toObject()
                         .value(QStringLiteral("start")).toObject()
                         .value(QStringLiteral("line")).toInt();
        } else {
            s.line = o.value(QStringLiteral("location")).toObject()
                         .value(QStringLiteral("range")).toObject()
                         .value(QStringLiteral("start")).toObject()
                         .value(QStringLiteral("line")).toInt();
        }
        if (o.contains(QStringLiteral("children"))) {
            parseSymbols(o.value(QStringLiteral("children")).toArray(), s.children);
        }
        out.push_back(s);
    }
}
} // namespace

LspManager::LspManager(QObject *parent)
    : QObject(parent)
{
}

LspManager::ServerDef LspManager::serverFor(const QString &path) const
{
    const QString ext = QFileInfo(path).suffix().toLower();
    // A server from an installed VS Code extension wins over the built-ins.
    if (const auto it = m_extServers.constFind(ext); it != m_extServers.constEnd()) {
        return it.value();
    }
    if (ext == QLatin1String("go")) {
        return {QStringLiteral("gopls"), {}, QStringLiteral("go")};
    }
    if (ext == QLatin1String("rs")) {
        return {QStringLiteral("rust-analyzer"), {}, QStringLiteral("rust")};
    }
    if (ext == QLatin1String("py")) {
        return {QStringLiteral("pyright-langserver"),
                {QStringLiteral("--stdio")}, QStringLiteral("python")};
    }
    if (ext == QLatin1String("c")) {
        return {QStringLiteral("clangd"), {}, QStringLiteral("c")};
    }
    if (ext == QLatin1String("cc") || ext == QLatin1String("cpp") || ext == QLatin1String("cxx")
        || ext == QLatin1String("h") || ext == QLatin1String("hpp")
        || ext == QLatin1String("hxx")) {
        return {QStringLiteral("clangd"), {}, QStringLiteral("cpp")};
    }
    if (ext == QLatin1String("ts") || ext == QLatin1String("tsx")) {
        return {QStringLiteral("typescript-language-server"),
                {QStringLiteral("--stdio")}, QStringLiteral("typescript")};
    }
    if (ext == QLatin1String("js") || ext == QLatin1String("jsx")) {
        return {QStringLiteral("typescript-language-server"),
                {QStringLiteral("--stdio")}, QStringLiteral("javascript")};
    }
    if (ext == QLatin1String("php")) {
        return {QStringLiteral("intelephense"),
                {QStringLiteral("--stdio")}, QStringLiteral("php")};
    }
    return {};
}

LspClient *LspManager::ensureClient(const ServerDef &def, const QString &root)
{
    const QString key = def.command + QLatin1Char('\n') + root;
    if (LspClient *existing = m_clients.value(key, nullptr)) {
        return existing;
    }
    auto *client = new LspClient(this);
    connect(client, &LspClient::diagnostics, this, &LspManager::onDiagnostics);
    client->start(def.command, def.args, root);
    m_clients.insert(key, client);
    return client;
}

void LspManager::documentOpened(KTextEditor::Document *doc, const QString &projectRoot)
{
    if (!doc || m_docs.contains(doc)) {
        return;
    }
    const QString path = doc->url().toLocalFile();
    if (path.isEmpty()) {
        return;
    }
    const ServerDef def = serverFor(path);
    if (def.command.isEmpty()) {
        return; // no language server known for this file type
    }
    LspClient *client = ensureClient(def, projectRoot);

    DocEntry entry;
    entry.doc = doc;
    entry.path = path;
    entry.client = client;
    entry.debounce = new QTimer(this);
    entry.debounce->setSingleShot(true);
    entry.debounce->setInterval(400);
    entry.completion = new LspCompletionModel(client, path, this);
    entry.hover = new LspHoverProvider(client, path, this);
    m_docs.insert(doc, entry);

    connect(entry.debounce, &QTimer::timeout, this, [this, doc] {
        auto it = m_docs.find(doc);
        if (it != m_docs.end()) {
            it->client->didChange(it->path, doc->text());
        }
    });
    connect(doc, &KTextEditor::Document::textChanged, this, [this, doc] {
        auto it = m_docs.find(doc);
        if (it != m_docs.end()) {
            it->debounce->start();
        }
    });

    // Offer completion and hover in every view of the document, now and later.
    const QList<KTextEditor::View *> views = doc->views();
    for (KTextEditor::View *view : views) {
        view->registerCompletionModel(entry.completion);
        view->registerTextHintProvider(entry.hover);
    }
    connect(doc, &KTextEditor::Document::viewCreated, this,
            [this](KTextEditor::Document *d, KTextEditor::View *view) {
                auto it = m_docs.find(d);
                if (it != m_docs.end()) {
                    view->registerCompletionModel(it->completion);
                    view->registerTextHintProvider(it->hover);
                }
            });

    client->didOpen(path, def.languageId, doc->text());
}

void LspManager::documentClosed(KTextEditor::Document *doc)
{
    auto it = m_docs.find(doc);
    if (it == m_docs.end()) {
        return;
    }
    qDeleteAll(it->ranges);
    it->debounce->deleteLater();
    const QList<KTextEditor::View *> views = doc->views();
    for (KTextEditor::View *view : views) {
        if (it->completion) {
            view->unregisterCompletionModel(it->completion);
        }
        if (it->hover) {
            view->unregisterTextHintProvider(it->hover);
        }
    }
    if (it->completion) {
        it->completion->deleteLater();
    }
    if (it->hover) {
        it->hover->deleteLater();
    }
    it->client->didClose(it->path);
    m_docs.erase(it);
    emit problemsChanged();
}

void LspManager::onDiagnostics(const QString &path, const QJsonArray &items)
{
    for (auto it = m_docs.begin(); it != m_docs.end(); ++it) {
        if (it->path == path) {
            renderDiagnostics(it.value(), items);
            return;
        }
    }
}

void LspManager::renderDiagnostics(DocEntry &entry, const QJsonArray &items)
{
    qDeleteAll(entry.ranges);
    entry.ranges.clear();
    entry.problems.clear();

    for (const QJsonValue &value : items) {
        const QJsonObject d = value.toObject();
        const QJsonObject range = d.value(QStringLiteral("range")).toObject();
        const QJsonObject start = range.value(QStringLiteral("start")).toObject();
        const QJsonObject end = range.value(QStringLiteral("end")).toObject();
        int sl = start.value(QStringLiteral("line")).toInt();
        int sc = start.value(QStringLiteral("character")).toInt();
        int el = end.value(QStringLiteral("line")).toInt();
        int ec = end.value(QStringLiteral("character")).toInt();
        const int severity = d.value(QStringLiteral("severity")).toInt(1);
        const QString message = d.value(QStringLiteral("message")).toString();

        if (sl == el && sc == ec) {
            // Zero-width range — underline the whole line so it is visible.
            sc = 0;
            ec = qMax(1, entry.doc->lineLength(sl));
        }

        KTextEditor::MovingRange *mr =
            entry.doc->newMovingRange(KTextEditor::Range(sl, sc, el, ec));
        KTextEditor::Attribute::Ptr attr(new KTextEditor::Attribute());
        attr->setUnderlineStyle(QTextCharFormat::SpellCheckUnderline);
        attr->setUnderlineColor(severityColor(severity));
        mr->setAttribute(attr);
        entry.ranges.append(mr);

        entry.problems.append(Problem{entry.path, sl, severity, message});
    }
    emit problemsChanged();
}

QList<Problem> LspManager::problems() const
{
    QList<Problem> all;
    for (const DocEntry &entry : m_docs) {
        all += entry.problems;
    }
    return all;
}

void LspManager::gotoDefinition(KTextEditor::View *view)
{
    if (!view) {
        return;
    }
    auto it = m_docs.find(view->document());
    if (it == m_docs.end()) {
        return;
    }
    it->client->request(
        QStringLiteral("textDocument/definition"),
        positionParams(it->path, view->cursorPosition()),
        [this](const QJsonValue &result) {
            Location loc;
            if (result.isArray()) {
                const QJsonArray arr = result.toArray();
                if (!arr.isEmpty() && toLocation(arr.first().toObject(), loc)) {
                    emit definitionResolved(loc.path, loc.line);
                }
            } else if (result.isObject() && toLocation(result.toObject(), loc)) {
                emit definitionResolved(loc.path, loc.line);
            }
        });
}

void LspManager::findReferences(KTextEditor::View *view)
{
    if (!view) {
        return;
    }
    auto it = m_docs.find(view->document());
    if (it == m_docs.end()) {
        return;
    }
    QJsonObject params = positionParams(it->path, view->cursorPosition());
    params[QStringLiteral("context")] =
        QJsonObject{{QStringLiteral("includeDeclaration"), true}};
    it->client->request(
        QStringLiteral("textDocument/references"), params,
        [this](const QJsonValue &result) {
            QList<Location> locations;
            for (const QJsonValue &v : result.toArray()) {
                Location loc;
                if (toLocation(v.toObject(), loc)) {
                    locations.append(loc);
                }
            }
            emit referencesResolved(locations);
        });
}

void LspManager::requestSymbols(const QString &path)
{
    for (auto it = m_docs.begin(); it != m_docs.end(); ++it) {
        if (it->path != path) {
            continue;
        }
        it->client->request(
            QStringLiteral("textDocument/documentSymbol"),
            QJsonObject{{QStringLiteral("textDocument"),
                         QJsonObject{{QStringLiteral("uri"),
                                      QUrl::fromLocalFile(path).toString()}}}},
            [this, path](const QJsonValue &result) {
                std::vector<Symbol> parsed;
                parseSymbols(result.toArray(), parsed);
                emit symbolsResolved(path, QList<Symbol>(parsed.begin(), parsed.end()));
            });
        return;
    }
    // No LSP-tracked document for this path — clear the outline.
    emit symbolsResolved(path, QList<Symbol>());
}

void LspManager::registerExtensionServer(const QStringList &fileExtensions,
                                         const QString &command,
                                         const QStringList &args,
                                         const QString &languageId)
{
    if (command.isEmpty()) {
        return;
    }
    for (const QString &raw : fileExtensions) {
        QString ext = raw.trimmed().toLower();
        if (ext.startsWith(QLatin1Char('.'))) {
            ext.remove(0, 1);
        }
        if (!ext.isEmpty()) {
            m_extServers.insert(ext, ServerDef{command, args, languageId});
        }
    }
}

void LspManager::clearExtensionServers()
{
    m_extServers.clear();
}
