// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>

class QPlainTextEdit;
class QComboBox;
class QCheckBox;
class QWidget;

// The choices a guided New Agent dialog collects. Empty strings mean "leave the
// agent's sticky default" (the data values match AgentPanel's combos).
struct NewAgentChoices {
    QString task;           // optional first task, pre-filled into the composer
    QString backend;        // claude | kimi
    QString modelId;        // "" | opus | sonnet | haiku | fable
    QString isolation;      // auto | isolated | workspace
    QString permissionMode; // acceptEdits | default | auto | bypassPermissions
    QString effort;         // "" | low | medium | high | xhigh | max
};

// NewAgentDialog — a friendly front door for starting an agent: describe the
// task in plain words, pick how clever it should be and whether it works in a
// private copy, with the power options tucked behind "Advanced". It only
// collects choices; the caller creates the agent and pre-fills the task.
class NewAgentDialog : public QDialog
{
    Q_OBJECT
public:
    explicit NewAgentDialog(const QString &projectName, QWidget *parent = nullptr);

    NewAgentChoices choices() const;

private:
    QPlainTextEdit *m_task = nullptr;
    QComboBox *m_backend = nullptr;
    QComboBox *m_model = nullptr;
    QCheckBox *m_sandbox = nullptr;
    QWidget *m_advanced = nullptr;
    QComboBox *m_permission = nullptr;
    QComboBox *m_effort = nullptr;
};
