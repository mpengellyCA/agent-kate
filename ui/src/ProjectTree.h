#pragma once

#include <QString>
#include <QStringList>
#include <QWidget>

class QAction;
class QFileSystemModel;
class QLabel;
class QModelIndex;
class QPoint;
class QToolButton;
class QTreeView;

// ProjectTree is the workspace file browser. It shows the agent's worktree,
// supports right-click file operations (new, rename, copy/cut/paste, trash,
// add-to-.gitignore, open-with, open-in-Dolphin, KDE properties, open
// terminal here), drag-and-drop of files into the chat input, and a few
// header conveniences (path label, hidden-files toggle, new file/folder).
class ProjectTree : public QWidget
{
    Q_OBJECT
public:
    explicit ProjectTree(QWidget *parent = nullptr);

    void setRoot(const QString &path);
    QString root() const { return m_root; }

Q_SIGNALS:
    void fileActivated(const QString &path);
    // Request the integrated terminal to open a new tab at this directory.
    void terminalRequested(const QString &dir);
    // Request the active chat input to attach these paths as context.
    void attachToChatRequested(const QStringList &paths);

private:
    void onContextMenu(const QPoint &pos);
    void onActivated(const QModelIndex &idx);
    void onSelectionChanged();
    void setShowHidden(bool show);

    // Operations
    void actNewFile(const QString &targetDir);
    void actNewFolder(const QString &targetDir);
    void actRename(const QString &path);
    void actTrash(const QStringList &paths);
    void actCopy(const QStringList &paths, bool cut);
    void actPaste(const QString &destDir);
    void actDuplicate(const QString &path);
    void actCopyPath(const QString &path, bool relative);
    void actOpenContaining(const QString &path);
    void actOpenWithDefault(const QString &path);
    void actProperties(const QStringList &paths);
    void actAddToGitignore(const QStringList &paths);

    // Helpers
    QString currentTargetDir() const; // selection if dir, else parent of selection, else root
    QStringList selectedPaths() const;
    QString repoRootFor(const QString &path) const;

    QTreeView *m_tree = nullptr;
    QFileSystemModel *m_model = nullptr;
    QLabel *m_pathLabel = nullptr;
    QToolButton *m_hiddenToggle = nullptr;
    QString m_root;
};
