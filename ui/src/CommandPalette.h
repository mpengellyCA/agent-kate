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
// caller-supplied list of QAction*; it knows nothing about MainWindow or any
// other application internals. The orchestrator builds the action list (from
// menus, KStandardAction, custom shortcuts, …) and calls setActions() +
// showPalette(). When a command is chosen the palette closes *first* and then
// triggers the action, so the action runs against the underlying window rather
// than the (now-gone) popup.
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

    // Provide the commands to show. The palette filters/sorts these; invalid,
    // separator, invisible, disabled, or empty-text actions are skipped, and
    // duplicates (same display text + shortcut) are collapsed.
    void setActions(const QList<QAction *> &actions);

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
