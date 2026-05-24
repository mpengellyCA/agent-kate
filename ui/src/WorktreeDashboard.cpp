// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#include "WorktreeDashboard.h"
#include "ipc/CoreClient.h"

#include <QHeaderView>
#include <QJsonArray>
#include <QJsonValue>
#include <QPalette>
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

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_view);

    m_pollTimer->setInterval(kPollIntervalMs);
    connect(m_pollTimer, &QTimer::timeout, this, &WorktreeDashboard::refresh);
    connect(m_core, &CoreClient::notification, this, &WorktreeDashboard::onNotification);
    connect(m_core, &CoreClient::connected, this, &WorktreeDashboard::refresh);
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
    // Phase 1: there is no git.invalidated push yet (planned for phase 2), but
    // already react to it when it lands so the dashboard reflects core-side
    // mutations immediately instead of waiting up to one poll tick.
    if (method == QLatin1String("git.invalidated")) {
        refresh();
    }
}
