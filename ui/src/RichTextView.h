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

// RichTextView hosts a Markdown *or* HTML file inside an editor tab with three
// modes:
//
//   Raw     — just the editable KTextEditor view (identical to a normal text tab).
//   Preview — just the rendered document (QTextBrowser).
//   Split   — editor on the left, live preview on the right.
//
// The two formats share all of this machinery; only the render step differs:
// Markdown is parsed with QTextDocument::setMarkdown, while HTML is handed to the
// browser verbatim via setHtml. Both render through QTextBrowser, so the preview
// is Qt's rich-text subset (no JavaScript, limited CSS) — the same fidelity the
// assistant-message renderer uses.
//
// Unlike ImageView, this owns a *real* KTextEditor::Document, so raw/split edits
// are genuine Kate edits (highlighting, LSP, save) and the rest of the app
// (Outline, Problems, Git gutter, blame) treats the file like any other open
// document. EditorArea is responsible for emitting documentOpened/documentClosed
// for the Document this exposes via document().
class RichTextView : public QWidget
{
    Q_OBJECT
public:
    enum Mode { Raw, Preview, Split };
    enum Format { Markdown, Html };

    // Builds a document+view over `path` using the shared editor engine, the
    // same way EditorArea::openFile does for plain text tabs. The format is
    // derived from the file's suffix.
    RichTextView(KTextEditor::Editor *editor, const QString &path, QWidget *parent = nullptr);

    QString path() const { return m_path; }
    KTextEditor::Document *document() const { return m_doc; }
    KTextEditor::View *view() const { return m_view; }

    void setMode(Mode mode);
    Mode mode() const { return m_mode; }

    // True for Markdown and HTML files. formatFor returns which of the two a
    // given path is (only meaningful when canDisplay is true).
    static bool canDisplay(const QString &path);
    static Format formatFor(const QString &path);

protected:
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void render();
    void scheduleRender();
    // Clamp oversized preview images to the pane width (GitHub's max-width:100%),
    // preserving aspect ratio and never enlarging past the image's natural size.
    void constrainImages();

    QString m_path;
    Format m_format = Markdown;
    KTextEditor::Document *m_doc = nullptr;
    KTextEditor::View *m_view = nullptr;
    QTextBrowser *m_preview = nullptr;
    QSplitter *m_splitter = nullptr;
    QTimer *m_debounce = nullptr;
    QTimer *m_relayout = nullptr;
    Mode m_mode = Preview;
    bool m_previewDirty = true;
};
