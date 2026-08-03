// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "NewAgentDialog.h"
#include "ProviderConfig.h"
#include "ipc/CoreClient.h"
#include "shell/ChipPainter.h"
#include "shell/ElidingLabel.h"
#include "state/EngineAvailability.h"
#include "state/EnsembleCatalog.h"
#include "state/HarnessTraits.h"

#include <KColorScheme>
#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QCheckBox>
#include <QClipboard>
#include <QDoubleSpinBox>
#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QGuiApplication>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QPainter>
#include <QPalette>
#include <QPlainTextEdit>
#include <QPointer>
#include <QProcess>
#include <QPushButton>
#include <QSignalBlocker>
#include <QStandardItemModel>
#include <QTimer>
#include <QToolButton>
#include <QVBoxLayout>

namespace IsolationCopy {

namespace {
// How long a `git rev-parse` may take before we stop waiting for it. Nothing
// user-visible hangs on this — the caller is already showing the Unknown
// wording — it only bounds the stray process on a wedged mount.
constexpr int kProbeTimeoutMs = 5000;
} // namespace

void probeIsolationAsync(const QString &projectPath, QObject *context,
                         const std::function<void(Availability)> &done)
{
    if (!done) {
        return;
    }
    // No path: nothing to ask about. Answering from the CALLER's working
    // directory is the mistake worktree.EffectiveIsolation calls out — it would
    // report on whatever repo happens to be there, which is a confident answer
    // about the wrong project.
    if (projectPath.trimmed().isEmpty()) {
        done(Availability::Unknown);
        return;
    }
    if (!context) {
        return; // nothing to report to; see the header
    }
    // Unparented on purpose. As a child of the dialog it would be destroyed
    // with it, and ~QProcess kills and then BLOCKS until the process is gone —
    // which would move the very freeze this is removing from open to close. It
    // deletes itself when git answers instead.
    auto *git = new QProcess;
    git->setWorkingDirectory(projectPath);
    git->setProcessChannelMode(QProcess::MergedChannels);
    const QPointer<QObject> guard(context);
    // settle runs exactly once: finished() and errorOccurred() can both fire
    // for one run (a crash emits each), so the first one through disconnects
    // the other. Disconnecting and deleteLater()ing from inside the signal are
    // both safe; the callback is invoked last, after this object has stopped
    // being able to call back into a dialog that may not be there.
    const auto settle = [git, guard, done](Availability a) {
        git->disconnect();
        git->deleteLater();
        if (!guard) {
            return; // the dialog closed while git was still running
        }
        done(a);
    };
    QObject::connect(git, &QProcess::errorOccurred, git,
                     [settle](QProcess::ProcessError) {
                         // No git on PATH, or it never started: we cannot know,
                         // and "cannot know" must not become "no copy".
                         settle(Availability::Unknown);
                     });
    QObject::connect(git, &QProcess::finished, git,
                     [settle](int exitCode, QProcess::ExitStatus status) {
                         if (status != QProcess::NormalExit) {
                             settle(Availability::Unknown);
                             return;
                         }
                         // git answered. Exit 0 = a HEAD commit exists, so
                         // `git worktree add` has something to branch from.
                         // Anything else (fresh repo, not a repo) means
                         // Create() will run the agent in the workspace.
                         settle(exitCode == 0 ? Availability::Available
                                              : Availability::Unavailable);
                     });
    QTimer::singleShot(kProbeTimeoutMs, git, [git] {
        if (git->state() != QProcess::NotRunning) {
            git->kill(); // -> finished(CrashExit) -> Unknown
        }
    });
    git->start(QStringLiteral("git"), {QStringLiteral("rev-parse"),
                                       QStringLiteral("--verify"),
                                       QStringLiteral("HEAD")});
}

} // namespace IsolationCopy

namespace {

// Grey out one row of a combo. QComboBox has no per-entry enabled flag; its
// default model is a QStandardItemModel, so the item carries it — the same
// idiom AgentPanel::applyModelEffortSupport uses for efforts a model cannot run.
void setComboEntryEnabled(QComboBox *combo, int index, bool on,
                          const QString &whyNot = QString())
{
    auto *model = qobject_cast<QStandardItemModel *>(combo->model());
    QStandardItem *item = model ? model->item(index) : nullptr;
    if (!item) {
        return; // a custom model: annotated but selectable, which is still honest
    }
    item->setEnabled(on);
    if (!on && !whyNot.isEmpty()) {
        item->setToolTip(whyNot);
    }
}

// Move off a refused entry. A combo whose current row is disabled still SHOWS
// it and still reports its data, so leaving the selection there would hand the
// launch exactly the choice the row was disabled to prevent.
void selectFirstEnabled(QComboBox *combo)
{
    auto *model = qobject_cast<QStandardItemModel *>(combo->model());
    if (!model) {
        return;
    }
    const int cur = combo->currentIndex();
    if (cur >= 0 && model->item(cur) && model->item(cur)->isEnabled()) {
        return;
    }
    for (int i = 0; i < combo->count(); ++i) {
        if (model->item(i) && model->item(i)->isEnabled()) {
            combo->setCurrentIndex(i);
            return;
        }
    }
    // Nothing is startable at all. Leave the selection alone: the choice is
    // moot, and EngineAvailability's banner is the surface that says why.
}

} // namespace

// --- the preflight card (plan 26 phase 2) -----------------------------------

EngineHealth EngineHealth::fromJson(const QJsonObject &o)
{
    EngineHealth h;
    h.engineId = o.value(QStringLiteral("engineId")).toString();
    h.state = o.value(QStringLiteral("state")).toString();
    h.version = o.value(QStringLiteral("version")).toString();
    h.models = o.value(QStringLiteral("models")).toInt();
    const QJsonArray checks = o.value(QStringLiteral("checks")).toArray();
    for (const QJsonValue &v : checks) {
        const QJsonObject c = v.toObject();
        h.checks.append(EngineCheck{
            c.value(QStringLiteral("name")).toString(),
            c.value(QStringLiteral("state")).toString(),
            c.value(QStringLiteral("detail")).toString(),
            c.value(QStringLiteral("remedy")).toString(),
        });
    }
    return h;
}

namespace {

// The traffic light's palette, from KColorScheme's status roles — never a
// hardcoded green/amber/red, so it stays legible under every colour scheme.
void healthColors(const QString &state, QColor *fill, QColor *text)
{
    const KColorScheme scheme(QPalette::Active, KColorScheme::View);
    if (state == QLatin1String("ok")) {
        *fill = scheme.background(KColorScheme::PositiveBackground).color();
        *text = scheme.foreground(KColorScheme::PositiveText).color();
    } else if (state == QLatin1String("warn")) {
        *fill = scheme.background(KColorScheme::NeutralBackground).color();
        *text = scheme.foreground(KColorScheme::NeutralText).color();
    } else if (state == QLatin1String("bad")) {
        *fill = scheme.background(KColorScheme::NegativeBackground).color();
        *text = scheme.foreground(KColorScheme::NegativeText).color();
    } else { // unknown / pending: quiet, promises nothing
        *fill = scheme.background(KColorScheme::AlternateBackground).color();
        *text = scheme.foreground(KColorScheme::InactiveText).color();
    }
}

// The chip's one-word verdict. Human words, not the wire tokens.
QString healthChipText(const QString &state)
{
    if (state == QLatin1String("ok")) {
        return i18nc("engine health chip", "Ready");
    }
    if (state == QLatin1String("warn")) {
        return i18nc("engine health chip", "Attention");
    }
    if (state == QLatin1String("bad")) {
        return i18nc("engine health chip", "Not ready");
    }
    return i18nc("engine health chip", "Unchecked");
}

} // namespace

HealthChip::HealthChip(QWidget *parent)
    : QWidget(parent)
{
    setSizePolicy(QSizePolicy::Fixed, QSizePolicy::Fixed);
}

void HealthChip::setVerdict(const QString &state, const QString &text)
{
    if (state == m_state && text == m_text) {
        return; // same verdict, no repaint (the card's Reactive already
                // guards this path, but the chip stays safe on its own too)
    }
    m_state = state;
    m_text = text;
    updateGeometry();
    update();
}

QSize HealthChip::sizeHint() const
{
    return {ChipPainter::chipWidth(font(), m_text), ChipPainter::chipHeight(font())};
}

void HealthChip::paintEvent(QPaintEvent *)
{
    QColor fill, text;
    healthColors(m_state, &fill, &text);
    QPainter p(this);
    p.setRenderHint(QPainter::Antialiasing);
    ChipPainter::drawChip(&p, QRect(QPoint(0, 0), sizeHint()), m_text, font(),
                          fill, text);
}

PreflightCard::PreflightCard(QWidget *parent)
    : QWidget(parent)
{
    auto *root = new QVBoxLayout(this);
    root->setContentsMargins(0, 0, 0, 0);
    root->setSpacing(4);

    auto *header = new QHBoxLayout;
    header->setContentsMargins(0, 0, 0, 0);
    m_toggle = new QToolButton(this);
    m_toggle->setAutoRaise(true);
    m_toggle->setCheckable(true);
    m_toggle->setArrowType(Qt::RightArrow);
    m_toggle->setToolTip(i18n("Show the engine's health checks"));
    header->addWidget(m_toggle);
    m_chip = new HealthChip(this);
    header->addWidget(m_chip);
    header->addSpacing(ChipPainter::kChipGap);
    m_summary = new ElidingLabel(this);
    DisclosureStyle::apply(m_summary);
    header->addWidget(m_summary, 1);
    root->addLayout(header);

    m_body = new QWidget(this);
    m_bodyLayout = new QVBoxLayout(m_body);
    m_bodyLayout->setContentsMargins(m_toggle->sizeHint().width(), 0, 0, 0);
    m_bodyLayout->setSpacing(2);
    m_body->setVisible(false);
    root->addWidget(m_body);

    connect(m_toggle, &QToolButton::toggled, this, [this](bool on) {
        m_toggle->setArrowType(on ? Qt::DownArrow : Qt::RightArrow);
        m_body->setVisible(on && m_bodyLayout->count() > 0);
    });

    // The verdict repaints ONLY on a real change: Reactive<T>'s full-field
    // equality is the whole flicker guard, and PreflightCardTest pins it.
    m_health.subscribe(this, [this](const EngineHealth &h) { rebuild(h); });

    setPending(QString());
}

void PreflightCard::setPending(const QString &engineId)
{
    const EngineHealth &held = m_health.get();
    if (!held.state.isEmpty() && held.engineId == engineId) {
        return; // the verdict on show already answers for this engine
    }
    m_chip->setVerdict(QStringLiteral("pending"),
                       i18nc("engine health chip", "Checking…"));
    m_summary->setText(QString());
    m_body->setVisible(false);
    m_toggle->setEnabled(false);
}

void PreflightCard::setHealth(const EngineHealth &health)
{
    m_health.set(health); // equal verdicts stop right here — no rebuild
}

void PreflightCard::rebuild(const EngineHealth &health)
{
    ++m_rebuilds;
    m_chip->setVerdict(health.state, healthChipText(health.state));

    QStringList summaryBits;
    if (!health.version.isEmpty()) {
        summaryBits << health.version;
    }
    if (health.models > 0) {
        summaryBits << i18np("%1 model", "%1 models", health.models);
    }
    m_summary->setText(summaryBits.join(i18nc("summary separator", " · ")));

    // One line per non-OK check; an all-green engine keeps the card to its
    // single header line.
    while (QLayoutItem *item = m_bodyLayout->takeAt(0)) {
        delete item->widget();
        delete item;
    }
    for (const EngineCheck &check : health.checks) {
        if (check.state == QLatin1String("ok")) {
            continue;
        }
        auto *line = new ElidingLabel(
            i18nc("health check line: name, state, detail", "%1: %2 — %3",
                  check.name, check.state, check.detail),
            m_body);
        line->setToolTip(check.detail);
        DisclosureStyle::apply(line);
        m_bodyLayout->addWidget(line);
        if (check.remedy.isEmpty()) {
            continue;
        }
        // The engine's own remedy, copyable. Running it in a terminal is
        // deferred (the terminal lives on MainWindow); Copy loses nothing.
        auto *remedyRow = new QWidget(m_body);
        auto *remedyLayout = new QHBoxLayout(remedyRow);
        remedyLayout->setContentsMargins(0, 0, 0, 0);
        auto *command = new QLabel(remedyRow);
        QFont mono = command->font();
        mono.setFamilies({QStringLiteral("monospace")});
        command->setFont(mono);
        command->setText(check.remedy);
        command->setTextInteractionFlags(Qt::TextSelectableByMouse);
        remedyLayout->addWidget(command);
        auto *copy = new QToolButton(remedyRow);
        copy->setIcon(QIcon::fromTheme(QStringLiteral("edit-copy")));
        copy->setText(i18nc("@action:button copy the remedy command", "Copy"));
        copy->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
        copy->setAutoRaise(true);
        const QString remedy = check.remedy;
        connect(copy, &QToolButton::clicked, remedyRow, [remedy] {
            QGuiApplication::clipboard()->setText(remedy);
        });
        remedyLayout->addWidget(copy);
        remedyLayout->addStretch(1);
        m_bodyLayout->addWidget(remedyRow);
    }

    const bool hasLines = m_bodyLayout->count() > 0;
    m_toggle->setEnabled(hasLines);
    if (hasLines && !m_autoExpanded) {
        // Open once, on the first verdict worth reading — and never again,
        // so a user who collapsed the card stays collapsed.
        m_autoExpanded = true;
        m_toggle->setChecked(true);
    }
    m_body->setVisible(m_toggle->isChecked() && hasLines);
}

NewAgentDialog::NewAgentDialog(const QString &projectName, CoreClient *core,
                               QWidget *parent, const QString &projectPath)
    : QDialog(parent)
    , m_core(core)
    , m_projectPath(projectPath)
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
    // Every entry is LISTED — hiding an engine would make the product look like
    // it supports one — but an entry that cannot start today says so and cannot
    // be chosen. This is the guided front door: it creates the agent the moment
    // it is accepted, so offering a route that ends in "executable file not
    // found in $PATH" (audit F37) or "no API credential supplied" (audit F46)
    // spends the user's task description to tell them something we already
    // knew. Both annotations come from the shared helpers, so this picker, the
    // panel's combo and the roster's quick menu cannot drift apart.
    for (const HarnessTraits &t : HarnessRegistry::self()->all()) {
        const bool present = EngineAvailability::isPresent(t.id);
        const QString engineLabel =
            EngineAvailability::pickerLabel(t.id, t.displayName);
        m_engine->addItem(engineLabel, t.id);
        setComboEntryEnabled(
            m_engine, m_engine->count() - 1, present,
            i18n("Agent Kate drives an agent command-line program, and this "
                 "engine's is not installed on this machine."));
        if (!t.providerRouting) {
            continue;
        }
        const QList<ProviderProfile> profiles = ProviderStore::load();
        for (const ProviderProfile &p : profiles) {
            if (!p.routed()) {
                continue; // the direct entry IS the base harness row
            }
            m_engine->addItem(i18nc("engine entry: harness via provider",
                                    "%1 via %2", engineLabel,
                                    ProviderStore::pickerLabel(p)),
                              t.id + QLatin1Char('|') + p.id);
            setComboEntryEnabled(
                m_engine, m_engine->count() - 1,
                present && ProviderStore::keyResolvable(p),
                present ? i18n("No API key is stored for %1 — add one under "
                               "Options ▸ Configure API Providers….", p.name)
                        : i18n("Agent Kate drives an agent command-line "
                               "program, and this engine's is not installed on "
                               "this machine."));
        }
    }
    // Never leave the picker resting on an entry it has just refused.
    selectFirstEnabled(m_engine);
    form->addRow(i18n("Which agent?"), m_engine);
    // The preflight card (plan 26): the selected engine's health, refreshed
    // with the combo. It WARNS only — Create is never blocked on a verdict.
    m_preflight = new PreflightCard(this);
    form->addRow(m_preflight);
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
    connect(m_engine, &QComboBox::currentIndexChanged, this,
            [this, rebuildBackendChoices, probeEngine] {
        probeEngine();
        rebuildBackendChoices();
        refreshPreflight();
    });
    connect(HarnessRegistry::self(), &HarnessRegistry::changed, this,
            rebuildBackendChoices);

    // HONEST LABELLING (audit F30/F49). Every word of this control, and the
    // reasoning behind splitting the promise off the label and into a note that
    // depends on a probe, lives in IsolationCopy (NewAgentDialog.h).
    m_sandbox = new QCheckBox(IsolationCopy::checkboxLabel(), this);
    m_sandbox->setChecked(true);
    m_sandbox->setToolTip(IsolationCopy::checkboxTooltip());
    root->addWidget(m_sandbox);
    m_isolationNote = new QLabel(this);
    m_isolationNote->setWordWrap(true);
    m_isolationNote->setTextFormat(Qt::PlainText);
    DisclosureStyle::apply(m_isolationNote);
    root->addWidget(m_isolationNote);
    // Resolve the degradation BEFORE the dialog can be accepted rather than
    // promising a private copy and apologising once the agent has started.
    //
    // The probe does NOT block the constructor (audit F12's class): it starts
    // in Availability::Unknown, whose wording covers both outcomes and promises
    // neither, and repaints if git answers while the dialog is still open. A
    // user who accepts before then sends "auto" — the same request the
    // conditional sentence describes.
    connect(m_sandbox, &QCheckBox::toggled, this,
            [this] { updateIsolationState(); });
    updateIsolationState();
    IsolationCopy::probeIsolationAsync(
        projectPath, this, [this](IsolationCopy::Availability a) {
            if (a == m_isolationAvailability) {
                return;
            }
            m_isolationAvailability = a;
            updateIsolationState();
        });

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
    refreshPreflight();
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
    // The isolation checkbox has two independent owners — the ensemble picker
    // and the project probe — so it is enabled in one place that consults both.
    updateIsolationState();
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

// updateIsolationState keeps the checkbox and the sentence under it telling the
// same story (audit F30, second pass). The failure it exists to prevent: a
// ticked box promising a private copy on a project where "auto" will silently
// hand the agent the user's own files instead.
void NewAgentDialog::updateIsolationState()
{
    const bool crew = !m_ensemble->currentData().toString().isEmpty();
    const bool possible =
        m_isolationAvailability != IsolationCopy::Availability::Unavailable;
    if (!possible && m_sandbox->isChecked()) {
        // Untick rather than leave a ticked box above a note that says it will
        // not happen. choices() then reports "workspace" — precisely what the
        // launch would have degraded to anyway, so the fresh never-committed
        // project still starts (audit F49) with the promise withdrawn instead
        // of broken. Blocked so this does not recurse through the toggle.
        QSignalBlocker block(m_sandbox);
        m_sandbox->setChecked(false);
    }
    m_sandbox->setEnabled(!crew && possible);
    m_isolationNote->setText(
        IsolationCopy::isolationNote(m_isolationAvailability,
                                     m_sandbox->isChecked()));
}

// refreshPreflight asks the core for the selected engine's health and
// publishes the verdict into the card. Fire-and-forget: the core caches the
// answer for 30 s, the card's Reactive guard swallows identical repeats, and
// nothing here can gate the Create button (warn, never block).
void NewAgentDialog::refreshPreflight()
{
    if (!m_preflight) {
        return;
    }
    const QString engineId =
        m_engine->currentData().toString().section(QLatin1Char('|'), 0, 0);
    m_preflight->setPending(engineId);
    if (engineId.isEmpty() || !m_core || !m_core->isConnected()) {
        return; // the card stays on its quiet "checking…"/held verdict
    }
    QJsonObject params{{QStringLiteral("engineId"), engineId}};
    if (!m_projectPath.isEmpty()) {
        // claude's doctor reads the project directory's settings.
        params.insert(QStringLiteral("project"), m_projectPath);
    }
    m_core->call(
        QStringLiteral("engine.health"), params,
        // The context argument (below) QPointer-guards this continuation:
        // a reply landing after the dialog closed is dropped, never a UAF.
        [this, engineId](const QJsonObject &result, const QJsonObject &error) {
            const QString current =
                m_engine->currentData().toString().section(QLatin1Char('|'), 0, 0);
            if (current != engineId) {
                return; // the user moved on; a stale verdict must not land
            }
            if (!error.isEmpty()) {
                // Best-effort to the end: an unreachable probe is an UNKNOWN
                // verdict on the card, never a blank or a scary red.
                EngineHealth h;
                h.engineId = engineId;
                h.state = QStringLiteral("unknown");
                h.checks.append(EngineCheck{
                    QStringLiteral("health"), QStringLiteral("unknown"),
                    error.value(QStringLiteral("message")).toString(), QString()});
                m_preflight->setHealth(h);
                return;
            }
            const QJsonArray engines =
                result.value(QStringLiteral("engines")).toArray();
            for (const QJsonValue &v : engines) {
                const EngineHealth h = EngineHealth::fromJson(v.toObject());
                if (h.engineId == engineId) {
                    m_preflight->setHealth(h);
                    return;
                }
            }
        },
        this);
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
    // UX (audit F49): "auto", never "isolated". worktree.Create refuses
    // ModeIsolated on a repo with no commit to branch from ("isolation needs at
    // least one commit"), so the RECOMMENDED default used to fail a brand-new
    // project with raw git-speak in the conversation. ModeAuto isolates wherever
    // isolation is possible and falls back to the workspace where it is not.
    //
    // The fallback is no longer something the user only learns about afterwards
    // (audit F30, second pass): where the probe knows a copy is impossible,
    // updateIsolationState() has already unticked the box and said why, so this
    // returns "workspace" — the same launch that succeeds, without the dialog
    // having promised the opposite. Where the probe could not answer, "auto" is
    // still the request and the note under the checkbox is conditional to match;
    // the panel's "Working directly in your files" then confirms which it was.
    c.isolation = m_sandbox->isChecked() ? QStringLiteral("auto")
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
