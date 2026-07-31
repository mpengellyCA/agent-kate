#pragma once

#include <QHash>
#include <QPointer>
#include <QSet>
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
class QTimer;

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
    // Re-key an existing group, keeping its tabs. Used when a fresh agent's core
    // thread id arrives and its per-run "pending" key is replaced by the stable
    // one (see EditorSession.h). No-op if `from` has no group or `to` already
    // has one; returns whether the rename happened.
    bool renameGroup(const QString &from, const QString &to);
    void openFile(const QString &groupKey, const QString &path, int line = -1,
                  int column = 0);
    void openDiff(const QString &groupKey, const QString &title, const QString &text);
    bool saveCurrent();
    // Save a specific document (the format-on-save path captures the document at
    // save-request time, so a tab switch during the async format round-trip can't
    // redirect the write to the wrong file). Returns false if the document is no
    // longer open or the write failed. Callers own the status feedback.
    bool saveDocument(KTextEditor::Document *doc);
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

    // Autosave: debounced write of a modified document ~1s after the last edit,
    // plus save on focus-out / app deactivation. Off by default until the caller
    // reflects the persisted preference. Autosave never runs the LSP formatter,
    // so the cursor never jumps — manual Ctrl+S still formats.
    void setAutosaveEnabled(bool on);
    bool isAutosaveEnabled() const { return m_autosaveEnabled; }
    // Flush every modified local document now, without formatting. Called on app
    // deactivation so edits reach disk even before the per-doc debounce fires.
    void autosaveAll();

Q_SIGNALS:
    void openFilesChanged();
    void statusMessage(const QString &text);
    void currentFileChanged(const QString &path);
    void documentOpened(KTextEditor::Document *doc, const QString &path);
    void documentClosed(KTextEditor::Document *doc);
    // A document was written to disk by autosave (no LSP formatting was run).
    // MainWindow relays this to the LSP so diagnostics refresh on the saved file.
    void documentAutosaved(KTextEditor::Document *doc);
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

    // Autosave helpers. A document qualifies for autosave when it has a local
    // URL, is modified, isn't read-only, and isn't showing the modified-on-disk
    // reload banner (never clobber the on-disk version the human is deciding on).
    void wireAutosave(KTextEditor::Document *doc, QWidget *bannerHost);
    bool autosaveCandidate(KTextEditor::Document *doc) const;
    void autosaveDocument(KTextEditor::Document *doc);
    // The container widget hosting a document's view (for reload-banner lookup).
    QWidget *bannerHostForDocument(KTextEditor::Document *doc) const;

    QStackedWidget *m_stack = nullptr;
    QWidget *m_placeholder = nullptr;
    KTextEditor::Editor *m_editor = nullptr;
    QHash<QString, QTabWidget *> m_groups;
    QString m_activeGroup;

    bool m_autosaveEnabled = false;
    // One shared single-shot timer coalesces the debounce; whichever document
    // last edited is remembered and written when it fires.
    QTimer *m_autosaveTimer = nullptr;
    QPointer<KTextEditor::Document> m_autosavePending;
    // Documents whose autosave is suspended after a failed write (deleted /
    // read-only file). Without this an autosave that fails would re-fire on every
    // keystroke, and KTextEditor would pop a modal error dialog each time — a
    // once-per-second dialog storm while typing. Cleared by a successful manual
    // save. Keyed by document URL so it survives the QPointer being cleared on
    // close and re-open of the same path.
    QSet<QString> m_autosaveSuspended;
};
