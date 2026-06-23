#include "ProjectTree.h"

#include "FileFilterProxyModel.h"
#include "GitStatusDelegate.h"
#include "ipc/CoreClient.h"

#include <KConfigGroup>
#include <KIO/CopyJob>
#include <KIO/DeleteOrTrashJob>
#include <KIO/OpenFileManagerWindowJob>
#include <KJobWidgets>
#include <KLocalizedString>
#include <KPropertiesDialog>
#include <KSharedConfig>

#include <QAction>
#include <QApplication>
#include <QClipboard>
#include <QDesktopServices>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QFileSystemModel>
#include <QFont>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QIcon>
#include <QInputDialog>
#include <QItemSelectionModel>
#include <QJsonArray>
#include <QJsonObject>
#include <QKeySequence>
#include <QLabel>
#include <QLineEdit>
#include <QMenu>
#include <QMessageBox>
#include <QMimeData>
#include <QPointer>
#include <QProcess>
#include <QShortcut>
#include <QStackedWidget>
#include <QStandardPaths>
#include <QStringList>
#include <QTextStream>
#include <QTimer>
#include <QToolBar>
#include <QToolButton>
#include <QTreeView>
#include <QUrl>
#include <QVBoxLayout>

namespace {
// KDE cut-vs-copy marker on the clipboard.
constexpr const char *kKdeCutSelection = "application/x-kde-cutselection";

bool clipboardIsCut(const QMimeData *mime)
{
    if (!mime) {
        return false;
    }
    const QByteArray flag = mime->data(QLatin1String(kKdeCutSelection));
    return !flag.isEmpty() && flag.at(0) == '1';
}

QList<QUrl> pathsToUrls(const QStringList &paths)
{
    QList<QUrl> urls;
    urls.reserve(paths.size());
    for (const QString &p : paths) {
        urls.append(QUrl::fromLocalFile(p));
    }
    return urls;
}

// Fold a gitstatus/status.go status string into the delegate's ordered enum.
// Order matters: directories roll up to the max() of their children's codes.
int statusCode(const QString &s)
{
    if (s == QLatin1String("conflicted")) {
        return GitStatusDelegate::Conflicted;
    }
    if (s == QLatin1String("deleted")) {
        return GitStatusDelegate::Deleted;
    }
    if (s == QLatin1String("modified")) {
        return GitStatusDelegate::Modified;
    }
    if (s == QLatin1String("renamed")) {
        return GitStatusDelegate::Renamed;
    }
    if (s == QLatin1String("added")) {
        return GitStatusDelegate::Added;
    }
    if (s == QLatin1String("untracked")) {
        return GitStatusDelegate::Untracked;
    }
    return GitStatusDelegate::Clean;
}
} // namespace

ProjectTree::ProjectTree(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
    , m_tree(new QTreeView(this))
    , m_model(new QFileSystemModel(this))
{
    m_model->setReadOnly(false); // allow in-place rename via setData
    m_model->setFilter(QDir::AllEntries | QDir::NoDotAndDotDot); // hidden filtered in by default off

    // One shared proxy carries the name filter; the git delegate decorates on
    // top of it. We never stack two proxies on the file system model.
    m_proxy = new FileFilterProxyModel(m_model, this);

    m_tree->setModel(m_proxy);
    m_tree->setHeaderHidden(true);
    m_tree->setUniformRowHeights(true);
    m_tree->setEditTriggers(QAbstractItemView::EditKeyPressed); // F2 to rename
    m_tree->setSelectionMode(QAbstractItemView::ExtendedSelection);
    m_tree->setContextMenuPolicy(Qt::CustomContextMenu);
    m_tree->setDragEnabled(true);
    m_tree->setDragDropMode(QAbstractItemView::DragOnly);

    m_gitDelegate = new GitStatusDelegate(m_model, this);
    m_tree->setItemDelegateForColumn(0, m_gitDelegate);

    for (int col = 1; col < m_model->columnCount(); ++col) {
        m_tree->hideColumn(col);
    }

    // Header: project heading + small toolbar (filter, sync, hidden toggle,
    // new file, new folder, open terminal, open in Dolphin).
    auto *header = new QWidget(this);
    auto *headerLayout = new QHBoxLayout(header);
    headerLayout->setContentsMargins(4, 2, 2, 2);
    headerLayout->setSpacing(2);

    m_pathLabel = new QLabel(header);
    m_pathLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_pathLabel->setToolTip(i18n("Workspace root — right-click any item for actions"));
    m_pathLabel->setForegroundRole(QPalette::PlaceholderText);
    QFont headingFont = m_pathLabel->font();
    headingFont.setBold(true);
    m_pathLabel->setFont(headingFont);
    m_pathLabel->setSizePolicy(QSizePolicy::Expanding, QSizePolicy::Preferred);
    m_pathLabel->setTextFormat(Qt::PlainText);
    headerLayout->addWidget(m_pathLabel, 1);

    auto makeBtn = [&](const QString &icon, const QString &tip) {
        auto *b = new QToolButton(header);
        b->setIcon(QIcon::fromTheme(icon));
        b->setAutoRaise(true);
        b->setToolTip(tip);
        headerLayout->addWidget(b);
        return b;
    };

    m_syncToggle = makeBtn(QStringLiteral("go-jump"), i18n("Sync with editor"));
    m_syncToggle->setCheckable(true);
    auto *newFileBtn = makeBtn(QStringLiteral("document-new"), i18n("New File"));
    auto *newFolderBtn = makeBtn(QStringLiteral("folder-new"), i18n("New Folder"));
    m_hiddenToggle = makeBtn(QStringLiteral("view-hidden"), i18n("Show Hidden Files"));
    m_hiddenToggle->setCheckable(true);
    auto *terminalBtn = makeBtn(QStringLiteral("utilities-terminal"), i18n("Open Terminal Here"));
    auto *dolphinBtn = makeBtn(QStringLiteral("system-file-manager"), i18n("Open in Dolphin"));

    // Quick-find filter box below the heading row.
    m_filterEdit = new QLineEdit(this);
    m_filterEdit->setClearButtonEnabled(true);
    m_filterEdit->setPlaceholderText(i18n("Filter files…"));
    m_filterEdit->addAction(QIcon::fromTheme(QStringLiteral("view-filter")),
                            QLineEdit::LeadingPosition);

    // Tree page lives in a stack so an empty root shows a friendly placeholder.
    m_stack = new QStackedWidget(this);

    auto *placeholder = new QWidget(m_stack);
    auto *phLayout = new QVBoxLayout(placeholder);
    phLayout->setContentsMargins(24, 24, 24, 24);
    phLayout->addStretch();
    auto *phIcon = new QLabel(placeholder);
    phIcon->setPixmap(QIcon::fromTheme(QStringLiteral("folder-open"))
                          .pixmap(48, 48));
    phIcon->setAlignment(Qt::AlignCenter);
    phLayout->addWidget(phIcon);
    auto *phText = new QLabel(
        i18n("Select an agent to browse its workspace"), placeholder);
    phText->setAlignment(Qt::AlignCenter);
    phText->setWordWrap(true);
    phText->setForegroundRole(QPalette::PlaceholderText);
    phLayout->addWidget(phText);
    phLayout->addStretch();

    m_stack->addWidget(placeholder); // page 0
    m_stack->addWidget(m_tree);      // page 1
    m_stack->setCurrentIndex(0);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(header);
    layout->addWidget(m_filterEdit);
    layout->addWidget(m_stack, 1);

    // Debounced filter application.
    m_filterTimer = new QTimer(this);
    m_filterTimer->setSingleShot(true);
    m_filterTimer->setInterval(150);
    connect(m_filterTimer, &QTimer::timeout, this, [this] {
        m_proxy->setFilterText(m_filterEdit->text());
        applyFilterEffects();
    });
    connect(m_filterEdit, &QLineEdit::textChanged, this,
            [this] { m_filterTimer->start(); });

    // Debounced git status refresh, driven by git.invalidated.
    m_gitTimer = new QTimer(this);
    m_gitTimer->setSingleShot(true);
    m_gitTimer->setInterval(150);
    connect(m_gitTimer, &QTimer::timeout, this, &ProjectTree::refreshGitStatus);

    // The git snapshot is the canonical state: refreshGitStatus()'s reply
    // set()s it, and the subscriber runs ONLY when the map truly changed. An
    // identical snapshot is dropped, so repeated git.invalidated bursts no
    // longer repaint every visible row — killing the emblem flicker.
    m_gitStatuses.subscribe(this, [this](const QHash<QString, int> &statuses) {
        applyGitStatuses(statuses);
    });

    // Restore the persisted "sync with editor" preference.
    const KConfigGroup grp = KSharedConfig::openConfig()->group(QLatin1String("Files"));
    m_syncToggle->setChecked(grp.readEntry("syncWithEditor", false));
    m_syncWithEditor = m_syncToggle->isChecked();

    connect(m_syncToggle, &QToolButton::toggled, this, &ProjectTree::setSyncWithEditor);
    connect(newFileBtn, &QToolButton::clicked, this,
            [this] { actNewFile(currentTargetDir()); });
    connect(newFolderBtn, &QToolButton::clicked, this,
            [this] { actNewFolder(currentTargetDir()); });
    connect(m_hiddenToggle, &QToolButton::toggled, this, &ProjectTree::setShowHidden);
    connect(terminalBtn, &QToolButton::clicked, this,
            [this] { emit terminalRequested(currentTargetDir()); });
    connect(dolphinBtn, &QToolButton::clicked, this, [this] {
        const QString target =
            selectedPaths().isEmpty() ? m_root : selectedPaths().first();
        actOpenContaining(target);
    });

    // When a folder finishes loading, nudge the viewport so newly-revealed rows
    // pick up their git decoration. While filtering, re-run auto-expand through
    // the debounce timer rather than calling expandAll() on every signal — each
    // expand fetches more dirs, which would otherwise cascade into repeated
    // full-tree expansions on large repos.
    connect(m_model, &QFileSystemModel::directoryLoaded, this, [this](const QString &) {
        if (m_proxy->isFiltering()) {
            m_filterTimer->start();
        }
        m_tree->viewport()->update();
    });

    if (m_core) {
        connect(m_core, &CoreClient::notification, this,
                [this](const QString &method, const QJsonObject &) {
                    if (method == QLatin1String("git.invalidated")) {
                        scheduleGitRefresh();
                    }
                });
        // If the tree was rooted before the core finished connecting, pull the
        // first snapshot as soon as the connection comes up.
        connect(m_core, &CoreClient::connected, this,
                &ProjectTree::scheduleGitRefresh);
    }

    connect(m_tree, &QTreeView::activated, this, &ProjectTree::onActivated);
    connect(m_tree, &QTreeView::customContextMenuRequested, this,
            &ProjectTree::onContextMenu);
    connect(m_tree, &QTreeView::doubleClicked, this, &ProjectTree::onActivated);

    // Delete key → move to trash.
    auto *delShortcut = new QShortcut(QKeySequence(Qt::Key_Delete), m_tree);
    delShortcut->setContext(Qt::WidgetWithChildrenShortcut);
    connect(delShortcut, &QShortcut::activated, this,
            [this] { actTrash(selectedPaths()); });

    // Ctrl+C / Ctrl+X / Ctrl+V on the tree.
    auto *copyShortcut = new QShortcut(QKeySequence::Copy, m_tree);
    copyShortcut->setContext(Qt::WidgetWithChildrenShortcut);
    connect(copyShortcut, &QShortcut::activated, this,
            [this] { actCopy(selectedPaths(), false); });
    auto *cutShortcut = new QShortcut(QKeySequence::Cut, m_tree);
    cutShortcut->setContext(Qt::WidgetWithChildrenShortcut);
    connect(cutShortcut, &QShortcut::activated, this,
            [this] { actCopy(selectedPaths(), true); });
    auto *pasteShortcut = new QShortcut(QKeySequence::Paste, m_tree);
    pasteShortcut->setContext(Qt::WidgetWithChildrenShortcut);
    connect(pasteShortcut, &QShortcut::activated, this,
            [this] { actPaste(currentTargetDir()); });
}

void ProjectTree::setRoot(const QString &path)
{
    m_root = path;
    if (path.isEmpty()) {
        m_pathLabel->clear();
        m_pathLabel->setToolTip(QString());
        m_stack->setCurrentIndex(0); // placeholder
        m_gitStatuses.set({}); // clears the delegate via the subscriber
        return;
    }
    const QModelIndex srcIdx = m_model->setRootPath(path);
    m_tree->setRootIndex(m_proxy->mapFromSource(srcIdx));
    // Project heading: the folder name in bold, full path on hover.
    const QString name = QDir(path).dirName();
    m_pathLabel->setText(name.isEmpty() ? QDir::toNativeSeparators(path) : name);
    m_pathLabel->setToolTip(QDir::toNativeSeparators(path));
    m_stack->setCurrentIndex(1); // tree
    refreshGitStatus();
}

void ProjectTree::setShowHidden(bool show)
{
    QDir::Filters f = QDir::AllEntries | QDir::NoDotAndDotDot;
    if (show) {
        f |= QDir::Hidden | QDir::System;
    }
    m_model->setFilter(f);
}

void ProjectTree::applyFilterEffects()
{
    if (m_proxy->isFiltering()) {
        m_tree->expandAll();
    } else {
        m_tree->collapseAll();
        // Keep the root's direct children visible after clearing.
        m_tree->setRootIndex(m_proxy->mapFromSource(m_model->index(m_root)));
    }
}

void ProjectTree::setSyncWithEditor(bool on)
{
    m_syncWithEditor = on;
    KConfigGroup grp = KSharedConfig::openConfig()->group(QLatin1String("Files"));
    grp.writeEntry("syncWithEditor", on);
    grp.sync();
}

QModelIndex ProjectTree::sourceIndex(const QModelIndex &viewIndex) const
{
    return m_proxy->mapToSource(viewIndex);
}

QModelIndex ProjectTree::viewIndex(const QModelIndex &srcIndex) const
{
    return m_proxy->mapFromSource(srcIndex);
}

void ProjectTree::revealPath(const QString &path)
{
    // Unconditional: callers decide whether to honour the sync-with-editor
    // toggle (auto-sync does; the explicit "Reveal in Tree" action does not).
    if (path.isEmpty() || m_root.isEmpty()) {
        return;
    }
    // Guard against paths outside the current root.
    const QString cleanRoot = QDir(m_root).absolutePath();
    const QString cleanPath = QFileInfo(path).absoluteFilePath();
    if (!cleanPath.startsWith(cleanRoot)) {
        return;
    }
    const QModelIndex src = m_model->index(path);
    if (!src.isValid()) {
        // QFileSystemModel lazy-loads; index() may be invalid until the parent
        // directories are populated. Nudge the model to fetch the chain, then
        // give up gracefully — directoryLoaded will not re-trigger reveal, but
        // the common case (file already in a loaded folder) works immediately.
        m_model->index(QFileInfo(path).absolutePath());
        return;
    }
    const QModelIndex idx = viewIndex(src);
    if (!idx.isValid()) {
        return;
    }
    // Expand ancestors top-down so each level is populated before the next.
    QList<QModelIndex> ancestors;
    for (QModelIndex p = idx.parent(); p.isValid(); p = p.parent()) {
        ancestors.prepend(p);
    }
    for (const QModelIndex &p : ancestors) {
        m_tree->expand(p);
    }
    m_tree->setCurrentIndex(idx);
    m_tree->scrollTo(idx, QAbstractItemView::PositionAtCenter);
}

void ProjectTree::scheduleGitRefresh()
{
    m_gitTimer->start(); // debounce coalesces bursts of git.invalidated
}

void ProjectTree::refreshGitStatus()
{
    if (!m_core || !m_core->isConnected() || m_root.isEmpty()) {
        return;
    }
    const QString activeRoot = QDir(m_root).absolutePath();
    // CoreClient owns the pending callback and outlives this widget (it is
    // reparented to the tail of MainWindow's child list to survive shutdown),
    // so guard against a reply landing after the tree is destroyed.
    QPointer<ProjectTree> guard(this);
    m_core->call(
        QStringLiteral("git.snapshot"), {},
        [this, guard, activeRoot](const QJsonObject &result, const QJsonObject &error) {
            if (!guard) {
                return;
            }
            if (!error.isEmpty()) {
                return;
            }
            // The tree may have re-rooted while the call was in flight.
            if (QDir(m_root).absolutePath() != activeRoot) {
                return;
            }
            const QJsonArray threads =
                result.value(QStringLiteral("threads")).toArray();
            QHash<QString, int> map;
            for (const QJsonValue &v : threads) {
                const QJsonObject o = v.toObject();
                // The tree is rooted at the worktree's working directory, which
                // is the snapshot's `path`. Match that exactly so switching
                // agents decorates only the active worktree (scoping just like
                // WorktreeDashboard, but keyed on the dir we actually display).
                const QString snapPath =
                    QDir(o.value(QStringLiteral("path")).toString()).absolutePath();
                if (snapPath != activeRoot) {
                    continue;
                }
                const QJsonArray files =
                    o.value(QStringLiteral("files")).toArray();
                for (const QJsonValue &fv : files) {
                    const QJsonObject f = fv.toObject();
                    const QString rel = f.value(QStringLiteral("path")).toString();
                    const int code =
                        statusCode(f.value(QStringLiteral("status")).toString());
                    if (rel.isEmpty() || code == GitStatusDelegate::Clean) {
                        continue;
                    }
                    const QString abs =
                        QDir::cleanPath(activeRoot + QLatin1Char('/') + rel);
                    map.insert(abs, code);
                    // Roll the strongest child status up the directory chain,
                    // stopping before the root itself (the heading carries no
                    // status emblem).
                    QString dir = abs;
                    while (true) {
                        const int slash = dir.lastIndexOf(QLatin1Char('/'));
                        if (slash < 0) {
                            break;
                        }
                        dir = dir.left(slash);
                        if (dir.length() <= activeRoot.length()) {
                            break; // reached (or passed) the root
                        }
                        map.insert(dir, qMax(map.value(dir, 0), code));
                    }
                }
                break; // only one snapshot matches the active worktree
            }
            // set() diffs against the current snapshot: an identical map is a
            // no-op (no subscriber, no repaint). Only a genuinely changed map
            // fires applyGitStatuses(), which repaints just the changed rows.
            m_gitStatuses.set(std::move(map));
        });
}

void ProjectTree::applyGitStatuses(const QHash<QString, int> &statuses)
{
    // Diff against the delegate's current map BEFORE replacing it, so we know
    // exactly which absolute paths flipped status (added, removed, or changed
    // code). Only those rows need repainting — a blanket viewport()->update()
    // is what caused the flicker.
    const QHash<QString, int> &previous = m_gitDelegate->statuses();
    QStringList changedPaths;
    for (auto it = statuses.constBegin(); it != statuses.constEnd(); ++it) {
        if (previous.value(it.key(), GitStatusDelegate::Clean) != it.value()) {
            changedPaths << it.key();
        }
    }
    for (auto it = previous.constBegin(); it != previous.constEnd(); ++it) {
        if (!statuses.contains(it.key())) {
            changedPaths << it.key(); // cleared since the last snapshot
        }
    }

    if (!m_gitDelegate->setStatuses(statuses)) {
        return; // map was identical after all — nothing to repaint
    }

    // Repaint only the rows whose status changed, mapping each absolute path
    // back through the file system model (and the filter proxy) to a view index.
    // Paths not currently realised in the lazily-loaded model resolve to an
    // invalid index and are skipped; they pick up the new emblem when their
    // folder is expanded (directoryLoaded nudges the viewport).
    for (const QString &path : std::as_const(changedPaths)) {
        const QModelIndex src = m_model->index(path);
        if (!src.isValid()) {
            continue;
        }
        const QModelIndex idx = viewIndex(src);
        if (idx.isValid()) {
            m_tree->update(idx);
        }
    }
}

void ProjectTree::onActivated(const QModelIndex &idx)
{
    const QModelIndex src = sourceIndex(idx);
    if (!src.isValid()) {
        return;
    }
    if (!m_model->isDir(src)) {
        emit fileActivated(m_model->filePath(src));
    }
}

QStringList ProjectTree::selectedPaths() const
{
    QStringList out;
    const auto idxs = m_tree->selectionModel()->selectedIndexes();
    for (const QModelIndex &i : idxs) {
        if (i.column() != 0) {
            continue;
        }
        out << m_model->filePath(sourceIndex(i));
    }
    return out;
}

QString ProjectTree::currentTargetDir() const
{
    const QStringList sel = selectedPaths();
    if (!sel.isEmpty()) {
        const QFileInfo info(sel.first());
        return info.isDir() ? info.absoluteFilePath() : info.absolutePath();
    }
    return m_root;
}

void ProjectTree::onContextMenu(const QPoint &pos)
{
    const QModelIndex idx = m_tree->indexAt(pos);
    QStringList sel = selectedPaths();
    if (sel.isEmpty() && idx.isValid()) {
        sel << m_model->filePath(sourceIndex(idx));
    }
    const bool hasSel = !sel.isEmpty();
    const QString first = hasSel ? sel.first() : m_root;
    const QFileInfo firstInfo(first);
    const QString targetDir = firstInfo.isDir() ? first : firstInfo.absolutePath();

    QMenu menu(this);

    if (hasSel && !firstInfo.isDir()) {
        QAction *open = menu.addAction(QIcon::fromTheme(QStringLiteral("document-open")),
                                       i18n("Open"));
        connect(open, &QAction::triggered, this,
                [this, first] { emit fileActivated(first); });

        QAction *openWith = menu.addAction(
            QIcon::fromTheme(QStringLiteral("document-open")), i18n("Open with Default App"));
        connect(openWith, &QAction::triggered, this,
                [this, first] { actOpenWithDefault(first); });
        menu.addSeparator();
    }

    if (hasSel) {
        QAction *attach =
            menu.addAction(QIcon::fromTheme(QStringLiteral("mail-attachment")),
                           i18n("Attach to Chat as Context"));
        connect(attach, &QAction::triggered, this,
                [this, sel] { emit attachToChatRequested(sel); });
        menu.addSeparator();
    }

    QAction *newFile = menu.addAction(QIcon::fromTheme(QStringLiteral("document-new")),
                                      i18n("New File…"));
    connect(newFile, &QAction::triggered, this,
            [this, targetDir] { actNewFile(targetDir); });
    QAction *newFolder = menu.addAction(QIcon::fromTheme(QStringLiteral("folder-new")),
                                        i18n("New Folder…"));
    connect(newFolder, &QAction::triggered, this,
            [this, targetDir] { actNewFolder(targetDir); });

    menu.addSeparator();

    QAction *cut = menu.addAction(QIcon::fromTheme(QStringLiteral("edit-cut")), i18n("Cut"));
    cut->setShortcut(QKeySequence::Cut);
    cut->setEnabled(hasSel);
    connect(cut, &QAction::triggered, this, [this, sel] { actCopy(sel, true); });

    QAction *copy = menu.addAction(QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy"));
    copy->setShortcut(QKeySequence::Copy);
    copy->setEnabled(hasSel);
    connect(copy, &QAction::triggered, this, [this, sel] { actCopy(sel, false); });

    QAction *paste = menu.addAction(QIcon::fromTheme(QStringLiteral("edit-paste")),
                                    i18n("Paste"));
    paste->setShortcut(QKeySequence::Paste);
    const QMimeData *cb = QApplication::clipboard()->mimeData();
    paste->setEnabled(cb && cb->hasUrls());
    connect(paste, &QAction::triggered, this,
            [this, targetDir] { actPaste(targetDir); });

    if (hasSel && sel.size() == 1 && !firstInfo.isDir()) {
        QAction *dup = menu.addAction(QIcon::fromTheme(QStringLiteral("edit-copy")),
                                      i18n("Duplicate"));
        connect(dup, &QAction::triggered, this, [this, first] { actDuplicate(first); });
    }

    if (hasSel && sel.size() == 1) {
        QAction *rename =
            menu.addAction(QIcon::fromTheme(QStringLiteral("edit-rename")), i18n("Rename…"));
        rename->setShortcut(QKeySequence(Qt::Key_F2));
        connect(rename, &QAction::triggered, this, [this, first] { actRename(first); });
    }

    if (hasSel) {
        QAction *trash =
            menu.addAction(QIcon::fromTheme(QStringLiteral("user-trash")),
                           i18n("Move to Trash"));
        trash->setShortcut(QKeySequence(Qt::Key_Delete));
        connect(trash, &QAction::triggered, this, [this, sel] { actTrash(sel); });
    }

    menu.addSeparator();

    if (hasSel) {
        QAction *copyAbs = menu.addAction(i18n("Copy Full Path"));
        connect(copyAbs, &QAction::triggered, this,
                [this, first] { actCopyPath(first, false); });
        QAction *copyRel = menu.addAction(i18n("Copy Relative Path"));
        connect(copyRel, &QAction::triggered, this,
                [this, first] { actCopyPath(first, true); });
    }

    menu.addSeparator();

    QAction *termHere = menu.addAction(
        QIcon::fromTheme(QStringLiteral("utilities-terminal")), i18n("Open Terminal Here"));
    connect(termHere, &QAction::triggered, this,
            [this, targetDir] { emit terminalRequested(targetDir); });

    QAction *runHere = menu.addAction(
        QIcon::fromTheme(QStringLiteral("system-run")), i18n("Run Command Here…"));
    connect(runHere, &QAction::triggered, this, [this, targetDir] {
        bool ok = false;
        const QString command = QInputDialog::getText(
            this, i18n("Run Command"),
            i18n("Command to run in %1:", QDir(targetDir).dirName()),
            QLineEdit::Normal, QString(), &ok);
        if (ok && !command.isEmpty()) {
            emit runCommandRequested(targetDir, command);
        }
    });

    QAction *inDolphin = menu.addAction(
        QIcon::fromTheme(QStringLiteral("system-file-manager")), i18n("Open in Dolphin"));
    connect(inDolphin, &QAction::triggered, this,
            [this, first] { actOpenContaining(first); });

    if (hasSel) {
        QAction *gitignore =
            menu.addAction(QIcon::fromTheme(QStringLiteral("vcs-removed")),
                           i18n("Add to .gitignore"));
        connect(gitignore, &QAction::triggered, this,
                [this, sel] { actAddToGitignore(sel); });
    }

    menu.addSeparator();

    QAction *refresh =
        menu.addAction(QIcon::fromTheme(QStringLiteral("view-refresh")), i18n("Refresh"));
    // Re-pull the git snapshot only. Tearing down and re-rooting the
    // QFileSystemModel here would collapse the tree and drop the user's
    // expansion/selection; refreshGitStatus() refreshes the decoration in place.
    connect(refresh, &QAction::triggered, this, &ProjectTree::refreshGitStatus);

    QAction *hidden = menu.addAction(i18n("Show Hidden Files"));
    hidden->setCheckable(true);
    hidden->setChecked(m_hiddenToggle->isChecked());
    connect(hidden, &QAction::toggled, m_hiddenToggle, &QToolButton::setChecked);

    if (hasSel) {
        menu.addSeparator();
        QAction *props =
            menu.addAction(QIcon::fromTheme(QStringLiteral("document-properties")),
                           i18n("Properties"));
        connect(props, &QAction::triggered, this, [this, sel] { actProperties(sel); });
    }

    menu.exec(m_tree->viewport()->mapToGlobal(pos));
}

void ProjectTree::actNewFile(const QString &targetDir)
{
    bool ok = false;
    const QString name = QInputDialog::getText(
        this, i18n("New File"), i18n("File name:"), QLineEdit::Normal, QString(), &ok);
    if (!ok || name.trimmed().isEmpty()) {
        return;
    }
    const QString full = QDir(targetDir).absoluteFilePath(name.trimmed());
    if (QFileInfo::exists(full)) {
        QMessageBox::warning(this, i18n("New File"),
                             i18n("A file named \"%1\" already exists.", name));
        return;
    }
    QFile f(full);
    if (!f.open(QIODevice::WriteOnly)) {
        QMessageBox::warning(this, i18n("New File"),
                             i18n("Could not create %1: %2", full, f.errorString()));
        return;
    }
    f.close();
    emit fileActivated(full);
}

void ProjectTree::actNewFolder(const QString &targetDir)
{
    bool ok = false;
    const QString name = QInputDialog::getText(
        this, i18n("New Folder"), i18n("Folder name:"), QLineEdit::Normal, QString(), &ok);
    if (!ok || name.trimmed().isEmpty()) {
        return;
    }
    QDir d(targetDir);
    if (!d.mkpath(name.trimmed())) {
        QMessageBox::warning(this, i18n("New Folder"),
                             i18n("Could not create folder \"%1\".", name));
    }
}

void ProjectTree::actRename(const QString &path)
{
    const QModelIndex src = m_model->index(path);
    if (!src.isValid()) {
        return;
    }
    const QModelIndex idx = viewIndex(src);
    if (!idx.isValid()) {
        return;
    }
    m_tree->setCurrentIndex(idx);
    m_tree->edit(idx);
}

void ProjectTree::actTrash(const QStringList &paths)
{
    if (paths.isEmpty()) {
        return;
    }
    const auto reply = QMessageBox::question(
        this, i18n("Move to Trash"),
        i18np("Move %1 item to the trash?", "Move %1 items to the trash?", paths.size()),
        QMessageBox::Yes | QMessageBox::No, QMessageBox::Yes);
    if (reply != QMessageBox::Yes) {
        return;
    }
    auto *job = new KIO::DeleteOrTrashJob(pathsToUrls(paths), KIO::AskUserActionInterface::Trash,
                                          KIO::AskUserActionInterface::DefaultConfirmation, this);
    KJobWidgets::setWindow(job, window());
    job->start();
}

void ProjectTree::actCopy(const QStringList &paths, bool cut)
{
    if (paths.isEmpty()) {
        return;
    }
    auto *mime = new QMimeData;
    mime->setUrls(pathsToUrls(paths));
    mime->setData(QLatin1String(kKdeCutSelection), cut ? QByteArrayLiteral("1")
                                                        : QByteArrayLiteral("0"));
    QApplication::clipboard()->setMimeData(mime);
}

void ProjectTree::actPaste(const QString &destDir)
{
    const QMimeData *cb = QApplication::clipboard()->mimeData();
    if (!cb || !cb->hasUrls()) {
        return;
    }
    const QList<QUrl> urls = cb->urls();
    const bool cut = clipboardIsCut(cb);
    const QUrl dest = QUrl::fromLocalFile(destDir);
    KIO::CopyJob *job = cut ? KIO::move(urls, dest) : KIO::copy(urls, dest);
    KJobWidgets::setWindow(job, window());
    if (cut) {
        // Clear the cut-marker so a second paste copies instead of moving.
        QApplication::clipboard()->clear();
    }
}

void ProjectTree::actDuplicate(const QString &path)
{
    const QFileInfo info(path);
    const QString base = info.completeBaseName();
    const QString suffix = info.suffix().isEmpty() ? QString()
                                                    : QStringLiteral(".") + info.suffix();
    QString candidate;
    int n = 1;
    do {
        candidate = info.absoluteDir().absoluteFilePath(
            QStringLiteral("%1 (copy%2)%3")
                .arg(base, n == 1 ? QString() : QStringLiteral(" %1").arg(n), suffix));
        ++n;
    } while (QFileInfo::exists(candidate));
    if (!QFile::copy(path, candidate)) {
        QMessageBox::warning(this, i18n("Duplicate"),
                             i18n("Could not duplicate %1.", info.fileName()));
    }
}

void ProjectTree::actCopyPath(const QString &path, bool relative)
{
    QString out = path;
    if (relative && !m_root.isEmpty()) {
        out = QDir(m_root).relativeFilePath(path);
    }
    QApplication::clipboard()->setText(out);
}

void ProjectTree::actOpenContaining(const QString &path)
{
    // KIO highlightInFileManager opens the default file manager (Dolphin on
    // Plasma) with the entry selected.
    KIO::highlightInFileManager({QUrl::fromLocalFile(path)});
}

void ProjectTree::actOpenWithDefault(const QString &path)
{
    QDesktopServices::openUrl(QUrl::fromLocalFile(path));
}

void ProjectTree::actProperties(const QStringList &paths)
{
    auto *dlg = new KPropertiesDialog(pathsToUrls(paths), this);
    dlg->setAttribute(Qt::WA_DeleteOnClose);
    dlg->show();
}

QString ProjectTree::repoRootFor(const QString &path) const
{
    QDir d(QFileInfo(path).absolutePath());
    while (true) {
        if (d.exists(QStringLiteral(".git"))) {
            return d.absolutePath();
        }
        if (!d.cdUp()) {
            break;
        }
    }
    return m_root;
}

void ProjectTree::actAddToGitignore(const QStringList &paths)
{
    if (paths.isEmpty()) {
        return;
    }
    const QString repo = repoRootFor(paths.first());
    const QString gi = QDir(repo).filePath(QStringLiteral(".gitignore"));

    QStringList existing;
    {
        QFile f(gi);
        if (f.open(QIODevice::ReadOnly | QIODevice::Text)) {
            QTextStream in(&f);
            while (!in.atEnd()) {
                existing << in.readLine();
            }
        }
    }

    QStringList toAdd;
    for (const QString &p : paths) {
        QString rel = QDir(repo).relativeFilePath(p);
        if (QFileInfo(p).isDir() && !rel.endsWith(QLatin1Char('/'))) {
            rel += QLatin1Char('/');
        }
        const QString entry = QStringLiteral("/") + rel; // anchor at repo root
        if (!existing.contains(entry) && !existing.contains(rel)) {
            toAdd << entry;
        }
    }
    if (toAdd.isEmpty()) {
        return;
    }
    QFile f(gi);
    if (!f.open(QIODevice::WriteOnly | QIODevice::Append | QIODevice::Text)) {
        QMessageBox::warning(this, i18n("Add to .gitignore"),
                             i18n("Could not write %1: %2", gi, f.errorString()));
        return;
    }
    QTextStream out(&f);
    if (f.size() > 0 && !existing.isEmpty() && !existing.last().isEmpty()) {
        out << '\n';
    }
    for (const QString &line : toAdd) {
        out << line << '\n';
    }
}
