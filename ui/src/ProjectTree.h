#pragma once

#include <QHash>
#include <QJsonObject>
#include <QString>
#include <QStringList>
#include <QWidget>

class CoreClient;
class FileFilterProxyModel;
class GitStatusDelegate;
class QAction;
class QFileSystemModel;
class QLabel;
class QLineEdit;
class QModelIndex;
class QPoint;
class QStackedWidget;
class QTimer;
class QToolButton;
class QTreeView;

// ProjectTree is the workspace file browser. It shows the agent's worktree,
// supports right-click file operations (new, rename, copy/cut/paste, trash,
// add-to-.gitignore, open-with, open-in-Dolphin, KDE properties, open
// terminal here), drag-and-drop of files into the chat input, and a few
// header conveniences (project heading, name filter, hidden-files toggle,
// sync-with-editor, new file/folder). Entries are decorated with their git
// status (emblem + tint) sourced from the core's git.snapshot.
class ProjectTree : public QWidget
{
    Q_OBJECT
public:
    explicit ProjectTree(CoreClient *core, QWidget *parent = nullptr);

    void setRoot(const QString &path);
    QString root() const { return m_root; }

    // Select, scroll to and expand to a path if it lives under the current
    // root. No-op for paths outside the tree.
    void revealPath(const QString &path);

Q_SIGNALS:
    void fileActivated(const QString &path);
    // Request the integrated terminal to open a new tab at this directory.
    void terminalRequested(const QString &dir);
    // Request the integrated terminal to open a tab at `dir` and run `command`.
    void runCommandRequested(const QString &dir, const QString &command);
    // Request the active chat input to attach these paths as context.
    void attachToChatRequested(const QStringList &paths);

private:
    void onContextMenu(const QPoint &pos);
    void onActivated(const QModelIndex &idx);
    void setShowHidden(bool show);
    void applyFilterEffects();
    void setSyncWithEditor(bool on);

    // Git status decoration.
    void refreshGitStatus();
    void scheduleGitRefresh();

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
    // Map a source QFileSystemModel index from a (possibly proxied) view index.
    QModelIndex sourceIndex(const QModelIndex &viewIndex) const;
    // Map a source index to the view (proxy) index for selection/scroll.
    QModelIndex viewIndex(const QModelIndex &srcIndex) const;

    CoreClient *m_core = nullptr;
    QTreeView *m_tree = nullptr;
    QFileSystemModel *m_model = nullptr;
    FileFilterProxyModel *m_proxy = nullptr;
    GitStatusDelegate *m_gitDelegate = nullptr;
    QStackedWidget *m_stack = nullptr;
    QLabel *m_pathLabel = nullptr;
    QLineEdit *m_filterEdit = nullptr;
    QToolButton *m_hiddenToggle = nullptr;
    QToolButton *m_syncToggle = nullptr;
    QTimer *m_filterTimer = nullptr;
    QTimer *m_gitTimer = nullptr;
    QString m_root;
    bool m_syncWithEditor = false;
};
