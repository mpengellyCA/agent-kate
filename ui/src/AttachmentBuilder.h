// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStringList>

class QJsonArray;

// Attachment-building helpers split out of AgentPanel: the file-reading, binary
// sniffing, image base64-encoding and ranged-excerpt extraction that turn local
// paths into the {kind,name,mediaType,…} attachment objects the next message
// carries. None of this touches the panel's widget tree — it reads files and
// mutates a QJsonArray — so it lives here as free functions. The panel keeps the
// chip UI (rebuildAttachChips) and the rejection banner (showAttachNotice); these
// builders only assemble the data and report which files were skipped and why.
namespace agentkate
{
// buildPathAttachments appends an attachment object for each readable path in
// `paths` to `attachments`, de-duplicating against entries already present.
// `workspace` (may be empty) is used only to flag paths outside the project root
// with an "outside" marker. Returns the human-readable reasons any files were
// skipped (binary, too large, unreadable, a folder), for the caller's banner.
QStringList buildPathAttachments(const QStringList &paths, const QString &workspace,
                                 QJsonArray &attachments);

// buildItemAttachments appends a ranged text-excerpt attachment for each item in
// `items` that carries a "line" key (widening by a few lines of context each
// side); items without a line range are collected into `wholeFile` for the
// caller to route through buildPathAttachments. De-duplicates by display name.
// Returns the human-readable reasons any items were skipped, for the caller's
// banner.
QStringList buildItemAttachments(const QJsonArray &items, QJsonArray &attachments,
                                 QStringList &wholeFile);
} // namespace agentkate
