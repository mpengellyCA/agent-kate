#include "MainWindow.h"
#include "AgentDock.h"
#include "AgentPanel.h"
#include "AppearanceDialog.h"
#include "CommandPalette.h"
#include "EditorArea.h"
#include "ExtensionsDialog.h"
#include "OutlinePanel.h"
#include "ProblemsPanel.h"
#include "ProjectTree.h"
#include "ProvidersDialog.h"
#include "ReferencesPanel.h"
#include "ShutdownDialog.h"
#include "SearchPanel.h"
#include "SessionBrowserDialog.h"
#include "SkillsDialog.h"
#include "TerminalPanel.h"
#include "WelcomeDialog.h"
#include "WorktreeDashboard.h"
#include "AiInspectorPanel.h"
#include "CooperationPanel.h"
#include "cowork/CoworkPanel.h"
#include "cowork/CoworkPortal.h"
#include "shell/ShellLayout.h"
#include "shell/SideBar.h"
#include "shell/StubPanel.h"
#include "git/BlameController.h"
#include "git/GutterController.h"
#include "git/LogViewer.h"
#include "ipc/CoreClient.h"

#include <KTextEditor/Cursor>
#include <KTextEditor/Document>
#include <KTextEditor/Range>
#include <KTextEditor/View>
#include "lsp/LspActions.h"
#include "lsp/LspClient.h"
#include "lsp/LspManager.h"
#include "lsp/WorkspaceSymbolDialog.h"

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
#include <QApplication>
#include <functional>
#include <QCloseEvent>
#include <QCoreApplication>
#include <QCursor>
#include <QDebug>
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
#include <QShortcut>
#include <QSplitter>
#include <QStackedWidget>
#include <QStatusBar>
#include <QTabWidget>
#include <QTimer>
#include <QToolBar>
#include <QToolButton>

MainWindow::MainWindow(const QString &openPath, QWidget *parent)
    : KMainWindow(parent)
{
    setWindowTitle(i18n("Agent Kate"));
    m_tabsByAgent =
        KSharedConfig::openConfig()->group(QStringLiteral("Editor"))
            .readEntry("tabsByAgent", false);
    // Capture first-run BEFORE setupUi() bumps the View schema: a brand-new
    // profile (no schema yet) gets the friendly Simple default; everyone else
    // keeps the Advanced surface they already know.
    m_firstRunProfile =
        KSharedConfig::openConfig()->group(QStringLiteral("View"))
            .readEntry("schema", 0) == 0;

    setupUi();
    setupActions();
    setupTopToolbar();
    setupHamburger();
    setupShellShortcuts();
    setupPerspectives();
    setupCore();
    setupExperience();

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
    auto *coreLogView = new QPlainTextEdit(this);
    coreLogView->setReadOnly(true);
    coreLogView->setMaximumBlockCount(5000);
    coreLogView->setFrameShape(QFrame::NoFrame);
    registerPanel(m_keyOutput, QIcon::fromTheme(QStringLiteral("utilities-log-viewer")),
                  i18n("Output"), coreLogView, QStringLiteral("bottom"));
    registerPanel(m_keyTasks, QIcon::fromTheme(QStringLiteral("view-task")),
                  i18n("Tasks"),
                  new StubPanel(i18n("Tasks / Hooks"),
                                i18n("Background tasks, hook runs, and queued work appear here."),
                                this),
                  QStringLiteral("bottom"));

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
        {m_keyWorktrees, i18n("Each agent works in its own isolated copy of the project "
                              "(a git worktree). Review and manage them here.")},
        {m_keyGitLog, i18n("The commit history for the active project.")},
        {m_keyCoop, i18n("See who — human or agent — is editing what, in real time.")},
        {m_keyCowork, i18n("Let an agent see and control your desktop, only with your permission.")},
        {m_keyInspector, i18n("A live view of the agent's model, token use, cost and tool calls.")},
        {m_keyTerminal, i18n("A built-in command-line terminal.")},
        {m_keyReferences, i18n("Everywhere the selected symbol is used.")},
        {m_keyProblems, i18n("Errors and warnings from the language tools.")},
        {m_keyOutput, i18n("Background log output from Agent Kate's core.")},
        {m_keyTasks, i18n("Background tasks, hook runs and queued work.")},
    };
    for (auto it = panelHelp.constBegin(); it != panelHelp.constEnd(); ++it) {
        SideBar *bar = panelBar(it.key());
        const int id = panelId(it.key());
        if (!bar || id < 0) {
            continue;
        }
        if (auto *tab = bar->tabBar()->tab(id)) {
            QString title = bar->panelLabel(id);
            title.replace(QLatin1String("&&"), QLatin1String("&"));
            tab->setToolTip(QStringLiteral("<b>%1</b><p>%2</p>")
                                .arg(title.toHtmlEscaped(), it.value().toHtmlEscaped()));
        }
    }
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
    saveAllAct->setShortcut(QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_S));
    connect(saveAllAct, &QAction::triggered, this, &MainWindow::onSaveAll);
    fileMenu->addAction(saveAllAct);

    fileMenu->addSeparator();
    fileMenu->addAction(KStandardAction::quit(this, &QWidget::close, this));

    // The Agent menu sits right after File — agents are the primary thing this
    // app is about, and its actions were previously buried in roster right-clicks.
    setupAgentMenu();

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

    // Autosave: write file edits to disk automatically ~1s after you stop
    // typing (and on focus loss). On by default; persisted in [Editor] autosave.
    const KConfigGroup editorCfg =
        KSharedConfig::openConfig()->group(QStringLiteral("Editor"));
    const bool autosaveOn = editorCfg.readEntry("autosave", true);
    auto *autosaveAct = optionsMenu->addAction(i18n("&Autosave files"));
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

    optionsMenu->addSeparator();
    auto *providersAct = optionsMenu->addAction(i18n("Configure API &Providers…"));
    providersAct->setToolTip(i18n(
        "Configure third-party, Anthropic-compatible API providers (Fireworks, "
        "OpenRouter, …) that an agent can use in place of Anthropic."));
    connect(providersAct, &QAction::triggered, this, [this] {
        ProvidersDialog dlg(this);
        if (dlg.exec() == QDialog::Accepted) {
            m_agent->reloadProviders();
        }
    });

    optionsMenu->addSeparator();
    auto *appearanceAct = optionsMenu->addAction(
        QIcon::fromTheme(QStringLiteral("preferences-desktop-color")),
        i18n("&Appearance…"));
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
    m_simpleAct = expMenu->addAction(i18n("&Simple — just the essentials"));
    m_advancedAct = expMenu->addAction(i18n("&Advanced — every developer tool"));
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

    auto *paletteAct = viewMenu->addAction(
        QIcon::fromTheme(QStringLiteral("show-menu")), i18n("&Command Palette…"));
    // Ctrl+Shift+P is the convention; Ctrl+P is a friendly second binding since
    // Agent Kate has no print action to clash with.
    paletteAct->setShortcuts({QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_P),
                              QKeySequence(Qt::CTRL | Qt::Key_P)});
    paletteAct->setToolTip(
        i18n("Search and run any command by name — the fastest way to reach "
             "every feature."));
    connect(paletteAct, &QAction::triggered, this, &MainWindow::showCommandPalette);
    viewMenu->addSeparator();
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
                                                    : panelId(m_keyProblems);
            m_bottomBar->setRaisedId(target);
        }
    });

    auto *findInProjAct = viewMenu->addAction(
        QIcon::fromTheme(QStringLiteral("edit-find")),
        i18n("Find in &Project…"));
    findInProjAct->setShortcut(Qt::CTRL | Qt::SHIFT | Qt::Key_F);
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
    auto *nextMatchAct = new QAction(i18n("Next Search Match"), this);
    nextMatchAct->setShortcut(Qt::Key_F3);
    nextMatchAct->setShortcutContext(Qt::WidgetWithChildrenShortcut);
    connect(nextMatchAct, &QAction::triggered, this, [this] {
        if (m_search) {
            m_search->focusNextResult();
        }
    });
    auto *prevMatchAct = new QAction(i18n("Previous Search Match"), this);
    prevMatchAct->setShortcut(Qt::SHIFT | Qt::Key_F3);
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
    auto *newTermAct = viewMenu->addAction(
        QIcon::fromTheme(QStringLiteral("utilities-terminal")), i18n("&New Terminal"));
    newTermAct->setShortcut(Qt::CTRL | Qt::SHIFT | Qt::Key_T);
    newTermAct->setEnabled(termOk);
    connect(newTermAct, &QAction::triggered, this, [this] {
        if (!m_terminal) {
            return;
        }
        raisePanelByKey(m_keyTerminal);
        m_terminal->newTerminal();
    });

    auto *focusTermAct = viewMenu->addAction(i18n("&Focus Terminal"));
    focusTermAct->setShortcut(Qt::CTRL | Qt::Key_QuoteLeft);
    focusTermAct->setEnabled(termOk);
    connect(focusTermAct, &QAction::triggered, this, [this] {
        if (!m_terminal) {
            return;
        }
        raisePanelByKey(m_keyTerminal);
        m_terminal->focusActiveTerminal();
    });

    auto *nextTermAct = viewMenu->addAction(i18n("Next Terminal"));
    nextTermAct->setShortcut(Qt::CTRL | Qt::Key_PageDown);
    nextTermAct->setEnabled(termOk);
    connect(nextTermAct, &QAction::triggered, this, [this] {
        if (m_terminal) {
            m_terminal->nextTerminal();
        }
    });

    auto *prevTermAct = viewMenu->addAction(i18n("Previous Terminal"));
    prevTermAct->setShortcut(Qt::CTRL | Qt::Key_PageUp);
    prevTermAct->setEnabled(termOk);
    connect(prevTermAct, &QAction::triggered, this, [this] {
        if (m_terminal) {
            m_terminal->previousTerminal();
        }
    });

    m_openWorktreeTerminalAct = viewMenu->addAction(
        QIcon::fromTheme(QStringLiteral("utilities-terminal")),
        i18n("Open Terminal in &Worktree"));
    // Ctrl+Shift+T is "New Terminal" here; Ctrl+Alt+T is free.
    m_openWorktreeTerminalAct->setShortcut(Qt::CTRL | Qt::ALT | Qt::Key_T);
    m_openWorktreeTerminalAct->setToolTip(
        i18n("Open a terminal rooted in the active agent's worktree."));
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
    m_advancedActions = {m_blameToggle, newTermAct, focusTermAct, nextTermAct,
                         prevTermAct, m_openWorktreeTerminalAct,
                         m_agentCommitAct, m_agentPrAct, m_agentMergeAct,
                         m_agentTerminalAct, m_agentDiscardAct};

    m_codeMenu = menuBar()->addMenu(i18n("&Code"));
    QMenu *codeMenu = m_codeMenu;
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

    QAction *symbolAct = codeMenu->addAction(QIcon::fromTheme(QStringLiteral("code-context")),
                                             i18n("Go to &Symbol in Workspace…"));
    symbolAct->setShortcut(Qt::CTRL | Qt::Key_T);
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

    QAction *quickFixAct = codeMenu->addAction(QIcon::fromTheme(QStringLiteral("tools-wizard")),
                                               i18n("&Quick Fix…"));
    quickFixAct->setShortcut(Qt::CTRL | Qt::Key_Period);
    connect(quickFixAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->requestCodeActions(view);
        }
    });

    QAction *renameAct = codeMenu->addAction(i18n("Rena&me Symbol"));
    renameAct->setShortcut(Qt::Key_F2);
    connect(renameAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->renameSymbol(view);
        }
    });

    QAction *formatAct = codeMenu->addAction(i18n("&Format Document"));
    formatAct->setShortcut(Qt::CTRL | Qt::ALT | Qt::Key_L);
    connect(formatAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->formatDocument(view);
        }
    });

    m_formatOnSave = codeMenu->addAction(i18n("Format on &Save"));
    m_formatOnSave->setCheckable(true);
    m_formatOnSave->setChecked(KSharedConfig::openConfig()
                                   ->group(QStringLiteral("CodeIntelligence"))
                                   .readEntry("formatOnSave", false));
    connect(m_formatOnSave, &QAction::toggled, this, [](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("CodeIntelligence"))
            .writeEntry("formatOnSave", on);
    });

    QAction *sigAct = codeMenu->addAction(i18n("Show Signature &Help"));
    sigAct->setShortcut(QKeySequence(Qt::CTRL | Qt::SHIFT | Qt::Key_Space));
    connect(sigAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->requestSignatureHelp(view);
        }
    });

    QAction *nextProbAct = codeMenu->addAction(i18n("&Next Problem"));
    nextProbAct->setShortcut(Qt::Key_F8);
    connect(nextProbAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->nextProblem(view);
        }
    });

    QAction *prevProbAct = codeMenu->addAction(i18n("&Previous Problem"));
    prevProbAct->setShortcut(Qt::SHIFT | Qt::Key_F8);
    connect(prevProbAct, &QAction::triggered, this, [this] {
        if (KTextEditor::View *view = m_editor->currentView()) {
            m_lsp->prevProblem(view);
        }
    });

    QAction *restartAct = codeMenu->addAction(QIcon::fromTheme(QStringLiteral("view-refresh")),
                                              i18n("&Restart Language Server"));
    connect(restartAct, &QAction::triggered, this, [this] {
        if (!m_activeFilePath.isEmpty()) {
            m_lsp->restartServersForCurrentFile(m_activeFilePath);
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

void MainWindow::showCommandPalette()
{
    if (!m_commandPalette) {
        m_commandPalette = new CommandPalette(this);
    }
    // Gather every leaf action reachable from the menu bar. Because the menu
    // bar is the single source of truth (the hamburger mirrors it), this one
    // walk surfaces File/Options/View/Code/Help plus perspectives and the
    // centre-mode toggles — the whole feature surface, searchable by name.
    QList<QAction *> actions;
    QSet<QAction *> seen;
    std::function<void(QMenu *)> walk = [&](QMenu *menu) {
        for (QAction *a : menu->actions()) {
            if (a->isSeparator()) {
                continue;
            }
            if (a->menu()) {
                if (a->isVisible()) { // skip menus hidden by Simple mode
                    walk(a->menu());
                }
                continue;
            }
            if (!seen.contains(a)) {
                seen.insert(a);
                actions << a;
            }
        }
    };
    for (QAction *top : menuBar()->actions()) {
        if (top->menu()) {
            if (top->isVisible()) {
                walk(top->menu());
            }
        } else if (!top->isSeparator() && !seen.contains(top)) {
            seen.insert(top);
            actions << top;
        }
    }
    m_commandPalette->setActions(actions);
    m_commandPalette->showPalette();
}

// setupAgentMenu builds the &Agent menu and wires every entry to AgentDock's
// active-agent surface. Enable-state tracks the active agent (updateAgentActions).
void MainWindow::setupAgentMenu()
{
    m_agentMenu = menuBar()->addMenu(i18n("&Agent"));

    auto *newAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("list-add")), i18n("&New Agent"));
    newAct->setToolTip(i18n("Describe a task and start a fresh agent in the current project."));
    connect(newAct, &QAction::triggered, this,
            [this] { m_agent->newAgentInActiveProjectGuided(); });

    m_agentRenameAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("document-edit")), i18n("&Rename Agent…"));
    connect(m_agentRenameAct, &QAction::triggered, this,
            [this] { m_agent->renameActiveAgent(); });

    m_agentResumeAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("media-playback-start")), i18n("Res&ume Agent"));
    m_agentResumeAct->setToolTip(i18n("Relaunch a paused agent and continue its conversation."));
    connect(m_agentResumeAct, &QAction::triggered, this,
            [this] { m_agent->resumeActiveAgent(); });

    m_agentMenu->addSeparator();

    m_agentAttachAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("mail-attachment")), i18n("&Attach Files…"));
    m_agentAttachAct->setToolTip(i18n("Give the active agent files as context for its next message."));
    connect(m_agentAttachAct, &QAction::triggered, this,
            [this] { m_agent->attachToActiveAgent(); });

    m_agentChangesAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("vcs-diff")), i18n("Show &Changes"));
    m_agentChangesAct->setToolTip(i18n("Review the changes the active agent has made."));
    connect(m_agentChangesAct, &QAction::triggered, this,
            [this] { m_agent->showActiveAgentChanges(); });

    m_agentStopAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("process-stop")), i18n("&Stop Agent"));
    m_agentStopAct->setToolTip(i18n("Stop the running agent (it stays available to resume)."));
    connect(m_agentStopAct, &QAction::triggered, this,
            [this] { m_agent->stopActiveAgent(); });

    m_agentMenu->addSeparator();

    // Git / worktree lifecycle — hidden in Simple mode (added to m_advancedActions
    // in setupActions, which runs after this).
    m_agentCommitAct = m_agentMenu->addAction(i18n("&Commit Changes…"));
    connect(m_agentCommitAct, &QAction::triggered, this,
            [this] { m_agent->commitActiveAgent(); });
    m_agentPrAct = m_agentMenu->addAction(i18n("Create &Pull Request…"));
    connect(m_agentPrAct, &QAction::triggered, this,
            [this] { m_agent->createPullRequestForActiveAgent(); });
    m_agentMergeAct = m_agentMenu->addAction(i18n("&Merge into Local Main…"));
    connect(m_agentMergeAct, &QAction::triggered, this,
            [this] { m_agent->mergeActiveAgent(); });
    m_agentTerminalAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("utilities-terminal")),
        i18n("Open &Terminal in Worktree"));
    connect(m_agentTerminalAct, &QAction::triggered, this,
            [this] { m_agent->openActiveAgentTerminal(); });

    m_agentMenu->addSeparator();

    m_agentTagsAct = m_agentMenu->addAction(
        QIcon::fromTheme(QStringLiteral("tag")), i18n("Edit &Tags…"));
    connect(m_agentTagsAct, &QAction::triggered, this,
            [this] { m_agent->editActiveAgentTags(); });

    m_agentDiscardAct = m_agentMenu->addAction(i18n("&Discard Worktree"));
    m_agentDiscardAct->setToolTip(i18n("Throw away the agent's working copy and its changes."));
    connect(m_agentDiscardAct, &QAction::triggered, this,
            [this] { m_agent->discardActiveAgentWorktree(); });

    m_agentCloseAct = m_agentMenu->addAction(i18n("&Close Agent"));
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
    m_agentRenameAct->setEnabled(has);
    m_agentResumeAct->setEnabled(dormant);
    m_agentAttachAct->setEnabled(has && !dormant);
    m_agentChangesAct->setEnabled(has);
    m_agentStopAct->setEnabled(running);
    m_agentCommitAct->setEnabled(worktree);
    m_agentPrAct->setEnabled(worktree);
    m_agentMergeAct->setEnabled(worktree);
    m_agentTerminalAct->setEnabled(worktree);
    m_agentTagsAct->setEnabled(has);
    m_agentDiscardAct->setEnabled(worktree);
    m_agentCloseAct->setEnabled(has);
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
void MainWindow::setupShellShortcuts()
{
    auto bindRaise = [this](SideBar *bar, QKeyCombination base) {
        for (int i = 0; i < 9; ++i) {
            const QKeySequence seq(
                QKeyCombination(base.keyboardModifiers(),
                                static_cast<Qt::Key>(Qt::Key_1 + i)));
            auto *sc = new QShortcut(seq, this);
            connect(sc, &QShortcut::activated, this, [bar, i] {
                const int id = bar->panelIdAt(i);
                if (id >= 0) {
                    bar->setRaisedId(bar->raisedId() == id ? -1 : id);
                }
            });
        }
        const QKeySequence collapseSeq(
            QKeyCombination(base.keyboardModifiers(), Qt::Key_0));
        auto *collapse = new QShortcut(collapseSeq, this);
        connect(collapse, &QShortcut::activated, this,
                [bar] { bar->setRaisedId(-1); });
    };
    bindRaise(m_leftBar, QKeyCombination(Qt::AltModifier, Qt::Key_0));
    bindRaise(m_rightBar,
              QKeyCombination(Qt::ControlModifier | Qt::AltModifier, Qt::Key_0));

    // Ctrl+E toggles between editor-only and split; Ctrl+Shift+E toggles
    // between chat-only and split. Both route through applyCentreMode so the
    // top-toolbar buttons stay in sync.
    auto *focusEditor = new QShortcut(Qt::CTRL | Qt::Key_E, this);
    connect(focusEditor, &QShortcut::activated, this, [this] {
        applyCentreMode(m_centreMode == QLatin1String("editor")
                            ? QStringLiteral("split")
                            : QStringLiteral("editor"));
    });
    auto *focusAgent = new QShortcut(Qt::CTRL | Qt::SHIFT | Qt::Key_E, this);
    connect(focusAgent, &QShortcut::activated, this, [this] {
        applyCentreMode(m_centreMode == QLatin1String("chat")
                            ? QStringLiteral("split")
                            : QStringLiteral("chat"));
    });
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
    auto add = [this](const QString &label, const QString &key, const QString &tip,
                      const QKeySequence &shortcut = QKeySequence()) {
        QAction *act = m_perspectivesMenu->addAction(label);
        act->setToolTip(tip);
        if (!shortcut.isEmpty()) {
            act->setShortcut(shortcut);
        }
        connect(act, &QAction::triggered, this, [this, key] { applyPerspective(key); });
    };
    add(i18n("&Converse"), QStringLiteral("converse"),
        i18n("Focus the conversation with your agent."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_1);
    add(i18n("&Build"), QStringLiteral("build"),
        i18n("Focus the code editor and files."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_2);
    add(i18n("&Review"), QStringLiteral("review"),
        i18n("Editor and agent side by side, with changes and history."),
        Qt::CTRL | Qt::SHIFT | Qt::Key_3);
    add(i18n("&Side by Side"), QStringLiteral("split"),
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
    // The UI handshake (claiming the "ui" role) is now sent by CoreClient as the first
    // frame on connect, so by the time connected() fires the role is already being
    // established ahead of any UI-only query. Just do the post-connect work here.
    connect(m_core, &CoreClient::connected, this, [this] {
        pushOpenFilesToCore();
        reloadExtensionServers();
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
    if (m_openWorktreeTerminalAct) {
        m_openWorktreeTerminalAct->setEnabled(
            m_agent && !m_agent->worktreePathForAgent(agentId).isEmpty());
    }
    setWindowTitle(i18n("Agent Kate — %1", QDir(projectPath).dirName()));
    m_editor->setActiveGroup(groupKey());
    // Reopen the tabs the human had for this project last run (once per run).
    restoreEditorSession(projectPath);
    updateAgentActions();
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
// can reopen them. Each group is keyed by its own group key (a project path, or
// "agent-N" when tabs are grouped by agent) — the same key restoreEditorSession
// reads back — so the working set for *all* open projects survives a quit, not
// just the active one.
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
        if (key.isEmpty()) {
            continue;
        }
        KConfigGroup grp = sessions.group(key);
        grp.writeEntry("openFiles", m_editor->openFilePathsForGroup(key));
        grp.writeEntry("active", m_editor->currentPathForGroup(key));
    }
}

// restoreEditorSession replays the current group's saved tabs once per app run.
// Skips files that no longer exist, caps the count to keep startup snappy, and
// guards re-entrant persistence while replaying. Keyed by the active group key
// (project path or "agent-N"), matching how persistEditorSession wrote it.
void MainWindow::restoreEditorSession(const QString &projectPath)
{
    Q_UNUSED(projectPath);
    const QString key = groupKey();
    if (key.isEmpty() || m_restoredSessions.contains(key)) {
        return;
    }
    m_restoredSessions.insert(key);

    const KConfigGroup grp = KSharedConfig::openConfig()
                                 ->group(QStringLiteral("Editor"))
                                 .group(QStringLiteral("Sessions"))
                                 .group(key);
    const QStringList files = grp.readEntry("openFiles", QStringList());
    if (files.isEmpty()) {
        return;
    }
    const QString active = grp.readEntry("active", QString());

    // Cap restored tabs so a session with many heavy viewers (PDFs) doesn't
    // stall startup; the rest stay one click away in the tree.
    constexpr int kMaxRestore = 20;

    m_restoringSession = true;
    int opened = 0;
    for (const QString &path : files) {
        if (opened >= kMaxRestore) {
            break;
        }
        if (QFileInfo::exists(path)) {
            m_editor->openFile(key, path);
            ++opened;
        }
    }
    // Re-activate the previously-focused file if it was restored.
    if (!active.isEmpty() && QFileInfo::exists(active)) {
        m_editor->openFile(key, active);
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
    // Prompt to save any modified documents; a cancel aborts the close.
    if (m_editor && !m_editor->confirmCloseAll()) {
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
    auto addLayoutItem = [this, layoutMenu](const QString &key) {
        QAction *a = layoutMenu->addAction(layoutDisplayName(key));
        connect(a, &QAction::triggered, this, [this, key] { applyPerspective(key); });
    };
    addLayoutItem(QStringLiteral("converse"));
    addLayoutItem(QStringLiteral("build"));
    addLayoutItem(QStringLiteral("split"));
    addLayoutItem(QStringLiteral("review"));
    m_layoutButton->setMenu(layoutMenu);
    toolbar->addWidget(m_layoutButton);
    toolbar->addSeparator();

    // Centre-slab mode toggle: Editor / Split / Chat. The three actions form
    // an exclusive group so exactly one stays raised; applyCentreMode hides
    // the inactive halves of the centre split and persists the choice.
    auto *modeGroup = new QActionGroup(this);
    modeGroup->setExclusive(true);
    m_centreEditorAct = new QAction(
        QIcon::fromTheme(QStringLiteral("document-edit")), i18n("Editor"), this);
    m_centreSplitAct = new QAction(
        QIcon::fromTheme(QStringLiteral("view-split-left-right")),
        i18n("Split"), this);
    m_centreChatAct = new QAction(
        QIcon::fromTheme(QStringLiteral("im-user")), i18n("Chat"), this);
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
