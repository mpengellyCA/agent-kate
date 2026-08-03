#pragma once

#include <QHash>
#include <QList>
#include <QObject>
#include <QPointer>
#include <QSet>
#include <QString>
#include <QTimer>

class KNotification;
class QWidget;

namespace agentkate {

// AgentNotifier turns agent state transitions into KDE desktop notifications.
//
// It is fed the same three signals the roster card is fed (title, status,
// attention), so it can never claim a state the card does not show. Every alert
// is a [Event/…] section of ui/agentkate.notifyrc, which is what lets a user
// silence one kind of alert — or all of them — from System Settings ▸
// Notifications instead of from a setting this class would otherwise invent.
//
// Suppression rule: an agent whose panel is on screen in an ACTIVE window needs
// no popup — the transition it would announce is already visible. Anything else
// (window inactive, or a different agent selected) is background work, and
// background work finishing unseen is the whole reason this exists.
//
// Deciding and emitting are separate steps: evaluateStatus/evaluateAttention
// fold a report into the per-agent state and return what it earns, and only
// emitAlert talks to KNotification. The split is what makes the policy testable
// without a notification server.
class AgentNotifier : public QObject
{
    Q_OBJECT
public:
    explicit AgentNotifier(QWidget *window, QObject *parent = nullptr);
    ~AgentNotifier() override;

    // What a report should announce, once suppression and dedup have had their
    // say. Alert::None means "nothing to show".
    enum class Alert { None, Finished, Failed, NeedsAttention, Question };

    void setAgentTitle(int agentId, const QString &title);
    // status is an AgentRoles::AgentStatus value as int (AgentCardDelegate.h) —
    // the same int AgentPanel::statusChanged carries.
    void reportStatus(int agentId, int status);
    void reportAttention(int agentId, bool attention);
    // A question is attention with a more useful notification category.  It
    // shares the same latch as ordinary permissions so one prompt cannot earn
    // both alerts when status and attention signals arrive afterwards.
    void reportQuestion(int agentId);
    // The agent whose panel is currently on top of the stack; -1 for none.
    void setVisibleAgent(int agentId);
    void forgetAgent(int agentId);
    // Retract every outstanding persistent popup. A Persistent notification is
    // parentless and only auto-deletes once it closes, so nothing else ever
    // takes it down: without this the "needs your attention" banners of a quit
    // app sit in the notification centre forever, and clicking one calls into a
    // process that no longer exists. Wired to QCoreApplication::aboutToQuit and
    // run again from the destructor.
    void closeAllAlerts();

    // The decision half of report*(), exposed for tests. Both mutate the agent's
    // remembered state exactly as the report path does.
    Alert evaluateStatus(int agentId, int status);
    Alert evaluateAttention(int agentId, bool attention);
    Alert evaluateQuestion(int agentId);

    // --- finish aggregation ------------------------------------------------
    // A fleet running a multi-turn workflow earns one Finished alert per agent
    // per turn, which as one popup each is unusable. So the FIRST finish speaks
    // immediately — a lone agent behaves exactly as it always has — and opens a
    // window; every finish landing inside that window is pooled and announced
    // once, as "N agents finished", when it closes.
    //
    // Both halves are exposed (and driven) here rather than from a timer
    // callback so the rule is testable without a notification server, exactly
    // like evaluate*.
    //
    // noteFinished folds one finish into the window and returns true when it
    // should be announced on its own (the window was closed and has now
    // opened), false when it was pooled.
    bool noteFinished(int agentId);
    // takeFinishBatch drains the pooled finishes at window close. A non-empty
    // result is what the aggregate popup covers, and re-opens the window so a
    // still-running fleet keeps costing one popup per window. An empty result
    // closes it: the burst is over and the next finish speaks immediately.
    QList<int> takeFinishBatch();
    // The pooling window. Long enough to swallow a fleet finishing a turn
    // together, short enough that the aggregate is still news.
    static constexpr int kFinishWindowMs = 10000;

    // --- failure aggregation -----------------------------------------------
    // Failures got none of the above, so a crash-looping agent — start, error,
    // restart, error — earned one popup per crash, unbounded (audit F24). They
    // are batched on exactly the same rule, with their own window: an agent
    // failing twice inside one window is one line in the aggregate, so a loop
    // costs one popup per window instead of one per crash.
    //
    // A separate window from the finish one on purpose: the two are different
    // notifyrc events, so a user who silenced one still hears the other, and a
    // fleet finishing must not delay news that something broke.
    bool noteFailed(int agentId);
    QList<int> takeFailBatch();
    static constexpr int kFailWindowMs = 10000;

Q_SIGNALS:
    // The user clicked the popup: raise the window and select this agent.
    void agentActivationRequested(int agentId);

protected:
    // Whether our window currently has the focus. Virtual because window
    // activation is a compositor decision no headless test can stage, and the
    // suppression rule it feeds is the part worth testing.
    virtual bool windowIsActive() const;

private:
    struct State {
        QString title;
        int status = -1;
        // Whether a needs-attention alert is already outstanding for this agent.
        // Status and the attention flag can both announce the same prompt, and
        // they do not arrive in a fixed order; without this the user gets two
        // popups for one decision.
        bool attentionRaised = false;
        // The live persistent popup for that prompt. Persistent notifications
        // never expire on their own, so the one thing that can retract them is
        // us, once the prompt is answered (or the agent is gone).
        QPointer<KNotification> attention;
    };

    bool shouldNotify(int agentId) const;
    void clearAttention(State &st);
    void emitAlert(int agentId, Alert alert);
    // The batching rule itself, shared by finishes and failures so the two can
    // never drift apart. `pending`/`open` are the window's whole state.
    static bool noteInWindow(QList<int> &pending, bool &open, int agentId);
    static QList<int> takeWindowBatch(QList<int> &pending, bool &open);
    // Window close: emit the aggregate for whatever pooled up, or let the
    // window lapse when nothing did.
    void flushFinishBatch();
    void flushFailBatch();
    KNotification *notify(int agentId, const QString &eventId, const QString &title,
                          const QString &body, bool persistent);
    QString displayName(int agentId) const;

    QPointer<QWidget> m_window;
    QHash<int, State> m_state;
    // Agent ids are monotonic and never reused, so a closed agent's late signals
    // (a panel torn down asynchronously still emits) can be dropped by id rather
    // than silently re-creating state for something that no longer exists.
    QSet<int> m_forgotten;
    int m_visibleAgent = -1;
    // Finishes seen since the window opened, in arrival order and without
    // repeats (one agent finishing twice inside a window is still one line in
    // the aggregate). Empty while the window is open but nothing has piled up.
    QList<int> m_pendingFinish;
    bool m_finishWindowOpen = false;
    QTimer m_finishTimer;
    // The same three, for failures.
    QList<int> m_pendingFail;
    bool m_failWindowOpen = false;
    QTimer m_failTimer;
};

} // namespace agentkate
