#include "ProvidersDialog.h"

#include <QDialogButtonBox>
#include <QFormLayout>
#include <QFrame>
#include <QHBoxLayout>
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
        return ProvidersDialog::tr("Main model");
    }
    if (slot == QLatin1String("opus")) {
        return ProvidersDialog::tr("Opus slot");
    }
    if (slot == QLatin1String("sonnet")) {
        return ProvidersDialog::tr("Sonnet slot");
    }
    if (slot == QLatin1String("haiku")) {
        return ProvidersDialog::tr("Haiku slot");
    }
    if (slot == QLatin1String("subagent")) {
        return ProvidersDialog::tr("Subagent / small-fast");
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

} // namespace

ProvidersDialog::ProvidersDialog(QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(tr("API Providers"));
    resize(680, 520);

    m_profiles = ProviderStore::load();

    auto *outer = new QVBoxLayout(this);

    auto *intro = new QLabel(
        tr("Route an agent's Claude Code harness at a third-party, "
           "Anthropic-compatible API. Pick a provider per agent when you start it; "
           "<b>Claude (direct)</b> is the default and changes nothing."),
        this);
    intro->setWordWrap(true);
    outer->addWidget(intro);

    auto *body = new QHBoxLayout;
    outer->addLayout(body, 1);

    // --- Left: profile list + add/remove ---
    auto *leftCol = new QVBoxLayout;
    m_list = new QListWidget(this);
    leftCol->addWidget(m_list, 1);
    auto *listBtns = new QHBoxLayout;
    auto *addBtn = new QPushButton(tr("Add"), this);
    m_removeBtn = new QPushButton(tr("Remove"), this);
    listBtns->addWidget(addBtn);
    listBtns->addWidget(m_removeBtn);
    listBtns->addStretch(1);
    leftCol->addLayout(listBtns);
    body->addLayout(leftCol);

    // --- Right: edit form ---
    auto *form = new QFormLayout;
    m_name = new QLineEdit(this);
    m_baseUrl = new QLineEdit(this);
    m_baseUrl->setPlaceholderText(QStringLiteral("https://api.fireworks.ai/inference"));
    m_envVar = new QLineEdit(this);
    m_envVar->setPlaceholderText(QStringLiteral("FIREWORKS_API_KEY"));
    m_key = new QLineEdit(this);
    m_key->setEchoMode(QLineEdit::Password);
    m_key->setPlaceholderText(tr("enter to set / replace"));
    m_keyStatus = new QLabel(this);
    m_keyStatus->setTextFormat(Qt::PlainText);

    form->addRow(tr("Name"), m_name);
    form->addRow(tr("Base URL"), m_baseUrl);
    form->addRow(tr("API key"), m_key);
    form->addRow(QString(), m_keyStatus);
    form->addRow(tr("Key env var"), m_envVar);

    auto *sep = new QFrame(this);
    sep->setFrameShape(QFrame::HLine);
    form->addRow(sep);

    auto *modelsHint = new QLabel(
        tr("Model ids are provider-specific (see your provider's model list). "
           "The Main model is used when an agent leaves Model on “Provider default”."),
        this);
    modelsHint->setWordWrap(true);
    form->addRow(modelsHint);

    for (const QString &slot : ProviderStore::modelSlots()) {
        auto *edit = new QLineEdit(this);
        m_modelEdits.insert(slot, edit);
        form->addRow(slotLabel(slot), edit);
    }

    m_walletNote = new QLabel(this);
    m_walletNote->setWordWrap(true);
    form->addRow(m_walletNote);

    body->addLayout(form, 1);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Save | QDialogButtonBox::Cancel,
                                         this);
    outer->addWidget(buttons);

    if (!ProviderStore::walletAvailable()) {
        m_key->setEnabled(false);
        m_walletNote->setText(
            tr("⚠ KWallet is unavailable, so API keys can't be stored securely. "
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
            m_keyStatus->setText(tr("A key is stored in KWallet."));
        } else if (fromEnv) {
            m_keyStatus->setText(tr("Resolved from %1 in the environment.").arg(p.envVar));
        } else {
            m_keyStatus->setText(tr("No key set."));
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
    p.name = tr("New provider");
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
                this, tr("Invalid base URL"),
                tr("Provider “%1” has an invalid base URL.\n\n"
                   "Use an https:// URL (http:// is allowed only for localhost).")
                    .arg(p.name.isEmpty() ? p.id : p.name));
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
