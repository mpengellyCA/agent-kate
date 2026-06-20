#include "LspHoverProvider.h"
#include "LspClient.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/View>

#include <QCursor>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QPointer>
#include <QStringList>
#include <QTextDocument>
#include <QToolTip>
#include <QUrl>

namespace {
struct HoverContent {
    QString text;
    bool markdown = false;
};

// extractHover flattens an LSP Hover result's "contents" and reports whether the
// payload was MarkupContent of kind "markdown".
HoverContent extractHover(const QJsonValue &result)
{
    HoverContent out;
    const QJsonValue contents = result.toObject().value(QStringLiteral("contents"));
    if (contents.isString()) {
        out.text = contents.toString();
    } else if (contents.isObject()) {
        const QJsonObject o = contents.toObject();
        out.text = o.value(QStringLiteral("value")).toString();
        out.markdown = o.value(QStringLiteral("kind")).toString() == QLatin1String("markdown");
    } else if (contents.isArray()) {
        QStringList parts;
        for (const QJsonValue &v : contents.toArray()) {
            if (v.isString()) {
                parts << v.toString();
            } else if (v.isObject()) {
                parts << v.toObject().value(QStringLiteral("value")).toString();
            }
        }
        out.text = parts.join(QStringLiteral("\n\n"));
    }
    return out;
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
            const HoverContent hover = extractHover(result);
            const QString trimmed = hover.text.trimmed();
            if (trimmed.isEmpty()) {
                return;
            }
            // The reply lands a moment after the hover paused; show it where the
            // pointer rests. Markdown is converted to HTML so the tooltip
            // renders headings/code rather than raw markup.
            QString tip = trimmed;
            if (hover.markdown) {
                QTextDocument md;
                md.setMarkdown(trimmed);
                tip = md.toHtml();
            }
            QToolTip::showText(QCursor::pos(), tip);
        });
    // Shown asynchronously above — nothing for KTextEditor to display now.
    return QString();
}
