#pragma once

#include <KMainWindow>
#include <QString>

class CoreClient;
class EditorArea;
class ProjectTree;
class AgentDock;
class LspManager;
class TerminalPanel;
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

    CoreClient *m_core = nullptr;
    EditorArea *m_editor = nullptr;
    ProjectTree *m_tree = nullptr;
    AgentDock *m_agent = nullptr;
    LspManager *m_lsp = nullptr;
    TerminalPanel *m_terminal = nullptr;
    QLabel *m_coreStatus = nullptr;

    QString m_activeProject;
    int m_activeAgentId = -1;
    bool m_tabsByAgent = false; // editor tab grouping: false = project, true = agent
};
