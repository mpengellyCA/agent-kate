// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>

class QPlainTextEdit;
class QComboBox;
class QCheckBox;
class QWidget;
class CoreClient;

// The choices a guided New Agent dialog collects. Empty strings mean "leave the
// agent's sticky default" (the data values match AgentPanel's combos).
struct NewAgentChoices {
    QString task;           // optional first task, pre-filled into the composer
    QString backend;        // harness id from the engine picker (claude, kimi, …)
    QString providerId;     // optional provider overlay; "" = direct
    QString modelId;        // tier token / discovered model id; "" = default
    QString isolation;      // auto | isolated | workspace
    QString permissionMode; // the harness's own mode vocabulary; "" = default
    QString effort;         // the harness's own effort vocabulary; "" = default
};

// NewAgentDialog — a friendly front door for starting an agent: describe the
// task in plain words, pick how clever it should be and whether it works in a
// private copy, with the power options tucked behind "Advanced". It only
// collects choices; the caller creates the agent and pre-fills the task.
class NewAgentDialog : public QDialog
{
    Q_OBJECT
public:
    explicit NewAgentDialog(const QString &projectName, CoreClient *core,
                            QWidget *parent = nullptr);

    NewAgentChoices choices() const;

private:
    CoreClient *m_core = nullptr; // for the lazy discovered-option probe
    QPlainTextEdit *m_task = nullptr;
    QComboBox *m_engine = nullptr; // harness + optional provider overlay
    QComboBox *m_model = nullptr;
    QCheckBox *m_sandbox = nullptr;
    QWidget *m_advanced = nullptr;
    QComboBox *m_permission = nullptr;
    QComboBox *m_effort = nullptr;
};
