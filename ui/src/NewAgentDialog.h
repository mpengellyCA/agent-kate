// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>
#include <QStringList>

class QPlainTextEdit;
class QComboBox;
class QCheckBox;
class QLabel;
class QLineEdit;
class QFormLayout;
class QWidget;
class CoreClient;

// The choices a guided New Agent dialog collects. Empty strings mean "leave the
// agent's sticky default" (the data values match AgentPanel's combos).
struct NewAgentChoices {
    QString task;           // optional first task, pre-filled into the composer
    // ensemble names a controller/worker recipe (plan 16 P4) instead of a single
    // agent. When set, every field below except task is the ensemble's business:
    // the caller applies it core-side (mode.apply) rather than creating a panel.
    QString ensemble;
    QString backend;        // harness id from the engine picker (claude, kimi, …)
    QString providerId;     // optional provider overlay; "" = direct
    QString modelId;        // tier token / discovered model id; "" = default
    QString isolation;      // auto | isolated | workspace
    QString permissionMode; // the harness's own mode vocabulary; "" = default
    QString effort;         // the harness's own effort vocabulary; "" = default
    // The launch-option sweep (plan 16 P6), each offered only when the chosen
    // engine declares the capability. Empty lists mean "not requested".
    QStringList fallbackModels;  // models to fall back to, in order
    QStringList disallowedTools; // tool names this agent may not use
    QStringList addDirs;         // extra directories its tools may reach
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
    // Enable/disable the single-agent pickers: an ensemble brings its own
    // controller engine, model and options, so leaving them live would offer
    // choices the launch ignores.
    void applyEnsembleMode();

    CoreClient *m_core = nullptr; // for the lazy discovered-option probe
    QPlainTextEdit *m_task = nullptr;
    QComboBox *m_ensemble = nullptr; // "Single agent" or one ensemble
    QLabel *m_ensembleHint = nullptr;
    QComboBox *m_engine = nullptr; // harness + optional provider overlay
    QComboBox *m_model = nullptr;
    QCheckBox *m_sandbox = nullptr;
    QWidget *m_advanced = nullptr;
    QComboBox *m_permission = nullptr;
    QComboBox *m_effort = nullptr;
    // Sweep fields + their form rows, so a row can be hidden entirely for an
    // engine that cannot express it (offering it would be a lie).
    QLineEdit *m_fallbackModels = nullptr;
    QLineEdit *m_disallowedTools = nullptr;
    QLineEdit *m_addDirs = nullptr;
    QFormLayout *m_advancedForm = nullptr;
};
