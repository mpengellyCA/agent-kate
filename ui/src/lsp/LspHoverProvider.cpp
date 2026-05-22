#include "LspHoverProvider.h"
#include "LspClient.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/View>

#include <QCursor>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QPointer>
#include <QToolTip>
#include <QUrl>

namespace {
// extractHover flattens an LSP Hover result's "contents" to plain text.
QString extractHover(const QJsonValue &result)
{
    const QJsonValue contents = result.toObject().value(QStringLiteral("contents"));
    if (contents.isString()) {
        return contents.toString();
    }
    if (contents.isObject()) {
        return contents.toObject().value(QStringLiteral("value")).toString();
    }
    if (contents.isArray()) {
        QStringList parts;
        for (const QJsonValue &v : contents.toArray()) {
            if (v.isString()) {
                parts << v.toString();
            } else if (v.isObject()) {
                parts << v.toObject().value(QStringLiteral("value")).toString();
            }
        }
        return parts.join(QStringLiteral("\n"));
    }
    return QString();
}
} // namespace

LspHoverProvider::LspHoverProvider(LspClient *client, const QString &path, QObject *parent)
    : QObject(parent)
    , m_client(client)
    , m_path(path)
{
}

QString LspHoverProvider::textHint(KTextEditor::View *view, const KTextEditor::Cursor &position)
{
    if (!m_client || !view) {
        return QString();
    }
    QPointer<LspHoverProvider> self(this);
    m_client->request(
        QStringLiteral("textDocument/hover"),
        QJsonObject{
            {QStringLiteral("textDocument"),
             QJsonObject{{QStringLiteral("uri"), QUrl::fromLocalFile(m_path).toString()}}},
            {QStringLiteral("position"),
             QJsonObject{{QStringLiteral("line"), position.line()},
                         {QStringLiteral("character"), position.column()}}}},
        [self](const QJsonValue &result) {
            if (!self) {
                return;
            }
            const QString text = extractHover(result).trimmed();
            if (!text.isEmpty()) {
                // The reply lands a moment after the hover paused; show it
                // where the pointer rests.
                QToolTip::showText(QCursor::pos(), text);
            }
        });
    // Shown asynchronously above — nothing for KTextEditor to display now.
    return QString();
}
