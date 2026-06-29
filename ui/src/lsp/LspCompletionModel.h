#pragma once

#include <KTextEditor/CodeCompletionModel>
#include <KTextEditor/CodeCompletionModelControllerInterface>

#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QList>
#include <QString>

class LspClient;
class LspManager;
namespace KTextEditor {
class View;
}

// LspCompletionModel feeds KTextEditor's completion popup from a language
// server. When the editor invokes completion it fires textDocument/completion;
// the async result resets the model and the popup updates. Items carry a kind
// icon, documentation, and precise text edits (incl. auto-import edits).
class LspCompletionModel : public KTextEditor::CodeCompletionModel,
                           public KTextEditor::CodeCompletionModelControllerInterface
{
    Q_OBJECT
    Q_INTERFACES(KTextEditor::CodeCompletionModelControllerInterface)
public:
    explicit LspCompletionModel(LspClient *client, QObject *parent = nullptr);

    void setPath(const QString &path) { m_path = path; }
    void setManager(LspManager *manager) { m_manager = manager; }

    void completionInvoked(KTextEditor::View *view, const KTextEditor::Range &range,
                           InvocationType invocationType) override;
    void executeCompletionItem(KTextEditor::View *view, const KTextEditor::Range &word,
                               const QModelIndex &index) const override;

    QVariant data(const QModelIndex &index, int role) const override;
    QModelIndex index(int row, int column, const QModelIndex &parent) const override;
    QModelIndex parent(const QModelIndex &child) const override;
    int rowCount(const QModelIndex &parent) const override;
    int columnCount(const QModelIndex &parent) const override;

    bool shouldAbortCompletion(KTextEditor::View *view, const KTextEditor::Range &range,
                               const QString &currentCompletion) override;

private:
    struct Item {
        QString label;
        QString detail;
        QString documentation;
        QString insertText;
        int kind = 0;
        bool snippet = false;
        QJsonObject textEdit;            // optional precise replacement range
        QJsonArray additionalTextEdits;  // e.g. auto-import lines
    };
    void applyCompletions(const QJsonValue &result);

    LspClient *m_client = nullptr;
    LspManager *m_manager = nullptr;
    QString m_path;
    QList<Item> m_items;
    bool m_pending = false;
    // Bumped on every completion request; a reply only applies if it still matches,
    // so a late/out-of-order response can't overwrite a newer popup with stale items.
    quint64 m_completionGen = 0;
};
