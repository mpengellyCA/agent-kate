// Audit F29 / F50: the Worktree Dashboard's discard confirmation is the one
// sentence in the app whose being wrong costs the user their work.
//
// The dashboard lists non-isolated ("workspace mode") agents beside isolated
// ones, and for those rows the dirty count is the porcelain status of the
// user's REAL checkout — their own uncommitted edits included. The prompt used
// to say "Permanently discard all N uncommitted changes in worktree #3" for
// both, framing an operation on the user's whole project as agent-scoped, and
// the core then ran `git reset --hard` + `git clean -fd` there.
//
// These tests pin the properties that make the sentence true, each of which
// fails if the isolation branch is inverted or dropped.

#include "WorktreeDashboard.h"

#include <QtTest>

class WorktreeCopyTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void isolatedPromptNamesTheWorktree();
    void workspacePromptNeverSaysWorktreeNumber();
    void workspacePromptNamesTheRealPath();
    void workspacePromptDisclosesTheUsersOwnWork();
    void workspacePromptEscapesThePath();
    void confirmLabelsAreNamedActionsAndDiffer();
    void pluralFormsAgreeWithTheCount();
    void notIsolatedMarkersAreNonEmptyAndDistinct();
};

// The isolated row keeps the agent-scoped framing, because there it is true.
void WorktreeCopyTest::isolatedPromptNamesTheWorktree()
{
    const auto p = WorktreeCopy::discardPrompt(
        /*isolated*/ true, 3, QStringLiteral("agentkate/t-1"),
        QStringLiteral("/home/u/proj/.agentkate/worktrees/t-1"), 4);
    QVERIFY(p.body.contains(QStringLiteral("#3")));
    QVERIFY(p.body.contains(QStringLiteral("worktree")));
    // Nothing of the user's is at stake, so the prompt must not claim otherwise.
    QVERIFY(!p.body.contains(QStringLiteral("yourself")));
}

// The whole finding in one assertion: a workspace row is not a worktree, and
// "#3" is exactly the phrasing that made the user think it was.
void WorktreeCopyTest::workspacePromptNeverSaysWorktreeNumber()
{
    const auto p = WorktreeCopy::discardPrompt(
        /*isolated*/ false, 3, QStringLiteral("main"),
        QStringLiteral("/home/u/proj"), 4);
    QVERIFY(!p.body.contains(QStringLiteral("#3")));
    QVERIFY(!p.body.contains(QStringLiteral("worktree")));
    QVERIFY(!p.title.contains(QStringLiteral("worktree")));
}

void WorktreeCopyTest::workspacePromptNamesTheRealPath()
{
    const auto p = WorktreeCopy::discardPrompt(
        /*isolated*/ false, 3, QStringLiteral("main"),
        QStringLiteral("/home/u/proj"), 4);
    QVERIFY(p.body.contains(QStringLiteral("/home/u/proj")));
}

// "Including anything you changed yourself" is the fact the old prompt hid.
void WorktreeCopyTest::workspacePromptDisclosesTheUsersOwnWork()
{
    const auto p = WorktreeCopy::discardPrompt(
        /*isolated*/ false, 0, QStringLiteral("main"),
        QStringLiteral("/home/u/proj"), 4);
    QVERIFY(p.body.contains(QStringLiteral("yourself")));
    QVERIFY(p.body.contains(QStringLiteral("directly in your own")));
}

// The body is shown as rich text, so a path with markup in it must not be able
// to rewrite the sentence the user is being asked to agree to.
void WorktreeCopyTest::workspacePromptEscapesThePath()
{
    const auto p = WorktreeCopy::discardPrompt(
        /*isolated*/ false, 0, QStringLiteral("main"),
        QStringLiteral("/home/u/<b>proj</b>"), 1);
    QVERIFY(!p.body.contains(QStringLiteral("<b>proj</b>")));
    QVERIFY(p.body.contains(QStringLiteral("&lt;b&gt;proj&lt;/b&gt;")));
}

// The destructive button spells out what it does, and the two rows do not
// share a label — a user who has learned "Discard changes" on isolated rows
// must not be able to click the same words and lose their own work.
void WorktreeCopyTest::confirmLabelsAreNamedActionsAndDiffer()
{
    const auto iso = WorktreeCopy::discardPrompt(
        true, 1, QStringLiteral("agentkate/t-1"), QStringLiteral("/w/t-1"), 2);
    const auto ws = WorktreeCopy::discardPrompt(
        false, 1, QStringLiteral("main"), QStringLiteral("/home/u/proj"), 2);
    QVERIFY(!iso.confirmLabel.isEmpty());
    QVERIFY(!ws.confirmLabel.isEmpty());
    QVERIFY(iso.confirmLabel != ws.confirmLabel);
    QVERIFY(iso.title != ws.title);
    for (const QString &label : {iso.confirmLabel, ws.confirmLabel}) {
        QVERIFY(label != QStringLiteral("Yes"));
        QVERIFY(label != QStringLiteral("OK"));
    }
}

// Both branches count with i18np, so "1 uncommitted change" never reads as a
// plural and the count shown is the count the core will destroy.
void WorktreeCopyTest::pluralFormsAgreeWithTheCount()
{
    const auto isoOne = WorktreeCopy::discardPrompt(
        true, 2, QStringLiteral("agentkate/t-2"), QStringLiteral("/w/t-2"), 1);
    QVERIFY(isoOne.body.contains(QStringLiteral("1 uncommitted change")));
    QVERIFY(!isoOne.body.contains(QStringLiteral("changes")));

    const auto wsMany = WorktreeCopy::discardPrompt(
        false, 2, QStringLiteral("main"), QStringLiteral("/home/u/proj"), 7);
    QVERIFY(wsMany.body.contains(QStringLiteral("7 uncommitted changes")));
}

// The card pill and the tooltip are how the row announces the distinction
// before the user ever reaches the dialog (audit F50).
void WorktreeCopyTest::notIsolatedMarkersAreNonEmptyAndDistinct()
{
    QVERIFY(!WorktreeCopy::notIsolatedPill().isEmpty());
    QVERIFY(!WorktreeCopy::notIsolatedTooltip().isEmpty());
    QVERIFY(WorktreeCopy::notIsolatedTooltip().contains(
        QStringLiteral("your own files")));
}

QTEST_MAIN(WorktreeCopyTest)
#include "WorktreeCopyTest.moc"
