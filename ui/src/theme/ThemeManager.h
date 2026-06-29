// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "theme/Theme.h"

#include <QList>
#include <QObject>
#include <QString>

// ThemeManager — the single owner of Agent Kate's appearance.
//
// It keeps a catalog of selectable themes (the built-in Agent Kate themes plus
// every colour scheme installed on the system), applies the active one to the
// whole QApplication, and persists the choice to KConfig's [Appearance] group.
//
// The point of routing appearance through one object is *override*: Agent Kate
// can wear its own identity (the signature "Midnight" theme) regardless of the
// rest of the desktop, or deliberately follow a *different* KDE scheme than
// every other app — without the user touching System Settings.
//
// Usage:
//   - main() calls applySavedOrDefault() once, before any window is shown.
//   - The Appearance dialog calls themes()/applyTheme().
//   - Delegates read ThemeManager::colors() for semantic, app-specific colours.
class ThemeManager : public QObject
{
    Q_OBJECT
public:
    static ThemeManager *instance();

    // The full catalog: built-in Agent Kate themes first, then a separator-free
    // list of installed KDE colour schemes. Cheap to call repeatedly.
    QList<AkThemeDef> themes() const;
    AkThemeDef themeById(const QString &id) const;

    QString currentId() const { return m_currentId; }
    const AkColors &colors() const { return m_colors; }
    bool isDark() const { return m_colors.dark; }

    // The syntax-highlighting theme name to use, or empty to pick a sensible
    // default by light/dark. Editors and the diff view honour this.
    QString syntaxTheme() const { return m_syntaxTheme; }

    // The default theme id used on first run / when the config is empty.
    static QString defaultId();

    // Apply `id` to the running application now. Persists the choice unless
    // `persist` is false (used for live-preview in the dialog).
    void applyTheme(const QString &id, bool persist = true);

    // Read the saved choice (or the default) and apply it. Call once at startup.
    void applySavedOrDefault();

    // Convenience for leaf delegates that only need the semantic colours.
    static const AkColors &palette() { return instance()->colors(); }

Q_SIGNALS:
    // Emitted after a theme is applied. Most widgets repaint automatically from
    // the QApplication palette change; this is for the few that cache colours.
    void changed();

private:
    explicit ThemeManager(QObject *parent = nullptr);

    void rebuildCatalog();
    AkThemeDef resolve(const QString &id) const; // fills palette/colors for the id

    QList<AkThemeDef> m_builtins;
    QString m_currentId;
    AkColors m_colors;
    QString m_syntaxTheme;
};
