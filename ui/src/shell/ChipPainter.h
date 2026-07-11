// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QColor>
#include <QFont>
#include <QRect>
#include <QString>

class QPainter;

// ChipPainter — the shared rounded-pill/chip renderer used by roster and
// worktree cards (plan 13 phases 7 & 9). All colours are passed in by the
// caller (resolved from QPalette / ThemeManager) so the helper never hardcodes
// a theme; geometry is font-metric-relative at call time so chips survive font
// scaling and HiDPI.
//
// Two entry points:
//   - drawChip(): paint a single chip and return its width, for callers that
//     lay out chips themselves (worktree status pills).
//   - drawChipRow(): lay out a left-to-right run of text chips with a "+N"
//     overflow chip when the row runs out of width, matching the roster's tag
//     row behaviour.
namespace ChipPainter {

// Fixed paddings (kept small; everything else is derived from font metrics).
constexpr int kChipHPad = 6;   // horizontal text inset inside a chip
constexpr int kChipVPad = 1;   // vertical text inset inside a chip
constexpr int kChipGap = 4;    // gap between adjacent chips
constexpr int kChipRadius = 4; // corner radius

// Height a chip drawn with `font` occupies (text height + vertical padding).
int chipHeight(const QFont &font);

// Width the chip for `text` in `font` occupies (text advance + horizontal
// padding). Use to pre-measure a chip before deciding whether it fits.
int chipWidth(const QFont &font, const QString &text);

// Paint one chip at `rect` (its size should come from chipWidth/chipHeight).
// `fill` is the pill background; when `outline` is true the pill is drawn as a
// 1px outline in `textColor` instead of a fill (used on selected rows where a
// filled Highlight chip would be invisible). Text is centred in `textColor`.
void drawChip(QPainter *painter, const QRect &rect, const QString &text,
              const QFont &font, const QColor &fill, const QColor &textColor,
              bool outline = false);

// Lay out `chips` left-to-right starting at (x, top) within [x, rightEdge].
// When the next chip would overflow and more than one remains, a "+N" chip is
// drawn in its place and layout stops. `outline` matches drawChip. Returns the
// x just past the last chip drawn.
int drawChipRow(QPainter *painter, int x, int top, int rightEdge,
                const QStringList &chips, const QFont &font, const QColor &fill,
                const QColor &textColor, bool outline = false);

} // namespace ChipPainter
