#include "TerminalPanel.h"

#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KPluginFactory>
#include <KPluginMetaData>
#include <KSharedConfig>
#include <KParts/ReadOnlyPart>

#include <kde_terminal_interface.h>

#include <QByteArray>
#include <QDir>
#include <QFile>
#include <QFontMetrics>
#include <QIcon>
#include <QInputDialog>
#include <QLabel>
#include <QMenu>
#include <QStandardPaths>
#include <QTabBar>
#include <QTabWidget>
#include <QToolButton>
#include <QVBoxLayout>
#include <QVariant>

namespace {
// Each session widget carries the absolute project path it was started under,
// stashed as a dynamic property so we can show/hide on project switch without
// a side-table.
constexpr const char *kProjectProp = "ak.projectPath";
// The Konsole KPart QObject, kept on the container so features (rename, run,
// duplicate, persistence) can reach the TerminalInterface without a side-table.
constexpr const char *kPartProp = "ak.part";
// A user-supplied tab title; when present it suppresses cwd-driven auto-rename.
constexpr const char *kCustomTitleProp = "ak.customTitle";

constexpr const char *kCfgGroup = "Terminal";

// Returns the TerminalInterface for a session container, or nullptr.
TerminalInterface *interfaceOf(QWidget *container)
{
    if (!container) {
        return nullptr;
    }
    auto *part = qobject_cast<QObject *>(container->property(kPartProp).value<QObject *>());
    return part ? qobject_cast<TerminalInterface *>(part) : nullptr;
}

// Probe whether the Konsole KPart is installed.
bool konsoleAvailable()
{
    return KPluginMetaData(QStringLiteral("kf6/parts/konsolepart")).isValid();
}

// Apply the theme's chosen Konsole profile (colours + behaviour) to a session.
void applyProfile(TerminalInterface *iface)
{
    if (iface) {
        iface->setCurrentProfile(ThemeManager::instance()->effectiveTerminalProfile());
    }
}

// Copy Agent Kate's bundled Konsole color schemes + profiles into the user's
// Konsole data dir (~/.local/share/konsole) so the embedded terminal can wear a
// scheme that matches the app themes. Konsole only discovers profiles/schemes
// from that location, and there is no in-memory API to push raw colours to the
// KPart. Idempotent: only writes a file when missing or out of date, and only
// ever touches our own "AgentKate*"-named files — never the user's profiles.
// Runs before the first session is created, so the KPart's ProfileManager sees
// the files on its initial scan.
void installKonsoleAssets()
{
    const QString dest =
        QStandardPaths::writableLocation(QStandardPaths::GenericDataLocation)
        + QStringLiteral("/konsole");
    if (!QDir().mkpath(dest)) {
        return;
    }
    static const QStringList assets = {
        QStringLiteral("AgentKateMidnight.colorscheme"),
        QStringLiteral("AgentKateMidnight.profile"),
        QStringLiteral("AgentKateDaylight.colorscheme"),
        QStringLiteral("AgentKateDaylight.profile"),
    };
    for (const QString &name : assets) {
        QFile res(QStringLiteral(":/konsole/") + name);
        if (!res.open(QIODevice::ReadOnly)) {
            continue;
        }
        const QByteArray want = res.readAll();
        const QString path = dest + QLatin1Char('/') + name;
        QFile existing(path);
        if (existing.exists() && existing.open(QIODevice::ReadOnly)
            && existing.readAll() == want) {
            continue; // already up to date
        }
        existing.close();
        QFile out(path);
        if (out.open(QIODevice::WriteOnly | QIODevice::Truncate)) {
            out.write(want);
        }
    }
}
} // namespace

TerminalPanel::TerminalPanel(QWidget *parent)
    : QWidget(parent)
    , m_workdir(QDir::homePath())
    , m_konsoleMissing(!konsoleAvailable())
{
    // Make our signature Konsole schemes/profiles discoverable before any part
    // is created, so "Match interface" and the profile picker have them to use.
    if (!m_konsoleMissing) {
        installKonsoleAssets();
    }

    m_tabs = new QTabWidget(this);
    m_tabs->setTabsClosable(true);
    m_tabs->setMovable(true);
    m_tabs->setDocumentMode(true);
    connect(m_tabs, &QTabWidget::tabCloseRequested, this, &TerminalPanel::closeTab);

    // Double-click a tab to give it a sticky custom name.
    connect(m_tabs->tabBar(), &QTabBar::tabBarDoubleClicked, this,
            [this](int index) {
                if (index >= 0) {
                    renameTab(index);
                }
            });

    // Right-click a tab for the per-tab actions.
    m_tabs->tabBar()->setContextMenuPolicy(Qt::CustomContextMenu);
    connect(m_tabs->tabBar(), &QTabBar::customContextMenuRequested, this,
            &TerminalPanel::onTabContextMenu);

    // A "+" button in the tab-bar corner opens another terminal.
    auto *addButton = new QToolButton(this);
    addButton->setIcon(QIcon::fromTheme(QStringLiteral("list-add")));
    addButton->setAutoRaise(true);
    addButton->setToolTip(i18n("New Terminal"));
    addButton->setEnabled(!m_konsoleMissing);
    connect(addButton, &QToolButton::clicked, this, &TerminalPanel::newTerminal);
    m_tabs->setCornerWidget(addButton, Qt::TopRightCorner);
    m_addButton = addButton;

    // Re-profile every open session when the interface or terminal theme
    // changes, so a live theme switch recolours running shells immediately.
    connect(ThemeManager::instance(), &ThemeManager::changed, this, [this] {
        for (int i = 0; i < m_tabs->count(); ++i) {
            applyProfile(interfaceOf(m_tabs->widget(i)));
        }
    });

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_tabs);
    // First terminal is created lazily by setWorkingDirectory once the active
    // project is known — opening it eagerly here would anchor the session to
    // the home directory.
}

TerminalPanel::~TerminalPanel()
{
    // The base ~QWidget (running after this body) deletes m_tabs and, through
    // it, every container and its Konsole part. Each part's destroyed-handler
    // calls back into m_tabs, which by then is mid-destruction — a use-after-
    // free. Qt's automatic disconnect of this-context connections only happens
    // later in ~QObject, too late to help. Flag the teardown so the handlers
    // bail out instead.
    m_shuttingDown = true;
}

void TerminalPanel::setWorkingDirectory(const QString &dir)
{
    if (dir.isEmpty() || dir == m_workdir) {
        // Same project — but on the very first call we may still have no tabs
        // because the constructor deferred to us. Make sure one exists.
        if (dir == m_workdir && m_tabs->count() == 0 && !m_konsoleMissing) {
            maybeRestoreFor(m_workdir);
            if (m_tabs->count() == 0) {
                newTerminal();
            }
        }
        return;
    }
    m_workdir = dir;
    // Lazily materialise persisted tabs for a project the first time it becomes
    // active — never eagerly for inactive projects.
    maybeRestoreFor(m_workdir);
    applyVisibility();
    // Ensure the active project has at least one visible tab.
    bool anyVisible = false;
    for (int i = 0; i < m_tabs->count(); ++i) {
        if (m_tabs->isTabVisible(i)) {
            anyVisible = true;
            break;
        }
    }
    if (!anyVisible && !m_konsoleMissing) {
        newTerminal();
    }
}

void TerminalPanel::applyVisibility()
{
    int firstVisible = -1;
    for (int i = 0; i < m_tabs->count(); ++i) {
        QWidget *w = m_tabs->widget(i);
        const QString proj = w ? w->property(kProjectProp).toString() : QString();
        const bool visible = (proj == m_workdir);
        m_tabs->setTabVisible(i, visible);
        if (visible && firstVisible < 0) {
            firstVisible = i;
        }
    }
    if (firstVisible >= 0) {
        m_tabs->setCurrentIndex(firstVisible);
    }
}

void TerminalPanel::newTerminal()
{
    openTerminalAt(m_workdir);
}

void TerminalPanel::openTerminalAt(const QString &dir)
{
    if (m_konsoleMissing) {
        return;
    }
    const QString cwd = dir.isEmpty() ? m_workdir : dir;
    QWidget *session = createSession(cwd);
    if (!session) {
        return;
    }
    // Tab is still owned by the active project for show/hide scoping; only the
    // initial shell CWD differs from the project root.
    session->setProperty(kProjectProp, m_workdir);
    const QString title = QDir(cwd).dirName();
    const int idx = m_tabs->addTab(session, title.isEmpty() ? i18n("Terminal") : title);
    m_tabs->setTabToolTip(idx, cwd);
    // Re-apply visibility so a freshly created tab in a non-active project
    // (shouldn't happen, but defensive) doesn't accidentally show up.
    applyVisibility();
    if (m_tabs->isTabVisible(idx)) {
        m_tabs->setCurrentIndex(idx);
        if (QWidget *fw = session->focusWidget()) {
            fw->setFocus();
        }
    }
}

void TerminalPanel::runCommandAt(const QString &dir, const QString &command)
{
    if (m_konsoleMissing || command.isEmpty()) {
        return;
    }
    openTerminalAt(dir);
    if (TerminalInterface *iface = interfaceOf(m_tabs->currentWidget())) {
        iface->sendInput(command + QLatin1Char('\n'));
    }
}

void TerminalPanel::focusActiveTerminal()
{
    if (QWidget *w = m_tabs->currentWidget()) {
        if (QWidget *fw = w->focusWidget()) {
            fw->setFocus();
        }
    }
}

void TerminalPanel::nextTerminal()
{
    const int n = m_tabs->count();
    if (n == 0) {
        return;
    }
    for (int step = 1; step <= n; ++step) {
        const int idx = (m_tabs->currentIndex() + step) % n;
        if (m_tabs->isTabVisible(idx)) {
            m_tabs->setCurrentIndex(idx);
            focusActiveTerminal();
            return;
        }
    }
}

void TerminalPanel::previousTerminal()
{
    const int n = m_tabs->count();
    if (n == 0) {
        return;
    }
    for (int step = 1; step <= n; ++step) {
        const int idx = (m_tabs->currentIndex() - step + n) % n;
        if (m_tabs->isTabVisible(idx)) {
            m_tabs->setCurrentIndex(idx);
            focusActiveTerminal();
            return;
        }
    }
}

void TerminalPanel::closeTab(int index)
{
    QWidget *w = m_tabs->widget(index);
    m_tabs->removeTab(index);
    if (w) {
        w->deleteLater(); // destroys the container -> the KPart -> the shell
    }
}

void TerminalPanel::closeProject(const QString &project)
{
    // ONLY reached on explicit project CLOSE — destroy that project's tabs.
    // Iterate backwards so removeTab() index shifts don't skip entries.
    for (int i = m_tabs->count() - 1; i >= 0; --i) {
        QWidget *w = m_tabs->widget(i);
        if (w && w->property(kProjectProp).toString() == project) {
            closeTab(i);
        }
    }
}

void TerminalPanel::renameTab(int index)
{
    if (index < 0) {
        return;
    }
    bool ok = false;
    const QString name = QInputDialog::getText(
        this, i18n("Rename Terminal"), i18n("Tab name:"), QLineEdit::Normal,
        m_tabs->tabText(index), &ok);
    if (!ok) {
        return;
    }
    if (name.isEmpty()) {
        // Clear the custom title — auto-rename from cwd resumes.
        if (QWidget *w = m_tabs->widget(index)) {
            w->setProperty(kCustomTitleProp, QVariant());
        }
        return;
    }
    m_tabs->setTabText(index, name);
    if (QWidget *w = m_tabs->widget(index)) {
        w->setProperty(kCustomTitleProp, name);
    }
}

void TerminalPanel::onTabContextMenu(const QPoint &pos)
{
    const int index = m_tabs->tabBar()->tabAt(pos);

    QMenu menu(this);
    QAction *newAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("list-add")), i18n("New Terminal"));
    newAct->setEnabled(!m_konsoleMissing);
    connect(newAct, &QAction::triggered, this, &TerminalPanel::newTerminal);

    if (index >= 0) {
        QAction *renameAct = menu.addAction(i18n("Rename Tab…"));
        connect(renameAct, &QAction::triggered, this,
                [this, index] { renameTab(index); });

        QAction *dupAct = menu.addAction(
            QIcon::fromTheme(QStringLiteral("tab-duplicate")), i18n("Duplicate"));
        dupAct->setEnabled(!m_konsoleMissing);
        connect(dupAct, &QAction::triggered, this, [this, index] {
            TerminalInterface *iface = interfaceOf(m_tabs->widget(index));
            const QString cwd =
                iface ? iface->currentWorkingDirectory() : m_workdir;
            openTerminalAt(cwd);
        });

        menu.addSeparator();
        QAction *closeAct = menu.addAction(
            QIcon::fromTheme(QStringLiteral("tab-close")), i18n("Close Tab"));
        connect(closeAct, &QAction::triggered, this,
                [this, index] { closeTab(index); });

        QAction *closeOthersAct = menu.addAction(i18n("Close Other Tabs"));
        connect(closeOthersAct, &QAction::triggered, this, [this, index] {
            QWidget *keep = m_tabs->widget(index);
            // Only close tabs of the active project, leaving other projects'
            // background sessions untouched.
            for (int i = m_tabs->count() - 1; i >= 0; --i) {
                QWidget *w = m_tabs->widget(i);
                if (w != keep && w
                    && w->property(kProjectProp).toString() == m_workdir) {
                    closeTab(i);
                }
            }
        });
    }
    menu.exec(m_tabs->tabBar()->mapToGlobal(pos));
}

void TerminalPanel::onPartDirectoryChanged(const QString &dir)
{
    QObject *part = sender();
    if (dir.isEmpty() || !part) {
        return;
    }
    // Find the container hosting this part.
    for (int i = 0; i < m_tabs->count(); ++i) {
        QWidget *w = m_tabs->widget(i);
        if (!w || w->property(kPartProp).value<QObject *>() != part) {
            continue;
        }
        m_tabs->setTabToolTip(i, dir);
        // A sticky custom title suppresses cwd-driven auto-rename.
        if (!w->property(kCustomTitleProp).toString().isEmpty()) {
            return;
        }
        const QString name = QDir(dir).dirName();
        const QString label = name.isEmpty() ? i18n("Terminal") : name;
        const QFontMetrics fm(m_tabs->tabBar()->font());
        m_tabs->setTabText(
            i, fm.elidedText(label, Qt::ElideMiddle, 180));
        return;
    }
}

// createSession builds one tab: a container hosting a fresh Konsole KPart, or a
// message label if the terminal could not be loaded.
QWidget *TerminalPanel::createSession(const QString &cwd)
{
    auto *container = new QWidget(m_tabs);
    auto *layout = new QVBoxLayout(container);
    layout->setContentsMargins(0, 0, 0, 0);

    const auto factoryResult =
        KPluginFactory::loadFactory(KPluginMetaData(QStringLiteral("kf6/parts/konsolepart")));
    if (!factoryResult) {
        m_konsoleMissing = true;
        if (m_addButton) {
            m_addButton->setEnabled(false);
        }
        auto *msg = new QLabel(
            i18n("The Konsole terminal could not be loaded.\n"
                 "Install the 'konsole' package to use the integrated terminal."),
            container);
        msg->setAlignment(Qt::AlignCenter);
        msg->setWordWrap(true);
        layout->addWidget(msg);
        return container;
    }

    KParts::ReadOnlyPart *part =
        factoryResult.plugin->create<KParts::ReadOnlyPart>(container, container);
    if (!part || !part->widget()) {
        auto *msg = new QLabel(i18n("Could not start a terminal session."), container);
        msg->setAlignment(Qt::AlignCenter);
        layout->addWidget(msg);
        return container;
    }
    layout->addWidget(part->widget());

    // Stash the part so rename / run / duplicate / persistence can reach it.
    container->setProperty(kPartProp, QVariant::fromValue<QObject *>(part));

    // Live tab title: rename the tab as the shell changes directory. The
    // konsolepart emits currentDirectoryChanged(QString) at runtime, but it is
    // NOT declared on the KParts::ReadOnlyPart base class — so connect by
    // signature (string-based) and route through a dedicated slot, which lets
    // the SIGNAL/SLOT macros resolve the dynamic connection.
    connect(part, SIGNAL(currentDirectoryChanged(QString)), this,
            SLOT(onPartDirectoryChanged(QString)));

    // Graceful exit: when the shell exits, the part is destroyed — close the
    // (now dead) tab so no blank terminal lingers.
    connect(part, &QObject::destroyed, this, [this, container] {
        if (m_shuttingDown) {
            return; // m_tabs is being torn down with us — don't touch it.
        }
        const int idx = m_tabs->indexOf(container);
        if (idx >= 0) {
            m_tabs->removeTab(idx);
            container->deleteLater();
        }
    });

    if (auto *terminal = qobject_cast<TerminalInterface *>(part)) {
        terminal->showShellInDir(cwd);
        // Dress the session in the theme's Konsole profile from the start.
        applyProfile(terminal);
    }
    return container;
}

void TerminalPanel::saveSession()
{
    KConfigGroup grp = KSharedConfig::openConfig()->group(QString::fromLatin1(kCfgGroup));
    grp.deleteGroup();
    grp = KSharedConfig::openConfig()->group(QString::fromLatin1(kCfgGroup));

    int n = 0;
    for (int i = 0; i < m_tabs->count(); ++i) {
        QWidget *w = m_tabs->widget(i);
        if (!w) {
            continue;
        }
        const QString project = w->property(kProjectProp).toString();
        if (project.isEmpty()) {
            continue;
        }
        TerminalInterface *iface = interfaceOf(w);
        QString cwd = iface ? iface->currentWorkingDirectory() : QString();
        if (cwd.isEmpty()) {
            cwd = project;
        }
        KConfigGroup tab = grp.group(QString::number(n++));
        tab.writeEntry("project", project);
        tab.writeEntry("cwd", cwd);
        tab.writeEntry("title", w->property(kCustomTitleProp).toString());
    }
    grp.writeEntry("count", n);
    grp.sync();
}

void TerminalPanel::maybeRestoreFor(const QString &project)
{
    if (m_konsoleMissing || project.isEmpty()) {
        return;
    }
    // Skip if any live tab already belongs to this project.
    for (int i = 0; i < m_tabs->count(); ++i) {
        QWidget *w = m_tabs->widget(i);
        if (w && w->property(kProjectProp).toString() == project) {
            return;
        }
    }

    KConfigGroup grp = KSharedConfig::openConfig()->group(QString::fromLatin1(kCfgGroup));
    const int count = grp.readEntry("count", 0);
    if (count <= 0) {
        return;
    }
    for (int i = 0; i < count; ++i) {
        KConfigGroup tab = grp.group(QString::number(i));
        if (tab.readEntry("project", QString()) != project) {
            continue;
        }
        const QString cwd = tab.readEntry("cwd", project);
        QWidget *session = createSession(cwd);
        if (!session) {
            continue;
        }
        session->setProperty(kProjectProp, project);
        const QString custom = tab.readEntry("title", QString());
        QString label = custom;
        if (label.isEmpty()) {
            label = QDir(cwd).dirName();
        }
        const int idx =
            m_tabs->addTab(session, label.isEmpty() ? i18n("Terminal") : label);
        m_tabs->setTabToolTip(idx, cwd);
        if (!custom.isEmpty()) {
            session->setProperty(kCustomTitleProp, custom);
        }
    }
}
