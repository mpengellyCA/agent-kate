// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// Plan 27 §2: the tray's DECISION layer, no D-Bus anywhere. Same split (and
// same reason) as AgentNotifierTest: evaluate/fold is policy and testable,
// only embed() talks to a StatusNotifier host, and none of these tests call
// it. What is pinned:
//
//   * status from counts — Passive when idle (recorded decision), Active while
//     running, NeedsAttention (the pulsing state) the moment anyone is blocked;
//   * the tooltip counts, which must agree with the roster's definitions
//     (running == Working, not NeedsInput/RateLimited);
//   * the AgentNotifier fold copied verbatim — NeedsInput raises attention,
//     any other status clears it, a forgotten agent's late signals are dropped;
//   * shouldHideToTray — the close-to-tray gate whose FALSE cases are the
//     traps: no tray host (the unquittable-app fallback), a genuine quit, a
//     session logout, and the preference simply being off (the default);
//   * shouldExplainNoHost — the one-time fallback explanation's gate.

#include "shell/TrayPresence.h"

#include <QTest>

using agentkate::TrayPresence;

// AgentRoles::AgentStatus (AgentCardDelegate.h) as the ints the wire carries.
namespace {
constexpr int kIdle = 0;
constexpr int kWorking = 1;
constexpr int kNeedsInput = 2;
constexpr int kDormant = 3;
constexpr int kError = 4;
constexpr int kRateLimited = 5;
} // namespace

class TrayPresenceTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void statusFromCounts();
    void tooltipNamesBothCounts();
    void runningMeansWorkingOnly();
    void needsInputRaisesAttentionAndOtherStatusClearsIt();
    void attentionWireLatchesAndClears();
    void forgottenAgentStaysForgotten();
    void firstAttentionAgentIsTheLongestWaiting();
    void hideToTrayGate();
    void noHostExplanationGate();
};

void TrayPresenceTest::statusFromCounts()
{
    // Passive when idle is the recorded decision for open question 4.
    QCOMPARE(TrayPresence::evaluateStatus(0, 0), TrayPresence::Status::Passive);
    QCOMPARE(TrayPresence::evaluateStatus(3, 0), TrayPresence::Status::Active);
    // Attention outranks running: one blocked agent must pulse even while ten
    // others are happily computing.
    QCOMPARE(TrayPresence::evaluateStatus(10, 1),
             TrayPresence::Status::NeedsAttention);
    QCOMPARE(TrayPresence::evaluateStatus(0, 2),
             TrayPresence::Status::NeedsAttention);
}

void TrayPresenceTest::tooltipNamesBothCounts()
{
    const QString both = TrayPresence::tooltipText(3, 1);
    QVERIFY2(both.contains(QStringLiteral("3")) && both.contains(QStringLiteral("1")),
             qPrintable(QStringLiteral("tooltip lost a count: ") + both));
    // One-sided states name only the side that exists.
    const QString runningOnly = TrayPresence::tooltipText(2, 0);
    QVERIFY(runningOnly.contains(QStringLiteral("2")));
    QVERIFY(!runningOnly.contains(QStringLiteral("waiting")));
    const QString idle = TrayPresence::tooltipText(0, 0);
    QVERIFY(!idle.isEmpty());
}

void TrayPresenceTest::runningMeansWorkingOnly()
{
    TrayPresence tray;
    tray.reportStatus(1, kWorking);
    tray.reportStatus(2, kIdle);
    tray.reportStatus(3, kDormant);
    tray.reportStatus(4, kError);
    // RateLimited is parked, NOT running — counting it re-fights audit F43.
    tray.reportStatus(5, kRateLimited);
    QCOMPARE(tray.runningCount(), 1);
    QCOMPARE(tray.status(), TrayPresence::Status::Active);
}

void TrayPresenceTest::needsInputRaisesAttentionAndOtherStatusClearsIt()
{
    TrayPresence tray;
    tray.reportStatus(1, kNeedsInput);
    QCOMPARE(tray.attentionCount(), 1);
    QCOMPARE(tray.status(), TrayPresence::Status::NeedsAttention);
    // The prompt was answered and the turn resumed: attention must fall away
    // with the status, exactly as the roster card's marker does.
    tray.reportStatus(1, kWorking);
    QCOMPARE(tray.attentionCount(), 0);
    QCOMPARE(tray.status(), TrayPresence::Status::Active);
}

void TrayPresenceTest::attentionWireLatchesAndClears()
{
    TrayPresence tray;
    tray.reportStatus(1, kWorking);
    tray.reportAttention(1, true);
    QCOMPARE(tray.attentionCount(), 1);
    tray.reportAttention(1, false);
    QCOMPARE(tray.attentionCount(), 0);
    QCOMPARE(tray.status(), TrayPresence::Status::Active);
}

void TrayPresenceTest::forgottenAgentStaysForgotten()
{
    TrayPresence tray;
    tray.reportStatus(7, kNeedsInput);
    QCOMPARE(tray.status(), TrayPresence::Status::NeedsAttention);
    tray.forgetAgent(7);
    QCOMPARE(tray.attentionCount(), 0);
    QCOMPARE(tray.status(), TrayPresence::Status::Passive);
    // Panel teardown is deleteLater, so late signals from the doomed panel are
    // real; they must not resurrect the agent (ids are never reused).
    tray.reportStatus(7, kWorking);
    tray.reportAttention(7, true);
    tray.setAgentTitle(7, QStringLiteral("ghost"));
    QCOMPARE(tray.runningCount(), 0);
    QCOMPARE(tray.attentionCount(), 0);
}

void TrayPresenceTest::firstAttentionAgentIsTheLongestWaiting()
{
    TrayPresence tray;
    QCOMPARE(tray.firstAttentionAgent(), -1);
    tray.reportStatus(3, kNeedsInput);
    tray.reportStatus(5, kNeedsInput);
    // Ids are monotonic, so the lowest blocked id has waited longest.
    QCOMPARE(tray.firstAttentionAgent(), 3);
    tray.reportStatus(3, kIdle);
    QCOMPARE(tray.firstAttentionAgent(), 5);
}

void TrayPresenceTest::hideToTrayGate()
{
    // The one TRUE row: preference on, live tray, plain close.
    QVERIFY(TrayPresence::shouldHideToTray(true, true, false, false));
    // Preference off — the DEFAULT — always quits (recorded decision).
    QVERIFY(!TrayPresence::shouldHideToTray(false, true, false, false));
    // No tray item: hiding would strand the app (the unquittable-app trap).
    QVERIFY(!TrayPresence::shouldHideToTray(true, false, false, false));
    // File ▸ Quit / tray Quit mean quit, never hide.
    QVERIFY(!TrayPresence::shouldHideToTray(true, true, true, false));
    // Session logout: the session manager is ending us; hide would lose the
    // stop-and-compact shutdown.
    QVERIFY(!TrayPresence::shouldHideToTray(true, true, false, true));
}

void TrayPresenceTest::noHostExplanationGate()
{
    // Preference on, no host, never explained → say it once.
    QVERIFY(TrayPresence::shouldExplainNoHost(true, false, false));
    // Already said, or a host exists, or the preference is off → silence.
    QVERIFY(!TrayPresence::shouldExplainNoHost(true, false, true));
    QVERIFY(!TrayPresence::shouldExplainNoHost(true, true, false));
    QVERIFY(!TrayPresence::shouldExplainNoHost(false, false, false));
}

QTEST_MAIN(TrayPresenceTest)
#include "TrayPresenceTest.moc"
