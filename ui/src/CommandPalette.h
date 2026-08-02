// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QList>
#include <QPointer>
#include <QString>
#include <QVector>

class QAction;
class QLineEdit;
class QListWidget;
class QListWidgetItem;

// CommandPalette is a VS-Code-style, fuzzy-searchable popup that lists every
// command in the application alongside its keyboard shortcut. It is the single
// discoverable entry point for advanced functions that would otherwise clutter
// menus and toolbars: the user opens it (typically via Ctrl+Shift+P / Ctrl+P),
// types a few characters of what they want, and triggers it with Enter.
//
// The palette is deliberately self-contained. It depends only on Qt/KF6 and a
// caller-supplied list of Entry; it knows nothing about MainWindow or any other
// application internals. The orchestrator builds that list — in this app, from
// the window's one KActionCollection plus the commands panels publish through
// MainWindow::registerCommands, which is why an action that appears in no menu
// is still listed — and calls setActions() + showPalette(). When a command is
// chosen the palette closes *first* and then triggers the action, so the action
// runs against the underlying window rather than the (now-gone) popup.
//
// Matching is a case-insensitive fuzzy subsequence search over the display
// text, ranked so that the most intuitive results float to the top: exact
// prefix > word-boundary subsequence > contiguous substring > loose
// subsequence. Up/Down (and Ctrl+N / Ctrl+P) move the selection while focus
// stays in the search field; Enter triggers; Esc closes.
class CommandPalette : public QDialog
{
    Q_OBJECT
public:
    explicit CommandPalette(QWidget *parent = nullptr);

    // One command offered to the palette.
    //
    // `group` is an optional surface name ("Problems", "Terminal") prefixed to
    // the display text so a panel's commands read as a namespace and can be
    // found by typing the panel's name.
    //
    // `advanced` marks a command the application currently HIDES — Agent Kate's
    // Simple mode hides the Code menu and the developer-only View items. Those
    // are still listed here, tagged, deliberately: the palette is meant to be
    // the escape hatch out of Simple mode, and a search that silently returns
    // nothing for "format document" is a second wall rather than a way through.
    // `available` is whether the command can actually run, and it exists only
    // because of a Qt behaviour that would otherwise be a security hole here:
    // QAction::setVisible(false) clears the action's `enabled` flag as a SIDE
    // EFFECT (restoring the app's real intent on setVisible(true)), so a hidden
    // action cannot be asked whether it would work — it always answers "no".
    // Treating that answer as "drop it" is what made Simple mode a wall;
    // ignoring it entirely would let the palette run a command the menu bar
    // refuses to offer, which for this application means Create Pull Request on
    // an agent with no branch and Discard on one with nothing to discard. So
    // for a HIDDEN action the caller states the answer; for a visible one the
    // action is asked directly and this field is ignored.
    struct Entry {
        QAction *action = nullptr;
        QString group;
        bool advanced = false;
        bool available = true;
    };

    // Item-data roles on the result rows. Public so a test can assert what the
    // palette decided about a row without reimplementing the delegate.
    static constexpr int ShortcutRole = Qt::UserRole + 1;
    static constexpr int CommandIndexRole = Qt::UserRole + 2;
    static constexpr int AdvancedRole = Qt::UserRole + 3;

    // Provide the commands to show. The palette filters/sorts these; invalid,
    // separator, disabled, or empty-text actions are skipped, and duplicates
    // (same display text + shortcut) are collapsed. An INVISIBLE action is kept
    // and tagged Advanced rather than dropped — see Entry::advanced.
    void setActions(const QList<Entry> &entries);

    // Show centred horizontally near the top of the parent, clearing and
    // focusing the search box and re-running the filter from scratch.
    void showPalette();

protected:
    void showEvent(QShowEvent *event) override;
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    // One candidate command. We keep our own cleaned display text and a guarded
    // pointer back to the source action so we can trigger it later.
    struct Command {
        QString text;       // mnemonics stripped, ready to display
        QString shortcut;   // native-text shortcut, may be empty
        QString lowerText;  // cached lower-case display text for matching
        bool advanced = false; // hidden by Simple mode — listed, but tagged
        QPointer<QAction> action;
    };

    void rebuildList(const QString &query);
    void moveSelection(int delta);
    void triggerCurrent();
    QAction *actionForRow(int row) const;

    QLineEdit *m_search = nullptr;
    QListWidget *m_list = nullptr;
    QVector<Command> m_commands;
};
