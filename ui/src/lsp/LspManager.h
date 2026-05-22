#pragma once

#include <QHash>
#include <QJsonArray>
#include <QJsonValue>
#include <QList>
#include <QObject>
#include <QString>
#include <QStringList>

#include <vector>

namespace KTextEditor {
class Document;
class MovingRange;
class View;
}
class LspClient;
class LspCompletionModel;
class LspHoverProvider;
class QTimer;

// Problem is one diagnostic, for the Problems panel.
struct Problem {
    QString path;
    int line = 0;
    int severity = 1; // 1 error, 2 warning, 3 info, 4 hint
    QString message;
};

// Location is one place in the codebase — for go-to-definition / references.
struct Location {
    QString path;
    int line = 0;
};

// Symbol is one entry in a file's document-symbol outline.
struct Symbol {
    QString name;
    QString detail;
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

    // Register / clear language servers discovered in installed VS Code
    // extensions. These take precedence over the built-in defaults for the
    // file extensions they declare, and apply to files opened afterwards.
    void registerExtensionServer(const QStringList &fileExtensions,
                                 const QString &command, const QStringList &args,
                                 const QString &languageId);
    void clearExtensionServers();

Q_SIGNALS:
    void problemsChanged();
    void definitionResolved(const QString &path, int line);
    void referencesResolved(const QList<Location> &locations);
    void symbolsResolved(const QString &path, const QList<Symbol> &symbols);

private:
    struct ServerDef {
        QString command;
        QStringList args;
        QString languageId;
    };
    struct DocEntry {
        KTextEditor::Document *doc = nullptr;
        QString path;
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

    QHash<KTextEditor::Document *, DocEntry> m_docs;
    QHash<QString, LspClient *> m_clients;  // key = command + '\n' + root
    QHash<QString, ServerDef> m_extServers; // file suffix -> extension server
};
