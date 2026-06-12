#pragma once

#include <QString>
#include <QWidget>

namespace KTextEditor {
class Document;
class Editor;
class View;
}
class QTextBrowser;
class QSplitter;
class QTimer;

// MarkdownView hosts a Markdown file inside an editor tab with three modes:
//
//   Raw     — just the editable KTextEditor view (identical to a normal text tab).
//   Preview — just the rendered Markdown (QTextDocument::setMarkdown → QTextBrowser).
//   Split   — editor on the left, live preview on the right.
//
// Unlike ImageView, this owns a *real* KTextEditor::Document, so raw/split edits
// are genuine Kate edits (highlighting, LSP, save) and the rest of the app
// (Outline, Problems, Git gutter, blame) treats the file like any other open
// document. EditorArea is responsible for emitting documentOpened/documentClosed
// for the Document this exposes via document().
class MarkdownView : public QWidget
{
    Q_OBJECT
public:
    enum Mode { Raw, Preview, Split };

    // Builds a document+view over `path` using the shared editor engine, the
    // same way EditorArea::openFile does for plain text tabs.
    MarkdownView(KTextEditor::Editor *editor, const QString &path, QWidget *parent = nullptr);

    QString path() const { return m_path; }
    KTextEditor::Document *document() const { return m_doc; }
    KTextEditor::View *view() const { return m_view; }

    void setMode(Mode mode);
    Mode mode() const { return m_mode; }

    static bool canDisplay(const QString &path);

protected:
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void render();
    void scheduleRender();
    // Clamp oversized preview images to the pane width (GitHub's max-width:100%),
    // preserving aspect ratio and never enlarging past the image's natural size.
    void constrainImages();

    QString m_path;
    KTextEditor::Document *m_doc = nullptr;
    KTextEditor::View *m_view = nullptr;
    QTextBrowser *m_preview = nullptr;
    QSplitter *m_splitter = nullptr;
    QTimer *m_debounce = nullptr;
    QTimer *m_relayout = nullptr;
    Mode m_mode = Preview;
    bool m_previewDirty = true;
};
