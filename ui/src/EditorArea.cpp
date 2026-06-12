#include "EditorArea.h"
#include "CsvView.h"
#include "DiffView.h"
#include "ImageView.h"
#include "KPartView.h"
#include "RichTextView.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Editor>
#include <KTextEditor/View>

#include <QFileInfo>
#include <QLabel>
#include <QStackedWidget>
#include <QTabWidget>
#include <QUrl>
#include <QVBoxLayout>

EditorArea::EditorArea(QWidget *parent)
    : QWidget(parent)
    , m_stack(new QStackedWidget(this))
    , m_editor(KTextEditor::Editor::instance())
{
    auto *placeholder =
        new QLabel(QStringLiteral("Select a file in the project tree to start editing"));
    placeholder->setAlignment(Qt::AlignCenter);
    placeholder->setStyleSheet(QStringLiteral("color: palette(mid); font-size: 14px;"));
    m_placeholder = placeholder;
    m_stack->addWidget(m_placeholder);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_stack);
}

EditorArea::~EditorArea()
{
    // KTextEditor::Document and KTextEditor::View have different parent
    // chains here — the Document is a QObject child of EditorArea (set when
    // openFile() called createDocument(this)), while the View is a widget
    // child of the QTabWidget inside m_stack. Qt destroys QObject children
    // in reverse insertion order, so documents added at runtime get torn
    // down BEFORE the long-lived m_stack. The dangling Documents leave their
    // still-alive Views asserting on a destroyed model inside ~ViewPrivate
    // (Q_ASSERT(numBuckets > 0) on shutdown). Close every open tab first so
    // KTextEditor's documented "delete doc destroys its views" path runs
    // while the QStackedWidget is still intact.
    //
    // Block our own signals during teardown: removeTab() inside closeTabIn()
    // fires QTabBar::currentChanged → our groupTabs lambda → emitCurrentFile
    // → currentFileChanged. MainWindow listens on that signal and calls
    // m_lsp->requestSymbols(...), but m_lsp is a sibling child of MainWindow
    // created after m_editor, so it's already been destroyed by the time we
    // run — the listener would dereference a dangling pointer. None of the
    // shutdown listeners need to hear these signals anyway.
    QSignalBlocker blocker(this);
    const auto groups = m_groups.values();
    for (QTabWidget *tabs : groups) {
        while (tabs->count() > 0) {
            closeTabIn(tabs, 0);
        }
    }
}

QTabWidget *EditorArea::groupTabs(const QString &key, bool create)
{
    if (const auto it = m_groups.constFind(key); it != m_groups.constEnd()) {
        return it.value();
    }
    if (!create || key.isEmpty()) {
        return nullptr;
    }
    auto *tabs = new QTabWidget;
    tabs->setTabsClosable(true);
    tabs->setMovable(true);
    tabs->setDocumentMode(true);
    connect(tabs, &QTabWidget::tabCloseRequested, this,
            [this, tabs](int index) { closeTabIn(tabs, index); });
    connect(tabs, &QTabWidget::currentChanged, this, [this](int) { emitCurrentFile(); });
    m_groups.insert(key, tabs);
    m_stack->addWidget(tabs);
    return tabs;
}

QTabWidget *EditorArea::activeTabs() const
{
    return m_groups.value(m_activeGroup, nullptr);
}

void EditorArea::setActiveGroup(const QString &groupKey)
{
    m_activeGroup = groupKey;
    if (!groupKey.isEmpty()) {
        groupTabs(groupKey, true); // ensure the group exists
    }
    updateVisible();
    emitCurrentFile();
}

void EditorArea::updateVisible()
{
    QTabWidget *tabs = activeTabs();
    if (tabs && tabs->count() > 0) {
        m_stack->setCurrentWidget(tabs);
    } else {
        m_stack->setCurrentWidget(m_placeholder);
    }
}

void EditorArea::openFile(const QString &groupKey, const QString &path, int line)
{
    if (!m_editor) {
        emit statusMessage(QStringLiteral("KTextEditor engine unavailable"));
        return;
    }
    QTabWidget *tabs = groupTabs(groupKey, true);
    if (!tabs) {
        return;
    }
    const QString abs = QFileInfo(path).absoluteFilePath();

    for (int i = 0; i < tabs->count(); ++i) {
        QWidget *w = tabs->widget(i);
        if (auto *view = qobject_cast<KTextEditor::View *>(w)) {
            if (view->document()->url().toLocalFile() == abs) {
                tabs->setCurrentIndex(i);
                m_activeGroup = groupKey;
                updateVisible();
                if (line >= 0) {
                    view->setCursorPosition(KTextEditor::Cursor(line, 0));
                }
                return;
            }
        } else if (auto *md = qobject_cast<RichTextView *>(w)) {
            if (md->path() == abs) {
                tabs->setCurrentIndex(i);
                m_activeGroup = groupKey;
                updateVisible();
                if (line >= 0) {
                    md->view()->setCursorPosition(KTextEditor::Cursor(line, 0));
                }
                return;
            }
        } else if (auto *img = qobject_cast<ImageView *>(w)) {
            if (img->path() == abs) {
                tabs->setCurrentIndex(i);
                m_activeGroup = groupKey;
                updateVisible();
                return;
            }
        } else if (auto *csv = qobject_cast<CsvView *>(w)) {
            if (csv->path() == abs) {
                tabs->setCurrentIndex(i);
                m_activeGroup = groupKey;
                updateVisible();
                return;
            }
        } else if (auto *kpart = qobject_cast<KPartView *>(w)) {
            if (kpart->path() == abs) {
                tabs->setCurrentIndex(i);
                m_activeGroup = groupKey;
                updateVisible();
                return;
            }
        }
    }

    // File-type dispatch, in precedence order. RichTextView claims Markdown and
    // HTML; the ordered shape is: rich text → csv → image → KPart → text.
    if (RichTextView::canDisplay(abs)) {
        auto *md = new RichTextView(m_editor, abs, tabs);
        const int idx = tabs->addTab(md, QFileInfo(abs).fileName());
        tabs->setTabToolTip(idx, abs);
        tabs->setCurrentWidget(md);
        m_activeGroup = groupKey;
        updateVisible();
        if (line >= 0) {
            md->view()->setCursorPosition(KTextEditor::Cursor(line, 0));
        }
        // A real Kate document backs this tab, so the rest of the app (LSP,
        // Outline, Problems, Git gutter/blame) must see it open like any other.
        emit documentOpened(md->document(), abs);
        emit openFilesChanged();
        return;
    }

    if (CsvView::canDisplay(abs)) {
        auto *csv = new CsvView(abs, tabs);
        const int idx = tabs->addTab(csv, QFileInfo(abs).fileName());
        tabs->setTabToolTip(idx, abs);
        tabs->setCurrentWidget(csv);
        m_activeGroup = groupKey;
        updateVisible();
        emit openFilesChanged();
        emitCurrentFile();
        return;
    }

    if (ImageView::canDisplay(abs)) {
        auto *img = new ImageView(abs, tabs);
        const int idx = tabs->addTab(img, QFileInfo(abs).fileName());
        tabs->setTabToolTip(idx, abs);
        tabs->setCurrentWidget(img);
        m_activeGroup = groupKey;
        updateVisible();
        emit openFilesChanged();
        emitCurrentFile();
        return;
    }

    // Generic KDE viewer part (Okular for PDF/ODF/Office/ePub, Ark for archives,
    // KFontView for fonts, ...) — whatever the user has installed. Checked after
    // images so plain rasters stay in ImageView; KPartView::canDisplay already
    // excludes text/markdown/csv so source files fall through to KTextEditor.
    if (KPartView::canDisplay(abs)) {
        auto *kpart = new KPartView(abs, tabs);
        const int idx = tabs->addTab(kpart, QFileInfo(abs).fileName());
        tabs->setTabToolTip(idx, abs);
        tabs->setCurrentWidget(kpart);
        m_activeGroup = groupKey;
        updateVisible();
        emit openFilesChanged();
        emitCurrentFile();
        return;
    }

    KTextEditor::Document *doc = m_editor->createDocument(this);
    // Agents edit files on disk; reload silently rather than nagging the human.
    connect(doc, &KTextEditor::Document::modifiedOnDisk, this,
            [doc](KTextEditor::Document *, bool,
                  KTextEditor::Document::ModifiedOnDiskReason reason) {
                if (reason != KTextEditor::Document::OnDiskUnmodified && !doc->isModified()) {
                    doc->documentReload();
                }
            });
    doc->openUrl(QUrl::fromLocalFile(abs));

    KTextEditor::View *view = doc->createView(tabs);
    const int idx = tabs->addTab(view, QFileInfo(abs).fileName());
    tabs->setTabToolTip(idx, abs);
    tabs->setCurrentWidget(view);
    m_activeGroup = groupKey;
    updateVisible();
    if (line >= 0) {
        view->setCursorPosition(KTextEditor::Cursor(line, 0));
    }
    emit documentOpened(doc, abs);
    emit openFilesChanged();
}

void EditorArea::openDiff(const QString &groupKey, const QString &title, const QString &text)
{
    QTabWidget *tabs = groupTabs(groupKey, true);
    if (!tabs) {
        return;
    }
    auto *view = new DiffView(text, tabs);
    const int idx = tabs->addTab(view, title);
    tabs->setTabToolTip(idx, title);
    tabs->setCurrentWidget(view);
    m_activeGroup = groupKey;
    updateVisible();
}

bool EditorArea::saveCurrent()
{
    QTabWidget *tabs = activeTabs();
    if (!tabs) {
        return false;
    }
    KTextEditor::View *view = currentView();
    if (!view) {
        return false;
    }
    const bool ok = view->document()->documentSave();
    emit statusMessage(ok ? QStringLiteral("Saved %1").arg(view->document()->documentName())
                          : QStringLiteral("Save failed"));
    return ok;
}

KTextEditor::View *EditorArea::currentView() const
{
    if (QTabWidget *tabs = activeTabs()) {
        QWidget *w = tabs->currentWidget();
        if (auto *view = qobject_cast<KTextEditor::View *>(w)) {
            return view;
        }
        if (auto *md = qobject_cast<RichTextView *>(w)) {
            return md->view();
        }
    }
    return nullptr;
}

QStringList EditorArea::openFilePaths() const
{
    QStringList paths;
    for (QTabWidget *tabs : m_groups) {
        for (int i = 0; i < tabs->count(); ++i) {
            QWidget *w = tabs->widget(i);
            if (auto *view = qobject_cast<KTextEditor::View *>(w)) {
                const QString p = view->document()->url().toLocalFile();
                if (!p.isEmpty()) {
                    paths << p;
                }
            } else if (auto *md = qobject_cast<RichTextView *>(w)) {
                if (!md->path().isEmpty()) {
                    paths << md->path();
                }
            } else if (auto *img = qobject_cast<ImageView *>(w)) {
                if (!img->path().isEmpty()) {
                    paths << img->path();
                }
            } else if (auto *csv = qobject_cast<CsvView *>(w)) {
                if (!csv->path().isEmpty()) {
                    paths << csv->path();
                }
            } else if (auto *kpart = qobject_cast<KPartView *>(w)) {
                if (!kpart->path().isEmpty()) {
                    paths << kpart->path();
                }
            }
        }
    }
    return paths;
}

void EditorArea::closeTabIn(QTabWidget *tabs, int index)
{
    QWidget *widget = tabs->widget(index);
    if (auto *view = qobject_cast<KTextEditor::View *>(widget)) {
        KTextEditor::Document *doc = view->document();
        emit documentClosed(doc);
        tabs->removeTab(index);
        delete doc; // also destroys its views
    } else if (auto *md = qobject_cast<RichTextView *>(widget)) {
        // Owns a real document like a text tab: announce closure, tear the
        // document down first (which destroys its embedded view) — same
        // KTextEditor-safe ordering as the plain-view path — then the host.
        KTextEditor::Document *doc = md->document();
        emit documentClosed(doc);
        tabs->removeTab(index);
        delete doc;
        delete md;
    } else {
        tabs->removeTab(index);
        if (widget) {
            widget->deleteLater();
        }
    }
    updateVisible();
    emit openFilesChanged();
}

void EditorArea::emitCurrentFile()
{
    QString path;
    if (QTabWidget *tabs = activeTabs()) {
        QWidget *w = tabs->currentWidget();
        if (auto *view = qobject_cast<KTextEditor::View *>(w)) {
            path = view->document()->url().toLocalFile();
        } else if (auto *md = qobject_cast<RichTextView *>(w)) {
            path = md->path();
        } else if (auto *img = qobject_cast<ImageView *>(w)) {
            path = img->path();
        } else if (auto *csv = qobject_cast<CsvView *>(w)) {
            path = csv->path();
        } else if (auto *kpart = qobject_cast<KPartView *>(w)) {
            path = kpart->path();
        }
    }
    emit currentFileChanged(path);
}
