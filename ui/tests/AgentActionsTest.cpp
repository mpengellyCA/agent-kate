// The per-agent action gates, and the labels that describe what those actions
// destroy or open.
//
// Why this file exists at all: MainWindow.cpp, AgentDock.cpp and
// AgentRoster.cpp are compiled by exactly one target (the application), so
// nothing in the suite could see a guard that lived in them. A verifier deleted
// every enablement gate in this cluster and the suite stayed 23/23 green. The
// decision now lives in AgentActions.h and the roster's menu is built by a
// function that can be called without exec()ing a modal, so both are pinned
// here — the pure decision AND the primary path that consumes it.
//
// The two properties under test are the ones users get hurt by when they slip:
//
//   * "Create pull request…" / "Merge the agent's changes…" need a PRIVATE
//     BRANCH, not merely a working directory. The core refuses both outright
//     for a workspace-mode thread, so offering them can only end in an error
//     after the user has typed a PR title.
//   * A destructive action's confirmation must be true in every state the
//     mechanism can reach. agent.discard removes nothing from the user's own
//     checkout when the agent is not isolated (worktree.Remove returns early),
//     and removes six distinct things when it is.

#include "AgentActions.h"
#include "AgentRoster.h"

#include <QAction>
#include <QMenu>
#include <QtTest>

class AgentActionsTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void pullRequestAndMergeNeedAPrivateBranch();
    void aWorkspaceAgentStillGetsTheEverydayActions();
    void aNeverStartedAgentHasNothingToDiscard();
    void theRosterMenuIsGatedNotJustTheMenuBar();
    void theTerminalActionSaysWhichFolderItOpens();
    void theDiscardPromptNamesWhatItDestroys();
    void theDiscardPromptDoesNotThreatenTheUsersOwnFiles();
};

// The isolation term itself. Delete it (e.g. gate pullRequest/merge on hasPath)
// and this fails: a workspace agent, which has a path but no branch, would be
// offered both.
void AgentActionsTest::pullRequestAndMergeNeedAPrivateBranch()
{
    const auto workspace = AgentActions::compute(
        /*exists=*/true, /*running=*/true, /*dormant=*/false,
        /*isolated=*/false, /*hasPath=*/true);
    QVERIFY2(!workspace.pullRequest,
             "PR offered on an agent with no branch to open one from");
    QVERIFY2(!workspace.merge,
             "Merge offered on an agent with no branch to merge");

    const auto isolated = AgentActions::compute(
        /*exists=*/true, /*running=*/true, /*dormant=*/false,
        /*isolated=*/true, /*hasPath=*/true);
    QVERIFY2(isolated.pullRequest, "PR refused on an isolated agent");
    QVERIFY2(isolated.merge, "Merge refused on an isolated agent");
}

// The gate must not overshoot: a workspace agent is a normal, supported way to
// run, and everything that does not need a branch stays available to it.
void AgentActionsTest::aWorkspaceAgentStillGetsTheEverydayActions()
{
    const auto e = AgentActions::compute(
        /*exists=*/true, /*running=*/true, /*dormant=*/false,
        /*isolated=*/false, /*hasPath=*/true);
    QVERIFY(e.rename);
    QVERIFY(e.changes);
    QVERIFY(e.stop);
    QVERIFY(e.tags);
    QVERIFY(e.close);
    QVERIFY2(e.commit, "a workspace agent cannot commit its own work");
    QVERIFY2(e.terminal,
             "the terminal action was disabled rather than renamed");
    QVERIFY2(e.discard, "a started workspace agent cannot be deleted");
}

void AgentActionsTest::aNeverStartedAgentHasNothingToDiscard()
{
    const auto e = AgentActions::compute(
        /*exists=*/true, /*running=*/false, /*dormant=*/false,
        /*isolated=*/false, /*hasPath=*/false);
    QVERIFY2(!e.discard, "Discard offered on an agent with nothing to discard");
    QVERIFY2(!e.commit, "Commit offered on an agent with no working directory");
    QVERIFY2(!e.terminal, "a terminal was offered in a directory that is not known");
    QVERIFY2(e.close, "Close — the action that DOES apply — was disabled");
}

// The primary path. The previous round wired canLand() to the menu bar only;
// the roster's right-click menu, which is how most users reach these, kept
// offering both. This drives the real widget and asserts the built menu.
void AgentActionsTest::theRosterMenuIsGatedNotJustTheMenuBar()
{
    AgentRoster roster;
    roster.addProject(QStringLiteral("/p"), QStringLiteral("proj"));
    roster.addAgent(QStringLiteral("/p"), 1, QStringLiteral("Agent 1"));
    roster.setAgentHasWorktreePath(1, true);
    roster.setAgentIsolated(1, false);

    AgentRowMenu m = roster.buildAgentRowMenu(1, &roster);
    QVERIFY(m.menu);
    QVERIFY(m.action(QStringLiteral("pr")));
    QVERIFY(m.action(QStringLiteral("merge")));
    QVERIFY2(!m.action(QStringLiteral("pr"))->isEnabled(),
             "the roster still offers a PR on a workspace agent");
    QVERIFY2(!m.action(QStringLiteral("merge"))->isEnabled(),
             "the roster still offers Merge on a workspace agent");
    // …and says why, rather than greying out with no explanation.
    QVERIFY(!m.action(QStringLiteral("merge"))->toolTip().isEmpty());
    QVERIFY2(m.action(QStringLiteral("commit"))->isEnabled(),
             "the gate overshot: a workspace agent cannot commit");
    delete m.menu;

    roster.setAgentIsolated(1, true);
    AgentRowMenu iso = roster.buildAgentRowMenu(1, &roster);
    QVERIFY(iso.menu);
    QVERIFY2(iso.action(QStringLiteral("pr"))->isEnabled(),
             "an isolated agent lost its PR action");
    QVERIFY2(iso.action(QStringLiteral("merge"))->isEnabled(),
             "an isolated agent lost its Merge action");
    delete iso.menu;
}

// "Open Terminal in Worktree" opened the user's own checkout for a workspace
// agent. The fix is the NAME, not the enablement — the action is useful either
// way, so a fix that disabled it would be its own regression.
void AgentActionsTest::theTerminalActionSaysWhichFolderItOpens()
{
    const QString isolated = AgentActions::terminalActionLabel(true);
    const QString workspace = AgentActions::terminalActionLabel(false);
    QVERIFY2(isolated != workspace,
             "the terminal action is called the same thing in both states");
    QVERIFY2(isolated.contains(QStringLiteral("Worktree")),
             "the isolated label stopped naming the worktree");
    QVERIFY2(!workspace.contains(QStringLiteral("Worktree")),
             "the workspace label still claims a worktree the agent has not got");
    QVERIFY(AgentActions::terminalActionTooltip(true)
            != AgentActions::terminalActionTooltip(false));

    AgentRoster roster;
    roster.addProject(QStringLiteral("/p"), QStringLiteral("proj"));
    roster.addAgent(QStringLiteral("/p"), 1, QStringLiteral("Agent 1"));
    roster.setAgentHasWorktreePath(1, true);
    roster.setAgentIsolated(1, false);
    AgentRowMenu m = roster.buildAgentRowMenu(1, &roster);
    QVERIFY(m.menu);
    QAction *term = m.action(QStringLiteral("terminal"));
    QVERIFY(term);
    QCOMPARE(term->text(), workspace);
    QVERIFY2(term->isEnabled(),
             "the terminal action was disabled instead of renamed");
    delete m.menu;
}

// agent.discard removes the worktree AND the branch AND the session record AND
// the transcript AND the attachment sidecar AND every approval grant. The old
// prompt ("this agent's worktree and all of its uncommitted changes") named one
// of six.
void AgentActionsTest::theDiscardPromptNamesWhatItDestroys()
{
    const WorktreeCopy::DiscardPrompt p = AgentActions::agentDiscardPrompt(
        /*isolated=*/true, QStringLiteral("Refactor the parser"),
        QStringLiteral("/home/u/.cache/ak/wt-3"));
    QVERIFY2(p.body.contains(QStringLiteral("Refactor the parser")),
             "the prompt does not say which agent is about to be deleted");
    QVERIFY2(p.body.contains(QStringLiteral("/home/u/.cache/ak/wt-3")),
             "the prompt does not name the directory it removes");
    QVERIFY2(p.body.contains(QStringLiteral("branch")),
             "the prompt does not mention the branch it deletes");
    QVERIFY2(p.body.contains(QStringLiteral("conversation")),
             "the prompt does not mention the conversation it throws away");
    QVERIFY2(p.body.contains(QStringLiteral("permission")),
             "the prompt does not mention the grants it revokes");
    QVERIFY2(!p.confirmLabel.isEmpty()
                 && !p.confirmLabel.contains(QStringLiteral("Yes")),
             "the destructive button does not spell out what it does");
    QVERIFY(!p.title.isEmpty());
}

// The other direction, and the more surprising one: for a NON-isolated agent
// worktree.Remove() returns immediately, so nothing in the user's checkout is
// touched. A prompt that says their uncommitted changes are about to go is
// both false and frightening — it invites cancelling a safe operation.
void AgentActionsTest::theDiscardPromptDoesNotThreatenTheUsersOwnFiles()
{
    const WorktreeCopy::DiscardPrompt ws = AgentActions::agentDiscardPrompt(
        /*isolated=*/false, QStringLiteral("Fix the CSV import"),
        QStringLiteral("/home/u/project"));
    const WorktreeCopy::DiscardPrompt iso = AgentActions::agentDiscardPrompt(
        /*isolated=*/true, QStringLiteral("Fix the CSV import"),
        QStringLiteral("/home/u/project"));
    QVERIFY2(ws.body != iso.body,
             "the same sentence is shown whether or not the agent is isolated");
    QVERIFY2(ws.body.contains(QStringLiteral("your files are left exactly as "
                                             "they are"),
                              Qt::CaseInsensitive),
             "the workspace prompt does not say the user's files survive");
    QVERIFY2(!ws.body.contains(QStringLiteral("uncommitted")),
             "the workspace prompt still claims uncommitted work is destroyed");
    QVERIFY2(ws.body.contains(QStringLiteral("conversation"))
                 && ws.body.contains(QStringLiteral("permission")),
             "the workspace prompt does not say what IS destroyed");
    // The isolated one is the one that may promise nothing of the user's goes.
    QVERIFY(iso.body.contains(QStringLiteral("Your own checkout is not touched")));
    QVERIFY(ws.confirmLabel != iso.confirmLabel);
}

QTEST_MAIN(AgentActionsTest)
#include "AgentActionsTest.moc"
