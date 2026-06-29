#pragma once

#include <KMainWindow>
#include <QHash>
#include <QSet>
#include <QString>

namespace KTextEditor {
class Document;
class View;
}
class GutterController;
class BlameController;

class CoreClient;
class EditorArea;
class ProjectTree;
class AgentDock;
class LspManager;
class SearchPanel;
class TerminalPanel;
class WorktreeDashboard;
class LogViewer;
class CoworkPanel;
class CooperationPanel;
class AiInspectorPanel;
class CoworkPortal;
class SideBar;
class ShellLayout;
class CommandPalette;
class QAction;
class QLabel;
class QLineEdit;
class QToolButton;

// MainWindow is the Agent Kate arena shell — a project-aware, agent-centric KDE
// main window. The agent roster (left) holds projects and their agents; the
// active agent drives the file tree and the editor on the right.
class MainWindow : public KMainWindow
{
    Q_OBJECT
public:
    explicit MainWindow(const QString &openPath = QString(), QWidget *parent = nullptr);
    ~MainWindow() override;

protected:
    void closeEvent(QCloseEvent *event) override;
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void setupUi();
    void setupActions();
    void setupHamburger();
    void setupTopToolbar();
    void setupShellShortcuts();
    void setupPerspectives();
    void setupCore();
    // Collect every leaf action in the window and show the searchable command
    // palette — the keyboard-first way to reach any feature.
    void showCommandPalette();

    // Experience level — "simple" hides power tooling (the Code menu and the
    // developer-only panels) so a newcomer sees only the essentials; "advanced"
    // restores the full surface. Persisted; new profiles start Simple.
    void setupExperience();
    void applyExperienceLevel(const QString &level, bool persist = true);
    void toggleExperienceLevel();
    // Show/hide one activity-rail tab without destroying its panel; collapses
    // the strip if the hidden tab was the raised one.
    void setPanelTabVisible(const QString &key, bool visible);
    // Friendly display name for a layout key (converse/build/split/review).
    static QString layoutDisplayName(const QString &key);
    void applyPerspective(const QString &name);
    void applyCentreMode(const QString &mode); // "editor" | "split" | "chat"

    // Panel placement: register every tool window through registerPanel so
    // the persisted location overrides (or the default strip) decide which
    // SideBar gets it. The key is a stable identifier used in KConfig.
    int registerPanel(const QString &key, const QIcon &icon,
                      const QString &label, QWidget *widget,
                      const QString &defaultStrip);
    SideBar *barByName(const QString &name) const;
    QString nameForBar(SideBar *bar) const;
    void showPanelContextMenu(SideBar *bar, int id, const QPoint &globalPos);
    void movePanelToStrip(const QString &key, const QString &targetStrip);
    void detachPanel(const QString &key);
    void reattachPanel(const QString &key); // floating → previous strip
    int panelId(const QString &key) const;
    SideBar *panelBar(const QString &key) const;
    void raisePanelByKey(const QString &key); // raise it (and re-attach if floating)
    void updateCursorStatus();
    void updateBreadcrumb(const QString &path);
    void updateAgentBadge();
    void updateLspStatus();    // refresh the status-bar language-server widget
    void onSave();
    void onSaveAll();
    void reloadExtensionServers();
    void onAgentActivated(int agentId, const QString &projectPath);
    void setTabsByAgent(bool byAgent);
    QString groupKey() const;
    void pushOpenFilesToCore();

    void persistShellState();
    // Persist/restore the editor's open tabs per project, so a restart reopens
    // the files the human was working on. Keyed by project path (agent ids may
    // be reassigned). m_restoringSession guards the replay from re-persisting.
    void persistEditorSession();
    void restoreEditorSession(const QString &projectPath);

    CoreClient *m_core = nullptr;
    // Set once the graceful stop-and-compact shutdown has run, so the re-entered
    // closeEvent takes the normal teardown path instead of re-showing the dialog.
    bool m_shutdownComplete = false;
    EditorArea *m_editor = nullptr;
    ProjectTree *m_tree = nullptr;
    AgentDock *m_agent = nullptr;
    LspManager *m_lsp = nullptr;
    TerminalPanel *m_terminal = nullptr;
    SearchPanel *m_search = nullptr;
    WorktreeDashboard *m_worktreeDashboard = nullptr;
    LogViewer *m_logViewer = nullptr;
    CoworkPanel *m_coworkPanel = nullptr;
    CooperationPanel *m_coopPanel = nullptr;
    AiInspectorPanel *m_inspectorPanel = nullptr;
    CoworkPortal *m_coworkPortal = nullptr;
    QLabel *m_gitStatusLabel = nullptr; // status-bar git widget for the active editor

    ShellLayout *m_shell = nullptr;
    CommandPalette *m_commandPalette = nullptr; // lazily created on first use
    SideBar *m_leftBar = nullptr;
    SideBar *m_rightBar = nullptr;
    SideBar *m_bottomBar = nullptr;

    // Stable key → id within whichever SideBar currently hosts the panel.
    // The bar pointer is looked up via m_panelHomes; keep both in sync.
    struct PanelInfo {
        QString key;
        QIcon icon;
        QString label;
        QWidget *widget = nullptr;
        SideBar *bar = nullptr;        // null while detached/floating
        int barId = -1;                // id within bar (>=0) or -1
        QWidget *floatingHost = nullptr; // window when detached
        QString lastStrip;             // remembered strip for re-attach
    };
    QHash<QString, PanelInfo> m_panels;       // by stable key
    QHash<QWidget *, QString> m_keyByWidget;  // for reverse lookup
    // Cached ids for the few panels MainWindow refers to by name (Files for
    // the breadcrumb, References for the LSP focus, Problems / Roster /
    // Git Log for perspectives). Updated by registerPanel + move.
    QString m_keyRoster = QStringLiteral("roster");
    QString m_keyFiles = QStringLiteral("files");
    QString m_keyOutline = QStringLiteral("outline");
    QString m_keySearch = QStringLiteral("search");
    QString m_keyWorktrees = QStringLiteral("worktrees");
    QString m_keyGitLog = QStringLiteral("gitlog");
    QString m_keyCoop = QStringLiteral("coop");
    QString m_keyCowork = QStringLiteral("cowork");
    QString m_keyInspector = QStringLiteral("inspector");
    QString m_keyTerminal = QStringLiteral("terminal");
    QString m_keyReferences = QStringLiteral("references");
    QString m_keyProblems = QStringLiteral("problems");
    QString m_keyOutput = QStringLiteral("output");
    QString m_keyTasks = QStringLiteral("tasks");

    int m_lastBottomTab = -1; // last tab raised by the user, used to restore on Ctrl+J
    QAction *m_toggleBottomAct = nullptr;
    // Opens a terminal in the active agent's worktree; enabled only while that
    // agent actually has a worktree directory.
    QAction *m_openWorktreeTerminalAct = nullptr;

    QMenu *m_perspectivesMenu = nullptr;
    QAction *m_centreEditorAct = nullptr;
    QAction *m_centreSplitAct = nullptr;
    QAction *m_centreChatAct = nullptr;
    QToolButton *m_layoutButton = nullptr; // top-toolbar "Layout ▾" preset switcher
    QString m_centreMode;

    // Experience level ("simple" / "advanced") and the chrome it gates.
    QString m_experienceLevel;
    bool m_firstRunProfile = false;        // captured before the shell migration
    QMenu *m_codeMenu = nullptr;           // hidden in Simple mode
    QList<QAction *> m_advancedActions;     // dev-only menu actions hidden in Simple
    QAction *m_simpleAct = nullptr;        // radio sync for the Experience menu
    QAction *m_advancedAct = nullptr;
    QToolButton *m_experienceButton = nullptr; // status-bar Simple/Advanced toggle
    QList<int> m_centreSplitSizes; // remembered horizontal split for restore

    // Top toolbar + status-bar widgets
    QWidget *m_breadcrumbWidget = nullptr; // hosts clickable segment buttons
    QLabel *m_breadcrumbLabel = nullptr;   // fallback when no file is open
    QLineEdit *m_toolbarSearch = nullptr; // top-toolbar Search box → SearchPanel
    QLabel *m_agentBadge = nullptr;       // top-toolbar agent chip
    QLabel *m_cursorPosLabel = nullptr;   // status bar: Ln 42 Col 17
    QLabel *m_modeLabel = nullptr;        // status bar: UTF-8 LF C++
    QToolButton *m_lspStatusButton = nullptr; // status bar: language-server state
    QAction *m_formatOnSave = nullptr;    // Code → Format on Save (KConfig-backed)
    QLabel *m_agentStatusLabel = nullptr; // status bar (rightmost): agent name + dot
    KTextEditor::View *m_observedView = nullptr; // currently wired-for-cursor

    QHash<KTextEditor::Document *, GutterController *> m_gutters;
    QHash<KTextEditor::Document *, BlameController *> m_blames;
    QAction *m_blameToggle = nullptr;
    QString m_activeFilePath;

    QString m_activeProject;
    int m_activeAgentId = -1;
    bool m_tabsByAgent = false; // editor tab grouping: false = project, true = agent

    // Session restore: projects whose saved tabs have already been replayed
    // (each project restores once per app run), and a guard so the replay's
    // openFile calls don't re-trigger persistence.
    QSet<QString> m_restoredSessions;
    bool m_restoringSession = false;
};
