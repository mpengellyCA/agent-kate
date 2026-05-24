// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#pragma once

#include <QDialog>
#include <QJsonObject>
#include <QString>
#include <QStringList>

class CoreClient;
class QLabel;
class QListWidget;
class QPushButton;
class QTimer;

// ConflictDialog drives the post-Land conflict-resolution flow. It opens with
// the list of unmerged paths the git.land response returned, offers KDiff3 as
// the resolver, then polls the workspace's merge state so the human can
// click Finalize once they're done in KDiff3 (or Abort to roll the merge back).
class ConflictDialog : public QDialog
{
    Q_OBJECT
public:
    ConflictDialog(CoreClient *core, const QString &threadId, const QString &branch,
                   const QString &into, const QStringList &initialConflicts,
                   QWidget *parent = nullptr);

Q_SIGNALS:
    // Reported back so the dashboard can post a status-bar toast.
    void finalized(const QString &threadId, const QString &into);
    void aborted(const QString &threadId);

private:
    void refreshStatus();
    void onOpenTool();
    void onFinalize();
    void onAbort();

    CoreClient *m_core;
    QString m_threadId;
    QString m_branch;
    QString m_into;

    QLabel *m_summary = nullptr;
    QListWidget *m_files = nullptr;
    QPushButton *m_openBtn = nullptr;
    QPushButton *m_finalizeBtn = nullptr;
    QPushButton *m_abortBtn = nullptr;
    QPushButton *m_closeBtn = nullptr;
    QTimer *m_pollTimer = nullptr;
    bool m_inFlight = false;
};
