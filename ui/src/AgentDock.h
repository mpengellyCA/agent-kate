#pragma once

#include <QHash>
#include <QList>
#include <QObject>
#include <QString>
#include <QStringList>

class CoreClient;
class AgentPanel;
class AgentRoster;
class QStackedWidget;
class QWidget;

// AgentDock orchestrates the agent-centric workspace: a stack of agent
// conversation panels plus the AgentRoster (project → agent tree). The shell
// hosts these widgets in distinct layout slots — the roster on the left
// sidebar and the panel stack inside the centre split — so AgentDock is a
// pure QObject that owns the widgets without being placed on screen itself.
class AgentDock : public QObject
{
    Q_OBJECT
public:
    explicit AgentDock(CoreClient *core, QWidget *parent = nullptr);

    QWidget *roster() const; // the project/agent tree, hosted by the left sidebar
    QStackedWidget *panelStack() const; // the conversation stack, hosted by the centre split
    QWidget *activePanel() const; // the panel currently on top of the stack

    void addProject(const QString &path);
    void openProjectDialog();

    // Bring an attached Claude Code session in as a dormant agent and resume it.
    void attachSession(const QString &project, const QString &threadId,
                       const QString &title);

    // Re-apply chat preferences (send key, tool-card visibility) to every panel.
    void applyChatSettings();

    // threadId of the currently-active agent panel, empty if none / not started.
    QString currentThreadId() const;

    // Worktree directory for the given agent, derived from the last git.snapshot.
    // Empty if the agent has no worktree yet (not started / not promoted).
    QString worktreePathForAgent(int agentId) const;

Q_SIGNALS:
    void statusMessage(const QString &text);
    void openDiff(const QString &title, const QString &diffText);
    void agentActivated(int agentId, const QString &projectPath);
    void projectFocused(const QString &projectPath);
    // Emitted when a project is explicitly CLOSED (not merely switched away
    // from). Consumers tied to a project — e.g. its terminal tabs — tear down.
    void projectClosed(const QString &projectPath);
    // Routed to MainWindow to focus the Terminal panel at this project path.
    void openTerminalRequested(const QString &projectPath);
    // Carries an agent's WORKTREE path (distinct from a project path) so the
    // shell can open a terminal rooted there.
    void openWorktreeTerminalRequested(const QString &worktreePath);

private:
    struct Entry {
        int id;
        QString project;
        AgentPanel *panel;
    };

    void ensureProject(const QString &path);
    AgentPanel *addAgent(const QString &projectPath, const QString &model = QString());
    AgentPanel *addDormantAgent(const QString &project, const QString &threadId,
                                const QString &title, bool isolated);
    void renameAgent(int agentId);
    void closeOtherProjects(const QString &keepPath);
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
    QWidget *m_dialogParent = nullptr; // window-scope parent for modal dialogs
    QList<Entry> m_agents;
    QStringList m_projects;
    // threadId → worktree path, refreshed from each git.snapshot. Lets the
    // roster resolve an agent's worktree dir without a fresh RPC round-trip.
    QHash<QString, QString> m_worktreePathByThread;
    int m_counter = 0;
};
