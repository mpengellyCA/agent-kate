// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

// WorktreeDashboard.h is included for WorktreeCopy::DiscardPrompt, deliberately
// rather than declaring a second struct of the same shape: the dashboard's
// discard and the roster's discard are two destructive confirmations in one
// product and they share a vocabulary (a title, a body that names the blast
// radius, and a button that spells out what it does instead of saying "Yes").
#include "WorktreeDashboard.h"

#include <KLocalizedString>

#include <QString>
#include <Qt>

// AgentActions holds the ENABLEMENT DECISION for the per-agent actions, and the
// labels whose truth depends on the same facts, as pure functions.
//
// Why a module and not two copies of an `if`: these actions are reachable from
// two places — the window's &Agent menu and the roster's right-click menu — and
// until now each decided for itself. The roster's copy simply did not exist,
// so the gate that stops "Create pull request…" being offered on an agent that
// has no branch to open one from was, on the primary path, absent. Neither
// MainWindow.cpp nor AgentRoster.cpp is compiled into any test binary, so that
// absence was invisible: a verifier deleted every gate in this cluster and the
// suite stayed green. The decision lives here so a test can see it.
namespace AgentActions {

// The two facts an agent row needs to gate its own menu, carried as item data
// on the roster row and pushed by AgentDock from exactly the sources the
// window's Agent menu reads (AgentPanel::isIsolated / worktreePathForAgent), so
// the two surfaces cannot drift apart.
//
// Numbered well clear of AgentRoles (AgentCardDelegate.h, currently up to
// Qt::UserRole + 14) so the delegate's roles have room to grow; these are not
// painted, which is why they are not in that namespace.
constexpr int IsolatedRole = Qt::UserRole + 20; // bool — has a private branch
constexpr int HasPathRole = Qt::UserRole + 21;  // bool — has a working directory

// AgentActionEnablement is the answer for one agent: which of its actions are
// meaningful right now. Every field defaults to false, so a caller that forgets
// to wire one fails closed.
struct AgentActionEnablement {
    bool rename = false;
    bool resume = false;
    bool attach = false;
    bool changes = false;
    bool stop = false;
    bool commit = false;
    bool pullRequest = false;
    bool merge = false;
    bool terminal = false;
    bool tags = false;
    bool discard = false;
    bool close = false;
};

// compute decides, from the agent's state alone, what may be offered.
//
//   exists   — there is an agent to act on at all (a selected row / panel).
//   running  — it has a live process, so Stop means something.
//   dormant  — it is stopped but resumable.
//   isolated — it has a PRIVATE BRANCH of its own. Deliberately stronger than
//              hasPath: git.snapshot reports a working directory for
//              workspace-mode threads too, so gating on a path offered "Merge"
//              and "Create pull request…" for agents with no branch to merge or
//              open a PR from. The core refuses both outright in that state
//              (worktree.go, "!wt.Isolated"), so offering them can only end in
//              an error dialog. Callers pass a value that already implies
//              started — AgentDock::activeAgentIsolated() requires a thread id,
//              because before agent.start there is no branch anywhere.
//   hasPath  — a working directory is known for it (isolated worktree OR the
//              workspace checkout it runs in).
inline AgentActionEnablement compute(bool exists, bool running, bool dormant,
                                     bool isolated, bool hasPath)
{
    AgentActionEnablement e;
    e.rename = exists;
    e.resume = dormant;
    e.attach = exists && !dormant;
    e.changes = exists;
    e.stop = running;
    // Committing works in whatever directory the agent has — a workspace agent
    // commits onto the current branch (AgentDock warns before it does).
    e.commit = hasPath;
    e.pullRequest = isolated;
    e.merge = isolated;
    // A dormant agent's directory still exists on disk — dormancy stops the
    // process, it does not remove the worktree — so a terminal rooted there is
    // still useful and still correct. What the action is CALLED depends on
    // isolation; see terminalActionLabel.
    e.terminal = hasPath;
    e.tags = exists;
    // Discard is the destructive one and it needs something to destroy. An
    // agent that never started has no worktree, no session and no grants; Close
    // is the action for that.
    e.discard = hasPath;
    e.close = exists;
    return e;
}

// terminalActionLabel names the Open-Terminal action for what it actually
// opens. It was called "Open Terminal in Worktree" in every state, but for a
// workspace-mode agent the folder it opens is the USER'S OWN CHECKOUT, not a
// private copy — the label claimed an isolation the agent does not have. The
// action stays enabled either way (a terminal in your project is useful); only
// the name changes.
inline QString terminalActionLabel(bool isolated)
{
    return isolated
        ? i18nc("@action opens a terminal in the agent's private worktree",
                "Open &Terminal in Worktree")
        : i18nc("@action opens a terminal in the user's own project checkout",
                "Open &Terminal in Project Folder");
}

inline QString terminalActionTooltip(bool isolated)
{
    return isolated
        ? i18n("Open a terminal in this agent's private copy of your project.")
        : i18n("This agent has no private copy — this opens a terminal in your "
               "own project folder.");
}

// discardActionLabel, same rule: "Discard Worktree" on an agent that has no
// worktree describes an operation that will not happen. What agent.discard
// really does to a workspace agent is delete the AGENT.
inline QString discardActionLabel(bool isolated)
{
    return isolated ? i18nc("@action", "&Discard Worktree")
                    : i18nc("@action", "&Delete Agent");
}

inline QString discardActionTooltip(bool isolated)
{
    return isolated
        ? i18n("Throw away the agent's private copy, its branch and everything "
               "in them, and delete the agent.")
        : i18n("Delete the agent and its conversation. Your own files are not "
               "touched — this agent has no private copy.");
}

// agentDiscardPrompt builds the confirmation for `agent.discard`.
//
// SAFETY (the same class as audit F29, which is why it wears F29's struct): the
// old prompt asked "Discard this agent's worktree and all of its uncommitted
// changes?" — which understates the blast radius in one direction and
// overstates it in the other, depending on isolation:
//
//   isolated  — the core removes the worktree AND deletes the branch AND drops
//               the session record, the transcript, the attachment sidecar and
//               every approval grant the agent held (handlers.go, agent.discard).
//               "its worktree and uncommitted changes" names one of six.
//   workspace — worktree.Remove() returns early for a non-isolated thread, so
//               NOTHING in the user's checkout is touched. The old prompt told
//               a user about to delete an agent that their uncommitted changes
//               were about to be discarded, which is the more frightening lie
//               of the two and invites them to cancel an operation that is safe.
//
// path may be empty (the worktree path is not always known to the UI yet); the
// sentence then simply omits it rather than printing an empty <tt></tt>.
inline WorktreeCopy::DiscardPrompt agentDiscardPrompt(bool isolated,
                                                      const QString &agentName,
                                                      const QString &path)
{
    WorktreeCopy::DiscardPrompt p;
    const QString name = agentName.isEmpty() ? i18n("this agent")
                                             : agentName.toHtmlEscaped();
    if (!isolated) {
        p.title = i18nc("@title:window", "Delete this agent?");
        p.body = i18n(
            "Delete <b>%1</b> for good?<br><br>"
            "This throws away the agent itself: its conversation, its saved "
            "session (it can never be resumed) and every permission you granted "
            "it.<br><br>"
            "This agent has no private copy — it works directly in your own "
            "files — so <b>your files are left exactly as they are</b>, "
            "including anything it already changed there.<br><br>"
            "This cannot be undone.",
            name);
        p.confirmLabel = i18nc("@action:button destructive", "Delete agent");
        return p;
    }
    p.title = i18nc("@title:window", "Delete this agent and its private copy?");
    p.body = path.isEmpty()
        ? i18n(
            "Delete <b>%1</b> for good?<br><br>"
            "This removes the agent's private copy of your project and its "
            "branch, with everything in them that has not been committed and "
            "merged, and throws away the agent's conversation, its saved "
            "session and every permission you granted it.<br><br>"
            "Your own checkout is not touched. This cannot be undone.",
            name)
        : i18n(
            "Delete <b>%1</b> for good?<br><br>"
            "This removes the agent's private copy of your project at "
            "<tt>%2</tt> and its branch, with everything in them that has not "
            "been committed and merged, and throws away the agent's "
            "conversation, its saved session and every permission you granted "
            "it.<br><br>"
            "Your own checkout is not touched. This cannot be undone.",
            name, path.toHtmlEscaped());
    p.confirmLabel =
        i18nc("@action:button destructive", "Delete agent and its copy");
    return p;
}

} // namespace AgentActions
