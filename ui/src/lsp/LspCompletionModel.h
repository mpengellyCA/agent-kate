#pragma once

#include <KTextEditor/CodeCompletionModel>
#include <KTextEditor/CodeCompletionModelControllerInterface>

#include <QJsonValue>
#include <QList>
#include <QString>

class LspClient;
namespace KTextEditor {
class View;
}

// LspCompletionModel feeds KTextEditor's completion popup from a language
// server. When the editor invokes completion it fires textDocument/completion;
// the async result resets the model and the popup updates.
class LspCompletionModel : public KTextEditor::CodeCompletionModel,
                           public KTextEditor::CodeCompletionModelControllerInterface
{
    Q_OBJECT
    Q_INTERFACES(KTextEditor::CodeCompletionModelControllerInterface)
public:
    LspCompletionModel(LspClient *client, const QString &path, QObject *parent = nullptr);

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
        QString insertText;
    };
    void applyCompletions(const QJsonValue &result);

    LspClient *m_client = nullptr;
    QString m_path;
    QList<Item> m_items;
    bool m_pending = false;
};
