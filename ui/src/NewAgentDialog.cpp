// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "NewAgentDialog.h"

#include <KLocalizedString>

#include <QCheckBox>
#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QLabel>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QVBoxLayout>

NewAgentDialog::NewAgentDialog(const QString &projectName, QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18nc("@title:window", "New Agent"));

    auto *root = new QVBoxLayout(this);
    root->setSpacing(12);

    auto *heading = new QLabel(
        projectName.isEmpty()
            ? i18n("Start a new agent")
            : i18n("Start a new agent in <b>%1</b>", projectName.toHtmlEscaped()),
        this);
    heading->setTextFormat(Qt::RichText);
    root->addWidget(heading);

    // The task — the one thing a newcomer actually wants to express.
    auto *taskLabel = new QLabel(i18n("What should this agent do?"), this);
    root->addWidget(taskLabel);
    m_task = new QPlainTextEdit(this);
    m_task->setPlaceholderText(
        i18n("Describe the task in your own words — you can refine it once the agent starts."));
    m_task->setMinimumHeight(96);
    root->addWidget(m_task);

    // The two choices most people care about, in plain language.
    auto *form = new QFormLayout;
    form->setLabelAlignment(Qt::AlignLeft);
    m_model = new QComboBox(this);
    m_model->addItem(i18n("Use my default"), QString());
    m_model->addItem(i18n("Smartest (Opus)"), QStringLiteral("opus"));
    m_model->addItem(i18n("Balanced (Sonnet)"), QStringLiteral("sonnet"));
    m_model->addItem(i18n("Fastest (Haiku)"), QStringLiteral("haiku"));
    m_model->setToolTip(i18n("Which model powers this agent. Smarter is more capable; faster is cheaper."));
    form->addRow(i18n("How clever?"), m_model);
    root->addLayout(form);

    m_sandbox = new QCheckBox(
        i18n("Work in a private copy, so changes don't touch my files until I approve"), this);
    m_sandbox->setChecked(true);
    m_sandbox->setToolTip(
        i18n("Recommended. The agent works in its own sandbox (a git worktree); "
             "you merge its changes back when you're happy with them."));
    root->addWidget(m_sandbox);

    // Power options, hidden until asked for.
    auto *advToggle = new QCheckBox(i18n("Show advanced options"), this);
    root->addWidget(advToggle);

    m_advanced = new QWidget(this);
    auto *advForm = new QFormLayout(m_advanced);
    advForm->setContentsMargins(0, 0, 0, 0);
    m_permission = new QComboBox(m_advanced);
    m_permission->addItem(i18n("Apply edits automatically"), QStringLiteral("acceptEdits"));
    m_permission->addItem(i18n("Ask before each step"), QStringLiteral("default"));
    m_permission->addItem(i18n("Work freely"), QStringLiteral("auto"));
    m_permission->addItem(i18n("Expert — never ask"), QStringLiteral("bypassPermissions"));
    advForm->addRow(i18n("When to ask"), m_permission);
    m_effort = new QComboBox(m_advanced);
    m_effort->addItem(i18n("Default"), QString());
    m_effort->addItem(i18n("Low"), QStringLiteral("low"));
    m_effort->addItem(i18n("Medium"), QStringLiteral("medium"));
    m_effort->addItem(i18n("High"), QStringLiteral("high"));
    m_effort->addItem(i18n("Extra-high"), QStringLiteral("xhigh"));
    m_effort->addItem(i18n("Maximum"), QStringLiteral("max"));
    advForm->addRow(i18n("Thinking effort"), m_effort);
    m_advanced->setVisible(false);
    root->addWidget(m_advanced);
    connect(advToggle, &QCheckBox::toggled, m_advanced, &QWidget::setVisible);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    buttons->button(QDialogButtonBox::Ok)->setText(i18n("Create Agent"));
    buttons->button(QDialogButtonBox::Ok)->setIcon(QIcon::fromTheme(QStringLiteral("list-add")));
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    root->addWidget(buttons);

    m_task->setFocus();
    resize(460, 420);
}

NewAgentChoices NewAgentDialog::choices() const
{
    NewAgentChoices c;
    c.task = m_task->toPlainText().trimmed();
    c.modelId = m_model->currentData().toString();
    c.isolation = m_sandbox->isChecked() ? QStringLiteral("isolated")
                                         : QStringLiteral("workspace");
    c.permissionMode = m_permission->currentData().toString();
    c.effort = m_effort->currentData().toString();
    return c;
}
