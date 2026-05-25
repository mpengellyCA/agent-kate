#include "MainWindow.h"
#include "AgentDock.h"
#include "EditorArea.h"
#include "ExtensionsDialog.h"
#include "OutlinePanel.h"
#include "ProblemsPanel.h"
#include "ProjectTree.h"
#include "ReferencesPanel.h"
#include "SessionBrowserDialog.h"
#include "SkillsDialog.h"
#include "TerminalPanel.h"
#include "WelcomeDialog.h"
#include "WorktreeDashboard.h"
#include "shell/ShellLayout.h"
#include "shell/SideBar.h"
#include "git/BlameController.h"
#include "git/GutterController.h"
#include "git/LogViewer.h"
#include "ipc/CoreClient.h"

#include <KTextEditor/Document>
#include <KTextEditor/View>
#include "lsp/LspManager.h"

#include <KAboutData>
#include <KMultiTabBar>
#include <KConfigGroup>
#include <KHamburgerMenu>
#include <KHelpMenu>
#include <KLocalizedString>
#include <KSharedConfig>
#include <KStandardAction>
#include <KToggleAction>
#include <KToolBar>

#include <QAction>
#include <QActionGroup>
#include <QCloseEvent>
#include <QCoreApplication>
#include <QDebug>
#include <QDir>
#include <QFileInfo>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QKeySequence>
#include <QMenu>
#include <QMenuBar>
#include <QLabel>
#include <QLineEdit>
#include <QStackedWidget>
#include <QStatusBar>
#include <QTabWidget>
#include <QTimer>
#include <QToolBar>

MainWindow::MainWindow(const QString &openPath, QWidget *parent)
    : KMainWindow(parent)
{
    setWindowTitle(i18n("Agent Kate"));
    m_tabsByAgent =
        KSharedConfig::openConfig()->group(QStringLiteral("Editor"))
            .readEntry("tabsByAgent", false);

    setupUi();
    setupActions();
    setupTopToolbar();
    setupHamburger();
    setupCore();

    // Resolve the launch argument: a file opens its parent directory as the
    // first project, then the file itself once that project is active.
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

    resize(1500, 900);
    setAutoSaveSettings();
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
}

void MainWindow::setupUi()
{
    m_core = new CoreClient(this);

    // AgentDock is a QObject orchestrator: its widgets (the roster and the
    // panel stack) are placed by ShellLayout into the left strip and the
    // centre split's right pane respectively.
    m_agent = new AgentDock(m_core, this);

    m_editor = new EditorArea(this);
    m_tree = new ProjectTree(this);
    m_lsp = new LspManager(this);

    auto *problems = new ProblemsPanel(m_lsp, this);
    auto *references = new ReferencesPanel(this);
    auto *outline = new OutlinePanel(this);
    m_terminal = new TerminalPanel(this);
    m_worktreeDashboard = new WorktreeDashboard(m_core, this);
    m_logViewer = new LogViewer(m_core, this);

    connect(problems, &ProblemsPanel::activated, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_worktreeDashboard, &WorktreeDashboard::statusMessage, this,
            [this](const QString &text) { statusBar()->showMessage(text, 6000); });

    // Three Kate-style activity strips frame the centre. Each SideBar pairs
    // a KMultiTabBar (placed at the window edge by ShellLayout) with its own
    // QStackedWidget (placed inside the centre splitter so the panel area
    // resizes with a draggable handle).
    m_leftBar = new SideBar(KMultiTabBar::Left, this);
    m_rightBar = new SideBar(KMultiTabBar::Right, this);
    m_bottomBar = new SideBar(KMultiTabBar::Bottom, this);

    m_leftRosterId = m_leftBar->addPanel(
        QIcon::fromTheme(QStringLiteral("system-users")),
        i18n("Projects && Agents"), m_agent->roster());
    m_leftFilesId = m_leftBar->addPanel(
        QIcon::fromTheme(QStringLiteral("folder")),
        i18n("Files"), m_tree);
    m_leftOutlineId = m_leftBar->addPanel(
        QIcon::fromTheme(QStringLiteral("code-context")),
        i18n("Outline"), outline);

    m_rightWorktreesId = m_rightBar->addPanel(
        QIcon::fromTheme(QStringLiteral("vcs-branch")),
        i18n("Worktrees"), m_worktreeDashboard);
    m_rightGitLogId = m_rightBar->addPanel(
        QIcon::fromTheme(QStringLiteral("vcs-commit")),
        i18n("Git Log"), m_logViewer);

    m_bottomTerminalId = m_bottomBar->addPanel(
        QIcon::fromTheme(QStringLiteral("utilities-terminal")),
        i18n("Terminal"), m_terminal);
    m_bottomReferencesId = m_bottomBar->addPanel(
        QIcon::fromTheme(QStringLiteral("dialog-information")),
        i18n("References"), references);
    m_bottomProblemsId = m_bottomBar->addPanel(
        QIcon::fromTheme(QStringLiteral("dialog-warning")),
        i18n("Problems"), problems);

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
    setCentralWidget(m_shell);

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
    wireStrip(m_leftBar, QStringLiteral("leftStrip"), m_leftRosterId);
    wireStrip(m_rightBar, QStringLiteral("rightStrip"), m_rightWorktreesId);
    wireStrip(m_bottomBar, QStringLiteral("bottomStrip"), -1);
    m_lastBottomTab = m_bottomBar->raisedId();

    connect(m_lsp, &LspManager::definitionResolved, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_lsp, &LspManager::referencesResolved, references, &ReferencesPanel::setLocations);
    connect(m_lsp, &LspManager::referencesResolved, this,
            [this](const QList<Location> &) {
                if (m_bottomBar && m_bottomReferencesId >= 0) {
                    m_bottomBar->setRaisedId(m_bottomReferencesId);
                }
            });
    connect(references, &ReferencesPanel::activated, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_lsp, &LspManager::symbolsResolved, outline, &OutlinePanel::setSymbols);
    connect(outline, &OutlinePanel::activated, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &path) {
        if (!path.isEmpty()) {
            m_lsp->requestSymbols(path);
        }
    });

    connect(m_agent, &AgentDock::agentActivated, this, &MainWindow::onAgentActivated);
    connect(m_agent, &AgentDock::projectFocused, this,
            [this](const QString &path) { m_tree->setRoot(path); });
    connect(m_agent, &AgentDock::openDiff, this,
            [this](const QString &title, const QString &diff) {
                m_editor->openDiff(groupKey(), title, diff);
            });

    connect(m_tree, &ProjectTree::fileActivated, this,
            [this](const QString &path) { m_editor->openFile(groupKey(), path); });
    connect(m_editor, &EditorArea::openFilesChanged, this, &MainWindow::pushOpenFilesToCore);
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
}

void MainWindow::setupActions()
{
    QMenu *fileMenu = menuBar()->addMenu(i18n("&File"));

    auto *openAct = new QAction(QIcon::fromTheme(QStringLiteral("folder-open")),
                                i18n("&Open Project…"), this);
    openAct->setShortcut(QKeySequence::Open);
    connect(openAct, &QAction::triggered, this, [this] { m_agent->openProjectDialog(); });
    fileMenu->addAction(openAct);

    auto *welcomeAct = new QAction(QIcon::fromTheme(QStringLiteral("go-home")),
                                   i18n("&Welcome Screen…"), this);
    welcomeAct->setToolTip(i18n("Pick a recent project, open a folder, or start a new one."));
    connect(welcomeAct, &QAction::triggered, this, [this] {
        WelcomeDialog dlg(this);
        if (dlg.exec() == QDialog::Accepted && !dlg.selectedPath().isEmpty()) {
            m_agent->addProject(dlg.selectedPath());
        }
    });
    fileMenu->addAction(welcomeAct);

    auto *resumeAct = new QAction(QIcon::fromTheme(QStringLiteral("document-open-recent")),
                                  i18n("&Resume a Session…"), this);
    connect(resumeAct, &QAction::triggered, this, [this] {
        auto *dlg = new SessionBrowserDialog(m_core, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        connect(dlg, &SessionBrowserDialog::attachRequested, m_agent,
                &AgentDock::attachSession);
        dlg->show();
    });
    fileMenu->addAction(resumeAct);

    QAction *saveAct = KStandardAction::save(this, &MainWindow::onSave, this);
    fileMenu->addAction(saveAct);

    fileMenu->addSeparator();
    fileMenu->addAction(KStandardAction::quit(this, &QWidget::close, this));

    QMenu *optionsMenu = menuBar()->addMenu(i18n("&Options"));
    QMenu *grouping = optionsMenu->addMenu(i18n("Editor Tabs Grouped By"));
    auto *groupingActs = new QActionGroup(this);
    QAction *byProject = grouping->addAction(i18n("Project"));
    QAction *byAgent = grouping->addAction(i18n("Agent"));
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

    auto *enterSendsAct = optionsMenu->addAction(i18n("&Enter Sends the Message"));
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

    auto *showToolsAct = optionsMenu->addAction(i18n("Show &Tool Calls"));
    showToolsAct->setCheckable(true);
    showToolsAct->setChecked(agentCfg.readEntry("showTools", true));
    connect(showToolsAct, &QAction::toggled, this, [this](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("showTools", on);
        m_agent->applyChatSettings();
    });

    QMenu *viewMenu = menuBar()->addMenu(i18n("&View"));
    m_blameToggle = viewMenu->addAction(i18n("Show Git &Blame"));
    m_blameToggle->setCheckable(true);
    m_blameToggle->setShortcut(Qt::CTRL | Qt::SHIFT | Qt::Key_B);
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
    m_toggleBottomAct = viewMenu->addAction(i18n("Toggle &Bottom Panel"));
    m_toggleBottomAct->setShortcut(Qt::CTRL | Qt::Key_J);
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
                                                    : m_bottomProblemsId;
            m_bottomBar->setRaisedId(target);
        }
    });

    QMenu *codeMenu = menuBar()->addMenu(i18n("&Code"));
    QAction *defAct = codeMenu->addAction(i18n("Go to &Definition"));
    defAct->setShortcut(Qt::Key_F12);
    connect(defAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->gotoDefinition(view);
        }
    });
    QAction *refAct = codeMenu->addAction(i18n("Find &References"));
    refAct->setShortcut(Qt::SHIFT | Qt::Key_F12);
    connect(refAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->findReferences(view);
        }
    });

    codeMenu->addSeparator();
    auto *extAct = new QAction(QIcon::fromTheme(QStringLiteral("install")),
                               i18n("Manage Language &Extensions…"), this);
    connect(extAct, &QAction::triggered, this, [this] {
        auto *dlg = new ExtensionsDialog(m_core, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        connect(dlg, &ExtensionsDialog::extensionsChanged, this,
                &MainWindow::reloadExtensionServers);
        dlg->show();
    });
    codeMenu->addAction(extAct);

    auto *skillsAct = new QAction(QIcon::fromTheme(QStringLiteral("preferences-plugin")),
                                  i18n("Manage Claude &Skills…"), this);
    connect(skillsAct, &QAction::triggered, this, [this] {
        if (m_activeProject.isEmpty()) {
            return;
        }
        auto *dlg = new SkillsDialog(m_core, m_activeProject, this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        dlg->show();
    });
    codeMenu->addAction(skillsAct);

    auto *helpMenu = new KHelpMenu(this, KAboutData::applicationData());
    menuBar()->addMenu(helpMenu->menu());
}

void MainWindow::setupCore()
{
    // The status bar is the landing spot for transient agent feedback —
    // "Merged X into main", "Commit failed: …", etc. Without it those
    // messages are silently dropped (see AgentDock::statusMessage).
    statusBar()->setSizeGripEnabled(false);
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
    statusBar()->addPermanentWidget(m_cursorPosLabel);

    m_modeLabel = new QLabel(this);
    m_modeLabel->setContentsMargins(8, 0, 8, 0);
    statusBar()->addPermanentWidget(m_modeLabel);

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
            [this](int, const QString &) { updateAgentBadge(); });
    updateAgentBadge();
    updateCursorStatus();

    connect(m_core, &CoreClient::coreLog, this,
            [](const QString &line) { qInfo().noquote() << "[akcore]" << line; });
    connect(m_core, &CoreClient::failed, this, [](const QString &msg) {
        qWarning().noquote() << "[core]" << msg;
    });
    connect(m_core, &CoreClient::connected, this, [this] {
        m_core->call(QStringLiteral("handshake"), {},
                     [this](const QJsonObject &, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             return;
                         }
                         pushOpenFilesToCore();
                         reloadExtensionServers();
                     });
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

void MainWindow::onAgentActivated(int agentId, const QString &projectPath)
{
    m_activeAgentId = agentId;
    m_activeProject = projectPath;
    m_tree->setRoot(projectPath);
    m_terminal->setWorkingDirectory(projectPath);
    if (m_logViewer) {
        m_logViewer->setActiveSource(projectPath, m_agent->currentThreadId());
    }
    if (m_worktreeDashboard) {
        m_worktreeDashboard->setActiveProject(projectPath);
    }
    setWindowTitle(i18n("Agent Kate — %1", QDir(projectPath).dirName()));
    m_editor->setActiveGroup(groupKey());
}

QString MainWindow::groupKey() const
{
    if (m_tabsByAgent) {
        return QStringLiteral("agent-%1").arg(m_activeAgentId);
    }
    return m_activeProject;
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
    m_editor->saveCurrent();
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
    persistShellState();
    KMainWindow::closeEvent(event);
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
    showMenubarAct->setShortcut(Qt::CTRL | Qt::Key_M);

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

    m_breadcrumbLabel = new QLabel(toolbar);
    m_breadcrumbLabel->setContentsMargins(8, 0, 8, 0);
    m_breadcrumbLabel->setTextFormat(Qt::PlainText);
    m_breadcrumbLabel->setMinimumWidth(120);
    toolbar->addWidget(m_breadcrumbLabel);

    auto *stretch = new QWidget(toolbar);
    stretch->setSizePolicy(QSizePolicy::Expanding, QSizePolicy::Preferred);
    toolbar->addWidget(stretch);

    // Placeholder global-symbol search (wiring lives in Phase 4).
    auto *search = new QLineEdit(toolbar);
    search->setPlaceholderText(i18n("Search…  (Ctrl+T)"));
    search->setClearButtonEnabled(true);
    search->addAction(QIcon::fromTheme(QStringLiteral("search")),
                      QLineEdit::LeadingPosition);
    search->setFixedWidth(260);
    search->setEnabled(false); // disabled until Phase 4 wires global symbol search
    toolbar->addWidget(search);

    m_agentBadge = new QLabel(toolbar);
    m_agentBadge->setContentsMargins(8, 2, 8, 2);
    m_agentBadge->setTextFormat(Qt::PlainText);
    toolbar->addWidget(m_agentBadge);

    connect(m_editor, &EditorArea::currentFileChanged, this,
            &MainWindow::updateBreadcrumb);
}

void MainWindow::updateBreadcrumb(const QString &path)
{
    if (!m_breadcrumbLabel) {
        return;
    }
    if (path.isEmpty() || m_activeProject.isEmpty()) {
        m_breadcrumbLabel->setText(m_activeProject.isEmpty()
            ? QString() : QDir(m_activeProject).dirName());
        return;
    }
    const QString project = QDir(m_activeProject).dirName();
    const QString rel = QDir(m_activeProject).relativeFilePath(path);
    QString crumb = project;
    for (const QString &seg : rel.split(QLatin1Char('/'), Qt::SkipEmptyParts)) {
        crumb += QStringLiteral("  ›  ") + seg;
    }
    m_breadcrumbLabel->setText(crumb);
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
        m_cursorPosLabel->setText(
            i18n("Ln %1, Col %2", cursor.line() + 1, cursor.column() + 1));
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
                 });
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
