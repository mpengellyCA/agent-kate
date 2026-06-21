// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QAbstractTableModel>
#include <QJsonObject>
#include <QList>
#include <QString>
#include <QTimer>
#include <QWidget>

class CoreClient;
class QLabel;
class QPushButton;
class QTableView;

// WorktreeRow is one row in the dashboard: the per-thread git state the core
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
    QString error;
};

// WorktreeModel is the table model behind WorktreeDashboard. Pure data — the
// poll, the RPC, and the row mapping live in the dashboard widget itself.
class WorktreeModel : public QAbstractTableModel
{
    Q_OBJECT
public:
    enum Column {
        ColAgent = 0,
        ColBranch,
        ColIsolation,
        ColAhead,
        ColBehind,
        ColRemote,
        ColDirty,
        ColPath,
        ColCount,
    };

    explicit WorktreeModel(QObject *parent = nullptr);
    int rowCount(const QModelIndex &parent = {}) const override;
    int columnCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &index, int role = Qt::DisplayRole) const override;
    QVariant headerData(int section, Qt::Orientation orientation,
                        int role = Qt::DisplayRole) const override;

    void setRows(QList<WorktreeRow> rows);
    const WorktreeRow *rowAt(int row) const;

private:
    QList<WorktreeRow> m_rows;
};

// WorktreeDashboard renders the git state of every active agent's worktree:
// branch, ahead/behind vs the fork point, dirty file count, conflict flag.
// It polls git.snapshot at 1 Hz while visible and listens for git.invalidated
// to short-cut the next tick.
class WorktreeDashboard : public QWidget
{
    Q_OBJECT
public:
    explicit WorktreeDashboard(CoreClient *core, QWidget *parent = nullptr);

    // Restrict the dashboard to worktrees whose RepoRoot matches projectPath.
    // An empty path shows everything (legacy behaviour).
    void setActiveProject(const QString &projectPath);

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
    void onNotification(const QString &method, const QJsonObject &params);
    void openCommitDialog();
    void landSelected();
    void openPRDialog();
    void discardSelected();
    void removeSelected();
    void showRowContextMenu(const QPoint &pos);
    void updatePlaceholder();
    const WorktreeRow *selectedRow() const;

    CoreClient *m_core = nullptr;
    QTableView *m_view = nullptr;
    WorktreeModel *m_model = nullptr;
    QTimer *m_pollTimer = nullptr;
    QPushButton *m_commitBtn = nullptr;
    QPushButton *m_landBtn = nullptr;
    QPushButton *m_prBtn = nullptr;
    QPushButton *m_discardBtn = nullptr;
    QLabel *m_placeholder = nullptr;
    QString m_activeProject;
    bool m_inFlight = false;
};
