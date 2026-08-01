// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QTextDocument>

class QString;

namespace agentkate
{
// Rendering MODEL-AUTHORED markdown into a QTextDocument, with raw HTML disabled
// AT THE PARSER.
//
// Two problems, one root cause. QTextDocument::setMarkdown() runs md4c, and by
// default md4c recognises raw HTML; Qt's importer then feeds it to the rich-text
// engine ("handled in the same way as in setHtml"). So:
//
//   * Correctness: CommonMark reads "<T>" as an HTML open tag, and the importer
//     drops the surrounding text — "Reactive<T> with equality guards" rendered
//     as "Reactive". Same bite on C++ LSP hovers ("std::vector<int>") and on
//     markdown previews.
//   * Security: everything in the transcript is repository content refracted
//     through an agent. Raw HTML there is arbitrary Qt rich text — colours,
//     tables, images — on the very surface where the human reads what they are
//     about to approve. A convincing fake "Allow" row is a phish, not a typo.
//
// WHY THE PARSER FLAG AND NOT A PRE-PASS. This used to be a hand-written
// CommonMark scanner that escaped '<' everywhere it believed raw HTML would be
// live, leaving fences, indented code and code spans verbatim. Every audit round
// found another place where that scanner and md4c disagreed about block
// structure — a backtick in a fence info string, a fence opened inside a list
// item, an indented-code floor measured from the wrong column — and each
// disagreement was a hole, because "we think this is code" meant "copy it
// verbatim" and md4c thought it was a paragraph. Three rounds, three new
// divergences: the bug was not any particular rule, it was owning a SECOND
// PARSER that has to agree with md4c to be safe.
//
// Qt exposes md4c's own MD_FLAG_NOHTMLBLOCKS|MD_FLAG_NOHTMLSPANS as
// MarkdownNoHTML. With it, the one parser in the pipeline never recognises HTML
// at all: '<' is ordinary text, delivered through the text callback and inserted
// with QTextCursor::insertText. There is nothing left to disagree with, so the
// property holds BY CONSTRUCTION rather than by imitation — and, as a bonus, the
// text is byte-exact (no scanner means no over-escaping: code blocks, code
// spans, backslash escapes and "<http://…>" autolinks all render as written).
//
// Entities are the one thing md4c still translates under this flag ("&lt;" ->
// "<"). That is safe and verified: the entity token has no '<' of its own, so it
// can only ever produce a literal character in the text, never an element.
//
// THE RULE: model-authored markdown goes through setMarkdownSafe(). Never call
// QTextDocument::setMarkdown() directly on it — the default features re-enable
// raw HTML. MarkdownUtilTest enforces this over the whole source tree.
inline constexpr QTextDocument::MarkdownFeatures kSafeMarkdownFeatures =
    QTextDocument::MarkdownFeatures(QTextDocument::MarkdownDialectGitHub)
    | QTextDocument::MarkdownNoHTML;

// Parse `md` into `doc` with the GitHub dialect and raw HTML disabled.
void setMarkdownSafe(QTextDocument &doc, const QString &md);
} // namespace agentkate
