#include "WelcomeDialog.h"
#include "RecentProjects.h"
#include "state/SessionProjects.h"

#include <KIO/OpenFileManagerWindowJob>
#include <KLocalizedString>

#include <QAction>
#include <QApplication>
#include <QClipboard>
#include <QDateTime>
#include <QDialogButtonBox>
#include <QDir>
#include <QFile>
#include <QFileDialog>
#include <QFileInfo>
#include <QFrame>
#include <QHBoxLayout>
#include <QIcon>
#include <QInputDialog>
#include <QKeySequence>
#include <QLabel>
#include <QMenu>
#include <QPixmap>
#include <QListWidget>
#include <QListWidgetItem>
#include <QMessageBox>
#include <QPushButton>
#include <QShortcut>
#include <QStandardPaths>
#include <QUrl>
#include <QVBoxLayout>

namespace {
constexpr int kPathRole = Qt::UserRole + 1;
constexpr int kPinnedRole = Qt::UserRole + 2;

// Cheap, dependency-free branch hint: read <gitdir>/HEAD and parse the
// "ref: refs/heads/<branch>" line. Handles plain repos (.git is a directory)
// and worktrees/submodules (.git is a FILE holding "gitdir: <path>"). Returns
// an empty string for a detached HEAD, an unreadable file, or a non-git folder.
QString branchHint(const QString &path)
{
    const QString dotGit = QDir(path).filePath(QStringLiteral(".git"));
    const QFileInfo dotGitInfo(dotGit);
    QString gitDir = dotGit;
    if (dotGitInfo.isFile()) {
        // Worktree / submodule: ".git" is a pointer file.
        QFile ptr(dotGit);
        if (!ptr.open(QIODevice::ReadOnly | QIODevice::Text)) {
            return QString();
        }
        const QString line = QString::fromUtf8(ptr.readLine()).trimmed();
        const QString prefix = QStringLiteral("gitdir: ");
        if (!line.startsWith(prefix)) {
            return QString();
        }
        const QString target = line.mid(prefix.length());
        gitDir = QDir::isAbsolutePath(target) ? target
                                              : QDir(path).filePath(target);
    } else if (!dotGitInfo.isDir()) {
        return QString();
    }

    QFile head(QDir(gitDir).filePath(QStringLiteral("HEAD")));
    if (!head.open(QIODevice::ReadOnly | QIODevice::Text)) {
        return QString();
    }
    const QString line = QString::fromUtf8(head.readLine()).trimmed();
    const QString prefix = QStringLiteral("ref: refs/heads/");
    if (line.startsWith(prefix)) {
        return line.mid(prefix.length());
    }
    return QString(); // detached HEAD or unexpected format
}

// A short, human relative time for the last-opened hint.
QString relativeOpened(const QDateTime &when)
{
    if (!when.isValid()) {
        return QString();
    }
    const qint64 secs = when.toUTC().secsTo(QDateTime::currentDateTimeUtc());
    if (secs < 60) {
        return i18n("just now");
    }
    if (secs < 3600) {
        return i18np("%1 minute ago", "%1 minutes ago", int(secs / 60));
    }
    if (secs < 86400) {
        return i18np("%1 hour ago", "%1 hours ago", int(secs / 3600));
    }
    return i18np("%1 day ago", "%1 days ago", int(secs / 86400));
}
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

    auto *headerRow = new QHBoxLayout;
    headerRow->setContentsMargins(0, 0, 0, 0);
    headerRow->setSpacing(14);

    auto *logo = new QLabel(this);
    const QPixmap logoPix(QStringLiteral(":/branding/logo.png"));
    const int logoSize = 72;
    logo->setPixmap(logoPix.scaled(logoSize, logoSize,
                                   Qt::KeepAspectRatio,
                                   Qt::SmoothTransformation));
    logo->setFixedSize(logoSize, logoSize);
    headerRow->addWidget(logo, 0, Qt::AlignTop);

    auto *headerText = new QVBoxLayout;
    headerText->setSpacing(0);
    auto *title = new QLabel(i18n("<h2 style='margin:0;'>Agent Kate</h2>"), this);
    title->setTextFormat(Qt::RichText);
    auto *subtitle = new QLabel(i18n("Work alongside AI agents on your projects"), this);
    subtitle->setForegroundRole(QPalette::PlaceholderText);
    headerText->addWidget(title);
    headerText->addWidget(subtitle);
    headerRow->addLayout(headerText, 1);
    headerRow->setAlignment(headerText, Qt::AlignVCenter);
    outer->addLayout(headerRow);

    // A welcoming, jargon-free orientation line for newcomers.
    auto *intro = new QLabel(
        i18n("Pick a project below to begin. You can switch between Simple and "
             "Advanced views any time, and give Agent Kate its own look in "
             "Options ▸ Appearance."),
        this);
    intro->setWordWrap(true);
    intro->setForegroundRole(QPalette::PlaceholderText);
    outer->addWidget(intro);

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
    m_lastLabel->setForegroundRole(QPalette::PlaceholderText);
    m_lastLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);

    auto *heroText = new QVBoxLayout;
    heroText->setSpacing(2);
    heroText->addWidget(heroLabel);
    heroText->addWidget(m_lastLabel);

    m_reopenButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("document-open-recent")),
        i18n("Reopen"), this);
    m_reopenButton->setObjectName(QStringLiteral("reopenButton"));
    m_reopenButton->setDefault(true);
    m_reopenButton->setAutoDefault(true);

    // Reopening the whole SET is the multi-project user's real intent, and
    // until now they had to re-add every folder by hand on every launch (audit
    // F47). Only offered when the last session held more than one project —
    // with one, "Reopen" already is the whole session.
    m_sessionButton = new QPushButton(
        QIcon::fromTheme(QStringLiteral("view-restore")), QString(), this);
    m_sessionButton->setObjectName(QStringLiteral("reopenSessionButton"));
    m_sessionButton->setAutoDefault(true);
    m_sessionButton->setVisible(false);

    heroRow->addLayout(heroText, 1);
    heroRow->addWidget(m_sessionButton, 0, Qt::AlignVCenter);
    heroRow->addWidget(m_reopenButton, 0, Qt::AlignVCenter);
    outer->addWidget(heroFrame);

    // Recent projects list — double-click or Enter to open.
    auto *recentLabel = new QLabel(i18n("Recent projects"), this);
    outer->addWidget(recentLabel);

    m_list = new QListWidget(this);
    m_list->setAlternatingRowColors(true);
    m_list->setSelectionMode(QAbstractItemView::SingleSelection);
    m_list->setUniformItemSizes(false); // pinned header rows differ in height
    m_list->setContextMenuPolicy(Qt::CustomContextMenu);
    outer->addWidget(m_list, 1);

    // Every other list in the app says what fills it when it is empty; this one
    // used to be a blank box on the very first screen a new user sees (F50).
    m_emptyHint = new QLabel(
        i18n("Nothing here yet — projects you open will be listed here so you "
             "can come straight back to them."),
        this);
    m_emptyHint->setWordWrap(true);
    m_emptyHint->setAlignment(Qt::AlignCenter);
    m_emptyHint->setForegroundRole(QPalette::PlaceholderText);
    m_emptyHint->setVisible(false);
    outer->addWidget(m_emptyHint, 1);

    m_listHint = new QLabel(
        i18n("Double-click to open · Right-click for more · Delete to remove"),
        this);
    m_listHint->setForegroundRole(QPalette::PlaceholderText);
    outer->addWidget(m_listHint);

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
    connect(m_sessionButton, &QPushButton::clicked, this, &WelcomeDialog::reopenSession);
    connect(openButton, &QPushButton::clicked, this, &WelcomeDialog::chooseFolder);
    connect(newButton, &QPushButton::clicked, this, &WelcomeDialog::createNewProject);
    connect(quitButton, &QPushButton::clicked, this, &QDialog::reject);
    connect(m_list, &QListWidget::itemActivated,
            this, &WelcomeDialog::onItemActivated);
    connect(m_list, &QListWidget::customContextMenuRequested,
            this, &WelcomeDialog::onContextMenu);

    auto *del = new QShortcut(QKeySequence(QKeySequence::Delete), m_list);
    del->setContext(Qt::WidgetShortcut);
    connect(del, &QShortcut::activated, this, &WelcomeDialog::onRemoveCurrent);

    refreshList();
}

void WelcomeDialog::addRow(const QString &path, bool pinned)
{
    const QFileInfo info(path);
    const QString name = info.fileName().isEmpty() ? path : info.fileName();

    QStringList tail{path};
    const QString branch = branchHint(path);
    if (!branch.isEmpty()) {
        tail.prepend(QStringLiteral("⌥ %1").arg(branch));
    }
    const QString opened = relativeOpened(RecentProjects::lastOpened(path));
    if (!opened.isEmpty()) {
        tail.prepend(opened);
    }

    auto *item = new QListWidgetItem(m_list);
    item->setText(QStringLiteral("%1\n%2").arg(name, tail.join(QStringLiteral(" · "))));
    item->setData(kPathRole, path);
    item->setData(kPinnedRole, pinned);

    const bool exists = info.exists() && info.isDir();
    if (pinned) {
        item->setIcon(QIcon::fromTheme(QStringLiteral("emblem-favorite")));
    } else {
        item->setIcon(QIcon::fromTheme(
            exists ? QStringLiteral("folder") : QStringLiteral("folder-remote")));
    }
    if (!exists) {
        item->setToolTip(i18n("This folder no longer exists."));
    } else {
        item->setToolTip(path);
    }
}

void WelcomeDialog::refreshList()
{
    const QStringList recents = RecentProjects::load();
    const QStringList pins = RecentProjects::pinned();
    m_list->clear();

    // The previous session's whole project set (existing folders only), which
    // may be larger than "the most recent one" (audit F47).
    const QStringList session = SessionProjects::load();
    if (session.size() > 1) {
        m_sessionButton->setText(i18np("Reopen Session (%1 Project)",
                                       "Reopen Session (%1 Projects)",
                                       session.size()));
        m_sessionButton->setToolTip(session.join(QLatin1Char('\n')));
        m_sessionButton->setVisible(true);
        // The set is the better guess than its newest member, so it takes the
        // Enter key. Both are safe, non-destructive actions.
        m_sessionButton->setDefault(true);
        m_reopenButton->setDefault(false);
    } else {
        m_sessionButton->setVisible(false);
        m_sessionButton->setDefault(false);
        m_reopenButton->setDefault(true);
    }

    const QString last = recents.isEmpty() ? QString() : recents.constFirst();
    if (last.isEmpty()) {
        m_lastLabel->setText(
            i18n("No project has been opened yet — pick a folder below."));
        m_reopenButton->setEnabled(false);
    } else {
        const QString name = QDir(last).dirName();
        const QString opened = relativeOpened(RecentProjects::lastOpened(last));
        const QString display = name.isEmpty() ? last : name;
        m_lastLabel->setText(opened.isEmpty()
            ? QStringLiteral("%1 — %2").arg(display, last)
            : i18n("%1 — %2 · %3", display, last, opened));
        m_reopenButton->setEnabled(true);
    }

    // Pinned favourites first, then the remaining recents (skipping any that
    // are already shown as pins).
    for (const QString &path : pins) {
        addRow(path, true);
    }
    for (const QString &path : recents) {
        if (pins.contains(path)) {
            continue;
        }
        addRow(path, false);
    }
    if (m_list->count() > 0) {
        m_list->setCurrentRow(0);
    }
    const bool empty = m_list->count() == 0;
    m_list->setVisible(!empty);
    if (m_emptyHint) {
        m_emptyHint->setVisible(empty);
    }
    if (m_listHint) {
        m_listHint->setVisible(!empty); // no list, nothing to say about using it
    }
}

void WelcomeDialog::onContextMenu(const QPoint &pos)
{
    QListWidgetItem *item = m_list->itemAt(pos);
    if (!item) {
        return;
    }
    const QString path = item->data(kPathRole).toString();
    const bool pinned = item->data(kPinnedRole).toBool();

    QMenu menu(this);
    QAction *open = menu.addAction(
        QIcon::fromTheme(QStringLiteral("document-open")), i18n("Open"));
    connect(open, &QAction::triggered, this, [this, item] { onItemActivated(item); });

    QAction *reveal = menu.addAction(
        QIcon::fromTheme(QStringLiteral("system-file-manager")),
        i18n("Open Containing Folder"));
    connect(reveal, &QAction::triggered, this,
            [path] { KIO::highlightInFileManager({QUrl::fromLocalFile(path)}); });

    QAction *copy = menu.addAction(
        QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy Path"));
    connect(copy, &QAction::triggered, this,
            [path] { QApplication::clipboard()->setText(path); });

    menu.addSeparator();
    QAction *pinAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("emblem-favorite")),
        pinned ? i18n("Unpin") : i18n("Pin"));
    connect(pinAct, &QAction::triggered, this, [this, path, pinned] {
        if (pinned) {
            RecentProjects::unpin(path);
        } else {
            RecentProjects::pin(path);
        }
        refreshList();
    });

    QAction *remove = menu.addAction(
        QIcon::fromTheme(QStringLiteral("list-remove")),
        i18n("Remove from List"));
    connect(remove, &QAction::triggered, this, [this, path] {
        RecentProjects::forget(path);
        refreshList();
    });

    menu.exec(m_list->viewport()->mapToGlobal(pos));
}

void WelcomeDialog::reopenLast()
{
    const QString last = RecentProjects::last();
    if (last.isEmpty()) {
        return;
    }
    accept(last);
}

void WelcomeDialog::reopenSession()
{
    const QStringList session = SessionProjects::load();
    if (session.isEmpty()) {
        return;
    }
    acceptMany(session);
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
    acceptMany({path});
}

void WelcomeDialog::acceptMany(const QStringList &paths)
{
    if (paths.isEmpty()) {
        return;
    }
    m_selectedPaths = paths;
    m_selected = paths.constFirst();
    QDialog::accept();
}
