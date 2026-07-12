// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "theme/Theme.h"

#include <QList>
#include <QObject>
#include <QString>
#include <QStringList>

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

    // The syntax-highlighting theme name that the *interface* theme prefers, or
    // empty to pick a sensible default by light/dark. This is the "match the
    // interface" value; most callers want editorSyntaxTheme() instead.
    QString syntaxTheme() const { return m_syntaxTheme; }

    // The editor syntax theme, chosen independently of the interface palette.
    // An empty id means "match the interface" (follow syntaxTheme()); a non-empty
    // id names a specific KSyntaxHighlighting theme to use everywhere code is
    // highlighted (editor, diff view, tool inspector).
    QString editorThemeId() const { return m_editorThemeId; }

    // The concrete syntax-highlighting theme name that editors and highlighted
    // panes should use: the explicit editor-theme override when set, otherwise
    // the interface theme's syntax theme, falling back to a light/dark default so
    // this is always a valid, non-empty theme name.
    QString editorSyntaxTheme() const;

    // Set the editor syntax theme. An empty id restores "match the interface".
    // Persists to KConfig's [Appearance] group unless `persist` is false (used
    // for live preview), then emits changed() so open views re-theme.
    void setEditorTheme(const QString &id, bool persist = true);

    // Every syntax-highlighting theme name the user can pick, sorted for display.
    static QStringList availableEditorThemes();

    // The terminal (Konsole) profile, chosen independently of the interface.
    // An empty id means "match the interface" — the Agent Kate terminal profile
    // whose light/dark matches the current interface theme. A non-empty id names
    // a specific Konsole profile to use for every terminal session.
    QString terminalProfileId() const { return m_terminalProfileId; }

    // The concrete Konsole profile name terminals should use: the explicit
    // override when set, otherwise the Agent Kate profile matching the interface
    // theme's light/dark. Always a non-empty profile name.
    QString effectiveTerminalProfile() const;

    // Set the terminal profile. An empty id restores "match the interface".
    // Persists to KConfig's [Appearance] group unless `persist` is false (live
    // preview), then emits changed() so open terminal sessions re-profile.
    void setTerminalProfile(const QString &id, bool persist = true);

    // Every Konsole profile name the user can pick, sorted. Always includes the
    // two bundled Agent Kate profiles plus any profile installed on the system.
    static QStringList availableTerminalProfiles();

    // The bundled Agent Kate terminal profile names (dark / light).
    static QString midnightTerminalProfile();
    static QString daylightTerminalProfile();

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
    QString m_syntaxTheme;     // syntax theme the interface theme prefers
    QString m_editorThemeId;   // editor-theme override; empty == match interface
    QString m_terminalProfileId; // terminal-profile override; empty == match interface
};
