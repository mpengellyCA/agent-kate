#include "SkillsDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDesktopServices>
#include <QDialog>
#include <QDialogButtonBox>
#include <QDir>
#include <QFont>
#include <QFormLayout>
#include <QHBoxLayout>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QListWidget>
#include <QPointer>
#include <QPushButton>
#include <QSplitter>
#include <QTextBrowser>
#include <QUrl>
#include <QVBoxLayout>

SkillsDialog::SkillsDialog(CoreClient *core, const QString &target, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_target(target)
{
    setWindowTitle(i18n("Manage Claude Skills"));
    resize(720, 480);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Install Claude Code skills from the central Agent Kate catalog "
             "into this project. Installed skills are symlinked, so editing "
             "the catalog copy updates every project that uses it."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    m_targetLabel = new QLabel(this);
    m_targetLabel->setText(i18n("Target: %1", m_target));
    m_targetLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);
    layout->addWidget(m_targetLabel);

    // The two-pane install/remove area lives above a read-only detail view, so
    // the user can inspect a skill's full markdown before installing it.
    auto *splitter = new QSplitter(Qt::Vertical, this);

    auto *columnsHost = new QWidget(splitter);
    auto *columns = new QHBoxLayout(columnsHost);
    columns->setContentsMargins(0, 0, 0, 0);

    auto *catalogCol = new QVBoxLayout;
    catalogCol->addWidget(new QLabel(i18n("Available"), columnsHost));
    m_catalogList = new QListWidget(columnsHost);
    catalogCol->addWidget(m_catalogList, 1);
    columns->addLayout(catalogCol, 1);

    auto *middle = new QVBoxLayout;
    middle->addStretch(1);
    m_installButton = new QPushButton(i18n("Install →"), columnsHost);
    m_uninstallButton = new QPushButton(i18n("← Remove"), columnsHost);
    middle->addWidget(m_installButton);
    middle->addWidget(m_uninstallButton);
    middle->addStretch(1);
    columns->addLayout(middle);

    auto *installedCol = new QVBoxLayout;
    installedCol->addWidget(new QLabel(i18n("Installed in this project"), columnsHost));
    m_installedList = new QListWidget(columnsHost);
    installedCol->addWidget(m_installedList, 1);
    columns->addLayout(installedCol, 1);

    splitter->addWidget(columnsHost);

    auto *detailHost = new QWidget(splitter);
    auto *detailLayout = new QVBoxLayout(detailHost);
    detailLayout->setContentsMargins(0, 0, 0, 0);
    detailLayout->addWidget(new QLabel(i18n("Skill details"), detailHost));
    m_detail = new QTextBrowser(detailHost);
    m_detail->setReadOnly(true);
    m_detail->setPlaceholderText(i18n("Select a skill to view its contents"));
    detailLayout->addWidget(m_detail, 1);
    splitter->addWidget(detailHost);
    splitter->setStretchFactor(0, 2);
    splitter->setStretchFactor(1, 1);

    layout->addWidget(splitter, 1);

    m_status = new QLabel(this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    auto *bottomRow = new QHBoxLayout;
    m_newSkillButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("document-new")), i18n("New Skill…"), this);
    m_refreshButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("view-refresh")), i18n("Refresh"), this);
    m_openCatalogButton = new QPushButton(i18n("Open Catalog Folder…"), this);
    bottomRow->addWidget(m_newSkillButton);
    bottomRow->addWidget(m_refreshButton);
    bottomRow->addWidget(m_openCatalogButton);
    bottomRow->addStretch(1);
    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    bottomRow->addWidget(buttons);
    layout->addLayout(bottomRow);

    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::accept);
    connect(m_installButton, &QPushButton::clicked, this, &SkillsDialog::install);
    connect(m_uninstallButton, &QPushButton::clicked, this, &SkillsDialog::uninstall);
    connect(m_openCatalogButton, &QPushButton::clicked, this, &SkillsDialog::openCatalogDir);
    connect(m_newSkillButton, &QPushButton::clicked, this, &SkillsDialog::createSkill);
    connect(m_refreshButton, &QPushButton::clicked, this, &SkillsDialog::refresh);
    connect(m_catalogList, &QListWidget::itemSelectionChanged, this, &SkillsDialog::updateButtons);
    connect(m_catalogList, &QListWidget::itemSelectionChanged, this, &SkillsDialog::loadDetail);
    connect(m_installedList, &QListWidget::itemSelectionChanged, this, &SkillsDialog::updateButtons);
    connect(m_catalogList, &QListWidget::itemDoubleClicked, this, &SkillsDialog::install);
    connect(m_installedList, &QListWidget::itemDoubleClicked, this, &SkillsDialog::uninstall);

    updateButtons();
    refresh();
}

void SkillsDialog::refresh()
{
    if (!m_core || !m_core->isConnected()) {
        setStatus(i18n("The core is not connected."));
        return;
    }
    QPointer<SkillsDialog> self(this);
    m_core->call(QStringLiteral("skills.listCatalog"), {},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->setStatus(
                             i18n("Could not list catalog: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->m_catalogDir =
                         result.value(QStringLiteral("catalogDir")).toString();
                     self->populateCatalog(
                         result.value(QStringLiteral("skills")).toArray());
                 });

    m_core->call(QStringLiteral("skills.listInstalled"),
                 QJsonObject{{QStringLiteral("target"), m_target}},
                 [self](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->setStatus(
                             i18n("Could not list installed skills: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->populateInstalled(
                         result.value(QStringLiteral("installed")).toArray());
                 });
}

void SkillsDialog::populateCatalog(const QJsonArray &items)
{
    m_catalogList->clear();
    if (items.isEmpty()) {
        auto *placeholder = new QListWidgetItem(
            i18n("No skills in the catalog yet.\nDrop SKILL.md directories into:\n%1",
                 m_catalogDir),
            m_catalogList);
        QFont f = placeholder->font();
        f.setItalic(true);
        placeholder->setFont(f);
        placeholder->setFlags(placeholder->flags() & ~Qt::ItemIsSelectable);
    }
    for (const QJsonValue &v : items) {
        const QJsonObject s = v.toObject();
        const QString name = s.value(QStringLiteral("name")).toString();
        const QString desc = s.value(QStringLiteral("description")).toString();
        QString label = name;
        if (!desc.isEmpty()) {
            label += QStringLiteral("\n    ") + desc;
        }
        auto *item = new QListWidgetItem(label, m_catalogList);
        item->setData(Qt::UserRole, name);
        item->setToolTip(s.value(QStringLiteral("path")).toString());
        if (m_installedNames.contains(name)) {
            QFont f = item->font();
            f.setBold(true);
            item->setFont(f);
        }
    }
    updateButtons();
}

void SkillsDialog::populateInstalled(const QJsonArray &items)
{
    m_installedList->clear();
    m_installedNames.clear();
    for (const QJsonValue &v : items) {
        const QJsonObject s = v.toObject();
        const QString name = s.value(QStringLiteral("name")).toString();
        const bool managed = s.value(QStringLiteral("inCatalog")).toBool();
        m_installedNames.insert(name);
        QString label = name;
        if (!managed) {
            label += i18n("    (not from catalog)");
        }
        auto *item = new QListWidgetItem(label, m_installedList);
        item->setData(Qt::UserRole, name);
        item->setData(Qt::UserRole + 1, managed);
        item->setToolTip(s.value(QStringLiteral("path")).toString());
        if (!managed) {
            item->setFlags(item->flags() & ~Qt::ItemIsSelectable);
        }
    }
    // Re-bold any catalog items that are now installed.
    for (int i = 0; i < m_catalogList->count(); ++i) {
        QListWidgetItem *it = m_catalogList->item(i);
        const QString name = it->data(Qt::UserRole).toString();
        if (name.isEmpty()) {
            continue;
        }
        QFont f = it->font();
        f.setBold(m_installedNames.contains(name));
        it->setFont(f);
    }
    updateButtons();
}

void SkillsDialog::install()
{
    QListWidgetItem *it = m_catalogList->currentItem();
    if (!it) {
        return;
    }
    const QString name = it->data(Qt::UserRole).toString();
    if (name.isEmpty()) {
        return;
    }
    QPointer<SkillsDialog> self(this);
    setStatus(i18n("Installing %1…", name));
    m_core->call(QStringLiteral("skills.install"),
                 QJsonObject{{QStringLiteral("name"), name},
                             {QStringLiteral("target"), m_target}},
                 [self, name](const QJsonObject &, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->setStatus(
                             i18n("Install failed: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->setStatus(i18n("Installed %1.", name));
                     self->refresh();
                 });
}

void SkillsDialog::uninstall()
{
    QListWidgetItem *it = m_installedList->currentItem();
    if (!it || !it->data(Qt::UserRole + 1).toBool()) {
        return;
    }
    const QString name = it->data(Qt::UserRole).toString();
    QPointer<SkillsDialog> self(this);
    setStatus(i18n("Removing %1…", name));
    m_core->call(QStringLiteral("skills.uninstall"),
                 QJsonObject{{QStringLiteral("name"), name},
                             {QStringLiteral("target"), m_target}},
                 [self, name](const QJsonObject &, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->setStatus(
                             i18n("Remove failed: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->setStatus(i18n("Removed %1.", name));
                     self->refresh();
                 });
}

void SkillsDialog::openCatalogDir()
{
    if (m_catalogDir.isEmpty()) {
        return;
    }
    QDir().mkpath(m_catalogDir);
    QDesktopServices::openUrl(QUrl::fromLocalFile(m_catalogDir));
}

void SkillsDialog::loadDetail()
{
    QListWidgetItem *it = m_catalogList->currentItem();
    const QString name = it ? it->data(Qt::UserRole).toString() : QString();
    if (name.isEmpty()) {
        m_detail->clear();
        m_detailName.clear();
        return;
    }
    if (name == m_detailName) {
        return;
    }
    m_detailName = name;
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    m_detail->setPlainText(i18n("Loading…"));
    QPointer<SkillsDialog> self(this);
    m_core->call(QStringLiteral("skills.read"),
                 QJsonObject{{QStringLiteral("name"), name}},
                 [self, name](const QJsonObject &result, const QJsonObject &error) {
                     if (!self || self->m_detailName != name) {
                         return; // selection moved on; ignore stale reply
                     }
                     if (!error.isEmpty()) {
                         self->m_detail->setPlainText(
                             i18n("Could not read skill: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     // Render the markdown source as plain text — it is shown
                     // verbatim so the user sees the real frontmatter and body.
                     self->m_detail->setPlainText(
                         result.value(QStringLiteral("content")).toString());
                 });
}

void SkillsDialog::createSkill()
{
    QDialog dlg(this);
    dlg.setWindowTitle(i18n("New Skill"));
    auto *form = new QFormLayout(&dlg);
    auto *nameEdit = new QLineEdit(&dlg);
    nameEdit->setPlaceholderText(i18n("e.g. format-go"));
    auto *descEdit = new QLineEdit(&dlg);
    descEdit->setPlaceholderText(i18n("One-line summary of what it does"));
    form->addRow(i18n("Name:"), nameEdit);
    form->addRow(i18n("Description:"), descEdit);
    auto *box = new QDialogButtonBox(
        QDialogButtonBox::Ok | QDialogButtonBox::Cancel, &dlg);
    form->addRow(box);
    connect(box, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    connect(box, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);

    if (dlg.exec() != QDialog::Accepted) {
        return;
    }
    const QString name = nameEdit->text().trimmed();
    const QString desc = descEdit->text().trimmed();
    if (name.isEmpty()) {
        setStatus(i18n("A skill name is required."));
        return;
    }
    if (!m_core || !m_core->isConnected()) {
        setStatus(i18n("The core is not connected."));
        return;
    }
    setStatus(i18n("Creating %1…", name));
    QPointer<SkillsDialog> self(this);
    m_core->call(QStringLiteral("skills.create"),
                 QJsonObject{{QStringLiteral("name"), name},
                             {QStringLiteral("description"), desc}},
                 [self, name](const QJsonObject &result, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->setStatus(
                             i18n("Could not create skill: %1",
                                  error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     self->setStatus(i18n("Created %1.", name));
                     const QString path =
                         result.value(QStringLiteral("path")).toString();
                     if (!path.isEmpty()) {
                         QDesktopServices::openUrl(QUrl::fromLocalFile(path));
                     }
                     self->refresh();
                 });
}

void SkillsDialog::updateButtons()
{
    QListWidgetItem *cat = m_catalogList->currentItem();
    const QString catName = cat ? cat->data(Qt::UserRole).toString() : QString();
    m_installButton->setEnabled(!catName.isEmpty());

    QListWidgetItem *inst = m_installedList->currentItem();
    const bool managed = inst && inst->data(Qt::UserRole + 1).toBool();
    m_uninstallButton->setEnabled(managed);
}

void SkillsDialog::setStatus(const QString &message)
{
    m_status->setText(message);
}
