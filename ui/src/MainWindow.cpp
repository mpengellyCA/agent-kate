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
#include "WorktreeDashboard.h"
#include "git/GutterController.h"
#include "ipc/CoreClient.h"
#include "lsp/LspManager.h"

#include <KAboutData>
#include <KMultiTabBar>
#include <KConfigGroup>
#include <KHelpMenu>
#include <KLocalizedString>
#include <KSharedConfig>
#include <KStandardAction>

#include <QAction>
#include <QActionGroup>
#include <QCoreApplication>
#include <QDebug>
#include <QDir>
#include <QDockWidget>
#include <QFileInfo>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QKeySequence>
#include <QMenu>
#include <QMenuBar>
#include <QLabel>
#include <QStatusBar>
#include <QTabWidget>
#include <QTimer>
#include <QToolBar>

#include <array>

MainWindow::MainWindow(const QString &openPath, QWidget *parent)
    : KMainWindow(parent)
{
    setWindowTitle(i18n("AgentKate"));
    m_tabsByAgent =
        KSharedConfig::openConfig()->group(QStringLiteral("Editor"))
            .readEntry("tabsByAgent", false);

    setupUi();
    setupActions();
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

MainWindow::~MainWindow() = default;

void MainWindow::setupUi()
{
    m_core = new CoreClient(this);

    // Centre: the agent conversation — AgentKate is agent-first.
    m_agent = new AgentDock(m_core, this);
    setCentralWidget(m_agent);

    // Left: the project → agent roster.
    auto *rosterDock = new QDockWidget(i18n("Projects & Agents"), this);
    rosterDock->setObjectName(QStringLiteral("rosterDock"));
    rosterDock->setWidget(m_agent->roster());
    addDockWidget(Qt::LeftDockWidgetArea, rosterDock);

    // Right: the active project's file tree above the editor.
    m_tree = new ProjectTree(this);
    auto *treeDock = new QDockWidget(i18n("Files"), this);
    treeDock->setObjectName(QStringLiteral("filesDock"));
    treeDock->setWidget(m_tree);
    addDockWidget(Qt::RightDockWidgetArea, treeDock);

    m_editor = new EditorArea(this);
    auto *editorDock = new QDockWidget(i18n("Editor"), this);
    editorDock->setObjectName(QStringLiteral("editorDock"));
    editorDock->setWidget(m_editor);
    addDockWidget(Qt::RightDockWidgetArea, editorDock);
    splitDockWidget(treeDock, editorDock, Qt::Vertical);

    // Bottom: the Problems panel, fed by the LSP manager. The three bottom
    // docks share one slot — only one is visible at a time, driven by the
    // KMultiTabBar strip below (see setupBottomStrip).
    m_lsp = new LspManager(this);
    auto *problems = new ProblemsPanel(m_lsp, this);
    m_problemsDock = new QDockWidget(i18n("Problems"), this);
    m_problemsDock->setObjectName(QStringLiteral("problemsDock"));
    m_problemsDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
    m_problemsDock->setWidget(problems);
    addDockWidget(Qt::BottomDockWidgetArea, m_problemsDock);
    connect(problems, &ProblemsPanel::activated, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });

    auto *references = new ReferencesPanel(this);
    m_referencesDock = new QDockWidget(i18n("References"), this);
    m_referencesDock->setObjectName(QStringLiteral("referencesDock"));
    m_referencesDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("dialog-information")));
    m_referencesDock->setWidget(references);
    addDockWidget(Qt::BottomDockWidgetArea, m_referencesDock);

    m_terminal = new TerminalPanel(this);
    m_terminalDock = new QDockWidget(i18n("Terminal"), this);
    m_terminalDock->setObjectName(QStringLiteral("terminalDock"));
    m_terminalDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("utilities-terminal")));
    m_terminalDock->setWidget(m_terminal);
    addDockWidget(Qt::BottomDockWidgetArea, m_terminalDock);

    setupBottomStrip();

    // Outline panel — the active file's symbols, tabbed with the file tree.
    auto *outline = new OutlinePanel(this);
    auto *outlineDock = new QDockWidget(i18n("Outline"), this);
    outlineDock->setObjectName(QStringLiteral("outlineDock"));
    outlineDock->setWidget(outline);
    addDockWidget(Qt::RightDockWidgetArea, outlineDock);
    tabifyDockWidget(treeDock, outlineDock);

    // Worktree dashboard — every agent's branch / ahead / behind / dirty
    // count, polled at 1 Hz. Tabbed with the file tree so the right column
    // doubles as "what are the agents doing to git right now?".
    m_worktreeDashboard = new WorktreeDashboard(m_core, this);
    auto *worktreeDock = new QDockWidget(i18n("Worktrees"), this);
    worktreeDock->setObjectName(QStringLiteral("worktreeDock"));
    worktreeDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("vcs-branch")));
    worktreeDock->setWidget(m_worktreeDashboard);
    addDockWidget(Qt::RightDockWidgetArea, worktreeDock);
    tabifyDockWidget(treeDock, worktreeDock);
    treeDock->raise();

    connect(m_lsp, &LspManager::definitionResolved, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_lsp, &LspManager::referencesResolved, references, &ReferencesPanel::setLocations);
    connect(m_lsp, &LspManager::referencesResolved, this,
            [this](const QList<Location> &) { showBottomTab(1); });
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

    resizeDocks({rosterDock, treeDock}, {270, 480}, Qt::Horizontal);
    resizeDocks({treeDock, editorDock}, {240, 620}, Qt::Vertical);
    resizeDocks({m_problemsDock}, {230}, Qt::Vertical);

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
    m_toggleBottomAct = viewMenu->addAction(i18n("Toggle &Bottom Panel"));
    m_toggleBottomAct->setShortcut(Qt::CTRL | Qt::Key_J);
    m_toggleBottomAct->setToolTip(
        i18n("Show or hide the Terminal / References / Problems strip."));
    connect(m_toggleBottomAct, &QAction::triggered, this, [this] {
        const bool anyVisible = m_problemsDock->isVisible()
            || m_referencesDock->isVisible() || m_terminalDock->isVisible();
        if (anyVisible) {
            hideBottomTab(0);
            hideBottomTab(1);
            hideBottomTab(2);
        } else {
            showBottomTab(m_lastBottomTab >= 0 ? m_lastBottomTab : 2);
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

    // Permanent git status widget on the right of the status bar. Populated
    // by the active editor's GutterController as it polls the core.
    m_gitStatusLabel = new QLabel(this);
    m_gitStatusLabel->setContentsMargins(8, 0, 8, 0);
    m_gitStatusLabel->setStyleSheet(QStringLiteral("color: palette(mid);"));
    statusBar()->addPermanentWidget(m_gitStatusLabel);

    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &path) {
        m_activeFilePath = path;
        if (!m_gitStatusLabel) {
            return;
        }
        if (path.isEmpty()) {
            m_gitStatusLabel->clear();
        } else {
            m_gitStatusLabel->setText(QStringLiteral("⎇ …"));
        }
    });

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
}

void MainWindow::onAgentActivated(int agentId, const QString &projectPath)
{
    m_activeAgentId = agentId;
    m_activeProject = projectPath;
    m_tree->setRoot(projectPath);
    m_terminal->setWorkingDirectory(projectPath);
    setWindowTitle(i18n("AgentKate — %1", QDir(projectPath).dirName()));
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

// setupBottomStrip installs a Kate-style KMultiTabBar across the bottom edge
// of the window. The three bottom docks are hidden by default; clicking a tab
// raises that dock (hiding the others) and clicking the raised tab again
// collapses the row entirely so the editor reclaims the vertical space.
void MainWindow::setupBottomStrip()
{
    auto *bar = addToolBar(i18n("Bottom Panel"));
    bar->setObjectName(QStringLiteral("bottomStripBar"));
    bar->setMovable(false);
    bar->setFloatable(false);
    bar->setContextMenuPolicy(Qt::PreventContextMenu);
    addToolBar(Qt::BottomToolBarArea, bar);

    m_bottomStrip = new KMultiTabBar(KMultiTabBar::Bottom, bar);
    m_bottomStrip->setStyle(KMultiTabBar::VSNET);
    m_bottomStrip->appendTab(QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                             0, i18n("Terminal"));
    m_bottomStrip->appendTab(QIcon::fromTheme(QStringLiteral("dialog-information")),
                             1, i18n("References"));
    m_bottomStrip->appendTab(QIcon::fromTheme(QStringLiteral("dialog-warning")),
                             2, i18n("Problems"));
    bar->addWidget(m_bottomStrip);

    const std::array<std::pair<int, QDockWidget *>, 3> mapping{{
        {0, m_terminalDock}, {1, m_referencesDock}, {2, m_problemsDock}}};
    for (const auto &[id, dock] : mapping) {
        connect(m_bottomStrip->tab(id), &KMultiTabBarTab::clicked, this, [this, id] {
            if (m_bottomStrip->isTabRaised(id)) {
                showBottomTab(id);
            } else {
                hideBottomTab(id);
            }
        });
        connect(dock, &QDockWidget::visibilityChanged, this,
                [this, id](bool visible) { syncBottomTabFromDock(id, visible); });
    }

    const KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("View"));
    const int initial = cfg.readEntry("bottomTab", -1);
    m_problemsDock->setVisible(false);
    m_referencesDock->setVisible(false);
    m_terminalDock->setVisible(false);
    if (initial >= 0 && initial <= 2) {
        showBottomTab(initial);
    }
}

void MainWindow::showBottomTab(int id)
{
    if (!m_bottomStrip) {
        return;
    }
    QDockWidget *target = id == 0 ? m_terminalDock
                       : id == 1 ? m_referencesDock
                       : id == 2 ? m_problemsDock : nullptr;
    if (!target) {
        return;
    }
    for (int other : {0, 1, 2}) {
        if (other == id) {
            continue;
        }
        QDockWidget *d = other == 0 ? m_terminalDock
                       : other == 1 ? m_referencesDock : m_problemsDock;
        d->setVisible(false);
        m_bottomStrip->setTab(other, false);
    }
    target->setVisible(true);
    target->raise();
    m_bottomStrip->setTab(id, true);
    m_lastBottomTab = id;
    KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .writeEntry("bottomTab", id);
}

void MainWindow::hideBottomTab(int id)
{
    if (!m_bottomStrip) {
        return;
    }
    QDockWidget *target = id == 0 ? m_terminalDock
                       : id == 1 ? m_referencesDock
                       : id == 2 ? m_problemsDock : nullptr;
    if (!target) {
        return;
    }
    target->setVisible(false);
    m_bottomStrip->setTab(id, false);
    KSharedConfig::openConfig()
        ->group(QStringLiteral("View"))
        .writeEntry("bottomTab", -1);
}

// syncBottomTabFromDock keeps the strip in sync when the dock's own × button
// hides it, or when Qt restores dock state on startup.
void MainWindow::syncBottomTabFromDock(int id, bool visible)
{
    if (!m_bottomStrip) {
        return;
    }
    if (m_bottomStrip->isTabRaised(id) != visible) {
        m_bottomStrip->setTab(id, visible);
    }
    if (visible) {
        m_lastBottomTab = id;
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
