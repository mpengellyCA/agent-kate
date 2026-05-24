#include "SkillsDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDesktopServices>
#include <QDialogButtonBox>
#include <QDir>
#include <QFont>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QListWidget>
#include <QPointer>
#include <QPushButton>
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

    auto *columns = new QHBoxLayout;

    auto *catalogCol = new QVBoxLayout;
    catalogCol->addWidget(new QLabel(i18n("Available"), this));
    m_catalogList = new QListWidget(this);
    catalogCol->addWidget(m_catalogList, 1);
    columns->addLayout(catalogCol, 1);

    auto *middle = new QVBoxLayout;
    middle->addStretch(1);
    m_installButton = new QPushButton(i18n("Install →"), this);
    m_uninstallButton = new QPushButton(i18n("← Remove"), this);
    middle->addWidget(m_installButton);
    middle->addWidget(m_uninstallButton);
    middle->addStretch(1);
    columns->addLayout(middle);

    auto *installedCol = new QVBoxLayout;
    installedCol->addWidget(new QLabel(i18n("Installed in this project"), this));
    m_installedList = new QListWidget(this);
    installedCol->addWidget(m_installedList, 1);
    columns->addLayout(installedCol, 1);

    layout->addLayout(columns, 1);

    m_status = new QLabel(this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    auto *bottomRow = new QHBoxLayout;
    m_openCatalogButton = new QPushButton(i18n("Open Catalog Folder…"), this);
    bottomRow->addWidget(m_openCatalogButton);
    bottomRow->addStretch(1);
    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    bottomRow->addWidget(buttons);
    layout->addLayout(bottomRow);

    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::accept);
    connect(m_installButton, &QPushButton::clicked, this, &SkillsDialog::install);
    connect(m_uninstallButton, &QPushButton::clicked, this, &SkillsDialog::uninstall);
    connect(m_openCatalogButton, &QPushButton::clicked, this, &SkillsDialog::openCatalogDir);
    connect(m_catalogList, &QListWidget::itemSelectionChanged, this, &SkillsDialog::updateButtons);
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
