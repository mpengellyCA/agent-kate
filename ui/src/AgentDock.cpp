#include "AgentDock.h"
#include "AgentPanel.h"
#include "AgentRoster.h"
#include "ipc/CoreClient.h"

#include <QDir>
#include <QFileDialog>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonObject>
#include <QLineEdit>
#include <QMessageBox>
#include <QStackedWidget>
#include <QVBoxLayout>

AgentDock::AgentDock(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
    , m_stack(new QStackedWidget(this))
    , m_roster(new AgentRoster)
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_stack);

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
    connect(m_roster, &AgentRoster::closeProjectRequested, this, &AgentDock::closeProject);
    connect(m_roster, &AgentRoster::projectFocused, this, &AgentDock::projectFocused);
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
    });
    connect(m_roster, &AgentRoster::commitRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e || e->panel->threadId().isEmpty()) {
            emit statusMessage(QStringLiteral("Start the agent before committing"));
            return;
        }
        // A non-isolated agent commits onto the workspace's current branch —
        // make that explicit before it lands somewhere unexpected (e.g. main).
        if (!e->panel->isIsolated()
            && QMessageBox::warning(
                   this, QStringLiteral("Commit in the workspace"),
                   QStringLiteral(
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
            this, QStringLiteral("Commit agent changes"), QStringLiteral("Commit message:"),
            QLineEdit::Normal, QStringLiteral("AgentKate change"), &ok);
        if (!ok) {
            return;
        }
        m_core->call(QStringLiteral("agent.commit"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()},
                                 {QStringLiteral("message"), msg}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(QStringLiteral("Commit failed: %1")
                                 .arg(error.value(QStringLiteral("message")).toString()));
                         } else {
                             const QString branch =
                                 result.value(QStringLiteral("branch")).toString();
                             emit statusMessage(
                                 branch.isEmpty()
                                     ? QStringLiteral("Committed the agent's changes")
                                     : QStringLiteral("Committed to %1").arg(branch));
                         }
                     });
    });
    connect(m_roster, &AgentRoster::prRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e || e->panel->threadId().isEmpty()) {
            emit statusMessage(QStringLiteral("Start the agent before opening a PR"));
            return;
        }
        bool ok = false;
        const QString title = QInputDialog::getText(
            this, QStringLiteral("Create pull request"), QStringLiteral("Pull request title:"),
            QLineEdit::Normal, QString(), &ok);
        if (!ok) {
            return;
        }
        m_core->call(QStringLiteral("agent.openPR"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()},
                                 {QStringLiteral("title"), title}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(QStringLiteral("Pull request failed: %1")
                                 .arg(error.value(QStringLiteral("message")).toString()));
                         } else {
                             emit statusMessage(QStringLiteral("Pull request opened: %1")
                                 .arg(result.value(QStringLiteral("url")).toString()));
                         }
                     });
    });
    connect(m_roster, &AgentRoster::landRequested, this, [this](int id) {
        Entry *e = entryById(id);
        if (!e || e->panel->threadId().isEmpty()) {
            emit statusMessage(QStringLiteral("Start the agent before merging its work"));
            return;
        }
        if (!e->panel->isIsolated()) {
            emit statusMessage(QStringLiteral(
                "This agent runs in the workspace — it has no branch to merge"));
            return;
        }
        if (QMessageBox::question(
                this, QStringLiteral("Merge into local main"),
                QStringLiteral(
                    "Merge this agent's branch into your local main branch?\n\n"
                    "Its commits are merged into the workspace locally — nothing "
                    "is pushed to GitHub."))
            != QMessageBox::Yes) {
            return;
        }
        m_core->call(QStringLiteral("agent.land"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(QStringLiteral("Merge failed: %1")
                                 .arg(error.value(QStringLiteral("message")).toString()));
                         } else {
                             emit statusMessage(QStringLiteral("Merged %1 into %2")
                                 .arg(result.value(QStringLiteral("branch")).toString(),
                                      result.value(QStringLiteral("into")).toString()));
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
        if (QMessageBox::question(this, QStringLiteral("Discard worktree"),
                QStringLiteral("Discard this agent's worktree and all of its uncommitted "
                               "changes? This cannot be undone."))
            != QMessageBox::Yes) {
            return;
        }
        m_core->call(QStringLiteral("agent.discard"),
                     QJsonObject{{QStringLiteral("threadId"), e->panel->threadId()}},
                     [this](const QJsonObject &, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             emit statusMessage(QStringLiteral("Discard failed: %1")
                                 .arg(error.value(QStringLiteral("message")).toString()));
                         } else {
                             emit statusMessage(QStringLiteral("Discarded the agent's worktree"));
                         }
                     });
    });
}

QWidget *AgentDock::roster() const
{
    return m_roster;
}

void AgentDock::openProjectDialog()
{
    const QString dir = QFileDialog::getExistingDirectory(
        this, QStringLiteral("Open Project Folder"),
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
            emit statusMessage(QStringLiteral("That session is already attached"));
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

AgentPanel *AgentDock::addAgent(const QString &projectPath)
{
    const int id = ++m_counter;
    auto *panel = new AgentPanel(m_core, this);
    panel->setWorkspace(projectPath);
    m_stack->addWidget(panel);
    m_agents.append(Entry{id, projectPath, panel});

    m_roster->addAgent(projectPath, id, QStringLiteral("Agent %1").arg(id));
    wireAgentPanel(id, panel);

    m_roster->setCurrentAgent(id); // activates it via the roster
    return panel;
}

// addDormantAgent restores a persisted, not-running thread into the roster
// without stealing focus from the active agent.
AgentPanel *AgentDock::addDormantAgent(const QString &project, const QString &threadId,
                                       const QString &title, bool isolated)
{
    const int id = ++m_counter;
    auto *panel = new AgentPanel(m_core, this);
    panel->setWorkspace(project);
    m_stack->addWidget(panel);
    m_agents.append(Entry{id, project, panel});

    const QString label = title.isEmpty() ? QStringLiteral("Agent %1").arg(id) : title;
    m_roster->addAgent(project, id, label);
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
    connect(panel, &AgentPanel::dormantChanged, this,
            [this, agentId](bool dormant) { m_roster->setAgentDormant(agentId, dormant); });
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
        emit statusMessage(QStringLiteral("AgentKate keeps at least one project open"));
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
    if (m_agents.isEmpty() && !m_projects.isEmpty()) {
        addAgent(m_projects.constFirst());
    }
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
