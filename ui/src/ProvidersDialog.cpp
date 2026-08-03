#include "ProvidersDialog.h"

#include "ipc/CoreClient.h"
#include "state/HarnessTraits.h"

#include <KLocalizedString>

#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QFrame>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QMessageBox>
#include <QPushButton>
#include <QVBoxLayout>

namespace {

// A human label for a model slot key.
QString slotLabel(const QString &slot)
{
    if (slot == QLatin1String("main")) {
        return i18n("Main model");
    }
    if (slot == QLatin1String("opus")) {
        return i18n("Opus slot");
    }
    if (slot == QLatin1String("sonnet")) {
        return i18n("Sonnet slot");
    }
    if (slot == QLatin1String("haiku")) {
        return i18n("Haiku slot");
    }
    if (slot == QLatin1String("subagent")) {
        return i18n("Subagent / small-fast");
    }
    return slot;
}

bool baseUrlLooksValid(const QString &url)
{
    const QString u = url.trimmed();
    return u.startsWith(QLatin1String("https://")) ||
           u.startsWith(QLatin1String("http://localhost")) ||
           u.startsWith(QLatin1String("http://127.0.0.1"));
}

// The marker session.EnvNotStored writes in place of a redacted env VALUE.
// KIMI_CODE_HOME is a path and never redacted in practice, but a marker that
// did land here must not be offered as a directory.
const QLatin1String kEnvNotStored("__agentkate_not_stored__");

} // namespace

ProvidersDialog::ProvidersDialog(QWidget *parent, CoreClient *core)
    : QDialog(parent)
    , m_core(core)
{
    setWindowTitle(i18nc("@title:window", "API Providers"));
    resize(680, 620);

    m_profiles = ProviderStore::load();

    auto *outer = new QVBoxLayout(this);

    auto *claudeSection = new QGroupBox(i18n("API providers (Claude Code)"), this);
    auto *claudeLayout = new QVBoxLayout(claudeSection);

    auto *intro = new QLabel(
        i18n("Route an agent's Claude Code harness at a third-party, "
             "Anthropic-compatible API. Pick a provider per agent when you start it; "
             "<b>Claude (direct)</b> is the default and changes nothing."),
        claudeSection);
    intro->setWordWrap(true);
    claudeLayout->addWidget(intro);

    auto *body = new QHBoxLayout;
    claudeLayout->addLayout(body, 1);

    // --- Left: profile list + add/remove ---
    auto *leftCol = new QVBoxLayout;
    m_list = new QListWidget(claudeSection);
    leftCol->addWidget(m_list, 1);
    auto *listBtns = new QHBoxLayout;
    auto *addBtn = new QPushButton(i18nc("@action:button", "Add"), claudeSection);
    m_removeBtn = new QPushButton(i18nc("@action:button", "Remove"), claudeSection);
    listBtns->addWidget(addBtn);
    listBtns->addWidget(m_removeBtn);
    listBtns->addStretch(1);
    leftCol->addLayout(listBtns);
    body->addLayout(leftCol);

    // --- Right: edit form ---
    auto *form = new QFormLayout;
    m_name = new QLineEdit(claudeSection);
    m_baseUrl = new QLineEdit(claudeSection);
    m_baseUrl->setPlaceholderText(QStringLiteral("https://api.fireworks.ai/inference"));
    m_envVar = new QLineEdit(claudeSection);
    m_envVar->setPlaceholderText(QStringLiteral("FIREWORKS_API_KEY"));
    m_key = new QLineEdit(claudeSection);
    m_key->setEchoMode(QLineEdit::Password);
    m_key->setPlaceholderText(i18n("enter to set / replace"));
    m_keyStatus = new QLabel(claudeSection);
    m_keyStatus->setTextFormat(Qt::PlainText);

    form->addRow(i18n("Name"), m_name);
    form->addRow(i18n("Base URL"), m_baseUrl);
    form->addRow(i18n("API key"), m_key);
    form->addRow(QString(), m_keyStatus);
    form->addRow(i18n("Key env var"), m_envVar);

    auto *sep = new QFrame(claudeSection);
    sep->setFrameShape(QFrame::HLine);
    form->addRow(sep);

    auto *modelsHint = new QLabel(
        i18n("Model ids are provider-specific (see your provider's model list). "
             "The Main model is used when an agent leaves Model on “Provider default”."),
        claudeSection);
    modelsHint->setWordWrap(true);
    form->addRow(modelsHint);

    for (const QString &slot : ProviderStore::modelSlots()) {
        auto *edit = new QLineEdit(claudeSection);
        m_modelEdits.insert(slot, edit);
        form->addRow(slotLabel(slot), edit);
    }

    m_walletNote = new QLabel(claudeSection);
    m_walletNote->setWordWrap(true);
    form->addRow(m_walletNote);

    body->addLayout(form, 1);
    outer->addWidget(claudeSection, 1);

    // --- The kimi provider registry (plan 26) ---
    buildKimiSection(outer);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Save | QDialogButtonBox::Cancel,
                                         this);
    outer->addWidget(buttons);

    if (!ProviderStore::walletAvailable()) {
        m_key->setEnabled(false);
        m_walletNote->setText(
            i18n("⚠ KWallet is unavailable, so API keys can't be stored securely. "
                 "Supply the key through the environment variable named above instead."));
    } else {
        m_walletNote->clear();
    }

    connect(addBtn, &QPushButton::clicked, this, &ProvidersDialog::addProfile);
    connect(m_removeBtn, &QPushButton::clicked, this, &ProvidersDialog::removeProfile);
    connect(buttons, &QDialogButtonBox::accepted, this, &ProvidersDialog::saveAndAccept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(m_list, &QListWidget::currentRowChanged, this, [this](int row) {
        commitForm();
        loadIntoForm(row);
    });
    // Stash key edits per profile as the user types, so switching rows keeps them.
    connect(m_key, &QLineEdit::textEdited, this, [this](const QString &text) {
        if (m_current >= 0 && m_current < m_profiles.size()) {
            m_pendingKeys.insert(m_profiles[m_current].id, text);
        }
    });

    rebuildList(0);
}

void ProvidersDialog::rebuildList(int selectRow)
{
    m_list->blockSignals(true);
    m_list->clear();
    for (const ProviderProfile &p : m_profiles) {
        m_list->addItem(p.name.isEmpty() ? p.id : p.name);
    }
    m_list->blockSignals(false);
    if (selectRow >= 0 && selectRow < m_profiles.size()) {
        m_list->setCurrentRow(selectRow); // fires currentRowChanged → loadIntoForm
    } else {
        loadIntoForm(m_list->currentRow());
    }
}

void ProvidersDialog::loadIntoForm(int row)
{
    m_current = row;
    const bool valid = row >= 0 && row < m_profiles.size();
    const ProviderProfile p = valid ? m_profiles[row] : ProviderProfile();

    m_name->setText(p.name);
    m_baseUrl->setText(p.baseUrl);
    m_envVar->setText(p.envVar);
    for (auto it = m_modelEdits.constBegin(); it != m_modelEdits.constEnd(); ++it) {
        it.value()->setText(p.models.value(it.key()));
    }

    // Key field shows the pending edit if any, else blank with a stored/unset hint.
    if (valid && m_pendingKeys.contains(p.id)) {
        m_key->setText(m_pendingKeys.value(p.id));
    } else {
        m_key->clear();
    }
    if (valid && p.routed()) {
        const bool stored = ProviderStore::hasStoredKey(p.id);
        const bool fromEnv = !p.envVar.isEmpty() &&
                             !qEnvironmentVariableIsEmpty(p.envVar.toLocal8Bit().constData());
        if (stored) {
            m_keyStatus->setText(i18n("A key is stored in KWallet."));
        } else if (fromEnv) {
            m_keyStatus->setText(i18n("Resolved from %1 in the environment.", p.envVar));
        } else {
            m_keyStatus->setText(i18n("No key set."));
        }
    } else {
        m_keyStatus->clear();
    }

    updateEditableState();
}

void ProvidersDialog::commitForm()
{
    if (m_current < 0 || m_current >= m_profiles.size()) {
        return;
    }
    ProviderProfile &p = m_profiles[m_current];
    if (p.id == ProviderStore::directId()) {
        return; // the direct sentinel is read-only
    }
    p.name = m_name->text().trimmed();
    p.baseUrl = m_baseUrl->text().trimmed();
    p.envVar = m_envVar->text().trimmed();
    for (auto it = m_modelEdits.constBegin(); it != m_modelEdits.constEnd(); ++it) {
        const QString v = it.value()->text().trimmed();
        if (v.isEmpty()) {
            p.models.remove(it.key());
        } else {
            p.models.insert(it.key(), v);
        }
    }
}

void ProvidersDialog::updateEditableState()
{
    const bool valid = m_current >= 0 && m_current < m_profiles.size();
    const bool isDirect = valid && m_profiles[m_current].id == ProviderStore::directId();
    const bool editable = valid && !isDirect;

    m_name->setEnabled(editable);
    m_baseUrl->setEnabled(editable);
    m_envVar->setEnabled(editable);
    m_key->setEnabled(editable && ProviderStore::walletAvailable());
    for (QLineEdit *e : m_modelEdits) {
        e->setEnabled(editable);
    }
    // The built-in presets are editable but cannot be removed; the direct
    // sentinel is neither.
    m_removeBtn->setEnabled(editable && !m_profiles[m_current].builtin);
}

void ProvidersDialog::addProfile()
{
    commitForm();
    // Generate an id unique among current profiles.
    QStringList used;
    for (const ProviderProfile &p : m_profiles) {
        used << p.id;
    }
    int n = m_profiles.size();
    QString id;
    do {
        id = QStringLiteral("provider-%1").arg(++n);
    } while (used.contains(id));

    ProviderProfile p;
    p.id = id;
    p.name = i18n("New provider");
    m_profiles.append(p);
    rebuildList(m_profiles.size() - 1);
}

void ProvidersDialog::removeProfile()
{
    if (m_current < 0 || m_current >= m_profiles.size()) {
        return;
    }
    const ProviderProfile p = m_profiles[m_current];
    if (p.id == ProviderStore::directId() || p.builtin) {
        return;
    }
    m_pendingKeys.remove(p.id);
    m_profiles.removeAt(m_current);
    rebuildList(qMin(m_current, m_profiles.size() - 1));
}

void ProvidersDialog::saveAndAccept()
{
    commitForm();

    // Validate routed profiles before persisting.
    for (const ProviderProfile &p : m_profiles) {
        if (p.id == ProviderStore::directId()) {
            continue;
        }
        if (p.routed() && !baseUrlLooksValid(p.baseUrl)) {
            QMessageBox::warning(
                this, i18nc("@title:window", "Invalid base URL"),
                i18n("Provider “%1” has an invalid base URL.\n\n"
                     "Use an https:// URL (http:// is allowed only for localhost).",
                     p.name.isEmpty() ? p.id : p.name));
            return;
        }
    }

    ProviderStore::save(m_profiles);
    // m_pendingKeys only holds keys the user actually edited, so apply each one —
    // including an emptied field, which setKey() treats as "clear the entry".
    // Skipping empty values here made a stored key impossible to remove from the UI.
    for (auto it = m_pendingKeys.constBegin(); it != m_pendingKeys.constEnd(); ++it) {
        ProviderStore::setKey(it.key(), it.value());
    }
    accept();
}

// --- the kimi provider registry section (plan 26 phase 4) -------------------

void ProvidersDialog::buildKimiSection(QVBoxLayout *outer)
{
    m_kimiSection = new QGroupBox(i18n("Kimi provider registry"), this);
    auto *layout = new QVBoxLayout(m_kimiSection);

    auto *note = new QLabel(
        i18n("Kimi Code keeps its own provider registry inside the engine's "
             "home directory. Keys are held by kimi's credential store — "
             "Agent Kate never sees or stores them."),
        m_kimiSection);
    note->setWordWrap(true);
    layout->addWidget(note);

    auto *homeRow = new QHBoxLayout;
    homeRow->addWidget(new QLabel(i18n("Registry:"), m_kimiSection));
    m_kimiHome = new QComboBox(m_kimiSection);
    m_kimiHome->setToolTip(
        i18n("Which registry to edit: the user's default kimi home, or a "
             "specific agent's private home (KIMI_CODE_HOME). Two kimi agents "
             "in one project can target different provider sets."));
    homeRow->addWidget(m_kimiHome, 1);
    layout->addLayout(homeRow);

    m_kimiList = new QListWidget(m_kimiSection);
    m_kimiList->setSelectionMode(QAbstractItemView::SingleSelection);
    layout->addWidget(m_kimiList, 1);

    m_kimiStatus = new QLabel(m_kimiSection);
    m_kimiStatus->setWordWrap(true);
    layout->addWidget(m_kimiStatus);

    auto *btns = new QHBoxLayout;
    m_kimiRefresh = new QPushButton(i18nc("@action:button", "Refresh"), m_kimiSection);
    m_kimiImport = new QPushButton(
        i18nc("@action:button", "Import from models.dev"), m_kimiSection);
    m_kimiImport->setToolTip(
        i18n("Discover and import providers from the public models.dev catalog "
             "(runs “kimi provider catalog”)."));
    m_kimiAdd = new QPushButton(i18nc("@action:button", "Add from URL…"), m_kimiSection);
    m_kimiAdd->setToolTip(
        i18n("Import every provider listed in a custom registry (api.json) URL "
             "(runs “kimi provider add”)."));
    m_kimiRemove = new QPushButton(i18nc("@action:button", "Remove"), m_kimiSection);
    btns->addWidget(m_kimiRefresh);
    btns->addWidget(m_kimiImport);
    btns->addWidget(m_kimiAdd);
    btns->addWidget(m_kimiRemove);
    btns->addStretch(1);
    layout->addLayout(btns);

    outer->addWidget(m_kimiSection, 1);

    connect(m_kimiRefresh, &QPushButton::clicked, this,
            &ProvidersDialog::refreshKimiProviders);
    connect(m_kimiImport, &QPushButton::clicked, this,
            &ProvidersDialog::kimiImportCatalog);
    connect(m_kimiAdd, &QPushButton::clicked, this, &ProvidersDialog::kimiAddFromUrl);
    connect(m_kimiRemove, &QPushButton::clicked, this,
            &ProvidersDialog::kimiRemoveSelected);
    connect(m_kimiHome, &QComboBox::currentIndexChanged, this,
            [this] { refreshKimiProviders(); });
    connect(m_kimiList, &QListWidget::currentRowChanged, this, [this](int row) {
        m_kimiRemove->setEnabled(row >= 0);
    });

    // The section exists only where an engine keeps a registry at all —
    // driven by the trait, never by a backend name compare.
    const auto anyRegistry = [] {
        const QList<HarnessTraits> all = HarnessRegistry::self()->all();
        for (const HarnessTraits &t : all) {
            if (t.providerRegistry) {
                return true;
            }
        }
        return false;
    };
    m_kimiSection->setVisible(anyRegistry());
    connect(HarnessRegistry::self(), &HarnessRegistry::changed, m_kimiSection,
            [this, anyRegistry] { m_kimiSection->setVisible(anyRegistry()); });

    if (!m_core || !m_core->isConnected()) {
        setKimiBusy(true, i18n("Not connected to the Agent Kate core — the "
                               "registry cannot be read right now."));
        return;
    }
    refreshKimiHomes();
    refreshKimiProviders();
}

QString ProvidersDialog::kimiHomeThreadId() const
{
    return m_kimiHome ? m_kimiHome->currentData().toString() : QString();
}

void ProvidersDialog::setKimiBusy(bool busy, const QString &status)
{
    m_kimiRefresh->setEnabled(!busy);
    m_kimiImport->setEnabled(!busy);
    m_kimiAdd->setEnabled(!busy);
    m_kimiRemove->setEnabled(!busy && m_kimiList->currentRow() >= 0);
    m_kimiHome->setEnabled(!busy);
    m_kimiStatus->setText(status);
}

// refreshKimiHomes lists the selectable registries: the user's default home,
// plus every persisted thread whose Env carries a KIMI_CODE_HOME overlay —
// read from session.listThreads, the record surface the UI already holds
// (the RPC is UI-only; the Env values it returns are exactly what the home
// selector exists to show).
void ProvidersDialog::refreshKimiHomes()
{
    m_kimiHome->clear();
    m_kimiHome->addItem(i18n("User default"), QString());
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    m_core->call(
        QStringLiteral("session.listThreads"), QJsonObject{},
        [this](const QJsonObject &result, const QJsonObject &error) {
            if (!error.isEmpty()) {
                return; // the default entry alone is still a working selector
            }
            const QJsonArray threads = result.value(QStringLiteral("threads")).toArray();
            for (const QJsonValue &v : threads) {
                const QJsonObject rec = v.toObject();
                const QString home = rec.value(QStringLiteral("env"))
                                         .toObject()
                                         .value(QStringLiteral("KIMI_CODE_HOME"))
                                         .toString();
                if (home.isEmpty() || home == kEnvNotStored) {
                    continue;
                }
                const QString threadId = rec.value(QStringLiteral("threadId")).toString();
                QString title = rec.value(QStringLiteral("title")).toString();
                if (title.isEmpty()) {
                    title = threadId;
                }
                m_kimiHome->addItem(
                    i18nc("agent title and its private kimi home path", "%1 — %2",
                          title, home),
                    threadId);
            }
        },
        this);
}

void ProvidersDialog::renderKimiProviders(const QJsonArray &providers)
{
    m_kimiList->clear();
    for (const QJsonValue &v : providers) {
        const QJsonObject p = v.toObject();
        const QString id = p.value(QStringLiteral("id")).toString();
        const int models = p.value(QStringLiteral("models")).toArray().size();
        QStringList bits;
        bits << p.value(QStringLiteral("type")).toString();
        bits << p.value(QStringLiteral("baseUrl")).toString();
        bits << i18np("%1 model", "%1 models", models);
        bits << (p.value(QStringLiteral("hasApiKey")).toBool()
                     ? i18n("credential held by kimi")
                     : i18n("no credential"));
        const QString status = p.value(QStringLiteral("status")).toString();
        if (!status.isEmpty()) {
            bits << status;
        }
        auto *item = new QListWidgetItem(
            i18nc("provider id and its facts", "%1   (%2)", id,
                  bits.join(i18nc("list separator", ", "))),
            m_kimiList);
        item->setData(Qt::UserRole, id);
    }
    m_kimiRemove->setEnabled(m_kimiList->currentRow() >= 0);
    if (providers.isEmpty()) {
        m_kimiStatus->setText(
            i18n("This registry has no providers yet. Import from models.dev, "
                 "or add a custom registry URL."));
    } else {
        m_kimiStatus->clear();
    }
}

void ProvidersDialog::refreshKimiProviders()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    setKimiBusy(true, i18n("Reading the registry…"));
    m_core->call(
        QStringLiteral("kimiProvider.list"),
        QJsonObject{{QStringLiteral("threadId"), kimiHomeThreadId()}},
        [this](const QJsonObject &result, const QJsonObject &error) {
            setKimiBusy(false);
            if (!error.isEmpty()) {
                m_kimiStatus->setText(
                    error.value(QStringLiteral("message")).toString());
                return;
            }
            renderKimiProviders(result.value(QStringLiteral("providers")).toArray());
        },
        this);
}

void ProvidersDialog::kimiImportCatalog()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    setKimiBusy(true, i18n("Importing providers from models.dev…"));
    m_core->call(
        QStringLiteral("kimiProvider.catalog"),
        QJsonObject{{QStringLiteral("threadId"), kimiHomeThreadId()}},
        [this](const QJsonObject &result, const QJsonObject &error) {
            setKimiBusy(false);
            if (!error.isEmpty()) {
                m_kimiStatus->setText(
                    error.value(QStringLiteral("message")).toString());
                return;
            }
            renderKimiProviders(result.value(QStringLiteral("providers")).toArray());
        },
        this);
}

void ProvidersDialog::kimiAddFromUrl()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    bool ok = false;
    const QString url = QInputDialog::getText(
        this, i18nc("@title:window", "Add Providers from a Registry URL"),
        i18n("URL of a custom provider registry (api.json). Every provider it "
             "lists is imported."),
        QLineEdit::Normal, QString(), &ok);
    if (!ok || url.trimmed().isEmpty()) {
        return;
    }
    setKimiBusy(true, i18n("Importing providers from %1…", url.trimmed()));
    m_core->call(
        QStringLiteral("kimiProvider.add"),
        QJsonObject{{QStringLiteral("threadId"), kimiHomeThreadId()},
                    {QStringLiteral("url"), url.trimmed()}},
        [this](const QJsonObject &result, const QJsonObject &error) {
            setKimiBusy(false);
            if (!error.isEmpty()) {
                m_kimiStatus->setText(
                    error.value(QStringLiteral("message")).toString());
                return;
            }
            renderKimiProviders(result.value(QStringLiteral("providers")).toArray());
        },
        this);
}

void ProvidersDialog::kimiRemoveSelected()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    QListWidgetItem *item = m_kimiList->currentItem();
    if (!item) {
        return;
    }
    const QString id = item->data(Qt::UserRole).toString();
    // The CLI's own consequence, stated before the click: `kimi provider
    // remove` removes the provider AND every model alias that referenced it.
    const auto answer = QMessageBox::warning(
        this, i18nc("@title:window", "Remove Provider"),
        i18n("Remove “%1” from this registry?\n\nThis also removes every "
             "model alias that referenced it — agents whose model pointed at "
             "one of those aliases will need a different model.",
             id),
        QMessageBox::Yes | QMessageBox::No, QMessageBox::No);
    if (answer != QMessageBox::Yes) {
        return;
    }
    setKimiBusy(true, i18n("Removing %1…", id));
    m_core->call(
        QStringLiteral("kimiProvider.remove"),
        QJsonObject{{QStringLiteral("threadId"), kimiHomeThreadId()},
                    {QStringLiteral("id"), id}},
        [this](const QJsonObject &result, const QJsonObject &error) {
            setKimiBusy(false);
            if (!error.isEmpty()) {
                m_kimiStatus->setText(
                    error.value(QStringLiteral("message")).toString());
                return;
            }
            renderKimiProviders(result.value(QStringLiteral("providers")).toArray());
        },
        this);
}
