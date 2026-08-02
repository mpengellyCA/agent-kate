// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// Plan 27 §1: one KActionCollection, and the name table that keys it.
//
// Two properties are pinned here, and neither can be pinned the ordinary way.
// MainWindow.cpp is compiled by the application target alone — it is far too
// entangled to link into a test binary — so a guard that lives inside it is
// invisible to the suite. That is not a hypothetical: AgentActions.h records a
// verifier deleting every per-agent action gate and ctest staying green. The
// same shape applies here, so the same answer is used: the decision lives in a
// header a test can include (ActionIds.h), and the *use* of it is pinned by
// scanning the source, exactly as MarkdownUtilTest scans for unguarded
// setMarkdown() calls.
//
//   1. THE NAMES ARE FROZEN. An action's id is the KConfig key its shortcut is
//      stored under, so renaming one silently discards every user's
//      customisation of that action — no error, no migration, just a binding
//      that quietly reverts. The frozen list below is what turns a rename into
//      a failing test instead of a bug report.
//
//   2. EVERY ID IS ACTUALLY REGISTERED, no shortcut is bound behind the
//      collection's back, and the isolation gates on the destructive agent
//      actions still derive from AgentActions::compute. That last one is the
//      constraint four rounds of audit remediation put there: Merge, Open-PR
//      and Discard are gated because a workspace-mode agent has no private
//      branch to merge or open a PR from and its "discard" would reach the
//      user's own checkout, and a refactor that rebuilds actions from static
//      strings is exactly how such a gate silently reverts.

#include "shell/ActionIds.h"

#include <QCoreApplication>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QRegularExpression>
#include <QSet>
#include <QString>
#include <QStringList>
#include <QTest>

namespace {

// Locate ui/src from wherever the test binary was run. Same walk-up strategy as
// MarkdownUtilTest, keyed on a file that can only be this tree.
QString uiSrcDir()
{
    for (const QString &from :
         {QCoreApplication::applicationDirPath(), QDir::currentPath()}) {
        QDir d(from);
        for (int i = 0; i < 8; ++i) {
            if (d.exists(QStringLiteral("ui/src/shell/ActionIds.h"))) {
                return d.absoluteFilePath(QStringLiteral("ui/src"));
            }
            if (!d.cdUp()) {
                break;
            }
        }
    }
    const QFileInfo self(QString::fromUtf8(__FILE__));
    if (self.isAbsolute()) {
        QDir t = self.absoluteDir(); // ui/tests
        if (t.cdUp() && t.exists(QStringLiteral("src/shell/ActionIds.h"))) {
            return t.absoluteFilePath(QStringLiteral("src"));
        }
    }
    return QString();
}

QString readSource(const QString &relative)
{
    const QString dir = uiSrcDir();
    if (dir.isEmpty()) {
        return QString();
    }
    QFile f(QDir(dir).absoluteFilePath(relative));
    if (!f.open(QIODevice::ReadOnly | QIODevice::Text)) {
        return QString();
    }
    return QString::fromUtf8(f.readAll());
}

// Drop //-to-end-of-line comments so a scan cannot be satisfied — or tripped —
// by prose. This file's own subject matter guarantees the word "setShortcut"
// appears in MainWindow.cpp's comments; without this, the scan would be a
// permanent false positive.
QString withoutLineComments(const QString &src)
{
    QString out;
    out.reserve(src.size());
    const QStringList lines = src.split(QLatin1Char('\n'));
    for (const QString &line : lines) {
        const int idx = line.indexOf(QLatin1String("//"));
        out += (idx >= 0 ? line.left(idx) : line);
        out += QLatin1Char('\n');
    }
    return out;
}

// The body of one MainWindow method, from its signature to the first
// column-zero closing brace.
QString methodBody(const QString &src, const QString &signature)
{
    const int start = src.indexOf(signature);
    if (start < 0) {
        return QString();
    }
    const int end = src.indexOf(QLatin1String("\n}\n"), start);
    return end < 0 ? QString() : src.mid(start, end - start);
}

// THE FROZEN CATALOGUE. Every id this application has ever shipped, spelled
// exactly as it is stored in the user's agentkaterc.
//
// If this test fails because a name changed: put the name back. Retitling an
// action is free; renaming its id throws away the user's shortcut. Adding an
// entry here (with the constant, all(), and the registration in MainWindow) is
// the only edit this list should ever see.
QStringList frozenIds()
{
    QStringList ids{
        QStringLiteral("file_open_project"),
        QStringLiteral("file_welcome_screen"),
        QStringLiteral("file_resume_session"),
        QStringLiteral("file_save"),
        QStringLiteral("file_save_all"),
        QStringLiteral("file_quit"),

        QStringLiteral("agent_new"),
        QStringLiteral("agent_rename"),
        QStringLiteral("agent_resume"),
        QStringLiteral("agent_attach_files"),
        QStringLiteral("agent_show_changes"),
        QStringLiteral("agent_merge_changes"),
        QStringLiteral("agent_stop"),
        QStringLiteral("agent_commit"),
        QStringLiteral("agent_create_pull_request"),
        QStringLiteral("agent_open_terminal"),
        QStringLiteral("agent_edit_tags"),
        QStringLiteral("agent_discard"),
        QStringLiteral("agent_close"),
        QStringLiteral("agent_manage_skills"),

        QStringLiteral("options_tabs_by_project"),
        QStringLiteral("options_tabs_by_agent"),
        QStringLiteral("options_enter_sends"),
        QStringLiteral("options_show_tool_calls"),
        QStringLiteral("options_autosave"),
        QStringLiteral("options_configure_providers"),
        QStringLiteral("options_appearance"),
        QStringLiteral("options_experience_simple"),
        QStringLiteral("options_experience_advanced"),
        QStringLiteral("options_language_extensions"),
        QStringLiteral("options_show_menubar"),
        QStringLiteral("options_configure_keybinding"),

        QStringLiteral("view_command_palette"),
        QStringLiteral("view_git_blame"),
        QStringLiteral("view_toggle_bottom_panel"),
        QStringLiteral("view_find_in_project"),
        QStringLiteral("view_next_search_match"),
        QStringLiteral("view_previous_search_match"),
        QStringLiteral("view_new_terminal"),
        QStringLiteral("view_focus_terminal"),
        QStringLiteral("view_next_terminal"),
        QStringLiteral("view_previous_terminal"),
        QStringLiteral("view_agent_terminal"),
        QStringLiteral("view_centre_editor"),
        QStringLiteral("view_centre_split"),
        QStringLiteral("view_centre_chat"),
        QStringLiteral("view_focus_editor"),
        QStringLiteral("view_focus_agent"),

        QStringLiteral("layout_converse"),
        QStringLiteral("layout_build"),
        QStringLiteral("layout_review"),
        QStringLiteral("layout_split"),

        QStringLiteral("code_goto_definition"),
        QStringLiteral("code_find_references"),
        QStringLiteral("code_goto_symbol"),
        QStringLiteral("code_quick_fix"),
        QStringLiteral("code_rename_symbol"),
        QStringLiteral("code_format_document"),
        QStringLiteral("code_format_on_save"),
        QStringLiteral("code_signature_help"),
        QStringLiteral("code_next_problem"),
        QStringLiteral("code_previous_problem"),
        QStringLiteral("code_restart_language_server"),

        QStringLiteral("panel_collapse_left"),
        QStringLiteral("panel_collapse_right"),
    };
    for (const QString &side : {QStringLiteral("left"), QStringLiteral("right")}) {
        for (int i = 1; i <= 9; ++i) {
            ids << QStringLiteral("panel_raise_%1_%2").arg(side).arg(i);
        }
    }
    return ids;
}

} // namespace

class ActionIdsTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void idsAreNonEmptyAndUnique();
    void catalogueIsFrozen();
    void railIdsFollowTheDocumentedShape();
    void everyDeclaredIdIsRegisteredInMainWindow();
    void noShortcutIsBoundBehindTheCollection();
    void railTooltipsNameTheActiveBinding();
    void destructiveAgentActionsStillDeriveFromCompute();
};

// The plan's own acceptance check: an id that is empty, or shared with another
// action, is a shortcut the user cannot address. KActionCollection is no help
// here — its addAction() documents that a duplicate name silently REPLACES the
// earlier action in the collection, so a collision costs an action with no
// warning anywhere.
void ActionIdsTest::idsAreNonEmptyAndUnique()
{
    const QStringList ids = ActionIds::all();
    QVERIFY2(ids.size() > 50, "catalogue is implausibly small — wrong build?");
    QSet<QString> seen;
    for (const QString &id : ids) {
        QVERIFY2(!id.trimmed().isEmpty(), "an action id is empty");
        QVERIFY2(!seen.contains(id),
                 qPrintable(QStringLiteral("duplicate action id: ") + id));
        seen.insert(id);
    }
}

void ActionIdsTest::catalogueIsFrozen()
{
    QStringList actual = ActionIds::all();
    QStringList expected = frozenIds();
    actual.sort();
    expected.sort();
    // Report the difference both ways: a rename shows up as one removal plus
    // one addition, which is the message that tells you what happened.
    QStringList added;
    for (const QString &id : std::as_const(actual)) {
        if (!expected.contains(id)) {
            added << id;
        }
    }
    QStringList removed;
    for (const QString &id : std::as_const(expected)) {
        if (!actual.contains(id)) {
            removed << id;
        }
    }
    QVERIFY2(removed.isEmpty(),
             qPrintable(QStringLiteral(
                            "action id(s) GONE — every user's customised "
                            "shortcut for these is now orphaned: ")
                        + removed.join(QStringLiteral(", "))));
    QVERIFY2(added.isEmpty(),
             qPrintable(QStringLiteral("new action id(s) not in the frozen "
                                       "list — add them there deliberately: ")
                        + added.join(QStringLiteral(", "))));
}

// The rail ids are generated rather than written out, because the binding
// belongs to a POSITION in a strip and not to a particular panel. Generated
// still means frozen, so the shape is pinned explicitly.
void ActionIdsTest::railIdsFollowTheDocumentedShape()
{
    QCOMPARE(ActionIds::railRaise(true, 1), QStringLiteral("panel_raise_left_1"));
    QCOMPARE(ActionIds::railRaise(true, 9), QStringLiteral("panel_raise_left_9"));
    QCOMPARE(ActionIds::railRaise(false, 3), QStringLiteral("panel_raise_right_3"));
    QCOMPARE(ActionIds::railCollapse(true), QStringLiteral("panel_collapse_left"));
    QCOMPARE(ActionIds::railCollapse(false), QStringLiteral("panel_collapse_right"));
    QCOMPARE(ActionIds::kRailOrdinals, 9);
}

// Declaring an id is not registering it. This walks every constant in
// ActionIds.h and demands MainWindow.cpp name it — so an action created but
// left out of the collection (the exact regression this whole phase exists to
// prevent, and the one every later plan is most likely to reintroduce) fails
// here rather than being discovered when a user cannot rebind it.
void ActionIdsTest::everyDeclaredIdIsRegisteredInMainWindow()
{
    const QString header = readSource(QStringLiteral("shell/ActionIds.h"));
    const QString window = readSource(QStringLiteral("MainWindow.cpp"));
    if (header.isEmpty() || window.isEmpty()) {
        QSKIP("source tree not reachable from the test binary");
    }

    // inline constexpr char FileSave[] = "file_save";
    static const QRegularExpression decl(
        QStringLiteral("constexpr\\s+char\\s+([A-Za-z0-9_]+)\\s*\\[\\]\\s*=\\s*\"([^\"]*)\""));
    QStringList symbols;
    QStringList values;
    auto it = decl.globalMatch(header);
    while (it.hasNext()) {
        const QRegularExpressionMatch m = it.next();
        symbols << m.captured(1);
        values << m.captured(2);
    }
    QVERIFY2(symbols.size() > 50,
             qPrintable(QStringLiteral("parsed only %1 constants from "
                                       "ActionIds.h — parser out of step with "
                                       "the header").arg(symbols.size())));

    // all() must serve exactly the constants that exist: a constant added but
    // left out of all() would be invisible to the frozen-catalogue check above.
    const QStringList declared = ActionIds::all();
    QStringList missingFromAll;
    for (const QString &v : std::as_const(values)) {
        if (!declared.contains(v)) {
            missingFromAll << v;
        }
    }
    QVERIFY2(missingFromAll.isEmpty(),
             qPrintable(QStringLiteral("declared but absent from ActionIds::all(): ")
                        + missingFromAll.join(QStringLiteral(", "))));

    QStringList unregistered;
    for (const QString &sym : std::as_const(symbols)) {
        if (!window.contains(QStringLiteral("ActionIds::") + sym)) {
            unregistered << sym;
        }
    }
    QVERIFY2(unregistered.isEmpty(),
             qPrintable(QStringLiteral(
                            "declared in ActionIds.h but never registered in "
                            "MainWindow.cpp — the action exists with no "
                            "configurable shortcut and no palette entry: ")
                        + unregistered.join(QStringLiteral(", "))));

    // The generated rail ids have no constant to look for, so check the
    // generators are the thing MainWindow calls.
    QVERIFY2(window.contains(QStringLiteral("ActionIds::railRaise(")),
             "the rail raise accelerators are not registered by id");
    QVERIFY2(window.contains(QStringLiteral("ActionIds::railCollapse(")),
             "the rail collapse accelerators are not registered by id");
}

// A shortcut set with QAction::setShortcut, or bound with a raw QShortcut, is a
// literal: it appears in no collection, has no default to reset to, is absent
// from Configure Shortcuts, and cannot be rebound. Both were how every binding
// in this window worked before plan 27 §1, and both are easy to reach for again
// when adding "just one more" action.
void ActionIdsTest::noShortcutIsBoundBehindTheCollection()
{
    const QString window = readSource(QStringLiteral("MainWindow.cpp"));
    if (window.isEmpty()) {
        QSKIP("source tree not reachable from the test binary");
    }
    const QString code = withoutLineComments(window);
    QVERIFY2(!code.contains(QStringLiteral("setShortcut(")),
             "MainWindow.cpp binds a shortcut with setShortcut() — use "
             "registerAction(id, act, {seq}), which declares it as a DEFAULT "
             "the user may override");
    QVERIFY2(!code.contains(QStringLiteral("setShortcuts(")),
             "MainWindow.cpp binds shortcuts with setShortcuts() — use "
             "registerAction(id, act, {seqs})");
    QVERIFY2(!code.contains(QStringLiteral("new QShortcut(")),
             "MainWindow.cpp creates a raw QShortcut — it has no name, no text "
             "and no collection entry, so nothing can list, search or rebind it");
}

// The rail tabs carry the only on-screen mention of the raise accelerators, and
// they used to COMPUTE the text from Alt+ordinal — correct exactly as long as
// nobody rebound anything. Now that rebinding is possible, a computed hint
// would confidently name the binding the user had just replaced, which is worse
// than naming none. The hint must come off the registered action.
void ActionIdsTest::railTooltipsNameTheActiveBinding()
{
    const QString window = readSource(QStringLiteral("MainWindow.cpp"));
    if (window.isEmpty()) {
        QSKIP("source tree not reachable from the test binary");
    }
    const QString body = withoutLineComments(
        methodBody(window, QStringLiteral("void MainWindow::refreshPanelTooltips()")));
    QVERIFY2(!body.isEmpty(), "MainWindow::refreshPanelTooltips() not found");
    QVERIFY2(body.contains(QStringLiteral("m_actions->action(ActionIds::railRaise(")),
             "the rail tooltip does not read its binding from the collection — "
             "it will name a shortcut the user may have rebound");
    QVERIFY2(!body.contains(QStringLiteral("QKeyCombination(")),
             "the rail tooltip is rebuilding a key sequence by hand instead of "
             "reading the action's active shortcut");
}

// The security constraint the audit rounds established, kept honest across the
// refactor. Merge and Create-Pull-Request need a private BRANCH (the core
// refuses both outright otherwise), and Discard on a workspace-mode agent would
// otherwise be offered as though it removed a worktree that does not exist.
// AgentActions::compute is the one predicate both the menu bar and the roster
// derive from — so what is pinned is that this menu still asks it, rather than
// deciding for itself.
void ActionIdsTest::destructiveAgentActionsStillDeriveFromCompute()
{
    const QString window = readSource(QStringLiteral("MainWindow.cpp"));
    if (window.isEmpty()) {
        QSKIP("source tree not reachable from the test binary");
    }
    const QString body = withoutLineComments(
        methodBody(window, QStringLiteral("void MainWindow::updateAgentActions()")));
    QVERIFY2(!body.isEmpty(), "MainWindow::updateAgentActions() not found");
    QVERIFY2(body.contains(QStringLiteral("AgentActions::compute(")),
             "updateAgentActions no longer asks AgentActions::compute — the "
             "&Agent menu and the roster's context menu can now disagree");

    // Every gated action must take its enablement from the computed struct.
    // A literal true/false here is the revert this test exists to catch.
    struct Gate {
        const char *member;
        const char *field;
    };
    static const Gate gates[] = {
        {"m_agentMergeAct", "en.merge"},
        {"m_agentPrAct", "en.pullRequest"},
        {"m_agentDiscardAct", "en.discard"},
        {"m_agentCommitAct", "en.commit"},
        {"m_agentTerminalAct", "en.terminal"},
        {"m_agentStopAct", "en.stop"},
        {"m_agentResumeAct", "en.resume"},
        {"m_agentRenameAct", "en.rename"},
        {"m_agentAttachAct", "en.attach"},
        {"m_agentChangesAct", "en.changes"},
        {"m_agentTagsAct", "en.tags"},
        {"m_agentCloseAct", "en.close"},
    };
    for (const Gate &g : gates) {
        const QString expected = QStringLiteral("%1->setEnabled(%2)")
                                     .arg(QLatin1String(g.member),
                                          QLatin1String(g.field));
        QVERIFY2(body.contains(expected),
                 qPrintable(QStringLiteral("expected `%1` in updateAgentActions "
                                           "— the action's enablement no longer "
                                           "comes from AgentActions::compute")
                                .arg(expected)));
        // …and nothing hard-enables it alongside.
        QVERIFY2(!body.contains(QStringLiteral("%1->setEnabled(true)")
                                    .arg(QLatin1String(g.member))),
                 qPrintable(QStringLiteral("%1 is hard-enabled with "
                                           "setEnabled(true), bypassing the gate")
                                .arg(QLatin1String(g.member))));
    }

    // The isolation-aware labels are part of the same finding: the UI must not
    // claim a containment a git worktree does not provide.
    QVERIFY2(body.contains(QStringLiteral("AgentActions::terminalActionLabel(isolated)")),
             "the Open-Terminal label no longer tracks isolation — it would "
             "say \"in Worktree\" while opening the user's own checkout");
    QVERIFY2(body.contains(QStringLiteral("AgentActions::discardActionLabel(isolated)")),
             "the Discard label no longer tracks isolation — it would offer to "
             "discard a worktree the agent does not have");
}

QTEST_MAIN(ActionIdsTest)
#include "ActionIdsTest.moc"
