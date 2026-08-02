// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "state/RateLimitState.h"

#include <KLocalizedString>
#include <KNotification>

#include <QApplication>
#include <QLocale>
#include <QSet>

namespace agentkate {
namespace {
bool s_desktopAlerts = true;

// Slack on the expiry wake. The reset time comes from the engine and both
// clocks drift; firing a hair late costs nothing (the claim is already about to
// be dropped) while firing early would re-arm in a loop.
constexpr int kExpirySlackMs = 250;
} // namespace

RateLimitState::RateLimitState(QObject *parent)
    : QObject(parent)
    , m_expiry(new QTimer(this))
{
    m_expiry->setSingleShot(true);
    connect(m_expiry, &QTimer::timeout, this, [this] {
        // The soonest window has rolled over. Nothing else was going to say so
        // — a parked agent emits no further events — so this is the only thing
        // that stops the strip claiming a limit that has cleared.
        announce(); // re-arms the once-per-episode latch when nothing is left
        rearmExpiry();
        Q_EMIT changed();
    });
}

RateLimitState *RateLimitState::self()
{
    static RateLimitState *instance = new RateLimitState;
    return instance;
}

void RateLimitState::setDesktopAlertsEnabled(bool on)
{
    s_desktopAlerts = on;
}

void RateLimitState::report(const QString &threadId, const RateLimitReport &r)
{
    if (threadId.isEmpty()) {
        return;
    }
    // The CLI emits one of these every turn. Only a genuine change is news —
    // otherwise the roster strip would re-layout on every event of every agent.
    const auto it = m_byThread.constFind(threadId);
    if (it != m_byThread.constEnd() && *it == r) {
        return;
    }
    m_byThread.insert(threadId, r);
    announce();
    rearmExpiry();
    Q_EMIT changed();
}

void RateLimitState::noteWake(const QString &threadId, const QDateTime &at)
{
    if (threadId.isEmpty()) {
        return;
    }
    if (!at.isValid()) {
        clearWake(threadId);
        return;
    }
    const auto it = m_wakes.constFind(threadId);
    if (it != m_wakes.constEnd() && *it == at) {
        return; // the core re-states an armed wake on every report it folds in
    }
    m_wakes.insert(threadId, at);
    announce();
    rearmExpiry();
    Q_EMIT changed();
}

void RateLimitState::clearWake(const QString &threadId)
{
    if (m_wakes.remove(threadId) == 0) {
        return;
    }
    if (!limited()) {
        m_announced = false;
    }
    rearmExpiry();
    Q_EMIT changed();
}

QDateTime RateLimitState::resumesAt() const
{
    const QDateTime now = QDateTime::currentDateTime();
    QDateTime soonest;
    for (const QDateTime &at : m_wakes) {
        if (!at.isValid() || at <= now) {
            continue; // a moment that has passed promises nothing
        }
        if (!soonest.isValid() || at < soonest) {
            soonest = at;
        }
    }
    return soonest;
}

bool RateLimitState::resumeArmed(const QString &threadId) const
{
    const auto it = m_wakes.constFind(threadId);
    return it != m_wakes.constEnd() && it->isValid()
        && *it > QDateTime::currentDateTime();
}

QString RateLimitState::resumeCaveat() const
{
    if (!resumesAt().isValid()) {
        return {};
    }
    return i18nc("@info:tooltip the condition under which an automatic resume "
                 "actually happens",
                 "Agent Kate has to stay open until then — a scheduled resume "
                 "cannot start it.");
}

void RateLimitState::forget(const QString &threadId)
{
    const bool hadWake = m_wakes.remove(threadId) > 0;
    if (m_byThread.remove(threadId) == 0 && !hadWake) {
        return;
    }
    // Leaving the limited state re-arms the latch, so the NEXT episode is
    // announced — including the case where the last limited agent was simply
    // closed rather than reset.
    if (!limited()) {
        m_announced = false;
    }
    rearmExpiry();
    Q_EMIT changed();
}

void RateLimitState::forgetReport(const QString &threadId)
{
    if (m_byThread.remove(threadId) == 0) {
        return;
    }
    if (!limited()) {
        m_announced = false;
    }
    rearmExpiry();
    Q_EMIT changed();
}

int RateLimitState::limitedCount() const
{
    const QDateTime now = QDateTime::currentDateTime();
    // A thread can be parked on the evidence of either half — an engine that
    // exits when the window is exhausted leaves only its armed wake behind —
    // so count the UNION, never the sum: one agent is one agent.
    QSet<QString> parked;
    for (auto it = m_byThread.constBegin(); it != m_byThread.constEnd(); ++it) {
        if (it.value().liveAt(now)) {
            parked.insert(it.key());
        }
    }
    for (auto it = m_wakes.constBegin(); it != m_wakes.constEnd(); ++it) {
        if (it.value().isValid() && it.value() > now) {
            parked.insert(it.key());
        }
    }
    return parked.size();
}

QDateTime RateLimitState::resetsAt() const
{
    const QDateTime now = QDateTime::currentDateTime();
    QDateTime soonest;
    for (const RateLimitReport &r : m_byThread) {
        if (!r.liveAt(now) || !r.resetsAt.isValid()) {
            continue;
        }
        if (!soonest.isValid() || r.resetsAt < soonest) {
            soonest = r.resetsAt;
        }
    }
    return soonest;
}

void RateLimitState::rearmExpiry()
{
    if (!m_expiry) {
        return;
    }
    // Whichever comes first: the window rolling over, or the armed resume
    // arriving. Both are moments when this state stops being true and nothing
    // else is going to say so — a parked agent emits no further events.
    QDateTime when = resetsAt();
    const QDateTime resume = resumesAt();
    if (resume.isValid() && (!when.isValid() || resume < when)) {
        when = resume;
    }
    if (!when.isValid()) {
        m_expiry->stop(); // nothing live with a known reset time
        return;
    }
    const qint64 ms = QDateTime::currentDateTime().msecsTo(when);
    m_expiry->start(int(qBound<qint64>(0, ms, 24LL * 60 * 60 * 1000))
                    + kExpirySlackMs);
}

QString RateLimitState::summary() const
{
    const int n = limitedCount();
    if (n == 0) {
        return {};
    }
    // "Resuming at" is a promise, so it is said ONLY when the core has actually
    // armed the resume. Without a wake the honest line is the weaker one: this
    // is when the window reopens, and somebody will still have to press
    // Resume. The difference between the two is the whole feature.
    const QDateTime resume = resumesAt();
    if (resume.isValid()) {
        return i18ncp("roster strip: how many agents are parked, and when they "
                      "will resume themselves",
                      "%1 agent paused by a usage limit — resuming at %2",
                      "%1 agents paused by a usage limit — resuming at %2", n,
                      QLocale().toString(resume.toLocalTime().time(), QLocale::ShortFormat));
    }
    const QDateTime when = resetsAt();
    if (!when.isValid()) {
        // No usable reset time: say what is true and stop, rather than invent
        // one. The panel's own chip has the window name if the event carried it.
        return i18np("%1 agent is paused by a usage limit",
                     "%1 agents are paused by a usage limit", n);
    }
    return i18ncp("roster strip: how many agents are parked, and until when",
                  "%1 agent paused by a usage limit — resets at %2",
                  "%1 agents paused by a usage limit — resets at %2", n,
                  QLocale().toString(when.toLocalTime().time(), QLocale::ShortFormat));
}

QString RateLimitState::takeAnnouncement()
{
    if (!limited()) {
        m_announced = false; // back under the limit: the next episode speaks
        return {};
    }
    if (m_announced) {
        return {}; // one notice per episode, not one per turn
    }
    m_announced = true;
    return summary();
}

void RateLimitState::announce()
{
    const QString notice = takeAnnouncement();
    if (notice.isEmpty()) {
        return;
    }
    // Subscribers first: the episode has begun whether or not this build (or
    // this moment) can put a popup on screen.
    Q_EMIT announced(notice);
    if (!s_desktopAlerts) {
        return;
    }
    // An active Agent Kate window is already showing the roster strip that says
    // this — the same suppression rule AgentNotifier uses, for the same reason:
    // a popup for something on screen is an interruption, not news.
    if (qApp && qApp->activeWindow()) {
        return;
    }
    auto *n = new KNotification(QStringLiteral("agentRateLimited"),
                                KNotification::CloseOnTimeout);
    n->setComponentName(QStringLiteral("agentkate"));
    n->setTitle(i18n("Agents paused by a usage limit"));
    n->setText(notice);
    n->setIconName(QStringLiteral("agentkate"));
    n->setAutoDelete(true);
    n->sendEvent();
}

} // namespace agentkate
