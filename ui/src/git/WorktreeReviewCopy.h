// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <KLocalizedString>

#include <QString>

// WorktreeReviewCopy holds the sentences (and the one predicate) shared by the
// two places an agent's work is reviewed and handed back: the Worktree
// Dashboard and its diff reader. Header-only and widget-free for the same
// reason as WorktreeCopy — a string that can only be read out of a running
// QDialog cannot be asserted on, and these are strings whose being wrong costs
// the user either their work or their trust in what they are looking at.
//
// Three audit findings converge here:
//
//   F41 — the workspace-mode diff. `git diff HEAD` is tracked-only, so an agent
//         that only CREATED files produced an empty patch and the UI said it
//         "has not changed anything yet". The core now appends untracked files
//         (worktree.Diff), but two honest gaps remain that no git diff can
//         close: paths git is told to ignore, and absolute-path writes outside
//         the folder entirely. A view that cannot show those must not imply it
//         has shown everything — hence diffLimits() and an empty state that
//         says "nothing here", never "nothing happened".
//
//   F50 — "Land into main" merges into the workspace's CURRENT branch
//         (worktree.LandWithOptions reads `git branch --show-current`), which
//         is very often not main. One phrasing, used by every site.
//
//   F29/F5x — the merge actions are only meaningful for an agent working on its
//         own branch. canLand() is the single predicate, so the button's
//         enablement and the handler's refusal cannot drift apart; the core
//         refuses a non-isolated Land as well (worktree.LandWithOptions), and
//         both layers are meant, not one instead of the other.
namespace WorktreeReviewCopy {

// canLand is true only for an isolated thread with commits of its own to merge.
//
// A non-isolated ("workspace mode") thread has no branch of its own — it IS the
// workspace — so landing it is meaningless, and offering it is the same
// asymmetry that made F29 a data-loss bug: git.snapshot reports a path for
// every thread, so "has a worktree" is true for threads that have no worktree.
inline bool canLand(bool isolated, int ahead)
{
    return isolated && ahead > 0;
}

inline QString landLabel()
{
    return i18nc("@action:button", "Land into workspace…");
}

// Says what "the workspace" means, because the honest answer is not a fixed
// branch name and the old label ("Land into main") named one that is often
// wrong.
inline QString landTooltip()
{
    return i18n(
        "Merges this agent's branch into whichever branch your workspace is on "
        "right now — not necessarily main. Available only for an agent working "
        "in its own copy, once it has committed something to merge.");
}

// The line above the file list. The two modes are not the same thing wearing
// one sentence: in workspace mode the "worktree" is the user's own checkout and
// the changes listed include their own.
inline QString diffHeader(bool isolated, const QString &label, const QString &path)
{
    if (isolated) {
        return i18n("Uncommitted changes in worktree <b>%1</b>.",
                    label.toHtmlEscaped());
    }
    if (path.isEmpty()) {
        return i18n("This agent works <b>directly in your own files</b>. "
                    "Everything uncommitted there is listed below — Agent Kate "
                    "cannot tell your edits from the agent's.");
    }
    return i18n("This agent works <b>directly in your own files</b> at "
                "<tt>%1</tt>. Everything uncommitted there is listed below — "
                "Agent Kate cannot tell your edits from the agent's.",
                path.toHtmlEscaped());
}

// The residue neither mode can show (audit F41). Stated in both, because it is
// true in both: `git diff` is a report about tracked-or-untracked files in ONE
// directory, and an agent runs at the user's uid with the whole filesystem in
// reach.
inline QString diffLimits(bool isolated)
{
    return isolated
        ? i18n("Not shown: files this project tells git to ignore, and anything "
               "the agent wrote outside its copy. This is a diff of one folder, "
               "not a record of everything the agent did.")
        : i18n("Not shown: files this project tells git to ignore, and anything "
               "the agent wrote outside this folder. This is a diff of one "
               "folder, not a record of everything the agent did.");
}

// The empty state. "No uncommitted changes." full stop is the same false
// statement as "has not changed anything yet" — it answers a question about the
// agent with a fact about one directory.
inline QString diffEmptyMessage(bool isolated)
{
    return isolated
        ? i18n("Nothing uncommitted in this copy. Files git is told to ignore, "
               "and anything written outside the copy, would not show up here.")
        : i18n("Nothing uncommitted in this folder. Files git is told to "
               "ignore, and anything written outside the folder, would not show "
               "up here.");
}

} // namespace WorktreeReviewCopy
