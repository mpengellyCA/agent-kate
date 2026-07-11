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
class QVBoxLayout;

// CommitDetailDialog is the full-size, tabbed commit reader for the log viewer
// (plan 13 phase 8). Double-clicking a row (or "Open commit…" in the context
// menu) opens one; the embedded CommitDetailPanel stays for single-click
// browsing.
//
// Layout: a header (subject, copyable short sha, author-initials chip, absolute
// + relative date, ref chips) over a QTabWidget with two tabs —
//   - Changes: a per-file list with status colours and ±line counts; selecting
//     a file loads that file's scoped diff via git.commit.diff{path}.
//   - Patch: the whole commit's unified diff.
// Both render through DiffView, so inline / side-by-side comes for free.
//
// Non-modal (show() + WA_DeleteOnClose) so the user can keep browsing the log
// with it open; it remembers its size in KConfig. All async CoreClient replies
// are QPointer-guarded (this is a known SIGSEGV class in this codebase).
class CommitDetailDialog : public QDialog
{
    Q_OBJECT
public:
    // Exactly one of threadId / repoRoot identifies the log source, mirroring
    // CommitDetailPanel::setCommit. `sha` is the commit to show.
    CommitDetailDialog(CoreClient *core, const QString &threadId,
                       const QString &repoRoot, const QString &sha,
                       QWidget *parent = nullptr);
    ~CommitDetailDialog() override;

private:
    void loadDetail();
    void loadPatch();
    void loadFileDiff(const QString &path);
    void applyDetail(const QJsonObject &detail);
    void replaceChangesDiff(const QString &patch);
    void onFileRowChanged(int row);
    QJsonObject sourceParams() const;

    CoreClient *m_core = nullptr;
    QString m_threadId;
    QString m_repoRoot;
    QString m_sha;

    QLabel *m_header = nullptr;
    QLabel *m_body = nullptr;
    QListWidget *m_files = nullptr;
    DiffView *m_changesDiff = nullptr; // per-file diff (Changes tab)
    QVBoxLayout *m_changesDiffSlot = nullptr;
    DiffView *m_patch = nullptr;       // whole-commit diff (Patch tab)
    QVBoxLayout *m_patchSlot = nullptr;
};
