#include "LspCompletionModel.h"
#include "LspClient.h"
#include "LspManager.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Range>
#include <KTextEditor/View>

#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QPointer>
#include <QRegularExpression>
#include <QUrl>

namespace {
// LSP CompletionItemKind → a Breeze theme icon name. Mirrors VS Code's mapping.
QString iconNameForKind(int kind)
{
    switch (kind) {
    case 2:  // Method
    case 3:  // Function
        return QStringLiteral("code-function");
    case 4:  // Constructor
        return QStringLiteral("code-class");
    case 5:  // Field
    case 10: // Property
        return QStringLiteral("code-variable");
    case 6:  // Variable
        return QStringLiteral("code-variable");
    case 7:  // Class
    case 22: // Struct
        return QStringLiteral("code-class");
    case 8:  // Interface
        return QStringLiteral("code-class");
    case 9:  // Module
    case 11: // Unit
        return QStringLiteral("code-context");
    case 13: // Enum
    case 20: // EnumMember
        return QStringLiteral("code-class");
    case 14: // Keyword
        return QStringLiteral("code-context");
    case 21: // Constant
        return QStringLiteral("code-variable");
    case 15: // Snippet
        return QStringLiteral("text-x-generic");
    default:
        return QStringLiteral("code-context");
    }
}
} // namespace

LspCompletionModel::LspCompletionModel(LspClient *client, QObject *parent)
    : KTextEditor::CodeCompletionModel(parent)
    , m_client(client)
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
    const quint64 gen = ++m_completionGen;

    QPointer<LspCompletionModel> self(this);
    m_client->request(
        QStringLiteral("textDocument/completion"),
        QJsonObject{
            {QStringLiteral("textDocument"),
             QJsonObject{{QStringLiteral("uri"), QUrl::fromLocalFile(m_path).toString()}}},
            {QStringLiteral("position"),
             QJsonObject{{QStringLiteral("line"), pos.line()},
                         {QStringLiteral("character"), pos.column()}}}},
        [self, gen](const QJsonValue &result) {
            // Drop a stale/out-of-order reply: only the newest request may apply.
            if (self && gen == self->m_completionGen) {
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
        item.kind = o.value(QStringLiteral("kind")).toInt();
        // Documentation may be a plain string or a MarkupContent object.
        const QJsonValue doc = o.value(QStringLiteral("documentation"));
        if (doc.isString()) {
            item.documentation = doc.toString();
        } else if (doc.isObject()) {
            item.documentation = doc.toObject().value(QStringLiteral("value")).toString();
        }
        item.insertText = o.value(QStringLiteral("insertText")).toString();
        if (item.insertText.isEmpty()) {
            item.insertText = item.label;
        }
        item.snippet = o.value(QStringLiteral("insertTextFormat")).toInt(1) == 2;
        if (item.snippet) {
            // Drop ${n:placeholder} and $n markers for a plain insert.
            static const QRegularExpression braced(QStringLiteral("\\$\\{[^}]*\\}"));
            static const QRegularExpression bare(QStringLiteral("\\$\\d+"));
            item.insertText.remove(braced).remove(bare);
        }
        item.textEdit = o.value(QStringLiteral("textEdit")).toObject();
        item.additionalTextEdits = o.value(QStringLiteral("additionalTextEdits")).toArray();
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
    const Item &item = m_items.at(index.row());
    KTextEditor::Document *doc = view->document();

    KTextEditor::Document::EditingTransaction transaction(doc);
    // Apply the completion insertion FIRST, then the additional edits. The
    // additional edits (auto-imports) live above the cursor; applying the
    // (lower) insertion does not shift those higher lines, but applying the
    // imports first WOULD shift `word`/textEdit's plain ranges. So order matters.
    if (item.textEdit.isEmpty()) {
        doc->replaceText(word, item.insertText);
    } else {
        // Honour the server's precise replacement range when present.
        const QJsonObject range = item.textEdit.value(QStringLiteral("range")).toObject();
        const QJsonObject s = range.value(QStringLiteral("start")).toObject();
        const QJsonObject e = range.value(QStringLiteral("end")).toObject();
        QString newText = item.textEdit.value(QStringLiteral("newText")).toString();
        if (item.snippet) {
            static const QRegularExpression braced(QStringLiteral("\\$\\{[^}]*\\}"));
            static const QRegularExpression bare(QStringLiteral("\\$\\d+"));
            newText.remove(braced).remove(bare);
        }
        const KTextEditor::Range r(s.value(QStringLiteral("line")).toInt(),
                                   s.value(QStringLiteral("character")).toInt(),
                                   e.value(QStringLiteral("line")).toInt(),
                                   e.value(QStringLiteral("character")).toInt());
        doc->replaceText(r, newText);
    }
    if (m_manager && !item.additionalTextEdits.isEmpty()) {
        m_manager->applyEditsToPath(m_path, item.additionalTextEdits);
    }
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
    if (role == Qt::DecorationRole && index.column() == Icon) {
        return QIcon::fromTheme(iconNameForKind(item.kind));
    }
    if (role == KTextEditor::CodeCompletionModel::IsExpandable) {
        return !item.documentation.isEmpty();
    }
    if (role == KTextEditor::CodeCompletionModel::ExpandingWidget
        && !item.documentation.isEmpty()) {
        return item.documentation;
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
