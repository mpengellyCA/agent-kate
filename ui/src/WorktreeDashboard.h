// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "state/Reactive.h"

#include <QAbstractListModel>
#include <QHash>
#include <QJsonObject>
#include <QList>
#include <QString>
#include <QTimer>
#include <QWidget>

class CoreClient;
class QLabel;
class QListView;
class QPushButton;

// WorktreeRow is one card in the dashboard: the per-thread git state the core
// returns via git.snapshot, flattened for display.
struct WorktreeRow {
    QString threadId;
    int number = 0; // project-scoped agent number; 0 = not yet assigned
    QString branch;
    QString path;
    bool isolated = false;
    int ahead = 0;
    int behindBase = 0;
    int dirty = 0;
    bool conflicts = false;
    // Remote (origin) tracking, computed purely from local refs (no fetch).
    bool hasUpstream = false;
    int remoteAhead = 0;
    int remoteBehind = 0;
    // Seconds-since-epoch of the snapshot's last recompute; drives "updated Xs
    // ago" on the card's second line. 0 when the core did not report it.
    qint64 updatedAt = 0;
    QString error;
    // Human agent title (from the roster), keyed in by the dashboard from the
    // AgentDock title map. Not part of the git snapshot, so excluded from the
    // value-equality below — a title-only change repaints via a separate path.
    QString title;

    // Value equality over every git-sourced display field. Used by both the
    // model's setRows merge (skip emitting dataChanged for unchanged rows) and
    // by Reactive<QList<WorktreeRow>> (drop an identical snapshot entirely).
    // `title` is deliberately excluded: it comes from the roster, not the
    // snapshot, and is merged in after the Reactive guard.
    bool operator==(const WorktreeRow &o) const
    {
        return threadId == o.threadId && number == o.number && branch == o.branch
            && path == o.path && isolated == o.isolated && ahead == o.ahead
            && behindBase == o.behindBase && dirty == o.dirty
            && conflicts == o.conflicts && hasUpstream == o.hasUpstream
            && remoteAhead == o.remoteAhead && remoteBehind == o.remoteBehind
            && updatedAt == o.updatedAt && error == o.error;
    }
    bool operator!=(const WorktreeRow &o) const { return !(*this == o); }
};

// Item-data roles the WorktreeCardDelegate reads. The whole WorktreeRow is
// exposed via RowRole so the delegate can paint every pill in one pass without
// a role per field.
namespace WorktreeRoles {
constexpr int Row = Qt::UserRole + 1; // QVariant-wrapped WorktreeRow*
}

// WorktreeModel is the list model behind WorktreeDashboard. One row per
// worktree card. Pure data — the poll, the RPC, and the row mapping live in the
// dashboard widget itself.
class WorktreeModel : public QAbstractListModel
{
    Q_OBJECT
public:
    explicit WorktreeModel(QObject *parent = nullptr);
    int rowCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &index, int role = Qt::DisplayRole) const override;

    // Merge `rows` into the model in place, preserving the QListView selection
    // (rows are keyed and sorted by the immutable threadId).
    void setRows(QList<WorktreeRow> rows);
    const WorktreeRow *rowAt(int row) const;

private:
    QList<WorktreeRow> m_rows;
};

// WorktreeDashboard renders the git state of every active agent's worktree as a
// card list (plan 13 phase 9): branch + agent title + status pills (ahead /
// behind / remote / dirty / conflicts) with the path and "updated Xs ago" on a
// second line. It polls git.snapshot while visible and listens for
// git.invalidated to short-cut the next tick. Double-clicking a card opens a
// read-only diff modal.
class WorktreeDashboard : public QWidget
{
    Q_OBJECT
public:
    explicit WorktreeDashboard(CoreClient *core, QWidget *parent = nullptr);

    // Restrict the dashboard to worktrees whose RepoRoot matches projectPath.
    // An empty path shows everything (legacy behaviour).
    void setActiveProject(const QString &projectPath);

    // Feed the human agent titles (threadId → title) so cards can name the
    // agent, not just its branch. Called by MainWindow from the roster; a
    // change re-merges titles into the current rows without a fresh snapshot.
    void setAgentTitles(const QHash<QString, QString> &titlesByThread);

Q_SIGNALS:
    void statusMessage(const QString &text);
    // Requests a terminal rooted in the given worktree path.
    void openTerminalRequested(const QString &worktreePath);

protected:
    void showEvent(QShowEvent *e) override;
    void hideEvent(QHideEvent *e) override;
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void refresh();
    // Push `rows` into the model while preserving the selected row (by
    // threadId), then sync placeholder + action enablement.
    void applySnapshot(const QList<WorktreeRow> &rows);
    void onNotification(const QString &method, const QJsonObject &params);
    void openDiffDialog();
    void openCommitDialog();
    void landSelected();
    void openPRDialog();
    void discardSelected();
    void removeSelected();
    void analyzeAndCleanup();
    void showRowContextMenu(const QPoint &pos);
    void updatePlaceholder();
    void updateActionEnablement();
    const WorktreeRow *selectedRow() const;

    CoreClient *m_core = nullptr;
    QListView *m_view = nullptr;
    WorktreeModel *m_model = nullptr;
    QTimer *m_pollTimer = nullptr;
    // Repaints cards ~ every 30s so "updated Xs ago" stays fresh while visible.
    QTimer *m_relTimeTimer = nullptr;
    QPushButton *m_commitBtn = nullptr;
    QPushButton *m_landBtn = nullptr;
    QPushButton *m_prBtn = nullptr;
    QPushButton *m_discardBtn = nullptr;
    QPushButton *m_cleanupBtn = nullptr;
    QLabel *m_placeholder = nullptr;
    QString m_activeProject;
    QHash<QString, QString> m_titlesByThread;
    bool m_inFlight = false;
    // Set when refresh() is requested while a git.snapshot is already in flight,
    // so the dropped invalidation is re-issued once the reply lands instead of
    // waiting up to a full safety-net poll interval to catch up.
    bool m_refreshPending = false;

    // Canonical snapshot. set() in the git.snapshot reply; an identical
    // payload is dropped here (no subscriber, no setRows, no repaint).
    // subscribe() drives applySnapshot() only on a genuine change.
    Reactive<QList<WorktreeRow>> m_snapshot;
};
