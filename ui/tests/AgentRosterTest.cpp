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

#include <QLineEdit>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QtTest>

class AgentRosterTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void init();
    void cleanup();

    void nestsWorkerUnderController();
    void countsOnlyLiveWorkers();
    void closingAControllerOrphansItsWorkers();
    void unknownParentKeepsWorkerAtProjectLevel();
    void refusesCycles();
    void reparentKeepsSelection();
    void filterKeepsNestedMatchReachable();

private:
    QTreeWidgetItem *projectRow() const { return m_tree->topLevelItem(0); }
    QTreeWidgetItem *rowFor(int agentId) const;

    AgentRoster *m_roster = nullptr;
    QTreeWidget *m_tree = nullptr;
};

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

QTEST_MAIN(AgentRosterTest)
#include "AgentRosterTest.moc"
