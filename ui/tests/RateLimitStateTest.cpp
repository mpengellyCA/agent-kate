// Audit F43. A rate-limited agent used to be invisible everywhere except its
// own header: with five agents running, the parked one kept showing the roster's
// green "Working" arc and the only way to find out was to open it.
//
// The fix hoists the reports into one shared, account-level state, so this pins
// the four properties everything downstream depends on:
//
//   * the COUNT is per-agent but the QUESTION is account-wide (plan 28 §Phase 2:
//     one window, one wake schedule — modelling it per-thread produces a
//     thundering herd);
//   * resetsAt survives as DATA. Plan 28's auto-resume arms a wake timer on it,
//     and a QDateTime that had been flattened to "3:07 PM" inside a QLabel
//     cannot be armed on;
//   * the announcement is COALESCED — the CLI emits a rate_limit_event every
//     single turn, so "one notice per episode" is the whole difference between
//     a notification and a nuisance;
//   * a closed agent is forgotten, or a dead thread holds the roster strip up
//     forever.

#include "state/RateLimitState.h"

#include <QSignalSpy>
#include <QTest>

using agentkate::RateLimitReport;
using agentkate::RateLimitState;

namespace {
RateLimitReport allowed()
{
    return RateLimitReport{QStringLiteral("allowed"), QStringLiteral("five_hour"),
                           QDateTime(), false};
}
RateLimitReport rejected(const QDateTime &resets)
{
    return RateLimitReport{QStringLiteral("rejected"), QStringLiteral("five_hour"),
                           resets, false};
}
// A window that has NOT rolled over yet — what a live limit actually looks like.
QDateTime soon()
{
    return QDateTime::currentDateTimeUtc().addSecs(600);
}
} // namespace

class RateLimitStateTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase()
    {
        // Never reach for a notification server from a unit test.
        RateLimitState::setDesktopAlertsEnabled(false);
    }

    // The singleton is shared across test functions, so every one starts by
    // clearing the threads it will use.
    void cleanup()
    {
        RateLimitState *s = RateLimitState::self();
        for (const QString &tid :
             {QStringLiteral("t1"), QStringLiteral("t2"), QStringLiteral("t3")}) {
            s->forget(tid);
        }
        QCOMPARE(s->limitedCount(), 0);
    }

    // "allowed" is not a limit. Only a non-allowed status counts, and an empty
    // status is "no report yet" rather than a limit.
    void allowedIsNotALimit()
    {
        RateLimitState *s = RateLimitState::self();
        s->report(QStringLiteral("t1"), allowed());
        QCOMPARE(s->limitedCount(), 0);
        QVERIFY(!s->limited());
        QVERIFY(s->summary().isEmpty());
    }

    void countsOnlyLimitedThreads()
    {
        RateLimitState *s = RateLimitState::self();
        // A reset time in the FUTURE — a window that has already rolled over is
        // no longer a limit (see limitClearsWhenItsWindowRollsOver).
        s->report(QStringLiteral("t1"), rejected(soon()));
        s->report(QStringLiteral("t2"), allowed());
        s->report(QStringLiteral("t3"), rejected(soon()));
        QCOMPARE(s->limitedCount(), 2);
        QVERIFY(s->limited());
        QVERIFY(!s->summary().isEmpty());
    }

    // Plan 28 §Phase 2 arms its wake on this. The SOONEST reset among limited
    // threads is the one that matters, and an allowed thread's stale timestamp
    // must not drag it earlier.
    void resetsAtIsTheSoonestAmongLimited()
    {
        RateLimitState *s = RateLimitState::self();
        const QDateTime early = QDateTime::currentDateTimeUtc().addSecs(60);
        const QDateTime late = QDateTime::currentDateTimeUtc().addSecs(3600);
        s->report(QStringLiteral("t1"), rejected(late));
        s->report(QStringLiteral("t2"), rejected(early));
        QCOMPARE(s->resetsAt(), early);

        // An allowed thread carrying an even earlier stamp is not a limit and
        // must not be armed on.
        RateLimitReport ok = allowed();
        ok.resetsAt = QDateTime::currentDateTimeUtc().addSecs(1);
        s->report(QStringLiteral("t3"), ok);
        QCOMPARE(s->resetsAt(), early);
    }

    // No usable timestamp is a real case (the field is a unix number on some
    // builds and an ISO string on others). Report the limit, invent no time.
    void resetsAtAbsentIsInvalidNotGuessed()
    {
        RateLimitState *s = RateLimitState::self();
        s->report(QStringLiteral("t1"), rejected(QDateTime()));
        QVERIFY(s->limited());
        QVERIFY(!s->resetsAt().isValid());
        QVERIFY(!s->summary().isEmpty()); // still says the fleet is parked
    }

    // The coalescing rule: one notice per episode, however many events arrive.
    // Driven through the real report() path — the CLI's every-turn event rate is
    // the thing being defended against, so a synthetic entry point would not be
    // testing the feature.
    void announcementIsCoalescedPerEpisode()
    {
        RateLimitState *s = RateLimitState::self();
        QSignalSpy spy(s, &RateLimitState::announced);
        const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(600);

        s->report(QStringLiteral("t1"), rejected(resets));
        QCOMPARE(spy.count(), 1);
        QVERIFY(!spy.at(0).at(0).toString().isEmpty());

        // Every later event in the same episode — including a second agent
        // hitting the same account-wide window — is silent.
        RateLimitReport again = rejected(resets);
        again.overage = true; // a genuinely different report, same episode
        s->report(QStringLiteral("t1"), again);
        s->report(QStringLiteral("t2"), rejected(resets));
        QCOMPARE(spy.count(), 1);

        // Back under the limit re-arms it: the NEXT episode speaks again.
        s->report(QStringLiteral("t1"), allowed());
        s->report(QStringLiteral("t2"), allowed());
        QCOMPARE(spy.count(), 1); // nothing limited, nothing to say
        s->report(QStringLiteral("t1"), rejected(resets));
        QCOMPARE(spy.count(), 2);
    }

    // The CLI repeats itself every turn. A report identical to the one held
    // must not emit changed(), or the roster re-lays out on every event.
    void identicalReportIsSilent()
    {
        RateLimitState *s = RateLimitState::self();
        const RateLimitReport r = rejected(soon());
        s->report(QStringLiteral("t1"), r);
        QSignalSpy spy(s, &RateLimitState::changed);
        s->report(QStringLiteral("t1"), r);
        s->report(QStringLiteral("t1"), r);
        QCOMPARE(spy.count(), 0);
        // …but a real change still speaks.
        s->report(QStringLiteral("t1"), allowed());
        QCOMPARE(spy.count(), 1);
    }

    // A closed agent cannot still be waiting on a usage window.
    void forgettingAClosedAgentReleasesTheNotice()
    {
        RateLimitState *s = RateLimitState::self();
        s->report(QStringLiteral("t1"), rejected(soon()));
        QVERIFY(s->limited());
        QSignalSpy spy(s, &RateLimitState::changed);
        s->forget(QStringLiteral("t1"));
        QCOMPARE(spy.count(), 1);
        QVERIFY(!s->limited());
        QVERIFY(s->summary().isEmpty());
        // Forgetting an unknown thread is a no-op, not a spurious repaint.
        s->forget(QStringLiteral("t1"));
        QCOMPARE(spy.count(), 1);
    }

    // A status that outlives its own condition is worse than no status at all.
    // The engine emits a rate_limit_event per TURN, and a parked agent takes no
    // turns — so nothing was ever going to arrive to say the window had rolled
    // over, and the strip went on reading "resets at 15:04" at 17:00.
    void limitClearsWhenItsWindowRollsOver()
    {
        RateLimitState *s = RateLimitState::self();
        s->report(QStringLiteral("t1"),
                  rejected(QDateTime::currentDateTimeUtc().addSecs(-60)));
        QCOMPARE(s->limitedCount(), 0);
        QVERIFY(!s->limited());
        QVERIFY2(s->summary().isEmpty(),
                 "a usage limit whose reset time has passed must claim nothing");
        QVERIFY(!s->resetsAt().isValid());
    }

    // A limit with no usable reset time is NOT expired: we were told there is a
    // limit and not told when it ends, so we keep saying what we know.
    void limitWithNoResetTimeNeverExpires()
    {
        RateLimitState *s = RateLimitState::self();
        s->report(QStringLiteral("t1"), rejected(QDateTime()));
        QCOMPARE(s->limitedCount(), 1);
        QVERIFY(!s->summary().isEmpty());
    }

    // And the clearing must ANNOUNCE itself, because nothing else will: the
    // state arms a wake on the soonest reset so subscribers repaint at the
    // moment the claim stops being true, not at the next unrelated event.
    void expiryWakesSubscribersWithoutAnyFurtherReport()
    {
        RateLimitState *s = RateLimitState::self();
        s->report(QStringLiteral("t1"),
                  rejected(QDateTime::currentDateTimeUtc().addMSecs(300)));
        QVERIFY(s->limited());
        QSignalSpy spy(s, &RateLimitState::changed);
        // The wake, not the value: limitedCount() recomputes on every call, so
        // polling it would pass with no timer at all. What was missing — and
        // what the roster needs — is being TOLD.
        QTRY_VERIFY_WITH_TIMEOUT(spy.count() >= 1, 5000);
        QVERIFY(!s->limited());
    }

    // --- plan 28 §Phase 2: the armed automatic resume ------------------------

    // "Resets at" and "resumes at" are different promises, and only one of them
    // is ours to make. The window reopening is a fact about the account; an
    // agent CONTINUING is a thing the core has to have scheduled. So the words
    // change only when a wake is actually armed — this is the honest-labelling
    // rule applied to the feature's headline string.
    void onlyAnArmedWakeLicensesTheResumesAtClaim()
    {
        RateLimitState *s = RateLimitState::self();
        const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(600);
        s->report(QStringLiteral("t1"), rejected(resets));

        const QString beforeArming = s->summary();
        QVERIFY2(!beforeArming.contains(QStringLiteral("resum")),
                 "the strip promised a resume nobody had scheduled");
        QVERIFY(beforeArming.contains(QStringLiteral("resets")));
        QVERIFY2(s->resumeCaveat().isEmpty(),
                 "a caveat was offered for a promise that was never made");
        QVERIFY(!s->resumeArmed(QStringLiteral("t1")));

        s->noteWake(QStringLiteral("t1"), resets);

        QVERIFY(s->resumeArmed(QStringLiteral("t1")));
        QCOMPARE(s->resumesAt(), resets);
        const QString armed = s->summary();
        QVERIFY2(armed.contains(QStringLiteral("resuming")),
                 qPrintable(QStringLiteral("an armed resume is not surfaced: ") + armed));
        // And the condition it depends on is available to whatever renders it:
        // akcore only lives while this window does.
        QVERIFY(!s->resumeCaveat().isEmpty());
    }

    // The engine EXITS when the window is exhausted, taking its last report
    // with it. The armed resume is then the only remaining evidence that this
    // agent is waiting rather than finished — so it alone keeps the agent
    // counted, or the strip would go quiet on a fleet that is still parked.
    void anArmedWakeAloneKeepsTheAgentParked()
    {
        RateLimitState *s = RateLimitState::self();
        const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(600);
        s->report(QStringLiteral("t1"), rejected(resets));
        s->noteWake(QStringLiteral("t1"), resets);

        s->forgetReport(QStringLiteral("t1")); // the process exited
        QCOMPARE(s->limitedCount(), 1);
        QVERIFY(s->summary().contains(QStringLiteral("resuming")));

        // One agent is one agent: a report AND a wake for the same thread must
        // not be counted twice.
        s->report(QStringLiteral("t1"), rejected(resets));
        QCOMPARE(s->limitedCount(), 1);

        // Closing the agent for real drops both halves.
        s->forget(QStringLiteral("t1"));
        QCOMPARE(s->limitedCount(), 0);
        QVERIFY(s->summary().isEmpty());
    }

    // A resume time that has passed promises nothing. Same rule as an expired
    // reset time, and for the same reason: at 14:40 the words "resuming at
    // 14:37" teach the user to disbelieve everything else the strip says.
    void aPassedWakeStopsPromisingAResume()
    {
        RateLimitState *s = RateLimitState::self();
        const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(600);
        s->report(QStringLiteral("t1"), rejected(resets));
        s->noteWake(QStringLiteral("t1"), QDateTime::currentDateTimeUtc().addSecs(-30));

        QVERIFY(!s->resumesAt().isValid());
        QVERIFY(!s->resumeArmed(QStringLiteral("t1")));
        const QString text = s->summary();
        QVERIFY2(!text.contains(QStringLiteral("resuming")), qPrintable(text));
        QVERIFY(text.contains(QStringLiteral("resets"))); // the window is still shut
    }

    // The core cancels a wake when the human resumes or stops the agent itself.
    // The claim has to go with it, immediately — and repeats of the same armed
    // wake must stay silent, since the core re-states it as it folds in reports.
    void cancellingAWakeWithdrawsTheClaim()
    {
        RateLimitState *s = RateLimitState::self();
        const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(600);
        s->report(QStringLiteral("t1"), rejected(resets));
        s->noteWake(QStringLiteral("t1"), resets);

        QSignalSpy spy(s, &RateLimitState::changed);
        s->noteWake(QStringLiteral("t1"), resets); // same wake, again
        QCOMPARE(spy.count(), 0);

        s->clearWake(QStringLiteral("t1"));
        QCOMPARE(spy.count(), 1);
        QVERIFY(!s->resumeArmed(QStringLiteral("t1")));
        QVERIFY2(!s->summary().contains(QStringLiteral("resuming")),
                 "a cancelled resume was still being promised");
    }
};

QTEST_MAIN(RateLimitStateTest)
#include "RateLimitStateTest.moc"
