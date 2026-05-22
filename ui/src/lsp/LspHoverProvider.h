#pragma once

#include <KTextEditor/TextHintInterface>

#include <QObject>
#include <QString>

class LspClient;

// LspHoverProvider answers KTextEditor's text-hint requests with LSP hover
// information. textHint() fires textDocument/hover and returns empty; the async
// reply is shown as a tooltip at the cursor.
class LspHoverProvider : public QObject, public KTextEditor::TextHintProvider
{
    Q_OBJECT
public:
    LspHoverProvider(LspClient *client, const QString &path, QObject *parent = nullptr);

    QString textHint(KTextEditor::View *view, const KTextEditor::Cursor &position) override;

private:
    LspClient *m_client = nullptr;
    QString m_path;
};
