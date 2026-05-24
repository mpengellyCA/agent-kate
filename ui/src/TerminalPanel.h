#pragma once

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

    // Switch to a project: hides tabs from other projects, shows ones from
    // this project, creating a first tab if none exist yet for it. Existing
    // sessions are NOT destroyed — they keep running in the background.
    void setWorkingDirectory(const QString &dir);

public Q_SLOTS:
    void newTerminal();

private:
    QWidget *createSession();
    void applyVisibility();

    QTabWidget *m_tabs = nullptr;
    QString m_workdir;
    bool m_konsoleMissing = false;
    int m_counter = 0;
};
