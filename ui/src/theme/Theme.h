// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QColor>
#include <QPalette>
#include <QString>
#include <QVector>
#include <cmath>

inline double akColorContrastRatio(const QColor &foreground, const QColor &background)
{
    const auto luminance = [](const QColor &color) {
        const auto linear = [](qreal channel) {
            return channel <= 0.04045 ? channel / 12.92
                                      : std::pow((channel + 0.055) / 1.055, 2.4);
        };
        return 0.2126 * linear(color.redF()) + 0.7152 * linear(color.greenF())
            + 0.0722 * linear(color.blueF());
    };
    const double a = luminance(foreground);
    const double b = luminance(background);
    return (qMax(a, b) + 0.05) / (qMin(a, b) + 0.05);
}

// AkColors — the semantic, app-specific colours that QPalette does not cover.
//
// Delegates and panels read these (via ThemeManager::colors()) instead of
// hardcoding light/dark hex pairs per file, so a single theme switch recolours
// the whole app consistently. Every colour here is resolved once per theme and
// is valid for both the built-in Agent Kate themes and the "follow the system
// scheme" mode (where they are derived from KColorScheme at apply time).
struct AkColors {
    bool dark = true;

    QColor accent;       // the brand accent — focus rings, primary action, active agent
    QColor accentText;   // readable text drawn on top of `accent`

    QColor positive;     // success / added / "all good"
    QColor negative;     // error / removed / failure
    QColor neutral;      // warning / caution
    QColor info;         // hint / informational

    QColor addedBg;      // diff: inserted line background
    QColor removedBg;    // diff: deleted line background
    QColor hunkBg;       // diff: @@ hunk-header background

    QColor agentRunning; // roster status dot while a turn is in flight
    QColor agentIdle;    // roster status dot when idle / dormant

    // Transcript surfaces. These are semantic tokens, never fixed colours in
    // the delegate, so a KDE/system palette keeps its own character.
    QColor chatAssistantSurface;
    QColor chatUserSurface;
    QColor chatActivitySurface;
    QColor chatCodeSurface;
    QColor chatBorder;
    QColor chatRail;
    QColor chatMetadata;
    QColor chatAttachmentSurface;

    QVector<QColor> lanes; // git-graph lane hues (>= 6, stable across scroll)

    // Lane hue for graph column `i`, wrapping the palette. Falls back to the
    // accent if no lane palette was provided.
    QColor lane(int i) const
    {
        if (lanes.isEmpty())
            return accent;
        const int n = lanes.size();
        return lanes[((i % n) + n) % n];
    }
};

// AkThemeDef — a complete, selectable theme.
struct AkThemeDef {
    // How the theme paints the application chrome.
    enum Kind {
        BuiltinPalette, // Agent Kate's own coded QPalette (Midnight / Daylight)
        FollowSystem,   // do not override — use whatever KDE hands us
        KdeScheme,      // activate an installed KDE colour scheme by name
    };

    QString id;            // stable key persisted to KConfig
    QString name;          // human display name
    QString description;   // one-line blurb for the picker
    Kind kind = BuiltinPalette;
    QString kdeSchemeName; // for KdeScheme: the installed scheme's name
    bool dark = true;
    QString syntaxTheme;   // KSyntaxHighlighting theme name ("" = pick by dark/light)
    QPalette palette;      // for BuiltinPalette
    AkColors colors;       // semantic colours (resolved lazily for FollowSystem)
    bool builtin = true;   // false for KDE schemes discovered at runtime
};
