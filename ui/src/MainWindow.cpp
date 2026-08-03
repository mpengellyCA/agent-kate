#include "MainWindow.h"
#include "AgentActions.h"
#include "AgentDock.h"
#include "AgentPanel.h"
#include "AppearanceDialog.h"
#include "AttachmentBuilder.h"
#include "CommandPalette.h"
#include "EditorArea.h"
#include "EditorSession.h"
#include "ExtensionsDialog.h"
#include "OutlinePanel.h"
#include "ProblemsPanel.h"
#include "ProjectTree.h"
#include "ProvidersDialog.h"
#include "QuickAskDialog.h"
#include "ReferencesPanel.h"
#include "ShutdownDialog.h"
#include "SearchPanel.h"
#include "SessionBrowserDialog.h"
#include "SkillsDialog.h"
#include "TerminalPanel.h"
#include "WelcomeDialog.h"
#include "WorktreeDashboard.h"
#include "AiInspectorPanel.h"
#include "JobsPanel.h"
#include "CooperationPanel.h"
#include "cowork/CoworkPanel.h"
#include "cowork/CoworkPortal.h"
#include "shell/ActionIds.h"
#include "shell/ShellLayout.h"
#include "shell/SideBar.h"
#include "shell/TrayPresence.h"
#include "git/BlameController.h"
#include "git/GutterController.h"
#include "git/LogViewer.h"
#include "ipc/CoreClient.h"
#include "state/EngineAvailability.h"
#include "state/EnsembleCatalog.h"
#include "state/WindowTitle.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Range>
#include <KTextEditor/View>
#include "lsp/LspActions.h"
#include "lsp/LspClient.h"
#include "lsp/LspManager.h"
#include "lsp/WorkspaceSymbolDialog.h"

#include <KAboutData>
#include <KActionCollection>
#include <KMultiTabBar>
#include <KConfigGroup>
#include <KGlobalAccel>
#include <KHamburgerMenu>
#include <KHelpMenu>
#include <KLocalizedString>
#include <KMessageWidget>
#include <KNotification>
#include <KSharedConfig>
#include <KShortcutsDialog>
#include <KStandardAction>
#include <KToggleAction>
#include <KToolBar>
#include <KWindowSystem>

#include <QAction>
#include <QActionGroup>
#include <QApplication>
#include <algorithm>
#include <functional>
#include <QCloseEvent>
#include <QCoreApplication>
#include <QCursor>
#include <QDebug>
#include <QDesktopServices>
#include <QDir>
#include <QFileInfo>
#include <QHBoxLayout>
#include <QIcon>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonObject>
#include <QKeySequence>
#include <QMenu>
#include <QMenuBar>
#include <QPointer>
#include <QSharedPointer>
#include <QLabel>
#include <QLineEdit>
#include <QToolButton>
#include <QPlainTextEdit>
#include <QSplitter>
#include <QStackedWidget>
#include <QStatusBar>
#include <QTabWidget>
#include <QTimer>
#include <QToolBar>
#include <QToolButton>
#include <QUrl>
#include <QVBoxLayout>

MainWindow::MainWindow(const QString &openPath, QWidget *parent)
    : KMainWindow(parent)
{
    refreshWindowTitle();
    m_tabsByAgent =
        KSharedConfig::openConfig()->group(QStringLiteral("Editor"))
            .readEntry("tabsByAgent", false);
    // Capture first-run BEFORE setupUi() bumps the View schema: a brand-new
    // profile (no schema yet) gets the friendly Simple default; everyone else
    // keeps the Advanced surface they already know.
    m_firstRunProfile =
        KSharedConfig::openConfig()->group(QStringLiteral("View"))
            .readEntry("schema", 0) == 0;

    // The one action collection, created before anything that builds an action.
    // Its display name is what Settings ▸ Configure Shortcuts titles the group,
    // so it is the product name rather than the binary name.
    m_actions = new KActionCollection(this);
    m_actions->setComponentDisplayName(i18n("Agent Kate"));

    setupUi();
    setupActions();
    // Before setupTopToolbar: the toolbar's "Layout ▾" menu now reuses the very
    // same perspective actions the View ▸ Layout submenu holds, rather than
    // building a second, unregistered copy of each that could drift.
    setupPerspectives();
    setupTopToolbar();
    setupHamburger();
    setupShellShortcuts();
    setupCore();
    setupExperience();
    // Tray presence + the three global shortcuts (plan 27 §2/§3). After
    // setupActions (the tray menu reuses collection actions) and before
    // openLaunchPath (the first project's agents must already be wired into
    // the tray's decision layer).
    setupPresence();
    // Every action exists now. Lay the user's customised bindings from
    // [Shortcuts] over the declared defaults — this is the read half of what
    // KShortcutsDialog writes, and without it a rebind survives only until the
    // next launch. Then rebuild the rail hints, which name the ACTIVE binding.
    m_actions->readSettings();
    refreshPanelTooltips();
    // Before the first project, before the first task: say it if no agent CLI
    // is installed (audit F37). The registry serves its built-in engine list
    // immediately, so this is answerable without waiting for the core.
    updateEngineAvailabilityBanner();

    openLaunchPath(openPath);

    // Size/maximised state is persisted by KMainWindow's autosave into the
    // "MainWindow" group of the STATE config (~/.local/state/agentkatestaterc) —
    // NOT openConfig()'s agentkaterc. Reading the wrong file is what kept this
    // guard permanently false and re-imposed the default size on every launch.
    // KWindowConfig keys the size on the screen arrangement, and the count
    // leads: KF6 writes "4 screens: Width=1440" / "4 screens: Height=1065"
    // (older profiles carry the resolution-suffixed "Width 1920"), so matching on
    // "contains" covers both. A window last closed maximised restores from
    // Window-Maximized alone, which counts as restore data just as much.
    // Position is deliberately not fought for: under Wayland the compositor
    // places windows and there is no API to override it.
    const QStringList savedWindowKeys =
        KConfigGroup(KSharedConfig::openStateConfig(), QStringLiteral("MainWindow")).keyList();
    const bool haveSavedGeometry =
        std::any_of(savedWindowKeys.cbegin(), savedWindowKeys.cend(), [](const QString &k) {
            return k.contains(QLatin1String("Width")) || k.contains(QLatin1String("Height"))
                || k.contains(QLatin1String("Maximized"));
        });
    if (!haveSavedGeometry) {
        resize(1500, 900);
    }
    setAutoSaveSettings();

    // Both image cache dirs are append-only during a session; nothing ever
    // removed from them. Sweep once, well after launch, so a long-lived profile
    // does not accumulate gigabytes of dead screenshots.
    agentkate::scheduleImageCachePrune();
}

// openLaunchPath resolves one command-line style argument: a directory becomes a
// project, a file opens its parent directory as a project and then the file
// itself, and an empty path falls back to the working directory. Called once at
// construction and again for every path a second instance forwards to us.
void MainWindow::openLaunchPath(const QString &openPath)
{
    QString project = openPath;
    QString fileToOpen;
    if (!openPath.isEmpty()) {
        const QFileInfo info(openPath);
        if (info.isFile()) {
            project = info.absolutePath();
            fileToOpen = info.absoluteFilePath();
        } else if (info.isDir()) {
            project = info.absoluteFilePath();
        }
    }
    if (project.isEmpty()) {
        project = QDir::currentPath();
    }
    m_agent->addProject(project);
    if (!fileToOpen.isEmpty()) {
        m_editor->openFile(groupKey(), fileToOpen);
    }
}

// raiseAndActivate brings the window forward from wherever it is (minimised,
// buried, on another activity). Under Wayland the compositor grants that only
// against an XDG activation token proving a user action asked for it — the
// caller passes the one it was handed (a clicked notification), or leaves it
// empty when the current token has already been installed by someone else
// (AgentNotifier does exactly that, and KWindowSystem::updateStartupId is how
// the relaunch path installs the one KDBusService parks in the environment).
void MainWindow::raiseAndActivate(const QString &xdgActivationToken)
{
    if (!xdgActivationToken.isEmpty()) {
        KWindowSystem::setCurrentXdgActivationToken(xdgActivationToken);
    }
    setWindowState((windowState() & ~Qt::WindowMinimized) | Qt::WindowActive);
    show();
    raise();
    KWindowSystem::activateWindow(windowHandle());
    // Visible again means the normal quit-on-last-window rule applies again —
    // hideToTray() switches it off for exactly the hidden stretch (plan 27 §2).
    qApp->setQuitOnLastWindowClosed(true);
}

MainWindow::~MainWindow()
{
    // BlameController / GutterController are created as MainWindow children
    // when documents open — i.e. AFTER setupCore's one-shot reparent — so
    // m_core ends up mid-list and gets destroyed before them. Their pending
    // QTimer ticks then dereference a dangling m_core and segfault on exit.
    // Re-tail m_core once more here, immediately before ~QObject deletes the
    // child list, so it goes last regardless of what was added in between.
    if (m_core) {
        m_core->setParent(nullptr);
        m_core->setParent(this);
    }
    // Parentless on purpose (see showQuickAsk); reap it here.
    delete m_quickAsk;
    m_quickAsk = nullptr;
}

void MainWindow::setupUi()
{
    m_core = new CoreClient(this);

    // AgentDock is a QObject orchestrator: its widgets (the roster and the
    // panel stack) are placed by ShellLayout into the left strip and the
    // centre split's right pane respectively.
    m_agent = new AgentDock(m_core, this);

    m_editor = new EditorArea(this);
    m_tree = new ProjectTree(m_core, this);
    m_lsp = new LspManager(this);

    auto *problems = new ProblemsPanel(m_lsp, this);
    auto *references = new ReferencesPanel(this);
    auto *outline = new OutlinePanel(this);
    m_terminal = new TerminalPanel(this);
    m_worktreeDashboard = new WorktreeDashboard(m_core, this);
    m_logViewer = new LogViewer(m_core, this);
    m_coworkPanel = new CoworkPanel(m_core, this);
    // CoworkPortal services the core's portal requests using this window's surface.
    m_coworkPortal = new CoworkPortal(m_core, this, this);
    // The panel's browser-launch button needs the portal's park-then-flip of the
    // desktop-wide org.a11y.Status; BrowserLaunch no longer does it itself, precisely
    // so that an agent-triggered launch cannot (audit F8/F12).
    m_coworkPanel->setPortal(m_coworkPortal);

    connect(problems, &ProblemsPanel::activated, this,
            [this](const QString &path, int line) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path, line);
            });

    // Real ripgrep-backed Search panel (replaces the Phase 4 StubPanel). Lives
    // on the left strip; result activation opens the file in the editor.
    m_search = new SearchPanel(m_core, this);
    connect(m_search, &SearchPanel::resultActivated, this,
            [this](const QString &path, int line, int column) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path, line, column);
            });
    // Esc inside the search field returns focus to the active editor view.
    connect(m_search, &SearchPanel::escapeToEditor, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            view->setFocus();
        }
    });
    connect(m_search, &SearchPanel::attachToChatRequested, this,
            [this](const QStringList &paths) {
                if (auto *panel = qobject_cast<AgentPanel *>(m_agent->activePanel())) {
                    panel->attachPaths(paths);
                }
            });

    connect(m_worktreeDashboard, &WorktreeDashboard::statusMessage, this,
            [this](const QString &text) { statusBar()->showMessage(text, 6000); });
    connect(m_worktreeDashboard, &WorktreeDashboard::openTerminalRequested, this,
            [this](const QString &dir) {
                if (m_terminal && !dir.isEmpty()) {
                    raisePanelByKey(m_keyTerminal);
                    m_terminal->openTerminalAt(dir);
                }
            });

    // Three Kate-style activity strips frame the centre. Each SideBar pairs
    // a KMultiTabBar (placed at the window edge by ShellLayout) with its own
    // QStackedWidget (placed inside the centre splitter so the panel area
    // resizes with a draggable handle).
    m_leftBar = new SideBar(KMultiTabBar::Left, this);
    m_rightBar = new SideBar(KMultiTabBar::Right, this);
    m_bottomBar = new SideBar(KMultiTabBar::Bottom, this);
    for (SideBar *bar : { m_leftBar, m_rightBar, m_bottomBar }) {
        connect(bar, &SideBar::tabContextMenuRequested, this,
                [this, bar](int id, const QPoint &pos) {
                    showPanelContextMenu(bar, id, pos);
                });
    }

    registerPanel(m_keyRoster, QIcon::fromTheme(QStringLiteral("system-users")),
                  i18n("Projects && Agents"), m_agent->roster(),
                  QStringLiteral("left"));
    registerPanel(m_keyFiles, QIcon::fromTheme(QStringLiteral("folder")),
                  i18n("Files"), m_tree, QStringLiteral("left"));
    registerPanel(m_keyOutline, QIcon::fromTheme(QStringLiteral("code-context")),
                  i18n("Outline"), outline, QStringLiteral("left"));
    registerPanel(m_keySearch, QIcon::fromTheme(QStringLiteral("edit-find")),
                  i18n("Search"), m_search, QStringLiteral("left"));

    registerPanel(m_keyWorktrees, QIcon::fromTheme(QStringLiteral("vcs-branch")),
                  i18n("Worktrees"), m_worktreeDashboard,
                  QStringLiteral("right"));
    registerPanel(m_keyGitLog, QIcon::fromTheme(QStringLiteral("vcs-commit")),
                  i18n("Git Log"), m_logViewer, QStringLiteral("right"));
    m_coopPanel = new CooperationPanel(m_core, this);
    registerPanel(m_keyCoop, QIcon::fromTheme(QStringLiteral("im-user")),
                  i18n("Cooperation"), m_coopPanel, QStringLiteral("right"));
    registerPanel(m_keyCowork, QIcon::fromTheme(QStringLiteral("video-display")),
                  i18n("Cowork"), m_coworkPanel, QStringLiteral("right"));
    m_inspectorPanel = new AiInspectorPanel(m_core, this);
    registerPanel(m_keyInspector, QIcon::fromTheme(QStringLiteral("view-statistics")),
                  i18n("Agent Activity"), m_inspectorPanel, QStringLiteral("right"));

    registerPanel(m_keyTerminal, QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                  i18n("Terminal"), m_terminal, QStringLiteral("bottom"));
    registerPanel(m_keyReferences, QIcon::fromTheme(QStringLiteral("dialog-information")),
                  i18n("References"), references, QStringLiteral("bottom"));
    registerPanel(m_keyProblems, QIcon::fromTheme(QStringLiteral("dialog-warning")),
                  i18n("Problems"), problems, QStringLiteral("bottom"));
    // First user of the panel-command seam: three icon-only toolbar toggles that
    // appear in no menu and so were invisible to the palette (plan 27 §1).
    registerCommands(i18nc("@item command-palette group", "Problems"),
                     problems->commands());
    auto *coreLogView = new QPlainTextEdit(this);
    coreLogView->setReadOnly(true);
    coreLogView->setMaximumBlockCount(5000);
    coreLogView->setFrameShape(QFrame::NoFrame);
    registerPanel(m_keyOutput, QIcon::fromTheme(QStringLiteral("utilities-log-viewer")),
                  i18n("Output"), coreLogView, QStringLiteral("bottom"));
    m_jobsPanel = new JobsPanel(this);
    registerPanel(m_keyTasks, QIcon::fromTheme(QStringLiteral("view-task")),
                  i18n("Jobs"), m_jobsPanel, QStringLiteral("bottom"));

    // Plain-language descriptions for every activity-rail tab. The rail is
    // icon-only by default, which reads as cryptic to anyone who isn't a
    // power user — a hover that says what each panel is *for* makes the whole
    // shell approachable without adding chrome.
    const QHash<QString, QString> panelHelp = {
        {m_keyRoster, i18n("Your projects and the agents working in them. "
                           "Right-click an agent for actions.")},
        {m_keyFiles, i18n("Browse the files in the active project.")},
        {m_keyOutline, i18n("Jump to the functions, classes and symbols in the open file.")},
        {m_keySearch, i18n("Search across the whole project — plain text or regular expressions.")},
        // NOT "each agent works in its own isolated copy": an agent launched
        // with isolation "workspace", or with "auto" on a project git cannot
        // branch from, works directly in the user's own files — and this very
        // panel paints a "not isolated" pill for those rows
        // (WorktreeCopy::notIsolatedPill). The help text must not contradict
        // the panel it describes.
        {m_keyWorktrees, i18n("Where each agent does its work: its own private copy of "
                              "the project (a git worktree) where it has one, or your "
                              "own files where it has not. Review and manage them here.")},
        {m_keyGitLog, i18n("The commit history for the active project.")},
        {m_keyCoop, i18n("See who — human or agent — is editing what, in real time.")},
        {m_keyCowork, i18n("Let an agent see and control your desktop, only with your permission.")},
        {m_keyInspector, i18n("A live view of the agent's model, token use, cost and tool calls.")},
        {m_keyTerminal, i18n("A built-in command-line terminal.")},
        {m_keyReferences, i18n("Everywhere the selected symbol is used.")},
        {m_keyProblems, i18n("Errors and warnings from the language tools.")},
        {m_keyOutput, i18n("Background log output from Agent Kate's core.")},
        {m_keyTasks, i18n("Everything running in the background — detached shells, "
                          "sub-agents and Workflow runs — across every agent.")},
    };
    // Held on the panel record rather than used once here: the ordinal a tab's
    // shortcut hint names changes whenever a panel is moved between strips or
    // detached, so the tooltips have to be rebuildable (refreshPanelTooltips).
    for (auto it = panelHelp.constBegin(); it != panelHelp.constEnd(); ++it) {
        auto info = m_panels.find(it.key());
        if (info != m_panels.end()) {
            info->help = it.value();
        }
    }
    refreshPanelTooltips();
    // Wire the Output panel to drain m_core's coreLog. m_core does not exist
    // yet (setupCore runs after setupUi); defer the connect via a queued
    // single-shot, by which time m_core is constructed.
    QTimer::singleShot(0, this, [this, coreLogView] {
        if (m_core) {
            connect(m_core, &CoreClient::coreLog, coreLogView,
                    [coreLogView](const QString &line) {
                        coreLogView->appendPlainText(line);
                    });
        }
    });

    ShellLayout::Slots shellSlots;
    shellSlots.leftBar = m_leftBar->tabBar();
    shellSlots.leftStack = m_leftBar->panelStack();
    shellSlots.rightBar = m_rightBar->tabBar();
    shellSlots.rightStack = m_rightBar->panelStack();
    shellSlots.bottomBar = m_bottomBar->tabBar();
    shellSlots.bottomStack = m_bottomBar->panelStack();
    shellSlots.editor = m_editor;
    shellSlots.agentPanel = m_agent->panelStack();
    m_shell = new ShellLayout(shellSlots, this);

    // Shell-wide banner, above everything. The status bar's transient notices
    // cannot carry a condition that persists and disables the whole window —
    // losing the core does exactly that, so it gets a strip that stays put until
    // the connection is back.
    m_coreBanner = new KMessageWidget(this);
    m_coreBanner->setWordWrap(true);
    m_coreBanner->setCloseButtonVisible(false);
    m_coreBanner->setVisible(false);

    // "No agent CLI is installed" (audit F37). Its own strip, above the core's:
    // it is the more fundamental condition, and the two must never overwrite
    // each other's text.
    m_engineBanner = new KMessageWidget(this);
    m_engineBanner->setWordWrap(true);
    m_engineBanner->setCloseButtonVisible(false);
    m_engineBanner->setVisible(false);

    auto *centre = new QWidget(this);
    auto *centreLayout = new QVBoxLayout(centre);
    centreLayout->setContentsMargins(0, 0, 0, 0);
    centreLayout->setSpacing(0);
    centreLayout->addWidget(m_engineBanner);
    centreLayout->addWidget(m_coreBanner);
    centreLayout->addWidget(m_shell, 1);
    setCentralWidget(centre);

    // KMainWindow's autoSaveSettings() blob persists dock geometry. The new
    // shell stops using addDockWidget for tool panels, so that blob is dead.
    // Bump the schema once: the first launch on the new shell drops the
    // stored MainWindow state so KMainWindow falls back to our defaults.
    KSharedConfig::Ptr cfg = KSharedConfig::openConfig();
    KConfigGroup viewCfg = cfg->group(QStringLiteral("View"));
    const int schema = viewCfg.readEntry("schema", 0);
    if (schema < 2) {
        cfg->group(QStringLiteral("MainWindow")).deleteGroup();
        viewCfg.writeEntry("schema", 2);
        viewCfg.sync();
        QTimer::singleShot(0, this, [this] {
            statusBar()->showMessage(
                i18n("Layout has been reset for the new shell"), 6000);
        });
    }

    m_shell->restoreState(viewCfg.group(QStringLiteral("centreSplitter")));

    // Restore + persist the raised tab per strip.
    auto wireStrip = [this](SideBar *bar, const QString &key, int fallback) {
        const KConfigGroup g =
            KSharedConfig::openConfig()->group(QStringLiteral("View"));
        const int raised = g.readEntry(key, fallback);
        bar->setRaisedId(raised);
        connect(bar, &SideBar::raisedChanged, this, [this, key, bar](int id) {
            KSharedConfig::openConfig()
                ->group(QStringLiteral("View"))
                .writeEntry(key, id);
            if (bar == m_bottomBar && id >= 0) {
                m_lastBottomTab = id;
            }
        });
    };
    wireStrip(m_leftBar, QStringLiteral("leftStrip"), panelId(m_keyRoster));
    wireStrip(m_rightBar, QStringLiteral("rightStrip"), panelId(m_keyWorktrees));
    wireStrip(m_bottomBar, QStringLiteral("bottomStrip"), -1);
    m_lastBottomTab = m_bottomBar->raisedId();

    connect(m_lsp, &LspManager::definitionResolved, this,
            [this](const QString &path, int line) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_lsp, &LspManager::referencesResolved, references, &ReferencesPanel::setLocations);
    connect(m_lsp, &LspManager::referencesResolved, this,
            [this](const QList<Location> &) { raisePanelByKey(m_keyReferences); });
    connect(references, &ReferencesPanel::activated, this,
            [this](const QString &path, int line) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_lsp, &LspManager::symbolsResolved, outline, &OutlinePanel::setSymbols);
    connect(outline, &OutlinePanel::activated, this,
            [this](const QString &path, int line) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &path) {
        if (!path.isEmpty()) {
            m_lsp->requestSymbols(path);
            // Sync the file tree selection to the active editor when the user
            // has opted in via the persisted toggle.
            if (m_tree->isSyncWithEditor()) {
                m_tree->revealPath(path);
            }
        }
        updateLspStatus();
    });

    // A workspace edit (rename / code action) touched a file that isn't open.
    connect(m_lsp, &LspManager::openFileRequested, this,
            [this](const QString &path, int line) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path, line);
            });
    // Code actions arrived — pop the quick-fix menu at the cursor.
    connect(m_lsp, &LspManager::codeActionsResolved, this,
            [this](LspClient *client, const QJsonArray &actions) {
                LspActions::showMenu(m_lsp, client, actions, this, QCursor::pos());
            });
    // Rename requested — collect the new name and perform it.
    connect(m_lsp, &LspManager::renameRequested, this, [this](KTextEditor::View *view) {
        if (!view) {
            return;
        }
        const KTextEditor::Range word = view->document()->wordRangeAt(view->cursorPosition());
        const QString current = word.isValid() ? view->document()->text(word) : QString();
        bool ok = false;
        const QString name = QInputDialog::getText(
            this, i18nc("@title:window", "Rename Symbol"), i18n("New name:"),
            QLineEdit::Normal, current, &ok);
        if (ok && !name.isEmpty()) {
            m_lsp->performRename(view, name);
        }
    });
    // Transient LSP status messages and live server-status updates.
    connect(m_lsp, &LspManager::statusMessage, this,
            [this](const QString &text) { statusBar()->showMessage(text, 4000); });
    connect(m_lsp, &LspManager::serverStatusChanged, this, &MainWindow::updateLspStatus);

    connect(m_agent, &AgentDock::agentActivated, this, &MainWindow::onAgentActivated);
    // A fresh agent's thread id arrives after activation (async session start), so
    // re-point the thread-keyed panels when it lands — otherwise "Enable Cowork for
    // this agent" stays greyed out and Git Log keeps the stale thread.
    connect(m_agent, &AgentDock::activeThreadChanged, this, [this](const QString &threadId) {
        rememberThreadProject(threadId);
        // The shown agent's tab group was keyed on a pending (per-run) id while
        // it had no thread; re-key it and follow it, so tabs opened during the
        // session start stay with the agent instead of being stranded.
        if (adoptPendingEditorGroup(m_activeAgentId, m_activeProject, threadId)
            && m_editor) {
            m_editor->setActiveGroup(groupKey());
        }
        if (m_coworkPanel) {
            m_coworkPanel->setActiveThread(threadId, QString());
        }
        if (m_logViewer) {
            m_logViewer->setActiveSource(m_activeProject, threadId);
        }
        if (m_inspectorPanel) {
            m_inspectorPanel->setActiveThread(threadId);
        }
        updateAgentActions();
    });
    connect(m_agent, &AgentDock::projectFocused, this,
            [this](const QString &path) { m_tree->setRoot(path); });
    // A live promote (or a dormant agent's worktree path arriving in a later
    // git.snapshot) re-roots only the file browser's Worktree tab, without a
    // full agent re-activation.
    connect(m_agent, &AgentDock::activeWorktreeChanged, this,
            [this](const QString &worktreePath) {
                m_tree->setRoots(m_activeProject, worktreePath);
            });
    connect(m_agent, &AgentDock::openTerminalRequested, this,
            [this](const QString &dir) {
                if (m_terminal && !dir.isEmpty()) {
                    m_terminal->openTerminalAt(dir);
                }
            });
    connect(m_agent, &AgentDock::openWorktreeTerminalRequested, this,
            [this](const QString &dir) {
                if (m_terminal && !dir.isEmpty()) {
                    raisePanelByKey(m_keyTerminal);
                    m_terminal->openTerminalAt(dir);
                }
            });
    connect(m_agent, &AgentDock::agentTitlesChanged, m_worktreeDashboard,
            &WorktreeDashboard::setAgentTitles);
    // Jobs panel: agents publish their background work, and acting on a row
    // routes back to the agent that owns it.
    connect(m_agent, &AgentDock::jobsChanged, m_jobsPanel, &JobsPanel::setAgentJobs);
    connect(m_agent, &AgentDock::agentTitlesChanged, m_jobsPanel,
            &JobsPanel::setAgentTitles);
    connect(m_agent, &AgentDock::openJobsPanelRequested, this,
            [this] { raisePanelByKey(m_keyTasks); });
    // A clicked desktop notification: the dock has already selected the agent,
    // the window still has to come forward (it may be minimised or behind).
    // The notifier has already installed the notification's activation token as
    // the current one, so the empty argument here means "use it", not "none".
    connect(m_agent, &AgentDock::raiseWindowRequested, this,
            [this] { raiseAndActivate(); });

    // Task-bar-level "you are needed" (audit F50). A popup can be missed and
    // the window can be on another virtual desktop, in which case a blocked
    // agent produced no lasting signal anywhere. The title carries the count
    // for as long as it lasts; demandAttention flashes the task-bar entry once
    // per transition into "someone is waiting", and only while we are not the
    // active window — the roster already says it plainly when we are.
    connect(m_agent, &AgentDock::attentionCountChanged, this, [this](int count) {
        // Recorded, never written straight to the window: the project switch is
        // the other writer of this title, and when the two raced it erased the
        // count while agents were still blocked. attentionCountChanged is
        // change-gated, so nothing would ever have put it back.
        m_attentionCount = count;
        refreshWindowTitle();
        // QApplication::alert is the portable "demand attention" request (it
        // no-ops when the window IS active), unlike KWindowSystem, which in KF6
        // exposes activation but no state setter.
        if (count > 0) {
            QApplication::alert(this);
        }
    });
    connect(m_jobsPanel, &JobsPanel::clearFinishedRequested, m_agent,
            &AgentDock::forgetFinishedJobsEverywhere);
    connect(m_jobsPanel, &JobsPanel::openWorkflowRequested, m_agent,
            &AgentDock::showWorkflowMonitorFor);
    connect(m_jobsPanel, &JobsPanel::goToAgentRequested, m_agent,
            &AgentDock::selectAgentByThread);
    connect(m_jobsPanel, &JobsPanel::statusMessage, this,
            [this](const QString &text) { statusBar()->showMessage(text, 4000); });
    connect(m_jobsPanel, &JobsPanel::openFileRequested, this,
            [this](const QString &threadId, const QString &path) {
                ensureEditorVisible();
                m_editor->openFile(editorGroupForThread(threadId), path);
            });
    // The same titles name the agents in the inspector's all-threads timeline,
    // so a cross-agent call reads as "Reviewer" rather than a bare thread id.
    connect(m_agent, &AgentDock::agentTitlesChanged, this,
            [this](const QHash<QString, QString> &titles) {
                if (m_inspectorPanel) {
                    m_inspectorPanel->setAgentTitles(titles);
                }
            });
    connect(m_agent, &AgentDock::openDiff, this,
            [this](const QString &title, const QString &diff) {
                m_editor->openDiff(groupKey(), title, diff);
            });
    // A clicked text/file attachment chip opens the file in the editor.
    connect(m_agent, &AgentDock::openFileRequested, this,
            [this](const QString &path) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path);
            });
    connect(m_agent, &AgentDock::projectClosed, this,
            [this](const QString &path) {
                if (m_terminal) {
                    m_terminal->closeProject(path);
                }
            });

    connect(m_tree, &ProjectTree::fileActivated, this,
            [this](const QString &path) {
                ensureEditorVisible();
                m_editor->openFile(groupKey(), path);
            });
    connect(m_tree, &ProjectTree::terminalRequested, this,
            [this](const QString &dir) {
                if (m_terminal) {
                    raisePanelByKey(m_keyTerminal);
                    m_terminal->openTerminalAt(dir);
                }
            });
    connect(m_tree, &ProjectTree::runCommandRequested, this,
            [this](const QString &dir, const QString &command) {
                if (m_terminal) {
                    raisePanelByKey(m_keyTerminal);
                    m_terminal->runCommandAt(dir, command);
                }
            });
    connect(m_tree, &ProjectTree::attachToChatRequested, this,
            [this](const QStringList &paths) {
                if (auto *panel = qobject_cast<AgentPanel *>(m_agent->activePanel())) {
                    panel->attachPaths(paths);
                }
            });
    connect(m_editor, &EditorArea::openFilesChanged, this, &MainWindow::pushOpenFilesToCore);
    // Tab opened/closed: refresh the persisted session too. Debounced, because
    // persistence used to run only in closeEvent — so a crash replayed the tabs
    // of whatever the previous run happened to leave behind.
    m_sessionPersistTimer = new QTimer(this);
    m_sessionPersistTimer->setSingleShot(true);
    m_sessionPersistTimer->setInterval(1000);
    connect(m_sessionPersistTimer, &QTimer::timeout, this,
            &MainWindow::persistEditorSession);
    connect(m_editor, &EditorArea::openFilesChanged, this,
            &MainWindow::schedulePersistEditorSession);
    connect(m_editor, &EditorArea::revealInTreeRequested, this,
            [this](const QString &path) {
                if (m_tree) {
                    m_tree->revealPath(path);
                    raisePanelByKey(m_keyFiles);
                }
            });
    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &path) {
        if (m_core->isConnected()) {
            m_core->call(QStringLiteral("coop.setPresence"),
                         QJsonObject{{QStringLiteral("owner"), QStringLiteral("human")},
                                     {QStringLiteral("focusedFile"), path}});
        }
    });
    connect(m_editor, &EditorArea::documentOpened, this,
            [this](KTextEditor::Document *doc, const QString &path) {
                m_lsp->documentOpened(doc, m_activeProject);
                // Attach a per-document git blame controller (off by default;
                // the View menu's "Toggle Git Blame" turns it on for the
                // active view). The view to render against is the current
                // editor view, looked up via the editor area.
                if (!m_blames.contains(doc)) {
                    if (auto *view = m_editor->currentView()) {
                        auto *bc = new BlameController(doc, view, path, m_core, this);
                        m_blames.insert(doc, bc);
                    }
                }
                // Attach a per-document git gutter. The controller stops
                // polling itself when the document is closed.
                if (!m_gutters.contains(doc)) {
                    auto *gc = new GutterController(doc, path, m_core, this);
                    m_gutters.insert(doc, gc);
                    connect(gc, &GutterController::statusUpdated, this,
                            [this](const QString &p, const QString &branch,
                                   const QString &status, int hunks) {
                                if (p != m_activeFilePath || !m_gitStatusLabel) {
                                    return;
                                }
                                const QString left = branch.isEmpty()
                                    ? QStringLiteral("—") : branch;
                                m_gitStatusLabel->setText(
                                    QStringLiteral("⎇ %1 · %2 · %3 hunks")
                                        .arg(left,
                                             status.isEmpty()
                                                 ? QStringLiteral("clean") : status)
                                        .arg(hunks));
                            });
                }
            });
    connect(m_editor, &EditorArea::documentClosed, this,
            [this](KTextEditor::Document *doc) {
                m_lsp->documentClosed(doc);
                if (auto *gc = m_gutters.take(doc)) {
                    gc->deleteLater();
                }
                if (auto *bc = m_blames.take(doc)) {
                    bc->deleteLater();
                }
            });
    // Surface the editor's own save/autosave feedback ("Saved x.cpp",
    // "Autosaved x.cpp") in the status bar.
    connect(m_editor, &EditorArea::statusMessage, this,
            [this](const QString &text) { statusBar()->showMessage(text, 4000); });
    // Autosave writes bypass the LSP formatter, but the language server still
    // needs telling the file changed on disk so diagnostics refresh.
    connect(m_editor, &EditorArea::documentAutosaved, this,
            [this](KTextEditor::Document *doc) {
                if (doc && !doc->url().isEmpty()) {
                    m_lsp->documentSaved(doc->url().toLocalFile());
                }
            });
    // Flush unsaved edits when the app loses focus (Alt-Tab away, etc.), so
    // autosave doesn't wait out its debounce while attention is elsewhere.
    connect(qApp, &QApplication::applicationStateChanged, this,
            [this](Qt::ApplicationState state) {
                if (state != Qt::ApplicationActive && m_editor) {
                    m_editor->autosaveAll();
                }
            });
}

QAction *MainWindow::registerAction(const QString &id, QAction *act,
                                    const QList<QKeySequence> &defaults)
{
    if (!act || !m_actions) {
        return act;
    }
    m_actions->addAction(id, act);
    if (!defaults.isEmpty()) {
        // setDefaultShortcuts, not setShortcuts: it records the sequence as the
        // action's DEFAULT and applies it, so "Reset to Defaults" in the
        // shortcuts dialog has something to reset to and a user override is a
        // deviation the dialog can show rather than an invisible overwrite.
        KActionCollection::setDefaultShortcuts(act, defaults);
    }
    return act;
}

void MainWindow::registerCommands(const QString &group,
                                  const QList<QAction *> &actions)
{
    for (QAction *act : actions) {
        if (act) {
            m_panelCommands.append({group, QPointer<QAction>(act)});
        }
    }
}

void MainWindow::configureShortcuts()
{
    // Stack-allocated and modal: the dialog holds raw KActionCollection
    // pointers, and the editor part's collection belongs to whichever view is
    // current — blocking here is what guarantees that view cannot be closed out
    // from under it.
    KShortcutsDialog dlg(this);
    dlg.addCollection(m_actions, i18nc("@title:group shortcut list", "Agent Kate"));
    // Feed the embedded editor's own collection in too. EditorArea blanks the
    // part's file_save / file_save_as bindings to stop Qt disabling Ctrl+S with
    // an "Ambiguous shortcut overload" — a code-level workaround that leaves the
    // user no way to see what happened or to choose differently. Listing both
    // collections here is the surface where that conflict becomes visible and
    // resolvable. (The blanking stays: it is what makes Ctrl+S work out of the
    // box, and removing it would reintroduce the ambiguity for every user in
    // order to serve the few who want to rebind.)
    if (KTextEditor::View *view = m_editor ? m_editor->currentView() : nullptr) {
        if (KActionCollection *ac = view->actionCollection()) {
            dlg.addCollection(ac, i18nc("@title:group shortcut list", "Text Editor"));
        }
    }
    // configure(true) saves accepted changes into [Shortcuts]; the constructor
    // read that back at startup.
    dlg.configure(/*saveSettings=*/true);
    // A rebound rail accelerator changes what the tab hints should say.
    refreshPanelTooltips();
}

// --- KDE presence: tray + global shortcuts (plan 27 §2/§3) ------------------

void MainWindow::setupPresence()
{
    using agentkate::TrayPresence;

    // The decision layer always exists — the answer-attention shortcut reads
    // firstAttentionAgent() even in a session with no tray host. It is fed the
    // per-agent forwards of exactly the wires AgentNotifier consumes, so the
    // tray can never claim a state the roster card does not show.
    m_tray = new TrayPresence(this);
    connect(m_agent, &AgentDock::agentTitleChanged, m_tray,
            &TrayPresence::setAgentTitle);
    connect(m_agent, &AgentDock::agentStatusChanged, m_tray,
            &TrayPresence::reportStatus);
    connect(m_agent, &AgentDock::agentAttentionChanged, m_tray,
            &TrayPresence::reportAttention);
    connect(m_agent, &AgentDock::agentRemoved, m_tray, &TrayPresence::forgetAgent);
    connect(m_tray, &TrayPresence::agentActivationRequested, this,
            [this](int agentId) {
                raiseAndActivate();
                if (m_agent) {
                    m_agent->selectAgent(agentId);
                }
            });
    connect(m_tray, &TrayPresence::quitRequested, this, &MainWindow::requestQuit);

    // The three global shortcuts. Three is a budget, not a starting point —
    // every one is taken from the user's entire desktop. Registering through
    // KGlobalAccel is what lists them under System Settings ▸ Shortcuts ▸
    // Agent Kate, which is where KDE users discover what an app can do.
    auto *showHideAct =
        new QAction(QIcon::fromTheme(QStringLiteral("window")),
                    i18n("Show/Hide Agent Kate"), this);
    registerAction(QLatin1String(ActionIds::GlobalShowHide), showHideAct);
    connect(showHideAct, &QAction::triggered, this,
            &MainWindow::toggleWindowPresence);
    KGlobalAccel::setGlobalShortcut(
        showHideAct, QList<QKeySequence>{QKeySequence(Qt::META | Qt::Key_A)});

    auto *quickAskAct =
        new QAction(QIcon::fromTheme(QStringLiteral("mail-message-new")),
                    i18n("Quick-Ask the Active Agent"), this);
    registerAction(QLatin1String(ActionIds::GlobalQuickAsk), quickAskAct);
    connect(quickAskAct, &QAction::triggered, this, &MainWindow::showQuickAsk);
    KGlobalAccel::setGlobalShortcut(
        quickAskAct,
        QList<QKeySequence>{QKeySequence(Qt::META | Qt::SHIFT | Qt::Key_A)});

    // Registered UNBOUND (recorded decision): the least frequent of the three
    // and the most likely to collide. It appears in System Settings all the
    // same, which is where a user who wants it assigns it.
    auto *answerAct =
        new QAction(QIcon::fromTheme(QStringLiteral("dialog-warning")),
                    i18n("Answer Pending Attention"), this);
    registerAction(QLatin1String(ActionIds::GlobalAnswerAttention), answerAct);
    connect(answerAct, &QAction::triggered, this,
            &MainWindow::answerPendingAttention);
    KGlobalAccel::setGlobalShortcut(answerAct, QList<QKeySequence>{});
    // This is also the tray's "Answer pending permission" entry until plan
    // 24 adds questions. It is the same action as the global shortcut, so the
    // enabled state cannot drift between the two entry points.
    answerAct->setEnabled(false);
    connect(m_tray, &TrayPresence::presenceChanged, answerAct,
            [this, answerAct] {
                answerAct->setEnabled(m_tray && m_tray->firstAttentionAgent() >= 0);
            });

    // The tray item itself, only when this session can actually show one. The
    // context menu is built FROM the collection — the same QActions the menus
    // hold, so enablement and retitling stay one story. TrayPresence inserts
    // its per-agent submenu at the front.
    if (TrayPresence::hostAvailable()) {
        auto *trayMenu = new QMenu(this);
        if (QAction *newAgent =
                m_actions->action(QLatin1String(ActionIds::AgentNew))) {
            trayMenu->addAction(newAgent);
        }
        trayMenu->addAction(showHideAct);
        trayMenu->addAction(answerAct);
        trayMenu->addSeparator();
        if (QAction *quitAct =
                m_actions->action(QLatin1String(ActionIds::FileQuit))) {
            trayMenu->addAction(quitAct);
        }
        // Force the native window into existence so the item can associate
        // with it before the first show().
        winId();
        m_tray->embed(windowHandle(), trayMenu);
    }
    // Close-to-tray enabled in a host-less session? Say once, at startup, that
    // the close button will keep quitting here.
    maybeExplainNoTrayHost();
}

void MainWindow::requestQuit()
{
    m_quitRequested = true;
    // A quit from the tray can arrive while the window is hidden; the
    // ShutdownDialog and any unsaved-file prompts need a visible home.
    if (!isVisible()) {
        raiseAndActivate();
    }
    close();
}

bool MainWindow::closeToTrayEnabled() const
{
    return KSharedConfig::openConfig()
        ->group(QStringLiteral("Behaviour"))
        .readEntry("closeToTray", false);
}

void MainWindow::hideToTray()
{
    // Only while hidden to the tray: with the main window gone, Qt would
    // otherwise end the process as soon as the last other window (a floating
    // panel, a dialog) closed. Flipped back in raiseAndActivate and on the
    // genuine-quit path, so a session can never get stuck unquittable.
    qApp->setQuitOnLastWindowClosed(false);
    hide();

    // One-shot, first time ever: a window that vanishes with work still
    // running and no sign of where it went is a support ticket.
    KConfigGroup grp =
        KSharedConfig::openConfig()->group(QStringLiteral("Behaviour"));
    if (grp.readEntry("hideToTrayNoticeShown", false)) {
        return;
    }
    grp.writeEntry("hideToTrayNoticeShown", true);
    grp.sync();
    const int running = m_agent ? m_agent->runningAgentCount() : 0;
    auto *n = new KNotification(QStringLiteral("hiddenToTray"));
    n->setComponentName(QStringLiteral("agentkate"));
    n->setTitle(i18n("Agent Kate is still running"));
    n->setText(running > 0
                   ? i18np("%1 agent keeps working in the background. Find "
                           "Agent Kate in the system tray.",
                           "%1 agents keep working in the background. Find "
                           "Agent Kate in the system tray.",
                           running)
                   : i18n("Find Agent Kate in the system tray."));
    n->setIconName(QStringLiteral("agentkate"));
    n->setAutoDelete(true);
    n->sendEvent();
}

void MainWindow::toggleWindowPresence()
{
    if (isVisible() && isActiveWindow()) {
        // Toggle away. With a live tray the window can vanish entirely;
        // without one it only minimises — a bare-WM session must always keep
        // a way back on screen.
        if (m_tray && m_tray->active()) {
            qApp->setQuitOnLastWindowClosed(false);
            hide();
        } else {
            showMinimized();
        }
        return;
    }
    raiseFromGlobalShortcut();
    if (m_agent) {
        m_agent->focusActiveComposer();
    }
}

void MainWindow::showQuickAsk()
{
    if (!m_agent || !m_agent->hasActiveAgent()) {
        // No target agent: the useful reading of "ask an agent" is "make one".
        raiseFromGlobalShortcut();
        if (m_agent) {
            m_agent->newAgentInActiveProjectGuided();
        }
        return;
    }
    if (!m_quickAsk) {
        // Deliberately parentless: a transient child of a HIDDEN main window
        // is exactly what a Wayland compositor may refuse to map, and hidden
        // is quick-ask's home case. Deleted in the destructor.
        m_quickAsk = new QuickAskDialog(nullptr);
        connect(m_quickAsk, &QuickAskDialog::submitted, this,
                [this](const QString &text) {
                    if (m_agent && m_agent->quickAskActiveAgent(text)) {
                        m_quickAsk->acceptSent();
                    } else {
                        m_quickAsk->showError(
                            i18n("Could not send — open Agent Kate to see why."));
                    }
                });
    }
    m_quickAsk->setTargetName(m_agent->activeAgentTitle());
    m_quickAsk->popUp();
}

void MainWindow::answerPendingAttention()
{
    raiseFromGlobalShortcut();
    const int agentId = m_tray ? m_tray->firstAttentionAgent() : -1;
    if (agentId >= 0 && m_agent) {
        m_agent->selectAgent(agentId);
    }
}

void MainWindow::raiseFromGlobalShortcut()
{
    // KGlobalAccel parks an XDG activation token in the environment for the
    // duration of the triggered action — the same convention KDBusService
    // uses for a relaunch. Spend it through the ONE raise path; a second
    // hand-rolled raise is how Wayland activation silently stops working.
    const QString token = qEnvironmentVariable("XDG_ACTIVATION_TOKEN");
    qunsetenv("XDG_ACTIVATION_TOKEN");
    raiseAndActivate(token);
}

void MainWindow::maybeExplainNoTrayHost()
{
    KConfigGroup grp =
        KSharedConfig::openConfig()->group(QStringLiteral("Behaviour"));
    if (!agentkate::TrayPresence::shouldExplainNoHost(
            closeToTrayEnabled(), m_tray && m_tray->active(),
            grp.readEntry("closeToTrayNoHostExplained", false))) {
        return;
    }
    grp.writeEntry("closeToTrayNoHostExplained", true);
    grp.sync();
    auto *banner = new KMessageWidget(
        i18n("Close to System Tray is enabled, but this session has no system "
             "tray. The close button will keep quitting Agent Kate — hiding "
             "the window here would leave no way to bring it back."),
        this);
    banner->setMessageType(KMessageWidget::Information);
    banner->setWordWrap(true);
    banner->setCloseButtonVisible(true);
    if (auto *layout = qobject_cast<QVBoxLayout *>(centralWidget()->layout())) {
        layout->insertWidget(0, banner);
        banner->animatedShow();
    }
}

void MainWindow::setupActions()
{
    QMenu *fileMenu = menuBar()->addMenu(i18n("&File"));

    auto *openAct = new QAction(QIcon::fromTheme(QStringLiteral("folder-open")),
                                i18n("&Open Project…"), this);
    registerAction(QLatin1String(ActionIds::FileOpenProject), openAct,
                   {QKeySequence(QKeySequence::Open)});
    connect(openAct, &QAction::triggered, this, [this] { m_agent->openProjectDialog(); });
    fileMenu->addAction(openAct);

    auto *welcomeAct = new QAction(QIcon::fromTheme(QStringLiteral("go-home")),
                                   i18n("&Welcome Screen…"), this);
    registerAction(QLatin1String(ActionIds::FileWelcomeScreen), welcomeAct);
    welcomeAct->setToolTip(i18n("Pick a recent project, open a folder, or start a new one."));
    connect(welcomeAct, &QAction::triggered, this, [this] {
        WelcomeDialog dlg(this);
        if (dlg.exec() != QDialog::Accepted) {
            return;
        }
        // "Reopen session" hands back the whole set, not just one path (F47).
        const QStringList paths = dlg.selectedPaths();
        for (const QString &path : paths) {
            if (!path.isEmpty()) {
                m_agent->addProject(path);
            }
        }
    });
    fileMenu->addAction(welcomeAct);

    auto *resumeAct = new QAction(QIcon::fromTheme(QStringLiteral("document-open-recent")),
                                  i18n("&Resume a Session…"), this);
    registerAction(QLatin1String(ActionIds::FileResumeSession), resumeAct);
    connect(resumeAct, &QAction::triggered, this, [this] {
        auto *dlg = new SessionBrowserDialog(m_core, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        connect(dlg, &SessionBrowserDialog::attachRequested, m_agent,
                &AgentDock::attachSession);
        dlg->show();
    });
    fileMenu->addAction(resumeAct);

    // KStandardAction already declares Ctrl+S as this action's default, so it
    // only needs the collection id — which is the same "file_save" the standard
    // action names itself, kept in ActionIds so the frozen catalogue is complete.
    QAction *saveAct = KStandardAction::save(this, &MainWindow::onSave, this);
    registerAction(QLatin1String(ActionIds::FileSave), saveAct);
    // Own Ctrl+S at the application level so it fires no matter which widget has
    // focus. Paired with EditorArea clearing the KTextEditor view's internal
    // file_save binding, this resolves the "Ambiguous shortcut overload" that
    // disabled Ctrl+S whenever the editor had focus.
    saveAct->setShortcutContext(Qt::ApplicationShortcut);
    fileMenu->addAction(saveAct);

    // KStandardAction has no saveAll in this KF6 release; build the equivalent
    // by hand with the conventional icon and Ctrl+Shift+S shortcut.
    auto *saveAllAct = new QAction(QIcon::fromTheme(QStringLiteral("document-save-all")),
                                   i18n("Save A&ll"), this);
    registerAction(QLatin1String(ActionIds::FileSaveAll), saveAllAct,
                   {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_S)});
    connect(saveAllAct, &QAction::triggered, this, &MainWindow::onSaveAll);
    fileMenu->addAction(saveAllAct);

    fileMenu->addSeparator();
    // requestQuit, not close: File ▸ Quit is a GENUINE quit even while
    // close-to-tray is on — the hide is reserved for the window's close button.
    fileMenu->addAction(registerAction(QLatin1String(ActionIds::FileQuit),
                                       KStandardAction::quit(this, &MainWindow::requestQuit, this)));

    // The Agent menu sits right after File — agents are the primary thing this
    // app is about, and its actions were previously buried in roster right-clicks.
    setupAgentMenu();

    QMenu *optionsMenu = menuBar()->addMenu(i18n("&Options"));
    QMenu *grouping = optionsMenu->addMenu(i18n("Editor Tabs Grouped By"));
    auto *groupingActs = new QActionGroup(this);
    QAction *byProject = registerAction(QLatin1String(ActionIds::OptionsTabsByProject),
                                        grouping->addAction(i18n("Project")));
    QAction *byAgent = registerAction(QLatin1String(ActionIds::OptionsTabsByAgent),
                                      grouping->addAction(i18n("Agent")));
    byProject->setCheckable(true);
    byAgent->setCheckable(true);
    groupingActs->addAction(byProject);
    groupingActs->addAction(byAgent);
    (m_tabsByAgent ? byAgent : byProject)->setChecked(true);
    connect(byProject, &QAction::triggered, this, [this] { setTabsByAgent(false); });
    connect(byAgent, &QAction::triggered, this, [this] { setTabsByAgent(true); });

    optionsMenu->addSeparator();
    const KConfigGroup agentCfg =
        KSharedConfig::openConfig()->group(QStringLiteral("Agent"));

    auto *enterSendsAct = registerAction(
        QLatin1String(ActionIds::OptionsEnterSends),
        optionsMenu->addAction(i18n("&Enter Sends the Message")));
    enterSendsAct->setCheckable(true);
    enterSendsAct->setChecked(agentCfg.readEntry("enterSends", true));
    enterSendsAct->setToolTip(i18n("On: Enter sends, Shift+Enter starts a new line. "
                                   "Off: Ctrl+Enter sends."));
    connect(enterSendsAct, &QAction::toggled, this, [this](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("enterSends", on);
        m_agent->applyChatSettings();
    });

    auto *showToolsAct = registerAction(
        QLatin1String(ActionIds::OptionsShowToolCalls),
        optionsMenu->addAction(i18n("Show &Tool Calls")));
    showToolsAct->setCheckable(true);
    showToolsAct->setChecked(agentCfg.readEntry("showTools", true));
    connect(showToolsAct, &QAction::toggled, this, [this](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("showTools", on);
        m_agent->applyChatSettings();
    });

    // Autosave: write file edits to disk automatically ~1s after you stop
    // typing (and on focus loss). On by default; persisted in [Editor] autosave.
    const KConfigGroup editorCfg =
        KSharedConfig::openConfig()->group(QStringLiteral("Editor"));
    const bool autosaveOn = editorCfg.readEntry("autosave", true);
    auto *autosaveAct = registerAction(QLatin1String(ActionIds::OptionsAutosave),
                                       optionsMenu->addAction(i18n("&Autosave files")));
    autosaveAct->setCheckable(true);
    autosaveAct->setChecked(autosaveOn);
    autosaveAct->setToolTip(i18n("Automatically save your edits shortly after you "
                                 "stop typing. Manual Ctrl+S still formats the file."));
    connect(autosaveAct, &QAction::toggled, this, [this](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Editor"))
            .writeEntry("autosave", on);
        if (m_editor) {
            m_editor->setAutosaveEnabled(on);
        }
    });
    if (m_editor) {
        m_editor->setAutosaveEnabled(autosaveOn);
    }

    // Close-to-tray (plan 27 §2). DEFAULT OFF — a recorded decision: changing
    // what the close button does without asking is hostile. With it on (and a
    // StatusNotifier host present) closing hides the window while agents keep
    // working; File ▸ Quit and the tray's Quit still run the real shutdown.
    auto *closeTrayAct = registerAction(
        QLatin1String(ActionIds::OptionsCloseToTray),
        optionsMenu->addAction(i18n("Close to System &Tray")));
    closeTrayAct->setCheckable(true);
    closeTrayAct->setChecked(closeToTrayEnabled());
    closeTrayAct->setToolTip(
        i18n("Closing the window hides Agent Kate in the system tray while "
             "agents keep working. Quit from the File menu or the tray to "
             "really stop."));
    connect(closeTrayAct, &QAction::toggled, this, [this](bool on) {
        KConfigGroup grp =
            KSharedConfig::openConfig()->group(QStringLiteral("Behaviour"));
        grp.writeEntry("closeToTray", on);
        grp.sync();
        // Turning it on in a session with no tray earns the one-time
        // explanation that the close button will keep quitting here.
        maybeExplainNoTrayHost();
    });

    optionsMenu->addSeparator();
    auto *providersAct = registerAction(
        QLatin1String(ActionIds::OptionsConfigureProviders),
        optionsMenu->addAction(i18n("Configure &Providers…")));
    providersAct->setToolTip(i18n(
        "Configure third-party API providers for Claude Code and the Kimi "
        "Code provider registry."));
    connect(providersAct, &QAction::triggered, this, [this] {
        ProvidersDialog dlg(this, m_core);
        if (dlg.exec() == QDialog::Accepted) {
            m_agent->reloadProviders();
        }
    });

    optionsMenu->addSeparator();
    auto *appearanceAct = registerAction(
        QLatin1String(ActionIds::OptionsAppearance),
        optionsMenu->addAction(
            QIcon::fromTheme(QStringLiteral("preferences-desktop-color")),
            i18n("&Appearance…")));
    appearanceAct->setToolTip(i18n(
        "Give Agent Kate its own look — a signature theme or any KDE colour "
        "scheme — independent of the rest of your desktop."));
    connect(appearanceAct, &QAction::triggered, this, [this] {
        AppearanceDialog dlg(this);
        dlg.exec();
    });

    // Experience level: Simple shows only the essentials; Advanced reveals the
    // full developer surface. The radio mirrors the status-bar toggle.
    QMenu *expMenu = optionsMenu->addMenu(QIcon::fromTheme(QStringLiteral("games-difficult")),
                                          i18n("E&xperience Level"));
    auto *expGroup = new QActionGroup(this);
    m_simpleAct = registerAction(QLatin1String(ActionIds::OptionsExperienceSimple),
                                 expMenu->addAction(i18n("&Simple — just the essentials")));
    m_advancedAct = registerAction(QLatin1String(ActionIds::OptionsExperienceAdvanced),
                                   expMenu->addAction(i18n("&Advanced — every developer tool")));
    for (QAction *a : {m_simpleAct, m_advancedAct}) {
        a->setCheckable(true);
        expGroup->addAction(a);
    }
    m_simpleAct->setToolTip(i18n("Hide the code editor's developer tools and panels "
                                 "— ideal for working alongside an agent."));
    m_advancedAct->setToolTip(i18n("Show everything: language tools, git, terminal and all panels."));
    connect(m_simpleAct, &QAction::triggered, this,
            [this] { applyExperienceLevel(QStringLiteral("simple")); });
    connect(m_advancedAct, &QAction::triggered, this,
            [this] { applyExperienceLevel(QStringLiteral("advanced")); });

    QMenu *viewMenu = menuBar()->addMenu(i18n("&View"));

    // Ctrl+Shift+P is the convention; Ctrl+P is a friendly second binding since
    // Agent Kate has no print action to clash with.
    auto *paletteAct = registerAction(
        QLatin1String(ActionIds::ViewCommandPalette),
        viewMenu->addAction(QIcon::fromTheme(QStringLiteral("show-menu")),
                            i18n("&Command Palette…")),
        {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_P),
         QKeySequence(Qt::CTRL | Qt::Key_P)});
    paletteAct->setToolTip(
        i18n("Search and run any command by name — the fastest way to reach "
             "every feature."));
    connect(paletteAct, &QAction::triggered, this, &MainWindow::showCommandPalette);
    viewMenu->addSeparator();
    m_blameToggle = registerAction(QLatin1String(ActionIds::ViewGitBlame),
                                   viewMenu->addAction(i18n("Show Git &Blame")),
                                   {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_B)});
    m_blameToggle->setCheckable(true);
    m_blameToggle->setToolTip(
        i18n("Show per-line author / sha annotations for the active editor."));
    connect(m_blameToggle, &QAction::toggled, this, [this](bool on) {
        KTextEditor::View *view = m_editor->currentView();
        if (!view) {
            return;
        }
        BlameController *bc = m_blames.value(view->document(), nullptr);
        if (!bc) {
            return;
        }
        bc->setEnabled(on);
    });
    // Sync the checkbox state when the user moves between editor tabs: the
    // toggle reflects the active view's controller, not a window-wide flag.
    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &) {
        if (!m_blameToggle) {
            return;
        }
        KTextEditor::View *view = m_editor->currentView();
        BlameController *bc = view ? m_blames.value(view->document(), nullptr) : nullptr;
        QSignalBlocker block(m_blameToggle);
        m_blameToggle->setChecked(bc != nullptr && bc->isEnabled());
        m_blameToggle->setEnabled(bc != nullptr);
    });
    viewMenu->addSeparator();
    m_toggleBottomAct = registerAction(
        QLatin1String(ActionIds::ViewToggleBottomPanel),
        viewMenu->addAction(i18n("Toggle &Bottom Panel")),
        {QKeySequence(Qt::CTRL | Qt::Key_J)});
    m_toggleBottomAct->setToolTip(
        i18n("Show or hide the Terminal / References / Problems strip."));
    connect(m_toggleBottomAct, &QAction::triggered, this, [this] {
        if (!m_bottomBar) {
            return;
        }
        if (m_bottomBar->raisedId() >= 0) {
            m_bottomBar->setRaisedId(-1);
        } else {
            const int target = m_lastBottomTab >= 0 ? m_lastBottomTab
                                                    : panelId(m_keyProblems);
            m_bottomBar->setRaisedId(target);
        }
    });

    auto *findInProjAct = registerAction(
        QLatin1String(ActionIds::ViewFindInProject),
        viewMenu->addAction(QIcon::fromTheme(QStringLiteral("edit-find")),
                            i18n("Find in &Project…")),
        {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_F)});
    findInProjAct->setToolTip(
        i18n("Search the active project with filters (case, regex, globs)."));
    connect(findInProjAct, &QAction::triggered, this, [this] {
        if (!m_search) {
            return;
        }
        raisePanelByKey(m_keySearch);
        m_search->focusQuery();
    });

    // F3 / Shift+F3 step through the project-search results, opening each in
    // turn. Scoped to the SearchPanel (WidgetWithChildrenShortcut) so they never
    // clash with KTextEditor's own find-next while a code editor has focus.
    auto *nextMatchAct = registerAction(QLatin1String(ActionIds::ViewNextSearchMatch),
                                        new QAction(i18n("Next Search Match"), this),
                                        {QKeySequence(Qt::Key_F3)});
    nextMatchAct->setShortcutContext(Qt::WidgetWithChildrenShortcut);
    connect(nextMatchAct, &QAction::triggered, this, [this] {
        if (m_search) {
            m_search->focusNextResult();
        }
    });
    auto *prevMatchAct = registerAction(QLatin1String(ActionIds::ViewPreviousSearchMatch),
                                        new QAction(i18n("Previous Search Match"), this),
                                        {QKeySequence(Qt::SHIFT | Qt::Key_F3)});
    prevMatchAct->setShortcutContext(Qt::WidgetWithChildrenShortcut);
    connect(prevMatchAct, &QAction::triggered, this, [this] {
        if (m_search) {
            m_search->focusPrevResult();
        }
    });
    if (m_search) {
        m_search->addAction(nextMatchAct);
        m_search->addAction(prevMatchAct);
    }

    viewMenu->addSeparator();
    const bool termOk = m_terminal && m_terminal->isAvailable();
    auto *newTermAct = registerAction(
        QLatin1String(ActionIds::ViewNewTerminal),
        viewMenu->addAction(QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                            i18n("&New Terminal")),
        {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_T)});
    newTermAct->setEnabled(termOk);
    connect(newTermAct, &QAction::triggered, this, [this] {
        if (!m_terminal) {
            return;
        }
        raisePanelByKey(m_keyTerminal);
        m_terminal->newTerminal();
    });

    auto *focusTermAct = registerAction(
        QLatin1String(ActionIds::ViewFocusTerminal),
        viewMenu->addAction(i18n("&Focus Terminal")),
        {QKeySequence(Qt::CTRL | Qt::Key_QuoteLeft)});
    focusTermAct->setEnabled(termOk);
    connect(focusTermAct, &QAction::triggered, this, [this] {
        if (!m_terminal) {
            return;
        }
        raisePanelByKey(m_keyTerminal);
        m_terminal->focusActiveTerminal();
    });

    auto *nextTermAct = registerAction(
        QLatin1String(ActionIds::ViewNextTerminal),
        viewMenu->addAction(i18n("Next Terminal")),
        {QKeySequence(Qt::CTRL | Qt::Key_PageDown)});
    nextTermAct->setEnabled(termOk);
    connect(nextTermAct, &QAction::triggered, this, [this] {
        if (m_terminal) {
            m_terminal->nextTerminal();
        }
    });

    auto *prevTermAct = registerAction(
        QLatin1String(ActionIds::ViewPreviousTerminal),
        viewMenu->addAction(i18n("Previous Terminal")),
        {QKeySequence(Qt::CTRL | Qt::Key_PageUp)});
    prevTermAct->setEnabled(termOk);
    connect(prevTermAct, &QAction::triggered, this, [this] {
        if (m_terminal) {
            m_terminal->previousTerminal();
        }
    });

    // Text + tooltip track the active agent's isolation (onAgentActivated): the
    // folder this opens is only a "worktree" when the agent has a private copy.
    // Ctrl+Shift+T is "New Terminal" here; Ctrl+Alt+T is free — and now, being a
    // declared default rather than a literal, a user whose desktop already owns
    // Ctrl+Alt+T can move it instead of losing the action.
    m_openWorktreeTerminalAct = registerAction(
        QLatin1String(ActionIds::ViewAgentTerminal),
        viewMenu->addAction(QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                            AgentActions::terminalActionLabel(/*isolated=*/false)),
        {QKeySequence(Qt::CTRL | Qt::ALT | Qt::Key_T)});
    m_openWorktreeTerminalAct->setToolTip(
        AgentActions::terminalActionTooltip(/*isolated=*/false));
    m_openWorktreeTerminalAct->setEnabled(false);
    connect(m_openWorktreeTerminalAct, &QAction::triggered, this, [this] {
        const QString dir =
            m_agent ? m_agent->worktreePathForAgent(m_activeAgentId) : QString();
        if (m_terminal && !dir.isEmpty()) {
            raisePanelByKey(m_keyTerminal);
            m_terminal->openTerminalAt(dir);
        }
    });

    // The developer-only View actions are hidden in Simple mode. Collect them
    // now that all of them exist (terminals, worktree terminal, git blame), plus
    // the Agent menu's git/worktree lifecycle (built earlier in setupAgentMenu).
    //
    // m_agentMergeAct is deliberately NOT in this list (audit F38). Isolation is
    // on by default and the promise made at the decision point is "changes don't
    // touch my files until I approve" — so getting the changes back is the
    // payoff of the whole flow, not a developer tool. Hiding it in Simple mode
    // left the least technical user with no visible approval button anywhere and
    // no way to answer "where did my changes go?".
    m_advancedActions = {m_blameToggle, newTermAct, focusTermAct, nextTermAct,
                         prevTermAct, m_openWorktreeTerminalAct,
                         m_agentCommitAct, m_agentPrAct,
                         m_agentTerminalAct, m_agentDiscardAct};

    m_codeMenu = menuBar()->addMenu(i18n("&Code"));
    QMenu *codeMenu = m_codeMenu;
    QAction *defAct = registerAction(QLatin1String(ActionIds::CodeGotoDefinition),
                                     codeMenu->addAction(i18n("Go to &Definition")),
                                     {QKeySequence(Qt::Key_F12)});
    connect(defAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->gotoDefinition(view);
        }
    });
    QAction *refAct = registerAction(QLatin1String(ActionIds::CodeFindReferences),
                                     codeMenu->addAction(i18n("Find &References")),
                                     {QKeySequence(Qt::SHIFT | Qt::Key_F12)});
    connect(refAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->findReferences(view);
        }
    });

    QAction *symbolAct = registerAction(
        QLatin1String(ActionIds::CodeGotoSymbol),
        codeMenu->addAction(QIcon::fromTheme(QStringLiteral("code-context")),
                            i18n("Go to &Symbol in Workspace…")),
        {QKeySequence(Qt::CTRL | Qt::Key_T)});
    connect(symbolAct, &QAction::triggered, this, [this] {
        auto *dlg = new WorkspaceSymbolDialog(m_lsp, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        connect(dlg, &WorkspaceSymbolDialog::symbolChosen, this,
                [this](const QString &path, int line) {
                    ensureEditorVisible();
                    m_editor->openFile(groupKey(), path, line);
                });
        dlg->show();
    });

    codeMenu->addSeparator();

    QAction *quickFixAct = registerAction(
        QLatin1String(ActionIds::CodeQuickFix),
        codeMenu->addAction(QIcon::fromTheme(QStringLiteral("tools-wizard")),
                            i18n("&Quick Fix…")),
        {QKeySequence(Qt::CTRL | Qt::Key_Period)});
    connect(quickFixAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->requestCodeActions(view);
        }
    });

    QAction *renameAct = registerAction(QLatin1String(ActionIds::CodeRenameSymbol),
                                        codeMenu->addAction(i18n("Rena&me Symbol")),
                                        {QKeySequence(Qt::Key_F2)});
    connect(renameAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->renameSymbol(view);
        }
    });

    QAction *formatAct = registerAction(QLatin1String(ActionIds::CodeFormatDocument),
                                        codeMenu->addAction(i18n("&Format Document")),
                                        {QKeySequence(Qt::CTRL | Qt::ALT | Qt::Key_L)});
    connect(formatAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->formatDocument(view);
        }
    });

    m_formatOnSave = registerAction(QLatin1String(ActionIds::CodeFormatOnSave),
                                    codeMenu->addAction(i18n("Format on &Save")));
    m_formatOnSave->setCheckable(true);
    m_formatOnSave->setChecked(KSharedConfig::openConfig()
                                   ->group(QStringLiteral("CodeIntelligence"))
                                   .readEntry("formatOnSave", false));
    connect(m_formatOnSave, &QAction::toggled, this, [](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("CodeIntelligence"))
            .writeEntry("formatOnSave", on);
    });

    QAction *sigAct = registerAction(QLatin1String(ActionIds::CodeSignatureHelp),
                                     codeMenu->addAction(i18n("Show Signature &Help")),
                                     {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_Space)});
    connect(sigAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->requestSignatureHelp(view);
        }
    });

    QAction *nextProbAct = registerAction(QLatin1String(ActionIds::CodeNextProblem),
                                          codeMenu->addAction(i18n("&Next Problem")),
                                          {QKeySequence(Qt::Key_F8)});
    connect(nextProbAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->nextProblem(view);
        }
    });

    QAction *prevProbAct = registerAction(QLatin1String(ActionIds::CodePreviousProblem),
                                          codeMenu->addAction(i18n("&Previous Problem")),
                                          {QKeySequence(Qt::SHIFT | Qt::Key_F8)});
    connect(prevProbAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->prevProblem(view);
        }
    });

    QAction *restartAct = registerAction(
        QLatin1String(ActionIds::CodeRestartLanguageServer),
        codeMenu->addAction(QIcon::fromTheme(QStringLiteral("view-refresh")),
                            i18n("&Restart Language Server")));
    connect(restartAct, &QAction::triggered, this, [this] {
        if (!m_activeFilePath.isEmpty()) {
            m_lsp->restartServersForCurrentFile(m_activeFilePath);
        }
    });

    // Skills and language extensions used to live at the bottom of the Code
    // menu, which Simple mode hides whole — so the two "install something that
    // makes your agent better at a job" features were invisible to exactly the
    // person they are for (audit F50). They are not editor navigation, so they
    // move out rather than being duplicated: skills to the Agent menu (they are
    // an agent capability), language extensions to Options (they configure the
    // editor). Both menus are visible at every experience level.
    auto *extAct = registerAction(
        QLatin1String(ActionIds::OptionsLanguageExtensions),
        new QAction(QIcon::fromTheme(QStringLiteral("install")),
                    i18n("Manage Language &Extensions…"), this));
    extAct->setToolTip(i18n("Add language support — syntax, completion and error "
                            "checking for more file types."));
    connect(extAct, &QAction::triggered, this, [this] {
        auto *dlg = new ExtensionsDialog(m_core, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        connect(dlg, &ExtensionsDialog::extensionsChanged, this,
                &MainWindow::reloadExtensionServers);
        dlg->show();
    });
    optionsMenu->addSeparator();
    optionsMenu->addAction(extAct);

    auto *skillsAct = registerAction(
        QLatin1String(ActionIds::AgentManageSkills),
        new QAction(QIcon::fromTheme(QStringLiteral("preferences-plugin")),
                    i18n("Manage Agent &Skills…"), this));
    skillsAct->setToolTip(
        i18n("Install skills — reusable instructions that teach your agents how "
             "to do a particular job."));
    connect(skillsAct, &QAction::triggered, this, [this] {
        if (m_activeProject.isEmpty()) {
            return;
        }
        auto *dlg = new SkillsDialog(m_core, m_activeProject, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        dlg->show();
    });
    if (m_agentMenu) {
        m_agentMenu->addSeparator();
        m_agentMenu->addAction(skillsAct);
    }

    // Settings ▸ Configure Shortcuts, the surface the whole collection exists
    // for. It goes last in Options so it sits below the things it configures.
    optionsMenu->addSeparator();
    optionsMenu->addAction(registerAction(
        QLatin1String(ActionIds::OptionsConfigureKeyBinding),
        KStandardAction::keyBindings(this, &MainWindow::configureShortcuts, this)));

    auto *helpMenu = new KHelpMenu(this, KAboutData::applicationData());
    menuBar()->addMenu(helpMenu->menu());
}

// updateEngineAvailabilityBanner puts the missing-CLI condition on screen
// BEFORE the user writes a task, instead of after (audit F37). It fires only
// for the state that actually strands someone — not one engine installed;
// a single missing engine out of several is said in the picker instead
// (AgentDock::seedEngineChoices), where the choice is being made.
void MainWindow::updateEngineAvailabilityBanner()
{
    if (!m_engineBanner) {
        return;
    }
    const QList<EngineAvailability::Engine> engines = EngineAvailability::scan();
    const QString message = EngineAvailability::missingEnginesMessage(engines);
    if (message.isEmpty()) {
        m_engineBanner->animatedHide();
        return;
    }
    // Rebuild the action every time: the set of missing engines (and so the
    // link we can honestly offer) can change between calls.
    const auto actions = m_engineBanner->actions();
    for (QAction *a : actions) {
        m_engineBanner->removeAction(a);
        // removeAction only DETACHES: the action stays parented to the banner
        // and every refresh added another "How to Install…" that nothing would
        // ever free. deleteLater rather than delete because this can run from
        // inside the banner's own action handling (audit F50, leak hygiene).
        a->deleteLater();
    }
    m_engineBanner->setMessageType(KMessageWidget::Warning);
    m_engineBanner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
    m_engineBanner->setCloseButtonVisible(false);
    m_engineBanner->setText(message);
    const QString url = EngineAvailability::installUrl(engines);
    if (!url.isEmpty()) {
        auto *install = new QAction(QIcon::fromTheme(QStringLiteral("install")),
                                    i18n("How to Install…"), m_engineBanner);
        // An app-authored constant URL — nothing model-controlled reaches here,
        // so it does not need SafeContent's confirm-and-scheme gate (audit F14).
        connect(install, &QAction::triggered, this,
                [url] { QDesktopServices::openUrl(QUrl(url)); });
        m_engineBanner->addAction(install);
    }
    m_engineBanner->animatedShow();
}

void MainWindow::refreshWindowTitle()
{
    setWindowTitle(WindowTitle::compose(m_titleProject, m_attentionCount));
}

void MainWindow::showCommandPalette()
{
    if (!m_commandPalette) {
        m_commandPalette = new CommandPalette(this);
    }
    // THE COLLECTION FIRST, not the menu bar (plan 27 §1). The old walk started
    // from menuBar() and could therefore only ever list what was in a menu: the
    // twenty rail accelerators, the three centre-mode toolbar buttons and every
    // panel-local command were absent, and CommandPalette's own header claimed
    // it listed "every command in the application" while listing a fraction.
    // The collection is the registry those actions are now born into.
    QList<CommandPalette::Entry> entries;
    QSet<QAction *> seen;

    // Simple mode hides commands two ways: individually (m_advancedActions) and
    // wholesale (the Code menu's menuAction, which leaves its CHILDREN visible).
    // Both are "advanced" to the palette, which lists them tagged rather than
    // dropping them — see CommandPalette::Entry.
    QSet<QAction *> advanced;
    for (QAction *a : std::as_const(m_advancedActions)) {
        if (a) {
            advanced.insert(a);
        }
    }
    const bool codeMenuHidden =
        m_codeMenu && !m_codeMenu->menuAction()->isVisible();
    if (codeMenuHidden) {
        const auto codeActions = m_codeMenu->actions();
        for (QAction *a : codeActions) {
            advanced.insert(a);
        }
    }

    // Whether a command can actually run. For a visible action the action is
    // the authority. For a HIDDEN one it is not: QAction::setVisible(false)
    // clears `enabled` as a side effect and setVisible(true) recomputes it from
    // the enablement the application really asked for — so the truth is in
    // there, just not readable. Flip it back for the length of the question,
    // with signals blocked so no menu relayouts and no handler observes the
    // flicker. Getting this wrong in the permissive direction would let the
    // palette run Create Pull Request on an agent with no branch, which is
    // exactly the gate AgentActions::compute exists to hold.
    const auto available = [](QAction *a) {
        if (a->isVisible()) {
            return a->isEnabled();
        }
        const QSignalBlocker block(a);
        a->setVisible(true);
        const bool ok = a->isEnabled();
        a->setVisible(false);
        return ok;
    };

    const auto collected = m_actions->actions();
    for (QAction *a : collected) {
        if (!a || seen.contains(a)) {
            continue;
        }
        seen.insert(a);
        entries.append({a, QString(), advanced.contains(a), available(a)});
    }

    // Then the menu-bar walk, merged. It is not redundant: KHelpMenu builds its
    // own actions and hands us a menu, never a collection, so About / Handbook /
    // Report Bug reach the palette only this way.
    std::function<void(QMenu *)> walk = [&](QMenu *menu) {
        for (QAction *a : menu->actions()) {
            if (a->isSeparator()) {
                continue;
            }
            if (a->menu()) {
                walk(a->menu());
                continue;
            }
            if (!seen.contains(a)) {
                seen.insert(a);
                entries.append({a, QString(), advanced.contains(a), available(a)});
            }
        }
    };
    const auto topLevel = menuBar()->actions();
    for (QAction *top : topLevel) {
        if (top->menu()) {
            walk(top->menu());
        } else if (!top->isSeparator() && !seen.contains(top)) {
            seen.insert(top);
            entries.append({top, QString(), advanced.contains(top), available(top)});
        }
    }

    // Finally the commands panels published for themselves (registerCommands).
    // Guarded pointers: a panel destroyed since it registered simply drops out.
    for (const PanelCommand &pc : std::as_const(m_panelCommands)) {
        QAction *a = pc.action.data();
        if (!a || seen.contains(a)) {
            continue;
        }
        seen.insert(a);
        entries.append({a, pc.group, false, available(a)});
    }

    m_commandPalette->setActions(entries);
    m_commandPalette->showPalette();
}

// setupAgentMenu builds the &Agent menu and wires every entry to AgentDock's
// active-agent surface. Enable-state tracks the active agent (updateAgentActions).
void MainWindow::setupAgentMenu()
{
    m_agentMenu = menuBar()->addMenu(i18n("&Agent"));

    auto *newAct = registerAction(
        QLatin1String(ActionIds::AgentNew),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("list-add")),
                               i18n("&New Agent")));
    newAct->setToolTip(i18n("Describe a task and start a fresh agent in the current project."));
    connect(newAct, &QAction::triggered, this,
            [this] { m_agent->newAgentInActiveProjectGuided(); });

    m_agentRenameAct = registerAction(
        QLatin1String(ActionIds::AgentRename),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("document-edit")),
                               i18n("&Rename Agent…")));
    connect(m_agentRenameAct, &QAction::triggered, this,
            [this] { m_agent->renameActiveAgent(); });

    m_agentResumeAct = registerAction(
        QLatin1String(ActionIds::AgentResume),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("media-playback-start")),
                               i18n("Res&ume Agent")));
    m_agentResumeAct->setToolTip(i18n("Relaunch a paused agent and continue its conversation."));
    connect(m_agentResumeAct, &QAction::triggered, this,
            [this] { m_agent->resumeActiveAgent(); });

    m_agentMenu->addSeparator();

    m_agentAttachAct = registerAction(
        QLatin1String(ActionIds::AgentAttachFiles),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("mail-attachment")),
                               i18n("&Attach Files…")));
    m_agentAttachAct->setToolTip(i18n("Give the active agent files as context for its next message."));
    connect(m_agentAttachAct, &QAction::triggered, this,
            [this] { m_agent->attachToActiveAgent(); });

    m_agentChangesAct = registerAction(
        QLatin1String(ActionIds::AgentShowChanges),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("vcs-diff")),
                               i18n("Show &Changes")));
    m_agentChangesAct->setToolTip(i18n("Review the changes the active agent has made."));
    connect(m_agentChangesAct, &QAction::triggered, this,
            [this] { m_agent->showActiveAgentChanges(); });

    // Review, then approve. This one is NOT part of the git/worktree section
    // below and is not hidden in Simple mode (audit F38): an isolated agent's
    // work is unreachable without it, so it belongs next to "Show Changes"
    // where the user has just looked at what they are approving.
    //
    // It is no longer called "Merge into Local Main": agent.land merges into
    // the workspace's CURRENT branch, whatever that is (audit F50).
    m_agentMergeAct = registerAction(
        QLatin1String(ActionIds::AgentMergeChanges),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("vcs-merge")),
                               i18n("&Merge the Agent's Changes…")));
    m_agentMergeAct->setToolTip(
        i18n("Bring this agent's work out of its private copy and into your "
             "project's current branch."));
    connect(m_agentMergeAct, &QAction::triggered, this,
            [this] { m_agent->mergeActiveAgent(); });

    m_agentStopAct = registerAction(
        QLatin1String(ActionIds::AgentStop),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("process-stop")),
                               i18n("&Stop Agent")));
    m_agentStopAct->setToolTip(i18n("Stop the running agent (it stays available to resume)."));
    connect(m_agentStopAct, &QAction::triggered, this,
            [this] { m_agent->stopActiveAgent(); });

    m_agentMenu->addSeparator();

    // Git / worktree lifecycle — hidden in Simple mode (added to m_advancedActions
    // in setupActions, which runs after this).
    m_agentCommitAct = registerAction(QLatin1String(ActionIds::AgentCommit),
                                      m_agentMenu->addAction(i18n("&Commit Changes…")));
    connect(m_agentCommitAct, &QAction::triggered, this,
            [this] { m_agent->commitActiveAgent(); });
    m_agentPrAct = registerAction(QLatin1String(ActionIds::AgentCreatePullRequest),
                                  m_agentMenu->addAction(i18n("Create &Pull Request…")));
    connect(m_agentPrAct, &QAction::triggered, this,
            [this] { m_agent->createPullRequestForActiveAgent(); });
    // Text + tooltip are re-derived per agent in updateAgentActions (the folder
    // is only a "worktree" when the agent is isolated). The COLLECTION ID does
    // not change with the label — a user who rebinds this keeps that binding
    // when they switch to a workspace-mode agent and the wording changes.
    m_agentTerminalAct = registerAction(
        QLatin1String(ActionIds::AgentOpenTerminal),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                               AgentActions::terminalActionLabel(/*isolated=*/false)));
    connect(m_agentTerminalAct, &QAction::triggered, this,
            [this] { m_agent->openActiveAgentTerminal(); });

    m_agentMenu->addSeparator();

    m_agentTagsAct = registerAction(
        QLatin1String(ActionIds::AgentEditTags),
        m_agentMenu->addAction(QIcon::fromTheme(QStringLiteral("tag")),
                               i18n("Edit &Tags…")));
    connect(m_agentTagsAct, &QAction::triggered, this,
            [this] { m_agent->editActiveAgentTags(); });

    // Same rule as the terminal action: the label swings between "Discard
    // Worktree" and "Delete Agent" with the agent's isolation, the id does not.
    m_agentDiscardAct = registerAction(
        QLatin1String(ActionIds::AgentDiscard),
        m_agentMenu->addAction(AgentActions::discardActionLabel(/*isolated=*/false)));
    m_agentDiscardAct->setToolTip(
        AgentActions::discardActionTooltip(/*isolated=*/false));
    connect(m_agentDiscardAct, &QAction::triggered, this,
            [this] { m_agent->discardActiveAgentWorktree(); });

    m_agentCloseAct = registerAction(QLatin1String(ActionIds::AgentClose),
                                     m_agentMenu->addAction(i18n("&Close Agent")));
    connect(m_agentCloseAct, &QAction::triggered, this,
            [this] { m_agent->closeActiveAgent(); });

    // Keep enable-state fresh whenever the menu opens (covers state changes the
    // signal-based refresh might miss, e.g. a turn finishing).
    connect(m_agentMenu, &QMenu::aboutToShow, this, &MainWindow::updateAgentActions);
    updateAgentActions(); // start in the right enable-state (no active agent yet)
}

void MainWindow::updateAgentActions()
{
    if (!m_agentRenameAct) {
        return; // menu not built yet
    }
    const bool has = m_agent && m_agent->hasActiveAgent();
    const bool running = m_agent && m_agent->activeAgentRunning();
    const bool dormant = m_agent && m_agent->activeAgentDormant();
    const bool worktree = m_agent && m_agent->activeAgentHasWorktree();
    // Merge needs a private BRANCH, not merely a path. git.snapshot hands back a
    // path for workspace-mode threads too, so `worktree` was true for an agent
    // running in the user's own checkout and "Merge Agent's Changes" offered to
    // merge a branch that does not exist. AgentDock::landRequested already
    // refuses that case; this stops the product from offering it in the first
    // place, rather than explaining afterwards.
    const bool isolated = m_agent && m_agent->activeAgentIsolated();
    // One decision, shared with the roster's right-click menu (which is how
    // most users reach these) so the two surfaces cannot disagree — and, unlike
    // this file, testable: see AgentActions.h / AgentRosterTest.
    const AgentActions::AgentActionEnablement en =
        AgentActions::compute(has, running, dormant, isolated, worktree);
    m_agentRenameAct->setEnabled(en.rename);
    m_agentResumeAct->setEnabled(en.resume);
    m_agentAttachAct->setEnabled(en.attach);
    m_agentChangesAct->setEnabled(en.changes);
    m_agentStopAct->setEnabled(en.stop);
    m_agentCommitAct->setEnabled(en.commit);
    // A pull request needs the same private BRANCH merging does: the core
    // refuses OpenPRWithOptions outright when the thread is not isolated
    // (worktree.go, "!wt.Isolated"), so enabling this on a workspace-mode agent
    // offers an action that cannot succeed.
    m_agentPrAct->setEnabled(en.pullRequest);
    m_agentMergeAct->setEnabled(en.merge);
    // A greyed-out item with no reason is its own small dead end. The one
    // state worth naming is the one a user will not guess: the agent IS
    // started and does have changes, it simply has no branch of its own.
    m_agentMergeAct->setToolTip(
        worktree && !isolated
            ? i18n("This agent works directly in your project, so it has no "
                   "separate branch to merge.")
            : i18n("Bring this agent's work out of its private copy and into "
                   "your project's current branch."));
    m_agentTerminalAct->setEnabled(en.terminal);
    // "Open Terminal in Worktree" opens the user's OWN checkout when the agent
    // has no private copy — the action is right, the name was not.
    m_agentTerminalAct->setText(AgentActions::terminalActionLabel(isolated));
    m_agentTerminalAct->setToolTip(AgentActions::terminalActionTooltip(isolated));
    m_agentTagsAct->setEnabled(en.tags);
    m_agentDiscardAct->setEnabled(en.discard);
    m_agentDiscardAct->setText(AgentActions::discardActionLabel(isolated));
    m_agentDiscardAct->setToolTip(AgentActions::discardActionTooltip(isolated));
    m_agentCloseAct->setEnabled(en.close);
}

// setupExperience installs the status-bar Simple/Advanced toggle and applies the
// saved (or first-run default) level. A brand-new profile starts in Simple so a
// non-developer isn't met with a wall of code tooling on first launch.
void MainWindow::setupExperience()
{
    m_experienceButton = new QToolButton(this);
    m_experienceButton->setAutoRaise(true);
    m_experienceButton->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_experienceButton->setIcon(QIcon::fromTheme(QStringLiteral("games-difficult")));
    m_experienceButton->setToolTip(i18n("Switch between Simple (just the essentials) "
                                        "and Advanced (every developer tool)."));
    connect(m_experienceButton, &QToolButton::clicked, this,
            &MainWindow::toggleExperienceLevel);
    statusBar()->insertPermanentWidget(0, m_experienceButton);

    KConfigGroup grp = KSharedConfig::openConfig()->group(QStringLiteral("Experience"));
    QString level = grp.readEntry("level", QString());
    const bool wasUnset = level.isEmpty();
    if (wasUnset) {
        level = m_firstRunProfile ? QStringLiteral("simple") : QStringLiteral("advanced");
    }
    // Persist a computed first-run default so it can't flip once the View schema
    // bumps and m_firstRunProfile would read false next launch.
    applyExperienceLevel(level, /*persist=*/wasUnset);
}

void MainWindow::applyExperienceLevel(const QString &level, bool persist)
{
    const bool simple = (level == QLatin1String("simple"));
    m_experienceLevel = simple ? QStringLiteral("simple") : QStringLiteral("advanced");

    // Hide the Code menu and the developer-only View actions in Simple mode.
    if (m_codeMenu) {
        m_codeMenu->menuAction()->setVisible(!simple);
    }
    for (QAction *a : m_advancedActions) {
        if (a) {
            a->setVisible(!simple);
        }
    }
    // Hide the developer-only panels. The essentials — Projects & Agents, Files,
    // Search and Cowork — always stay on the rail.
    const QStringList advancedPanels = {
        m_keyOutline, m_keyWorktrees, m_keyGitLog, m_keyCoop, m_keyInspector,
        m_keyTerminal, m_keyReferences, m_keyProblems, m_keyOutput, m_keyTasks};
    for (const QString &key : advancedPanels) {
        setPanelTabVisible(key, !simple);
    }

    if (m_simpleAct) m_simpleAct->setChecked(simple);
    if (m_advancedAct) m_advancedAct->setChecked(!simple);
    if (m_experienceButton) {
        m_experienceButton->setText(simple ? i18n("Simple") : i18n("Advanced"));
    }

    if (persist) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Experience"))
            .writeEntry("level", m_experienceLevel);
    }
    statusBar()->showMessage(
        simple ? i18n("Simple mode — just the essentials. Switch to Advanced for the developer tools.")
               : i18n("Advanced mode — every tool and panel is available."),
        5000);
}

void MainWindow::toggleExperienceLevel()
{
    applyExperienceLevel(m_experienceLevel == QLatin1String("simple")
                             ? QStringLiteral("advanced")
                             : QStringLiteral("simple"));
}

void MainWindow::setPanelTabVisible(const QString &key, bool visible)
{
    SideBar *bar = panelBar(key);
    const int id = panelId(key);
    if (!bar || id < 0) {
        return;
    }
    if (auto *tab = bar->tabBar()->tab(id)) {
        tab->setVisible(visible);
        // If we just hid the raised panel, collapse the strip so we don't leave
        // an orphaned panel area open with no tab to toggle it.
        if (!visible && bar->raisedId() == id) {
            bar->setRaisedId(-1);
        }
    }
}

// setupShellShortcuts wires the JetBrains-style raise-by-ordinal accelerators
// for the left/right strips, plus Ctrl+E / Ctrl+Shift+E perspective toggles.
//
// These were raw QShortcuts until plan 27 §1. A QShortcut is invisible to every
// configuration surface there is — it has no name, no text, no icon and no entry
// in any collection — so twenty of this application's bindings could not be
// listed, searched, rebound or even discovered, and the only thing that ever
// said they existed was a hand-built tooltip. They are ordinary named actions
// now; each is added to the window as well as the collection, because an action
// in no menu needs a widget to make its shortcut live.
void MainWindow::setupShellShortcuts()
{
    auto bindRaise = [this](SideBar *bar, bool leftBar, Qt::KeyboardModifiers mods) {
        const QString stripName = leftBar
            ? i18nc("@item the left activity rail", "Left Sidebar")
            : i18nc("@item the right activity rail", "Right Sidebar");
        for (int i = 0; i < ActionIds::kRailOrdinals; ++i) {
            auto *act = registerAction(
                ActionIds::railRaise(leftBar, i + 1),
                new QAction(i18nc("@action raise the Nth panel of a sidebar",
                                  "Raise %1 Panel %2", stripName, i + 1),
                            this),
                {QKeySequence(QKeyCombination(mods,
                                              static_cast<Qt::Key>(Qt::Key_1 + i)))});
            connect(act, &QAction::triggered, this, [bar, i] {
                const int id = bar->panelIdAt(i);
                if (id >= 0) {
                    bar->setRaisedId(bar->raisedId() == id ? -1 : id);
                }
            });
            addAction(act);
        }
        auto *collapse = registerAction(
            ActionIds::railCollapse(leftBar),
            new QAction(i18nc("@action collapse a whole sidebar",
                              "Collapse %1", stripName), this),
            {QKeySequence(QKeyCombination(mods, Qt::Key_0))});
        connect(collapse, &QAction::triggered, this,
                [bar] { bar->setRaisedId(-1); });
        addAction(collapse);
    };
    bindRaise(m_leftBar, /*leftBar=*/true, Qt::AltModifier);
    bindRaise(m_rightBar, /*leftBar=*/false,
              Qt::ControlModifier | Qt::AltModifier);

    // Ctrl+E toggles between editor-only and split; Ctrl+Shift+E toggles
    // between chat-only and split. Both route through applyCentreMode so the
    // top-toolbar buttons stay in sync.
    auto *focusEditor = registerAction(
        QLatin1String(ActionIds::ViewFocusEditor),
        new QAction(i18nc("@action", "Focus the Editor"), this),
        {QKeySequence(Qt::CTRL | Qt::Key_E)});
    connect(focusEditor, &QAction::triggered, this, [this] {
        applyCentreMode(m_centreMode == QLatin1String("editor")
                            ? QStringLiteral("split")
                            : QStringLiteral("editor"));
    });
    addAction(focusEditor);
    auto *focusAgent = registerAction(
        QLatin1String(ActionIds::ViewFocusAgent),
        new QAction(i18nc("@action", "Focus the Agent Conversation"), this),
        {QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_E)});
    connect(focusAgent, &QAction::triggered, this, [this] {
        applyCentreMode(m_centreMode == QLatin1String("chat")
                            ? QStringLiteral("split")
                            : QStringLiteral("chat"));
    });
    addAction(focusAgent);
}

// setupPerspectives adds a small menu of named layout snapshots — Code Focus,
// Chat Focus, and Reset — that the hamburger surfaces under "View ▸ Perspective".
void MainWindow::setupPerspectives()
{
    // Find the existing &View menu; the perspectives submenu becomes its
    // child so destruction order is well-defined. Earlier this menu was
    // parented to `this`, which (because m_perspectivesMenu was constructed
    // AFTER the menubar) made the menu die BEFORE the QAction inside View
    // that referenced it — the View menu's internal action hash would then
    // visit a dangling child during teardown, hitting Q_ASSERT(numBuckets>0)
    // inside QHash on shutdown.
    QMenu *viewMenu = nullptr;
    for (QAction *a : menuBar()->actions()) {
        if (a->text() == i18n("&View") && a->menu()) {
            viewMenu = a->menu();
            break;
        }
    }
    if (!viewMenu) {
        return; // no View menu — nothing to attach to
    }
    m_perspectivesMenu = new QMenu(i18n("&Layout"), viewMenu);
    m_perspectivesMenu->setIcon(QIcon::fromTheme(QStringLiteral("view-multiple-objects")));
    auto add = [this](const QString &id, const QString &label, const QString &key,
                      const QString &tip, const QKeySequence &shortcut) {
        QAction *act = registerAction(id, m_perspectivesMenu->addAction(label),
                                      {shortcut});
        act->setToolTip(tip);
        connect(act, &QAction::triggered, this, [this, key] { applyPerspective(key); });
    };
    add(QLatin1String(ActionIds::LayoutConverse), i18n("&Converse"),
        QStringLiteral("converse"),
        i18n("Focus the conversation with your agent."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_1);
    add(QLatin1String(ActionIds::LayoutBuild), i18n("&Build"),
        QStringLiteral("build"),
        i18n("Focus the code editor and files."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_2);
    add(QLatin1String(ActionIds::LayoutReview), i18n("&Review"),
        QStringLiteral("review"),
        i18n("Editor and agent side by side, with changes and history."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_3);
    add(QLatin1String(ActionIds::LayoutSplit), i18n("&Side by Side"),
        QStringLiteral("split"),
        i18n("A balanced split of the editor and the agent."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_4);

    viewMenu->addSeparator();
    viewMenu->addMenu(m_perspectivesMenu);
}

QString MainWindow::layoutDisplayName(const QString &key)
{
    if (key == QLatin1String("converse")) return i18n("Converse");
    if (key == QLatin1String("build")) return i18n("Build");
    if (key == QLatin1String("review")) return i18n("Review");
    if (key == QLatin1String("split")) return i18n("Side by Side");
    return key;
}

void MainWindow::applyPerspective(const QString &name)
{
    if (!m_shell) {
        return;
    }
    // Only raise a panel whose tab is actually visible — Simple mode hides the
    // developer panels, and a layout must not drag a hidden one back on screen.
    auto raiseIfShown = [this](const QString &key) {
        SideBar *bar = panelBar(key);
        const int id = panelId(key);
        if (!bar || id < 0) {
            return;
        }
        if (auto *tab = bar->tabBar()->tab(id); tab && tab->isVisible()) {
            raisePanelByKey(key);
        }
    };

    // Route the centre split through applyCentreMode so the Editor/Split/Chat
    // toggles and the persisted centre mode stay in sync with the layout.
    if (name == QLatin1String("converse")) {
        applyCentreMode(QStringLiteral("chat"));
        raiseIfShown(m_keyRoster);
        if (m_rightBar) m_rightBar->setRaisedId(-1);
        if (m_bottomBar) m_bottomBar->setRaisedId(-1);
        if (m_agent) {
            if (auto *p = m_agent->activePanel()) p->setFocus();
        }
    } else if (name == QLatin1String("build")) {
        applyCentreMode(QStringLiteral("editor"));
        raiseIfShown(m_keyFiles);
        if (m_rightBar) m_rightBar->setRaisedId(-1);
        if (m_bottomBar) m_bottomBar->setRaisedId(-1);
        if (auto *v = m_editor ? m_editor->currentView() : nullptr) v->setFocus();
    } else if (name == QLatin1String("review")) {
        applyCentreMode(QStringLiteral("split"));
        if (m_leftBar) m_leftBar->setRaisedId(-1);
        raiseIfShown(m_keyGitLog);
        raiseIfShown(m_keyProblems);
        if (auto *h = m_shell->centreHSplitter()) h->setSizes({650, 550});
    } else { // "split" — a balanced side-by-side, and the default reset
        applyCentreMode(QStringLiteral("split"));
        raiseIfShown(m_keyRoster);
        raiseIfShown(m_keyWorktrees);
        if (m_bottomBar) m_bottomBar->setRaisedId(-1);
        if (auto *h = m_shell->centreHSplitter()) h->setSizes({700, 500});
    }
    if (m_layoutButton) {
        m_layoutButton->setText(layoutDisplayName(name));
    }
    statusBar()->showMessage(i18n("Layout: %1", layoutDisplayName(name)), 3000);
}

SideBar *MainWindow::barByName(const QString &name) const
{
    if (name == QLatin1String("left")) return m_leftBar;
    if (name == QLatin1String("right")) return m_rightBar;
    if (name == QLatin1String("bottom")) return m_bottomBar;
    return nullptr;
}

QString MainWindow::nameForBar(SideBar *bar) const
{
    if (bar == m_leftBar) return QStringLiteral("left");
    if (bar == m_rightBar) return QStringLiteral("right");
    if (bar == m_bottomBar) return QStringLiteral("bottom");
    return QString();
}

int MainWindow::panelId(const QString &key) const
{
    return m_panels.value(key).barId;
}

SideBar *MainWindow::panelBar(const QString &key) const
{
    return m_panels.value(key).bar;
}

void MainWindow::raisePanelByKey(const QString &key)
{
    auto it = m_panels.find(key);
    if (it == m_panels.end()) {
        return;
    }
    // If currently floating, bring the host window forward; otherwise raise
    // the tab on its current strip.
    if (it->floatingHost) {
        it->floatingHost->raise();
        it->floatingHost->activateWindow();
        return;
    }
    if (it->bar && it->barId >= 0) {
        it->bar->setRaisedId(it->barId);
    }
}

// registerPanel places a panel on its persisted strip (or the supplied
// default) and records its location so movePanelToStrip can move it later.
int MainWindow::registerPanel(const QString &key, const QIcon &icon,
                              const QString &label, QWidget *widget,
                              const QString &defaultStrip)
{
    const QString cfgKey = QStringLiteral("View/panels/%1/strip").arg(key);
    const QString strip = KSharedConfig::openConfig()->group(QString())
        .readEntry(cfgKey, defaultStrip);

    PanelInfo info;
    info.key = key;
    info.icon = icon;
    info.label = label;
    info.widget = widget;
    info.lastStrip = (strip == QLatin1String("floating")) ? defaultStrip : strip;

    SideBar *bar = barByName(info.lastStrip);
    if (!bar) bar = barByName(defaultStrip);
    if (bar) {
        info.bar = bar;
        info.barId = bar->addPanel(icon, label, widget);
    }
    m_panels.insert(key, info);
    m_keyByWidget.insert(widget, key);

    if (strip == QLatin1String("floating")) {
        // Defer the detach so all panels register first.
        QTimer::singleShot(0, this, [this, key] { detachPanel(key); });
    }
    return info.barId;
}

// refreshPanelTooltips rebuilds every rail tab's hover text: what the panel is
// for, plus the accelerator that raises it.
//
// PLAN 27 §1 CONFIRMATION. The old comment here said the KActionCollection
// refactor would "take this whole method with it". Half of that is right and
// half is not, so only half was done:
//
//   * The BINDING HINT was the interim answer to raw QShortcuts being invisible
//     (audit F50), and the collection does supersede it as the mechanism —
//     which is why the sequence is now read back off the registered action
//     instead of being recomputed from Alt+ordinal. That matters: a user who
//     rebinds Alt+3 in Settings ▸ Configure Shortcuts used to get a tooltip
//     confidently naming the binding they had just replaced.
//   * The DESCRIPTION ("what this panel is for", PanelInfo::help) is not
//     superseded by anything. A collection lists commands; it has no concept of
//     a panel and nothing in it explains what the Cowork rail tab does. Deleting
//     help would delete the one thing that makes an icon-only rail readable to a
//     newcomer, in exchange for nothing. So it stays.
//
// It has to be a method rather than a one-shot at construction because the
// ordinal is a POSITION in the strip: moving or detaching one panel renumbers
// every panel after it, and a tooltip that then names the old key would be
// worse than naming none.
void MainWindow::refreshPanelTooltips()
{
    const auto railShortcut = [this](SideBar *bar, int id) -> QString {
        if (!m_actions || (bar != m_leftBar && bar != m_rightBar)) {
            return QString(); // no collection yet, or the bottom strip (unbound)
        }
        int index = -1;
        for (int i = 0; i < bar->panelCount(); ++i) {
            if (bar->panelIdAt(i) == id) {
                index = i;
                break;
            }
        }
        if (index < 0 || index >= ActionIds::kRailOrdinals) {
            return QString(); // bindRaise only binds the first kRailOrdinals
        }
        // The ACTIVE sequence, not the default: this text exists to tell the
        // truth about the key that works right now.
        QAction *act =
            m_actions->action(ActionIds::railRaise(bar == m_leftBar, index + 1));
        return act ? act->shortcut().toString(QKeySequence::NativeText) : QString();
    };
    for (auto it = m_panels.constBegin(); it != m_panels.constEnd(); ++it) {
        if (it->help.isEmpty() || !it->bar || it->barId < 0) {
            continue; // no description, or currently floating
        }
        auto *tab = it->bar->tabBar()->tab(it->barId);
        if (!tab) {
            continue;
        }
        QString title = it->bar->panelLabel(it->barId);
        title.replace(QLatin1String("&&"), QLatin1String("&"));
        QString tip = QStringLiteral("<b>%1</b><p>%2</p>")
                          .arg(title.toHtmlEscaped(), it->help.toHtmlEscaped());
        const QString seq = railShortcut(it->bar, it->barId);
        if (!seq.isEmpty()) {
            tip += QStringLiteral("<p>%1</p>")
                       .arg(i18nc("keyboard shortcut hint in a panel tooltip",
                                  "Shortcut: %1", seq)
                                .toHtmlEscaped());
        }
        tab->setToolTip(tip);
    }
}

void MainWindow::movePanelToStrip(const QString &key, const QString &targetStrip)
{
    auto it = m_panels.find(key);
    if (it == m_panels.end()) {
        return;
    }
    SideBar *target = barByName(targetStrip);
    if (!target) {
        return;
    }
    // Pull from the current host (sidebar or floating window) preserving
    // the widget's metadata, then re-add to the target strip.
    QWidget *widget = it->widget;
    QIcon icon = it->icon;
    QString label = it->label;
    if (it->bar) {
        auto meta = it->bar->takePanel(it->barId);
        if (meta.widget) {
            widget = meta.widget;
            icon = meta.icon;
            label = meta.label;
        }
        it->bar = nullptr;
        it->barId = -1;
    }
    if (it->floatingHost) {
        // The floating wrapper owned the widget; lift it out before deleting
        // the host. (deleteLater here would also destroy the widget.)
        widget->setParent(nullptr);
        it->floatingHost->deleteLater();
        it->floatingHost = nullptr;
    }
    it->bar = target;
    it->barId = target->addPanel(icon, label, widget);
    it->lastStrip = targetStrip;
    target->setRaisedId(it->barId);
    KSharedConfig::openConfig()->group(QString())
        .writeEntry(QStringLiteral("View/panels/%1/strip").arg(key), targetStrip);
    refreshPanelTooltips(); // both strips renumbered — see the method's comment
}

void MainWindow::detachPanel(const QString &key)
{
    auto it = m_panels.find(key);
    if (it == m_panels.end() || it->floatingHost) {
        return;
    }
    QWidget *widget = nullptr;
    QIcon icon = it->icon;
    QString label = it->label;
    if (it->bar) {
        auto meta = it->bar->takePanel(it->barId);
        widget = meta.widget;
        if (!icon.isNull()) icon = meta.icon;
        if (!meta.label.isEmpty()) label = meta.label;
        it->lastStrip = nameForBar(it->bar);
        it->bar = nullptr;
        it->barId = -1;
    } else {
        widget = it->widget;
    }
    if (!widget) {
        return;
    }
    auto *host = new QWidget(nullptr, Qt::Window);
    host->setAttribute(Qt::WA_DeleteOnClose, false); // we control teardown
    host->setWindowTitle(label);
    host->setWindowIcon(icon);
    auto *layout = new QVBoxLayout(host);
    layout->setContentsMargins(0, 0, 0, 0);
    widget->setParent(host);
    widget->show();
    layout->addWidget(widget);
    host->resize(600, 500);
    host->show();
    it->floatingHost = host;
    // When the user closes the floating window, re-attach to its last strip.
    host->installEventFilter(this);
    KSharedConfig::openConfig()->group(QString())
        .writeEntry(QStringLiteral("View/panels/%1/strip").arg(key),
                    QStringLiteral("floating"));
    refreshPanelTooltips();
}

void MainWindow::reattachPanel(const QString &key)
{
    auto it = m_panels.find(key);
    if (it == m_panels.end() || !it->floatingHost) {
        return;
    }
    const QString strip = it->lastStrip.isEmpty()
        ? QStringLiteral("left") : it->lastStrip;
    QWidget *widget = it->widget;
    widget->setParent(nullptr);
    it->floatingHost->removeEventFilter(this);
    it->floatingHost->deleteLater();
    it->floatingHost = nullptr;
    SideBar *bar = barByName(strip);
    if (bar) {
        it->bar = bar;
        it->barId = bar->addPanel(it->icon, it->label, widget);
        bar->setRaisedId(it->barId);
    }
    KSharedConfig::openConfig()->group(QString())
        .writeEntry(QStringLiteral("View/panels/%1/strip").arg(key), strip);
    refreshPanelTooltips();
}

// showPanelContextMenu pops a context menu next to a right-clicked tab,
// offering Move-to-other-strip and Detach actions.
void MainWindow::showPanelContextMenu(SideBar *bar, int id, const QPoint &globalPos)
{
    QWidget *widget = bar->panelWidget(id);
    const QString key = m_keyByWidget.value(widget);
    if (key.isEmpty()) {
        return;
    }
    QMenu menu(this);
    auto add = [&](const QString &label, const QString &target) -> QAction * {
        QAction *a = menu.addAction(label);
        if (bar == barByName(target)) {
            a->setEnabled(false);
        }
        return a;
    };
    QAction *toLeft   = add(i18n("Move to &Left Strip"),   QStringLiteral("left"));
    QAction *toRight  = add(i18n("Move to &Right Strip"),  QStringLiteral("right"));
    QAction *toBottom = add(i18n("Move to &Bottom Strip"), QStringLiteral("bottom"));
    menu.addSeparator();
    QAction *detach = menu.addAction(i18n("&Detach as Window"));
    QAction *chosen = menu.exec(globalPos);
    if (!chosen) return;
    if (chosen == toLeft)        movePanelToStrip(key, QStringLiteral("left"));
    else if (chosen == toRight)  movePanelToStrip(key, QStringLiteral("right"));
    else if (chosen == toBottom) movePanelToStrip(key, QStringLiteral("bottom"));
    else if (chosen == detach)   detachPanel(key);
}

void MainWindow::ensureEditorVisible()
{
    // In chat-only layout a freshly-opened file lands in a hidden editor. Show
    // it beside the conversation; leave editor/split modes untouched.
    if (m_centreMode == QLatin1String("chat")) {
        applyCentreMode(QStringLiteral("split"));
    }
}

void MainWindow::applyCentreMode(const QString &mode)
{
    if (!m_shell) {
        return;
    }
    QSplitter *h = m_shell->centreHSplitter();
    auto *agent = m_agent ? m_agent->panelStack() : nullptr;
    // Remember the last side-by-side proportions so we can restore them when
    // the user returns to "split" from one of the single-pane modes.
    if (m_centreMode == QLatin1String("split") && h && agent && m_editor
        && m_editor->isVisible() && agent->isVisible()) {
        m_centreSplitSizes = h->sizes();
    }
    m_centreMode = mode;
    KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .writeEntry("centreMode", mode);

    if (mode == QLatin1String("editor")) {
        if (m_editor) m_editor->setVisible(true);
        if (agent) agent->setVisible(false);
        if (m_centreEditorAct) m_centreEditorAct->setChecked(true);
        if (auto *v = m_editor ? m_editor->currentView() : nullptr) v->setFocus();
    } else if (mode == QLatin1String("chat")) {
        if (m_editor) m_editor->setVisible(false);
        if (agent) agent->setVisible(true);
        if (m_centreChatAct) m_centreChatAct->setChecked(true);
        if (m_agent) {
            if (auto *p = m_agent->activePanel()) p->setFocus();
        }
    } else { // "split"
        if (m_editor) m_editor->setVisible(true);
        if (agent) agent->setVisible(true);
        if (m_centreSplitAct) m_centreSplitAct->setChecked(true);
        if (h && m_centreSplitSizes.size() == h->count()) {
            h->setSizes(m_centreSplitSizes);
        } else if (h) {
            h->setSizes({700, 500});
        }
    }
}

void MainWindow::setupCore()
{
    // The status bar is the landing spot for transient agent feedback —
    // "Merged X into main", "Commit failed: …", etc. Without it those
    // messages are silently dropped (see AgentDock::statusMessage).
    statusBar()->setSizeGripEnabled(true);
    connect(m_agent, &AgentDock::statusMessage, this, [this](const QString &text) {
        statusBar()->showMessage(text, 8000);
    });

    // Cursor / mode / git / agent labels live as permanent status-bar widgets
    // so they survive the transient showMessage timeouts the agent uses for
    // toasts. Layout (left → right): git · Ln/Col · mode · agent.
    m_gitStatusLabel = new QLabel(this);
    m_gitStatusLabel->setContentsMargins(8, 0, 8, 0);
    m_gitStatusLabel->setStyleSheet(QStringLiteral("color: palette(mid);"));
    statusBar()->addPermanentWidget(m_gitStatusLabel);

    m_cursorPosLabel = new QLabel(this);
    m_cursorPosLabel->setContentsMargins(8, 0, 8, 0);
    m_cursorPosLabel->setToolTip(i18n("Click to go to line"));
    // Rendered as a link so a click jumps to a line — uses KTextEditor's native
    // go-to-line action when present, else a simple line-number prompt.
    m_cursorPosLabel->setTextInteractionFlags(Qt::LinksAccessibleByMouse);
    connect(m_cursorPosLabel, &QLabel::linkActivated, this, [this](const QString &) {
        KTextEditor::View *view = m_editor ? m_editor->currentView() : nullptr;
        if (!view) {
            return;
        }
        if (QAction *gotoAct = view->action("go_to_line")) {
            gotoAct->trigger();
            return;
        }
        bool ok = false;
        const int line = QInputDialog::getInt(
            this, i18n("Go to Line"), i18n("Line:"),
            view->cursorPosition().line() + 1, 1,
            view->document()->lines(), 1, &ok);
        if (ok) {
            view->setCursorPosition(KTextEditor::Cursor(line - 1, 0));
            view->setFocus();
        }
    });
    statusBar()->addPermanentWidget(m_cursorPosLabel);

    m_modeLabel = new QLabel(this);
    m_modeLabel->setContentsMargins(8, 0, 8, 0);
    statusBar()->addPermanentWidget(m_modeLabel);

    // Language-server status: an icon + text button. Clicking it restarts the
    // server for the active file. Hidden when no server backs the file.
    m_lspStatusButton = new QToolButton(this);
    m_lspStatusButton->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_lspStatusButton->setAutoRaise(true);
    m_lspStatusButton->setFocusPolicy(Qt::NoFocus);
    m_lspStatusButton->hide();
    connect(m_lspStatusButton, &QToolButton::clicked, this, [this] {
        if (!m_activeFilePath.isEmpty()) {
            m_lsp->restartServersForCurrentFile(m_activeFilePath);
        }
    });
    statusBar()->addPermanentWidget(m_lspStatusButton);

    m_agentStatusLabel = new QLabel(this);
    m_agentStatusLabel->setContentsMargins(8, 0, 8, 0);
    statusBar()->addPermanentWidget(m_agentStatusLabel);

    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &path) {
        m_activeFilePath = path;
        if (m_gitStatusLabel) {
            if (path.isEmpty()) {
                m_gitStatusLabel->clear();
            } else {
                m_gitStatusLabel->setText(QStringLiteral("⎇ …"));
            }
        }
        updateCursorStatus();
    });
    connect(m_agent, &AgentDock::agentActivated, this,
            [this](int, const QString &, const QString &) { updateAgentBadge(); });
    updateAgentBadge();
    updateCursorStatus();

    connect(m_core, &CoreClient::coreLog, this,
            [](const QString &line) { qInfo().noquote() << "[akcore]" << line; });
    connect(m_core, &CoreClient::failed, this, [](const QString &msg) {
        qWarning().noquote() << "[core]" << msg;
    });
    // The core took the socket but REFUSED this process the "ui" role, so every
    // UI-only RPC will be rejected and CoreClient deliberately never emits
    // connected(). Without this banner the window simply comes up inert and says
    // nothing, which is indistinguishable from a broken build — so it gets the
    // persistent, un-closable treatment, the same as losing the core outright.
    // See CoreClient::handshakeRefused (audit F13).
    connect(m_core, &CoreClient::handshakeRefused, this, [this](const QString &msg) {
        qCritical().noquote() << "[core] handshake refused:" << msg;
        m_coreBanner->setMessageType(KMessageWidget::Error);
        m_coreBanner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-error")));
        m_coreBanner->setCloseButtonVisible(false);
        m_coreBanner->setText(msg);
        m_coreBanner->animatedShow();
        statusBar()->showMessage(msg);
    });
    // A refused send drops something the human composed, so it is said out loud
    // through the same banner the connection uses — the log line is for us, not
    // for them. Transient by nature: the connection is fine, so it fades on its
    // own unless a real core loss has taken the banner over in the meantime.
    connect(m_core, &CoreClient::sendRefused, this,
            [this](const QString &method, const QString &reason) {
                qWarning().noquote() << "[core] refused" << method << ":" << reason;
                const QString text =
                    i18n("A message was too large to send to the core and was "
                         "dropped — try again with fewer or smaller attachments.");
                m_coreBanner->setMessageType(KMessageWidget::Warning);
                m_coreBanner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
                m_coreBanner->setCloseButtonVisible(true);
                m_coreBanner->setText(text);
                m_coreBanner->animatedShow();
                statusBar()->showMessage(text, 8000);
                QTimer::singleShot(15000, this, [this, text] {
                    if (m_coreBanner->text() == text) {
                        m_coreBanner->animatedHide();
                    }
                });
            });
    // Losing the core disables every action in the window, so its recovery is
    // the one condition that gets the persistent banner rather than a toast.
    connect(m_core, &CoreClient::reconnecting, this, [this] {
        m_coreBanner->setMessageType(KMessageWidget::Warning);
        m_coreBanner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
        m_coreBanner->setCloseButtonVisible(false);
        m_coreBanner->setText(
            i18n("Lost the connection to Agent Kate's core — reconnecting…"));
        m_coreBanner->animatedShow();
    });
    connect(m_core, &CoreClient::reconnected, this, [this](bool coreRespawned) {
        if (!coreRespawned) {
            m_coreBanner->animatedHide();
            statusBar()->showMessage(i18n("Reconnected to the core"), 6000);
            return;
        }
        // The core came back as a NEW process, so every agent that was running
        // died with the old one — AgentDock settles them as resumable off this
        // same signal. Keep the banner up saying so rather than flash
        // "reconnected" over a roster the user last saw working.
        m_coreBanner->setMessageType(KMessageWidget::Warning);
        m_coreBanner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
        m_coreBanner->setCloseButtonVisible(true);
        m_coreBanner->setText(i18n("The core was restarted, so your agents were "
                                   "interrupted. Their sessions were kept — "
                                   "Resume an agent to carry on."));
        m_coreBanner->animatedShow();
        statusBar()->showMessage(
            i18n("Reconnected to a restarted core — agents were interrupted"), 8000);
    });
    connect(m_core, &CoreClient::reconnectFailed, this, [this] {
        m_coreBanner->setMessageType(KMessageWidget::Error);
        m_coreBanner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-error")));
        m_coreBanner->setCloseButtonVisible(true);
        m_coreBanner->setText(i18n("The core stopped and could not be restarted; "
                                   "running agents were lost. Restart Agent Kate "
                                   "to carry on."));
        m_coreBanner->animatedShow();
    });
    // The UI handshake (claiming the "ui" role) is now sent by CoreClient as the first
    // frame on connect, so by the time connected() fires the role is already being
    // established ahead of any UI-only query. Just do the post-connect work here.
    connect(m_core, &CoreClient::connected, this, [this] {
        // Learn the core's harness capability sets first — every engine
        // picker and backend-specific affordance derives from them.
        HarnessRegistry::self()->fetch(m_core);
        // The ensemble catalogue drives the New Agent dialog and the roster's
        // quick menu; an older core simply answers with an error and the UI
        // offers no ensembles.
        EnsembleCatalog::self()->fetch(m_core);
        // Then refresh every engine/provider's live model catalogue in the
        // background; pickers rebuild from the cache on changed(), and a
        // failed/offline probe leaves the last good list intact.
        HarnessRegistry::self()->discoverAll(m_core);
        pushOpenFilesToCore();
        reloadExtensionServers();
    });
    // Re-check which engine CLIs exist whenever the registry's engine LIST can
    // have changed (the capability fetch above, and any later refresh): a core
    // that registers a third harness must not leave the banner claiming the
    // two we knew about are all there is (audit F37).
    connect(HarnessRegistry::self(), &HarnessRegistry::changed, this, [this] {
        EngineAvailability::invalidate();
        updateEngineAvailabilityBanner();
    });

    const QString corePath =
        QDir(QCoreApplication::applicationDirPath()).filePath(QStringLiteral("akcore"));
    qInfo("spawning core: %s", qPrintable(corePath));
    m_core->start(corePath);

    // Qt destroys QObject children in the order they were added. m_core was
    // constructed first in setupUi(), so it would be deleted first at shutdown
    // — before AgentPanel, WorktreeDashboard, GutterController, etc., whose
    // destructors call back into m_core. Reparenting moves m_core to the tail
    // of the child list so it outlives its dependents.
    m_core->setParent(nullptr);
    m_core->setParent(this);
}

void MainWindow::onAgentActivated(int agentId, const QString &projectPath,
                                  const QString &worktreePath)
{
    m_activeAgentId = agentId;
    m_activeProject = projectPath;
    rememberThreadProject(m_agent ? m_agent->currentThreadId() : QString());
    // The file browser scopes to the project root or the agent's worktree via
    // its own tab; Terminal/Search/Git Log stay project-scoped by design.
    m_tree->setRoots(projectPath, worktreePath);
    m_terminal->setWorkingDirectory(projectPath);
    if (m_search) {
        m_search->setProjectRoot(projectPath);
    }
    if (m_logViewer) {
        m_logViewer->setActiveSource(projectPath, m_agent->currentThreadId());
    }
    if (m_worktreeDashboard) {
        m_worktreeDashboard->setActiveProject(projectPath);
    }
    if (m_coworkPanel) {
        m_coworkPanel->setActiveThread(m_agent->currentThreadId(), QString());
    }
    // Re-point the Agent Activity inspector too. Without this it only tracked
    // threadIdChanged (thread creation), so selecting a different
    // already-running agent left it showing the previous thread's activity.
    if (m_inspectorPanel) {
        m_inspectorPanel->setActiveThread(m_agent->currentThreadId());
    }
    if (m_openWorktreeTerminalAct) {
        const bool isolated = m_agent && m_agent->activeAgentIsolated();
        m_openWorktreeTerminalAct->setEnabled(
            AgentActions::compute(
                m_agent != nullptr, /*running=*/false, /*dormant=*/false, isolated,
                /*hasPath=*/m_agent
                    && !m_agent->worktreePathForAgent(agentId).isEmpty())
                .terminal);
        m_openWorktreeTerminalAct->setText(
            AgentActions::terminalActionLabel(isolated));
        m_openWorktreeTerminalAct->setToolTip(
            AgentActions::terminalActionTooltip(isolated));
    }
    m_titleProject = QDir(projectPath).dirName();
    refreshWindowTitle();
    // An agent whose session started while a different agent was shown never
    // got the activeThreadChanged re-key; catch it here, before its group key
    // is resolved, so its tabs come back with it.
    adoptPendingEditorGroup(agentId, projectPath, m_agent ? m_agent->currentThreadId()
                                                          : QString());
    m_editor->setActiveGroup(groupKey());
    // Reopen the tabs the human had for this agent last run (once per run),
    // filtered to this agent's own roots.
    restoreEditorSession(projectPath, worktreePath);
    // Snapshot the tabs of the agent we just left; a crash after the switch
    // would otherwise lose them.
    schedulePersistEditorSession();
    updateAgentActions();
}

// groupKey names the editor tab group for the active agent. Every form is
// derived from stable identity — the project path, plus the agent's CORE thread
// id in tabs-by-agent mode. It deliberately never uses m_activeAgentId except
// for a still-thread-less agent, whose pending key is confined to this run
// (see EditorSession.h for the cross-project leak this replaced).
QString MainWindow::groupKey() const
{
    if (m_activeProject.isEmpty()) {
        return {};
    }
    if (!m_tabsByAgent) {
        return EditorSession::projectKey(m_activeProject);
    }
    const QString threadId = m_agent ? m_agent->currentThreadId() : QString();
    if (threadId.isEmpty()) {
        return EditorSession::pendingKey(m_activeProject, m_activeAgentId);
    }
    return EditorSession::agentKey(m_activeProject, threadId);
}

void MainWindow::rememberThreadProject(const QString &threadId)
{
    if (threadId.isEmpty() || m_activeProject.isEmpty()) {
        return;
    }
    m_projectByThread.insert(threadId, m_activeProject);
}

// editorGroupForThread names the tab group belonging to a GIVEN agent rather
// than the one on screen. The Jobs panel shows every agent's background work, so
// opening a shell log from there must land in the owning agent's group — putting
// it in the active one would break the per-agent scoping of plan 17 and hand a
// foreign path to that group's session persistence. A thread this window has
// never had activated has no known project, so it falls back to the active
// group: the file still opens, just where the user is looking.
QString MainWindow::editorGroupForThread(const QString &threadId) const
{
    const QString project = m_projectByThread.value(threadId);
    if (threadId.isEmpty() || project.isEmpty()) {
        return groupKey();
    }
    const QString key = m_tabsByAgent ? EditorSession::agentKey(project, threadId)
                                      : EditorSession::projectKey(project);
    return key.isEmpty() ? groupKey() : key;
}

void MainWindow::setTabsByAgent(bool byAgent)
{
    m_tabsByAgent = byAgent;
    KSharedConfig::openConfig()
        ->group(QStringLiteral("Editor"))
        .writeEntry("tabsByAgent", byAgent);
    m_editor->setActiveGroup(groupKey());
}

void MainWindow::onSave()
{
    KTextEditor::View *view = m_editor->currentView();
    const bool formatOnSave = m_formatOnSave && m_formatOnSave->isChecked();

    // When format-on-save is on and the active file's server can format, run the
    // formatter first, then save in the reply. Otherwise save synchronously so
    // markdown/csv/agent-save and server-less files are untouched.
    if (view && formatOnSave && m_lsp->canFormat(view)) {
        const QString path = m_activeFilePath;
        // Capture the DOCUMENT being saved, not the current view: the user may
        // switch tabs during the async format round-trip, and finishSave must
        // still write (and format) the document the save was requested for, not
        // whatever tab happens to be active when the reply lands. QPointer so a
        // closed document is simply skipped.
        QPointer<KTextEditor::Document> doc(view->document());
        // A hung or dead language server can drop the format callback, which
        // used to mean the file silently never saved. Guard with a single-shot
        // fallback that saves directly if the callback hasn't fired in ~1.5s.
        auto done = QSharedPointer<bool>::create(false);
        QPointer<MainWindow> self(this);
        m_lsp->formatDocument(view, [self, path, doc, done](bool) {
            if (!self || *done) {
                return;
            }
            *done = true;
            self->finishSave(doc, path);
        });
        QTimer::singleShot(1500, this, [self, path, doc, done] {
            if (!self || *done) {
                return;
            }
            *done = true;
            self->finishSave(doc, path);
        });
        return;
    }

    finishSave(m_editor->currentView() ? m_editor->currentView()->document() : nullptr,
               m_activeFilePath);
}

void MainWindow::finishSave(KTextEditor::Document *doc, const QString &path)
{
    // finishSave owns the single save-status message for the manual save paths;
    // EditorArea::saveDocument stays silent so a failure isn't reported twice.
    if (m_editor->saveDocument(doc)) {
        if (!path.isEmpty()) {
            m_lsp->documentSaved(path);
        }
        statusBar()->showMessage(
            i18n("Saved %1", path.isEmpty() ? i18n("file") : QFileInfo(path).fileName()),
            4000);
    } else {
        statusBar()->showMessage(
            i18n("Save failed: %1", path.isEmpty() ? i18n("no file") : path), 6000);
    }
}

void MainWindow::onSaveAll()
{
    m_editor->saveAll();
}

// persistEditorSession records every editor group's open tabs so the next run
// can reopen them, under the same key restoreEditorSession reads back — so the
// working set for *all* open projects survives a quit, not just the active one.
// Thread-less (pending) groups are skipped: their key means nothing next run.
// The sweep afterwards retires legacy groups and ones that can never replay.
void MainWindow::persistEditorSession()
{
    if (m_restoringSession || !m_editor) {
        return;
    }
    KConfigGroup sessions = KSharedConfig::openConfig()
                                ->group(QStringLiteral("Editor"))
                                .group(QStringLiteral("Sessions"));
    const QStringList keys = m_editor->groupKeys();
    for (const QString &key : keys) {
        if (!EditorSession::isPersistable(key)) {
            continue;
        }
        EditorSession::write(sessions, key, m_editor->openFilePathsForGroup(key),
                             m_editor->currentPathForGroup(key));
    }
    EditorSession::sweep(sessions);
    // Reach disk now: this also runs on the debounced path, whose whole point is
    // that a crash costs at most the last second of tab changes.
    sessions.sync();
}

// adoptPendingEditorGroup moves a fresh agent's tabs from its per-run pending
// key to the stable, persistable one once its core thread id exists. Returns
// whether a group actually moved.
bool MainWindow::adoptPendingEditorGroup(int agentId, const QString &projectPath,
                                         const QString &threadId)
{
    if (!m_tabsByAgent || !m_editor || threadId.isEmpty() || projectPath.isEmpty()) {
        return false;
    }
    const QString pending = EditorSession::pendingKey(projectPath, agentId);
    const QString stable = EditorSession::agentKey(projectPath, threadId);
    if (pending.isEmpty() || stable.isEmpty()
        || !m_editor->renameGroup(pending, stable)) {
        return false;
    }
    // The tabs are only worth remembering now that they have a stable key.
    schedulePersistEditorSession();
    return true;
}

void MainWindow::schedulePersistEditorSession()
{
    if (m_restoringSession || !m_sessionPersistTimer) {
        return;
    }
    m_sessionPersistTimer->start();
}

// restoreEditorSession replays the current group's saved tabs once per app run.
// Beyond "does it still exist", every path must live under the agent's own
// roots — the project, or its worktree when that is outside the project — so a
// stale or hand-edited group can never reopen another project's files.
void MainWindow::restoreEditorSession(const QString &projectPath,
                                      const QString &worktreePath)
{
    const QString key = groupKey();
    if (key.isEmpty() || projectPath.isEmpty() || m_restoredSessions.contains(key)) {
        return;
    }
    m_restoredSessions.insert(key);

    // Isolated worktrees normally live at <project>/.agentkate/worktrees/<id>,
    // already inside the project root; the explicit root covers a relocated one.
    QStringList roots{projectPath};
    if (!worktreePath.isEmpty()) {
        roots << worktreePath;
    }
    const KConfigGroup sessions = KSharedConfig::openConfig()
                                      ->group(QStringLiteral("Editor"))
                                      .group(QStringLiteral("Sessions"));
    const EditorSession::Session session = EditorSession::read(sessions, key, roots);
    if (session.files.isEmpty()) {
        return;
    }

    m_restoringSession = true;
    for (const QString &path : session.files) {
        m_editor->openFile(key, path);
    }
    // Re-activate the previously-focused file (read() only reports one that
    // survived filtering).
    if (!session.active.isEmpty()) {
        m_editor->openFile(key, session.active);
    }
    m_restoringSession = false;
}

// persistShellState writes the centre QSplitter geometry to KConfig.
void MainWindow::persistShellState()
{
    if (!m_shell) {
        return;
    }
    KConfigGroup grp = KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .group(QStringLiteral("centreSplitter"));
    m_shell->saveState(grp);
}

void MainWindow::closeEvent(QCloseEvent *event)
{
    // Snapshot the open tabs before any save-prompt closes them, so the session
    // restores the full working set next run.
    persistEditorSession();
    // Close-to-tray (plan 27 §2), decided BEFORE the unsaved-file prompt:
    // hiding is not closing, so no document is closed and nothing needs
    // asking. Every persist that the quit path runs still runs here — a crash
    // while hidden must not lose the session state this code is otherwise
    // careful to snapshot early. shouldHideToTray's clauses are the feature's
    // traps spelled out: preference off, no live tray item (the unquittable-
    // app fallback), a genuine quit (File ▸ Quit / tray Quit), session logout.
    if (!m_shutdownComplete
        && agentkate::TrayPresence::shouldHideToTray(
               closeToTrayEnabled(), m_tray && m_tray->active(),
               m_quitRequested, qApp->isSavingSession())) {
        persistShellState();
        if (m_terminal) {
            m_terminal->saveSession();
        }
        if (m_agent) {
            m_agent->persistLastActiveSessions();
        }
        event->ignore();
        hideToTray();
        return;
    }
    // Prompt to save any modified documents; a cancel aborts the close — and a
    // cancelled QUIT must disarm the flag, or the next plain close would quit
    // instead of hiding.
    if (m_editor && !m_editor->confirmCloseAll()) {
        m_quitRequested = false;
        event->ignore();
        return;
    }
    persistShellState();
    if (m_terminal) {
        m_terminal->saveSession();
    }
    // Remember the focused agent per project so the next launch lands back in it.
    if (m_agent) {
        m_agent->persistLastActiveSessions();
    }
    // Graceful, observable shutdown: while agents are live, run the stop-and
    // -compact dialog before tearing down so every agent is compacted and
    // resumable. The modal dialog drives app.shutdown and pumps progress events
    // until the core reports done; then we re-enter and take the normal path.
    if (!m_shutdownComplete && m_core && m_core->isConnected() && m_agent
        && m_agent->runningAgentCount() > 0) {
        event->ignore();
        ShutdownDialog dlg(m_core, this);
        dlg.exec();
        m_shutdownComplete = true;
        // Re-close on the next loop turn so this handler fully unwinds first.
        QMetaObject::invokeMethod(this, &QWidget::close, Qt::QueuedConnection);
        return;
    }
    // A genuine close: restore the normal quit rule (hideToTray switches it
    // off while hidden) so ending this window still ends the app.
    qApp->setQuitOnLastWindowClosed(true);
    KMainWindow::closeEvent(event);
}

// eventFilter watches the floating panel host windows so that closing one
// re-docks the panel to its last strip instead of orphaning the widget.
bool MainWindow::eventFilter(QObject *watched, QEvent *event)
{
    if (event->type() == QEvent::Close) {
        for (auto it = m_panels.begin(); it != m_panels.end(); ++it) {
            if (it->floatingHost == watched) {
                event->ignore();
                reattachPanel(it->key);
                return true;
            }
        }
    }
    return KMainWindow::eventFilter(watched, event);
}

// setupHamburger replaces the classic menubar with a Dolphin/Kate-style ☰
// button that mirrors the menubar's contents. The menubar remains in the
// tree (so the hamburger has something to mirror) but starts hidden;
// Ctrl+M toggles it for users who want the classic look.
void MainWindow::setupHamburger()
{
    KToggleAction *showMenubarAct = KStandardAction::showMenubar(
        this, [this](bool on) {
            menuBar()->setVisible(on);
            KSharedConfig::openConfig()
                ->group(QStringLiteral("View"))
                .writeEntry("menubar", on);
        }, this);
    // KStandardAction defaults this to Ctrl+M already, but say so through the
    // collection rather than setShortcut: the point is that it is a DEFAULT the
    // user may override, not a literal.
    registerAction(QLatin1String(ActionIds::OptionsShowMenubar), showMenubarAct,
                   {QKeySequence(Qt::CTRL | Qt::Key_M)});

    const bool wantMenubar = KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .readEntry("menubar", false);
    menuBar()->setVisible(wantMenubar);
    showMenubarAct->setChecked(wantMenubar);

    auto *toolbar = toolBar(QStringLiteral("topToolbar"));
    auto *hamburger = new KHamburgerMenu(this);
    hamburger->setMenuBar(menuBar());
    hamburger->setShowMenuBarAction(showMenubarAct);
    toolbar->addAction(hamburger);
    hamburger->hideActionsOf(toolbar);
}

// setupTopToolbar adds a thin toolbar with the breadcrumb, a placeholder
// global-search field, and an agent badge. The hamburger button is added
// by setupHamburger() and must run after this so the toolbar exists.
void MainWindow::setupTopToolbar()
{
    auto *toolbar = toolBar(QStringLiteral("topToolbar"));
    toolbar->setMovable(false);
    toolbar->setFloatable(false);
    toolbar->setIconSize(QSize(16, 16));
    toolbar->setContextMenuPolicy(Qt::PreventContextMenu);
    addToolBar(Qt::TopToolBarArea, toolbar);

    m_breadcrumbWidget = new QWidget(toolbar);
    auto *crumbLayout = new QHBoxLayout(m_breadcrumbWidget);
    crumbLayout->setContentsMargins(8, 0, 8, 0);
    crumbLayout->setSpacing(2);
    m_breadcrumbLabel = new QLabel(m_breadcrumbWidget);
    m_breadcrumbLabel->setTextFormat(Qt::PlainText);
    crumbLayout->addWidget(m_breadcrumbLabel);
    crumbLayout->addStretch();
    toolbar->addWidget(m_breadcrumbWidget);

    auto *stretch = new QWidget(toolbar);
    stretch->setSizePolicy(QSizePolicy::Expanding, QSizePolicy::Preferred);
    toolbar->addWidget(stretch);

    // Layout preset switcher — the friendly, high-level way to reshape the whole
    // workspace for the task at hand (Converse / Build / Review / Side by Side).
    m_layoutButton = new QToolButton(toolbar);
    m_layoutButton->setText(i18n("Layout"));
    m_layoutButton->setIcon(QIcon::fromTheme(QStringLiteral("view-multiple-objects")));
    m_layoutButton->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_layoutButton->setPopupMode(QToolButton::InstantPopup);
    m_layoutButton->setAutoRaise(true);
    m_layoutButton->setToolTip(i18n("Reshape the whole workspace for the task at hand."));
    auto *layoutMenu = new QMenu(m_layoutButton);
    // The SAME actions the View ▸ Layout submenu holds (setupPerspectives runs
    // first). A second set built here would be a second thing to keep in step —
    // and, being outside the collection, would show no shortcut and honour no
    // rebinding while sitting next to one that does.
    for (const char *id : {ActionIds::LayoutConverse, ActionIds::LayoutBuild,
                           ActionIds::LayoutSplit, ActionIds::LayoutReview}) {
        if (QAction *a = m_actions->action(QLatin1String(id))) {
            layoutMenu->addAction(a);
        }
    }
    m_layoutButton->setMenu(layoutMenu);
    toolbar->addWidget(m_layoutButton);
    toolbar->addSeparator();

    // Centre-slab mode toggle: Editor / Split / Chat. The three actions form
    // an exclusive group so exactly one stays raised; applyCentreMode hides
    // the inactive halves of the centre split and persists the choice.
    auto *modeGroup = new QActionGroup(this);
    modeGroup->setExclusive(true);
    // Toolbar-only until plan 27 §1 — and therefore unfindable, because the
    // palette walked the menu bar. They are in the collection now, which is what
    // makes "Chat" reachable by typing its name.
    m_centreEditorAct = registerAction(
        QLatin1String(ActionIds::ViewCentreEditor),
        new QAction(QIcon::fromTheme(QStringLiteral("document-edit")),
                    i18n("Editor"), this));
    m_centreSplitAct = registerAction(
        QLatin1String(ActionIds::ViewCentreSplit),
        new QAction(QIcon::fromTheme(QStringLiteral("view-split-left-right")),
                    i18n("Split"), this));
    m_centreChatAct = registerAction(
        QLatin1String(ActionIds::ViewCentreChat),
        new QAction(QIcon::fromTheme(QStringLiteral("im-user")),
                    i18n("Chat"), this));
    for (QAction *a : { m_centreEditorAct, m_centreSplitAct, m_centreChatAct }) {
        a->setCheckable(true);
        modeGroup->addAction(a);
        toolbar->addAction(a);
    }
    m_centreEditorAct->setToolTip(i18n("Show only the editor (Ctrl+E to toggle)"));
    m_centreSplitAct->setToolTip(i18n("Show editor and agent side by side"));
    m_centreChatAct->setToolTip(i18n("Show only the agent chat (Ctrl+Shift+E to toggle)"));
    connect(m_centreEditorAct, &QAction::triggered, this,
            [this] { applyCentreMode(QStringLiteral("editor")); });
    connect(m_centreSplitAct, &QAction::triggered, this,
            [this] { applyCentreMode(QStringLiteral("split")); });
    connect(m_centreChatAct, &QAction::triggered, this,
            [this] { applyCentreMode(QStringLiteral("chat")); });

    // Toolbar Search box → drives the ripgrep-backed Search panel. Pressing
    // Enter reveals the panel and forwards the query into it, so the panel's
    // debounce, toggles and workspace-scoped root are all inherited.
    m_toolbarSearch = new QLineEdit(toolbar);
    m_toolbarSearch->setPlaceholderText(i18n("Search project…  (Ctrl+Shift+F)"));
    m_toolbarSearch->setClearButtonEnabled(true);
    m_toolbarSearch->addAction(QIcon::fromTheme(QStringLiteral("search")),
                               QLineEdit::LeadingPosition);
    // Flexible width instead of a fixed 260px: it shrinks on a narrow window
    // (down to a usable floor) and the toolbar spills the rest to its overflow
    // menu, rather than the search box clipping or pinning the window wide.
    m_toolbarSearch->setMinimumWidth(120);
    m_toolbarSearch->setMaximumWidth(320);
    m_toolbarSearch->setSizePolicy(QSizePolicy::Preferred, QSizePolicy::Fixed);
    connect(m_toolbarSearch, &QLineEdit::returnPressed, this, [this] {
        const QString q = m_toolbarSearch->text().trimmed();
        if (q.isEmpty() || !m_search)
            return;
        raisePanelByKey(m_keySearch);
        m_search->search(q);
    });
    toolbar->addWidget(m_toolbarSearch);

    m_agentBadge = new QLabel(toolbar);
    m_agentBadge->setContentsMargins(8, 2, 8, 2);
    m_agentBadge->setTextFormat(Qt::PlainText);
    toolbar->addWidget(m_agentBadge);

    connect(m_editor, &EditorArea::currentFileChanged, this,
            &MainWindow::updateBreadcrumb);

    // Restore the persisted centre-slab mode now that the toolbar actions exist.
    // A brand-new profile lands chat-forward (the Converse layout) so a newcomer
    // starts in the conversation, not a code editor; everyone else gets the
    // remembered mode (default side-by-side).
    const QString mode = KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .readEntry("centreMode",
                   m_firstRunProfile ? QStringLiteral("chat") : QStringLiteral("split"));
    applyCentreMode(mode);
}

void MainWindow::updateBreadcrumb(const QString &path)
{
    if (!m_breadcrumbWidget) {
        return;
    }
    // Rebuild the segment row each refresh — cheap, and the segments aren't
    // styled per-segment so a churn here is invisible to the user.
    auto *layout = static_cast<QHBoxLayout *>(m_breadcrumbWidget->layout());
    if (!layout) {
        return;
    }
    while (auto *item = layout->takeAt(0)) {
        if (auto *w = item->widget()) {
            w->deleteLater();
        }
        delete item;
    }
    m_breadcrumbLabel = nullptr;

    auto addLabel = [&](const QString &text) {
        auto *lbl = new QLabel(text, m_breadcrumbWidget);
        lbl->setForegroundRole(QPalette::PlaceholderText);
        layout->addWidget(lbl);
    };
    auto addSegment = [&](const QString &text, const QString &openDir) {
        auto *btn = new QToolButton(m_breadcrumbWidget);
        btn->setText(text);
        btn->setAutoRaise(true);
        btn->setToolButtonStyle(Qt::ToolButtonTextOnly);
        btn->setCursor(Qt::PointingHandCursor);
        connect(btn, &QToolButton::clicked, this, [this, openDir] {
            if (!openDir.isEmpty() && m_tree) {
                m_tree->setRoot(openDir);
                raisePanelByKey(m_keyFiles);
            }
        });
        layout->addWidget(btn);
    };

    if (m_activeProject.isEmpty()) {
        layout->addStretch();
        return;
    }
    addSegment(QDir(m_activeProject).dirName(), m_activeProject);
    if (!path.isEmpty()) {
        const QString rel = QDir(m_activeProject).relativeFilePath(path);
        const QStringList segs = rel.split(QLatin1Char('/'), Qt::SkipEmptyParts);
        QString accumulated = m_activeProject;
        for (int i = 0; i < segs.size(); ++i) {
            addLabel(QStringLiteral("›"));
            accumulated = QDir(accumulated).filePath(segs[i]);
            // Only intermediate segments open a directory; the final segment
            // is the file itself, so its click just navigates the tree to
            // its parent directory.
            const QString openDir = (i == segs.size() - 1)
                ? QFileInfo(accumulated).absolutePath()
                : accumulated;
            addSegment(segs[i], openDir);
        }
    }
    layout->addStretch();
}

void MainWindow::updateAgentBadge()
{
    if (m_agentBadge) {
        const QString tid = m_agent ? m_agent->currentThreadId() : QString();
        m_agentBadge->setText(tid.isEmpty()
            ? i18n("no active agent")
            : i18n("agent: %1", tid));
    }
    if (m_agentStatusLabel) {
        const QString tid = m_agent ? m_agent->currentThreadId() : QString();
        m_agentStatusLabel->setText(tid.isEmpty()
            ? i18n("· no agent")
            : QStringLiteral("● ") + tid);
    }
}

void MainWindow::updateCursorStatus()
{
    KTextEditor::View *view = m_editor ? m_editor->currentView() : nullptr;
    if (view != m_observedView) {
        if (m_observedView) {
            disconnect(m_observedView, nullptr, this, nullptr);
        }
        m_observedView = view;
        if (view) {
            connect(view, &KTextEditor::View::cursorPositionChanged, this,
                    [this](KTextEditor::View *, const KTextEditor::Cursor &) {
                        updateCursorStatus();
                    });
            connect(view, &QObject::destroyed, this, [this](QObject *o) {
                if (m_observedView == o) {
                    m_observedView = nullptr;
                    updateCursorStatus();
                }
            });
        }
    }
    if (!view) {
        if (m_cursorPosLabel) m_cursorPosLabel->clear();
        if (m_modeLabel) m_modeLabel->clear();
        return;
    }
    const auto cursor = view->cursorPosition();
    if (m_cursorPosLabel) {
        // Wrapped in an anchor so the label is clickable (jump to line); the
        // href payload is unused — the linkActivated handler reads the view.
        m_cursorPosLabel->setText(
            QStringLiteral("<a href=\"#gotoline\" style=\"text-decoration:none\">%1</a>")
                .arg(i18n("Ln %1, Col %2", cursor.line() + 1, cursor.column() + 1)));
    }
    if (m_modeLabel) {
        KTextEditor::Document *doc = view->document();
        const QString enc = doc->encoding().isEmpty()
            ? QStringLiteral("UTF-8") : doc->encoding();
        const QString mode = doc->mode();
        m_modeLabel->setText(mode.isEmpty()
            ? enc
            : QStringLiteral("%1 · %2").arg(enc, mode));
    }
}

// updateLspStatus refreshes the status-bar language-server widget for the file
// that currently has focus, hiding it when no server backs that file.
void MainWindow::updateLspStatus()
{
    if (!m_lspStatusButton) {
        return;
    }
    QString iconName;
    const QString text = m_lsp->statusFor(m_activeFilePath, iconName);
    if (text.isEmpty()) {
        m_lspStatusButton->hide();
        return;
    }
    m_lspStatusButton->setIcon(QIcon::fromTheme(iconName));
    m_lspStatusButton->setText(text);
    m_lspStatusButton->setToolTip(i18n("Click to restart the language server"));
    m_lspStatusButton->show();
}

// reloadExtensionServers asks the core for every installed VS Code extension
// and hands each one's bundled language server to the LSP manager, so files
// opened afterwards use it. Called on connect and after an install.
void MainWindow::reloadExtensionServers()
{
    if (!m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("vsix.list"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     m_lsp->clearExtensionServers();
                     const QJsonArray exts =
                         result.value(QStringLiteral("extensions")).toArray();
                     for (const QJsonValue &v : exts) {
                         const QJsonObject server =
                             v.toObject().value(QStringLiteral("server")).toObject();
                         if (server.isEmpty()) {
                             continue;
                         }
                         QStringList args;
                         for (const QJsonValue &a :
                              server.value(QStringLiteral("args")).toArray()) {
                             args << a.toString();
                         }
                         QStringList fileExts;
                         for (const QJsonValue &e :
                              server.value(QStringLiteral("fileExtensions")).toArray()) {
                             fileExts << e.toString();
                         }
                         const QJsonArray langIds =
                             server.value(QStringLiteral("languageIds")).toArray();
                         m_lsp->registerExtensionServer(
                             fileExts, server.value(QStringLiteral("command")).toString(),
                             args,
                             langIds.isEmpty() ? QString() : langIds.first().toString());
                     }
                     // Apply newly-registered servers to already-open files.
                     m_lsp->rebindOpenDocuments();
                     updateLspStatus();
                 },
                 this); // lifetime guard so the disconnect-drain can't touch a dead window
}

void MainWindow::pushOpenFilesToCore()
{
    if (!m_core->isConnected()) {
        return;
    }
    QJsonArray files;
    const QStringList paths = m_editor->openFilePaths();
    for (const QString &p : paths) {
        files.append(p);
    }
    m_core->call(QStringLiteral("coop.setOpenFiles"),
                 QJsonObject{{QStringLiteral("owner"), QStringLiteral("human")},
                             {QStringLiteral("files"), files}});
}
