// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QString>

// WindowTitle composes the main window's title from its parts, in one place.
//
// It exists because there were two writers and no composition: the attention
// counter wrote "(3) Agent Kate" when agents blocked, and selecting an agent
// wrote "Agent Kate — myproject" straight over it. The counter's emitter is
// change-gated, so the count was never re-announced — answering the prompt was
// the only thing that could clear a number the user could no longer see. Every
// title change now goes through compose(), so neither part can erase the other.
namespace WindowTitle {

// projectName is the active project's directory name ("" before one is open);
// attentionCount is how many agents are waiting on the human (0 = none).
QString compose(const QString &projectName, int attentionCount);

} // namespace WindowTitle
