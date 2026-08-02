#pragma once

#include "state/AgentJob.h"

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

namespace agentkate {
class AgentNotifier;
}

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
    // Isolated == the agent has a PRIVATE branch of its own. Deliberately not
    // the same question as activeAgentHasWorktree(): git.snapshot reports a
    // path for every thread, workspace-mode threads included, so "has a path"
    // enabled Merge for agents with nothing to merge from — the same
    // isolated-vs-workspace asymmetry that made Discard a data-loss bug (F29).
    bool activeAgentIsolated() const;

    void newAgentInActiveProject();
    // The guided New Agent dialog for one named project. Every visible "new
    // agent" control routes here (audit F45) — the bare panel with five
    // unlabeled combos is not a first-run experience.
    void newAgentGuided(const QString &projectPath);
    // Apply an ensemble (plan 16 P4): the core starts its controller, already
    // briefed, and the dock adopts that thread into a roster panel. task is the
    // human's first instruction, appended to the controller's briefing.
    void applyEnsemble(const QString &projectPath, const QString &ensemble,
                       const QString &task = QString());
    // Open the ensemble editor (roster quick menu / Agent menu).
    void openEnsembleEditor();
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

    // Jobs-panel actions, routed to the agents that own the work.
    void forgetFinishedJobsEverywhere();
    void showWorkflowMonitorFor(const QString &threadId);
    void selectAgentByThread(const QString &threadId);

Q_SIGNALS:
    void statusMessage(const QString &text);
    void openDiff(const QString &title, const QString &diffText);
    // Routed to MainWindow to open a file (a clicked attachment chip) in the
    // editor, making the editor visible if the layout was chat-only.
    void openFileRequested(const QString &path);
    // worktreePath is the selected agent's isolated worktree (empty when it
    // runs non-isolated in the workspace or has no worktree yet), so the file
    // browser can offer a Worktree scope tab alongside the Project one.
    void agentActivated(int agentId, const QString &projectPath,
                        const QString &worktreePath);
    // Emitted when the CURRENTLY-shown agent's thread id is assigned/changes (e.g. a
    // freshly-created agent's session starting). agentActivated fires on selection,
    // before a fresh agent has a thread; this closes that gap for thread-keyed panels.
    void activeThreadChanged(const QString &threadId);
    // Emitted when the CURRENTLY-shown agent's isolated worktree path becomes
    // known or changes (a live promote, or a dormant agent's path arriving in a
    // git.snapshot after activation). Lets the file browser re-root its Worktree
    // tab without a full agent re-activation.
    void activeWorktreeChanged(const QString &worktreePath);
    void projectFocused(const QString &projectPath);
    // Emitted when a project is explicitly CLOSED (not merely switched away
    // from). Consumers tied to a project — e.g. its terminal tabs — tear down.
    void projectClosed(const QString &projectPath);
    // Routed to MainWindow to focus the Terminal panel at this project path.
    void openTerminalRequested(const QString &projectPath);
    // Carries an agent's WORKTREE path (distinct from a project path) so the
    // shell can open a terminal rooted there.
    void openWorktreeTerminalRequested(const QString &worktreePath);
    // Human agent titles keyed by thread id, re-pushed on every event that can
    // change the mapping (a rename, a thread being bound, a git.snapshot). The
    // WorktreeDashboard names each card from these; the Jobs panel names each
    // agent group, which would otherwise read as a raw uuid.
    void agentTitlesChanged(const QHash<QString, QString> &titlesByThread);
    // One agent's full background-job set, forwarded from its panel to the Jobs
    // panel. An empty vector means "this agent has none" — which is also how a
    // closing agent's rows are reaped.
    void jobsChanged(const QString &threadId, const QVector<agentkate::AgentJob> &jobs);
    // The in-chat tray's "N finished" chip asks the window to raise the Jobs panel.
    void openJobsPanelRequested();
    // A desktop notification was clicked: the window must un-minimise, raise and
    // take focus. The dock has already selected the agent it was about.
    void raiseWindowRequested();
    // How many agents are blocked on the user, forwarded from the roster so the
    // window can put it in the task bar (audit F50).
    void attentionCountChanged(int count);

private:
    struct Entry {
        int id;
        QString project;
        AgentPanel *panel;
    };

    // Effective worktree root to scope the file browser's Worktree tab for an
    // agent: the panel's live workdir when known (isolated only), else the
    // snapshot-derived path gated on the panel's isolated flag. Empty for
    // non-isolated agents so the tab disables.
    QString worktreeRootForAgent(int agentId) const;
    // Re-resolve and push the shown agent's worktree scope to the file browser.
    void emitActiveWorktree();
    // Emit activeWorktreeChanged only when the path actually differs from the
    // last one pushed. git.invalidated (and thus refreshAgentNumbers →
    // emitActiveWorktree) fires on every file an agent touches; without this
    // guard the file browser would re-root — collapsing the user's expansion and
    // scroll — constantly. A sentinel distinguishes "nothing emitted yet" from an
    // emitted empty path so the first empty scope still propagates.
    void pushActiveWorktree(const QString &worktreePath);

    void ensureProject(const QString &path);
    // Select an already-open project's most relevant agent. False when the
    // project has no agents left to select.
    bool focusExistingProject(const QString &path);
    // (Re)build the roster's "+ New Agent" quick menu from the harness
    // registry: an engine section per harness, its tier tokens or cached
    // discovered models beneath. Called at startup and on HarnessRegistry
    // changes (a capability refresh or a landed option probe).
    void seedEngineChoices();
    AgentPanel *addAgent(const QString &projectPath, const QString &model = QString(),
                         const QString &backend = QString());
    AgentPanel *addDormantAgent(const QString &project, const QString &threadId,
                                const QString &title, bool isolated,
                                const QString &backend = QString());
    void renameAgent(int agentId);
    // Fork an agent: show the Fork dialog prefilled from the source, call
    // agent.fork, then adopt the newly-started thread into its own roster panel.
    void forkAgent(int agentId);
    void closeOtherProjects(const QString &keepPath);
    void wireAgentPanel(int agentId, AgentPanel *panel);
    void restoreThreads(const QString &project);
    // Settle every on-screen agent after the core was replaced by a fresh
    // process: their threads died with the old one, and only the UI knows it.
    void handleCoreRespawn();
    // Reconcile the roster with the core's orchestration linkage (plan 16 P5):
    // adopt agent-launched workers, nest them under their controller, and push
    // roles for the ⇄ badges.
    void refreshOrchestrationLinks();
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
    // Emit agentTitlesChanged from the roster's current titles. Split out of
    // refreshAgentNumbers because that one only ever fires from a git.snapshot
    // reply: a rename, a freshly-bound thread or a newly-wired panel would
    // otherwise not reach the title consumers until git next moved, leaving the
    // Jobs panel naming agents by raw thread uuid.
    void pushAgentTitles();
    // Push each agent's isolation + "has a working directory" onto its roster
    // row, so the roster's right-click menu gates its actions from exactly the
    // facts the window's &Agent menu reads. Cheap and synchronous (no RPC) for
    // the same reason pushAgentTitles is: a thread binding or a promote must
    // reach the menu before the next git.snapshot, or the primary path spends
    // minutes offering actions the core will refuse.
    void pushAgentActionFacts();
    bool hasThread(const QString &threadId) const;
    // Add or remove one tag on an agent, optimistic with rollback on error.
    void mutateTag(int agentId, const QString &tag, bool add);
    // Open the tag editor for an agent and persist the result via agent.setTags.
    void editTags(int agentId);
    // Sonnet auto-organize: request proposals, then preview+apply.
    void autoOrganize(const QString &projectPath);
    void showOrganizeProposals(const QString &projectPath,
                               const QJsonArray &proposals);
    // What to do with the agent's persisted composer draft when its entry goes.
    //
    // Keep is the default and it is the default for a REASON: Close is not
    // destruction. A closed agent is re-openable (its session is archived, its
    // worktree is still there) and must find the text it had waiting. Clearing
    // on every teardown fixed a stale `draft-…` config key by destroying the
    // user's unsent words — a strictly worse trade.
    enum class DraftDisposition {
        Keep,  // the agent can come back; its draft must come back with it
        Forget // the thread is GONE (discarded / cleaned up / archived away)
    };
    void removeAgentEntry(int agentId,
                          DraftDisposition drafts = DraftDisposition::Keep);
    void closeAgent(int agentId, DraftDisposition drafts = DraftDisposition::Keep);
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
    // Desktop alerts for agents the user is not currently watching.
    agentkate::AgentNotifier *m_notifier = nullptr;
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
    // Last worktree path pushed to the file browser via activeWorktreeChanged.
    // Sentinel (never a valid path) means "nothing emitted yet"; see
    // pushActiveWorktree.
    QString m_lastEmittedWorktree = QStringLiteral("\x01<uninitialised>");
    int m_counter = 0;
};
