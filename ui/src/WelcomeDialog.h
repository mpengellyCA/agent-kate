#pragma once

#include <QDialog>
#include <QString>
#include <QStringList>

class QListWidget;
class QListWidgetItem;
class QLabel;
class QPushButton;

// WelcomeDialog is the launch-time project picker. Shown before MainWindow when
// Agent Kate starts without a command-line path, so the user lands on a chosen
// project instead of whatever happens to be the current working directory
// (which, for the installed desktop entry, is $HOME).
//
// Resolution order: a recent-projects list (most recent first), explicit "Open
// folder…" / "New project…" actions, and a one-click "Reopen last project"
// button at the top. Returns the chosen path via selectedPath(); the caller
// should treat a rejected dialog as "user wants to quit".
class WelcomeDialog : public QDialog
{
    Q_OBJECT
public:
    explicit WelcomeDialog(QWidget *parent = nullptr);

    QString selectedPath() const { return m_selected; }
    // Every project the user asked to open. One entry for a normal pick; the
    // whole remembered set when they took "Reopen previous session" (audit
    // F47). Always starts with selectedPath(), so a caller that only handles
    // one project still behaves exactly as before.
    QStringList selectedPaths() const { return m_selectedPaths; }

private:
    void refreshList();
    void chooseFolder();
    void createNewProject();
    void reopenLast();
    void reopenSession();
    void accept(const QString &path);
    void acceptMany(const QStringList &paths);
    void onItemActivated(QListWidgetItem *item);
    void onRemoveCurrent();
    void onContextMenu(const QPoint &pos);
    void addRow(const QString &path, bool pinned);

    QString m_selected;
    QStringList m_selectedPaths;
    QLabel *m_lastLabel = nullptr;
    QPushButton *m_reopenButton = nullptr;
    QPushButton *m_sessionButton = nullptr; // "Reopen previous session (N projects)"
    QListWidget *m_list = nullptr;
    QLabel *m_emptyHint = nullptr; // shown instead of an empty recents list
    QLabel *m_listHint = nullptr;  // "Double-click to open · …", meaningless when empty
};
