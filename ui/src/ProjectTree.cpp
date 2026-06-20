#include "ProjectTree.h"

#include <KIO/CopyJob>
#include <KIO/DeleteOrTrashJob>
#include <KIO/OpenFileManagerWindowJob>
#include <KJobWidgets>
#include <KLocalizedString>
#include <KPropertiesDialog>

#include <QAction>
#include <QApplication>
#include <QClipboard>
#include <QDesktopServices>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QFileSystemModel>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QIcon>
#include <QInputDialog>
#include <QItemSelectionModel>
#include <QKeySequence>
#include <QLabel>
#include <QMenu>
#include <QMessageBox>
#include <QMimeData>
#include <QProcess>
#include <QShortcut>
#include <QStandardPaths>
#include <QStringList>
#include <QTextStream>
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
} // namespace

ProjectTree::ProjectTree(QWidget *parent)
    : QWidget(parent)
    , m_tree(new QTreeView(this))
    , m_model(new QFileSystemModel(this))
{
    m_model->setReadOnly(false); // allow in-place rename via setData
    m_model->setFilter(QDir::AllEntries | QDir::NoDotAndDotDot); // hidden filtered in by default off

    m_tree->setModel(m_model);
    m_tree->setHeaderHidden(true);
    m_tree->setUniformRowHeights(true);
    m_tree->setEditTriggers(QAbstractItemView::EditKeyPressed); // F2 to rename
    m_tree->setSelectionMode(QAbstractItemView::ExtendedSelection);
    m_tree->setContextMenuPolicy(Qt::CustomContextMenu);
    m_tree->setDragEnabled(true);
    m_tree->setDragDropMode(QAbstractItemView::DragOnly);

    for (int col = 1; col < m_model->columnCount(); ++col) {
        m_tree->hideColumn(col);
    }

    // Header: path label + small toolbar (hidden toggle, new file, new folder,
    // open terminal, open in Dolphin).
    auto *header = new QWidget(this);
    auto *headerLayout = new QHBoxLayout(header);
    headerLayout->setContentsMargins(4, 2, 2, 2);
    headerLayout->setSpacing(2);

    m_pathLabel = new QLabel(header);
    m_pathLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_pathLabel->setToolTip(i18n("Workspace root — right-click any item for actions"));
    m_pathLabel->setStyleSheet(QStringLiteral("color: palette(mid);"));
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

    auto *newFileBtn = makeBtn(QStringLiteral("document-new"), i18n("New File"));
    auto *newFolderBtn = makeBtn(QStringLiteral("folder-new"), i18n("New Folder"));
    m_hiddenToggle = makeBtn(QStringLiteral("view-hidden"), i18n("Show Hidden Files"));
    m_hiddenToggle->setCheckable(true);
    auto *terminalBtn = makeBtn(QStringLiteral("utilities-terminal"), i18n("Open Terminal Here"));
    auto *dolphinBtn = makeBtn(QStringLiteral("system-file-manager"), i18n("Open in Dolphin"));

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(header);
    layout->addWidget(m_tree);

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
    const QModelIndex idx = m_model->setRootPath(path);
    m_tree->setRootIndex(idx);
    m_pathLabel->setText(QDir::toNativeSeparators(path));
}

void ProjectTree::setShowHidden(bool show)
{
    QDir::Filters f = QDir::AllEntries | QDir::NoDotAndDotDot;
    if (show) {
        f |= QDir::Hidden | QDir::System;
    }
    m_model->setFilter(f);
}

void ProjectTree::onActivated(const QModelIndex &idx)
{
    if (!idx.isValid()) {
        return;
    }
    if (!m_model->isDir(idx)) {
        emit fileActivated(m_model->filePath(idx));
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
        out << m_model->filePath(i);
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
        sel << m_model->filePath(idx);
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
    connect(refresh, &QAction::triggered, this, [this] {
        const QString r = m_root;
        m_model->setRootPath(QString());
        setRoot(r);
    });

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
    const QModelIndex idx = m_model->index(path);
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

void ProjectTree::onSelectionChanged()
{
    // Reserved for future status updates.
}
