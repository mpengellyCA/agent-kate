#include "AgentDock.h"
#include "RecentProjects.h"
#include "AgentPanel.h"
#include "AgentRoster.h"
#include "ipc/CoreClient.h"

#include <QDir>
#include <QFileDialog>
#include <QHash>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonObject>
#include <QLineEdit>
#include <QMessageBox>
#include <QStackedWidget>

#include <KLocalizedString>

AgentDock::AgentDock(CoreClient *core, QWidget *parent)
    : QObject(parent)
    , m_core(core)
    , m_stack(new QStackedWidget(parent))
    , m_roster(new AgentRoster(parent))
    , m_dialogParent(parent)
{
    connect(m_roster, &AgentRoster::openProjectRequested, this, &AgentDock::openProjectDialog);
    connect(m_roster, &AgentRoster::newAgentRequested, this, [this](const QString &project) {
        QString p = project;
        if (p.isEmpty() && !m_projects.isEmpty()) {
            p = m_projects.constLast();
        }
        if (!p.isEmpty()) {
            addAgent(p);
        }
    });
    connect(m_roster, &AgentRoster::newAgentWithModelRequested, this,
            [this](const QString &project, const QString &model) {
                QString p = project;
                if (p.isEmpty() && !m_projects.isEmpty()) {
                    p = m_projects.constLast();
                }
                if (!p.isEmpty()) {
                    addAgent(p, model);
                }
            });
    connect(m_roster, &AgentRoster::closeProjectRequested, this, &AgentDock::closeProject);
    connect(m_roster, &AgentRoster::closeOtherProjectsRequested, this,
            &AgentDock::closeOtherProjects);
    connect(m_roster, &AgentRoster::openTerminalRequested, this,
            &AgentDock::openTerminalRequested); // routed to MainWindow
    connect(m_roster, &AgentRoster::renameRequested, this, &AgentDock::renameAgent);
    connect(m_roster, &AgentRoster::projectFocused, this, &AgentDock::projectFocused);

    // Seed the "+ New Agent" model dropdown. Ids must match AgentPanel's combo.
    m_roster->setModelChoices({
        {QString(), i18n("Default model")},
        {QStringLiteral("opus"), i18n("Opus")},
        {QStringLiteral("sonnet"), i18n("Sonnet")},
        {QStringLiteral("haiku"), i18n("Haiku")},
        {QStringLiteral("fable"), i18n("Fable")},
    });
    connect(m_roster, &AgentRoster::agentActivated, this, [this](int id) {
        if (Entry *e = entryById(id)) {
            m_stack->setCurrentWidget(e->panel);
            emit agentActivated(e->id, e->project);
        }
    });
    connect(m_roster, &AgentRoster::closeRequested, this, &AgentDock::closeAgent);
    connect(m_roster, &AgentRoster::resumeRequested, this, [this](int id) {
        if (Entry *e = entryById(id)) {
            m_roster->setCurrentAgent(id);
            e->panel->resume();
        }
    });
    // Whenever the core (re)connects, restore each project's dormant threads.
    connect(m_core, &CoreClient::connected, this, [this] {
        const auto projects = m_projects;
        for (const QString &project : projects) {
            restoreThreads(project);
        }
        refreshAgentNumbers();
    });
    // git.invalidated fires whenever a worktree appears / mutates, which is
    // exactly when an agent's #N may have just been assigned (fresh start)
    // or changed. Keep the roster badges in sync with the dashboard.
    connect(m_core, &CoreClient::notification, this,
            [this](const QString &method, const QJsonObject &params) {
                if (method == QLatin1String("git.invalidated")) {
                    refreshAgentNumbers();
                } else if (method == QLatin1String("agent.discarded")) {
                    const QString threadId =
                        params.value(QStringLiteral("threadId")).toString();
                    if (threadId.isEmpty()) {
                        return;
                    }
                    for (const Entry &e : m_agents) {
                        if (e.panel->threadId() == threadId) {
                            closeAgent(e.id);
                            break;
                        }
                    }
                }
            });
    connect(m_roster, &AgentRoster::commitRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e || e->panel->threadId().isEmpty()) {
            emit statusMessage(i18n("Start the agent before committing"));
            return;
        }
        // A non-isolated agent commits onto the workspace's current branch —
        // make that explicit before it lands somewhere unexpected (e.g. main).
        if (!e->panel->isIsolated()
            && QMessageBox::warning(
                   m_dialogParent, i18n("Commit in the workspace"),
                   i18n(
                       "This agent runs directly in the workspace, so the commit "
                       "will land on the workspace's current branch — it is not "
                       "isolated.\n\nPromote the agent to a worktree first to keep "
                       "its commits on their own branch.\n\nCommit here anyway?"),
                   QMessageBox::Yes | QMessageBox::Cancel, QMessageBox::Cancel)
                   != QMessageBox::Yes) {
            return;
        }
        bool ok = false;
        const QString msg = QInputDialog::getText(
            m_dialogParent, i18n("Commit agent changes"), i18n("Commit message:"),
            QLineEdit::Normal, i18n("Agent Kate change"), &ok);
        if (!ok) {
            return;
        }
        m_core->call(QStringLiteral("agent.commit"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()},
                                 {QStringLiteral("message"), msg}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(i18n("Commit failed: %1",
                                 error.value(QStringLiteral("message")).toString()));
                         } else {
                             const QString branch =
                                 result.value(QStringLiteral("branch")).toString();
                             emit statusMessage(
                                 branch.isEmpty()
                                     ? i18n("Committed the agent's changes")
                                     : i18n("Committed to %1", branch));
                         }
                     });
    });
    connect(m_roster, &AgentRoster::prRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e || e->panel->threadId().isEmpty()) {
            emit statusMessage(i18n("Start the agent before opening a PR"));
            return;
        }
        bool ok = false;
        const QString title = QInputDialog::getText(
            m_dialogParent, i18n("Create pull request"), i18n("Pull request title:"),
            QLineEdit::Normal, QString(), &ok);
        if (!ok) {
            return;
        }
        m_core->call(QStringLiteral("agent.openPR"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()},
                                 {QStringLiteral("title"), title}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(i18n("Pull request failed: %1",
                                 error.value(QStringLiteral("message")).toString()));
                         } else {
                             emit statusMessage(i18n("Pull request opened: %1",
                                 result.value(QStringLiteral("url")).toString()));
                         }
                     });
    });
    connect(m_roster, &AgentRoster::landRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e || e->panel->threadId().isEmpty()) {
            emit statusMessage(i18n("Start the agent before merging its work"));
            return;
        }
        if (!e->panel->isIsolated()) {
            emit statusMessage(i18n(
                "This agent runs in the workspace — it has no branch to merge"));
            return;
        }
        if (QMessageBox::question(
                m_dialogParent, i18n("Merge into local main"),
                i18n(
                    "Merge this agent's branch into your local main branch?\n\n"
                    "Its commits are merged into the workspace locally — nothing "
                    "is pushed to GitHub."))
            != QMessageBox::Yes) {
            return;
        }
        m_core->call(QStringLiteral("agent.land"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         // Land is a deliberate, destructive-ish operation —
                         // its result must be unmissable, not a transient toast.
                         if (!error.isEmpty()) {
                             const QString msg =
                                 error.value(QStringLiteral("message")).toString();
                             emit statusMessage(i18n("Merge failed: %1", msg));
                             QMessageBox::warning(m_dialogParent,
                                 i18n("Merge into local main failed"), msg);
                         } else {
                             const QString branch =
                                 result.value(QStringLiteral("branch")).toString();
                             const QString into =
                                 result.value(QStringLiteral("into")).toString();
                             emit statusMessage(i18n("Merged %1 into %2", branch, into));
                             QMessageBox::information(m_dialogParent,
                                 i18n("Merge into local main"),
                                 i18n("Merged %1 into %2.", branch, into));
                         }
                     });
    });
    connect(m_roster, &AgentRoster::discardRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e) {
            return;
        }
        if (e->panel->threadId().isEmpty()) {
            closeAgent(id);
            return;
        }
        if (QMessageBox::question(m_dialogParent, i18n("Discard worktree"),
                i18n("Discard this agent's worktree and all of its uncommitted "
                     "changes? This cannot be undone."))
            != QMessageBox::Yes) {
            return;
        }
        m_core->call(QStringLiteral("agent.discard"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()}},
                     [this](const QJsonObject &, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(i18n("Discard failed: %1",
                                 error.value(QStringLiteral("message")).toString()));
                         } else {
                             emit statusMessage(i18n("Discarded the agent's worktree"));
                         }
                     });
    });
}

QWidget *AgentDock::roster() const
{
    return m_roster;
}

QStackedWidget *AgentDock::panelStack() const
{
    return m_stack;
}

QWidget *AgentDock::activePanel() const
{
    return m_stack ? m_stack->currentWidget() : nullptr;
}

void AgentDock::openProjectDialog()
{
    const QString dir = QFileDialog::getExistingDirectory(
        m_dialogParent, i18n("Open Project Folder"),
        m_projects.isEmpty() ? QDir::homePath() : m_projects.constLast());
    if (!dir.isEmpty()) {
        addProject(dir);
    }
}

// ensureProject adds a project row to the roster if it is not already open.
void AgentDock::ensureProject(const QString &path)
{
    if (m_projects.contains(path)) {
        return;
    }
    m_projects.append(path);
    QString name = QDir(path).dirName();
    if (name.isEmpty()) {
        name = path;
    }
    m_roster->addProject(path, name);
    RecentProjects::remember(path); // welcome screen reads this on next launch
}

void AgentDock::addProject(const QString &path)
{
    const bool wasOpen = m_projects.contains(path);
    ensureProject(path);
    addAgent(path);
    if (!wasOpen) {
        restoreThreads(path); // bring back this project's dormant threads
    }
}

void AgentDock::attachSession(const QString &project, const QString &threadId,
                              const QString &title)
{
    // Already shown? Just focus it.
    for (const Entry &e : m_agents) {
        if (e.panel->threadId() == threadId) {
            m_roster->setCurrentAgent(e.id);
            emit statusMessage(i18n("That session is already attached"));
            return;
        }
    }
    ensureProject(project);
    // An attached external session resumes in its own directory, non-isolated.
    AgentPanel *panel = addDormantAgent(project, threadId, title, /*isolated=*/false);
    if (!m_agents.isEmpty()) {
        m_roster->setCurrentAgent(m_agents.constLast().id); // focus the new agent
    }
    panel->resume();
}

void AgentDock::applyChatSettings()
{
    for (const Entry &e : m_agents) {
        e.panel->applyChatSettings();
    }
}

QString AgentDock::currentThreadId() const
{
    QWidget *w = m_stack->currentWidget();
    if (!w) {
        return {};
    }
    if (auto *panel = qobject_cast<AgentPanel *>(w)) {
        return panel->threadId();
    }
    return {};
}

AgentPanel *AgentDock::addAgent(const QString &projectPath, const QString &model)
{
    const int id = ++m_counter;
    auto *panel = new AgentPanel(m_core, m_stack);
    m_stack->addWidget(panel);
    m_agents.append(Entry{id, projectPath, panel});

    m_roster->addAgent(projectPath, id, i18n("Agent %1", id));
    wireAgentPanel(id, panel);
    // Apply a pre-picked model before the first start (the combo is still free).
    panel->preselectModel(model);
    // Set the workspace after wiring so the panel's first refresh() reaches the
    // roster — that's what seeds the card's initial status dot and subtitle.
    panel->setWorkspace(projectPath);

    m_roster->setCurrentAgent(id); // activates it via the roster
    return panel;
}

// addDormantAgent restores a persisted, not-running thread into the roster
// without stealing focus from the active agent.
AgentPanel *AgentDock::addDormantAgent(const QString &project, const QString &threadId,
                                       const QString &title, bool isolated)
{
    const int id = ++m_counter;
    auto *panel = new AgentPanel(m_core, m_stack);
    panel->setWorkspace(project);
    m_stack->addWidget(panel);
    m_agents.append(Entry{id, project, panel});

    const QString label = title.isEmpty() ? i18n("Agent %1", id) : title;
    m_roster->addAgent(project, id, label);
    // A persisted title is authoritative on restore (it may have been renamed)
    // — pin it so a resumed transcript's auto-derived title can't overwrite it.
    if (!title.isEmpty()) {
        m_roster->setAgentTitlePinned(id, title);
    }
    wireAgentPanel(id, panel);
    panel->setDormant(threadId, label, isolated); // emits dormantChanged
    return panel;
}

void AgentDock::wireAgentPanel(int agentId, AgentPanel *panel)
{
    connect(panel, &AgentPanel::statusMessage, this, &AgentDock::statusMessage);
    connect(panel, &AgentPanel::openDiff, this, &AgentDock::openDiff);
    connect(panel, &AgentPanel::titleChanged, this,
            [this, agentId](const QString &title) { m_roster->setAgentTitle(agentId, title); });
    connect(panel, &AgentPanel::stateChanged, this,
            [this, agentId](const QString &dot) { m_roster->setAgentStatus(agentId, dot); });
    connect(panel, &AgentPanel::subtitleChanged, this,
            [this, agentId](const QString &text) { m_roster->setAgentSubtitle(agentId, text); });
    connect(panel, &AgentPanel::dormantChanged, this,
            [this, agentId](bool dormant) { m_roster->setAgentDormant(agentId, dormant); });
    connect(panel, &AgentPanel::attentionChanged, this,
            [this, agentId](bool on) { m_roster->setAgentAttention(agentId, on); });
}

bool AgentDock::hasThread(const QString &threadId) const
{
    for (const Entry &e : m_agents) {
        if (e.panel->threadId() == threadId) {
            return true;
        }
    }
    return false;
}

// restoreThreads asks the core for this project's persisted threads and adds
// any not already shown as dormant agents. Safe to call repeatedly.
void AgentDock::restoreThreads(const QString &project)
{
    if (!m_core->isConnected()) {
        return; // the core-connected handler will retry once it is up
    }
    m_core->call(QStringLiteral("session.listThreads"),
                 QJsonObject{{QStringLiteral("project"), project}},
                 [this, project](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     const QJsonArray threads =
                         result.value(QStringLiteral("threads")).toArray();
                     for (const QJsonValue &v : threads) {
                         const QJsonObject rec = v.toObject();
                         const QString threadId =
                             rec.value(QStringLiteral("threadId")).toString();
                         if (threadId.isEmpty() || hasThread(threadId)) {
                             continue;
                         }
                         const bool isolated = rec.value(QStringLiteral("worktree"))
                                                   .toObject()
                                                   .value(QStringLiteral("isolated"))
                                                   .toBool();
                         addDormantAgent(project, threadId,
                                         rec.value(QStringLiteral("title")).toString(),
                                         isolated);
                     }
                     refreshAgentNumbers();
                 });
}

void AgentDock::refreshAgentNumbers()
{
    if (!m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("git.snapshot"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     QHash<QString, int> byThread;
                     const QJsonArray threads =
                         result.value(QStringLiteral("threads")).toArray();
                     for (const QJsonValue &v : threads) {
                         const QJsonObject o = v.toObject();
                         const QString id =
                             o.value(QStringLiteral("threadId")).toString();
                         const int n = o.value(QStringLiteral("number")).toInt();
                         if (!id.isEmpty() && n > 0) {
                             byThread.insert(id, n);
                         }
                     }
                     for (const Entry &e : m_agents) {
                         const QString tid = e.panel->threadId();
                         if (tid.isEmpty()) {
                             continue;
                         }
                         m_roster->setAgentNumber(e.id, byThread.value(tid, 0));
                     }
                 });
}

void AgentDock::removeAgentEntry(int agentId)
{
    for (int i = 0; i < m_agents.size(); ++i) {
        if (m_agents.at(i).id == agentId) {
            AgentPanel *panel = m_agents.at(i).panel;
            m_agents.removeAt(i);
            m_stack->removeWidget(panel);
            panel->deleteLater(); // ~AgentPanel stops its agent
            m_roster->removeAgent(agentId);
            return;
        }
    }
}

void AgentDock::closeAgent(int agentId)
{
    removeAgentEntry(agentId);
    if (m_agents.isEmpty() && !m_projects.isEmpty()) {
        addAgent(m_projects.constFirst()); // keep one agent available
    }
}

void AgentDock::closeProject(const QString &path)
{
    if (m_projects.size() <= 1) {
        emit statusMessage(i18n("Agent Kate keeps at least one project open"));
        return;
    }
    QList<int> ids;
    for (const Entry &e : m_agents) {
        if (e.project == path) {
            ids.append(e.id);
        }
    }
    for (int id : ids) {
        removeAgentEntry(id);
    }
    m_projects.removeAll(path);
    m_roster->removeProject(path);
    // The project is gone for good — tell consumers (e.g. its terminal tabs) to
    // release any per-project resources. This is the one place destroying a
    // terminal is correct: an explicit close, never a switch.
    emit projectClosed(path);
    if (m_agents.isEmpty() && !m_projects.isEmpty()) {
        addAgent(m_projects.constFirst());
    }
}

// closeOtherProjects closes every project except keepPath, respecting the
// keep-at-least-one guard (keepPath is the one we keep, so it always holds).
void AgentDock::closeOtherProjects(const QString &keepPath)
{
    const QStringList others = [&] {
        QStringList l;
        for (const QString &p : m_projects) {
            if (p != keepPath) {
                l.append(p);
            }
        }
        return l;
    }();
    for (const QString &p : others) {
        closeProject(p); // guard inside keeps the last one if needed
    }
}

// renameAgent prompts for a new title, applies it optimistically to the card,
// and persists it through the agent.rename IPC. On failure the card is
// restored to its previous title and the user is told why.
void AgentDock::renameAgent(int agentId)
{
    Entry *e = entryById(agentId);
    if (!e) {
        return;
    }
    bool ok = false;
    const QString current = m_roster->agentTitle(agentId);
    const QString title = QInputDialog::getText(
                              m_dialogParent, i18n("Rename agent"),
                              i18n("Agent name:"), QLineEdit::Normal, current, &ok)
                              .trimmed();
    // Reject an empty rename (and a no-op).
    if (!ok || title.isEmpty() || title == current) {
        return;
    }
    const QString threadId = e->panel->threadId();
    const bool wasPinned = m_roster->isAgentTitlePinned(agentId);
    // Optimistic: show (and pin) the new title now, roll back if the core
    // rejects it. With no backend thread yet the rename is local-only (still
    // pinned so the first derived title can't clobber it).
    m_roster->setAgentTitlePinned(agentId, title);
    if (threadId.isEmpty()) {
        return;
    }
    m_core->call(QStringLiteral("agent.rename"),
                 QJsonObject{{QStringLiteral("threadId"), threadId},
                             {QStringLiteral("title"), title}},
                 [this, agentId, current, title, wasPinned](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         // Restore the prior title. Only re-pin if the row was
                         // already user-pinned before this attempt — a failed
                         // rename must not permanently disable auto-titling on a
                         // row the user had never pinned.
                         if (wasPinned) {
                             m_roster->setAgentTitlePinned(agentId, current);
                         } else {
                             m_roster->restoreAgentTitleUnpinned(agentId, current);
                         }
                         emit statusMessage(i18n("Rename failed: %1",
                             error.value(QStringLiteral("message")).toString()));
                     } else {
                         emit statusMessage(i18n("Renamed agent to “%1”", title));
                     }
                 });
}

AgentDock::Entry *AgentDock::entryById(int agentId)
{
    for (Entry &e : m_agents) {
        if (e.id == agentId) {
            return &e;
        }
    }
    return nullptr;
}
