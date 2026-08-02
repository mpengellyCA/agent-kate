// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QJsonObject>
#include <QString>

class CoreClient;
class DiffView;
class QLabel;
class QListWidget;
class QPushButton;
class QVBoxLayout;

// WorktreeDiffDialog is the read-only diff reader for a worktree's *current*
// (uncommitted) state (plan 13 phase 9). Double-clicking a card in the
// WorktreeDashboard opens one. It shows a file list (from git.snapshot) beside
// a DiffView (from git.diff) — selecting a file scopes the diff to that path,
// "All files" shows the whole worktree patch. The footer hands off to the
// existing Commit… / Land / Open PR dialogs so a review flows straight into an
// action.
//
// Non-modal (show() + WA_DeleteOnClose); remembers its size in KConfig. All
// async CoreClient replies are QPointer-guarded (a known SIGSEGV class here).
class WorktreeDiffDialog : public QDialog
{
    Q_OBJECT
public:
    // `isolated` and `ahead` come straight off the dashboard row: together they
    // decide both what the header may claim (a workspace-mode thread is the
    // user's own checkout, not a worktree — audit F41) and whether landing this
    // thread is meaningful at all (WorktreeReviewCopy::canLand — audit F29's
    // asymmetry). `path` is the directory the agent actually runs in, named in
    // the workspace-mode header so the blast radius is visible.
    WorktreeDiffDialog(CoreClient *core, const QString &threadId,
                       const QString &branch, const QString &path, int number,
                       bool isolated, int ahead, QWidget *parent = nullptr);
    ~WorktreeDiffDialog() override;

Q_SIGNALS:
    // The user asked to commit / land / open a PR for this worktree from the
    // footer. The dashboard owns those dialogs, so it handles these.
    void commitRequested(const QString &threadId);
    void landRequested(const QString &threadId);
    void prRequested(const QString &threadId);

private:
    void loadFiles();
    void loadDiff(const QString &path);
    void replaceDiff(const QString &patch);
    void onFileRowChanged(int row);

    CoreClient *m_core = nullptr;
    QString m_threadId;
    QString m_branch;
    // Whether this thread has a copy of its own. Decides the empty-state
    // wording, so it has to outlive the constructor.
    bool m_isolated = false;

    QLabel *m_header = nullptr;
    QListWidget *m_files = nullptr;
    DiffView *m_diff = nullptr;
    QVBoxLayout *m_diffSlot = nullptr;

    // Monotonic token so two quick file selections can't paint out of order:
    // each loadDiff() bumps it and its reply discards itself if superseded.
    quint64 m_diffReq = 0;
};
