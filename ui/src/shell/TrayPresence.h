// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QMap>
#include <QObject>
#include <QSet>
#include <QString>

class KStatusNotifierItem;
class QMenu;
class QWindow;

namespace agentkate {

// TrayPresence is the application's Plasma system-tray face (plan 27 §2): a
// KStatusNotifierItem that is Passive while nothing runs, Active while agents
// work, and NeedsAttention (Plasma pulses it) while any agent is blocked on
// the human.
//
// It computes NOTHING of its own. It is fed the very same per-agent signals
// AgentNotifier is fed — AgentPanel::statusChanged / attentionChanged,
// forwarded per agent by AgentDock — and folds them with AgentNotifier's own
// rules (status NeedsInput raises attention, any other status change clears
// it), so the tray can never claim a state the roster card does not show.
// The deciding and the D-Bus talking are separate layers for the same reason
// AgentNotifier splits evaluate*/emitAlert: the policy is testable without a
// StatusNotifier host (TrayPresenceTest drives only the decision half).
//
// The KStatusNotifierItem itself is only created by embed(), which the window
// calls when a StatusNotifier host is actually present. Without a host the
// decision layer still runs (the "answer pending attention" shortcut reads
// firstAttentionAgent() regardless), but nothing tray-shaped exists — that is
// what keeps a bare-WM session out of the unquittable-app trap, together with
// shouldHideToTray() below.
class TrayPresence : public QObject
{
    Q_OBJECT
public:
    explicit TrayPresence(QObject *parent = nullptr);

    // What the tray icon claims. Values mirror KStatusNotifierItem::ItemStatus
    // without needing its header in the decision layer.
    enum class Status { Passive, Active, NeedsAttention };

    // --- the decision layer (pure, testable without D-Bus) -----------------
    // Icon status from the two counts. Attention outranks running: a blocked
    // agent is the one thing the user must come back for. Passive when idle is
    // a recorded decision (plan 27 open question 4): the tray stays clean, the
    // taskbar entry is the findable surface while the window is up.
    static Status evaluateStatus(int running, int attention);
    // "3 running · 1 waiting on you" — the tooltip subtitle for those counts.
    static QString tooltipText(int running, int attention);
    // Whether closing the window should hide to the tray instead of quitting.
    // Every clause is a trap this feature would otherwise spring:
    //   * preference off       → today's behaviour, closing quits (default OFF —
    //                            changing what the close button does without
    //                            asking is hostile);
    //   * no live tray item    → hiding would strand the app with no way back
    //                            (the unquittable-app trap on a bare WM);
    //   * quit requested       → File ▸ Quit / tray Quit mean QUIT, always;
    //   * session logout       → the session manager is ending us, hiding would
    //                            silently lose the shutdown compaction.
    static bool shouldHideToTray(bool preferenceOn, bool trayActive,
                                 bool quitRequested, bool sessionSaving);
    // Whether enabling close-to-tray in a host-less session earns the one-time
    // KMessageWidget explaining that the close button will keep quitting.
    static bool shouldExplainNoHost(bool preferenceOn, bool hostAvailable,
                                    bool alreadyExplained);

    // --- the input fold (same wires AgentNotifier consumes) ----------------
    void setAgentTitle(int agentId, const QString &title);
    // status is an AgentRoles::AgentStatus value as int (AgentCardDelegate.h) —
    // the same int AgentPanel::statusChanged carries.
    void reportStatus(int agentId, int status);
    void reportAttention(int agentId, bool attention);
    void forgetAgent(int agentId);

    int runningCount() const;
    int attentionCount() const;
    Status status() const { return evaluateStatus(runningCount(), attentionCount()); }
    // The blocked agent that has been waiting longest (lowest id — ids are
    // monotonic), or -1. Target of "Answer pending attention" (plan 27 §3).
    int firstAttentionAgent() const;

    // --- the StatusNotifierItem half ----------------------------------------
    // Is a StatusNotifierWatcher with a registered host on the session bus?
    // Asked once before embed(): KStatusNotifierItem itself would fall back to
    // a legacy XEmbed icon, which is exactly the surface Plasma no longer shows.
    static bool hostAvailable();
    // Create the tray item: window association (left-click show/hide with the
    // compositor-blessed activation path) and the context menu the caller built
    // from its KActionCollection. TrayPresence inserts its own per-agent
    // submenu at the front and keeps it current. Call at most once.
    void embed(QWindow *window, QMenu *contextMenu);
    // True once embed() ran — i.e. the tray genuinely exists.
    bool active() const { return m_sni != nullptr; }

Q_SIGNALS:
    // The counts changed (also fired on title changes so menus stay named).
    void presenceChanged();
    // A per-agent tray menu entry: raise the window and select this agent.
    void agentActivationRequested(int agentId);
    // The tray's Quit — a GENUINE quit (ShutdownDialog and all), never a hide.
    void quitRequested();

private:
    struct AgentState {
        QString title;
        int status = -1;
        bool attention = false;
    };

    bool isRunning(const AgentState &st) const;
    void refresh();          // push status + tooltip to the item
    void rebuildAgentsMenu();

    // Keyed container ordered by id: agent ids are monotonic, so iteration
    // order is arrival order — what both the submenu and "first blocked" want.
    QMap<int, AgentState> m_agents;
    QSet<int> m_forgotten;
    KStatusNotifierItem *m_sni = nullptr;
    QMenu *m_agentsMenu = nullptr;
};

} // namespace agentkate
