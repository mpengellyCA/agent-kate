// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "MarkdownUtil.h"

#include <QString>

namespace agentkate
{
// One line, and that is the point: the safety property lives in md4c's own
// NOHTML flags (see MarkdownUtil.h), not in a second parser of ours that has to
// keep agreeing with md4c about where code blocks begin and end. This function
// exists so there is exactly ONE call to QTextDocument::setMarkdown() in the
// tree, and so forgetting the flag is a compile-time impossibility rather than a
// review item.
void setMarkdownSafe(QTextDocument &doc, const QString &md)
{
    doc.setMarkdown(md, kSafeMarkdownFeatures);
}
} // namespace agentkate
