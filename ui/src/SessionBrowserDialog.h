#pragma once

#include <QDialog>

class CoreClient;
class QJsonArray;
class QLabel;
class QLineEdit;
class QListWidget;
class QPushButton;

// SessionBrowserDialog lists every Claude Code session found on disk and lets
// the user attach one — pulling that conversation into Agent Kate as a thread,
// even if Agent Kate did not start it. Backed by the core's session.browse and
// session.attach IPC.
class SessionBrowserDialog : public QDialog
{
    Q_OBJECT
public:
    explicit SessionBrowserDialog(CoreClient *core, QWidget *parent = nullptr);

Q_SIGNALS:
    // Emitted once a session has been attached as thread threadId.
    void attachRequested(const QString &project, const QString &threadId,
                         const QString &title);

private:
    void refresh();
    void populate(const QJsonArray &sessions);
    void applyFilter();
    void attachSelected();

    CoreClient *m_core = nullptr;
    QLineEdit *m_search = nullptr;
    QListWidget *m_list = nullptr;
    QLabel *m_status = nullptr;
    QPushButton *m_attachButton = nullptr;
};
