#include "LspManager.h"
#include "LspClient.h"
#include "LspCompletionModel.h"
#include "LspHoverProvider.h"
#include "LspSignatureHelp.h"

#include <KColorScheme>
#include <KLocalizedString>
#include <KSyntaxHighlighting/Theme>
#include <KTextEditor/Attribute>
#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Editor>
#include <KTextEditor/MovingRange>
#include <KTextEditor/Range>
#include <KTextEditor/View>

#include <QColor>
#include <QCursor>
#include <QFileInfo>
#include <QJsonObject>
#include <QTextCharFormat>
#include <QTimer>
#include <QToolTip>
#include <QUrl>

#include <algorithm>

namespace {
// severityColor draws diagnostic underlines from the active KTextEditor theme so
// they match the user's colour scheme, falling back to KColorScheme roles.
QColor severityColor(int severity)
{
    using KTextEditor::Editor;
    const auto theme = Editor::instance()->theme();
    auto fromTheme = [&theme](KSyntaxHighlighting::Theme::TextStyle style) {
        return QColor::fromRgba(theme.textColor(style));
    };
    switch (severity) {
    case 1: { // error
        const QColor c = fromTheme(KSyntaxHighlighting::Theme::Error);
        return c.isValid() ? c
                           : KColorScheme(QPalette::Active).foreground(KColorScheme::NegativeText).color();
    }
    case 2: { // warning
        const QColor c = fromTheme(KSyntaxHighlighting::Theme::Warning);
        return c.isValid() ? c
                           : KColorScheme(QPalette::Active).foreground(KColorScheme::NeutralText).color();
    }
    case 3: // info
        return KColorScheme(QPalette::Active).foreground(KColorScheme::ActiveText).color();
    default: // hint
        return KColorScheme(QPalette::Active).foreground(KColorScheme::InactiveText).color();
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
        // SymbolInformation carries the file path on its location.
        s.path = o.value(QStringLiteral("location"))
                     .toObject()
                     .value(QStringLiteral("uri"))
                     .toString();
        if (!s.path.isEmpty()) {
            s.path = QUrl(s.path).toLocalFile();
        }
        out.push_back(s);
    }
}

// rangeToJson serialises a KTextEditor range into an LSP range object.
QJsonObject rangeToJson(const KTextEditor::Range &r)
{
    return QJsonObject{
        {QStringLiteral("start"),
         QJsonObject{{QStringLiteral("line"), r.start().line()},
                     {QStringLiteral("character"), r.start().column()}}},
        {QStringLiteral("end"),
         QJsonObject{{QStringLiteral("line"), r.end().line()},
                     {QStringLiteral("character"), r.end().column()}}}};
}

// firstEditLine returns the start line of the first edit, for cursor placement.
int firstEditLine(const QJsonArray &edits)
{
    if (edits.isEmpty()) {
        return 0;
    }
    return edits.first()
        .toObject()
        .value(QStringLiteral("range"))
        .toObject()
        .value(QStringLiteral("start"))
        .toObject()
        .value(QStringLiteral("line"))
        .toInt();
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
    connect(client, &LspClient::applyEditRequested, this, &LspManager::applyWorkspaceEdit);
    connect(client, &LspClient::stateChanged, this,
            [this](LspClient::State) { emit serverStatusChanged(); });
    connect(client, &LspClient::progress, this, [this, client](const QString &text) {
        if (text.isEmpty()) {
            m_progress.remove(client);
        } else {
            m_progress.insert(client, text);
        }
        emit serverStatusChanged();
    });
    client->start(def.command, def.args, root);
    m_clients.insert(key, client);
    return client;
}

// wireDocument attaches the debounce timer, completion model and hover provider
// to one entry whose client has just been (re)assigned. Reused on open + rebind.
void LspManager::wireDocument(DocEntry &entry, const ServerDef &def)
{
    KTextEditor::Document *doc = entry.doc;
    entry.languageId = def.languageId;
    entry.debounce = new QTimer(this);
    entry.debounce->setSingleShot(true);
    entry.debounce->setInterval(400);
    entry.completion = new LspCompletionModel(entry.client, this);
    entry.completion->setManager(this);
    entry.completion->setPath(entry.path);
    entry.hover = new LspHoverProvider(entry.client, entry.path, this);

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

    entry.client->didOpen(entry.path, def.languageId, doc->text());
}

// unwireDocument tears down everything wireDocument created, leaving the entry's
// path/doc intact so it can be rewired against a different client.
void LspManager::unwireDocument(DocEntry &entry)
{
    // Drop the textChanged / viewCreated connections wireDocument made on the
    // document so a rebind/restart doesn't accumulate duplicates. LspManager is
    // the only thing connecting (doc → this), so this is safe and complete.
    disconnect(entry.doc, nullptr, this, nullptr);
    qDeleteAll(entry.ranges);
    entry.ranges.clear();
    if (entry.debounce) {
        entry.debounce->deleteLater();
        entry.debounce = nullptr;
    }
    const QList<KTextEditor::View *> views = entry.doc->views();
    for (KTextEditor::View *view : views) {
        if (entry.completion) {
            view->unregisterCompletionModel(entry.completion);
        }
        if (entry.hover) {
            view->unregisterTextHintProvider(entry.hover);
        }
    }
    if (entry.completion) {
        entry.completion->deleteLater();
        entry.completion = nullptr;
    }
    if (entry.hover) {
        entry.hover->deleteLater();
        entry.hover = nullptr;
    }
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
    entry.root = projectRoot;
    entry.client = client;
    auto inserted = m_docs.insert(doc, entry);
    wireDocument(inserted.value(), def);
    emit serverStatusChanged();
}

void LspManager::documentClosed(KTextEditor::Document *doc)
{
    auto it = m_docs.find(doc);
    if (it == m_docs.end()) {
        return;
    }
    unwireDocument(it.value());
    it->client->didClose(it->path);
    m_docs.erase(it);
    emit problemsChanged();
    emit serverStatusChanged();
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

        int rsc = sc;
        int rec = ec;
        if (sl == el && sc == ec) {
            // Zero-width range — underline the whole line so it is visible.
            rsc = 0;
            rec = qMax(1, entry.doc->lineLength(sl));
        }

        KTextEditor::MovingRange *mr =
            entry.doc->newMovingRange(KTextEditor::Range(sl, rsc, el, rec));
        KTextEditor::Attribute::Ptr attr(new KTextEditor::Attribute());
        attr->setUnderlineStyle(QTextCharFormat::SpellCheckUnderline);
        attr->setUnderlineColor(severityColor(severity));
        mr->setAttribute(attr);
        entry.ranges.append(mr);

        Problem p;
        p.path = entry.path;
        p.line = sl;
        p.column = sc;
        p.endLine = el;
        p.endColumn = ec;
        p.severity = severity;
        p.message = message;
        p.raw = d;
        entry.problems.append(p);
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

LspManager::DocEntry *LspManager::entryForView(KTextEditor::View *view)
{
    if (!view) {
        return nullptr;
    }
    auto it = m_docs.find(view->document());
    return it == m_docs.end() ? nullptr : &it.value();
}

LspManager::DocEntry *LspManager::entryForPath(const QString &path)
{
    for (auto it = m_docs.begin(); it != m_docs.end(); ++it) {
        if (it->path == path) {
            return &it.value();
        }
    }
    return nullptr;
}

// applyTextEdits applies an LSP TextEdit[] to one open document, bottom-to-top so
// earlier ranges stay valid, wrapped in a single undo step.
void LspManager::applyTextEdits(KTextEditor::Document *doc, const QJsonArray &edits)
{
    if (!doc || edits.isEmpty()) {
        return;
    }
    // Sort descending by start position so later edits don't shift earlier ones.
    std::vector<QJsonObject> sorted;
    sorted.reserve(edits.size());
    for (const QJsonValue &v : edits) {
        sorted.push_back(v.toObject());
    }
    auto startOf = [](const QJsonObject &e) {
        const QJsonObject s = e.value(QStringLiteral("range"))
                                  .toObject()
                                  .value(QStringLiteral("start"))
                                  .toObject();
        return KTextEditor::Cursor(s.value(QStringLiteral("line")).toInt(),
                                   s.value(QStringLiteral("character")).toInt());
    };
    std::stable_sort(sorted.begin(), sorted.end(),
                     [&](const QJsonObject &a, const QJsonObject &b) {
                         return startOf(b) < startOf(a);
                     });
    // One undo step for the whole batch.
    KTextEditor::Document::EditingTransaction transaction(doc);
    for (const QJsonObject &e : sorted) {
        const QJsonObject range = e.value(QStringLiteral("range")).toObject();
        const QJsonObject s = range.value(QStringLiteral("start")).toObject();
        const QJsonObject en = range.value(QStringLiteral("end")).toObject();
        const KTextEditor::Range r(s.value(QStringLiteral("line")).toInt(),
                                   s.value(QStringLiteral("character")).toInt(),
                                   en.value(QStringLiteral("line")).toInt(),
                                   en.value(QStringLiteral("character")).toInt());
        doc->replaceText(r, e.value(QStringLiteral("newText")).toString());
    }
}

void LspManager::applyWorkspaceEdit(const QJsonObject &edit)
{
    // documentChanges is preferred (ordered, versioned); fall back to changes map.
    if (edit.contains(QStringLiteral("documentChanges"))) {
        for (const QJsonValue &v : edit.value(QStringLiteral("documentChanges")).toArray()) {
            const QJsonObject change = v.toObject();
            // Only plain text-document edits are handled (not create/rename/delete).
            if (!change.contains(QStringLiteral("edits"))) {
                continue;
            }
            const QString uri = change.value(QStringLiteral("textDocument"))
                                    .toObject()
                                    .value(QStringLiteral("uri"))
                                    .toString();
            const QString path = QUrl(uri).toLocalFile();
            const QJsonArray edits = change.value(QStringLiteral("edits")).toArray();
            if (DocEntry *e = entryForPath(path)) {
                applyTextEdits(e->doc, edits);
            } else if (!path.isEmpty()) {
                // Open the file then apply once it is tracked.
                emit openFileRequested(path, firstEditLine(edits));
                // Best effort: re-apply on next event loop turn after it opens.
                const QJsonArray captured = edits;
                QTimer::singleShot(250, this, [this, path, captured] {
                    if (DocEntry *e = entryForPath(path)) {
                        applyTextEdits(e->doc, captured);
                    }
                });
            }
        }
        return;
    }
    const QJsonObject changes = edit.value(QStringLiteral("changes")).toObject();
    for (auto it = changes.begin(); it != changes.end(); ++it) {
        const QString path = QUrl(it.key()).toLocalFile();
        const QJsonArray edits = it.value().toArray();
        if (DocEntry *e = entryForPath(path)) {
            applyTextEdits(e->doc, edits);
        } else if (!path.isEmpty()) {
            emit openFileRequested(path, firstEditLine(edits));
            const QJsonArray captured = edits;
            QTimer::singleShot(250, this, [this, path, captured] {
                if (DocEntry *e = entryForPath(path)) {
                    applyTextEdits(e->doc, captured);
                }
            });
        }
    }
}

void LspManager::applyEditsToPath(const QString &path, const QJsonArray &edits)
{
    if (DocEntry *e = entryForPath(path)) {
        applyTextEdits(e->doc, edits);
    }
}

void LspManager::requestCodeActions(KTextEditor::View *view)
{
    DocEntry *e = entryForView(view);
    if (!e) {
        return;
    }
    // codeActionProvider may be `true` or an options object; bail only when it
    // is absent or explicitly false.
    const QJsonValue caCap =
        e->client->capabilities().value(QStringLiteral("codeActionProvider"));
    if (caCap.isUndefined() || (caCap.isBool() && !caCap.toBool())) {
        emit statusMessage(i18n("No code actions available"));
        return;
    }
    // Build the request range: the selection if any, else the cursor's line span.
    KTextEditor::Range sel = view->selectionRange();
    if (!sel.isValid() || sel.isEmpty()) {
        const KTextEditor::Cursor c = view->cursorPosition();
        sel = KTextEditor::Range(c, c);
    }
    QJsonObject range = rangeToJson(sel);

    // Diagnostics overlapping the request range become the action context.
    QJsonArray diags;
    for (const Problem &p : e->problems) {
        const KTextEditor::Range pr(p.line, p.column, p.endLine, p.endColumn);
        if (sel.overlaps(pr) || sel.contains(pr.start()) || pr.contains(sel.start())) {
            diags.append(p.raw);
        }
    }

    QJsonObject params{
        {QStringLiteral("textDocument"),
         QJsonObject{{QStringLiteral("uri"), QUrl::fromLocalFile(e->path).toString()}}},
        {QStringLiteral("range"), range},
        {QStringLiteral("context"), QJsonObject{{QStringLiteral("diagnostics"), diags}}}};

    LspClient *client = e->client;
    client->request(QStringLiteral("textDocument/codeAction"), params,
                    [this, client](const QJsonValue &result) {
                        emit codeActionsResolved(client, result.toArray());
                    });
}

void LspManager::executeCodeAction(LspClient *client, const QJsonObject &action)
{
    // Guard against a server restarted (client deleted) while the menu was open.
    if (!client || !m_clients.values().contains(client)) {
        return;
    }
    // A code action may carry an inline edit, a command, or both.
    if (action.contains(QStringLiteral("edit"))) {
        applyWorkspaceEdit(action.value(QStringLiteral("edit")).toObject());
    }
    const QJsonValue cmd = action.value(QStringLiteral("command"));
    if (cmd.isObject()) {
        const QJsonObject c = cmd.toObject();
        client->request(QStringLiteral("workspace/executeCommand"),
                        QJsonObject{{QStringLiteral("command"),
                                     c.value(QStringLiteral("command")).toString()},
                                    {QStringLiteral("arguments"),
                                     c.value(QStringLiteral("arguments")).toArray()}},
                        nullptr);
    } else if (cmd.isString()) {
        // The action itself is a Command (label + command + arguments).
        client->request(QStringLiteral("workspace/executeCommand"),
                        QJsonObject{{QStringLiteral("command"), cmd.toString()},
                                    {QStringLiteral("arguments"),
                                     action.value(QStringLiteral("arguments")).toArray()}},
                        nullptr);
    }
}

void LspManager::renameSymbol(KTextEditor::View *view)
{
    DocEntry *e = entryForView(view);
    if (!e) {
        return;
    }
    if (e->client->capabilities().value(QStringLiteral("renameProvider")).isUndefined()) {
        emit statusMessage(i18n("Rename is not supported by this server"));
        return;
    }
    emit renameRequested(view);
}

void LspManager::performRename(KTextEditor::View *view, const QString &newName)
{
    DocEntry *e = entryForView(view);
    if (!e || newName.isEmpty()) {
        return;
    }
    QJsonObject params = positionParams(e->path, view->cursorPosition());
    params[QStringLiteral("newName")] = newName;
    e->client->request(QStringLiteral("textDocument/rename"), params,
                       [this](const QJsonValue &result) {
                           if (result.isObject()) {
                               applyWorkspaceEdit(result.toObject());
                               emit statusMessage(i18n("Symbol renamed"));
                           }
                       });
}

void LspManager::requestSignatureHelp(KTextEditor::View *view)
{
    DocEntry *e = entryForView(view);
    if (!e) {
        return;
    }
    if (e->client->capabilities().value(QStringLiteral("signatureHelpProvider")).isUndefined()) {
        return;
    }
    e->client->request(
        QStringLiteral("textDocument/signatureHelp"),
        positionParams(e->path, view->cursorPosition()),
        [this](const QJsonValue &result) {
            const QString html = LspSignatureHelp::format(result);
            if (!html.isEmpty()) {
                QToolTip::showText(QCursor::pos(), html);
            }
        });
}

bool LspManager::canFormat(KTextEditor::View *view) const
{
    if (!view) {
        return false;
    }
    auto it = m_docs.constFind(view->document());
    if (it == m_docs.constEnd()) {
        return false;
    }
    return !it->client->capabilities()
                .value(QStringLiteral("documentFormattingProvider"))
                .isUndefined();
}

void LspManager::formatDocument(KTextEditor::View *view, std::function<void(bool)> then)
{
    DocEntry *e = entryForView(view);
    if (!e || !canFormat(view)) {
        if (then) {
            then(false);
        }
        return;
    }
    KTextEditor::Document *doc = e->doc;
    const QVariant tabVar = doc->configValue(QStringLiteral("tab-width"));
    const int tabWidth = tabVar.isValid() ? tabVar.toInt() : 4;
    const bool replaceTabs = doc->configValue(QStringLiteral("replace-tabs")).toBool();
    QJsonObject params{
        {QStringLiteral("textDocument"),
         QJsonObject{{QStringLiteral("uri"), QUrl::fromLocalFile(e->path).toString()}}},
        {QStringLiteral("options"),
         QJsonObject{{QStringLiteral("tabSize"), tabWidth > 0 ? tabWidth : 4},
                     {QStringLiteral("insertSpaces"), replaceTabs}}}};
    e->client->request(
        QStringLiteral("textDocument/formatting"), params,
        [this, doc, then](const QJsonValue &result) {
            if (result.isArray()) {
                applyTextEdits(doc, result.toArray());
            }
            if (then) {
                then(true);
            }
        });
}

void LspManager::documentSaved(const QString &path)
{
    if (DocEntry *e = entryForPath(path)) {
        e->client->didSave(path);
    }
}

void LspManager::nextProblem(KTextEditor::View *view)
{
    moveToProblem(view, +1);
}

void LspManager::prevProblem(KTextEditor::View *view)
{
    moveToProblem(view, -1);
}

void LspManager::moveToProblem(KTextEditor::View *view, int dir)
{
    DocEntry *e = entryForView(view);
    if (!e || e->problems.isEmpty()) {
        emit statusMessage(i18n("No problems in this file"));
        return;
    }
    QList<Problem> sorted = e->problems;
    std::sort(sorted.begin(), sorted.end(), [](const Problem &a, const Problem &b) {
        return a.line != b.line ? a.line < b.line : a.column < b.column;
    });
    const KTextEditor::Cursor cur = view->cursorPosition();
    int target = -1;
    if (dir > 0) {
        for (int i = 0; i < sorted.size(); ++i) {
            if (sorted[i].line > cur.line()
                || (sorted[i].line == cur.line() && sorted[i].column > cur.column())) {
                target = i;
                break;
            }
        }
        if (target < 0) {
            target = 0; // wrap to first
        }
    } else {
        for (int i = sorted.size() - 1; i >= 0; --i) {
            if (sorted[i].line < cur.line()
                || (sorted[i].line == cur.line() && sorted[i].column < cur.column())) {
                target = i;
                break;
            }
        }
        if (target < 0) {
            target = sorted.size() - 1; // wrap to last
        }
    }
    const Problem &p = sorted[target];
    view->setCursorPosition(KTextEditor::Cursor(p.line, p.column));
    emit statusMessage(p.message.simplified());
}

void LspManager::requestWorkspaceSymbols(const QString &query,
                                         std::function<void(const QList<Symbol> &)> callback)
{
    // Use any live client (workspace/symbol spans the project).
    LspClient *client = nullptr;
    for (LspClient *c : m_clients) {
        if (c->state() == LspClient::State::Running
            && !c->capabilities().value(QStringLiteral("workspaceSymbolProvider")).isUndefined()) {
            client = c;
            break;
        }
    }
    if (!client) {
        if (callback) {
            callback({});
        }
        return;
    }
    client->request(QStringLiteral("workspace/symbol"),
                    QJsonObject{{QStringLiteral("query"), query}},
                    [callback](const QJsonValue &result) {
                        std::vector<Symbol> parsed;
                        parseSymbols(result.toArray(), parsed);
                        if (callback) {
                            callback(QList<Symbol>(parsed.begin(), parsed.end()));
                        }
                    });
}

void LspManager::restartServersForCurrentFile(const QString &path)
{
    DocEntry *active = entryForPath(path);
    if (!active) {
        return;
    }
    LspClient *old = active->client;
    const QString root = active->root;
    const ServerDef def = serverFor(path);
    if (def.command.isEmpty()) {
        return;
    }
    const QString key = def.command + QLatin1Char('\n') + root;

    // Detach every document bound to the old client and remember them.
    QList<DocEntry *> affected;
    for (auto it = m_docs.begin(); it != m_docs.end(); ++it) {
        if (it->client == old) {
            affected.append(&it.value());
        }
    }
    for (DocEntry *e : affected) {
        unwireDocument(*e);
    }
    // Tear down the old client.
    m_clients.remove(key);
    m_progress.remove(old);
    old->stop();
    old->deleteLater();

    // Recreate and re-open everything.
    LspClient *fresh = ensureClient(def, root);
    for (DocEntry *e : affected) {
        e->client = fresh;
        const ServerDef d = serverFor(e->path);
        wireDocument(*e, d.command.isEmpty() ? def : d);
    }
    emit problemsChanged();
    emit serverStatusChanged();
    emit statusMessage(i18n("Language server restarted"));
}

void LspManager::rebindOpenDocuments()
{
    // Re-evaluate serverFor for every tracked document; rebind those whose server
    // command changed (e.g. an extension server was just installed).
    QList<KTextEditor::Document *> docs = m_docs.keys();
    for (KTextEditor::Document *doc : docs) {
        auto it = m_docs.find(doc);
        if (it == m_docs.end()) {
            continue;
        }
        const ServerDef def = serverFor(it->path);
        if (def.command.isEmpty()) {
            continue;
        }
        const QString key = def.command + QLatin1Char('\n') + it->root;
        LspClient *desired = m_clients.value(key, nullptr);
        if (desired == it->client) {
            continue; // already bound to the right server
        }
        // Switch this document to the desired (or a freshly created) client.
        it->client->didClose(it->path);
        unwireDocument(it.value());
        it->client = ensureClient(def, it->root);
        wireDocument(it.value(), def);
    }
    emit serverStatusChanged();
}

QString LspManager::statusFor(const QString &path, QString &iconName) const
{
    const DocEntry *e = nullptr;
    for (auto i = m_docs.constBegin(); i != m_docs.constEnd(); ++i) {
        if (i->path == path) {
            e = &i.value();
            break;
        }
    }
    if (!e || !e->client) {
        iconName.clear();
        return QString();
    }
    const QString name = QFileInfo(serverFor(path).command).fileName();
    switch (e->client->state()) {
    case LspClient::State::Crashed:
        iconName = QStringLiteral("dialog-error");
        return i18nc("@info:status language server crashed", "%1: crashed", name);
    case LspClient::State::Starting:
        iconName = QStringLiteral("chronometer");
        return i18nc("@info:status language server starting", "%1: starting…", name);
    case LspClient::State::Running: {
        const QString prog = m_progress.value(e->client);
        if (!prog.isEmpty()) {
            iconName = QStringLiteral("chronometer");
            return QStringLiteral("%1: %2").arg(name, prog);
        }
        iconName = QStringLiteral("dialog-information");
        return i18nc("@info:status language server ready", "%1: ready", name);
    }
    }
    iconName.clear();
    return QString();
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
