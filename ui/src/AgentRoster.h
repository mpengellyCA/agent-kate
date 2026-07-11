#pragma once

#include <QHash>
#include <QSet>
#include <QString>
#include <QStringList>
#include <QWidget>

class QAction;
class QLabel;
class QLineEdit;
class QToolButton;
class QTreeWidget;
class QTreeWidgetItem;

// AgentRoster is the left-hand navigation tree: projects as top-level rows,
// their agents nested beneath, each with a live status dot. Selecting an agent
// activates its conversation; selecting a project focuses that project.
class AgentRoster : public QWidget
{
    Q_OBJECT
public:
    explicit AgentRoster(QWidget *parent = nullptr);

    void addProject(const QString &path, const QString &name);
    void addAgent(const QString &projectPath, int agentId, const QString &title);
    // setAgentTitle(auto) is the live, derived-title path: it skips rows the
    // user has explicitly renamed (pinned). setAgentTitle(forced) is the
    // user-driven rename and always wins (and pins the row).
    void setAgentTitle(int agentId, const QString &title);
    void setAgentTitlePinned(int agentId, const QString &title);
    // Restore a title and clear the pin — used to roll back a failed rename on a
    // row the user had not previously pinned, so auto-titling resumes.
    void restoreAgentTitleUnpinned(int agentId, const QString &title);
    bool isAgentTitlePinned(int agentId) const;
    QString agentTitle(int agentId) const;
    void setAgentStatus(int agentId, const QString &dotColorHex);
    // A muted second line for the agent card (isolation / worktree / idle).
    // Derived locally in the UI for now — there is no backend description field.
    void setAgentSubtitle(int agentId, const QString &subtitle);
    // Worktree number (the same #N the WorktreeDashboard shows), so the
    // roster row can be cross-referenced with that table. 0 hides it.
    void setAgentNumber(int agentId, int number);
    // Organization tags shown as chips on the card and used by the tag filter.
    void setAgentTags(int agentId, const QStringList &tags);
    QStringList agentTags(int agentId) const;
    void setAgentDormant(int agentId, bool dormant);
    // "Needs your input" (Attention) signal, drawn as a card marker and rolled
    // up into a per-project count suffix. Busy ("working a turn") is intentionally
    // not surfaced in the roster — the status dot already conveys it.
    void setAgentAttention(int agentId, bool attention);
    void removeAgent(int agentId);
    void removeProject(const QString &path);
    void setCurrentAgent(int agentId);

    // The models offered by the "+ New Agent" dropdown (id, display label).
    void setModelChoices(const QList<QPair<QString, QString>> &models);

    // Programmatic equivalents of the context-menu git actions, so the window's
    // Agent menu can act on the active agent through the same wiring (these emit
    // the same signals the right-click menu does).
    void requestCommit(int agentId) { Q_EMIT commitRequested(agentId); }
    void requestPullRequest(int agentId) { Q_EMIT prRequested(agentId); }
    void requestMerge(int agentId) { Q_EMIT landRequested(agentId); }
    void requestDiscard(int agentId) { Q_EMIT discardRequested(agentId); }

Q_SIGNALS:
    void openProjectRequested();
    void newAgentRequested(const QString &projectPath);
    // newAgentWithModelRequested carries a pre-picked model id; addAgent
    // forwards it into the panel's model combo before the first start.
    void newAgentWithModelRequested(const QString &projectPath, const QString &model);
    void closeProjectRequested(const QString &projectPath);
    void closeOtherProjectsRequested(const QString &keepProjectPath);
    void openTerminalRequested(const QString &projectPath);
    void agentActivated(int agentId);
    void projectFocused(const QString &projectPath);
    void resumeRequested(int agentId);
    void renameRequested(int agentId);
    void forkRequested(int agentId);
    void commitRequested(int agentId);
    void prRequested(int agentId);
    void landRequested(int agentId);
    void discardRequested(int agentId);
    void closeRequested(int agentId);
    // Tagging: toggle one tag on an agent, or open the full tag editor.
    void addTagRequested(int agentId, const QString &tag);
    void removeTagRequested(int agentId, const QString &tag);
    void editTagsRequested(int agentId);
    // Run the Sonnet auto-organize pass over a project's agents.
    void autoOrganizeRequested(const QString &projectPath);
    // Open a Konsole tab rooted at the agent's worktree.
    void openWorktreeTerminalRequested(int agentId);

protected:
    void resizeEvent(QResizeEvent *event) override;
    void showEvent(QShowEvent *event) override;
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void setFilter(const QString &text);
    void applyFilter();
    // All distinct tags currently in use across agents in projectPath (or every
    // project when empty), in stable case-insensitive order. Reads the Tags role
    // from sibling items — no extra IPC.
    QStringList projectTags(const QString &projectPath) const;
    // Rebuild the tag-filter menu from the tags currently in use, preserving any
    // still-valid selections.
    void rebuildTagFilterMenu();
    void applyAttentionDisplay(QTreeWidgetItem *item);
    void recomputeProjectBadge(QTreeWidgetItem *project);
    void updateEmptyState();
    void openFileManager(const QString &path) const;
    QTreeWidgetItem *projectItem(const QString &path) const;
    QTreeWidgetItem *agentItem(int agentId) const;
    QString selectedProject() const;

    QLineEdit *m_filterEdit = nullptr;
    QToolButton *m_newButton = nullptr;
    QToolButton *m_tagFilterButton = nullptr;
    QTreeWidget *m_tree = nullptr;
    QLabel *m_emptyHint = nullptr;
    QString m_filter;
    QSet<QString> m_tagFilter; // lowercased tags the user is filtering by
    // The tag-filter menu's per-tag checkable actions, keyed by lowercased tag,
    // so rebuildTagFilterMenu() can diff (add new / drop departed) instead of
    // clearing and repopulating the whole menu on every tag change.
    QHash<QString, QAction *> m_tagFilterActions;
    QAction *m_tagFilterEmptyAct = nullptr; // "No tags yet" placeholder
    QAction *m_tagFilterSeparator = nullptr;
    QAction *m_tagFilterClearAct = nullptr;
    QList<QPair<QString, QString>> m_models;
};
