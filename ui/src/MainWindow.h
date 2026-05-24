#pragma once

#include <KMainWindow>
#include <QHash>
#include <QString>

namespace KTextEditor {
class Document;
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
class KMultiTabBar;
class QAction;
class QDockWidget;
class QLabel;

// MainWindow is the AgentKate arena shell — a project-aware, agent-centric KDE
// main window. The agent roster (left) holds projects and their agents; the
// active agent drives the file tree and the editor on the right.
class MainWindow : public KMainWindow
{
    Q_OBJECT
public:
    explicit MainWindow(const QString &openPath = QString(), QWidget *parent = nullptr);
    ~MainWindow() override;

private:
    void setupUi();
    void setupActions();
    void setupCore();
    void onSave();
    void reloadExtensionServers();
    void onAgentActivated(int agentId, const QString &projectPath);
    void setTabsByAgent(bool byAgent);
    QString groupKey() const;
    void pushOpenFilesToCore();

    void setupBottomStrip();
    void showBottomTab(int id);
    void hideBottomTab(int id);
    void syncBottomTabFromDock(int id, bool visible);

    CoreClient *m_core = nullptr;
    EditorArea *m_editor = nullptr;
    ProjectTree *m_tree = nullptr;
    AgentDock *m_agent = nullptr;
    LspManager *m_lsp = nullptr;
    TerminalPanel *m_terminal = nullptr;
    WorktreeDashboard *m_worktreeDashboard = nullptr;
    LogViewer *m_logViewer = nullptr;
    QLabel *m_gitStatusLabel = nullptr; // status-bar git widget for the active editor
    QDockWidget *m_problemsDock = nullptr;
    QDockWidget *m_referencesDock = nullptr;
    QDockWidget *m_terminalDock = nullptr;
    QAction *m_toggleBottomAct = nullptr;
    KMultiTabBar *m_bottomStrip = nullptr;
    int m_lastBottomTab = -1; // last tab raised by the user, used to restore on Ctrl+J

    QHash<KTextEditor::Document *, GutterController *> m_gutters;
    QHash<KTextEditor::Document *, BlameController *> m_blames;
    QAction *m_blameToggle = nullptr;
    QString m_activeFilePath;

    QString m_activeProject;
    int m_activeAgentId = -1;
    bool m_tabsByAgent = false; // editor tab grouping: false = project, true = agent
};
