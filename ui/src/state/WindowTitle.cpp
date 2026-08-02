// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WindowTitle.h"

#include <KLocalizedString>

namespace WindowTitle {

QString compose(const QString &projectName, int attentionCount)
{
    const QString base = projectName.isEmpty()
        ? i18n("Agent Kate")
        : i18nc("window title: application — active project", "Agent Kate — %1",
                projectName);
    if (attentionCount <= 0) {
        return base;
    }
    // The count leads, so it survives task-bar and window-list truncation —
    // which is the only place this signal is read from at a glance.
    return i18nc("window title prefix: how many agents are waiting on the human",
                 "(%1) %2", attentionCount, base);
}

} // namespace WindowTitle
