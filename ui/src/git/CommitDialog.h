// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#pragma once

#include <QDialog>
#include <QJsonObject>
#include <QString>
#include <QStringList>

class CoreClient;
class DiffView;
class QListWidget;
class QPlainTextEdit;
class QPushButton;
class QVBoxLayout;

// CommitDialog is the dashboard's per-thread "stage some files and commit"
// surface. It loads the worktree's dirty file list from git.snapshot, lets the
// user pick a subset and write a message, then calls git.commit. The diff
// preview pane reflects the full worktree diff via git.diff.
class CommitDialog : public QDialog
{
    Q_OBJECT
public:
    CommitDialog(CoreClient *core, const QString &threadId, const QString &branch,
                 QWidget *parent = nullptr);

Q_SIGNALS:
    // Emitted on a successful commit so the caller can surface a toast.
    void committed(const QString &threadId, const QString &branch);

private:
    void loadSnapshot();
    void loadDiff();
    void onCommitClicked();
    void onSuggestClicked();

    CoreClient *m_core;
    QString m_threadId;
    QString m_branch;

    QListWidget *m_files = nullptr;
    DiffView *m_diff = nullptr;
    QVBoxLayout *m_diffSlot = nullptr;
    QPlainTextEdit *m_message = nullptr;
    QPushButton *m_suggestBtn = nullptr;
    QPushButton *m_commitBtn = nullptr;
    QPushButton *m_cancelBtn = nullptr;
};
