#include "RichTextView.h"

#include <KConfigGroup>
#include <KSharedConfig>
#include <KTextEditor/Document>
#include <KTextEditor/Editor>
#include <KTextEditor/View>

#include <QActionGroup>
#include <QDesktopServices>
#include <QDir>
#include <QEvent>
#include <QFileInfo>
#include <QImageReader>
#include <QSplitter>
#include <QTextBlock>
#include <QTextBrowser>
#include <QTextCursor>
#include <QTextDocument>
#include <QTimer>
#include <QToolBar>
#include <QUrl>
#include <QVBoxLayout>

namespace {
constexpr int kRenderDebounceMs = 200;

// The persisted (global) last-used mode, so reopening rich-text files keeps the
// human's preference like the other sticky UI prefs. Markdown and HTML remember
// their mode independently (a reader may want HTML previewed but Markdown raw).
KConfigGroup modeConfig(RichTextView::Format format)
{
    const QString group = format == RichTextView::Html ? QStringLiteral("HtmlView")
                                                        : QStringLiteral("RichTextView");
    return KSharedConfig::openConfig()->group(group);
}

// markdownToHtml renders Markdown to an HTML fragment, mirroring the helper used
// for assistant messages (AgentPanel.cpp). Default-coloured text carries no
// explicit colour so it inherits the browser's palette text colour.
QString markdownToHtml(const QString &md)
{
    QTextDocument doc;
    doc.setMarkdown(md, QTextDocument::MarkdownDialectGitHub);
    return doc.toHtml();
}

// naturalImageSize returns the on-disk pixel dimensions of a Markdown image
// reference, resolving relative paths against the document's directory. Only the
// image header is read (cheap). Returns an invalid size for anything it can't
// resolve to a local file (e.g. remote http images, which the offline preview
// never loads anyway) so the caller leaves those untouched.
QSize naturalImageSize(const QString &name, const QString &baseDir)
{
    const QUrl url(name);
    QString file;
    if (url.isLocalFile()) {
        file = url.toLocalFile();
    } else if (url.isRelative()) {
        file = QDir(baseDir).filePath(name);
    } else {
        return {}; // remote or unsupported scheme
    }
    return QImageReader(file).size();
}
} // namespace

bool RichTextView::canDisplay(const QString &path)
{
    const QString suffix = QFileInfo(path).suffix().toLower();
    return suffix == QLatin1String("md") || suffix == QLatin1String("markdown")
        || suffix == QLatin1String("mdown") || suffix == QLatin1String("mkd")
        || suffix == QLatin1String("html") || suffix == QLatin1String("htm");
}

RichTextView::Format RichTextView::formatFor(const QString &path)
{
    const QString suffix = QFileInfo(path).suffix().toLower();
    return (suffix == QLatin1String("html") || suffix == QLatin1String("htm")) ? Html : Markdown;
}

RichTextView::RichTextView(KTextEditor::Editor *editor, const QString &path, QWidget *parent)
    : QWidget(parent)
    , m_path(QFileInfo(path).absoluteFilePath())
    , m_format(formatFor(path))
{
    // A genuine Kate document, parented to this widget so its lifetime is bound
    // to the tab. EditorArea emits documentOpened/documentClosed for it so LSP,
    // Outline and Git see the file like any other text document.
    m_doc = editor->createDocument(this);
    // Agents edit files on disk; reload silently rather than nagging the human
    // (same policy as EditorArea's plain-text path).
    connect(m_doc, &KTextEditor::Document::modifiedOnDisk, this,
            [this](KTextEditor::Document *, bool,
                   KTextEditor::Document::ModifiedOnDiskReason reason) {
                if (reason != KTextEditor::Document::OnDiskUnmodified && !m_doc->isModified()) {
                    m_doc->documentReload();
                }
            });
    m_doc->openUrl(QUrl::fromLocalFile(m_path));

    m_splitter = new QSplitter(Qt::Horizontal, this);
    m_view = m_doc->createView(m_splitter);
    m_splitter->addWidget(m_view);

    m_preview = new QTextBrowser(m_splitter);
    m_preview->setOpenLinks(false); // route clicks ourselves (below)
    m_preview->setFrameStyle(QFrame::NoFrame);
    // Resolve relative image links against the file's directory.
    m_preview->setSearchPaths({QFileInfo(m_path).absolutePath()});
    connect(m_preview, &QTextBrowser::anchorClicked, this, [](const QUrl &url) {
        if (url.scheme() == QLatin1String("http") || url.scheme() == QLatin1String("https")
            || url.scheme() == QLatin1String("mailto")) {
            QDesktopServices::openUrl(url);
        }
    });
    m_splitter->addWidget(m_preview);
    m_splitter->setStretchFactor(0, 1);
    m_splitter->setStretchFactor(1, 1);

    // Re-fit images to the pane width (GitHub-style max-width:100%) whenever the
    // preview viewport changes size — splitter drag, window resize, mode switch.
    // Debounced so an interactive resize settles before we relayout the document.
    m_relayout = new QTimer(this);
    m_relayout->setSingleShot(true);
    m_relayout->setInterval(50);
    connect(m_relayout, &QTimer::timeout, this, &RichTextView::constrainImages);
    m_preview->viewport()->installEventFilter(this);

    // Debounced live re-render while typing, like SearchPanel's search debounce.
    m_debounce = new QTimer(this);
    m_debounce->setSingleShot(true);
    m_debounce->setInterval(kRenderDebounceMs);
    connect(m_debounce, &QTimer::timeout, this, &RichTextView::render);
    connect(m_doc, &KTextEditor::Document::textChanged, this, &RichTextView::scheduleRender);

    // Toolbar: three mutually-exclusive, checkable mode actions.
    auto *toolbar = new QToolBar(this);
    toolbar->setIconSize(QSize(16, 16));
    auto *modeGroup = new QActionGroup(this);
    modeGroup->setExclusive(true);

    auto addMode = [&](const QString &label, Mode mode) {
        auto *action = toolbar->addAction(label);
        action->setCheckable(true);
        modeGroup->addAction(action);
        connect(action, &QAction::triggered, this, [this, mode] { setMode(mode); });
        return action;
    };
    auto *rawAction = addMode(tr("Raw"), Raw);
    auto *previewAction = addMode(tr("Preview"), Preview);
    auto *splitAction = addMode(tr("Split"), Split);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(toolbar);
    layout->addWidget(m_splitter, 1);

    const int saved = modeConfig(m_format).readEntry("mode", static_cast<int>(Preview));
    m_mode = static_cast<Mode>(qBound<int>(Raw, saved, Split));
    switch (m_mode) {
    case Raw:
        rawAction->setChecked(true);
        break;
    case Preview:
        previewAction->setChecked(true);
        break;
    case Split:
        splitAction->setChecked(true);
        break;
    }
    setMode(m_mode);
}

void RichTextView::setMode(Mode mode)
{
    m_mode = mode;
    modeConfig(m_format).writeEntry("mode", static_cast<int>(mode));

    const bool showEditor = (mode == Raw || mode == Split);
    const bool showPreview = (mode == Preview || mode == Split);
    m_view->setVisible(showEditor);
    m_preview->setVisible(showPreview);

    if (showPreview && m_previewDirty) {
        render();
    }
    if (showEditor) {
        m_view->setFocus();
    }
}

void RichTextView::scheduleRender()
{
    m_previewDirty = true;
    // Only spend cycles rendering when the preview pane is actually visible.
    if (m_preview->isVisible()) {
        m_debounce->start();
    }
}

void RichTextView::render()
{
    // Markdown is parsed to HTML; an .html file is already HTML, so feed it to
    // the browser as-is. Either way QTextBrowser renders Qt's rich-text subset.
    const QString html = m_format == Html ? m_doc->text() : markdownToHtml(m_doc->text());
    m_preview->setHtml(html);
    constrainImages();
    m_previewDirty = false;
}

void RichTextView::constrainImages()
{
    QTextDocument *doc = m_preview->document();
    // Available content width = viewport minus the document's left+right margins.
    const int avail = int(m_preview->viewport()->width() - 2 * doc->documentMargin());
    if (avail <= 0) {
        return; // pane not laid out yet; a resize event will re-run us
    }
    const QString baseDir = QFileInfo(m_path).absolutePath();

    // Collect first, mutate after: editing char formats while iterating the
    // fragment lists would invalidate the iterators.
    struct Pending {
        int pos;
        int len;
        QTextImageFormat fmt;
    };
    QList<Pending> pending;

    for (QTextBlock block = doc->begin(); block.isValid(); block = block.next()) {
        for (auto it = block.begin(); !it.atEnd(); ++it) {
            const QTextFragment frag = it.fragment();
            if (!frag.isValid() || !frag.charFormat().isImageFormat()) {
                continue;
            }
            QTextImageFormat img = frag.charFormat().toImageFormat();
            const QSize natural = naturalImageSize(img.name(), baseDir);
            if (natural.width() <= 0 || natural.height() <= 0) {
                continue; // unresolvable (remote/data) — leave as-is
            }

            const bool hasExplicit = img.hasProperty(QTextFormat::ImageWidth);
            if (natural.width() > avail) {
                // Too wide: scale down to the column, preserving aspect ratio.
                const qreal w = avail;
                const qreal h = natural.height() * (w / natural.width());
                if (hasExplicit && qRound(img.width()) == qRound(w)) {
                    continue; // already clamped correctly
                }
                img.setWidth(w);
                img.setHeight(h);
            } else {
                // Fits: render at natural size (drop any earlier clamp).
                if (!hasExplicit) {
                    continue;
                }
                img.clearProperty(QTextFormat::ImageWidth);
                img.clearProperty(QTextFormat::ImageHeight);
            }
            pending.append({frag.position(), frag.length(), img});
        }
    }

    if (pending.isEmpty()) {
        return;
    }
    QTextCursor cur(doc);
    cur.beginEditBlock();
    for (const Pending &p : pending) {
        cur.setPosition(p.pos);
        cur.setPosition(p.pos + p.len, QTextCursor::KeepAnchor);
        cur.setCharFormat(p.fmt);
    }
    cur.endEditBlock();
}

bool RichTextView::eventFilter(QObject *watched, QEvent *event)
{
    if (watched == m_preview->viewport() && event->type() == QEvent::Resize) {
        m_relayout->start();
    }
    return QWidget::eventFilter(watched, event);
}
