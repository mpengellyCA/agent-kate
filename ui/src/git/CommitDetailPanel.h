// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#pragma once

#include <QJsonObject>
#include <QString>
#include <QVBoxLayout>
#include <QWidget>

class CoreClient;
class DiffView;
class QLabel;
class QListWidget;
class QListWidgetItem;
class QPlainTextEdit;

// CommitDetailPanel is the right-hand pane of the log viewer. It pulls
// `git.commit.detail` for header + file list, then `git.commit.diff` for the
// full patch (optionally scoped to one file when the user picks one in the
// list).
//
// One panel per LogViewer window — feed it a new (threadId, sha) by calling
// setCommit().
class CommitDetailPanel : public QWidget
{
    Q_OBJECT
public:
    CommitDetailPanel(CoreClient *core, QWidget *parent = nullptr);

    void setCommit(const QString &threadId, const QString &sha);
    void clear();

private:
    void loadDetail();
    void loadDiff(const QString &path);
    void applyDetail(const QJsonObject &detail);
    void replaceDiff(const QString &patch);
    void onFileRowChanged(int row);

    CoreClient *m_core;
    QString m_threadId;
    QString m_sha;
    // Bumped on every setCommit() so in-flight replies for a stale commit can
    // be discarded — the user can click through commits faster than RPCs
    // round-trip.
    int m_token = 0;

    QLabel *m_header = nullptr;
    QPlainTextEdit *m_body = nullptr;
    QListWidget *m_files = nullptr;
    DiffView *m_diff = nullptr;
    QVBoxLayout *m_diffSlot = nullptr;
};
