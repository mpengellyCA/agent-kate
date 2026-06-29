// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

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

// toolResultText pulls plain text out of a tool_result content value, which may
// be a bare string or an array of content blocks.
QString toolResultText(const QJsonValue &content);

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
} // namespace agentkate
