#pragma once

#include <QPoint>
#include <QString>
#include <QWidget>

class QTabWidget;

// TerminalPanel embeds Konsole terminal sessions, each in its own tab, so the
// user can run several shells (build scripts, dev servers, …) side by side.
// Every tab hosts a Konsole KPart — the native KDE terminal that Kate embeds
// too. Tabs are scoped to the project they were started under: sessions keep
// running across project switches, but only tabs belonging to the active
// project are visible.
class TerminalPanel : public QWidget
{
    Q_OBJECT
public:
    explicit TerminalPanel(QWidget *parent = nullptr);
    ~TerminalPanel() override;

    // Switch to a project: hides tabs from other projects, shows ones from
    // this project, creating a first tab if none exist yet for it. Existing
    // sessions are NOT destroyed — they keep running in the background.
    void setWorkingDirectory(const QString &dir);

    // Open a new tab whose shell starts in the given directory. The tab is
    // still scoped to the currently active project, so it stays visible only
    // while that project is active. Used by ProjectTree's "Open Terminal Here".
    void openTerminalAt(const QString &dir);

    // Open (or focus) a terminal in `dir` and run `command` in it. Used by
    // ProjectTree's "Run Command Here".
    void runCommandAt(const QString &dir, const QString &command);

    // Move keyboard focus into the active tab's terminal widget.
    void focusActiveTerminal();

    // Cycle to the next / previous visible terminal tab of the active project.
    void nextTerminal();
    void previousTerminal();

    // True when the Konsole KPart is installed and usable. When false the panel
    // shows an explanatory message and refuses to spawn sessions; callers should
    // disable terminal actions accordingly.
    bool isAvailable() const { return !m_konsoleMissing; }

    // Persist / restore the SET of open tabs (project + last cwd + custom
    // title) — NOT scrollback or live shell state. restoreSession is lazy: tabs
    // are only materialised the first time their project becomes active.
    void saveSession();

public Q_SLOTS:
    void newTerminal();

    // Tear down every tab that belongs to `project`. Called ONLY on explicit
    // project CLOSE — never on a project switch, where sessions must survive.
    void closeProject(const QString &project);

private Q_SLOTS:
    // Connected (string-based) to the konsolepart's runtime
    // currentDirectoryChanged(QString) signal; uses sender() to find the tab.
    void onPartDirectoryChanged(const QString &dir);

private:
    QWidget *createSession(const QString &cwd);
    void applyVisibility();
    void closeTab(int index);
    void onTabContextMenu(const QPoint &pos);
    void renameTab(int index);
    void maybeRestoreFor(const QString &project);

    QTabWidget *m_tabs = nullptr;
    QString m_workdir;
    bool m_konsoleMissing = false;
    QWidget *m_addButton = nullptr;
    // Set in the destructor, before the base ~QWidget deletes our children. The
    // parts' destroyed-handlers reach back into m_tabs, which is itself being
    // torn down during shutdown — this flag tells them to stand down.
    bool m_shuttingDown = false;
};
