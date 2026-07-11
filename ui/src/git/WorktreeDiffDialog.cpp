// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorktreeDiffDialog.h"
#include "DiffView.h"
#include "ipc/CoreClient.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QDialogButtonBox>
#include <QFont>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QListWidget>
#include <QListWidgetItem>
#include <QPointer>
#include <QPushButton>
#include <QSplitter>
#include <QVBoxLayout>

namespace {
QString statusGlyph(const QString &status)
{
    if (status == QLatin1String("modified"))   return QStringLiteral("M");
    if (status == QLatin1String("added"))      return QStringLiteral("A");
    if (status == QLatin1String("deleted"))    return QStringLiteral("D");
    if (status == QLatin1String("untracked"))  return QStringLiteral("?");
    if (status == QLatin1String("conflicted")) return QStringLiteral("!");
    if (status == QLatin1String("renamed"))    return QStringLiteral("R");
    return QStringLiteral(" ");
}

// Semantic colour for a file's status glyph, matching CommitDetailDialog.
QColor statusColor(const QString &status)
{
    const AkColors &c = ThemeManager::palette();
    if (status == QLatin1String("added") || status == QLatin1String("untracked"))
        return c.positive;
    if (status == QLatin1String("deleted"))    return c.negative;
    if (status == QLatin1String("conflicted")) return c.negative;
    if (status == QLatin1String("renamed"))    return c.info;
    return c.neutral; // modified / other
}
} // namespace

WorktreeDiffDialog::WorktreeDiffDialog(CoreClient *core, const QString &threadId,
                                       const QString &branch, int number,
                                       bool canMerge, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_threadId(threadId)
    , m_branch(branch)
{
    setWindowTitle(branch.isEmpty()
                       ? i18nc("@title:window", "Worktree changes")
                       : i18nc("@title:window", "Changes in %1", branch));
    setAttribute(Qt::WA_DeleteOnClose);
    setModal(false);

    m_header = new QLabel(this);
    m_header->setTextFormat(Qt::RichText);
    m_header->setWordWrap(true);
    {
        const QString label =
            number > 0 ? i18nc("agent number then branch", "#%1 %2")
                             .arg(number)
                             .arg(branch.isEmpty()
                                      ? i18nc("git branch state", "(detached)")
                                      : branch)
                       : (branch.isEmpty() ? i18nc("git branch state", "(detached)")
                                           : branch);
        m_header->setText(i18n("Uncommitted changes in worktree <b>%1</b>.",
                               label.toHtmlEscaped()));
    }

    m_files = new QListWidget(this);
    m_files->setAlternatingRowColors(true);
    m_files->setSelectionMode(QAbstractItemView::SingleSelection);
    connect(m_files, &QListWidget::currentRowChanged, this,
            &WorktreeDiffDialog::onFileRowChanged);

    auto *diffHost = new QWidget(this);
    m_diffSlot = new QVBoxLayout(diffHost);
    m_diffSlot->setContentsMargins(0, 0, 0, 0);

    auto *splitter = new QSplitter(Qt::Horizontal, this);
    splitter->addWidget(m_files);
    splitter->addWidget(diffHost);
    splitter->setStretchFactor(0, 1);
    splitter->setStretchFactor(1, 3);

    // Footer: hand-off actions on the left, Close on the right. The dashboard
    // owns the Commit / Land / PR dialogs, so we only emit requests.
    auto *buttons = new QDialogButtonBox(this);
    auto *commitBtn =
        buttons->addButton(i18nc("@action:button", "Commit…"),
                           QDialogButtonBox::ActionRole);
    auto *landBtn = buttons->addButton(i18nc("@action:button", "Land into main…"),
                                       QDialogButtonBox::ActionRole);
    auto *prBtn =
        buttons->addButton(i18nc("@action:button", "Open PR…"),
                           QDialogButtonBox::ActionRole);
    landBtn->setEnabled(canMerge);
    prBtn->setEnabled(canMerge);
    auto *closeBtn = buttons->addButton(QDialogButtonBox::Close);
    connect(closeBtn, &QPushButton::clicked, this, &QDialog::accept);
    connect(commitBtn, &QPushButton::clicked, this, [this] {
        emit commitRequested(m_threadId);
        accept();
    });
    connect(landBtn, &QPushButton::clicked, this, [this] {
        emit landRequested(m_threadId);
        accept();
    });
    connect(prBtn, &QPushButton::clicked, this, [this] {
        emit prRequested(m_threadId);
        accept();
    });

    auto *root = new QVBoxLayout(this);
    root->setContentsMargins(12, 12, 12, 12);
    root->setSpacing(10);
    root->addWidget(m_header);
    root->addWidget(splitter, 1);
    root->addWidget(buttons);

    // Seed the diff pane so it is never empty before the RPC returns.
    replaceDiff(QString());
    m_diff->setEmptyMessage(i18n("Loading changes…"));

    const KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("WorktreeDiffDialog"));
    resize(cfg.readEntry("size", QSize(900, 640)));

    // loadFiles() selects row 0 ("All files") on return, which triggers
    // onFileRowChanged → loadDiff(QString()) — the whole-worktree patch. We do
    // not also call loadDiff here or the diff would be fetched twice per open.
    loadFiles();
}

WorktreeDiffDialog::~WorktreeDiffDialog()
{
    KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("WorktreeDiffDialog"));
    cfg.writeEntry("size", size());
}

void WorktreeDiffDialog::loadFiles()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    QPointer<WorktreeDiffDialog> guard(this);
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.snapshot"), {},
                 [this, guard, thread](const QJsonObject &result,
                                       const QJsonObject &error) {
                     if (!guard || !error.isEmpty()) {
                         return;
                     }
                     const QJsonArray threads =
                         result.value(QStringLiteral("threads")).toArray();
                     bool matched = false;
                     for (const QJsonValue &v : threads) {
                         const QJsonObject t = v.toObject();
                         if (t.value(QStringLiteral("threadId")).toString() != thread) {
                             continue;
                         }
                         matched = true;
                         m_files->clear();
                         auto *all = new QListWidgetItem(
                             i18nc("synthetic entry showing the whole worktree diff",
                                   "All files"),
                             m_files);
                         QFont f = all->font();
                         f.setItalic(true);
                         all->setFont(f);
                         all->setData(Qt::UserRole, QString()); // empty = no filter
                         const QJsonArray files =
                             t.value(QStringLiteral("files")).toArray();
                         for (const QJsonValue &fv : files) {
                             const QJsonObject fo = fv.toObject();
                             const QString status =
                                 fo.value(QStringLiteral("status")).toString();
                             const QString path =
                                 fo.value(QStringLiteral("path")).toString();
                             auto *item = new QListWidgetItem(
                                 QStringLiteral("%1  %2").arg(statusGlyph(status), path),
                                 m_files);
                             item->setData(Qt::UserRole, path);
                             item->setForeground(statusColor(status));
                             item->setToolTip(path);
                         }
                         m_files->setCurrentRow(0);
                         break;
                     }
                     // If the snapshot has no entry for this thread the file list
                     // stays empty and setCurrentRow(0) above never fires, so
                     // fetch the whole-worktree patch directly (it will show the
                     // "no uncommitted changes" empty state).
                     if (!matched) {
                         loadDiff(QString());
                     }
                 },
                 this);
}

void WorktreeDiffDialog::loadDiff(const QString &path)
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    QPointer<WorktreeDiffDialog> guard(this);
    const quint64 req = ++m_diffReq;
    QJsonObject params{{QStringLiteral("threadId"), m_threadId}};
    if (!path.isEmpty()) {
        params.insert(QStringLiteral("path"), path);
    }
    m_core->call(QStringLiteral("git.diff"), params,
                 [this, guard, req](const QJsonObject &result, const QJsonObject &error) {
                     // Discard a reply that a newer selection has superseded, so
                     // out-of-order replies can't leave a stale diff on screen.
                     if (!guard || req != m_diffReq) {
                         return;
                     }
                     QString patch;
                     if (error.isEmpty()) {
                         patch = result.value(QStringLiteral("patch")).toString();
                     }
                     replaceDiff(patch);
                 },
                 this);
}

void WorktreeDiffDialog::replaceDiff(const QString &patch)
{
    if (m_diff) {
        m_diff->deleteLater();
        m_diff = nullptr;
    }
    m_diff = new DiffView(patch, this);
    if (patch.isEmpty()) {
        m_diff->setEmptyMessage(i18n("No uncommitted changes."));
    }
    m_diffSlot->addWidget(m_diff);
}

void WorktreeDiffDialog::onFileRowChanged(int row)
{
    if (row < 0) {
        return;
    }
    QListWidgetItem *item = m_files->item(row);
    if (!item) {
        return;
    }
    loadDiff(item->data(Qt::UserRole).toString());
}
