// Plan 19: the Jobs panel is the first surface that shows background work from
// agents other than the one you are looking at, and it owns none of that work —
// each agent publishes a full snapshot and the panel replaces that agent's rows
// wholesale. That design has exactly three ways to go wrong, and all three lose
// or invent work rather than merely mis-drawing it:
//
//   * a snapshot for one agent clobbering another's rows,
//   * rows outliving the agent that owns them (a closed agent publishes an
//     empty set — if that doesn't reap, the panel shows dead jobs forever), and
//   * a filter hiding a job under a collapsed/filtered-out agent group, the
//     same class of bug plan 16 P5 hit in the roster.
//
// These drive the real widget through its public API and assert the tree a user
// would see.

#include "JobsPanel.h"

#include <QComboBox>
#include <QLineEdit>
#include <QPushButton>
#include <QTreeWidget>
#include <QtTest>

using agentkate::AgentJob;

namespace
{
AgentJob job(const QString &id, const QString &desc, bool done,
             AgentJob::Kind kind = AgentJob::Kind::Shell)
{
    AgentJob j;
    j.id = id;
    j.description = desc;
    j.done = done;
    j.kind = kind;
    j.startedMs = 1000;
    j.outputFile = QStringLiteral("/tmp/%1.log").arg(id);
    return j;
}
} // namespace

class JobsPanelTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void init();
    void cleanup();

    void groupsJobsByAgent();
    void snapshotReplacesOnlyThatAgent();
    void emptySnapshotReapsTheAgent();
    void stateFilterHidesGroupWithNoMatches();
    void textFilterMatchesAgentName();
    void clearFinishedIsRoutedNotLocal();
    void openDispatchesByRowKind();
    void groupRowIsNotOpenable();
    void jobWithNoIdIsStillAJobRow();

private:
    QTreeWidget *tree() const { return m_panel->findChild<QTreeWidget *>(); }
    QComboBox *stateFilter() const { return m_panel->findChild<QComboBox *>(); }
    QLineEdit *textFilter() const { return m_panel->findChild<QLineEdit *>(); }
    // Toolbar buttons by label — the panel exposes no handles to them.
    QPushButton *button(const QString &label) const;
    // Group row for an agent by its displayed title, or nullptr.
    QTreeWidgetItem *groupFor(const QString &title) const;

    JobsPanel *m_panel = nullptr;
};

void JobsPanelTest::init()
{
    m_panel = new JobsPanel;
    m_panel->setAgentTitles({{QStringLiteral("t1"), QStringLiteral("Builder")},
                             {QStringLiteral("t2"), QStringLiteral("Reviewer")}});
}

void JobsPanelTest::cleanup()
{
    delete m_panel;
    m_panel = nullptr;
}

QPushButton *JobsPanelTest::button(const QString &label) const
{
    const auto buttons = m_panel->findChildren<QPushButton *>();
    for (QPushButton *b : buttons) {
        if (b->text().contains(label)) {
            return b;
        }
    }
    return nullptr;
}

QTreeWidgetItem *JobsPanelTest::groupFor(const QString &title) const
{
    for (int i = 0; i < tree()->topLevelItemCount(); ++i) {
        if (tree()->topLevelItem(i)->text(0) == title) {
            return tree()->topLevelItem(i);
        }
    }
    return nullptr;
}

void JobsPanelTest::groupsJobsByAgent()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), false)});
    m_panel->setAgentJobs(QStringLiteral("t2"),
                          {job(QStringLiteral("b"), QStringLiteral("review"), false),
                           job(QStringLiteral("c"), QStringLiteral("lint"), true)});

    QCOMPARE(tree()->topLevelItemCount(), 2);
    QVERIFY(groupFor(QStringLiteral("Builder")) != nullptr);
    QCOMPARE(groupFor(QStringLiteral("Builder"))->childCount(), 1);
    QCOMPARE(groupFor(QStringLiteral("Reviewer"))->childCount(), 2);
    // Running sorts above finished within a group, so live work is never
    // pushed below a pile of completed jobs.
    QCOMPARE(groupFor(QStringLiteral("Reviewer"))->child(0)->text(0), QStringLiteral("review"));
}

void JobsPanelTest::snapshotReplacesOnlyThatAgent()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), false)});
    m_panel->setAgentJobs(QStringLiteral("t2"),
                          {job(QStringLiteral("b"), QStringLiteral("review"), false)});
    // A fresh snapshot for t1 must not disturb t2's rows.
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), true),
                           job(QStringLiteral("d"), QStringLiteral("test"), false)});

    QCOMPARE(groupFor(QStringLiteral("Builder"))->childCount(), 2);
    QCOMPARE(groupFor(QStringLiteral("Reviewer"))->childCount(), 1);
    QCOMPARE(groupFor(QStringLiteral("Reviewer"))->child(0)->text(0), QStringLiteral("review"));
}

void JobsPanelTest::emptySnapshotReapsTheAgent()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), false)});
    m_panel->setAgentJobs(QStringLiteral("t2"),
                          {job(QStringLiteral("b"), QStringLiteral("review"), false)});
    QCOMPARE(tree()->topLevelItemCount(), 2);

    // How a closing agent's rows are reaped — they must not outlive it.
    m_panel->setAgentJobs(QStringLiteral("t1"), {});
    QCOMPARE(tree()->topLevelItemCount(), 1);
    QVERIFY(groupFor(QStringLiteral("Builder")) == nullptr);
    QVERIFY(groupFor(QStringLiteral("Reviewer")) != nullptr);
}

void JobsPanelTest::stateFilterHidesGroupWithNoMatches()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), true)});
    m_panel->setAgentJobs(QStringLiteral("t2"),
                          {job(QStringLiteral("b"), QStringLiteral("review"), false)});

    stateFilter()->setCurrentIndex(1); // Running
    QCOMPARE(tree()->topLevelItemCount(), 1);
    QVERIFY(groupFor(QStringLiteral("Reviewer")) != nullptr);

    stateFilter()->setCurrentIndex(2); // Finished
    QCOMPARE(tree()->topLevelItemCount(), 1);
    QVERIFY(groupFor(QStringLiteral("Builder")) != nullptr);

    stateFilter()->setCurrentIndex(0); // All
    QCOMPARE(tree()->topLevelItemCount(), 2);
}

void JobsPanelTest::textFilterMatchesAgentName()
{
    // "npm ci" is deliberately a needle NO agent title contains, so the
    // description half of the filter cannot pass by matching "Builder".
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("npm ci"), false)});
    m_panel->setAgentJobs(QStringLiteral("t2"),
                          {job(QStringLiteral("b"), QStringLiteral("review"), false)});

    // Filtering by the AGENT's name keeps all of that agent's jobs — otherwise
    // "show me what Reviewer is doing" silently returns nothing.
    textFilter()->setText(QStringLiteral("Reviewer"));
    QCOMPARE(tree()->topLevelItemCount(), 1);
    QCOMPARE(groupFor(QStringLiteral("Reviewer"))->childCount(), 1);

    // And a job description still matches, hiding every agent with no match.
    textFilter()->setText(QStringLiteral("npm ci"));
    QCOMPARE(tree()->topLevelItemCount(), 1);
    QVERIFY(groupFor(QStringLiteral("Builder")) != nullptr);
    QVERIFY(groupFor(QStringLiteral("Reviewer")) == nullptr);
}

void JobsPanelTest::clearFinishedIsRoutedNotLocal()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), true)});
    QSignalSpy spy(m_panel, &JobsPanel::clearFinishedRequested);

    QPushButton *clear = button(QStringLiteral("Clear"));
    QVERIFY(clear != nullptr);
    QVERIFY(clear->isEnabled()); // there IS finished work to clear
    clear->click();

    // The panel mirrors the agents' state, so clearing must be a request, not a
    // local edit — a local clear would be undone by the next snapshot.
    QCOMPARE(spy.count(), 1);
    QCOMPARE(groupFor(QStringLiteral("Builder"))->childCount(), 1);
}

// Open is the one action whose destination depends on the row: a shell log goes
// to the window's editor, a workflow back to the agent that launched it, and a
// sub-agent transcript is drawn by the panel itself. Routing a row to the wrong
// one of those is silent — you get an empty editor tab or nothing at all.
void JobsPanelTest::openDispatchesByRowKind()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), false,
                               AgentJob::Kind::Shell)});
    m_panel->setAgentJobs(QStringLiteral("t2"),
                          {job(QStringLiteral("w"), QStringLiteral("ship it"), false,
                               AgentJob::Kind::Workflow)});

    QSignalSpy files(m_panel, &JobsPanel::openFileRequested);
    QSignalSpy flows(m_panel, &JobsPanel::openWorkflowRequested);

    tree()->setCurrentItem(groupFor(QStringLiteral("Builder"))->child(0));
    QVERIFY(button(QStringLiteral("Open"))->isEnabled());
    button(QStringLiteral("Open"))->click();
    QCOMPARE(files.count(), 1);
    // The owning thread must ride along: the panel shows other agents' work, and
    // the window scopes editor tabs per agent.
    QCOMPARE(files.at(0).at(0).toString(), QStringLiteral("t1"));
    QCOMPARE(files.at(0).at(1).toString(), QStringLiteral("/tmp/a.log"));
    QCOMPARE(flows.count(), 0);

    tree()->setCurrentItem(groupFor(QStringLiteral("Reviewer"))->child(0));
    button(QStringLiteral("Open"))->click();
    QCOMPARE(flows.count(), 1);
    QCOMPARE(flows.at(0).at(0).toString(), QStringLiteral("t2"));
    QCOMPARE(files.count(), 1); // a workflow never routes to the editor
}

void JobsPanelTest::groupRowIsNotOpenable()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QStringLiteral("a"), QStringLiteral("build"), false)});
    QSignalSpy files(m_panel, &JobsPanel::openFileRequested);
    QSignalSpy gone(m_panel, &JobsPanel::goToAgentRequested);

    tree()->setCurrentItem(groupFor(QStringLiteral("Builder")));
    QVERIFY(!button(QStringLiteral("Open"))->isEnabled());
    // An agent header is still a place to jump to that agent from.
    QVERIFY(button(QStringLiteral("Go to agent"))->isEnabled());
    button(QStringLiteral("Go to agent"))->click();
    QCOMPARE(gone.count(), 1);
    QCOMPARE(gone.at(0).at(0).toString(), QStringLiteral("t1"));
    QCOMPARE(files.count(), 0);
}

// A job whose CLI id never arrived is still a job: identity comes from an
// explicit row role, not from "has a non-empty id", or the row would render as
// an inert pseudo-header with Open greyed out.
void JobsPanelTest::jobWithNoIdIsStillAJobRow()
{
    m_panel->setAgentJobs(QStringLiteral("t1"),
                          {job(QString(), QStringLiteral("build"), false)});
    QSignalSpy files(m_panel, &JobsPanel::openFileRequested);

    QTreeWidgetItem *row = groupFor(QStringLiteral("Builder"))->child(0);
    QVERIFY(row != nullptr);
    tree()->setCurrentItem(row);
    QVERIFY(button(QStringLiteral("Open"))->isEnabled());
    button(QStringLiteral("Open"))->click();
    QCOMPARE(files.count(), 1);
    QCOMPARE(files.at(0).at(0).toString(), QStringLiteral("t1"));
}

QTEST_MAIN(JobsPanelTest)
#include "JobsPanelTest.moc"
