// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QString>

namespace agentkate
{
// neutralizeMarkdownRawHtml escapes the '<' characters that CommonMark would
// treat as raw inline/block HTML, replacing them with the &lt; entity so they
// render as literal text instead.
//
// Why this exists: QTextDocument::setMarkdown() runs md4c with raw-HTML enabled
// and no way to disable it. CommonMark accepts e.g. "<T>" as an HTML open tag,
// and Qt's markdown importer then *drops the surrounding text* — so an assistant
// message like "Reactive<T> with equality guards" collapses to "Reactive". The
// same bite hits C++ LSP hovers ("std::vector<int>") and markdown previews.
//
// Code spans, fenced/indented code blocks, and backslash escapes are left
// verbatim: there '<' is already literal and round-trips correctly, and escaping
// it would surface a stray "&lt;". Pass the result to setMarkdown() in place of
// the raw source.
QString neutralizeMarkdownRawHtml(const QString &md);
} // namespace agentkate
