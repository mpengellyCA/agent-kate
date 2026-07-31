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
} // namespace agentkate
