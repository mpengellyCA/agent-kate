// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "NewAgentDialog.h"
#include "ProviderConfig.h"
#include "state/EnsembleCatalog.h"
#include "state/HarnessTraits.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QCheckBox>
#include <QDoubleSpinBox>
#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QLabel>
#include <QLineEdit>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QSignalBlocker>
#include <QVBoxLayout>

NewAgentDialog::NewAgentDialog(const QString &projectName, CoreClient *core,
                               QWidget *parent)
    : QDialog(parent)
    , m_core(core)
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
    // Ensembles first: picking one replaces every choice below it (the recipe
    // owns the controller's engine, model and options), so it belongs at the
    // top where it reads as "one agent, or a crew".
    m_ensemble = new QComboBox(this);
    m_ensemble->addItem(i18n("A single agent"), QString());
    for (const Ensemble &e : EnsembleCatalog::self()->list()) {
        m_ensemble->addItem(i18n("Ensemble: %1", e.name), e.name);
        if (!e.description.isEmpty()) {
            m_ensemble->setItemData(m_ensemble->count() - 1, e.description, Qt::ToolTipRole);
        }
    }
    form->addRow(i18n("Work as"), m_ensemble);
    // With no ensembles to choose from (an older core, or every built-in
    // deleted) the row would be a picker with one option — hide it rather than
    // ask a question that has no alternatives.
    if (m_ensemble->count() < 2) {
        form->setRowVisible(m_ensemble, false);
    }
    m_engine = new QComboBox(this);
    m_engine->setToolTip(i18n(
        "Which agent engine runs this task: the agent program, optionally "
        "routed through a third-party API provider."));
    for (const HarnessTraits &t : HarnessRegistry::self()->all()) {
        m_engine->addItem(t.displayName, t.id);
        if (!t.providerRouting) {
            continue;
        }
        const QList<ProviderProfile> profiles = ProviderStore::load();
        for (const ProviderProfile &p : profiles) {
            if (p.routed()) {
                m_engine->addItem(i18nc("engine entry: harness via provider",
                                        "%1 via %2", t.displayName, p.name),
                                  t.id + QLatin1Char('|') + p.id);
            }
        }
    }
    form->addRow(i18n("Which agent?"), m_engine);
    m_model = new QComboBox(this);
    m_model->setToolTip(i18n("Which model powers this agent. Smarter is more capable; faster is cheaper."));
    form->addRow(i18n("How clever?"), m_model);
    root->addLayout(form);

    m_ensembleHint = new QLabel(this);
    m_ensembleHint->setWordWrap(true);
    m_ensembleHint->setVisible(false);
    root->addWidget(m_ensembleHint);

    // The model, when-to-ask and effort lists all follow the engine's harness:
    // its static vocabularies where it has them, else the lists discovered
    // from its last session's handshake — until one has run, only the
    // defaults are offered (the panel's Setup menu additionally takes a
    // free-text model id for discovered-model harnesses).
    auto rebuildBackendChoices = [this] {
        const HarnessTraits t = HarnessRegistry::self()->traits(
            m_engine->currentData().toString().section(QLatin1Char('|'), 0, 0));
        QSignalBlocker blockModel(m_model);
        QSignalBlocker blockPerm(m_permission);
        QSignalBlocker blockEffort(m_effort);
        // Preserve the user's picks across the rebuild — a late capability fetch
        // or option probe fires HarnessRegistry::changed, and clobbering the
        // selection to the first entry after they have chosen is the bug this
        // guards against (AgentPanel::rebuildEngineCombo uses the same pattern).
        const QString prevModel = m_model->currentData().toString();
        const QString prevPerm = m_permission->currentData().toString();
        const QString prevEffort = m_effort->currentData().toString();
        m_model->clear();
        m_permission->clear();
        m_effort->clear();
        const auto addDiscovered = [](QComboBox *combo, const QString &key) {
            const QStringList entries = KSharedConfig::openConfig()
                                            ->group(QStringLiteral("Agent"))
                                            .readEntry(key, QStringList());
            for (const QString &entry : entries) {
                const QString value = entry.section(QLatin1Char('|'), 0, 0);
                const QString name = entry.section(QLatin1Char('|'), 1);
                if (!value.isEmpty()) {
                    combo->addItem(name.isEmpty() ? value : name, value);
                }
            }
        };
        m_model->addItem(i18n("Use my default"), QString());
        // Live model catalogue for this engine/provider: a short recommended
        // group, then the full list. Both come from the cache the startup probe
        // fills (no hardcoded model names).
        {
            const QString data = m_engine->currentData().toString();
            const auto choices = HarnessRegistry::self()->modelChoices(
                data.section(QLatin1Char('|'), 0, 0), data.section(QLatin1Char('|'), 1, 1));
            const auto addEntries = [this](const QStringList &entries) {
                for (const QString &entry : entries) {
                    const QString value = entry.section(QLatin1Char('|'), 0, 0);
                    const QString name = entry.section(QLatin1Char('|'), 1);
                    if (!value.isEmpty() && m_model->findData(value) < 0) {
                        m_model->addItem(name.isEmpty() ? value : name, value);
                    }
                }
            };
            addEntries(choices.recommended);
            if (!choices.recommended.isEmpty() && !choices.all.isEmpty()) {
                m_model->insertSeparator(m_model->count());
            }
            addEntries(choices.all);
        }
        if (t.permissionModes.isEmpty()) {
            m_permission->addItem(i18n("CLI default"), QString());
            addDiscovered(m_permission, t.optionKey(QStringLiteral("mode")));
        } else {
            for (const QString &mode : t.permissionModes) {
                m_permission->addItem(HarnessRegistry::modeLabel(mode), mode);
            }
        }
        m_effort->addItem(i18n("Default"), QString());
        if (t.efforts.isEmpty()) {
            addDiscovered(m_effort, t.optionKey(QStringLiteral("thinking")));
        } else {
            for (const QString &effort : t.efforts) {
                m_effort->addItem(HarnessRegistry::effortLabel(effort), effort);
            }
        }
        const auto restore = [](QComboBox *combo, const QString &data) {
            const int idx = combo->findData(data);
            if (idx >= 0) {
                combo->setCurrentIndex(idx);
            }
            return idx >= 0;
        };
        restore(m_model, prevModel);
        // The mode list is the engine's own vocabulary in the CLI's order, so
        // index 0 is arbitrary: on the first build (nothing previously picked)
        // or after switching to an engine without the previous mode, land on
        // the harness's named default instead of whatever it lists first.
        if (!restore(m_permission, prevPerm)) {
            restore(m_permission, t.defaultPermissionMode());
        }
        restore(m_effort, prevEffort);
        // Only offer the sweep options this engine can actually apply. The same
        // flags are remembered for choices(), which must decide "was this row
        // offered?" from the capability and never from widget visibility —
        // collapsing Advanced hides every row without withdrawing any option.
        m_sweepSupport.fallbackModels = t.fallbackModels;
        m_sweepSupport.disallowedTools = t.disallowedTools;
        m_sweepSupport.addDirs = t.addDirs;
        m_sweepSupport.strictMcpConfig = t.strictMcpConfig;
        m_sweepSupport.costBudget = t.costBudget;
        if (m_advancedForm) {
            m_advancedForm->setRowVisible(m_fallbackModels, t.fallbackModels);
            m_advancedForm->setRowVisible(m_disallowedTools, t.disallowedTools);
            m_advancedForm->setRowVisible(m_addDirs, t.addDirs);
            m_advancedForm->setRowVisible(m_strictMcp, t.strictMcpConfig);
            m_advancedForm->setRowVisible(m_budget, t.costBudget);
        }
    };
    // Selecting a discovered-model engine (e.g. Kimi) with no cached option
    // lists probes the CLI once so the lists fill before the agent starts; the
    // HarnessRegistry::changed handler below repopulates the combos when the
    // result lands. A no-op for tier engines or an already-cached vocabulary.
    auto probeEngine = [this] {
        HarnessRegistry::self()->ensureDiscovered(
            m_core, m_engine->currentData().toString().section(QLatin1Char('|'), 0, 0));
    };
    connect(m_engine, &QComboBox::currentIndexChanged, this, [rebuildBackendChoices,
                                                              probeEngine] {
        probeEngine();
        rebuildBackendChoices();
    });
    connect(HarnessRegistry::self(), &HarnessRegistry::changed, this,
            rebuildBackendChoices);

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
    m_advancedForm = new QFormLayout(m_advanced);
    QFormLayout *advForm = m_advancedForm;
    advForm->setContentsMargins(0, 0, 0, 0);
    m_permission = new QComboBox(m_advanced);
    advForm->addRow(i18n("When to ask"), m_permission);
    m_effort = new QComboBox(m_advanced);
    advForm->addRow(i18n("Thinking effort"), m_effort);
    // The launch-option sweep (plan 16 P6). Each row is hidden for an engine
    // that cannot express it: offering a field the launch would silently drop
    // is exactly the downgrade the applied-truth rule exists to prevent.
    m_fallbackModels = new QLineEdit(m_advanced);
    m_fallbackModels->setPlaceholderText(i18n("e.g. sonnet, haiku"));
    m_fallbackModels->setToolTip(
        i18n("Models to fall back to, in order, if the chosen one is overloaded "
             "or unavailable."));
    advForm->addRow(i18n("Fall back to"), m_fallbackModels);
    m_disallowedTools = new QLineEdit(m_advanced);
    m_disallowedTools->setPlaceholderText(i18n("e.g. WebFetch, Bash(git push:*)"));
    m_disallowedTools->setToolTip(
        i18n("Tools this agent may never use. A denial always beats an allowance."));
    advForm->addRow(i18n("Never use"), m_disallowedTools);
    m_addDirs = new QLineEdit(m_advanced);
    m_addDirs->setPlaceholderText(i18n("e.g. /home/you/reference-docs"));
    m_addDirs->setToolTip(
        i18n("Extra directories this agent's tools may read, beyond its own "
             "working copy. Comma-separated."));
    advForm->addRow(i18n("Also allow access to"), m_addDirs);
    m_strictMcp = new QCheckBox(
        i18n("Isolate from global MCP servers"), m_advanced);
    m_strictMcp->setToolTip(
        i18n("Run this agent with only the tool servers Agent Kate wires in, "
             "ignoring the MCP servers configured globally for the CLI. Off by "
             "default — the global servers are usually the point."));
    advForm->addRow(QString(), m_strictMcp);
    m_budget = new QDoubleSpinBox(m_advanced);
    m_budget->setDecimals(2);
    m_budget->setRange(0.0, 10000.0);
    m_budget->setSingleStep(1.0);
    m_budget->setPrefix(QStringLiteral("$"));
    // 0 is "no ceiling", spelled out rather than shown as a $0.00 budget that
    // would read as "spend nothing".
    m_budget->setSpecialValueText(i18n("No limit"));
    m_budget->setToolTip(
        i18n("A hard spend ceiling for this agent's whole session, enforced by "
             "the engine itself: once it trips, the turn ends with an error "
             "instead of billing further."));
    advForm->addRow(i18n("Cost budget (USD)"), m_budget);
    m_advanced->setVisible(false);
    root->addWidget(m_advanced);
    connect(advToggle, &QCheckBox::toggled, m_advanced, &QWidget::setVisible);
    // All three per-backend combos exist now — populate them for the default
    // backend selection, and probe that engine's option lists if discovered.
    probeEngine();
    rebuildBackendChoices();
    connect(m_ensemble, &QComboBox::currentIndexChanged, this,
            &NewAgentDialog::applyEnsembleMode);
    applyEnsembleMode();

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    buttons->button(QDialogButtonBox::Ok)->setText(i18n("Create Agent"));
    buttons->button(QDialogButtonBox::Ok)->setIcon(QIcon::fromTheme(QStringLiteral("list-add")));
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    root->addWidget(buttons);

    m_task->setFocus();
    resize(460, 420);
}

// applyEnsembleMode greys out the single-agent pickers when an ensemble is
// selected. The ensemble defines its controller (and the roles it may launch),
// so leaving the engine/model/advanced rows live would show choices the launch
// silently ignores — the ensemble's recipe wins either way, so say so.
void NewAgentDialog::applyEnsembleMode()
{
    const QString name = m_ensemble->currentData().toString();
    const bool crew = !name.isEmpty();
    m_engine->setEnabled(!crew);
    m_model->setEnabled(!crew);
    m_sandbox->setEnabled(!crew);
    m_advanced->setEnabled(!crew);
    m_ensembleHint->setVisible(crew);
    if (!crew) {
        return;
    }
    const Ensemble e = EnsembleCatalog::self()->get(name);
    QStringList roles;
    for (const EnsembleMember &w : e.workers) {
        roles << (w.model.isEmpty() ? w.role : i18nc("worker role and its model",
                                                     "%1 (%2)", w.role, w.model));
    }
    const QString controller = e.controller.model.isEmpty()
        ? HarnessRegistry::self()->traits(e.controller.backend).displayName
        : e.controller.model;
    m_ensembleHint->setText(
        roles.isEmpty()
            ? i18n("Starts one controller agent on %1. It defines no worker roles, so it "
                   "chooses any helpers itself.", controller)
            : i18n("Starts one controller agent on %1, which may launch these workers as "
                   "the job needs them: %2.", controller, roles.join(i18nc(
                       "list separator", ", "))));
}

NewAgentChoices NewAgentDialog::choices() const
{
    NewAgentChoices c;
    c.task = m_task->toPlainText().trimmed();
    c.ensemble = m_ensemble->currentData().toString();
    const QString engine = m_engine->currentData().toString();
    c.backend = engine.section(QLatin1Char('|'), 0, 0);
    c.providerId = engine.section(QLatin1Char('|'), 1);
    c.modelId = m_model->currentData().toString();
    c.isolation = m_sandbox->isChecked() ? QStringLiteral("isolated")
                                         : QStringLiteral("workspace");
    c.permissionMode = m_permission->currentData().toString();
    c.effort = m_effort->currentData().toString();
    // An unsupported row is an option this engine cannot express — read nothing
    // from it, so a value typed before switching engines cannot leak into the
    // launch. The test is the engine's capability, NOT the widget's visibility:
    // the Advanced section is collapsible, and a collapsed section makes every
    // row invisible while withdrawing nothing the user chose.
    const auto listFrom = [](const QLineEdit *edit, bool supported) {
        QStringList out;
        if (!edit || !supported) {
            return out;
        }
        const QStringList parts = edit->text().split(QLatin1Char(','), Qt::SkipEmptyParts);
        for (const QString &p : parts) {
            const QString trimmed = p.trimmed();
            if (!trimmed.isEmpty()) {
                out << trimmed;
            }
        }
        return out;
    };
    c.fallbackModels = listFrom(m_fallbackModels, m_sweepSupport.fallbackModels);
    c.disallowedTools = listFrom(m_disallowedTools, m_sweepSupport.disallowedTools);
    c.addDirs = listFrom(m_addDirs, m_sweepSupport.addDirs);
    // Same capability rule as the list fields: a row this engine cannot express
    // was never really offered, so it must not be reported as a request.
    c.strictMcpConfig = m_strictMcp && m_sweepSupport.strictMcpConfig
        && m_strictMcp->isChecked();
    c.maxBudgetUsd =
        (m_budget && m_sweepSupport.costBudget) ? m_budget->value() : 0.0;
    return c;
}
