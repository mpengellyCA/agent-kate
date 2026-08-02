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

// The same file also pins the review/land copy shared by the dashboard and its
// diff reader (WorktreeReviewCopy): what the diff view may claim to have shown
// (F41), what "land" merges into (F50), and which threads may be landed at all
// (the F29 asymmetry, in its merge form).

#include "WorktreeDashboard.h"
#include "git/WorktreeReviewCopy.h"

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
    void landCopyNamesTheWorkspaceNotMain();
    void landIsRefusedForWorkspaceModeThreads();
    void diffHeaderDoesNotCallTheUsersCheckoutAWorktree();
    void diffViewDisclosesWhatItCannotShow();
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

// Audit F50: agent.land and git.land both merge into the workspace's CURRENT
// branch (`git branch --show-current`), which is very often not main. The
// action's own name must not claim otherwise, and the tooltip has to say what
// "the workspace" resolves to — the one place the honest answer fits.
void WorktreeCopyTest::landCopyNamesTheWorkspaceNotMain()
{
    const QString label = WorktreeReviewCopy::landLabel();
    QVERIFY(!label.isEmpty());
    QVERIFY(!label.contains(QStringLiteral("main"), Qt::CaseInsensitive));
    QVERIFY(label.contains(QStringLiteral("workspace")));
    const QString tip = WorktreeReviewCopy::landTooltip();
    // The only permitted mention of main is the one that denies it.
    QVERIFY(!tip.contains(QStringLiteral("into main")));
    QVERIFY(tip.contains(QStringLiteral("not necessarily main")));
    QVERIFY(tip.contains(QStringLiteral("right now")));
}

// The merge half of the F29 asymmetry: git.snapshot reports a path (and often a
// branch — the workspace's own) for EVERY thread, so "has a worktree" is true
// for threads that have none. Only an isolated thread with commits of its own
// can be landed, and the predicate the button and the handlers share must say
// so on its own, without help from the caller.
void WorktreeCopyTest::landIsRefusedForWorkspaceModeThreads()
{
    QVERIFY(!WorktreeReviewCopy::canLand(/*isolated*/ false, /*ahead*/ 5));
    QVERIFY(!WorktreeReviewCopy::canLand(false, 0));
    QVERIFY(!WorktreeReviewCopy::canLand(/*isolated*/ true, /*ahead*/ 0));
    QVERIFY(WorktreeReviewCopy::canLand(true, 1));
}

// Audit F41's converse: for a workspace-mode thread the "worktree" IS the
// user's checkout, and the changes listed are theirs as much as the agent's.
void WorktreeCopyTest::diffHeaderDoesNotCallTheUsersCheckoutAWorktree()
{
    const QString iso = WorktreeReviewCopy::diffHeader(
        true, QStringLiteral("#3 agentkate/t-1"),
        QStringLiteral("/home/u/proj/.agentkate/worktrees/t-1"));
    QVERIFY(iso.contains(QStringLiteral("worktree")));
    QVERIFY(iso.contains(QStringLiteral("#3")));

    const QString ws = WorktreeReviewCopy::diffHeader(
        false, QStringLiteral("#3 main"), QStringLiteral("/home/u/proj"));
    QVERIFY(!ws.contains(QStringLiteral("worktree")));
    QVERIFY(ws.contains(QStringLiteral("directly in your own files")));
    QVERIFY(ws.contains(QStringLiteral("/home/u/proj")));
    QVERIFY(ws.contains(QStringLiteral("cannot tell your edits")));

    // Shown as rich text, so a path cannot rewrite the sentence around it.
    const QString evil = WorktreeReviewCopy::diffHeader(
        false, QString(), QStringLiteral("/home/u/<b>proj</b>"));
    QVERIFY(!evil.contains(QStringLiteral("<b>proj</b>")));
    QVERIFY(evil.contains(QStringLiteral("&lt;b&gt;proj&lt;/b&gt;")));
}

// Audit F41, the residue no diff can cover: ignored paths and absolute-path
// writes outside the folder. The view may not imply it has shown everything —
// especially in its EMPTY state, which is where "has not changed anything yet"
// came from.
void WorktreeCopyTest::diffViewDisclosesWhatItCannotShow()
{
    for (bool isolated : {true, false}) {
        const QString limits = WorktreeReviewCopy::diffLimits(isolated);
        QVERIFY(limits.contains(QStringLiteral("ignore")));
        QVERIFY(limits.contains(QStringLiteral("outside")));

        const QString empty = WorktreeReviewCopy::diffEmptyMessage(isolated);
        QVERIFY(empty.contains(QStringLiteral("outside")));
        // "No uncommitted changes." full stop is the claim under test.
        QVERIFY(!empty.trimmed().endsWith(QStringLiteral("changes.")));
    }
}

QTEST_MAIN(WorktreeCopyTest)
#include "WorktreeCopyTest.moc"
