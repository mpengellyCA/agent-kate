// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDateTime>
#include <QHash>
#include <QObject>
#include <QString>
#include <QTimer>

// RateLimitState — the app's one view of "are we out of quota, and until when?"
//
// Every engine emits a `rate_limit_event` on every turn. AgentPanel already
// folds it into its own header chip and notes the transitions in its feed, and
// that half is good. What it could not do is make the fact visible from OUTSIDE
// that one panel: with five agents running, a rate-limited one kept showing the
// green "Working" arc in the roster while it was parked until quota reset, and
// the only way to find out was to open it (audit F43).
//
// So the reports are hoisted here, out of the widget that happened to receive
// them:
//
//   * It is SHARED. Plan 28 §Phase 2 (rate-window auto-resume) is driven by the
//     same `resetsAt` this surfaces, and it needs it as DATA — a QDateTime it
//     can arm a wake timer on — not a pre-formatted string inside a QLabel.
//     resetsAt() is that seam; nothing here formats anything the scheduler
//     would have to parse back.
//
//   * It is ACCOUNT-LEVEL, not per-thread. A usage window belongs to the
//     account, not to one agent (plan 28 §Phase 2 says so explicitly, and it is
//     why the panel's own chip is deliberately not folded into the per-agent
//     roster subtitle: N agents on one exhausted window would read as N
//     separate problems). Reports are still keyed by thread so a closed agent
//     can be forgotten and so the count is honest, but the QUESTION this
//     answers is "is the fleet parked", singular.
//
//   * The announcement is COALESCED. One notice per entry into a limited state,
//     not one per event — the CLI emits a rate_limit_event every single turn.
//     takeAnnouncement() is the decision half, split from any emission exactly
//     as AgentNotifier splits evaluate* from emitAlert, so the rule is testable
//     without a notification server.
namespace agentkate {

// One thread's latest rate_limit_event, parsed. `resetsAt` is invalid when the
// event carried no usable timestamp — show nothing rather than a wrong time.
struct RateLimitReport {
    QString status;        // "allowed" / "allowed_warning" / "rejected" / …
    QString rateLimitType; // the window the limit covers, e.g. "five_hour"
    QDateTime resetsAt;
    bool overage = false; // billing past the included quota

    // The only distinction that matters to every consumer: anything that is not
    // a plain "allowed" is worth pulling the eye. An empty status is "no report
    // yet", not a limit.
    bool limited() const { return !status.isEmpty() && status != QLatin1String("allowed"); }

    // Has the window this report describes already rolled over?
    //
    // The engine emits a rate_limit_event per TURN, so a parked agent — the
    // only kind this state is about — sends nothing more until it is unparked.
    // Without this the strip went on saying "3 agents paused by a usage limit —
    // resets at 15:04" at 17:00, which is worse than saying nothing: a status
    // that survives its own condition teaches the user to disbelieve it. A
    // report with no usable reset time is never expired — we do not know when
    // it clears, so we keep showing what we were told.
    bool expiredAt(const QDateTime &now) const
    {
        return resetsAt.isValid() && resetsAt <= now;
    }
    bool liveAt(const QDateTime &now) const { return limited() && !expiredAt(now); }

    bool operator==(const RateLimitReport &o) const
    {
        return status == o.status && rateLimitType == o.rateLimitType
            && resetsAt == o.resetsAt && overage == o.overage;
    }
};

class RateLimitState : public QObject
{
    Q_OBJECT
public:
    // Process-wide. Not a widget and owns no widget: the roster subscribes, the
    // panels report, and plan 28's scheduler will read it without either.
    static RateLimitState *self();

    // Fold one thread's latest report in. A report equal to the one already
    // held is silent — the CLI repeats itself every turn and a redundant
    // changed() would repaint the roster on every single event.
    void report(const QString &threadId, const RateLimitReport &r);
    // The agent is gone (closed, archived, rebound): drop everything it claimed
    // so a dead thread cannot hold the fleet notice up forever.
    void forget(const QString &threadId);
    // Narrower: the thread's PROCESS ended, but the thread has not. Its last
    // report is stale (nothing is mid-turn any more) while an armed automatic
    // resume is not — an engine that exits when the window is exhausted is the
    // very case the resume exists for, so dropping the schedule here would
    // erase the one piece of state saying this agent is waiting rather than
    // finished. The core is authoritative about the schedule and cancels it
    // itself when the human stops the agent.
    void forgetReport(const QString &threadId);

    // An automatic resume the CORE has armed for this thread, at `at` (plan 28
    // §Phase 2 — the `_ratewake` event). This is the ONLY thing that licenses
    // the words "resumes at" anywhere in the UI: the core arms the wake, the
    // core fires it, and if it never armed one we must not promise one. A wake
    // whose moment has passed stops counting for the same reason a stale reset
    // time does — a claim that outlives its condition teaches the user to
    // disbelieve every other claim we make.
    void noteWake(const QString &threadId, const QDateTime &at);
    void clearWake(const QString &threadId);
    // The soonest armed resume, invalid when none is armed.
    QDateTime resumesAt() const;
    // Is an automatic resume armed for this one thread?
    bool resumeArmed(const QString &threadId) const;
    // The caveat that makes "resuming at 14:37" honest, empty when nothing is
    // armed. akcore lives only as long as the window it serves (it shuts down
    // when the last client disconnects), so a wake can only fire while Agent
    // Kate is open. Firing with the app CLOSED is plan 28 §Phase 3's visible
    // resurrection, which does not exist yet — so we say so instead of letting
    // the user infer it.
    QString resumeCaveat() const;

    // How many live agents are currently parked: a non-"allowed" status whose
    // reset time has not already passed, or an armed automatic resume still in
    // the future. The second half matters because an engine that EXITS on a
    // usage limit takes its report with it — the wake is then the only
    // remaining evidence that the agent is waiting rather than finished.
    int limitedCount() const;
    bool limited() const { return limitedCount() > 0; }
    // The soonest reset among the limited threads, invalid when none is known.
    // THE plan-28 input: arm the wake here (plus jitter — see §Phase 2).
    QDateTime resetsAt() const;
    // One human line for a status strip, empty when nothing is limited.
    QString summary() const;

    // Desktop popups, on by default. Tests turn them off so a report() cannot
    // reach out to the notification server; nothing else should touch this.
    static void setDesktopAlertsEnabled(bool on);

Q_SIGNALS:
    // The fleet's limited state, count or reset time actually moved.
    void changed();
    // The fleet ENTERED a limited state — emitted once per episode, whatever
    // the event rate, and never again until it comes back under the limit.
    // This is the coalescing seam: the desktop popup is one subscriber and a
    // test is another, so the rule can be asserted on the real report() path
    // rather than through a synthetic entry point that consumes the latch.
    void announced(const QString &notice);

private:
    explicit RateLimitState(QObject *parent = nullptr);

    // The coalescing rule itself: a notice string the FIRST time the fleet
    // enters a limited state, empty every time after, until it leaves that
    // state again. Mutates the latch, so it has exactly ONE caller (announce).
    QString takeAnnouncement();

    // Raise the coalesced desktop popup, if one is earned and the user is not
    // already looking at the app (an active Agent Kate window is showing the
    // roster strip, which is the same news without an interruption).
    void announce();

    // Re-arm the wake that fires when the soonest live window rolls over.
    // Nothing else moves at that moment — the parked agent emits no further
    // events — so without this timer the strip would keep its stale claim until
    // some unrelated report happened along.
    void rearmExpiry();

    QHash<QString, RateLimitReport> m_byThread;
    // Thread id → the moment the core says it will resume that thread. Kept
    // apart from the reports because the two have different lifetimes: the
    // report belongs to a running turn, the wake outlives the process that
    // stalled.
    QHash<QString, QDateTime> m_wakes;
    // True while the fleet is in a limited state and its notice has been taken:
    // the latch that turns one event per turn into one notice per episode.
    bool m_announced = false;
    QTimer *m_expiry = nullptr;
};

} // namespace agentkate
