#include "WelcomeDialog.h"
#include "RecentProjects.h"

#include <KLocalizedString>

#include <QDialogButtonBox>
#include <QDir>
#include <QFileDialog>
#include <QFileInfo>
#include <QFrame>
#include <QHBoxLayout>
#include <QIcon>
#include <QInputDialog>
#include <QKeySequence>
#include <QLabel>
#include <QListWidget>
#include <QListWidgetItem>
#include <QMessageBox>
#include <QPushButton>
#include <QShortcut>
#include <QStandardPaths>
#include <QVBoxLayout>

namespace {
constexpr int kPathRole = Qt::UserRole + 1;
} // namespace

WelcomeDialog::WelcomeDialog(QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18n("Welcome to Agent Kate"));
    setWindowIcon(QIcon::fromTheme(QStringLiteral("agentkate")));
    setMinimumSize(640, 460);

    auto *outer = new QVBoxLayout(this);
    outer->setContentsMargins(24, 24, 24, 18);
    outer->setSpacing(14);

    auto *header = new QLabel(i18n("<h2 style='margin:0;'>Agent Kate</h2>"
                                   "<p style='margin:2px 0 0 0; color:palette(mid);'>"
                                   "Native multi-agent coding arena</p>"),
                              this);
    header->setTextFormat(Qt::RichText);
    outer->addWidget(header);

    // "Reopen last project" hero row — the single most likely action.
    auto *heroFrame = new QFrame(this);
    heroFrame->setFrameShape(QFrame::StyledPanel);
    auto *heroRow = new QHBoxLayout(heroFrame);
    heroRow->setContentsMargins(12, 10, 12, 10);
    heroRow->setSpacing(10);

    auto *heroLabel = new QLabel(this);
    heroLabel->setTextFormat(Qt::RichText);
    heroLabel->setText(i18n("<b>Continue where you left off</b>"));
    m_lastLabel = new QLabel(this);
    m_lastLabel->setStyleSheet(QStringLiteral("color: palette(mid);"));
    m_lastLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);

    auto *heroText = new QVBoxLayout;
    heroText->setSpacing(2);
    heroText->addWidget(heroLabel);
    heroText->addWidget(m_lastLabel);

    m_reopenButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("document-open-recent")),
        i18n("Reopen"), this);
    m_reopenButton->setDefault(true);
    m_reopenButton->setAutoDefault(true);

    heroRow->addLayout(heroText, 1);
    heroRow->addWidget(m_reopenButton, 0, Qt::AlignVCenter);
    outer->addWidget(heroFrame);

    // Recent projects list — double-click or Enter to open.
    auto *recentLabel = new QLabel(i18n("Recent projects"), this);
    outer->addWidget(recentLabel);

    m_list = new QListWidget(this);
    m_list->setAlternatingRowColors(true);
    m_list->setSelectionMode(QAbstractItemView::SingleSelection);
    m_list->setUniformItemSizes(true);
    outer->addWidget(m_list, 1);

    auto *hint = new QLabel(
        i18n("<small style='color:palette(mid);'>"
             "Double-click to open · Delete to remove from the list</small>"),
        this);
    hint->setTextFormat(Qt::RichText);
    outer->addWidget(hint);

    // Bottom action bar — "Open folder…", "New project…", and Cancel/Quit.
    auto *buttons = new QDialogButtonBox(this);
    auto *openButton = buttons->addButton(
        i18n("Open Folder…"), QDialogButtonBox::ActionRole);
    openButton->setIcon(QIcon::fromTheme(QStringLiteral("folder-open")));
    auto *newButton = buttons->addButton(
        i18n("New Project…"), QDialogButtonBox::ActionRole);
    newButton->setIcon(QIcon::fromTheme(QStringLiteral("folder-new")));
    auto *quitButton = buttons->addButton(QDialogButtonBox::Cancel);
    quitButton->setText(i18n("Quit"));
    quitButton->setIcon(QIcon::fromTheme(QStringLiteral("application-exit")));
    outer->addWidget(buttons);

    connect(m_reopenButton, &QPushButton::clicked, this, &WelcomeDialog::reopenLast);
    connect(openButton, &QPushButton::clicked, this, &WelcomeDialog::chooseFolder);
    connect(newButton, &QPushButton::clicked, this, &WelcomeDialog::createNewProject);
    connect(quitButton, &QPushButton::clicked, this, &QDialog::reject);
    connect(m_list, &QListWidget::itemActivated,
            this, &WelcomeDialog::onItemActivated);

    auto *del = new QShortcut(QKeySequence(QKeySequence::Delete), m_list);
    del->setContext(Qt::WidgetShortcut);
    connect(del, &QShortcut::activated, this, &WelcomeDialog::onRemoveCurrent);

    refreshList();
}

void WelcomeDialog::refreshList()
{
    const QStringList recents = RecentProjects::load();
    m_list->clear();

    const QString last = recents.isEmpty() ? QString() : recents.constFirst();
    if (last.isEmpty()) {
        m_lastLabel->setText(
            i18n("No project has been opened yet — pick a folder below."));
        m_reopenButton->setEnabled(false);
    } else {
        const QString name = QDir(last).dirName();
        m_lastLabel->setText(QStringLiteral("%1 — %2").arg(
            name.isEmpty() ? last : name, last));
        m_reopenButton->setEnabled(true);
    }

    const QIcon folder = QIcon::fromTheme(QStringLiteral("folder"));
    const QIcon missing = QIcon::fromTheme(QStringLiteral("folder-remote"));
    for (const QString &path : recents) {
        const QFileInfo info(path);
        const QString name = info.fileName().isEmpty() ? path : info.fileName();
        auto *item = new QListWidgetItem(m_list);
        item->setText(QStringLiteral("%1\n%2").arg(name, path));
        item->setData(kPathRole, path);
        const bool exists = info.exists() && info.isDir();
        item->setIcon(exists ? folder : missing);
        if (!exists) {
            item->setToolTip(i18n("This folder no longer exists."));
        }
    }
    if (m_list->count() > 0) {
        m_list->setCurrentRow(0);
    }
}

void WelcomeDialog::reopenLast()
{
    const QString last = RecentProjects::last();
    if (last.isEmpty()) {
        return;
    }
    accept(last);
}

void WelcomeDialog::chooseFolder()
{
    const QString start = RecentProjects::last();
    const QString dir = QFileDialog::getExistingDirectory(
        this, i18n("Open Project Folder"),
        start.isEmpty() ? QDir::homePath() : start);
    if (!dir.isEmpty()) {
        accept(dir);
    }
}

void WelcomeDialog::createNewProject()
{
    const QString defaultParent =
        QStandardPaths::writableLocation(QStandardPaths::HomeLocation);
    const QString parent = QFileDialog::getExistingDirectory(
        this, i18n("Choose Parent Folder for New Project"), defaultParent);
    if (parent.isEmpty()) {
        return;
    }
    bool ok = false;
    const QString name = QInputDialog::getText(
        this, i18n("New Project"), i18n("Project folder name:"),
        QLineEdit::Normal, QString(), &ok);
    if (!ok || name.trimmed().isEmpty()) {
        return;
    }
    QDir parentDir(parent);
    const QString trimmed = name.trimmed();
    if (parentDir.exists(trimmed)) {
        if (QMessageBox::question(
                this, i18n("Folder Exists"),
                i18n("A folder named \"%1\" already exists here. Open it as the "
                     "new project?", trimmed))
            != QMessageBox::Yes) {
            return;
        }
    } else if (!parentDir.mkpath(trimmed)) {
        QMessageBox::warning(this, i18n("Could Not Create Folder"),
                             i18n("Failed to create \"%1\" inside %2.",
                                  trimmed, parent));
        return;
    }
    accept(parentDir.absoluteFilePath(trimmed));
}

void WelcomeDialog::onItemActivated(QListWidgetItem *item)
{
    if (!item) {
        return;
    }
    const QString path = item->data(kPathRole).toString();
    if (path.isEmpty()) {
        return;
    }
    const QFileInfo info(path);
    if (!info.exists() || !info.isDir()) {
        if (QMessageBox::question(
                this, i18n("Folder Missing"),
                i18n("\"%1\" no longer exists. Remove it from the recent list?",
                     path))
            == QMessageBox::Yes) {
            RecentProjects::forget(path);
            refreshList();
        }
        return;
    }
    accept(path);
}

void WelcomeDialog::onRemoveCurrent()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item) {
        return;
    }
    const QString path = item->data(kPathRole).toString();
    RecentProjects::forget(path);
    refreshList();
}

void WelcomeDialog::accept(const QString &path)
{
    m_selected = path;
    QDialog::accept();
}
