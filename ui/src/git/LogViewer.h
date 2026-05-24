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
class QComboBox;
class QPushButton;
class QTableView;

// LogViewer is the visual git history pane for a single worktree. It glues
// together the building blocks shipped in Phase 5b:
//   - LogModel: paginated rows from git.log
//   - LogGraphDelegate: lane rails on the Graph column
//   - RefChipDelegate: branch/tag chips before the subject
//   - CommitDetailPanel: header + body + file list + diff for the selected row
//
// The viewer owns its own thread picker, populated from git.snapshot, so it is
// usable standalone. It listens for git.log.invalidated to refetch the first
// page when HEAD moves under it.
class LogViewer : public QWidget
{
    Q_OBJECT
public:
    explicit LogViewer(CoreClient *core, QWidget *parent = nullptr);

    // Select a specific worktree by threadId — if it isn't in the picker yet
    // we still set it and the snapshot refresh will surface it shortly.
    void setThreadId(const QString &threadId);

protected:
    void showEvent(QShowEvent *e) override;

private:
    void refreshThreads();
    void reloadFromFirstPage();
    void loadNextPage();
    void onSelectionChanged();
    void onNotification(const QString &method, const QJsonObject &params);
    void onScrolled(int value);

    // A log "source" is either an agent worktree (threadId set) or a
    // workspace branch (repoRoot + branch set).
    struct Source {
        QString threadId;
        QString repoRoot;
        QString branch;
    };
    Source sourceForIndex(int idx) const;

    CoreClient *m_core = nullptr;
    QComboBox *m_threadPicker = nullptr;
    QPushButton *m_refreshBtn = nullptr;
    QTableView *m_view = nullptr;
    LogModel *m_model = nullptr;
    LogGraphDelegate *m_graphDelegate = nullptr;
    CommitDetailPanel *m_detail = nullptr;

    Source m_source;
    // Bumped on every reload so in-flight page replies for a stale query are
    // dropped silently.
    int m_loadToken = 0;
    bool m_pageInFlight = false;
    bool m_endReached = false;
    // Once-per-show snapshot refresh so the picker is populated without
    // hammering git.snapshot on every poll.
    bool m_threadsLoaded = false;

    static constexpr int kPageSize = 200;
};
