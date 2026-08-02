// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "EnsembleDialog.h"
#include "ProviderConfig.h"
#include "ipc/CoreClient.h"
#include "state/HarnessTraits.h"

#include <KLocalizedString>
#include <KMessageBox>

#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QScrollArea>
#include <QSignalBlocker>
#include <QSplitter>
#include <QToolButton>
#include <QVBoxLayout>

namespace {
// Isolation is the same three-value vocabulary agent.start takes; the labels
// are the ones the New Agent dialog already uses in plain language.
void fillIsolation(QComboBox *combo)
{
    combo->addItem(i18n("Private copy (recommended)"), QStringLiteral("auto"));
    combo->addItem(i18n("Always a private copy"), QStringLiteral("isolated"));
    combo->addItem(i18n("Directly in the workspace"), QStringLiteral("workspace"));
}

void selectData(QComboBox *combo, const QString &data)
{
    const int idx = combo->findData(data);
    combo->setCurrentIndex(idx >= 0 ? idx : 0);
}

} // namespace

// The current INDEX is not enough: an id we have never discovered is shown as
// edit text while the index still points at the first row ("Engine default"),
// so reading the data would silently erase a model the ensemble was written
// with.
QString EnsembleDialog::modelIdFor(const QComboBox *combo)
{
    const int idx = combo->currentIndex();
    if (idx >= 0 && combo->itemText(idx) == combo->currentText()) {
        return combo->itemData(idx).toString(); // a catalogue row (empty = default)
    }
    return combo->currentText().trimmed(); // typed, or an id from another machine
}

EnsembleDialog::EnsembleDialog(CoreClient *core, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
{
    setWindowTitle(i18nc("@title:window", "Ensembles"));

    auto *root = new QVBoxLayout(this);
    auto *intro = new QLabel(
        i18n("An ensemble starts <b>one</b> controller agent and tells it which workers it "
             "may launch. The controller creates them itself, only when the job needs them."),
        this);
    intro->setWordWrap(true);
    intro->setTextFormat(Qt::RichText);
    root->addWidget(intro);

    auto *splitter = new QSplitter(Qt::Horizontal, this);
    root->addWidget(splitter, 1);

    // Left: the catalogue.
    auto *leftBox = new QWidget(splitter);
    auto *left = new QVBoxLayout(leftBox);
    left->setContentsMargins(0, 0, 0, 0);
    m_list = new QListWidget(leftBox);
    left->addWidget(m_list, 1);
    auto *newButton = new QPushButton(QIcon::fromTheme(QStringLiteral("list-add")),
                                      i18n("New ensemble"), leftBox);
    left->addWidget(newButton);
    splitter->addWidget(leftBox);

    // Right: the editor for the selected entry.
    auto *rightScroll = new QScrollArea(splitter);
    rightScroll->setWidgetResizable(true);
    auto *rightBox = new QWidget(rightScroll);
    auto *right = new QVBoxLayout(rightBox);

    auto *form = new QFormLayout;
    m_name = new QLineEdit(rightBox);
    form->addRow(i18n("Name"), m_name);
    m_description = new QLineEdit(rightBox);
    m_description->setPlaceholderText(i18n("What this ensemble is good at"));
    form->addRow(i18n("Description"), m_description);
    right->addLayout(form);

    auto *ctrlGroup = new QGroupBox(i18n("Controller"), rightBox);
    auto *ctrlForm = new QFormLayout(ctrlGroup);
    m_controllerBackend = new QComboBox(ctrlGroup);
    ctrlForm->addRow(i18n("Engine"), m_controllerBackend);
    m_controllerModel = new QComboBox(ctrlGroup);
    m_controllerModel->setEditable(true); // a model id may predate the catalogue
    ctrlForm->addRow(i18n("Model"), m_controllerModel);
    m_controllerIsolation = new QComboBox(ctrlGroup);
    fillIsolation(m_controllerIsolation);
    ctrlForm->addRow(i18n("Works in"), m_controllerIsolation);
    right->addWidget(ctrlGroup);
    // Switching engines clears the model: a model id belongs to one engine's
    // vocabulary, and carrying it across would write an ensemble whose launch
    // is refused (or silently downgraded) the first time it runs.
    connect(m_controllerBackend, &QComboBox::currentIndexChanged, this, [this] {
        fillModels(m_controllerModel, m_controllerBackend->currentData().toString(),
                   QString());
    });

    auto *workerGroup = new QGroupBox(i18n("Worker roles the controller may launch"), rightBox);
    auto *workerLayout = new QVBoxLayout(workerGroup);
    m_workerBox = new QVBoxLayout;
    workerLayout->addLayout(m_workerBox);
    auto *addWorker = new QPushButton(QIcon::fromTheme(QStringLiteral("list-add")),
                                      i18n("Add role"), workerGroup);
    workerLayout->addWidget(addWorker, 0, Qt::AlignLeft);
    right->addWidget(workerGroup);
    connect(addWorker, &QPushButton::clicked, this,
            [this] { addWorkerRow(EnsembleMember{}); });

    auto *promptGroup = new QGroupBox(i18n("Master prompt"), rightBox);
    auto *promptLayout = new QVBoxLayout(promptGroup);
    auto *promptHelp = new QLabel(
        i18n("The controller's opening briefing. Leave empty to use Agent Kate's default, "
             "which explains the orchestration tools. Placeholders: "
             "<code>{{ensemble_name}}</code>, <code>{{workspace}}</code>, "
             "<code>{{worker_roster}}</code> (the table of roles above, with the exact "
             "launch arguments for each)."),
        promptGroup);
    promptHelp->setWordWrap(true);
    promptHelp->setTextFormat(Qt::RichText);
    promptLayout->addWidget(promptHelp);
    m_prompt = new QPlainTextEdit(promptGroup);
    m_prompt->setMinimumHeight(140);
    promptLayout->addWidget(m_prompt);
    auto *showDefault = new QPushButton(i18n("Start from the default"), promptGroup);
    promptLayout->addWidget(showDefault, 0, Qt::AlignLeft);
    connect(showDefault, &QPushButton::clicked, this, [this] {
        m_prompt->setPlainText(EnsembleCatalog::self()->defaultMasterPrompt());
    });
    right->addWidget(promptGroup);

    m_status = new QLabel(rightBox);
    m_status->setWordWrap(true);
    right->addWidget(m_status);
    right->addStretch(1);
    rightScroll->setWidget(rightBox);
    splitter->addWidget(rightScroll);
    splitter->setStretchFactor(1, 1);

    auto *buttons = new QDialogButtonBox(this);
    auto *saveButton = buttons->addButton(i18n("Save"), QDialogButtonBox::ApplyRole);
    m_deleteButton = buttons->addButton(i18n("Delete"), QDialogButtonBox::DestructiveRole);
    buttons->addButton(QDialogButtonBox::Close);
    connect(saveButton, &QPushButton::clicked, this, &EnsembleDialog::onSave);
    connect(m_deleteButton, &QPushButton::clicked, this, &EnsembleDialog::onDelete);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::accept);
    root->addWidget(buttons);

    connect(newButton, &QPushButton::clicked, this, [this] {
        m_list->clearSelection();
        Ensemble blank;
        blank.name = i18n("My ensemble");
        blank.controller.isolation = QStringLiteral("auto");
        showEnsemble(blank);
        m_name->setFocus();
        m_name->selectAll();
    });
    connect(m_list, &QListWidget::currentItemChanged, this,
            [this](QListWidgetItem *item) {
                if (m_loading || !item) {
                    return;
                }
                showEnsemble(EnsembleCatalog::self()->get(item->data(Qt::UserRole).toString()));
            });
    // A save (ours or another window's) refreshes the catalogue; keep the list
    // in step without disturbing the entry being edited.
    connect(EnsembleCatalog::self(), &EnsembleCatalog::changed, this,
            [this] { reloadList(m_name->text()); });

    fillBackends(m_controllerBackend, QString());
    EnsembleCatalog::self()->fetch(m_core);
    reloadList();
    resize(880, 620);
}

void EnsembleDialog::fillBackends(QComboBox *backends, const QString &keep)
{
    QSignalBlocker block(backends);
    backends->clear();
    for (const HarnessTraits &t : HarnessRegistry::self()->all()) {
        backends->addItem(t.displayName, t.id);
    }
    selectData(backends, keep);
}

void EnsembleDialog::fillModels(QComboBox *models, const QString &backend,
                                const QString &keep)
{
    QSignalBlocker block(models);
    models->clear();
    models->addItem(i18n("Engine default"), QString());
    const auto choices =
        HarnessRegistry::self()->modelChoices(backend, ProviderStore::directId());
    const auto append = [models](const QStringList &entries) {
        for (const QString &entry : entries) {
            const QString value = entry.section(QLatin1Char('|'), 0, 0);
            const QString name = entry.section(QLatin1Char('|'), 1);
            if (!value.isEmpty() && models->findData(value) < 0) {
                models->addItem(name.isEmpty() ? value : name, value);
            }
        }
    };
    append(choices.recommended);
    append(choices.all);
    if (keep.isEmpty()) {
        models->setCurrentIndex(0);
        return;
    }
    const int idx = models->findData(keep);
    if (idx >= 0) {
        models->setCurrentIndex(idx);
    } else {
        // An id this UI has never discovered (an ensemble written elsewhere, a
        // model not in the cache yet) stays exactly as written — the harness
        // owns the vocabulary, not us.
        models->setEditText(keep);
    }
}

void EnsembleDialog::reloadList(const QString &selectName)
{
    m_loading = true;
    m_list->clear();
    for (const Ensemble &e : EnsembleCatalog::self()->list()) {
        auto *item = new QListWidgetItem(
            e.builtIn ? i18nc("built-in ensemble entry", "%1 (built-in)", e.name) : e.name,
            m_list);
        item->setData(Qt::UserRole, e.name);
        if (!e.description.isEmpty()) {
            item->setToolTip(e.description);
        }
    }
    m_loading = false;
    if (m_list->count() == 0) {
        return;
    }
    for (int i = 0; i < m_list->count(); ++i) {
        if (m_list->item(i)->data(Qt::UserRole).toString() == selectName) {
            m_list->setCurrentRow(i);
            return;
        }
    }
    if (selectName.isEmpty()) {
        m_list->setCurrentRow(0);
    }
}

void EnsembleDialog::clearWorkerRows()
{
    for (const WorkerRow &row : m_workerRows) {
        row.widget->deleteLater();
    }
    m_workerRows.clear();
}

void EnsembleDialog::addWorkerRow(const EnsembleMember &w)
{
    WorkerRow row;
    row.widget = new QWidget;
    auto *box = new QVBoxLayout(row.widget);
    box->setContentsMargins(0, 0, 0, 6);

    auto *top = new QHBoxLayout;
    row.role = new QLineEdit(w.role, row.widget);
    row.role->setPlaceholderText(i18n("Role (e.g. coder)"));
    top->addWidget(row.role, 1);
    row.backend = new QComboBox(row.widget);
    fillBackends(row.backend, w.backend);
    top->addWidget(row.backend, 1);
    row.model = new QComboBox(row.widget);
    row.model->setEditable(true);
    fillModels(row.model, w.backend, w.model);
    top->addWidget(row.model, 2);
    row.isolation = new QComboBox(row.widget);
    fillIsolation(row.isolation);
    selectData(row.isolation, w.isolation.isEmpty() ? QStringLiteral("auto") : w.isolation);
    top->addWidget(row.isolation, 1);
    auto *remove = new QToolButton(row.widget);
    remove->setIcon(QIcon::fromTheme(QStringLiteral("list-remove")));
    remove->setToolTip(i18n("Remove this role"));
    top->addWidget(remove);
    box->addLayout(top);

    row.notes = new QLineEdit(w.notes, row.widget);
    row.notes->setPlaceholderText(
        i18n("When the controller should use this role — it is the only hint it gets"));
    box->addWidget(row.notes);

    QComboBox *backendCombo = row.backend;
    QComboBox *modelCombo = row.model;
    connect(backendCombo, &QComboBox::currentIndexChanged, this,
            [this, backendCombo, modelCombo] {
                fillModels(modelCombo, backendCombo->currentData().toString(), QString());
            });
    connect(remove, &QToolButton::clicked, this, [this, widget = row.widget] {
        for (int i = 0; i < m_workerRows.size(); ++i) {
            if (m_workerRows.at(i).widget == widget) {
                m_workerRows.removeAt(i);
                break;
            }
        }
        widget->deleteLater();
    });

    m_workerBox->addWidget(row.widget);
    m_workerRows.append(row);
}

void EnsembleDialog::showEnsemble(const Ensemble &e)
{
    m_name->setText(e.name);
    m_description->setText(e.description);
    selectData(m_controllerBackend, e.controller.backend);
    fillModels(m_controllerModel, e.controller.backend, e.controller.model);
    selectData(m_controllerIsolation, e.controller.isolation.isEmpty()
                   ? QStringLiteral("auto")
                   : e.controller.isolation);
    clearWorkerRows();
    for (const EnsembleMember &w : e.workers) {
        addWorkerRow(w);
    }
    m_prompt->setPlainText(e.masterPrompt);
    m_deleteButton->setEnabled(EnsembleCatalog::self()->contains(e.name));
    m_status->setText(e.builtIn
                          ? i18n("This is a built-in ensemble. Saving keeps your version; "
                                 "deleting it removes it (your own copies are unaffected).")
                          : QString());
}

Ensemble EnsembleDialog::collect() const
{
    Ensemble e;
    e.name = m_name->text().trimmed();
    e.description = m_description->text().trimmed();
    e.controller.backend = m_controllerBackend->currentData().toString();
    e.controller.model = modelIdFor(m_controllerModel);
    e.controller.isolation = m_controllerIsolation->currentData().toString();
    for (const WorkerRow &row : m_workerRows) {
        EnsembleMember w;
        w.role = row.role->text().trimmed();
        w.backend = row.backend->currentData().toString();
        w.model = modelIdFor(row.model);
        w.isolation = row.isolation->currentData().toString();
        w.notes = row.notes->text().trimmed();
        e.workers.append(w);
    }
    e.masterPrompt = m_prompt->toPlainText().trimmed();
    return e;
}

void EnsembleDialog::onSave()
{
    const Ensemble e = collect();
    if (e.name.isEmpty()) {
        m_status->setText(i18n("Give the ensemble a name first."));
        return;
    }
    QPointer<EnsembleDialog> self(this);
    EnsembleCatalog::self()->save(m_core, e, [self, name = e.name](const QString &error) {
        if (!self) {
            return;
        }
        self->m_status->setText(error.isEmpty()
                                    ? i18n("Saved “%1”.", name)
                                    : i18n("Could not save: %1", error));
        if (error.isEmpty()) {
            self->m_deleteButton->setEnabled(true);
        }
    });
}

void EnsembleDialog::onDelete()
{
    const QString name = m_name->text().trimmed();
    if (name.isEmpty() || !EnsembleCatalog::self()->contains(name)) {
        return;
    }
    // UX (audit F50): warningContinueCancel defaults to Continue, so Enter deleted the
    // ensemble. The Dangerous option moves the default onto Cancel (KF6 header contract,
    // same as CleanupDialog's permanent-loss confirmation).
    if (KMessageBox::warningContinueCancel(
            this, i18n("Delete the ensemble “%1”?", name),
            i18nc("@title:window", "Delete Ensemble"), KStandardGuiItem::del(),
            KStandardGuiItem::cancel(), QString(),
            KMessageBox::Options(KMessageBox::Notify | KMessageBox::Dangerous))
        != KMessageBox::Continue) {
        return;
    }
    QPointer<EnsembleDialog> self(this);
    EnsembleCatalog::self()->remove(m_core, name, [self, name](const QString &error) {
        if (!self) {
            return;
        }
        self->m_status->setText(error.isEmpty() ? i18n("Deleted “%1”.", name)
                                                : i18n("Could not delete: %1", error));
    });
}
