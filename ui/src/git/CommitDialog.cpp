// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#include "CommitDialog.h"
#include "DiffView.h"
#include "ipc/CoreClient.h"

#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QListWidget>
#include <QListWidgetItem>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QSplitter>
#include <QVBoxLayout>

namespace {
// File status → short prefix in the checkbox list, so the user can tell at a
// glance what each entry is (M / A / D / ? / !).
QString statusGlyph(const QString &status)
{
    if (status == QLatin1String("modified"))   return QStringLiteral("M ");
    if (status == QLatin1String("added"))      return QStringLiteral("A ");
    if (status == QLatin1String("deleted"))    return QStringLiteral("D ");
    if (status == QLatin1String("untracked"))  return QStringLiteral("? ");
    if (status == QLatin1String("conflicted")) return QStringLiteral("! ");
    if (status == QLatin1String("renamed"))    return QStringLiteral("R ");
    return QStringLiteral("  ");
}
} // namespace

CommitDialog::CommitDialog(CoreClient *core, const QString &threadId, const QString &branch,
                           QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_threadId(threadId)
    , m_branch(branch)
{
    setWindowTitle(branch.isEmpty()
                       ? QStringLiteral("Commit changes")
                       : QStringLiteral("Commit to %1").arg(branch));
    resize(900, 640);

    m_files = new QListWidget(this);
    m_files->setSelectionMode(QAbstractItemView::SingleSelection);
    m_files->setAlternatingRowColors(true);

    // The diff slot holds whichever DiffView is current; we swap it out on
    // refresh because DiffView is constructed with its text and has no
    // public setter.
    auto *diffHost = new QWidget(this);
    m_diffSlot = new QVBoxLayout(diffHost);
    m_diffSlot->setContentsMargins(0, 0, 0, 0);

    auto *splitter = new QSplitter(Qt::Horizontal, this);
    splitter->addWidget(m_files);
    splitter->addWidget(diffHost);
    splitter->setStretchFactor(0, 1);
    splitter->setStretchFactor(1, 3);

    m_message = new QPlainTextEdit(this);
    m_message->setPlaceholderText(QStringLiteral("Commit message…"));
    m_message->setFixedHeight(110);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Cancel, this);
    m_suggestBtn = buttons->addButton(QStringLiteral("Suggest with Sonnet"),
                                      QDialogButtonBox::ActionRole);
    m_suggestBtn->setToolTip(QStringLiteral(
        "Ask Claude Sonnet to draft a commit message for the current diff."));
    m_commitBtn = buttons->addButton(QStringLiteral("Commit"),
                                     QDialogButtonBox::AcceptRole);
    m_commitBtn->setEnabled(false);
    m_cancelBtn = buttons->button(QDialogButtonBox::Cancel);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(m_commitBtn, &QPushButton::clicked, this, &CommitDialog::onCommitClicked);
    connect(m_suggestBtn, &QPushButton::clicked, this, &CommitDialog::onSuggestClicked);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(12, 12, 12, 12);
    layout->setSpacing(10);
    auto *header = new QLabel(
        branch.isEmpty()
            ? QStringLiteral("Pick the files to include, then write a message.")
            : QStringLiteral("Pick the files to include on <b>%1</b>, then "
                             "write a message.").arg(branch.toHtmlEscaped()),
        this);
    header->setTextFormat(Qt::RichText);
    layout->addWidget(header);
    layout->addWidget(splitter, 1);
    layout->addWidget(m_message);
    layout->addWidget(buttons);

    // Enable Commit only when at least one file is checked. The list is the
    // source of truth — the message can be empty (the core fills in a
    // default), but committing nothing is meaningless.
    auto refreshCommitEnabled = [this] {
        bool any = false;
        for (int i = 0; i < m_files->count(); ++i) {
            if (m_files->item(i)->checkState() == Qt::Checked) {
                any = true;
                break;
            }
        }
        m_commitBtn->setEnabled(any);
    };
    connect(m_files, &QListWidget::itemChanged, this,
            [refreshCommitEnabled](QListWidgetItem *) { refreshCommitEnabled(); });

    loadSnapshot();
    loadDiff();
}

void CommitDialog::loadSnapshot()
{
    if (!m_core->isConnected()) {
        return;
    }
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.snapshot"), {},
                 [this, thread](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     const QJsonArray threads =
                         result.value(QStringLiteral("threads")).toArray();
                     for (const QJsonValue &v : threads) {
                         const QJsonObject t = v.toObject();
                         if (t.value(QStringLiteral("threadId")).toString() != thread) {
                             continue;
                         }
                         const QJsonArray files =
                             t.value(QStringLiteral("files")).toArray();
                         m_files->clear();
                         for (const QJsonValue &fv : files) {
                             const QJsonObject f = fv.toObject();
                             const QString status =
                                 f.value(QStringLiteral("status")).toString();
                             const QString path =
                                 f.value(QStringLiteral("path")).toString();
                             auto *item = new QListWidgetItem(
                                 statusGlyph(status) + path, m_files);
                             item->setData(Qt::UserRole, path);
                             item->setFlags(item->flags() | Qt::ItemIsUserCheckable);
                             item->setCheckState(Qt::Checked);
                         }
                         if (m_files->count() > 0) {
                             m_commitBtn->setEnabled(true);
                         }
                         break;
                     }
                 });
}

void CommitDialog::loadDiff()
{
    if (!m_core->isConnected()) {
        return;
    }
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.diff"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this, thread](const QJsonObject &result, const QJsonObject &error) {
                     if (thread != m_threadId) {
                         return; // dialog reused for a different thread (defensive)
                     }
                     QString patch;
                     if (error.isEmpty()) {
                         patch = result.value(QStringLiteral("patch")).toString();
                     }
                     if (patch.isEmpty()) {
                         patch = QStringLiteral("(no diff)");
                     }
                     // Swap in a freshly constructed DiffView. The previous
                     // one (if any) is deleteLatered.
                     if (m_diff) {
                         m_diff->deleteLater();
                     }
                     m_diff = new DiffView(patch, this);
                     m_diffSlot->addWidget(m_diff);
                 });
}

void CommitDialog::onSuggestClicked()
{
    if (!m_core->isConnected()) {
        return;
    }
    const QString prevText = m_suggestBtn->text();
    m_suggestBtn->setEnabled(false);
    m_suggestBtn->setText(QStringLiteral("Drafting…"));
    const QString prevPlaceholder = m_message->placeholderText();
    m_message->setPlaceholderText(QStringLiteral("Sonnet is drafting a message…"));
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.suggestCommitMessage"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this, thread, prevText, prevPlaceholder]
                 (const QJsonObject &result, const QJsonObject &error) {
                     if (thread != m_threadId) {
                         return;
                     }
                     m_suggestBtn->setEnabled(true);
                     m_suggestBtn->setText(prevText);
                     if (!error.isEmpty()) {
                         m_message->setPlaceholderText(
                             QStringLiteral("Suggestion failed: %1")
                                 .arg(error.value(QStringLiteral("message"))
                                          .toString()));
                         return;
                     }
                     m_message->setPlaceholderText(prevPlaceholder);
                     const QString msg =
                         result.value(QStringLiteral("message")).toString();
                     if (msg.isEmpty()) {
                         m_message->setPlaceholderText(
                             QStringLiteral("Sonnet returned no message — "
                                            "is the diff empty?"));
                         return;
                     }
                     // Replace whatever is in the editor with the suggestion;
                     // the user can still edit it before committing.
                     m_message->setPlainText(msg);
                 });
}

void CommitDialog::onCommitClicked()
{
    QStringList paths;
    for (int i = 0; i < m_files->count(); ++i) {
        QListWidgetItem *item = m_files->item(i);
        if (item->checkState() == Qt::Checked) {
            paths << item->data(Qt::UserRole).toString();
        }
    }
    if (paths.isEmpty()) {
        return;
    }
    QJsonArray jpaths;
    for (const QString &p : paths) {
        jpaths.append(p);
    }
    m_commitBtn->setEnabled(false);
    const QString thread = m_threadId;
    const QString branch = m_branch;
    m_core->call(QStringLiteral("git.commit"),
                 QJsonObject{{QStringLiteral("threadId"), thread},
                             {QStringLiteral("message"), m_message->toPlainText()},
                             {QStringLiteral("paths"), jpaths}},
                 [this, thread, branch](const QJsonObject &result,
                                        const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_commitBtn->setEnabled(true);
                         // Show the failure inline rather than crash-closing.
                         m_message->setPlaceholderText(
                             QStringLiteral("Commit failed: %1")
                                 .arg(error.value(QStringLiteral("message"))
                                          .toString()));
                         return;
                     }
                     emit committed(thread,
                                    result.value(QStringLiteral("branch")).toString());
                     accept();
                 });
}
