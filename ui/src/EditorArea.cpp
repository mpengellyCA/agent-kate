#include "EditorArea.h"
#include "CsvView.h"
#include "DiffView.h"
#include "ImageView.h"
#include "KPartView.h"
#include "RichTextView.h"

#include <KLocalizedString>
#include <KMessageBox>
#include <KMessageWidget>
#include <KStandardGuiItem>
#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Editor>
#include <KTextEditor/View>

#include <QAction>
#include <QApplication>
#include <QClipboard>
#include <QDesktopServices>
#include <QDir>
#include <QFileInfo>
#include <QIcon>
#include <QLabel>
#include <QMenu>
#include <QStackedWidget>
#include <QTabBar>
#include <QTabWidget>
#include <QUrl>
#include <QVBoxLayout>

namespace {
// Property key marking a container QWidget that wraps a bare KTextEditor::View
// beneath its reload banner. viewForTab() looks the View up as a direct child.
constexpr const char *kWrappedViewTab = "editorarea_wrapped_view";
} // namespace

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
    //
    // CRITICAL: pass interactive=false. The save-on-close prompt must never
    // fire from the destructor — it would deref already-torn-down siblings
    // (KMessageBox spins an event loop) and there's no human-meaningful close
    // happening here anyway.
    QSignalBlocker blocker(this);
    const auto groups = m_groups.values();
    for (QTabWidget *tabs : groups) {
        while (tabs->count() > 0) {
            closeTabIn(tabs, 0, /*interactive=*/false);
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
            [this, tabs](int index) { closeTabIn(tabs, index, /*interactive=*/true); });
    connect(tabs, &QTabWidget::currentChanged, this, [this](int) { emitCurrentFile(); });

    // Right-click a tab for close/copy-path/reveal actions.
    QTabBar *bar = tabs->tabBar();
    bar->setContextMenuPolicy(Qt::CustomContextMenu);
    connect(bar, &QWidget::customContextMenuRequested, this,
            [this, tabs, bar](const QPoint &pos) {
                const int index = bar->tabAt(pos);
                if (index < 0) {
                    return;
                }
                const QString path = pathForTab(tabs->widget(index));
                QMenu menu;
                QAction *closeAct =
                    menu.addAction(QIcon::fromTheme(QStringLiteral("tab-close")), i18n("Close"));
                QAction *closeOthers = menu.addAction(i18n("Close Others"));
                QAction *closeRight = menu.addAction(i18n("Close to the Right"));
                closeOthers->setEnabled(tabs->count() > 1);
                closeRight->setEnabled(index < tabs->count() - 1);
                menu.addSeparator();
                QAction *copyPath = menu.addAction(
                    QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy Path"));
                QAction *copyRel = menu.addAction(i18n("Copy Relative Path"));
                QAction *reveal = menu.addAction(
                    QIcon::fromTheme(QStringLiteral("view-list-tree")), i18n("Reveal in Tree"));
                copyPath->setEnabled(!path.isEmpty());
                copyRel->setEnabled(!path.isEmpty());
                reveal->setEnabled(!path.isEmpty());

                QAction *chosen = menu.exec(bar->mapToGlobal(pos));
                if (!chosen) {
                    return;
                }
                if (chosen == closeAct) {
                    closeTabIn(tabs, index, /*interactive=*/true);
                } else if (chosen == closeOthers) {
                    // Iterate right-to-left so removeTab index shifts don't
                    // skip tabs; keep only the originally-clicked widget.
                    QWidget *keep = tabs->widget(index);
                    for (int i = tabs->count() - 1; i >= 0; --i) {
                        if (tabs->widget(i) != keep) {
                            if (!closeTabIn(tabs, i, /*interactive=*/true)) {
                                break; // user cancelled a save prompt
                            }
                        }
                    }
                } else if (chosen == closeRight) {
                    for (int i = tabs->count() - 1; i > index; --i) {
                        if (!closeTabIn(tabs, i, /*interactive=*/true)) {
                            break;
                        }
                    }
                } else if (chosen == copyPath) {
                    QApplication::clipboard()->setText(path);
                } else if (chosen == copyRel) {
                    // The group key is a project directory only when tabs are
                    // grouped by project; in agent-grouped mode it's "agent-N",
                    // which is not a base dir — fall back to the absolute path.
                    const QDir base(m_activeGroup);
                    const QString rel = base.exists()
                        ? base.relativeFilePath(path) : path;
                    QApplication::clipboard()->setText(rel.isEmpty() ? path : rel);
                } else if (chosen == reveal) {
                    emit revealInTreeRequested(path);
                }
            });

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

KTextEditor::View *EditorArea::viewForTab(QWidget *tabWidget) const
{
    if (!tabWidget) {
        return nullptr;
    }
    if (auto *view = qobject_cast<KTextEditor::View *>(tabWidget)) {
        return view;
    }
    if (auto *md = qobject_cast<RichTextView *>(tabWidget)) {
        return md->view();
    }
    // Plain-text tabs are wrapped in a thin container (the reload banner sits
    // above the View) — find the View child directly.
    if (tabWidget->property(kWrappedViewTab).toBool()) {
        return tabWidget->findChild<KTextEditor::View *>(
            QString(), Qt::FindDirectChildrenOnly);
    }
    return nullptr;
}

QString EditorArea::pathForTab(QWidget *w) const
{
    if (KTextEditor::View *view = viewForTab(w)) {
        return view->document()->url().toLocalFile();
    }
    if (auto *img = qobject_cast<ImageView *>(w)) {
        return img->path();
    }
    if (auto *csv = qobject_cast<CsvView *>(w)) {
        return csv->path();
    }
    if (auto *kpart = qobject_cast<KPartView *>(w)) {
        return kpart->path();
    }
    return {};
}

void EditorArea::updateTabIcon(KTextEditor::Document *doc)
{
    // Find the tab hosting this document across all groups and mark it dirty.
    for (QTabWidget *tabs : std::as_const(m_groups)) {
        for (int i = 0; i < tabs->count(); ++i) {
            KTextEditor::View *view = viewForTab(tabs->widget(i));
            if (view && view->document() == doc) {
                tabs->setTabIcon(i, doc->isModified()
                                        ? QIcon::fromTheme(QStringLiteral("document-save"))
                                        : QIcon());
                return;
            }
        }
    }
}

void EditorArea::wireDocument(KTextEditor::Document *doc, QTabWidget *tabs, QWidget *bannerHost)
{
    Q_UNUSED(tabs);
    // Dirty indicator: a save icon appears on the tab while modified.
    connect(doc, &KTextEditor::Document::modifiedChanged, this,
            [this, doc] { updateTabIcon(doc); });

    // Modified-on-disk handler. Silent reload is safe ONLY when the human has
    // no unsaved edits. When the buffer is modified and the file changed under
    // us (an agent rewrote it), surface a banner instead of discarding edits.
    connect(doc, &KTextEditor::Document::modifiedOnDisk, this,
            [this, doc, bannerHost](KTextEditor::Document *, bool,
                                    KTextEditor::Document::ModifiedOnDiskReason reason) {
                if (reason == KTextEditor::Document::OnDiskUnmodified) {
                    return;
                }
                if (!doc->isModified()) {
                    doc->documentReload();
                    return;
                }
                if (!bannerHost) {
                    return;
                }
                auto *box = bannerHost->layout();
                if (!box) {
                    return;
                }
                // Reuse an existing banner on this host rather than stacking.
                auto *banner = bannerHost->findChild<KMessageWidget *>(
                    QStringLiteral("editorReloadBanner"), Qt::FindDirectChildrenOnly);
                if (!banner) {
                    banner = new KMessageWidget(bannerHost);
                    banner->setObjectName(QStringLiteral("editorReloadBanner"));
                    banner->setMessageType(KMessageWidget::Warning);
                    banner->setCloseButtonVisible(true);
                    banner->setWordWrap(true);
                    auto *reloadAct = new QAction(
                        QIcon::fromTheme(QStringLiteral("view-refresh")),
                        i18n("Reload"), banner);
                    connect(reloadAct, &QAction::triggered, doc,
                            [doc] { doc->documentReload(); });
                    auto *keepAct = new QAction(
                        i18n("Keep My Version"), banner);
                    connect(keepAct, &QAction::triggered, banner,
                            [banner] { banner->animatedHide(); });
                    banner->addAction(reloadAct);
                    banner->addAction(keepAct);
                    // Hide automatically once the buffer is clean again (a
                    // successful reload clears isModified()).
                    connect(doc, &KTextEditor::Document::modifiedChanged, banner,
                            [doc, banner] {
                                if (!doc->isModified()) {
                                    banner->animatedHide();
                                }
                            });
                    box->addWidget(banner);
                    // Float to the top of the host's vertical layout.
                    if (auto *vbox = qobject_cast<QVBoxLayout *>(box)) {
                        vbox->removeWidget(banner);
                        vbox->insertWidget(0, banner);
                    }
                }
                QString text;
                switch (reason) {
                case KTextEditor::Document::OnDiskDeleted:
                    text = i18n("This file was deleted on disk, but you have "
                                "unsaved changes. Keep your version or discard it?");
                    break;
                case KTextEditor::Document::OnDiskCreated:
                    text = i18n("This file was created on disk while you have "
                                "unsaved changes. Reload it or keep your version?");
                    break;
                default:
                    text = i18n("This file was modified on disk (likely by an "
                                "agent) and you have unsaved changes. Reload to "
                                "take the new version, or keep yours.");
                    break;
                }
                banner->setText(text);
                banner->animatedShow();
            });
}

void EditorArea::openFile(const QString &groupKey, const QString &path, int line,
                          int column)
{
    if (column < 0) {
        column = 0;
    }
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
        if (KTextEditor::View *view = viewForTab(w)) {
            if (view->document()->url().toLocalFile() == abs) {
                tabs->setCurrentIndex(i);
                m_activeGroup = groupKey;
                updateVisible();
                if (line >= 0) {
                    view->setCursorPosition(KTextEditor::Cursor(line, column));
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
        // RichTextView's own modifiedOnDisk banner lives above its splitter;
        // here we wire the dirty-tab indicator on the document it exposes.
        connect(md->document(), &KTextEditor::Document::modifiedChanged, this,
                [this, doc = md->document()] { updateTabIcon(doc); });
        if (line >= 0) {
            md->view()->setCursorPosition(KTextEditor::Cursor(line, column));
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

    // Wrap the bare View in a thin container so the reload banner can sit above
    // it. This is the one structural change driven by the modified-on-disk
    // banner; every qobject_cast<View*> site resolves the View via viewForTab.
    auto *container = new QWidget(tabs);
    container->setProperty(kWrappedViewTab, true);
    auto *vbox = new QVBoxLayout(container);
    vbox->setContentsMargins(0, 0, 0, 0);
    vbox->setSpacing(0);

    KTextEditor::View *view = doc->createView(container);
    vbox->addWidget(view);

    wireDocument(doc, tabs, container);

    const bool opened = doc->openUrl(QUrl::fromLocalFile(abs));

    const int idx = tabs->addTab(container, QFileInfo(abs).fileName());
    tabs->setTabToolTip(idx, abs);
    tabs->setCurrentWidget(container);
    m_activeGroup = groupKey;
    updateVisible();
    if (line >= 0) {
        view->setCursorPosition(KTextEditor::Cursor(line, column));
    }

    // Surface a failed/unreadable open instead of leaving an empty editor.
    const QFileInfo fi(abs);
    if (!opened || (fi.exists() && !fi.isReadable())) {
        auto *banner = new KMessageWidget(container);
        banner->setMessageType(KMessageWidget::Error);
        banner->setCloseButtonVisible(true);
        banner->setWordWrap(true);
        banner->setText(i18n("Could not open %1.", fi.fileName()));
        auto *openExt =
            new QAction(QIcon::fromTheme(QStringLiteral("document-open")),
                        i18n("Open Externally"), banner);
        connect(openExt, &QAction::triggered, banner, [abs] {
            QDesktopServices::openUrl(QUrl::fromLocalFile(abs));
        });
        banner->addAction(openExt);
        vbox->insertWidget(0, banner);
        banner->animatedShow();
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

bool EditorArea::saveAll()
{
    bool allOk = true;
    int saved = 0;
    for (QTabWidget *tabs : std::as_const(m_groups)) {
        for (int i = 0; i < tabs->count(); ++i) {
            KTextEditor::View *view = viewForTab(tabs->widget(i));
            if (!view) {
                continue;
            }
            KTextEditor::Document *doc = view->document();
            if (doc->isModified() && !doc->url().isEmpty()) {
                if (doc->documentSave()) {
                    ++saved;
                } else {
                    allOk = false;
                }
            }
        }
    }
    if (saved > 0) {
        emit statusMessage(i18np("Saved %1 file", "Saved %1 files", saved));
    }
    return allOk;
}

bool EditorArea::confirmCloseAll()
{
    // Prompt for every modified local document. Reuse the interactive close
    // path so the Save/Discard/Cancel semantics match tab close exactly.
    for (QTabWidget *tabs : std::as_const(m_groups)) {
        for (int i = tabs->count() - 1; i >= 0; --i) {
            KTextEditor::View *view = viewForTab(tabs->widget(i));
            if (!view) {
                continue;
            }
            KTextEditor::Document *doc = view->document();
            if (doc->isModified() && !doc->url().isEmpty()) {
                if (!closeTabIn(tabs, i, /*interactive=*/true)) {
                    return false; // user cancelled — abort the window close
                }
            }
        }
    }
    return true;
}

KTextEditor::View *EditorArea::currentView() const
{
    if (QTabWidget *tabs = activeTabs()) {
        return viewForTab(tabs->currentWidget());
    }
    return nullptr;
}

QStringList EditorArea::openFilePaths() const
{
    QStringList paths;
    for (QTabWidget *tabs : m_groups) {
        for (int i = 0; i < tabs->count(); ++i) {
            const QString p = pathForTab(tabs->widget(i));
            if (!p.isEmpty()) {
                paths << p;
            }
        }
    }
    return paths;
}

QStringList EditorArea::groupKeys() const
{
    return m_groups.keys();
}

QStringList EditorArea::openFilePathsForGroup(const QString &key) const
{
    QStringList paths;
    if (QTabWidget *tabs = m_groups.value(key, nullptr)) {
        for (int i = 0; i < tabs->count(); ++i) {
            const QString p = pathForTab(tabs->widget(i));
            if (!p.isEmpty()) {
                paths << p;
            }
        }
    }
    return paths;
}

QString EditorArea::currentPathForGroup(const QString &key) const
{
    if (QTabWidget *tabs = m_groups.value(key, nullptr)) {
        return pathForTab(tabs->currentWidget());
    }
    return {};
}

bool EditorArea::closeTabIn(QTabWidget *tabs, int index, bool interactive)
{
    QWidget *widget = tabs->widget(index);
    if (KTextEditor::View *view = viewForTab(widget)) {
        KTextEditor::Document *doc = view->document();
        // Save-prompt only on the interactive path (never the destructor).
        if (interactive && doc->isModified() && !doc->url().isEmpty()) {
            const auto answer = KMessageBox::warningTwoActionsCancel(
                this,
                i18n("The document \"%1\" has been modified. Save your changes "
                     "or discard them?",
                     doc->documentName()),
                i18n("Close Document"),
                KStandardGuiItem::save(), KStandardGuiItem::discard());
            if (answer == KMessageBox::Cancel) {
                return false;
            }
            if (answer == KMessageBox::PrimaryAction && !doc->documentSave()) {
                return false; // save failed — keep the tab so edits aren't lost
            }
        }
        emit documentClosed(doc);
        tabs->removeTab(index);
        // The View may be wrapped in a container; tear the document down first
        // (destroys its views), then the now-empty container.
        if (auto *md = qobject_cast<RichTextView *>(widget)) {
            delete doc;
            delete md;
        } else if (widget && widget->property(kWrappedViewTab).toBool()) {
            delete doc; // destroys the View child
            delete widget; // the empty container
        } else {
            delete doc; // bare view path (defensive; not currently produced)
        }
    } else {
        tabs->removeTab(index);
        if (widget) {
            widget->deleteLater();
        }
    }
    // Prune a group once its last tab is gone, so a long session can't
    // accumulate dead empty QTabWidgets in m_groups. The destructor drains
    // every group through here too, but it iterates a values() *copy*, so
    // mutating m_groups (and tearing the now-empty tab widget down) underneath
    // it is safe. With no group left active, activeTabs() returns nullptr and
    // updateVisible() falls back to the placeholder — no "keep one" invariant.
    if (tabs->count() == 0) {
        const QString key = m_groups.key(tabs);
        if (!key.isEmpty()) {
            m_groups.remove(key);
            if (m_activeGroup == key) {
                m_activeGroup.clear();
            }
            m_stack->removeWidget(tabs);
            tabs->deleteLater();
        }
    }
    updateVisible();
    emit openFilesChanged();
    return true;
}

void EditorArea::emitCurrentFile()
{
    QString path;
    if (QTabWidget *tabs = activeTabs()) {
        path = pathForTab(tabs->currentWidget());
    }
    emit currentFileChanged(path);
}
