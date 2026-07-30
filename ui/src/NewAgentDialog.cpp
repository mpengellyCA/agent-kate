// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "NewAgentDialog.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QCheckBox>
#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QLabel>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QSignalBlocker>
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
    m_backend = new QComboBox(this);
    m_backend->addItem(i18n("Claude Code"), QStringLiteral("claude"));
    m_backend->addItem(i18n("Kimi Code"), QStringLiteral("kimi"));
    m_backend->setToolTip(i18n("Which agent program runs this task. Kimi Code needs the kimi CLI installed."));
    form->addRow(i18n("Which agent?"), m_backend);
    m_model = new QComboBox(this);
    m_model->setToolTip(i18n("Which model powers this agent. Smarter is more capable; faster is cheaper."));
    form->addRow(i18n("How clever?"), m_model);
    root->addLayout(form);

    // The model, when-to-ask and effort lists all follow the backend: Claude's
    // fixed vocabularies for Claude Code; for Kimi Code the CLI's own lists
    // (model / mode / thinking) as discovered from the last kimi session's
    // handshake — until one has run, only the defaults are offered (the
    // panel's Setup menu additionally takes a free-text model id).
    auto kimiOptions = [](const char *key) {
        return KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .readEntry(key, QStringList());
    };
    auto rebuildBackendChoices = [this, kimiOptions] {
        const bool kimi =
            m_backend->currentData().toString() == QLatin1String("kimi");
        QSignalBlocker blockModel(m_model);
        QSignalBlocker blockPerm(m_permission);
        QSignalBlocker blockEffort(m_effort);
        m_model->clear();
        m_permission->clear();
        m_effort->clear();
        m_model->addItem(i18n("Use my default"), QString());
        if (!kimi) {
            m_model->addItem(i18n("Smartest (Opus)"), QStringLiteral("opus"));
            m_model->addItem(i18n("Balanced (Sonnet)"), QStringLiteral("sonnet"));
            m_model->addItem(i18n("Fastest (Haiku)"), QStringLiteral("haiku"));
            m_permission->addItem(i18n("Apply edits automatically"),
                                  QStringLiteral("acceptEdits"));
            m_permission->addItem(i18n("Ask before each step"), QStringLiteral("default"));
            m_permission->addItem(i18n("Plan first — read-only until approved"),
                                  QStringLiteral("plan"));
            m_permission->addItem(i18n("Work freely"), QStringLiteral("auto"));
            m_permission->addItem(i18n("Expert — never ask"),
                                  QStringLiteral("bypassPermissions"));
            m_effort->addItem(i18n("Default"), QString());
            m_effort->addItem(i18n("Low"), QStringLiteral("low"));
            m_effort->addItem(i18n("Medium"), QStringLiteral("medium"));
            m_effort->addItem(i18n("High"), QStringLiteral("high"));
            m_effort->addItem(i18n("Extra-high"), QStringLiteral("xhigh"));
            m_effort->addItem(i18n("Maximum"), QStringLiteral("max"));
            return;
        }
        const auto addFrom = [](QComboBox *combo, const QStringList &entries) {
            for (const QString &entry : entries) {
                const QString value = entry.section(QLatin1Char('|'), 0, 0);
                const QString name = entry.section(QLatin1Char('|'), 1);
                if (!value.isEmpty()) {
                    combo->addItem(name.isEmpty() ? value : name, value);
                }
            }
        };
        addFrom(m_model, kimiOptions("kimiOpt-model"));
        m_permission->addItem(i18n("CLI default"), QString());
        addFrom(m_permission, kimiOptions("kimiOpt-mode"));
        m_effort->addItem(i18n("CLI default"), QString());
        addFrom(m_effort, kimiOptions("kimiOpt-thinking"));
    };
    connect(m_backend, &QComboBox::currentIndexChanged, this, rebuildBackendChoices);

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
    advForm->addRow(i18n("When to ask"), m_permission);
    m_effort = new QComboBox(m_advanced);
    advForm->addRow(i18n("Thinking effort"), m_effort);
    m_advanced->setVisible(false);
    root->addWidget(m_advanced);
    connect(advToggle, &QCheckBox::toggled, m_advanced, &QWidget::setVisible);
    // All three per-backend combos exist now — populate them for the default
    // backend selection.
    rebuildBackendChoices();

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
    c.backend = m_backend->currentData().toString();
    c.modelId = m_model->currentData().toString();
    c.isolation = m_sandbox->isChecked() ? QStringLiteral("isolated")
                                         : QStringLiteral("workspace");
    c.permissionMode = m_permission->currentData().toString();
    c.effort = m_effort->currentData().toString();
    return c;
}
