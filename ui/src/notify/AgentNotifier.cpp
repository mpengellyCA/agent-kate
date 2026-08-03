#include "AgentNotifier.h"

#include "AgentCardDelegate.h" // AgentRoles::AgentStatus

#include <QCoreApplication>
#include <QWidget>
#include <QWindow>

#include <KLocalizedString>
#include <KNotification>
#include <KWindowSystem>

namespace agentkate {

namespace {
constexpr int kStatusIdle = int(AgentRoles::AgentStatus::Idle);
constexpr int kStatusWorking = int(AgentRoles::AgentStatus::Working);
constexpr int kStatusNeedsInput = int(AgentRoles::AgentStatus::NeedsInput);
constexpr int kStatusError = int(AgentRoles::AgentStatus::Error);
} // namespace

AgentNotifier::AgentNotifier(QWidget *window, QObject *parent)
    : QObject(parent)
    , m_window(window)
{
    // The window (and with it this notifier) is never deleted on the normal exit
    // path — main returns straight out of exec() — so aboutToQuit is the only
    // moment guaranteed to arrive before the process goes.
    if (qApp) {
        connect(qApp, &QCoreApplication::aboutToQuit, this,
                &AgentNotifier::closeAllAlerts);
    }
    m_finishTimer.setSingleShot(true);
    m_finishTimer.setInterval(kFinishWindowMs);
    connect(&m_finishTimer, &QTimer::timeout, this, &AgentNotifier::flushFinishBatch);
    m_failTimer.setSingleShot(true);
    m_failTimer.setInterval(kFailWindowMs);
    connect(&m_failTimer, &QTimer::timeout, this, &AgentNotifier::flushFailBatch);
}

AgentNotifier::~AgentNotifier()
{
    closeAllAlerts();
}

void AgentNotifier::closeAllAlerts()
{
    for (State &st : m_state) {
        clearAttention(st);
    }
}

void AgentNotifier::setAgentTitle(int agentId, const QString &title)
{
    if (m_forgotten.contains(agentId)) {
        return;
    }
    m_state[agentId].title = title;
}

void AgentNotifier::setVisibleAgent(int agentId)
{
    m_visibleAgent = agentId;
}

void AgentNotifier::forgetAgent(int agentId)
{
    auto it = m_state.find(agentId);
    if (it != m_state.end()) {
        clearAttention(*it);
        m_state.erase(it);
    }
    m_forgotten.insert(agentId);
    // A closed agent must not turn up in an aggregate that has not fired yet:
    // by the time the window shuts, "it finished" is no longer true of anything
    // the user can open.
    m_pendingFinish.removeAll(agentId);
    m_pendingFail.removeAll(agentId);
    if (m_visibleAgent == agentId) {
        m_visibleAgent = -1;
    }
}

QString AgentNotifier::displayName(int agentId) const
{
    const QString title = m_state.value(agentId).title;
    return title.isEmpty() ? i18n("Agent") : title;
}

bool AgentNotifier::windowIsActive() const
{
    return m_window && m_window->isActiveWindow();
}

bool AgentNotifier::shouldNotify(int agentId) const
{
    if (!m_window) {
        return true;
    }
    return !windowIsActive() || agentId != m_visibleAgent;
}

void AgentNotifier::clearAttention(State &st)
{
    st.attentionRaised = false;
    if (st.attention) {
        // Retracts the popup; setAutoDelete then disposes of the object.
        st.attention->close();
        st.attention.clear();
    }
}

void AgentNotifier::reportStatus(int agentId, int status)
{
    emitAlert(agentId, evaluateStatus(agentId, status));
}

void AgentNotifier::reportAttention(int agentId, bool attention)
{
    emitAlert(agentId, evaluateAttention(agentId, attention));
}

void AgentNotifier::reportQuestion(int agentId)
{
    emitAlert(agentId, evaluateQuestion(agentId));
}

AgentNotifier::Alert AgentNotifier::evaluateStatus(int agentId, int status)
{
    if (m_forgotten.contains(agentId)) {
        return Alert::None;
    }
    State &st = m_state[agentId];
    const int prev = st.status;
    st.status = status;
    if (prev == status) {
        return Alert::None;
    }

    if (status == kStatusNeedsInput) {
        return evaluateAttention(agentId, true);
    }

    // Any other state means whatever was parked is parked no longer: retract the
    // outstanding prompt popup, and let the next prompt speak again.
    clearAttention(st);

    if (status == kStatusError) {
        return shouldNotify(agentId) ? Alert::Failed : Alert::None;
    }
    // Only a turn that was actually computing can "finish" — a fresh agent
    // settling into Idle, or a dormant one being adopted, has finished nothing.
    if (status == kStatusIdle && prev == kStatusWorking) {
        return shouldNotify(agentId) ? Alert::Finished : Alert::None;
    }
    return Alert::None;
}

AgentNotifier::Alert AgentNotifier::evaluateAttention(int agentId, bool attention)
{
    if (m_forgotten.contains(agentId)) {
        return Alert::None;
    }
    State &st = m_state[agentId];
    if (!attention) {
        clearAttention(st);
        return Alert::None;
    }
    if (st.attentionRaised) {
        return Alert::None;
    }
    // Latched even when the popup is suppressed: the prompt IS outstanding, and
    // the second half of the status/attention pair must not announce it again.
    st.attentionRaised = true;
    return shouldNotify(agentId) ? Alert::NeedsAttention : Alert::None;
}

AgentNotifier::Alert AgentNotifier::evaluateQuestion(int agentId)
{
    if (m_forgotten.contains(agentId)) {
        return Alert::None;
    }
    State &st = m_state[agentId];
    if (st.attentionRaised) {
        return Alert::None;
    }
    // Use the ordinary attention latch.  A question still blocks the agent,
    // but it gets its own notifyrc event rather than a second generic prompt.
    st.attentionRaised = true;
    return shouldNotify(agentId) ? Alert::Question : Alert::None;
}

bool AgentNotifier::noteInWindow(QList<int> &pending, bool &open, int agentId)
{
    if (!open) {
        // Nothing of this kind happened recently: this one is news on its own,
        // and it opens the window that will swallow whatever follows it.
        open = true;
        return true;
    }
    // Deliberately deduplicated by id: ONE agent crash-looping inside a window
    // is one line in the aggregate, not twelve.
    if (!pending.contains(agentId)) {
        pending.append(agentId);
    }
    return false;
}

QList<int> AgentNotifier::takeWindowBatch(QList<int> &pending, bool &open)
{
    QList<int> batch;
    batch.swap(pending);
    // A window that closes on an empty pool means the burst is over — reopen
    // only when something actually pooled, so a fleet still finishing (or an
    // agent still crash-looping) keeps costing one popup per window instead of
    // one per event.
    open = !batch.isEmpty();
    return batch;
}

bool AgentNotifier::noteFinished(int agentId)
{
    return noteInWindow(m_pendingFinish, m_finishWindowOpen, agentId);
}

QList<int> AgentNotifier::takeFinishBatch()
{
    return takeWindowBatch(m_pendingFinish, m_finishWindowOpen);
}

bool AgentNotifier::noteFailed(int agentId)
{
    return noteInWindow(m_pendingFail, m_failWindowOpen, agentId);
}

QList<int> AgentNotifier::takeFailBatch()
{
    return takeWindowBatch(m_pendingFail, m_failWindowOpen);
}

void AgentNotifier::flushFinishBatch()
{
    const QList<int> batch = takeFinishBatch();
    if (batch.isEmpty()) {
        return;
    }
    QStringList names;
    names.reserve(batch.size());
    for (int agentId : batch) {
        names << displayName(agentId);
    }
    // The popup is about several agents, so clicking it can only sensibly open
    // one — the first that finished, which is the oldest news in the batch.
    notify(batch.first(), QStringLiteral("agentFinished"),
           i18np("%1 agent finished", "%1 agents finished", batch.size()),
           names.join(i18nc("list separator", ", ")), false);
    m_finishTimer.start();
}

void AgentNotifier::flushFailBatch()
{
    const QList<int> batch = takeFailBatch();
    if (batch.isEmpty()) {
        return;
    }
    QStringList names;
    names.reserve(batch.size());
    for (int agentId : batch) {
        names << displayName(agentId);
    }
    notify(batch.first(), QStringLiteral("agentFailed"),
           i18np("%1 agent failed", "%1 agents failed", batch.size()),
           names.join(i18nc("list separator", ", ")), false);
    m_failTimer.start();
}

void AgentNotifier::emitAlert(int agentId, Alert alert)
{
    switch (alert) {
    case Alert::None:
        return;
    case Alert::Finished:
        if (!noteFinished(agentId)) {
            return; // pooled; the window's flush will speak for it
        }
        m_finishTimer.start();
        notify(agentId, QStringLiteral("agentFinished"),
               i18n("%1 finished", displayName(agentId)),
               i18n("The agent stopped working and is waiting for your next "
                    "instruction."),
               false);
        return;
    case Alert::Failed:
        // Same rule as Finished (audit F24): the first failure speaks, the rest
        // of the burst — or the rest of a crash loop — pool into one aggregate.
        if (!noteFailed(agentId)) {
            return; // pooled; the window's flush will speak for it
        }
        m_failTimer.start();
        notify(agentId, QStringLiteral("agentFailed"),
               i18n("%1 failed", displayName(agentId)),
               i18n("The last start or turn ended with an error."), false);
        return;
    case Alert::NeedsAttention:
        // Kept so it can be retracted the moment the prompt is answered in the
        // window — a Persistent popup has no other way of ever going away.
        m_state[agentId].attention =
            notify(agentId, QStringLiteral("agentNeedsAttention"),
                   i18n("%1 needs your attention", displayName(agentId)),
                   i18n("The agent is waiting on you before it can continue."), true);
        return;
    case Alert::Question:
        // Same persistent lifetime as a permission prompt: close it once the
        // interaction resolves, never leave a stale question in the tray.
        m_state[agentId].attention =
            notify(agentId, QStringLiteral("agentAsksQuestion"),
                   i18n("%1 asks a question", displayName(agentId)),
                   i18n("The agent needs your answer before it can continue."), true);
        return;
    }
}

KNotification *AgentNotifier::notify(int agentId, const QString &eventId,
                                     const QString &title, const QString &body,
                                     bool persistent)
{
    auto *n = new KNotification(eventId,
                                persistent ? KNotification::Persistent
                                           : KNotification::CloseOnTimeout);
    // The component name is what KNotification uses to find agentkate.notifyrc;
    // it is the file's basename, not the application's display name.
    n->setComponentName(QStringLiteral("agentkate"));
    n->setTitle(title);
    n->setText(body);
    n->setIconName(QStringLiteral("agentkate"));
    n->setAutoDelete(true);
    // Naming the window we belong to is what lets the notification server hand
    // back an XDG activation token strong enough for KWin to honour: an app
    // asking to be raised out of nowhere is a focus steal, an app raising the
    // window whose alert the user just clicked is not.
    if (m_window && m_window->window()->windowHandle()) {
        n->setWindow(m_window->window()->windowHandle());
    }

    // Clicking the popup should land the user in the agent it is about. `this`
    // is the connection context, so a notification outliving the notifier can
    // never call back into freed memory.
    KNotificationAction *open = n->addDefaultAction(i18n("Open"));
    connect(open, &KNotificationAction::activated, this,
            [this, agentId, n = QPointer<KNotification>(n)] {
                // The token rides on the notification the user just clicked and
                // is what the compositor checks when the raise path calls
                // activateWindow. Set it as the process-wide current token
                // first: KWindowSystem consumes it on the next activation, so it
                // cannot leak into an unrelated one later.
                if (n) {
                    const QString token = n->xdgActivationToken();
                    if (!token.isEmpty()) {
                        KWindowSystem::setCurrentXdgActivationToken(token);
                    }
                }
                Q_EMIT agentActivationRequested(agentId);
            });

    n->sendEvent();
    return n;
}

} // namespace agentkate
