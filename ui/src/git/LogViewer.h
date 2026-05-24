// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QHash>
#include <QJsonObject>
#include <QString>
#include <QWidget>

class CommitDetailPanel;
class CoreClient;
class LogGraphDelegate;
class LogModel;
class QLabel;
class QPushButton;
class QTableView;

// LogViewer is the visual git history pane for the active agent. It glues
// together the building blocks shipped in Phase 5b:
//   - LogModel: paginated rows from git.log
//   - LogGraphDelegate: lane rails on the Graph column
//   - RefChipDelegate: branch/tag chips before the subject
//   - CommitDetailPanel: header + body + file list + diff for the selected row
//
// Source is driven externally by MainWindow via setActiveSource(projectPath,
// threadId). If the active agent has a running worktree we render that
// thread's log; otherwise we fall back to the project's workspace branch.
// Listens for git.log.invalidated to refetch the first page on HEAD moves and
// for git.invalidated to pick up a thread once the agent starts.
class LogViewer : public QWidget
{
    Q_OBJECT
public:
    explicit LogViewer(CoreClient *core, QWidget *parent = nullptr);

    // Switch to the active agent / project. threadId may be empty if the
    // agent hasn't started yet — we then show the workspace branch and
    // upgrade to the thread automatically once it appears in git.snapshot.
    void setActiveSource(const QString &projectPath, const QString &threadId);

private:
    void reloadFromFirstPage();
    void loadNextPage();
    void onSelectionChanged();
    void onNotification(const QString &method, const QJsonObject &params);
    void onScrolled(int value);
    void resolveThreadForProject();
    void updateLabel();

    // A log "source" is either an agent worktree (threadId set) or a
    // workspace branch (repoRoot + branch set).
    struct Source {
        QString threadId;
        QString repoRoot;
        QString branch;
    };

    CoreClient *m_core = nullptr;
    QLabel *m_sourceLabel = nullptr;
    QPushButton *m_refreshBtn = nullptr;
    QTableView *m_view = nullptr;
    LogModel *m_model = nullptr;
    LogGraphDelegate *m_graphDelegate = nullptr;
    CommitDetailPanel *m_detail = nullptr;

    QString m_activeProject;
    Source m_source;
    int m_loadToken = 0;
    bool m_pageInFlight = false;
    bool m_endReached = false;

    static constexpr int kPageSize = 200;
};
