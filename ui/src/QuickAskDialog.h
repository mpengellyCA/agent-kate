// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>

class ElidingLabel;
class QLabel;
class QLineEdit;

// QuickAskDialog is the Meta+Shift+A surface (plan 27 §3): a small frameless
// always-on-top prompt line — KRunner for your agent — that sends one message
// to the last-focused agent WITHOUT switching windows.
//
// It owns no protocol and no send logic. Enter emits submitted(); the window
// routes that through AgentDock's existing composer send path and reports
// back with acceptSent() (sent — close and clear) or showError() (refused —
// stay open, keep the text, say why the window is worth opening). Escape
// closes, discarding nothing anywhere but this line edit.
class QuickAskDialog : public QDialog
{
    Q_OBJECT
public:
    explicit QuickAskDialog(QWidget *parent = nullptr);

    // Name the target agent in the header (ElidingLabel — an agent title is
    // user text of any length and must never widen this popup).
    void setTargetName(const QString &name);
    // Show + raise + focus the line edit, centred on the current screen.
    void popUp();
    // The send succeeded (or was queued): clear the line and close.
    void acceptSent();
    // The send was refused before any side effect: keep the text, show why.
    void showError(const QString &message);

Q_SIGNALS:
    void submitted(const QString &text);

private:
    ElidingLabel *m_target = nullptr;
    QLabel *m_error = nullptr;
    QLineEdit *m_edit = nullptr;
};
