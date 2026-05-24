// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#pragma once

#include <QDialog>
#include <QJsonObject>
#include <QString>

class CoreClient;
class QCheckBox;
class QLabel;
class QLineEdit;
class QPlainTextEdit;
class QPushButton;

// PRDialog drafts a GitHub pull request from a thread's commit history. The
// dialog asks the core for a suggested title and body via git.prDraft, lets
// the human edit them, then calls git.openPR which shells out to `gh`.
class PRDialog : public QDialog
{
    Q_OBJECT
public:
    PRDialog(CoreClient *core, const QString &threadId, const QString &branch,
             QWidget *parent = nullptr);

Q_SIGNALS:
    // Emitted with the created PR's URL so the caller can post a toast.
    void prOpened(const QString &url);

private:
    void loadDraft();
    void onCreateClicked();

    CoreClient *m_core;
    QString m_threadId;
    QString m_branch;

    QLineEdit *m_title = nullptr;
    QPlainTextEdit *m_body = nullptr;
    QCheckBox *m_draft = nullptr;
    QLabel *m_status = nullptr;
    QPushButton *m_createBtn = nullptr;
    QPushButton *m_cancelBtn = nullptr;
};
