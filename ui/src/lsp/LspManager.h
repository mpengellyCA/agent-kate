#pragma once

#include "LspClient.h"

#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QList>
#include <QObject>
#include <QString>
#include <QStringList>

#include <functional>
#include <vector>

namespace KTextEditor {
class Document;
class MovingRange;
class View;
}
class LspCompletionModel;
class LspHoverProvider;
class QTimer;

// Problem is one diagnostic, for the Problems panel. It keeps the raw LSP
// diagnostic JSON so code-actions can pass it back as context.
struct Problem {
    QString path;
    int line = 0;
    int column = 0;
    int endLine = 0;
    int endColumn = 0;
    int severity = 1; // 1 error, 2 warning, 3 info, 4 hint
    QString message;
    QJsonObject raw; // the verbatim LSP diagnostic
};

// Location is one place in the codebase — for go-to-definition / references.
struct Location {
    QString path;
    int line = 0;
};

// Symbol is one entry in a file's document-symbol outline, or a workspace symbol.
struct Symbol {
    QString name;
    QString detail;
    QString path; // set for workspace symbols (SymbolInformation.location.uri)
    int kind = 0;
    int line = 0;
    std::vector<Symbol> children;
};

// LspManager wires KTextEditor documents to language servers: it picks a server
// per file type, keeps one client per (server, project), syncs edits, and
// renders publishDiagnostics as squiggle underlines plus a Problems list.
class LspManager : public QObject
{
    Q_OBJECT
public:
    explicit LspManager(QObject *parent = nullptr);

    void documentOpened(KTextEditor::Document *doc, const QString &projectRoot);
    void documentClosed(KTextEditor::Document *doc);
    QList<Problem> problems() const;

    void gotoDefinition(KTextEditor::View *view);
    void findReferences(KTextEditor::View *view);
    void requestSymbols(const QString &path);

    // Code intelligence actions (all gated on server capabilities).
    void requestCodeActions(KTextEditor::View *view);
    // Apply a chosen code action (carries edit and/or command).
    void executeCodeAction(LspClient *client, const QJsonObject &action);
    void renameSymbol(KTextEditor::View *view);
    // Issue the actual rename once the new name has been collected from the user.
    void performRename(KTextEditor::View *view, const QString &newName);
    // Request signature help at the cursor and show it as a tooltip.
    void requestSignatureHelp(KTextEditor::View *view);
    // Format the whole document; runs `then` once edits are applied (or skipped).
    void formatDocument(KTextEditor::View *view, std::function<void(bool)> then = {});
    bool canFormat(KTextEditor::View *view) const;
    // Notify the server a document was saved (for save-watching servers).
    void documentSaved(const QString &path);

    // Diagnostic navigation within the active document.
    void nextProblem(KTextEditor::View *view);
    void prevProblem(KTextEditor::View *view);

    // Workspace symbol search (Ctrl+T). Query the active file's server.
    void requestWorkspaceSymbols(const QString &query,
                                 std::function<void(const QList<Symbol> &)> callback);

    // Apply an LSP WorkspaceEdit to open documents (the shared primitive that
    // rename, code actions and formatting all reuse).
    void applyWorkspaceEdit(const QJsonObject &edit);
    // Apply a bare TextEdit[] to the document at `path` (completion auto-imports).
    void applyEditsToPath(const QString &path, const QJsonArray &edits);

    // Restart / rebind servers.
    void restartServersForCurrentFile(const QString &path);
    void rebindOpenDocuments();
    // Human-readable status for the active file's server (icon name + text).
    QString statusFor(const QString &path, QString &iconName) const;

    // Register / clear language servers discovered in installed VS Code
    // extensions. These take precedence over the built-in defaults for the
    // file extensions they declare.
    void registerExtensionServer(const QStringList &fileExtensions,
                                 const QString &command, const QStringList &args,
                                 const QString &languageId);
    void clearExtensionServers();

Q_SIGNALS:
    void problemsChanged();
    void definitionResolved(const QString &path, int line);
    void referencesResolved(const QList<Location> &locations);
    void symbolsResolved(const QString &path, const QList<Symbol> &symbols);
    // A workspace edit touched a file that is not open; the window should open it.
    void openFileRequested(const QString &path, int line);
    // The active file's server status changed (icon name, status text).
    void serverStatusChanged();
    // A transient status-bar message (rename applied, format failed, …).
    void statusMessage(const QString &text);
    // Code actions arrived for a request — the window pops a menu.
    void codeActionsResolved(LspClient *client, const QJsonArray &actions);
    // A rename was requested on a view — the window prompts for the new name.
    void renameRequested(KTextEditor::View *view);

private:
    struct ServerDef {
        QString command;
        QStringList args;
        QString languageId;
    };
    struct DocEntry {
        KTextEditor::Document *doc = nullptr;
        QString path;
        QString languageId;
        QString root;
        LspClient *client = nullptr;
        LspCompletionModel *completion = nullptr;
        LspHoverProvider *hover = nullptr;
        QTimer *debounce = nullptr;
        QList<KTextEditor::MovingRange *> ranges;
        QList<Problem> problems;
    };

    ServerDef serverFor(const QString &path) const;
    LspClient *ensureClient(const ServerDef &def, const QString &root);
    void onDiagnostics(const QString &path, const QJsonArray &items);
    void renderDiagnostics(DocEntry &entry, const QJsonArray &items);
    DocEntry *entryForView(KTextEditor::View *view);
    DocEntry *entryForPath(const QString &path);
    void moveToProblem(KTextEditor::View *view, int dir);
    void wireDocument(DocEntry &entry, const ServerDef &def);
    void unwireDocument(DocEntry &entry);
    void applyTextEdits(KTextEditor::Document *doc, const QJsonArray &edits);

    QHash<KTextEditor::Document *, DocEntry> m_docs;
    QHash<QString, LspClient *> m_clients;  // key = command + '\n' + root
    QHash<QString, ServerDef> m_extServers; // file suffix -> extension server
    QHash<LspClient *, QString> m_progress; // latest $/progress text per client
};
