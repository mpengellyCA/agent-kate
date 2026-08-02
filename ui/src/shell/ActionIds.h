// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QString>
#include <QStringList>

// ActionIds is the name table for every user-visible action in MainWindow's
// KActionCollection (plan 27 §1).
//
// ============================ THE NAMES ARE A CONTRACT =======================
//
// An action's name is the KConfig key its shortcut is stored under. When a user
// rebinds Ctrl+Alt+T in Settings ▸ Configure Shortcuts, KActionCollection writes
//
//     [Shortcuts]
//     view_agent_terminal=Ctrl+Alt+Shift+T
//
// keyed by the name — nothing else. RENAMING AN ID SILENTLY DISCARDS EVERY
// USER'S CUSTOMISATION OF THAT ACTION: the old key is orphaned, the new one has
// no entry, and the app quietly reverts to its default binding with no error
// anywhere. There is no migration path short of writing one by hand.
//
// So: choose a name once, deliberately, and never change it. Retitling the
// action (its i18n() text) is free and encouraged; renaming its id is not.
// ActionIdsTest freezes this table against a literal list precisely so that a
// rename shows up as a failing test rather than as a bug report six months
// later. If the test fails because you renamed something, that is the test
// working — put the name back.
//
// Adding is fine: append a constant, add it to all(), extend the test's frozen
// list, and register it in MainWindow. Every plan in the approved-features
// program (21 fleet, 22 extensions, 23 checkpoints, 24 questions, 25 export)
// adds its actions here rather than building them loose.
//
// The scheme is KDE's: lower_snake_case, prefixed by the surface the action
// belongs to — file_ / agent_ / options_ / view_ / layout_ / code_ / panel_.
// Four names (file_save, file_quit, options_show_menubar,
// options_configure_keybinding) are KStandardAction's own and MUST keep those
// spellings, because that is what KStandardAction::name() produces and what
// every other KDE application stores them under.
namespace ActionIds {

// --- File --------------------------------------------------------------
inline constexpr char FileOpenProject[] = "file_open_project";
inline constexpr char FileWelcomeScreen[] = "file_welcome_screen";
inline constexpr char FileResumeSession[] = "file_resume_session";
inline constexpr char FileSave[] = "file_save";         // KStandardAction
inline constexpr char FileSaveAll[] = "file_save_all";
inline constexpr char FileQuit[] = "file_quit";         // KStandardAction

// --- Agent -------------------------------------------------------------
inline constexpr char AgentNew[] = "agent_new";
inline constexpr char AgentRename[] = "agent_rename";
inline constexpr char AgentResume[] = "agent_resume";
inline constexpr char AgentAttachFiles[] = "agent_attach_files";
inline constexpr char AgentShowChanges[] = "agent_show_changes";
inline constexpr char AgentMergeChanges[] = "agent_merge_changes";
inline constexpr char AgentStop[] = "agent_stop";
inline constexpr char AgentCommit[] = "agent_commit";
inline constexpr char AgentCreatePullRequest[] = "agent_create_pull_request";
inline constexpr char AgentOpenTerminal[] = "agent_open_terminal";
inline constexpr char AgentEditTags[] = "agent_edit_tags";
inline constexpr char AgentDiscard[] = "agent_discard";
inline constexpr char AgentClose[] = "agent_close";
inline constexpr char AgentManageSkills[] = "agent_manage_skills";

// --- Options -----------------------------------------------------------
inline constexpr char OptionsTabsByProject[] = "options_tabs_by_project";
inline constexpr char OptionsTabsByAgent[] = "options_tabs_by_agent";
inline constexpr char OptionsEnterSends[] = "options_enter_sends";
inline constexpr char OptionsShowToolCalls[] = "options_show_tool_calls";
inline constexpr char OptionsAutosave[] = "options_autosave";
inline constexpr char OptionsConfigureProviders[] = "options_configure_providers";
inline constexpr char OptionsAppearance[] = "options_appearance";
inline constexpr char OptionsExperienceSimple[] = "options_experience_simple";
inline constexpr char OptionsExperienceAdvanced[] = "options_experience_advanced";
inline constexpr char OptionsLanguageExtensions[] = "options_language_extensions";
inline constexpr char OptionsShowMenubar[] = "options_show_menubar";       // KStandardAction
inline constexpr char OptionsConfigureKeyBinding[] = "options_configure_keybinding"; // KStandardAction

// --- View --------------------------------------------------------------
inline constexpr char ViewCommandPalette[] = "view_command_palette";
inline constexpr char ViewGitBlame[] = "view_git_blame";
inline constexpr char ViewToggleBottomPanel[] = "view_toggle_bottom_panel";
inline constexpr char ViewFindInProject[] = "view_find_in_project";
inline constexpr char ViewNextSearchMatch[] = "view_next_search_match";
inline constexpr char ViewPreviousSearchMatch[] = "view_previous_search_match";
inline constexpr char ViewNewTerminal[] = "view_new_terminal";
inline constexpr char ViewFocusTerminal[] = "view_focus_terminal";
inline constexpr char ViewNextTerminal[] = "view_next_terminal";
inline constexpr char ViewPreviousTerminal[] = "view_previous_terminal";
inline constexpr char ViewAgentTerminal[] = "view_agent_terminal";
inline constexpr char ViewCentreEditor[] = "view_centre_editor";
inline constexpr char ViewCentreSplit[] = "view_centre_split";
inline constexpr char ViewCentreChat[] = "view_centre_chat";
inline constexpr char ViewFocusEditor[] = "view_focus_editor";
inline constexpr char ViewFocusAgent[] = "view_focus_agent";

// --- Layout presets ----------------------------------------------------
inline constexpr char LayoutConverse[] = "layout_converse";
inline constexpr char LayoutBuild[] = "layout_build";
inline constexpr char LayoutReview[] = "layout_review";
inline constexpr char LayoutSplit[] = "layout_split";

// --- Code --------------------------------------------------------------
inline constexpr char CodeGotoDefinition[] = "code_goto_definition";
inline constexpr char CodeFindReferences[] = "code_find_references";
inline constexpr char CodeGotoSymbol[] = "code_goto_symbol";
inline constexpr char CodeQuickFix[] = "code_quick_fix";
inline constexpr char CodeRenameSymbol[] = "code_rename_symbol";
inline constexpr char CodeFormatDocument[] = "code_format_document";
inline constexpr char CodeFormatOnSave[] = "code_format_on_save";
inline constexpr char CodeSignatureHelp[] = "code_signature_help";
inline constexpr char CodeNextProblem[] = "code_next_problem";
inline constexpr char CodePreviousProblem[] = "code_previous_problem";
inline constexpr char CodeRestartLanguageServer[] = "code_restart_language_server";

// --- Activity rail -----------------------------------------------------
// The rail accelerators are POSITIONAL — Alt+3 raises whatever panel is third
// in the left strip, not a particular panel — so their ids are generated per
// (strip, ordinal) rather than written out per panel. A panel moved between
// strips changes which id reaches it, which is exactly right: the binding
// belongs to the slot, not to the tenant.
//
// kRailOrdinals is the count bindRaise() actually binds; the tooltip builder
// and all() both read it so the three cannot drift.
inline constexpr int kRailOrdinals = 9;

// railRaise("left", 3) -> "panel_raise_left_3". ordinal is 1-based.
inline QString railRaise(bool leftBar, int ordinal)
{
    return QStringLiteral("panel_raise_%1_%2")
        .arg(leftBar ? QStringLiteral("left") : QStringLiteral("right"))
        .arg(ordinal);
}

inline QString railCollapse(bool leftBar)
{
    return leftBar ? QStringLiteral("panel_collapse_left")
                   : QStringLiteral("panel_collapse_right");
}

// all() is every id this application registers, in registration order. The
// collection is the runtime authority; this is the compile-time one, and
// ActionIdsTest asserts they agree.
inline QStringList all()
{
    QStringList ids{
        QLatin1String(FileOpenProject),
        QLatin1String(FileWelcomeScreen),
        QLatin1String(FileResumeSession),
        QLatin1String(FileSave),
        QLatin1String(FileSaveAll),
        QLatin1String(FileQuit),

        QLatin1String(AgentNew),
        QLatin1String(AgentRename),
        QLatin1String(AgentResume),
        QLatin1String(AgentAttachFiles),
        QLatin1String(AgentShowChanges),
        QLatin1String(AgentMergeChanges),
        QLatin1String(AgentStop),
        QLatin1String(AgentCommit),
        QLatin1String(AgentCreatePullRequest),
        QLatin1String(AgentOpenTerminal),
        QLatin1String(AgentEditTags),
        QLatin1String(AgentDiscard),
        QLatin1String(AgentClose),
        QLatin1String(AgentManageSkills),

        QLatin1String(OptionsTabsByProject),
        QLatin1String(OptionsTabsByAgent),
        QLatin1String(OptionsEnterSends),
        QLatin1String(OptionsShowToolCalls),
        QLatin1String(OptionsAutosave),
        QLatin1String(OptionsConfigureProviders),
        QLatin1String(OptionsAppearance),
        QLatin1String(OptionsExperienceSimple),
        QLatin1String(OptionsExperienceAdvanced),
        QLatin1String(OptionsLanguageExtensions),
        QLatin1String(OptionsShowMenubar),
        QLatin1String(OptionsConfigureKeyBinding),

        QLatin1String(ViewCommandPalette),
        QLatin1String(ViewGitBlame),
        QLatin1String(ViewToggleBottomPanel),
        QLatin1String(ViewFindInProject),
        QLatin1String(ViewNextSearchMatch),
        QLatin1String(ViewPreviousSearchMatch),
        QLatin1String(ViewNewTerminal),
        QLatin1String(ViewFocusTerminal),
        QLatin1String(ViewNextTerminal),
        QLatin1String(ViewPreviousTerminal),
        QLatin1String(ViewAgentTerminal),
        QLatin1String(ViewCentreEditor),
        QLatin1String(ViewCentreSplit),
        QLatin1String(ViewCentreChat),
        QLatin1String(ViewFocusEditor),
        QLatin1String(ViewFocusAgent),

        QLatin1String(LayoutConverse),
        QLatin1String(LayoutBuild),
        QLatin1String(LayoutReview),
        QLatin1String(LayoutSplit),

        QLatin1String(CodeGotoDefinition),
        QLatin1String(CodeFindReferences),
        QLatin1String(CodeGotoSymbol),
        QLatin1String(CodeQuickFix),
        QLatin1String(CodeRenameSymbol),
        QLatin1String(CodeFormatDocument),
        QLatin1String(CodeFormatOnSave),
        QLatin1String(CodeSignatureHelp),
        QLatin1String(CodeNextProblem),
        QLatin1String(CodePreviousProblem),
        QLatin1String(CodeRestartLanguageServer),
    };
    for (bool left : {true, false}) {
        for (int i = 1; i <= kRailOrdinals; ++i) {
            ids << railRaise(left, i);
        }
        ids << railCollapse(left);
    }
    return ids;
}

} // namespace ActionIds
