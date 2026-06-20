// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "ConflictDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QListWidget>
#include <QMessageBox>
#include <QPushButton>
#include <QTimer>
#include <QVBoxLayout>

namespace {
// Cadence for polling git.workspaceMergeStatus while the dialog is open. The
// fs watcher pushes git.invalidated on every save, so this is a safety net
// for the case where a save somehow misses the watcher.
constexpr int kPollIntervalMs = 1500;
} // namespace

ConflictDialog::ConflictDialog(CoreClient *core, const QString &threadId,
                               const QString &branch, const QString &into,
                               const QStringList &initialConflicts, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_threadId(threadId)
    , m_branch(branch)
    , m_into(into)
    , m_pollTimer(new QTimer(this))
{
    setWindowTitle(i18nc("@title:window", "Resolve merge conflicts"));
    resize(560, 460);

    m_summary = new QLabel(this);
    m_summary->setTextFormat(Qt::RichText);
    m_summary->setWordWrap(true);

    m_files = new QListWidget(this);
    m_files->setAlternatingRowColors(true);
    for (const QString &path : initialConflicts) {
        m_files->addItem(path);
    }

    m_openBtn = new QPushButton(i18nc("@action:button", "Resolve in KDiff3"), this);
    m_openBtn->setCursor(Qt::PointingHandCursor);
    m_finalizeBtn = new QPushButton(i18nc("@action:button", "Finalize merge"), this);
    m_finalizeBtn->setCursor(Qt::PointingHandCursor);
    m_finalizeBtn->setEnabled(false);
    m_abortBtn = new QPushButton(i18nc("@action:button", "Abort merge"), this);
    m_abortBtn->setCursor(Qt::PointingHandCursor);
    m_closeBtn = new QPushButton(i18nc("@action:button", "Close"), this);
    m_closeBtn->setCursor(Qt::PointingHandCursor);

    auto *buttons = new QHBoxLayout;
    buttons->addWidget(m_openBtn);
    buttons->addStretch(1);
    buttons->addWidget(m_abortBtn);
    buttons->addWidget(m_finalizeBtn);
    buttons->addWidget(m_closeBtn);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(14, 14, 14, 14);
    layout->setSpacing(10);
    layout->addWidget(m_summary);
    layout->addWidget(m_files, 1);
    layout->addLayout(buttons);

    connect(m_openBtn, &QPushButton::clicked, this, &ConflictDialog::onOpenTool);
    connect(m_finalizeBtn, &QPushButton::clicked, this, &ConflictDialog::onFinalize);
    connect(m_abortBtn, &QPushButton::clicked, this, &ConflictDialog::onAbort);
    connect(m_closeBtn, &QPushButton::clicked, this, &QDialog::reject);
    connect(m_core, &CoreClient::notification, this,
            [this](const QString &m, const QJsonObject &) {
                if (m == QLatin1String("git.invalidated")) {
                    refreshStatus();
                }
            });

    m_pollTimer->setInterval(kPollIntervalMs);
    connect(m_pollTimer, &QTimer::timeout, this, &ConflictDialog::refreshStatus);
    m_pollTimer->start();

    refreshStatus();
}

void ConflictDialog::refreshStatus()
{
    if (m_inFlight || !m_core->isConnected()) {
        return;
    }
    m_inFlight = true;
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.workspaceMergeStatus"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this, thread](const QJsonObject &result, const QJsonObject &error) {
                     m_inFlight = false;
                     if (thread != m_threadId || !error.isEmpty()) {
                         return;
                     }
                     const bool merging = result.value(QStringLiteral("merging")).toBool();
                     const QJsonArray conflictsArr =
                         result.value(QStringLiteral("conflicts")).toArray();
                     QStringList conflicts;
                     conflicts.reserve(conflictsArr.size());
                     for (const QJsonValue &v : conflictsArr) {
                         conflicts << v.toString();
                     }
                     m_files->clear();
                     for (const QString &p : conflicts) {
                         m_files->addItem(p);
                     }
                     if (!merging) {
                         m_summary->setText(
                             i18n("<b>The merge is no longer in progress.</b> "
                                  "Nothing left to do."));
                         m_openBtn->setEnabled(false);
                         m_finalizeBtn->setEnabled(false);
                         m_abortBtn->setEnabled(false);
                         return;
                     }
                     const QString into = m_into.isEmpty()
                         ? i18nc("merge destination when no branch name is known",
                                 "the workspace")
                         : m_into;
                     if (conflicts.isEmpty()) {
                         m_summary->setText(
                             i18n("All conflicts resolved on <b>%1</b>. "
                                  "Click <i>Finalize merge</i> to commit.",
                                  into.toHtmlEscaped()));
                         m_finalizeBtn->setEnabled(true);
                         m_openBtn->setEnabled(false);
                     } else {
                         m_summary->setText(
                             i18np("<b>%1 unresolved conflict</b> when merging "
                                   "<b>%2</b> into <b>%3</b>.",
                                   "<b>%1 unresolved conflicts</b> when merging "
                                   "<b>%2</b> into <b>%3</b>.",
                                   conflicts.size(),
                                   m_branch.toHtmlEscaped(),
                                   into.toHtmlEscaped()));
                         m_finalizeBtn->setEnabled(false);
                         m_openBtn->setEnabled(true);
                     }
                 });
}

void ConflictDialog::onOpenTool()
{
    m_openBtn->setEnabled(false);
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.openConflictTool"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     m_openBtn->setEnabled(true);
                     if (!error.isEmpty()) {
                         QMessageBox::warning(
                             this, i18nc("@title:window", "KDiff3 unavailable"),
                             error.value(QStringLiteral("message")).toString());
                     }
                 });
}

void ConflictDialog::onFinalize()
{
    m_finalizeBtn->setEnabled(false);
    const QString thread = m_threadId;
    const QString into = m_into;
    m_core->call(QStringLiteral("git.finalizeMerge"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this, thread, into](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_finalizeBtn->setEnabled(true);
                         QMessageBox::warning(
                             this, i18nc("@title:window", "Could not finalize merge"),
                             error.value(QStringLiteral("message")).toString());
                         return;
                     }
                     emit finalized(thread, into);
                     accept();
                 });
}

void ConflictDialog::onAbort()
{
    if (QMessageBox::question(
            this, i18nc("@title:window", "Abort merge?"),
            i18n("Roll back the in-progress merge and restore the "
                 "workspace to its pre-merge state?"),
            QMessageBox::Yes | QMessageBox::No, QMessageBox::No)
        != QMessageBox::Yes) {
        return;
    }
    m_abortBtn->setEnabled(false);
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.abortMerge"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this, thread](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_abortBtn->setEnabled(true);
                         QMessageBox::warning(
                             this, i18nc("@title:window", "Could not abort merge"),
                             error.value(QStringLiteral("message")).toString());
                         return;
                     }
                     emit aborted(thread);
                     accept();
                 });
}
