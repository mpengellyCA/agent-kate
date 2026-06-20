#pragma once

#include <QHash>
#include <QString>
#include <QStringList>
#include <QWidget>

namespace KTextEditor {
class Document;
class Editor;
class View;
}
class QStackedWidget;
class QTabWidget;

// EditorArea hosts editor tabs grouped by a caller-chosen key (a project path
// or an agent id). Each group has its own QTabWidget of KTextEditor views and
// diff views; setActiveGroup swaps the visible group. With no group active it
// shows a placeholder.
class EditorArea : public QWidget
{
    Q_OBJECT
public:
    explicit EditorArea(QWidget *parent = nullptr);
    ~EditorArea() override;

    void setActiveGroup(const QString &groupKey);
    void openFile(const QString &groupKey, const QString &path, int line = -1,
                  int column = 0);
    void openDiff(const QString &groupKey, const QString &title, const QString &text);
    bool saveCurrent();
    // Save every modified document across all groups. Returns true if all
    // succeeded (or there was nothing to save).
    bool saveAll();
    QStringList openFilePaths() const;
    // Every group key that currently has a tab widget (for session persistence
    // across all open projects/agents, not just the active one).
    QStringList groupKeys() const;
    // Open files for a single group key (for session persistence), and the
    // currently-active file path within that group.
    QStringList openFilePathsForGroup(const QString &key) const;
    QString currentPathForGroup(const QString &key) const;
    // Interactive close-all check used by MainWindow::closeEvent. Prompts for
    // each modified document (Save/Discard/Cancel). Returns false if the user
    // cancelled — the caller must then abort the window close.
    bool confirmCloseAll();
    KTextEditor::View *currentView() const;

Q_SIGNALS:
    void openFilesChanged();
    void statusMessage(const QString &text);
    void currentFileChanged(const QString &path);
    void documentOpened(KTextEditor::Document *doc, const QString &path);
    void documentClosed(KTextEditor::Document *doc);
    // Request the project tree to reveal/select this path (tab context menu).
    void revealInTreeRequested(const QString &path);

private:
    QTabWidget *groupTabs(const QString &key, bool create);
    QTabWidget *activeTabs() const;
    // Resolve the KTextEditor::View backing a tab widget, whether the tab is a
    // bare View or the thin container that wraps a View beneath its reload
    // banner (plain text) or a RichTextView. The single source of truth every
    // qobject_cast<View*> site routes through (see the wrapping note in .cpp).
    KTextEditor::View *viewForTab(QWidget *tabWidget) const;
    // The on-disk file path a tab represents, regardless of which viewer backs
    // it. Empty if the tab has no associated local file (e.g. a diff).
    QString pathForTab(QWidget *tabWidget) const;
    // Close a tab, optionally prompting to save when its document is modified.
    // interactive=true is the only path that may prompt (never the destructor).
    // Returns false only when an interactive prompt was cancelled.
    bool closeTabIn(QTabWidget *tabs, int index, bool interactive = false);
    // Wire a freshly-created document's modifiedChanged → dirty tab icon, and
    // its modifiedOnDisk → the reload banner. bannerHost is the widget whose
    // top-of-layout the banner is inserted into.
    void wireDocument(KTextEditor::Document *doc, QTabWidget *tabs, QWidget *bannerHost);
    void emitCurrentFile();
    void updateVisible();
    void updateTabIcon(KTextEditor::Document *doc);

    QStackedWidget *m_stack = nullptr;
    QWidget *m_placeholder = nullptr;
    KTextEditor::Editor *m_editor = nullptr;
    QHash<QString, QTabWidget *> m_groups;
    QString m_activeGroup;
};
