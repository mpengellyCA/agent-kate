// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "NewAgentDialog.h"
#include "ProviderConfig.h"
#include "state/HarnessTraits.h"

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
        };
        restore(m_model, prevModel);
        restore(m_permission, prevPerm);
        restore(m_effort, prevEffort);
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
    // backend selection, and probe that engine's option lists if discovered.
    probeEngine();
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
    const QString engine = m_engine->currentData().toString();
    c.backend = engine.section(QLatin1Char('|'), 0, 0);
    c.providerId = engine.section(QLatin1Char('|'), 1);
    c.modelId = m_model->currentData().toString();
    c.isolation = m_sandbox->isChecked() ? QStringLiteral("isolated")
                                         : QStringLiteral("workspace");
    c.permissionMode = m_permission->currentData().toString();
    c.effort = m_effort->currentData().toString();
    return c;
}
