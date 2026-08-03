// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TrayPresence.h"

#include "AgentCardDelegate.h" // AgentRoles::AgentStatus

#include <KLocalizedString>
#include <KStatusNotifierItem>

#include <QDBusConnection>
#include <QDBusInterface>
#include <QMenu>
#include <QVariant>

namespace agentkate {

namespace {
constexpr int kStatusWorking = int(AgentRoles::AgentStatus::Working);
constexpr int kStatusNeedsInput = int(AgentRoles::AgentStatus::NeedsInput);
} // namespace

TrayPresence::TrayPresence(QObject *parent)
    : QObject(parent)
{
}

TrayPresence::Status TrayPresence::evaluateStatus(int running, int attention)
{
    if (attention > 0) {
        return Status::NeedsAttention;
    }
    return running > 0 ? Status::Active : Status::Passive;
}

QString TrayPresence::tooltipText(int running, int attention)
{
    if (running <= 0 && attention <= 0) {
        return i18n("No agents running");
    }
    QStringList parts;
    if (running > 0) {
        parts << i18np("%1 running", "%1 running", running);
    }
    if (attention > 0) {
        parts << i18np("%1 waiting on you", "%1 waiting on you", attention);
    }
    return parts.join(i18nc("tooltip part separator", " · "));
}

bool TrayPresence::shouldHideToTray(bool preferenceOn, bool trayActive,
                                    bool quitRequested, bool sessionSaving)
{
    return preferenceOn && trayActive && !quitRequested && !sessionSaving;
}

bool TrayPresence::shouldExplainNoHost(bool preferenceOn, bool hostAvailable,
                                       bool alreadyExplained)
{
    return preferenceOn && !hostAvailable && !alreadyExplained;
}

void TrayPresence::setAgentTitle(int agentId, const QString &title)
{
    if (m_forgotten.contains(agentId)) {
        return;
    }
    m_agents[agentId].title = title;
    // Counts are unchanged but the submenu names come from here.
    rebuildAgentsMenu();
    Q_EMIT presenceChanged();
}

void TrayPresence::reportStatus(int agentId, int status)
{
    if (m_forgotten.contains(agentId)) {
        return;
    }
    AgentState &st = m_agents[agentId];
    st.status = status;
    // AgentNotifier's fold, verbatim: NeedsInput raises the attention latch,
    // any other status means whatever was parked is parked no longer. Copying
    // the rule (rather than inventing a second one) is what keeps the tray,
    // the popup and the roster card telling one story.
    if (status == kStatusNeedsInput) {
        st.attention = true;
    } else {
        st.attention = false;
    }
    refresh();
}

void TrayPresence::reportAttention(int agentId, bool attention)
{
    if (m_forgotten.contains(agentId)) {
        return;
    }
    m_agents[agentId].attention = attention;
    refresh();
}

void TrayPresence::forgetAgent(int agentId)
{
    m_agents.remove(agentId);
    // Ids are never reused, so a torn-down panel's late signals are dropped by
    // id instead of silently re-creating state (same rule as AgentNotifier).
    m_forgotten.insert(agentId);
    refresh();
}

bool TrayPresence::isRunning(const AgentState &st) const
{
    // Working only: a NeedsInput / RateLimited agent is precisely NOT running,
    // and counting it would let the tooltip contradict the roster.
    return st.status == kStatusWorking;
}

int TrayPresence::runningCount() const
{
    int n = 0;
    for (const AgentState &st : m_agents) {
        if (isRunning(st)) {
            ++n;
        }
    }
    return n;
}

int TrayPresence::attentionCount() const
{
    int n = 0;
    for (const AgentState &st : m_agents) {
        if (st.attention) {
            ++n;
        }
    }
    return n;
}

int TrayPresence::firstAttentionAgent() const
{
    for (auto it = m_agents.constBegin(); it != m_agents.constEnd(); ++it) {
        if (it->attention) {
            return it.key();
        }
    }
    return -1;
}

bool TrayPresence::hostAvailable()
{
    QDBusConnection bus = QDBusConnection::sessionBus();
    if (!bus.isConnected()) {
        return false;
    }
    QDBusInterface watcher(QStringLiteral("org.kde.StatusNotifierWatcher"),
                           QStringLiteral("/StatusNotifierWatcher"),
                           QStringLiteral("org.kde.StatusNotifierWatcher"), bus);
    if (!watcher.isValid()) {
        return false;
    }
    return watcher.property("IsStatusNotifierHostRegistered").toBool();
}

void TrayPresence::embed(QWindow *window, QMenu *contextMenu)
{
    if (m_sni || !contextMenu) {
        return;
    }
    m_sni = new KStatusNotifierItem(QStringLiteral("org.kde.agentkate"), this);
    m_sni->setCategory(KStatusNotifierItem::ApplicationStatus);
    m_sni->setTitle(i18n("Agent Kate"));
    m_sni->setIconByName(QStringLiteral("agentkate"));
    m_sni->setAttentionIconByName(QStringLiteral("agentkate"));
    // Left click shows/hides the window through KStatusNotifierItem's own
    // path, which carries the compositor-blessed activation token — the same
    // reason the KDBusService raise path is reused rather than reimplemented.
    m_sni->setAssociatedWindow(window);
    // The standard entries are replaced by the caller's collection-built menu;
    // in particular the built-in Quit would sidestep the ShutdownDialog. The
    // quitRequested interception below keeps even a host-invoked quit genuine.
    m_sni->setStandardActionsEnabled(false);
    m_agentsMenu = new QMenu(i18n("Agents"), contextMenu);
    m_agentsMenu->setIcon(QIcon::fromTheme(QStringLiteral("user-group-properties")));
    contextMenu->insertMenu(contextMenu->actions().isEmpty()
                                ? nullptr
                                : contextMenu->actions().constFirst(),
                            m_agentsMenu);
    m_sni->setContextMenu(contextMenu);
    connect(m_sni, &KStatusNotifierItem::quitRequested, this, [this] {
        // Never let the item quit the app itself: route it as a genuine quit
        // request so the stop-and-compact shutdown still runs.
        m_sni->abortQuit();
        Q_EMIT quitRequested();
    });
    rebuildAgentsMenu();
    refresh();
}

void TrayPresence::refresh()
{
    rebuildAgentsMenu();
    Q_EMIT presenceChanged();
    if (!m_sni) {
        return;
    }
    const int running = runningCount();
    const int attention = attentionCount();
    KStatusNotifierItem::ItemStatus item = KStatusNotifierItem::Passive;
    switch (evaluateStatus(running, attention)) {
    case Status::Passive:
        item = KStatusNotifierItem::Passive;
        break;
    case Status::Active:
        item = KStatusNotifierItem::Active;
        break;
    case Status::NeedsAttention:
        item = KStatusNotifierItem::NeedsAttention;
        break;
    }
    m_sni->setStatus(item);
    m_sni->setToolTip(QStringLiteral("agentkate"), i18n("Agent Kate"),
                      tooltipText(running, attention));
}

void TrayPresence::rebuildAgentsMenu()
{
    if (!m_agentsMenu) {
        return;
    }
    m_agentsMenu->clear();
    if (m_agents.isEmpty()) {
        QAction *none = m_agentsMenu->addAction(i18n("No agents"));
        none->setEnabled(false);
        return;
    }
    for (auto it = m_agents.constBegin(); it != m_agents.constEnd(); ++it) {
        const int agentId = it.key();
        const AgentState &st = it.value();
        QString label = st.title.isEmpty() ? i18n("Agent") : st.title;
        if (st.attention) {
            label = i18nc("agent name, blocked on the user",
                          "%1 — waiting on you", label);
        } else if (isRunning(st)) {
            label = i18nc("agent name, currently computing", "%1 — working", label);
        }
        QAction *act = m_agentsMenu->addAction(label);
        if (st.attention) {
            act->setIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
        }
        connect(act, &QAction::triggered, this,
                [this, agentId] { Q_EMIT agentActivationRequested(agentId); });
    }
}

} // namespace agentkate
