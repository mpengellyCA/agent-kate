// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "LogModel.h"
#include "state/Reactive.h"

#include <QHash>
#include <QJsonObject>
#include <QPoint>
#include <QString>
#include <QVector>
#include <QWidget>

class CommitDetailPanel;
class CoreClient;
class ElidingLabel;
class LogGraphDelegate;
class QComboBox;
class QLabel;
class QLineEdit;
class QModelIndex;
class QPushButton;
class QStackedWidget;
class QTableView;
class QTimer;

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
// Listens for git.log.invalidated to merge a fresh first page in place on HEAD
// moves (preserving selection + scroll, see refreshHead) and for git.invalidated
// only to pick up a thread once the agent starts — a plain working-tree change
// does not touch history, so it must not reload.
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
    // Fetch the branch list for the active source and repopulate the branch
    // selector, preserving the user's picked branch when it still exists.
    void reloadBranches();
    // Apply the client-side text search over already-loaded subject/author,
    // hiding non-matching rows in place (no re-query). When a non-empty filter
    // hides every loaded row and more history is available, kicks the next page
    // so the user can search deeper than the loaded window (progressive paging up
    // to the model's cap, guarded against an infinite loop by m_endReached).
    void applySearchFilter();
    // Open the tabbed CommitDetailDialog for a row (double-click / menu).
    void openCommitDialog(const QModelIndex &idx);
    // Branch selector picked a new branch: re-scope the log to it.
    void onBranchChanged(int index);
    // Path filter box committed: re-scope the log to that path (empty clears).
    void onPathFilterChanged();
    // refreshHead re-fetches the first page on a real history change and merges
    // it into the loaded model in place (see LogModel::applyHead), preserving
    // the user's selection and scroll position. Used instead of the wholesale
    // reloadFromFirstPage() so a HEAD move no longer flickers the whole view.
    void refreshHead();
    void loadNextPage();
    void onSelectionChanged();
    void onNotification(const QString &method, const QJsonObject &params);
    void onScrolled(int value);
    void resolveThreadForProject();
    void updateLabel();
    void showContextMenu(const QPoint &pos);
    void copySelectedSha(bool shortForm);
    void copySelectedSubject();
    void copySelectedAsPatch();
    void updateEmptyState();

    // A log "source" is either an agent worktree (threadId set) or a
    // workspace branch (repoRoot set). `branch` scopes the walk to a non-HEAD
    // branch in either mode; `path` narrows history to a file/dir. Both are
    // user-driven filters from the toolbar and travel straight into git.log.
    struct Source {
        QString threadId;
        QString repoRoot;
        QString branch;
        QString path;
    };

    // Add branch/path filter params to a git.log/git.branches request for the
    // active source. Shared by every loader so they stay in lock-step.
    void addSourceParams(QJsonObject &params, bool includeFilters) const;

    CoreClient *m_core = nullptr;
    ElidingLabel *m_sourceLabel = nullptr;
    QComboBox *m_branchCombo = nullptr;
    QLineEdit *m_pathEdit = nullptr;
    QLineEdit *m_searchEdit = nullptr;
    QPushButton *m_refreshBtn = nullptr;
    QTableView *m_view = nullptr;
    QStackedWidget *m_stack = nullptr;
    QLabel *m_emptyLabel = nullptr;
    LogModel *m_model = nullptr;
    LogGraphDelegate *m_graphDelegate = nullptr;
    CommitDetailPanel *m_detail = nullptr;

    QString m_activeProject;
    Source m_source;
    int m_loadToken = 0;
    bool m_pageInFlight = false;
    bool m_endReached = false;
    // Debounces the per-keystroke search so a full row scan (up to the 5000 cap)
    // doesn't run on every character.
    QTimer *m_searchTimer = nullptr;
    // True while a non-empty search filter is applied. The graph delegate reads
    // this (via a widget property) to skip painting lane rails, which would
    // otherwise connect through hidden rows and look corrupt.
    bool m_searchFilterActive = false;

    // Last first page we applied via refreshHead(). Gating on it means an
    // identical first page — a working-tree-only change that left history
    // untouched — produces no merge, no repaint and no selection/scroll churn.
    Reactive<QVector<UiLogEntry>> m_firstPage;

    static constexpr int kPageSize = 200;
};
