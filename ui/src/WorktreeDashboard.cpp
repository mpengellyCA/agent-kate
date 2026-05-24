// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#include "WorktreeDashboard.h"
#include "git/CommitDialog.h"
#include "git/ConflictDialog.h"
#include "git/PRDialog.h"
#include "ipc/CoreClient.h"

#include <QHBoxLayout>
#include <QHeaderView>
#include <QItemSelectionModel>
#include <QJsonArray>
#include <QJsonValue>
#include <QMessageBox>
#include <QPalette>
#include <QPushButton>
#include <QTableView>
#include <QVBoxLayout>

namespace {
// Poll cadence while visible. The cache on the core side coalesces concurrent
// reads, so 1 Hz is cheap even with many threads.
constexpr int kPollIntervalMs = 1000;
} // namespace

WorktreeModel::WorktreeModel(QObject *parent)
    : QAbstractTableModel(parent)
{
}

int WorktreeModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_rows.size();
}

int WorktreeModel::columnCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : ColCount;
}

QVariant WorktreeModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || index.row() < 0 || index.row() >= m_rows.size()) {
        return {};
    }
    const WorktreeRow &r = m_rows.at(index.row());
    if (role == Qt::DisplayRole) {
        switch (index.column()) {
        case ColBranch:
            return r.branch.isEmpty() ? QStringLiteral("(detached)") : r.branch;
        case ColIsolation:
            return r.isolated ? QStringLiteral("worktree") : QStringLiteral("workspace");
        case ColAhead:
            return r.ahead;
        case ColBehind:
            return r.behindBase;
        case ColDirty:
            if (r.conflicts) {
                return QStringLiteral("%1 · conflicts").arg(r.dirty);
            }
            return r.dirty;
        case ColPath:
            return r.path;
        }
    }
    if (role == Qt::ToolTipRole) {
        if (!r.error.isEmpty()) {
            return r.error;
        }
        return QStringLiteral("thread %1\n%2").arg(r.threadId, r.path);
    }
    if (role == Qt::ForegroundRole && r.conflicts && index.column() == ColDirty) {
        QPalette pal;
        return pal.color(QPalette::BrightText);
    }
    if (role == Qt::TextAlignmentRole) {
        switch (index.column()) {
        case ColAhead:
        case ColBehind:
        case ColDirty:
            return int(Qt::AlignRight | Qt::AlignVCenter);
        }
    }
    return {};
}

QVariant WorktreeModel::headerData(int section, Qt::Orientation orientation, int role) const
{
    if (role != Qt::DisplayRole || orientation != Qt::Horizontal) {
        return {};
    }
    switch (section) {
    case ColBranch:
        return QStringLiteral("Branch");
    case ColIsolation:
        return QStringLiteral("Mode");
    case ColAhead:
        return QStringLiteral("↑");
    case ColBehind:
        return QStringLiteral("↓");
    case ColDirty:
        return QStringLiteral("Dirty");
    case ColPath:
        return QStringLiteral("Path");
    }
    return {};
}

void WorktreeModel::setRows(QList<WorktreeRow> rows)
{
    beginResetModel();
    m_rows = std::move(rows);
    endResetModel();
}

const WorktreeRow *WorktreeModel::rowAt(int row) const
{
    if (row < 0 || row >= m_rows.size()) {
        return nullptr;
    }
    return &m_rows.at(row);
}

WorktreeDashboard::WorktreeDashboard(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
    , m_view(new QTableView(this))
    , m_model(new WorktreeModel(this))
    , m_pollTimer(new QTimer(this))
{
    m_view->setModel(m_model);
    m_view->setSelectionBehavior(QAbstractItemView::SelectRows);
    m_view->setSelectionMode(QAbstractItemView::SingleSelection);
    m_view->setShowGrid(false);
    m_view->setAlternatingRowColors(true);
    m_view->verticalHeader()->setVisible(false);
    m_view->horizontalHeader()->setStretchLastSection(true);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColBranch, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColIsolation, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColAhead, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColBehind, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColDirty, QHeaderView::ResizeToContents);

    m_commitBtn = new QPushButton(QStringLiteral("Commit selected…"), this);
    m_commitBtn->setEnabled(false);
    m_landBtn = new QPushButton(QStringLiteral("Land into main…"), this);
    m_landBtn->setEnabled(false);
    m_prBtn = new QPushButton(QStringLiteral("Open PR…"), this);
    m_prBtn->setEnabled(false);
    auto *toolbar = new QHBoxLayout;
    toolbar->setContentsMargins(6, 4, 6, 4);
    toolbar->addStretch(1);
    toolbar->addWidget(m_commitBtn);
    toolbar->addWidget(m_landBtn);
    toolbar->addWidget(m_prBtn);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(m_view, 1);
    layout->addLayout(toolbar);

    m_pollTimer->setInterval(kPollIntervalMs);
    connect(m_pollTimer, &QTimer::timeout, this, &WorktreeDashboard::refresh);
    connect(m_core, &CoreClient::notification, this, &WorktreeDashboard::onNotification);
    connect(m_core, &CoreClient::connected, this, &WorktreeDashboard::refresh);
    connect(m_view->selectionModel(), &QItemSelectionModel::selectionChanged, this,
            [this] {
                const WorktreeRow *r = selectedRow();
                m_commitBtn->setEnabled(r != nullptr);
                // Land and Open PR only make sense for an isolated worktree
                // on its own branch with commits actually to merge / push.
                const bool merging = r != nullptr && r->isolated && r->ahead > 0;
                m_landBtn->setEnabled(merging);
                m_prBtn->setEnabled(merging);
            });
    connect(m_view, &QTableView::doubleClicked, this,
            [this](const QModelIndex &) { openCommitDialog(); });
    connect(m_commitBtn, &QPushButton::clicked, this,
            &WorktreeDashboard::openCommitDialog);
    connect(m_landBtn, &QPushButton::clicked, this, &WorktreeDashboard::landSelected);
    connect(m_prBtn, &QPushButton::clicked, this, &WorktreeDashboard::openPRDialog);
}

const WorktreeRow *WorktreeDashboard::selectedRow() const
{
    const QModelIndexList sel = m_view->selectionModel()->selectedRows();
    if (sel.isEmpty()) {
        return nullptr;
    }
    return m_model->rowAt(sel.first().row());
}

void WorktreeDashboard::openPRDialog()
{
    const WorktreeRow *r = selectedRow();
    if (!r || r->branch.isEmpty()) {
        return;
    }
    auto *dlg = new PRDialog(m_core, r->threadId, r->branch, this);
    dlg->setAttribute(Qt::WA_DeleteOnClose);
    connect(dlg, &PRDialog::prOpened, this, [this](const QString &url) {
        emit statusMessage(url.isEmpty()
                               ? QStringLiteral("PR opened")
                               : QStringLiteral("PR opened: %1").arg(url));
        refresh();
    });
    dlg->show();
}

void WorktreeDashboard::landSelected()
{
    const WorktreeRow *r = selectedRow();
    if (!r || r->branch.isEmpty()) {
        return;
    }
    const QString threadId = r->threadId;
    const QString branch = r->branch;
    if (QMessageBox::question(
            this, QStringLiteral("Land into workspace?"),
            QStringLiteral("Merge <b>%1</b> into the workspace's current branch?"
                           "<br><br>Conflicts (if any) will open in KDiff3 "
                           "instead of rolling back.").arg(branch.toHtmlEscaped()),
            QMessageBox::Yes | QMessageBox::No, QMessageBox::No)
        != QMessageBox::Yes) {
        return;
    }
    m_landBtn->setEnabled(false);
    m_core->call(QStringLiteral("git.land"),
                 QJsonObject{{QStringLiteral("threadId"), threadId},
                             {QStringLiteral("keepConflicts"), true}},
                 [this, threadId, branch](const QJsonObject &result,
                                          const QJsonObject &error) {
                     m_landBtn->setEnabled(true);
                     if (!error.isEmpty()) {
                         QMessageBox::warning(
                             this, QStringLiteral("Could not land"),
                             error.value(QStringLiteral("message")).toString());
                         return;
                     }
                     const QString into =
                         result.value(QStringLiteral("into")).toString();
                     const QJsonArray confArr =
                         result.value(QStringLiteral("conflicts")).toArray();
                     if (confArr.isEmpty()) {
                         emit statusMessage(
                             QStringLiteral("Merged %1 into %2").arg(branch, into));
                         refresh();
                         return;
                     }
                     QStringList conflicts;
                     conflicts.reserve(confArr.size());
                     for (const QJsonValue &v : confArr) {
                         conflicts << v.toString();
                     }
                     auto *dlg = new ConflictDialog(m_core, threadId, branch, into,
                                                    conflicts, this);
                     dlg->setAttribute(Qt::WA_DeleteOnClose);
                     connect(dlg, &ConflictDialog::finalized, this,
                             [this, branch](const QString &, const QString &i) {
                                 emit statusMessage(QStringLiteral("Merged %1 into %2")
                                                        .arg(branch, i));
                                 refresh();
                             });
                     connect(dlg, &ConflictDialog::aborted, this,
                             [this](const QString &) {
                                 emit statusMessage(
                                     QStringLiteral("Merge aborted; workspace restored"));
                                 refresh();
                             });
                     dlg->show();
                 });
}

void WorktreeDashboard::openCommitDialog()
{
    const WorktreeRow *r = selectedRow();
    if (!r || r->threadId.isEmpty()) {
        return;
    }
    auto *dlg = new CommitDialog(m_core, r->threadId, r->branch, this);
    dlg->setAttribute(Qt::WA_DeleteOnClose);
    connect(dlg, &CommitDialog::committed, this,
            [this](const QString &, const QString &branch) {
                emit statusMessage(branch.isEmpty()
                                       ? QStringLiteral("Commit successful")
                                       : QStringLiteral("Committed to %1").arg(branch));
                refresh();
            });
    dlg->show();
}

void WorktreeDashboard::showEvent(QShowEvent *e)
{
    QWidget::showEvent(e);
    if (!m_pollTimer->isActive()) {
        m_pollTimer->start();
    }
    refresh();
}

void WorktreeDashboard::hideEvent(QHideEvent *e)
{
    QWidget::hideEvent(e);
    m_pollTimer->stop();
}

void WorktreeDashboard::refresh()
{
    if (m_inFlight || !m_core->isConnected()) {
        return;
    }
    m_inFlight = true;
    m_core->call(QStringLiteral("git.snapshot"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
                     m_inFlight = false;
                     if (!error.isEmpty()) {
                         return;
                     }
                     const QJsonArray arr =
                         result.value(QStringLiteral("threads")).toArray();
                     QList<WorktreeRow> rows;
                     rows.reserve(arr.size());
                     for (const QJsonValue &v : arr) {
                         const QJsonObject o = v.toObject();
                         WorktreeRow r;
                         r.threadId = o.value(QStringLiteral("threadId")).toString();
                         r.branch = o.value(QStringLiteral("branch")).toString();
                         r.path = o.value(QStringLiteral("path")).toString();
                         r.isolated = o.value(QStringLiteral("isolated")).toBool();
                         r.ahead = o.value(QStringLiteral("ahead")).toInt();
                         r.behindBase = o.value(QStringLiteral("behindBase")).toInt();
                         r.dirty = o.value(QStringLiteral("dirtyCount")).toInt();
                         r.conflicts = o.value(QStringLiteral("hasConflicts")).toBool();
                         r.error = o.value(QStringLiteral("error")).toString();
                         rows.append(r);
                     }
                     m_model->setRows(std::move(rows));
                 });
}

void WorktreeDashboard::onNotification(const QString &method, const QJsonObject &)
{
    // git.invalidated lands on every fs event the core's watcher sees as
    // well as on its own commit / land / discard mutations. Short-cut the
    // next poll so the dashboard reflects the change sub-second.
    if (method == QLatin1String("git.invalidated")) {
        refresh();
    }
}
