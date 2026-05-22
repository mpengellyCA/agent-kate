#include "LspCompletionModel.h"
#include "LspClient.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Range>
#include <KTextEditor/View>

#include <QJsonArray>
#include <QJsonObject>
#include <QPointer>
#include <QRegularExpression>
#include <QUrl>

LspCompletionModel::LspCompletionModel(LspClient *client, const QString &path, QObject *parent)
    : KTextEditor::CodeCompletionModel(parent)
    , m_client(client)
    , m_path(path)
{
}

void LspCompletionModel::completionInvoked(KTextEditor::View *view,
                                           const KTextEditor::Range &,
                                           InvocationType)
{
    if (!view || !m_client) {
        return;
    }
    const KTextEditor::Cursor pos = view->cursorPosition();
    // Force a sync so the server completes against the current document text.
    m_client->didChange(m_path, view->document()->text());
    m_pending = true;

    QPointer<LspCompletionModel> self(this);
    m_client->request(
        QStringLiteral("textDocument/completion"),
        QJsonObject{
            {QStringLiteral("textDocument"),
             QJsonObject{{QStringLiteral("uri"), QUrl::fromLocalFile(m_path).toString()}}},
            {QStringLiteral("position"),
             QJsonObject{{QStringLiteral("line"), pos.line()},
                         {QStringLiteral("character"), pos.column()}}}},
        [self](const QJsonValue &result) {
            if (self) {
                self->applyCompletions(result);
            }
        });
}

void LspCompletionModel::applyCompletions(const QJsonValue &result)
{
    m_pending = false;

    QJsonArray raw;
    if (result.isArray()) {
        raw = result.toArray();
    } else if (result.isObject()) {
        raw = result.toObject().value(QStringLiteral("items")).toArray();
    }

    beginResetModel();
    m_items.clear();
    for (const QJsonValue &value : raw) {
        const QJsonObject o = value.toObject();
        Item item;
        item.label = o.value(QStringLiteral("label")).toString().trimmed();
        item.detail = o.value(QStringLiteral("detail")).toString();
        item.insertText = o.value(QStringLiteral("insertText")).toString();
        if (item.insertText.isEmpty()) {
            item.insertText = item.label;
        }
        if (o.value(QStringLiteral("insertTextFormat")).toInt(1) == 2) {
            // Snippet — drop ${n:placeholder} and $n markers for a plain insert.
            static const QRegularExpression braced(QStringLiteral("\\$\\{[^}]*\\}"));
            static const QRegularExpression bare(QStringLiteral("\\$\\d+"));
            item.insertText.remove(braced).remove(bare);
        }
        if (!item.label.isEmpty()) {
            m_items.append(item);
        }
    }
    endResetModel();
}

void LspCompletionModel::executeCompletionItem(KTextEditor::View *view,
                                               const KTextEditor::Range &word,
                                               const QModelIndex &index) const
{
    if (!view || index.row() < 0 || index.row() >= m_items.size()) {
        return;
    }
    view->document()->replaceText(word, m_items.at(index.row()).insertText);
}

QVariant LspCompletionModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || index.row() < 0 || index.row() >= m_items.size()) {
        return QVariant();
    }
    const Item &item = m_items.at(index.row());
    if (role == Qt::DisplayRole) {
        if (index.column() == Name) {
            return item.label;
        }
        if (index.column() == Arguments) {
            return item.detail;
        }
    }
    return QVariant();
}

QModelIndex LspCompletionModel::index(int row, int column, const QModelIndex &parent) const
{
    if (parent.isValid() || row < 0 || row >= m_items.size() || column < 0
        || column >= ColumnCount) {
        return QModelIndex();
    }
    return createIndex(row, column);
}

QModelIndex LspCompletionModel::parent(const QModelIndex &) const
{
    return QModelIndex();
}

int LspCompletionModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_items.size();
}

int LspCompletionModel::columnCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : ColumnCount;
}

bool LspCompletionModel::shouldAbortCompletion(KTextEditor::View *,
                                               const KTextEditor::Range &,
                                               const QString &currentCompletion)
{
    if (m_pending) {
        return false; // keep the popup open while results are still loading
    }
    for (const QChar &c : currentCompletion) {
        if (!c.isLetterOrNumber() && c != QLatin1Char('_')) {
            return true;
        }
    }
    return false;
}
