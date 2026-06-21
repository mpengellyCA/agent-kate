#include "AgentDock.h"
#include "RecentProjects.h"
#include "AgentPanel.h"
#include "AgentRoster.h"
#include "AutoOrganizeDialog.h"
#include "TagEditorDialog.h"
#include "ipc/CoreClient.h"

#include <QDir>
#include <QFileDialog>
#include <QHash>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QLineEdit>
#include <QMessageBox>
#include <QPointer>
#include <QSet>
#include <QStackedWidget>
#include <QVector>

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

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
            // Remember where we are so the next launch lands here. No-op for a
            // blank starter agent (empty thread) — see setLastActiveThread.
            setLastActiveThread(e->project, e->panel->threadId());
            emit agentActivated(e->id, e->project);
        }
    });
    connect(m_roster, &AgentRoster::closeRequested, this, &AgentDock::closeAgent);
    connect(m_roster, &AgentRoster::openWorktreeTerminalRequested, this, [this](int id) {
        const QString p = worktreePathForAgent(id);
        if (!p.isEmpty()) {
            emit openWorktreeTerminalRequested(p);
        }
    });
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
                } else if (method == QLatin1String("agent.tagsChanged")) {
                    const QString threadId =
                        params.value(QStringLiteral("threadId")).toString();
                    if (threadId.isEmpty()) {
                        return;
                    }
                    QStringList tags;
                    const QJsonArray arr =
                        params.value(QStringLiteral("tags")).toArray();
                    for (const QJsonValue &v : arr) {
                        tags.append(v.toString());
                    }
                    if (Entry *e = entryByThread(threadId)) {
                        m_roster->setAgentTags(e->id, tags);
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
    // Tagging. add/remove apply optimistically and roll back on error (like
    // rename); the full editor sends the whole set via agent.setTags. Each
    // success also broadcasts agent.tagsChanged, which reconciles the roster to
    // the core's authoritatively normalized set.
    connect(m_roster, &AgentRoster::addTagRequested, this,
            [this](int id, const QString &tag) { mutateTag(id, tag, /*add=*/true); });
    connect(m_roster, &AgentRoster::removeTagRequested, this,
            [this](int id, const QString &tag) { mutateTag(id, tag, /*add=*/false); });
    connect(m_roster, &AgentRoster::editTagsRequested, this, &AgentDock::editTags);
    connect(m_roster, &AgentRoster::autoOrganizeRequested, this,
            &AgentDock::autoOrganize);
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
    // Always create a starter agent so the UI is never empty (and as a fallback
    // if nothing is restored). On a first open we may later jump focus to the
    // last-active restored session and prune this starter — see restoreThreads.
    addAgent(path);
    if (!wasOpen) {
        m_pendingFocusProjects.insert(path);
        m_initialAgentByProject.insert(path, m_agents.constLast().id);
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

int AgentDock::runningAgentCount() const
{
    int n = 0;
    for (const Entry &e : m_agents) {
        if (e.panel->isRunning()) {
            ++n;
        }
    }
    return n;
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

QString AgentDock::worktreePathForAgent(int agentId) const
{
    for (const Entry &e : m_agents) {
        if (e.id == agentId) {
            const QString tid = e.panel->threadId();
            if (tid.isEmpty()) {
                return {};
            }
            return m_worktreePathByThread.value(tid);
        }
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
                         AgentPanel *panel = addDormantAgent(
                             project, threadId,
                             rec.value(QStringLiteral("title")).toString(), isolated);
                         // Seed the roster card's tag chips from the persisted set.
                         QStringList tags;
                         const QJsonArray tagArr =
                             rec.value(QStringLiteral("tags")).toArray();
                         for (const QJsonValue &tv : tagArr) {
                             tags.append(tv.toString());
                         }
                         if (!tags.isEmpty()) {
                             if (Entry *e = entryByPanel(panel)) {
                                 m_roster->setAgentTags(e->id, tags);
                             }
                         }
                     }
                     refreshAgentNumbers();
                     // First restore for this project: land in the last-active
                     // session instead of the blank starter agent.
                     if (m_pendingFocusProjects.remove(project)) {
                         restoreInitialFocus(project);
                     }
                 });
}

void AgentDock::restoreInitialFocus(const QString &project)
{
    const int starterId = m_initialAgentByProject.take(project);
    const QString wantThread = lastActiveThread(project);
    if (wantThread.isEmpty()) {
        return; // nothing remembered — keep the fresh starter agent focused
    }
    Entry *target = entryByThread(wantThread);
    if (!target || target->id == starterId) {
        return; // remembered thread wasn't restored (removed?) — keep starter
    }
    // Focus the last-active agent in its dormant state. We deliberately do NOT
    // resume() it — the user decides when to spend the re-cache cost.
    m_roster->setCurrentAgent(target->id);
    // Prune the never-used starter so we land cleanly in the restored session
    // rather than leaving a stray empty "Agent N" beside it.
    if (Entry *starter = entryById(starterId)) {
        if (starter->panel->threadId().isEmpty()) {
            closeAgent(starterId);
        }
    }
}

void AgentDock::setLastActiveThread(const QString &project, const QString &threadId)
{
    // Never clear: activating a blank starter agent (empty thread) must not
    // erase the remembered real session for this project.
    if (project.isEmpty() || threadId.isEmpty()) {
        return;
    }
    KSharedConfig::openConfig()
        ->group(QStringLiteral("Agent"))
        .group(QStringLiteral("LastActive"))
        .group(project)
        .writeEntry(QStringLiteral("threadId"), threadId);
}

QString AgentDock::lastActiveThread(const QString &project) const
{
    return KSharedConfig::openConfig()
        ->group(QStringLiteral("Agent"))
        .group(QStringLiteral("LastActive"))
        .group(project)
        .readEntry(QStringLiteral("threadId"), QString());
}

void AgentDock::persistLastActiveSessions()
{
    // Capture the focused agent's thread at shutdown — covers a thread that
    // started while focused and so never fired a fresh activation write.
    if (auto *w = qobject_cast<AgentPanel *>(m_stack->currentWidget())) {
        if (Entry *e = entryByPanel(w)) {
            setLastActiveThread(e->project, e->panel->threadId());
        }
    }
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
                     m_worktreePathByThread.clear();
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
                         const QString p = o.value(QStringLiteral("path")).toString();
                         if (!id.isEmpty()) {
                             m_worktreePathByThread.insert(id, p);
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

AgentDock::Entry *AgentDock::entryByPanel(const AgentPanel *panel)
{
    for (Entry &e : m_agents) {
        if (e.panel == panel) {
            return &e;
        }
    }
    return nullptr;
}

AgentDock::Entry *AgentDock::entryByThread(const QString &threadId)
{
    for (Entry &e : m_agents) {
        if (e.panel->threadId() == threadId) {
            return &e;
        }
    }
    return nullptr;
}

// --- tagging ----------------------------------------------------------------

// mutateTag adds or removes one tag, applying the change to the roster
// immediately and rolling back if the core rejects it.
void AgentDock::mutateTag(int agentId, const QString &tag, bool add)
{
    Entry *e = entryById(agentId);
    if (!e || e->panel->threadId().isEmpty()) {
        emit statusMessage(i18n("Start the agent before tagging it"));
        return;
    }
    const QString threadId = e->panel->threadId();
    const QStringList before = m_roster->agentTags(agentId);

    // Optimistic local update so chips react instantly.
    QStringList next = before;
    if (add) {
        bool present = false;
        for (const QString &t : before) {
            if (t.compare(tag, Qt::CaseInsensitive) == 0) {
                present = true;
                break;
            }
        }
        if (!present) {
            next.append(tag);
        }
    } else {
        QStringList kept;
        for (const QString &t : next) {
            if (t.compare(tag, Qt::CaseInsensitive) != 0) {
                kept.append(t);
            }
        }
        next = kept;
    }
    m_roster->setAgentTags(agentId, next);

    const QString method = add ? QStringLiteral("agent.addTag")
                               : QStringLiteral("agent.removeTag");
    m_core->call(method,
                 QJsonObject{{QStringLiteral("threadId"), threadId},
                             {QStringLiteral("tag"), tag}},
                 [this, agentId, before](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_roster->setAgentTags(agentId, before); // roll back
                         emit statusMessage(i18n("Tag update failed: %1",
                             error.value(QStringLiteral("message")).toString()));
                     }
                     // On success the core's agent.tagsChanged converges us.
                 });
}

// editTags opens the full tag editor and, on accept, replaces the agent's tag
// set via agent.setTags.
void AgentDock::editTags(int agentId)
{
    Entry *e = entryById(agentId);
    if (!e || e->panel->threadId().isEmpty()) {
        emit statusMessage(i18n("Start the agent before tagging it"));
        return;
    }
    const QString threadId = e->panel->threadId();
    const QStringList current = m_roster->agentTags(agentId);

    // Suggestions: every tag in use across this agent's project.
    QStringList suggestions;
    QSet<QString> seen;
    for (const Entry &other : m_agents) {
        if (other.project != e->project) {
            continue;
        }
        for (const QString &t : m_roster->agentTags(other.id)) {
            if (!seen.contains(t.toLower())) {
                seen.insert(t.toLower());
                suggestions.append(t);
            }
        }
    }

    TagEditorDialog dlg(current, suggestions, m_dialogParent);
    if (dlg.exec() != QDialog::Accepted) {
        return;
    }
    const QStringList next = dlg.tags();
    const QStringList before = current;
    m_roster->setAgentTags(agentId, next); // optimistic

    QJsonArray arr;
    for (const QString &t : next) {
        arr.append(t);
    }
    m_core->call(QStringLiteral("agent.setTags"),
                 QJsonObject{{QStringLiteral("threadId"), threadId},
                             {QStringLiteral("tags"), arr}},
                 [this, agentId, before](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_roster->setAgentTags(agentId, before); // roll back
                         emit statusMessage(i18n("Tag update failed: %1",
                             error.value(QStringLiteral("message")).toString()));
                     }
                 });
}

// autoOrganize asks the core for Sonnet's proposed tags for the project, then
// shows a preview dialog. Nothing is applied until the user confirms; each
// confirmed row is written via agent.setTags (which broadcasts tagsChanged so
// the roster converges).
void AgentDock::autoOrganize(const QString &projectPath)
{
    if (!m_core->isConnected()) {
        emit statusMessage(i18n("The core is not connected"));
        return;
    }
    emit statusMessage(i18n("Asking Claude to organize agents…"));
    QPointer<AgentDock> self(this);
    m_core->call(QStringLiteral("agent.suggestTags"),
                 QJsonObject{{QStringLiteral("project"), projectPath}},
                 [self, projectPath](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         emit self->statusMessage(
                             i18n("Auto-organize failed: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->showOrganizeProposals(
                         projectPath,
                         result.value(QStringLiteral("proposals")).toArray());
                 });
}

void AgentDock::showOrganizeProposals(const QString &projectPath,
                                      const QJsonArray &proposals)
{
    // Map each proposed threadId to the agent's roster title for the preview.
    QVector<AutoOrganizeDialog::Proposal> rows;
    for (const QJsonValue &v : proposals) {
        const QJsonObject o = v.toObject();
        const QString threadId = o.value(QStringLiteral("threadId")).toString();
        if (threadId.isEmpty()) {
            continue;
        }
        Entry *e = entryByThread(threadId);
        if (!e || e->project != projectPath) {
            continue; // only this project's currently-shown agents
        }
        QStringList tags;
        const QJsonArray tagArr = o.value(QStringLiteral("tags")).toArray();
        for (const QJsonValue &tv : tagArr) {
            tags.append(tv.toString());
        }
        AutoOrganizeDialog::Proposal p;
        p.threadId = threadId;
        p.label = m_roster->agentTitle(e->id);
        if (p.label.isEmpty()) {
            p.label = i18n("Agent %1", e->id);
        }
        p.tags = tags;
        rows.append(p);
    }

    if (rows.isEmpty()) {
        emit statusMessage(i18n("Claude had no tag suggestions"));
        return;
    }

    AutoOrganizeDialog dlg(rows, m_dialogParent);
    if (dlg.exec() != QDialog::Accepted) {
        return;
    }
    const QVector<AutoOrganizeDialog::Result> results = dlg.results();
    int applied = 0;
    for (const AutoOrganizeDialog::Result &res : results) {
        Entry *e = entryByThread(res.threadId);
        if (!e) {
            continue;
        }
        m_roster->setAgentTags(e->id, res.tags); // optimistic
        QJsonArray arr;
        for (const QString &t : res.tags) {
            arr.append(t);
        }
        m_core->call(QStringLiteral("agent.setTags"),
                     QJsonObject{{QStringLiteral("threadId"), res.threadId},
                                 {QStringLiteral("tags"), arr}},
                     nullptr);
        ++applied;
    }
    emit statusMessage(i18n("Applied tags to %1 agent(s)", applied));
}
