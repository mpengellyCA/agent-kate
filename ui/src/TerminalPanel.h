#pragma once

#include <QString>
#include <QWidget>

class QTabWidget;

// TerminalPanel embeds Konsole terminal sessions, each in its own tab, so the
// user can run several shells (build scripts, dev servers, …) side by side.
// Every tab hosts a Konsole KPart — the native KDE terminal that Kate embeds
// too. New sessions start in the active project's directory.
class TerminalPanel : public QWidget
{
    Q_OBJECT
public:
    explicit TerminalPanel(QWidget *parent = nullptr);

    // Directory new terminals start in. Existing sessions keep their own cwd.
    void setWorkingDirectory(const QString &dir);

public Q_SLOTS:
    void newTerminal();

private:
    QWidget *createSession();

    QTabWidget *m_tabs = nullptr;
    QString m_workdir;
    bool m_konsoleMissing = false;
    int m_counter = 0;
};
