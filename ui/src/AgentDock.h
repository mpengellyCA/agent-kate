#pragma once

#include <QHash>
#include <QList>
#include <QObject>
#include <QSet>
#include <QString>
#include <QStringList>

class CoreClient;
class AgentPanel;
class AgentRoster;
class QStackedWidget;
class QWidget;
class QJsonArray;

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

    // Re-read configured API provider profiles into every not-yet-started panel's
    // provider picker (called after the Providers settings dialog closes).
    void reloadProviders();

    // threadId of the currently-active agent panel, empty if none / not started.
    QString currentThreadId() const;

    // Number of agents with a live process (used to decide whether app shutdown
    // needs the graceful stop-and-compact dialog).
    int runningAgentCount() const;

    // Persist the focused agent's thread so the next launch can land back in it.
    // Called on app shutdown to capture a thread that started while focused.
    void persistLastActiveSessions();

    // Worktree directory for the given agent, derived from the last git.snapshot.
    // Empty if the agent has no worktree yet (not started / not promoted).
    QString worktreePathForAgent(int agentId) const;

    // --- Active-agent actions, for the window's Agent menu / command palette ---
    // "Active" = the agent whose panel is currently shown in the stack. These let
    // the menu drive the same operations the roster's right-click menu does,
    // without the user having to find the right row.
    bool hasActiveAgent() const;
    bool activeAgentRunning() const;     // live process — Stop is meaningful
    bool activeAgentDormant() const;     // resumable — Resume is meaningful
    bool activeAgentHasWorktree() const; // git lifecycle ops are meaningful

    void newAgentInActiveProject();
    // Open the guided New Agent dialog, then create the agent with the chosen
    // model / sandbox / options and pre-fill the first task into its composer.
    void newAgentInActiveProjectGuided();
    void renameActiveAgent();
    void resumeActiveAgent();
    void attachToActiveAgent();
    void showActiveAgentChanges();
    void stopActiveAgent();
    void commitActiveAgent();
    void createPullRequestForActiveAgent();
    void mergeActiveAgent();
    void discardActiveAgentWorktree();
    void editActiveAgentTags();
    void openActiveAgentTerminal();
    void closeActiveAgent();

Q_SIGNALS:
    void statusMessage(const QString &text);
    void openDiff(const QString &title, const QString &diffText);
    void agentActivated(int agentId, const QString &projectPath);
    // Emitted when the CURRENTLY-shown agent's thread id is assigned/changes (e.g. a
    // freshly-created agent's session starting). agentActivated fires on selection,
    // before a fresh agent has a thread; this closes that gap for thread-keyed panels.
    void activeThreadChanged(const QString &threadId);
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
    // After a project's threads are restored on initial open, focus the
    // last-active one (dormant, not resumed) and prune the blank starter agent.
    void restoreInitialFocus(const QString &project);
    // Per-project last-active thread memory (KConfig), read on open / written on
    // activation. setLastActiveThread never clears: a blank starter agent must
    // not erase the remembered real session.
    void setLastActiveThread(const QString &project, const QString &threadId);
    QString lastActiveThread(const QString &project) const;
    // Pull git.snapshot and push each thread's worktree Number into the
    // roster, so the #N badge stays in sync with WorktreeDashboard.
    void refreshAgentNumbers();
    bool hasThread(const QString &threadId) const;
    // Add or remove one tag on an agent, optimistic with rollback on error.
    void mutateTag(int agentId, const QString &tag, bool add);
    // Open the tag editor for an agent and persist the result via agent.setTags.
    void editTags(int agentId);
    // Sonnet auto-organize: request proposals, then preview+apply.
    void autoOrganize(const QString &projectPath);
    void showOrganizeProposals(const QString &projectPath,
                               const QJsonArray &proposals);
    void removeAgentEntry(int agentId);
    void closeAgent(int agentId);
    void closeProject(const QString &path);
    Entry *entryById(int agentId);
    Entry *entryByPanel(const AgentPanel *panel);
    Entry *entryByThread(const QString &threadId);
    // The active panel (top of the stack) and its agent id / project, or
    // nullptr / -1 / empty when there is no active agent.
    AgentPanel *activeAgentPanel() const;
    int activeAgentId() const;
    QString activeProjectPath() const;

    CoreClient *m_core = nullptr;
    QStackedWidget *m_stack = nullptr;
    AgentRoster *m_roster = nullptr;
    QWidget *m_dialogParent = nullptr; // window-scope parent for modal dialogs
    QList<Entry> m_agents;
    QStringList m_projects;
    // threadId → worktree path, refreshed from each git.snapshot. Lets the
    // roster resolve an agent's worktree dir without a fresh RPC round-trip.
    QHash<QString, QString> m_worktreePathByThread;
    // Projects whose initial thread-restore should jump focus to the last-active
    // agent, plus the blank starter agent created for each (pruned once we land
    // in the restored session). Populated in addProject, drained in restore.
    QSet<QString> m_pendingFocusProjects;
    QHash<QString, int> m_initialAgentByProject;
    int m_counter = 0;
};
