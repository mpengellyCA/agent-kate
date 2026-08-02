// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QString>

// DraftStore names, and forgets, the composer drafts AgentPanel persists.
//
// A draft is written to KConfig [Agent] under a key derived from the agent's
// identity, so that closing the app does not lose a half-written task. Nothing
// ever removed those entries for an agent that was CLOSED with text in its
// composer, so a config file accumulated one dead `draft-…` line per closed
// agent, for the life of the profile (audit F50, "draft entries leak").
//
// The key derivation lives here rather than in AgentPanel because the agent
// that must forget a draft (AgentDock, tearing an entry down) is not the one
// that wrote it. AgentPanel::draftKey() still has its own copy of these two
// rules; folding it onto this module is a small, obvious follow-up and the
// reason the functions are shaped exactly like its private helper.
namespace DraftStore {

// Key for an agent that has started (has a core thread). Empty threadId → "".
QString threadKey(const QString &threadId);

// Key for an agent that has NOT started yet, scoped to its workspace path.
// NOTE: every not-yet-started agent in the same project shares this key, so a
// caller must only clear it once the last of them is gone. Empty path → "".
QString workspaceKey(const QString &workspacePath);

// Delete one draft entry. An empty key is a no-op, and so is a key with no
// stored draft — clearing is idempotent on purpose, since the two keys above
// are both plausible for one agent and the caller knows which apply.
void clear(const QString &key);

} // namespace DraftStore
