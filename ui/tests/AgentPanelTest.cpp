// The chat panel's two honest-labelling surfaces, exercised on the real widget
// rather than on the free functions behind it — which is the point.
//
// F44 (second pass): the empty-state hint tells a first-time user what will
// happen to their files when they send, and it reads the isolation picker to do
// it. The copy was already covered by a test on feedEmptyStateHtml(); what was
// NOT covered, and what was missing, is the WIRING — nothing connected the
// picker's currentIndexChanged to updateFeedEmptyState, so changing isolation
// left the previous promise on screen. A test on the string builder can never
// see that, so this one drives the combo.
//
// F30/F49 (convergence): this panel is the Ctrl+N path — the isolation picker
// people actually use — and it was still calling "auto" a private copy after
// the guided dialog had stopped. Its labels must be IsolationCopy's.
//
// F43: a rate-limited agent's claim is account-level and outlives the panel's
// attention span. It must be withdrawn when the agent stops being a running
// agent, not only when the panel is destroyed.

#include "AgentPanel.h"
#include "AgentChatHelpers.h"
#include "NewAgentDialog.h" // IsolationCopy — the shared isolation wording
#include "ipc/CoreClient.h"
#include "state/DraftStore.h"
#include "state/RateLimitState.h"

#include <QComboBox>
#include <QLabel>
#include <QPlainTextEdit>
#include <QStandardPaths>
#include <QtTest>

#include <KConfigGroup>
#include <KSharedConfig>

namespace {

// The "Where it works" picker: the only combo carrying all three isolation
// tokens (the permission picker also has an "auto", which is why all three).
QComboBox *isolationCombo(QWidget *panel)
{
    const auto combos = panel->findChildren<QComboBox *>();
    for (QComboBox *c : combos) {
        if (c->findData(QStringLiteral("auto")) >= 0
            && c->findData(QStringLiteral("isolated")) >= 0
            && c->findData(QStringLiteral("workspace")) >= 0) {
            return c;
        }
    }
    return nullptr;
}

// The empty-state hint over the transcript viewport, found by its content so
// the test is not coupled to the widget tree.
QLabel *emptyStateHint(QWidget *panel)
{
    const auto labels = panel->findChildren<QLabel *>();
    for (QLabel *l : labels) {
        if (l->text().contains(QStringLiteral("command palette"))) {
            return l;
        }
    }
    return nullptr;
}

agentkate::RateLimitReport limitedReport()
{
    agentkate::RateLimitReport r;
    r.status = QStringLiteral("rejected");
    r.rateLimitType = QStringLiteral("five_hour");
    r.resetsAt = QDateTime::currentDateTime().addSecs(3600);
    return r;
}

} // namespace

class AgentPanelTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void cleanup();

    void isolationLabelsComeFromTheSharedCopy();
    void changingIsolationRepaintsTheEmptyState();
    void rebindingTheThreadWithdrawsItsUsageLimitClaim();
    void closingInsideTheAutosaveWindowKeepsTheDraft();
    void aDestroyedThreadsDraftIsNotWrittenBackByThePendingTimer();
};

void AgentPanelTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    // Nothing here may reach a notification server.
    agentkate::RateLimitState::setDesktopAlertsEnabled(false);
}

void AgentPanelTest::cleanup()
{
    agentkate::RateLimitState::self()->forget(QStringLiteral("t-one"));
    agentkate::RateLimitState::self()->forget(QStringLiteral("t-two"));
}

// The convergence: this picker's words are the shared ones, so "auto" cannot go
// back to being called a private copy here while the other two are honest.
void AgentPanelTest::isolationLabelsComeFromTheSharedCopy()
{
    CoreClient core;
    AgentPanel panel(&core);
    QComboBox *isolation = isolationCombo(&panel);
    QVERIFY(isolation != nullptr);
    for (const char *mode : {"auto", "isolated", "workspace"}) {
        const QString id = QString::fromLatin1(mode);
        const int idx = isolation->findData(id);
        QVERIFY(idx >= 0);
        QCOMPARE(isolation->itemText(idx), IsolationCopy::modeLabel(id));
    }
    QCOMPARE(isolation->toolTip(), IsolationCopy::modeTooltip());
}

// The F44 wiring. The hint describes what sending will do to the user's files;
// change the picker and it must stop describing the old choice.
void AgentPanelTest::changingIsolationRepaintsTheEmptyState()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.resize(800, 600);

    QComboBox *isolation = isolationCombo(&panel);
    QVERIFY(isolation != nullptr);
    QLabel *hint = emptyStateHint(&panel);
    QVERIFY2(hint != nullptr, "the empty feed must carry its hint");

    isolation->setCurrentIndex(isolation->findData(QStringLiteral("workspace")));
    const QString workspaceText = hint->text();
    QVERIFY2(!workspaceText.contains(QStringLiteral("private copy")),
             "the hint promised a private copy to an agent set to work "
             "directly in the user's files");
    QVERIFY(workspaceText.contains(QStringLiteral("directly")));

    isolation->setCurrentIndex(isolation->findData(QStringLiteral("isolated")));
    const QString isolatedText = hint->text();
    QVERIFY2(isolatedText != workspaceText,
             "changing isolation left a stale promise on screen");
    QVERIFY(isolatedText.contains(QStringLiteral("private copy")));

    // And back: this is not a one-way latch.
    isolation->setCurrentIndex(isolation->findData(QStringLiteral("workspace")));
    QCOMPARE(hint->text(), workspaceText);
}

// F43. A usage-limit report is a fact about a RUNNING agent. When this panel
// stops being that agent — it goes dormant, or is rebound to another thread —
// the account-level claim must go with it, or the roster's "N agents paused by
// a usage limit" strip keeps counting a thread nobody is waiting on.
void AgentPanelTest::rebindingTheThreadWithdrawsItsUsageLimitClaim()
{
    CoreClient core;
    AgentPanel panel(&core);
    auto *state = agentkate::RateLimitState::self();

    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("claude"));
    state->report(QStringLiteral("t-one"), limitedReport());
    QCOMPARE(state->limitedCount(), 1);

    // The panel is now a different agent. The old thread is not parked on a
    // usage window — it is not running at all.
    panel.setDormant(QStringLiteral("t-two"), QStringLiteral("two"), false,
                     QStringLiteral("claude"));
    QCOMPARE(state->limitedCount(), 0);
}

// The draft's teardown contract, from the panel's side.
//
// The composer autosaves on a 400 ms debounce whose QTimer is a CHILD of this
// panel. "Type a sentence, hit Close" therefore destroys the timer before it
// fires and loses the text — the one case the feature exists for. flushDraft()
// settles the debounce, and AgentDock calls it on every KEEP teardown.
void AgentPanelTest::closingInsideTheAutosaveWindowKeepsTheDraft()
{
    const QString workspace = QStringLiteral("/tmp/agentkate-draft-flush-test");
    const QString key = DraftStore::workspaceKey(workspace);
    QVERIFY(!key.isEmpty());
    DraftStore::clear(key);

    {
        CoreClient core;
        AgentPanel panel(&core);
        panel.setWorkspace(workspace);
        QPlainTextEdit *composer = panel.findChild<QPlainTextEdit *>();
        QVERIFY(composer != nullptr);
        composer->setPlainText(QStringLiteral("half a thought, not yet sent"));
        // Deliberately do NOT spin the event loop: this is the close that lands
        // inside the debounce window.
        panel.flushDraft();
    }

    QCOMPARE(KSharedConfig::openConfig()
                 ->group(QStringLiteral("Agent"))
                 .readEntry(key, QString()),
             QStringLiteral("half a thought, not yet sent"));
    DraftStore::clear(key);
}

// The other side of the same contract. On a real discard the dock clears the
// stored draft and THEN lets the panel die (deleteLater), so a debounce still
// pending would write the text of a destroyed thread straight back into the
// config it was just purged from.
void AgentPanelTest::aDestroyedThreadsDraftIsNotWrittenBackByThePendingTimer()
{
    const QString workspace = QStringLiteral("/tmp/agentkate-draft-forget-test");
    const QString key = DraftStore::workspaceKey(workspace);
    DraftStore::clear(key);

    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(workspace);
    QPlainTextEdit *composer = panel.findChild<QPlainTextEdit *>();
    QVERIFY(composer != nullptr);
    composer->setPlainText(QStringLiteral("words of a thread being discarded"));

    // What AgentDock::removeAgentEntry does on DraftDisposition::Forget, in
    // order: silence the pending write, then clear.
    panel.dropPendingDraftWrite();
    DraftStore::clear(key);

    // Give any surviving 400 ms timer more than its interval to fire.
    QTest::qWait(600);
    QVERIFY2(KSharedConfig::openConfig()
                 ->group(QStringLiteral("Agent"))
                 .readEntry(key, QString())
                 .isEmpty(),
             "a pending autosave resurrected the draft of a destroyed thread");
}

QTEST_MAIN(AgentPanelTest)
#include "AgentPanelTest.moc"
