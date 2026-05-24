#pragma once

#include <QList>
#include <QString>
#include <QStringList>
#include <QWidget>

class CoreClient;
class AgentPanel;
class AgentRoster;
class QStackedWidget;

// AgentDock is the agent-centric workspace. It is the window's central widget —
// a stack of agent conversations — and it owns the AgentRoster (the project →
// agent tree placed in the left dock), coordinating many projects at once.
class AgentDock : public QWidget
{
    Q_OBJECT
public:
    explicit AgentDock(CoreClient *core, QWidget *parent = nullptr);

    QWidget *roster() const; // the project/agent tree, for the left dock
    void addProject(const QString &path);
    void openProjectDialog();

    // Bring an attached Claude Code session in as a dormant agent and resume it.
    void attachSession(const QString &project, const QString &threadId,
                       const QString &title);

    // Re-apply chat preferences (send key, tool-card visibility) to every panel.
    void applyChatSettings();

    // threadId of the currently-active agent panel, empty if none / not started.
    QString currentThreadId() const;

Q_SIGNALS:
    void statusMessage(const QString &text);
    void openDiff(const QString &title, const QString &diffText);
    void agentActivated(int agentId, const QString &projectPath);
    void projectFocused(const QString &projectPath);

private:
    struct Entry {
        int id;
        QString project;
        AgentPanel *panel;
    };

    void ensureProject(const QString &path);
    AgentPanel *addAgent(const QString &projectPath);
    AgentPanel *addDormantAgent(const QString &project, const QString &threadId,
                                const QString &title, bool isolated);
    void wireAgentPanel(int agentId, AgentPanel *panel);
    void restoreThreads(const QString &project);
    // Pull git.snapshot and push each thread's worktree Number into the
    // roster, so the #N badge stays in sync with WorktreeDashboard.
    void refreshAgentNumbers();
    bool hasThread(const QString &threadId) const;
    void removeAgentEntry(int agentId);
    void closeAgent(int agentId);
    void closeProject(const QString &path);
    Entry *entryById(int agentId);

    CoreClient *m_core = nullptr;
    QStackedWidget *m_stack = nullptr;
    AgentRoster *m_roster = nullptr;
    QList<Entry> m_agents;
    QStringList m_projects;
    int m_counter = 0;
};
