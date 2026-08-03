// Tests for AgentNotifier's alert policy — which agent state transitions earn a
// desktop popup, and which are noise. Only the decision half is exercised
// (evaluateStatus / evaluateAttention): emitting needs a notification server,
// while the rules below are exactly what a user notices when they are wrong.

#include "AgentCardDelegate.h" // AgentRoles::AgentStatus
#include "notify/AgentNotifier.h"

#include <QList>
#include <QTest>
#include <QWidget>

using agentkate::AgentNotifier;
using Alert = agentkate::AgentNotifier::Alert;

namespace {
constexpr int kIdle = int(AgentRoles::AgentStatus::Idle);
constexpr int kWorking = int(AgentRoles::AgentStatus::Working);
constexpr int kNeedsInput = int(AgentRoles::AgentStatus::NeedsInput);
constexpr int kDormant = int(AgentRoles::AgentStatus::Dormant);
constexpr int kError = int(AgentRoles::AgentStatus::Error);

// Window activation is a compositor decision; stage it directly instead.
class TestNotifier : public AgentNotifier
{
public:
    using AgentNotifier::AgentNotifier;
    bool active = false;

protected:
    bool windowIsActive() const override { return active; }
};
} // namespace

class AgentNotifierTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void finishedOnlyAfterWorking();
    void errorAlwaysFails();
    void attentionIsDeduped_data();
    void attentionIsDeduped();
    void questionGetsDistinctAlertAndConsumesAttentionLatch();
    void visibleAgentInActiveWindowIsSilent();
    void forgottenAgentSaysNothing();
    void closingAllAlertsRetractsOutstandingPrompts();
    void finishesCoalesceWithinTheWindow();
    void forgettingAnAgentDropsItFromTheBatch();
    void failuresCoalesceWithinTheWindow();
    void crashLoopCostsOnePopupPerWindow();
    void finishAndFailureWindowsAreIndependent();
};

// Idle is where an agent sits before it has ever run and where a dormant one
// lands when adopted; only a turn that was computing can have "finished".
void AgentNotifierTest::finishedOnlyAfterWorking()
{
    QWidget window;
    TestNotifier n(&window);

    QCOMPARE(n.evaluateStatus(1, kIdle), Alert::None);      // fresh agent
    QCOMPARE(n.evaluateStatus(1, kWorking), Alert::None);   // start is not news
    QCOMPARE(n.evaluateStatus(1, kIdle), Alert::Finished);  // the turn ended
    QCOMPARE(n.evaluateStatus(1, kIdle), Alert::None);      // repeat, not a change

    QCOMPARE(n.evaluateStatus(2, kDormant), Alert::None);
    QCOMPARE(n.evaluateStatus(2, kIdle), Alert::None);      // adoption, not a finish
}

void AgentNotifierTest::errorAlwaysFails()
{
    QWidget window;
    TestNotifier n(&window);

    QCOMPARE(n.evaluateStatus(1, kWorking), Alert::None);
    QCOMPARE(n.evaluateStatus(1, kError), Alert::Failed);
    // A second report of the same status is not a second failure.
    QCOMPARE(n.evaluateStatus(1, kError), Alert::None);
    // Recovering and failing again is.
    QCOMPARE(n.evaluateStatus(1, kWorking), Alert::None);
    QCOMPARE(n.evaluateStatus(1, kError), Alert::Failed);
}

// The status transition and the attention flag both announce the same prompt and
// arrive in no fixed order: one popup, whichever comes first.
void AgentNotifierTest::attentionIsDeduped_data()
{
    QTest::addColumn<bool>("statusFirst");
    QTest::newRow("status then flag") << true;
    QTest::newRow("flag then status") << false;
}

void AgentNotifierTest::attentionIsDeduped()
{
    QFETCH(bool, statusFirst);
    QWidget window;
    TestNotifier n(&window);
    n.evaluateStatus(1, kWorking);

    if (statusFirst) {
        QCOMPARE(n.evaluateStatus(1, kNeedsInput), Alert::NeedsAttention);
        QCOMPARE(n.evaluateAttention(1, true), Alert::None);
    } else {
        QCOMPARE(n.evaluateAttention(1, true), Alert::NeedsAttention);
        QCOMPARE(n.evaluateStatus(1, kNeedsInput), Alert::None);
    }

    // Answering the prompt re-arms it: the next one is a new decision.
    QCOMPARE(n.evaluateAttention(1, false), Alert::None);
    QCOMPARE(n.evaluateAttention(1, true), Alert::NeedsAttention);
    // So does moving on via the status channel.
    QCOMPARE(n.evaluateStatus(1, kWorking), Alert::None);
    QCOMPARE(n.evaluateStatus(1, kNeedsInput), Alert::NeedsAttention);
}

// A question is still a blocking prompt, but it must be individually
// configurable in KDE's notification settings.  Its shared latch also proves
// the question event cannot be followed by a second generic permission popup.
void AgentNotifierTest::questionGetsDistinctAlertAndConsumesAttentionLatch()
{
    QWidget window;
    TestNotifier n(&window);

    QCOMPARE(n.evaluateQuestion(1), Alert::Question);
    QCOMPARE(n.evaluateAttention(1, true), Alert::None);
    QCOMPARE(n.evaluateStatus(1, kNeedsInput), Alert::None);

    // Resolving the prompt re-arms a later, separate question.
    QCOMPARE(n.evaluateAttention(1, false), Alert::None);
    QCOMPARE(n.evaluateQuestion(1), Alert::Question);
}

void AgentNotifierTest::visibleAgentInActiveWindowIsSilent()
{
    QWidget window;
    TestNotifier n(&window);
    n.active = true;
    n.setVisibleAgent(1);

    // Agent 1 is on screen in the focused window — the user is watching it.
    n.evaluateStatus(1, kWorking);
    QCOMPARE(n.evaluateStatus(1, kIdle), Alert::None);
    QCOMPARE(n.evaluateAttention(1, true), Alert::None);

    // Agent 2 is background work even though the window is focused.
    n.evaluateStatus(2, kWorking);
    QCOMPARE(n.evaluateStatus(2, kIdle), Alert::Finished);

    // Window loses focus: even the shown agent has to speak up now.
    n.active = false;
    n.evaluateStatus(1, kWorking);
    QCOMPARE(n.evaluateStatus(1, kIdle), Alert::Finished);

    // A suppressed prompt still latches, so the visible agent's outstanding
    // attention is not re-announced when the window comes back either.
    n.active = true;
    QCOMPARE(n.evaluateAttention(1, true), Alert::None);
    n.active = false;
    QCOMPARE(n.evaluateAttention(1, true), Alert::None);
}

// A closed agent's panel outlives the close by an event-loop turn and can still
// emit; nothing it says may resurrect it.
void AgentNotifierTest::forgottenAgentSaysNothing()
{
    QWidget window;
    TestNotifier n(&window);
    n.evaluateStatus(1, kWorking);
    n.forgetAgent(1);

    QCOMPARE(n.evaluateStatus(1, kIdle), Alert::None);
    QCOMPARE(n.evaluateStatus(1, kError), Alert::None);
    QCOMPARE(n.evaluateAttention(1, true), Alert::None);
    QCOMPARE(n.evaluateStatus(1, kNeedsInput), Alert::None);

    // Other agents are unaffected.
    n.evaluateStatus(2, kWorking);
    QCOMPARE(n.evaluateStatus(2, kIdle), Alert::Finished);
}

// Quitting must take the persistent popups with it. The retraction itself needs
// a notification server, but the latch it clears is observable: an agent whose
// prompt has been retracted is free to announce the next one.
void AgentNotifierTest::closingAllAlertsRetractsOutstandingPrompts()
{
    QWidget window;
    TestNotifier n(&window);

    QCOMPARE(n.evaluateAttention(1, true), Alert::NeedsAttention);
    QCOMPARE(n.evaluateAttention(1, true), Alert::None); // still outstanding
    n.closeAllAlerts();
    QCOMPARE(n.evaluateAttention(1, true), Alert::NeedsAttention);
}

// A fleet finishing a turn together must not be one popup per agent. The first
// finish still speaks on its own — a lone agent behaves exactly as before — and
// everything landing in the window behind it is announced once.
void AgentNotifierTest::finishesCoalesceWithinTheWindow()
{
    QWidget window;
    TestNotifier n(&window);

    // Nothing has finished recently: this one is its own popup, alone.
    QVERIFY(n.noteFinished(1));
    QVERIFY(n.takeFinishBatch().isEmpty()); // and it pooled nothing

    // An empty window close ends the burst, so the next finish is news again.
    QVERIFY(n.noteFinished(1));

    // The rest of the fleet lands inside the window: pooled, not announced.
    QVERIFY(!n.noteFinished(2));
    QVERIFY(!n.noteFinished(3));
    QVERIFY(!n.noteFinished(2)); // same agent twice is still one line
    const QList<int> batch = n.takeFinishBatch();
    QCOMPARE(batch, QList<int>({2, 3}));

    // A non-empty close re-opens the window: a fleet that keeps finishing keeps
    // costing one popup per window, never one per agent.
    QVERIFY(!n.noteFinished(4));
    QCOMPARE(n.takeFinishBatch(), QList<int>({4}));
    // That close was non-empty too, so the window is still open…
    QVERIFY(!n.noteFinished(5));
    QCOMPARE(n.takeFinishBatch(), QList<int>({5}));
    // …until one closes on an empty pool.
    QVERIFY(n.takeFinishBatch().isEmpty());
    QVERIFY(n.noteFinished(6));
}

// The aggregate names agents the user can still open; a closed one is not that.
void AgentNotifierTest::forgettingAnAgentDropsItFromTheBatch()
{
    QWidget window;
    TestNotifier n(&window);

    QVERIFY(n.noteFinished(1)); // opens the window
    QVERIFY(!n.noteFinished(2));
    QVERIFY(!n.noteFinished(3));
    n.forgetAgent(2);
    QCOMPARE(n.takeFinishBatch(), QList<int>({3}));
}

// Failures used to get none of the batching finishes got (audit F24): every
// Error transition was its own popup, so an agent that crash-loops — start,
// error, restart, error — buried the desktop. Same rule, own window.
void AgentNotifierTest::failuresCoalesceWithinTheWindow()
{
    QWidget window;
    TestNotifier n(&window);

    // The first failure is news on its own and opens the window.
    QVERIFY(n.noteFailed(1));
    // Everything inside it pools.
    QVERIFY(!n.noteFailed(2));
    QVERIFY(!n.noteFailed(3));
    QVERIFY(!n.noteFailed(2)); // same agent twice is still one line
    QCOMPARE(n.takeFailBatch(), QList<int>({2, 3}));

    // A non-empty close re-opens the window; an empty one ends the burst.
    QVERIFY(!n.noteFailed(4));
    QCOMPARE(n.takeFailBatch(), QList<int>({4}));
    QVERIFY(n.takeFailBatch().isEmpty());
    QVERIFY(n.noteFailed(5));
}

// The shape the finding is actually about: ONE agent failing over and over.
// Whatever it does inside a window, it costs one popup per window.
void AgentNotifierTest::crashLoopCostsOnePopupPerWindow()
{
    QWidget window;
    TestNotifier n(&window);

    int popups = 0;
    for (int crash = 0; crash < 50; ++crash) {
        // Each crash is a real Failed alert — the evaluate half still fires.
        QCOMPARE(n.evaluateStatus(1, kWorking), Alert::None);
        QCOMPARE(n.evaluateStatus(1, kError), Alert::Failed);
        if (n.noteFailed(1)) {
            ++popups;
        }
    }
    QCOMPARE(popups, 1); // fifty crashes, one popup, until the window closes
    QCOMPARE(n.takeFailBatch(), QList<int>({1}));
    // Window re-opened by the non-empty close, so the loop keeps costing one
    // popup per window rather than one per crash.
    QVERIFY(!n.noteFailed(1));
}

// Separate windows on purpose: a fleet finishing must not delay the news that
// something broke, and vice versa.
void AgentNotifierTest::finishAndFailureWindowsAreIndependent()
{
    QWidget window;
    TestNotifier n(&window);

    QVERIFY(n.noteFinished(1));       // opens the finish window
    QVERIFY(n.noteFailed(2));         // a failure still speaks immediately
    QVERIFY(!n.noteFinished(3));
    QVERIFY(!n.noteFailed(4));
    QCOMPARE(n.takeFinishBatch(), QList<int>({3}));
    QCOMPARE(n.takeFailBatch(), QList<int>({4}));

    // forgetAgent drops a closed agent from BOTH pools.
    QVERIFY(!n.noteFinished(5));
    QVERIFY(!n.noteFailed(5));
    n.forgetAgent(5);
    QVERIFY(n.takeFinishBatch().isEmpty());
    QVERIFY(n.takeFailBatch().isEmpty());
}

QTEST_MAIN(AgentNotifierTest)
#include "AgentNotifierTest.moc"
