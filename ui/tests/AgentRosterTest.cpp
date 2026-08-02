// Plan 16 P5: the roster stopped being a two-level tree. Workers nest under the
// controller that launched them, which quietly invalidates every traversal that
// assumed project->child(j) — filtering, the working animation, the attention
// roll-up — and adds two ways to lose an agent outright:
//
//   * closing a controller would delete its workers' ROWS as tree children,
//     while the worker threads keep running (they are separate agents), and
//   * a filter match on a nested worker is invisible while its controller row
//     is hidden, because Qt hides a row whose parent is hidden.
//
// These drive the real widget, through its public API, and assert the tree
// shape a user would see.

#include "AgentCardDelegate.h"
#include "AgentRoster.h"
#include "state/RateLimitState.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QAction>
#include <QLabel>
#include <QLineEdit>
#include <QLocale>
#include <QMenu>
#include <QSignalSpy>
#include <QStandardPaths>
#include <QToolButton>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QtTest>

class AgentRosterTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void init();
    void cleanup();

    void nestsWorkerUnderController();
    void countsOnlyLiveWorkers();
    void closingAControllerOrphansItsWorkers();
    void unknownParentKeepsWorkerAtProjectLevel();
    void refusesCycles();
    void reparentKeepsSelection();
    void filterKeepsNestedMatchReachable();

    // Audit F37 / F47 / F50.
    void anUninstalledEngineIsListedButUnclickable();
    void publishesHowManyAgentsAreWaiting();
    void aCollapsedProjectStaysCollapsedNextSession();
    void aTagFilterSurvivesAndAppliesOnceItsTagAppears();

    // Audit F43: a rate-limited agent has to be visible from OUTSIDE its own
    // panel, and stop being visible once the fleet is back under its quota.
    void showsWhenTheFleetIsPausedByAUsageLimit();

private:
    QTreeWidgetItem *projectRow() const { return m_tree->topLevelItem(0); }
    QTreeWidgetItem *rowFor(int agentId) const;

    AgentRoster *m_roster = nullptr;
    QTreeWidget *m_tree = nullptr;
};

void AgentRosterTest::initTestCase()
{
    // The roster now reads and writes [View] state; never touch the
    // developer's own agentkaterc.
    QStandardPaths::setTestModeEnabled(true);
}

void AgentRosterTest::init()
{
    m_roster = new AgentRoster;
    m_tree = m_roster->findChild<QTreeWidget *>();
    QVERIFY(m_tree);
    m_roster->addProject(QStringLiteral("/p"), QStringLiteral("proj"));
    m_roster->addAgent(QStringLiteral("/p"), 1, QStringLiteral("Controller"));
    m_roster->addAgent(QStringLiteral("/p"), 2, QStringLiteral("Worker A"));
    m_roster->addAgent(QStringLiteral("/p"), 3, QStringLiteral("Worker B"));
}

void AgentRosterTest::cleanup()
{
    delete m_roster;
    m_roster = nullptr;
    m_tree = nullptr;
}

// Depth-first search for an agent row, mirroring what the roster does
// internally — the test must not assume the depth it is verifying.
QTreeWidgetItem *AgentRosterTest::rowFor(int agentId) const
{
    QList<QTreeWidgetItem *> stack;
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        stack.append(m_tree->topLevelItem(i));
    }
    while (!stack.isEmpty()) {
        QTreeWidgetItem *item = stack.takeLast();
        for (int i = 0; i < item->childCount(); ++i) {
            stack.append(item->child(i));
        }
        if (item->parent() && item->data(0, Qt::UserRole).toInt() == agentId) {
            return item;
        }
    }
    return nullptr;
}

void AgentRosterTest::nestsWorkerUnderController()
{
    QCOMPARE(projectRow()->childCount(), 3);
    m_roster->setAgentParent(2, 1);
    m_roster->setAgentParent(3, 1);
    QCOMPARE(projectRow()->childCount(), 1);
    QTreeWidgetItem *controller = projectRow()->child(0);
    QCOMPARE(controller->data(0, Qt::UserRole).toInt(), 1);
    QCOMPARE(controller->childCount(), 2);
    // Every agent stays findable at its new depth.
    QVERIFY(rowFor(2));
    QVERIFY(rowFor(3));
    QCOMPARE(rowFor(2)->parent(), controller);
}

void AgentRosterTest::countsOnlyLiveWorkers()
{
    m_roster->setAgentRole(1, QStringLiteral("controller"));
    m_roster->setAgentParent(2, 1);
    m_roster->setAgentParent(3, 1);
    QCOMPARE(rowFor(1)->data(0, AgentRoles::WorkerCount).toInt(), 2);
    // A dormant worker is not running under anybody.
    m_roster->setAgentDormant(3, true);
    QCOMPARE(rowFor(1)->data(0, AgentRoles::WorkerCount).toInt(), 1);
    m_roster->setAgentDormant(3, false);
    QCOMPARE(rowFor(1)->data(0, AgentRoles::WorkerCount).toInt(), 2);
    // Removing one re-counts too.
    m_roster->removeAgent(3);
    QCOMPARE(rowFor(1)->data(0, AgentRoles::WorkerCount).toInt(), 1);
}

void AgentRosterTest::closingAControllerOrphansItsWorkers()
{
    m_roster->setAgentParent(2, 1);
    m_roster->setAgentParent(3, 1);
    m_roster->removeAgent(1); // the human closes the controller

    QVERIFY2(rowFor(2), "a worker row was deleted with its controller");
    QVERIFY2(rowFor(3), "a worker row was deleted with its controller");
    QCOMPARE(rowFor(2)->parent(), projectRow());
    QCOMPARE(projectRow()->childCount(), 2);
}

void AgentRosterTest::unknownParentKeepsWorkerAtProjectLevel()
{
    m_roster->setAgentParent(2, 1);
    // -1 = "no controller row here" (launched by an agent this window never
    // saw, or one that has been archived). The worker must not vanish.
    m_roster->setAgentParent(2, -1);
    QCOMPARE(rowFor(2)->parent(), projectRow());
    // An id that is not in the roster at all behaves the same way.
    m_roster->setAgentParent(2, 99);
    QVERIFY(rowFor(2));
    QCOMPARE(rowFor(2)->parent(), projectRow());
}

void AgentRosterTest::refusesCycles()
{
    m_roster->setAgentParent(2, 1); // worker under controller
    m_roster->setAgentParent(1, 2); // …and its controller under it: a cycle
    // The controller stays where it was; nothing is detached or leaked.
    QVERIFY(rowFor(1));
    QVERIFY(rowFor(2));
    QCOMPARE(rowFor(1)->parent(), projectRow());
    QCOMPARE(rowFor(2)->parent(), rowFor(1));
    // Self-parenting is refused too.
    m_roster->setAgentParent(1, 1);
    QCOMPARE(rowFor(1)->parent(), projectRow());
}

void AgentRosterTest::reparentKeepsSelection()
{
    m_roster->setCurrentAgent(2);
    QCOMPARE(m_tree->currentItem(), rowFor(2));
    m_roster->setAgentParent(2, 1);
    QCOMPARE(m_tree->currentItem(), rowFor(2));
}

void AgentRosterTest::filterKeepsNestedMatchReachable()
{
    m_roster->setAgentParent(2, 1);
    m_roster->setAgentParent(3, 1);
    QLineEdit *filter = m_roster->findChild<QLineEdit *>();
    QVERIFY(filter);
    filter->setText(QStringLiteral("Worker A"));

    QVERIFY2(!rowFor(2)->isHidden(), "the matching worker is hidden");
    QVERIFY2(!rowFor(1)->isHidden(),
             "the controller is hidden, so Qt hides the matching worker with it");
    QVERIFY2(rowFor(3)->isHidden(), "a non-matching worker is still shown");
    QVERIFY(!projectRow()->isHidden());

    filter->setText(QString());
    QVERIFY(!rowFor(3)->isHidden());
}

// Audit F37: the "+ New Agent" dropdown listed every engine unconditionally,
// including ones whose command-line program is not installed — a choice whose
// only possible outcome is "executable file not found", after the user has
// already written a task.
void AgentRosterTest::anUninstalledEngineIsListedButUnclickable()
{
    QList<EngineChoice> choices;
    EngineChoice liveHeader;
    liveHeader.label = QStringLiteral("Claude Code");
    liveHeader.header = true;
    choices << liveHeader;
    EngineChoice live;
    live.backend = QStringLiteral("claude");
    live.label = QStringLiteral("Claude Code (default model)");
    choices << live;
    EngineChoice deadHeader;
    deadHeader.label = QStringLiteral("Kimi Code — not installed");
    deadHeader.header = true;
    deadHeader.available = false;
    choices << deadHeader;
    EngineChoice dead;
    dead.backend = QStringLiteral("kimi");
    dead.label = QStringLiteral("Kimi Code (default model)");
    dead.available = false;
    choices << dead;
    m_roster->setEngineChoices(choices);

    auto *newButton =
        m_roster->findChild<QToolButton *>(QStringLiteral("newAgentButton"));
    QVERIFY(newButton);
    QVERIFY(newButton->menu());

    QAction *liveAct = nullptr;
    QAction *deadAct = nullptr;
    const auto actions = newButton->menu()->actions();
    for (QAction *a : actions) {
        if (a->text() == live.label) {
            liveAct = a;
        }
        if (a->text() == dead.label) {
            deadAct = a;
        }
    }
    QVERIFY2(liveAct, "an installed engine lost its menu entry");
    QVERIFY(liveAct->isEnabled());
    QVERIFY2(deadAct, "a missing engine was hidden instead of marked");
    QVERIFY2(!deadAct->isEnabled(), "a dead engine choice is still clickable");
}

// Audit F50: a blocked agent produced no signal outside the roster, so a missed
// popup plus a window on another virtual desktop equalled silence. The count is
// the RAW truth, not the painted marker (which is suppressed on the selected
// row), or selecting the blocked agent would zero the task-bar signal while the
// agent is still waiting.
void AgentRosterTest::publishesHowManyAgentsAreWaiting()
{
    QSignalSpy spy(m_roster, &AgentRoster::attentionCountChanged);
    QCOMPARE(m_roster->attentionCount(), 0);

    m_roster->setAgentAttention(2, true);
    QCOMPARE(m_roster->attentionCount(), 1);
    QCOMPARE(spy.count(), 1);
    QCOMPARE(spy.takeLast().at(0).toInt(), 1);

    m_roster->setAgentAttention(3, true);
    QCOMPARE(m_roster->attentionCount(), 2);
    QCOMPARE(spy.count(), 1);

    // Looking at a blocked agent hides its marker but does not answer it.
    m_roster->setCurrentAgent(2);
    QCOMPARE(m_roster->attentionCount(), 2);

    m_roster->setAgentAttention(2, false);
    m_roster->setAgentAttention(3, false);
    QCOMPARE(m_roster->attentionCount(), 0);
    QCOMPARE(spy.takeLast().at(0).toInt(), 0);
}

// Audit F47: roster expand state was session-only, so every launch re-opened
// project rows the user had deliberately collapsed.
void AgentRosterTest::aCollapsedProjectStaysCollapsedNextSession()
{
    projectRow()->setExpanded(false);
    QVERIFY(!projectRow()->isExpanded());

    // A fresh roster, as a relaunch would build one.
    AgentRoster next;
    auto *tree = next.findChild<QTreeWidget *>();
    QVERIFY(tree);
    next.addProject(QStringLiteral("/p"), QStringLiteral("proj"));
    QVERIFY2(!tree->topLevelItem(0)->isExpanded(),
             "the collapsed project came back expanded");
    // …and a restored agent landing in it must not silently undo that.
    next.addAgent(QStringLiteral("/p"), 1, QStringLiteral("Restored"));
    QVERIFY2(!tree->topLevelItem(0)->isExpanded(),
             "the first restored agent re-expanded a collapsed project");

    projectRow()->setExpanded(true); // leave the config clean for the next test
}

// Audit F47: the tag filter was session-only too. The subtle part is that the
// roster is EMPTY when it restores, so a naive restore is erased by the filter
// menu's own prune-departed-tags pass before any agent arrives.
void AgentRosterTest::aTagFilterSurvivesAndAppliesOnceItsTagAppears()
{
    KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .writeEntry("rosterTagFilter", QStringList{QStringLiteral("backend")});

    AgentRoster next;
    auto *tree = next.findChild<QTreeWidget *>();
    QVERIFY(tree);
    next.addProject(QStringLiteral("/q"), QStringLiteral("q"));
    next.addAgent(QStringLiteral("/q"), 10, QStringLiteral("Backend agent"));
    next.addAgent(QStringLiteral("/q"), 11, QStringLiteral("Frontend agent"));
    next.setAgentTags(10, {QStringLiteral("backend")});
    next.setAgentTags(11, {QStringLiteral("ui")});

    QTreeWidgetItem *project = tree->topLevelItem(0);
    QVERIFY(project);
    QTreeWidgetItem *tagged = nullptr;
    QTreeWidgetItem *other = nullptr;
    for (int i = 0; i < project->childCount(); ++i) {
        QTreeWidgetItem *row = project->child(i);
        if (row->data(0, Qt::UserRole).toInt() == 10) {
            tagged = row;
        } else {
            other = row;
        }
    }
    QVERIFY(tagged);
    QVERIFY(other);
    QVERIFY2(!tagged->isHidden(), "the restored filter hid its own match");
    QVERIFY2(other->isHidden(), "the restored tag filter was not applied at all");

    KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .writeEntry("rosterTagFilter", QStringList());
}

// Audit F43. The whole complaint was that a parked agent was invisible unless
// you opened it, so this asserts the roster — the thing on screen while five
// agents run — actually says so, and stops saying so when it stops being true.
void AgentRosterTest::showsWhenTheFleetIsPausedByAUsageLimit()
{
    agentkate::RateLimitState::setDesktopAlertsEnabled(false);
    agentkate::RateLimitState *state = agentkate::RateLimitState::self();
    state->forget(QStringLiteral("thread-a"));

    // Find the strip by its content, not by an object name the roster could
    // rename: it is the only label that is hidden while nothing is limited.
    const auto strip = [this]() -> QLabel * {
        const QList<QLabel *> labels = m_roster->findChildren<QLabel *>();
        for (QLabel *l : labels) {
            if (l->text().contains(QStringLiteral("usage limit"))) {
                return l;
            }
        }
        return nullptr;
    };

    QVERIFY2(!strip(), "the roster claimed a usage limit before any was reported");

    const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(900);
    state->report(QStringLiteral("thread-a"),
                  agentkate::RateLimitReport{QStringLiteral("rejected"),
                                             QStringLiteral("five_hour"), resets,
                                             false});
    QLabel *shown = strip();
    QVERIFY2(shown, "a rate-limited agent left no trace anywhere in the roster");
    QVERIFY(shown->isVisibleTo(m_roster));
    // The reset time is the actionable half — "paused" without "until when" is
    // the same dead end the panel-only chip already was.
    QVERIFY(shown->text().contains(
        QLocale().toString(resets.toLocalTime().time(), QLocale::ShortFormat)));

    // Back under the limit: the strip goes away rather than lingering as a
    // permanent scare.
    state->report(QStringLiteral("thread-a"),
                  agentkate::RateLimitReport{QStringLiteral("allowed"),
                                             QStringLiteral("five_hour"), resets,
                                             false});
    QVERIFY2(!strip() || !shown->isVisibleTo(m_roster),
             "the usage-limit strip outlived the usage limit");
    state->forget(QStringLiteral("thread-a"));
}

QTEST_MAIN(AgentRosterTest)
#include "AgentRosterTest.moc"
