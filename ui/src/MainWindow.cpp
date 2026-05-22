#include "MainWindow.h"
#include "AgentDock.h"
#include "EditorArea.h"
#include "ExtensionsDialog.h"
#include "OutlinePanel.h"
#include "ProblemsPanel.h"
#include "ProjectTree.h"
#include "ReferencesPanel.h"
#include "SessionBrowserDialog.h"
#include "TerminalPanel.h"
#include "ipc/CoreClient.h"
#include "lsp/LspManager.h"

#include <KAboutData>
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
#include <QLabel>
#include <QMenu>
#include <QMenuBar>
#include <QStatusBar>
#include <QTabWidget>

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

    // Bottom: the Problems panel, fed by the LSP manager.
    m_lsp = new LspManager(this);
    auto *problems = new ProblemsPanel(m_lsp, this);
    auto *problemsDock = new QDockWidget(i18n("Problems"), this);
    problemsDock->setObjectName(QStringLiteral("problemsDock"));
    problemsDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
    problemsDock->setWidget(problems);
    addDockWidget(Qt::BottomDockWidgetArea, problemsDock);
    connect(problems, &ProblemsPanel::activated, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });

    // References panel — tabbed beside Problems in the bottom row.
    auto *references = new ReferencesPanel(this);
    auto *referencesDock = new QDockWidget(i18n("References"), this);
    referencesDock->setObjectName(QStringLiteral("referencesDock"));
    referencesDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("dialog-information")));
    referencesDock->setWidget(references);
    addDockWidget(Qt::BottomDockWidgetArea, referencesDock);
    tabifyDockWidget(problemsDock, referencesDock);

    // Terminal — embedded Konsole sessions, in the same bottom row.
    m_terminal = new TerminalPanel(this);
    auto *terminalDock = new QDockWidget(i18n("Terminal"), this);
    terminalDock->setObjectName(QStringLiteral("terminalDock"));
    terminalDock->setWindowIcon(QIcon::fromTheme(QStringLiteral("utilities-terminal")));
    terminalDock->setWidget(m_terminal);
    addDockWidget(Qt::BottomDockWidgetArea, terminalDock);
    tabifyDockWidget(problemsDock, terminalDock);
    problemsDock->raise();

    // Kate-style tool tabs: along the bottom edge, spanning the full width.
    setTabPosition(Qt::BottomDockWidgetArea, QTabWidget::South);
    setCorner(Qt::BottomLeftCorner, Qt::BottomDockWidgetArea);
    setCorner(Qt::BottomRightCorner, Qt::BottomDockWidgetArea);

    // Outline panel — the active file's symbols, tabbed with the file tree.
    auto *outline = new OutlinePanel(this);
    auto *outlineDock = new QDockWidget(i18n("Outline"), this);
    outlineDock->setObjectName(QStringLiteral("outlineDock"));
    outlineDock->setWidget(outline);
    addDockWidget(Qt::RightDockWidgetArea, outlineDock);
    tabifyDockWidget(treeDock, outlineDock);
    treeDock->raise();

    connect(m_lsp, &LspManager::definitionResolved, this,
            [this](const QString &path, int line) {
                m_editor->openFile(groupKey(), path, line);
            });
    connect(m_lsp, &LspManager::referencesResolved, references, &ReferencesPanel::setLocations);
    connect(m_lsp, &LspManager::referencesResolved, this,
            [referencesDock](const QList<Location> &) { referencesDock->raise(); });
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
    resizeDocks({problemsDock}, {230}, Qt::Vertical);

    connect(m_agent, &AgentDock::agentActivated, this, &MainWindow::onAgentActivated);
    connect(m_agent, &AgentDock::projectFocused, this,
            [this](const QString &path) { m_tree->setRoot(path); });
    connect(m_agent, &AgentDock::statusMessage, this,
            [this](const QString &t) { statusBar()->showMessage(t, 4000); });
    connect(m_agent, &AgentDock::openDiff, this,
            [this](const QString &title, const QString &diff) {
                m_editor->openDiff(groupKey(), title, diff);
            });

    connect(m_tree, &ProjectTree::fileActivated, this,
            [this](const QString &path) { m_editor->openFile(groupKey(), path); });
    connect(m_editor, &EditorArea::openFilesChanged, this, &MainWindow::pushOpenFilesToCore);
    connect(m_editor, &EditorArea::statusMessage, this,
            [this](const QString &t) { statusBar()->showMessage(t, 4000); });
    connect(m_editor, &EditorArea::currentFileChanged, this, [this](const QString &path) {
        if (m_core->isConnected()) {
            m_core->call(QStringLiteral("coop.setPresence"),
                         QJsonObject{{QStringLiteral("owner"), QStringLiteral("human")},
                                     {QStringLiteral("focusedFile"), path}});
        }
    });
    connect(m_editor, &EditorArea::documentOpened, this,
            [this](KTextEditor::Document *doc, const QString &) {
                m_lsp->documentOpened(doc, m_activeProject);
            });
    connect(m_editor, &EditorArea::documentClosed, this,
            [this](KTextEditor::Document *doc) { m_lsp->documentClosed(doc); });
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
    enterSendsAct->setChecked(agentCfg.readEntry("enterSends", false));
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

    auto *helpMenu = new KHelpMenu(this, KAboutData::applicationData());
    menuBar()->addMenu(helpMenu->menu());
}

void MainWindow::setupCore()
{
    m_coreStatus = new QLabel(i18n("Core: starting…"), this);
    statusBar()->addPermanentWidget(m_coreStatus);

    connect(m_core, &CoreClient::coreLog, this,
            [](const QString &line) { qInfo().noquote() << "[akcore]" << line; });
    connect(m_core, &CoreClient::failed, this, [this](const QString &msg) {
        m_coreStatus->setText(i18n("Core: failed"));
        qWarning().noquote() << "[core]" << msg;
    });
    connect(m_core, &CoreClient::disconnected, this,
            [this] { m_coreStatus->setText(i18n("Core: disconnected")); });
    connect(m_core, &CoreClient::connected, this, [this] {
        m_core->call(QStringLiteral("handshake"), {},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             m_coreStatus->setText(i18n("Core: handshake error"));
                             return;
                         }
                         m_coreStatus->setText(
                             i18n("Core: %1 %2",
                                  result.value(QStringLiteral("name")).toString(),
                                  result.value(QStringLiteral("version")).toString()));
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
