#pragma once

#include <KMainWindow>
#include <QHash>
#include <QString>

namespace KTextEditor {
class Document;
class View;
}
class GutterController;
class BlameController;

class CoreClient;
class EditorArea;
class ProjectTree;
class AgentDock;
class LspManager;
class TerminalPanel;
class WorktreeDashboard;
class LogViewer;
class SideBar;
class ShellLayout;
class QAction;
class QLabel;

// MainWindow is the Agent Kate arena shell — a project-aware, agent-centric KDE
// main window. The agent roster (left) holds projects and their agents; the
// active agent drives the file tree and the editor on the right.
class MainWindow : public KMainWindow
{
    Q_OBJECT
public:
    explicit MainWindow(const QString &openPath = QString(), QWidget *parent = nullptr);
    ~MainWindow() override;

protected:
    void closeEvent(QCloseEvent *event) override;

private:
    void setupUi();
    void setupActions();
    void setupHamburger();
    void setupTopToolbar();
    void setupCore();
    void updateCursorStatus();
    void updateBreadcrumb(const QString &path);
    void updateAgentBadge();
    void onSave();
    void reloadExtensionServers();
    void onAgentActivated(int agentId, const QString &projectPath);
    void setTabsByAgent(bool byAgent);
    QString groupKey() const;
    void pushOpenFilesToCore();

    void persistShellState();

    CoreClient *m_core = nullptr;
    EditorArea *m_editor = nullptr;
    ProjectTree *m_tree = nullptr;
    AgentDock *m_agent = nullptr;
    LspManager *m_lsp = nullptr;
    TerminalPanel *m_terminal = nullptr;
    WorktreeDashboard *m_worktreeDashboard = nullptr;
    LogViewer *m_logViewer = nullptr;
    QLabel *m_gitStatusLabel = nullptr; // status-bar git widget for the active editor

    ShellLayout *m_shell = nullptr;
    SideBar *m_leftBar = nullptr;
    SideBar *m_rightBar = nullptr;
    SideBar *m_bottomBar = nullptr;

    int m_leftRosterId = -1;
    int m_leftFilesId = -1;
    int m_leftOutlineId = -1;
    int m_rightWorktreesId = -1;
    int m_rightGitLogId = -1;
    int m_bottomTerminalId = -1;
    int m_bottomReferencesId = -1;
    int m_bottomProblemsId = -1;

    int m_lastBottomTab = -1; // last tab raised by the user, used to restore on Ctrl+J
    QAction *m_toggleBottomAct = nullptr;

    // Top toolbar + status-bar widgets
    QLabel *m_breadcrumbLabel = nullptr;
    QLabel *m_agentBadge = nullptr;       // top-toolbar agent chip
    QLabel *m_cursorPosLabel = nullptr;   // status bar: Ln 42 Col 17
    QLabel *m_modeLabel = nullptr;        // status bar: UTF-8 LF C++
    QLabel *m_agentStatusLabel = nullptr; // status bar (rightmost): agent name + dot
    KTextEditor::View *m_observedView = nullptr; // currently wired-for-cursor

    QHash<KTextEditor::Document *, GutterController *> m_gutters;
    QHash<KTextEditor::Document *, BlameController *> m_blames;
    QAction *m_blameToggle = nullptr;
    QString m_activeFilePath;

    QString m_activeProject;
    int m_activeAgentId = -1;
    bool m_tabsByAgent = false; // editor tab grouping: false = project, true = agent
};
