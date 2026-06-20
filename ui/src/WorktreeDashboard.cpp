// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorktreeDashboard.h"
#include "git/CommitDialog.h"
#include "git/ConflictDialog.h"
#include "git/PRDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QEvent>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QItemSelectionModel>
#include <QIcon>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QMenu>
#include <QMessageBox>
#include <QPalette>
#include <QPushButton>
#include <QResizeEvent>
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
        case ColAgent:
            return r.number > 0 ? QStringLiteral("#%1").arg(r.number) : QString();
        case ColBranch:
            return r.branch.isEmpty()
                       ? i18nc("git branch state", "(detached)")
                       : r.branch;
        case ColIsolation:
            return r.isolated
                       ? i18nc("agent runs in its own git worktree", "worktree")
                       : i18nc("agent runs directly in the workspace", "workspace");
        case ColAhead:
            return r.ahead;
        case ColBehind:
            return r.behindBase;
        case ColRemote:
            // No upstream tracking branch (never pushed) → "local". Otherwise
            // compact ↑ahead ↓behind against origin/<branch>.
            if (!r.hasUpstream) {
                return i18nc("branch has no upstream / has never been pushed",
                             "local");
            }
            return QStringLiteral("↑%1 ↓%2").arg(r.remoteAhead).arg(r.remoteBehind);
        case ColDirty:
            if (r.conflicts) {
                return i18nc("dirty file count followed by a conflict marker",
                             "%1 · conflicts", r.dirty);
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
        if (index.column() == ColRemote) {
            if (!r.hasUpstream) {
                return i18n("No upstream branch — this branch has not been "
                            "pushed to origin.");
            }
            return i18nc("remote tracking tooltip: ahead/behind counts vs origin",
                         "%1 ahead, %2 behind origin/%3.",
                         r.remoteAhead, r.remoteBehind, r.branch);
        }
        return i18n("thread %1\n%2", r.threadId, r.path);
    }
    if (role == Qt::ForegroundRole && r.conflicts && index.column() == ColDirty) {
        QPalette pal;
        return pal.color(QPalette::BrightText);
    }
    if (role == Qt::TextAlignmentRole) {
        switch (index.column()) {
        case ColAgent:
        case ColAhead:
        case ColBehind:
        case ColRemote:
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
    case ColAgent:
        return i18nc("worktree dashboard column", "Agent");
    case ColBranch:
        return i18nc("worktree dashboard column", "Branch");
    case ColIsolation:
        return i18nc("worktree dashboard column", "Mode");
    case ColAhead:
        return QStringLiteral("↑");
    case ColBehind:
        return QStringLiteral("↓");
    case ColRemote:
        return i18nc("worktree dashboard column: state vs origin remote",
                     "Remote");
    case ColDirty:
        return i18nc("worktree dashboard column", "Dirty");
    case ColPath:
        return i18nc("worktree dashboard column", "Path");
    }
    return {};
}

void WorktreeModel::setRows(QList<WorktreeRow> rows)
{
    // The core sorts snapshots by threadId (cd8ec6a) and threadIds are
    // immutable, so old and new lists are both sorted on the same stable key.
    // A merge walk emits insertions / removals / dataChanged in place — no
    // beginResetModel, which would otherwise clear the QTableView's selection
    // on every 1 Hz poll.
    int i = 0;
    int j = 0;
    while (i < m_rows.size() || j < rows.size()) {
        if (j >= rows.size()) {
            beginRemoveRows({}, i, i);
            m_rows.removeAt(i);
            endRemoveRows();
            continue;
        }
        if (i >= m_rows.size()) {
            beginInsertRows({}, i, i);
            m_rows.append(std::move(rows[j]));
            endInsertRows();
            ++i;
            ++j;
            continue;
        }
        const QString &oldId = m_rows[i].threadId;
        const QString &newId = rows[j].threadId;
        if (oldId == newId) {
            m_rows[i] = std::move(rows[j]);
            emit dataChanged(index(i, 0), index(i, ColCount - 1));
            ++i;
            ++j;
        } else if (oldId < newId) {
            beginRemoveRows({}, i, i);
            m_rows.removeAt(i);
            endRemoveRows();
        } else {
            beginInsertRows({}, i, i);
            m_rows.insert(i, std::move(rows[j]));
            endInsertRows();
            ++i;
            ++j;
        }
    }
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
        WorktreeModel::ColAgent, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColBranch, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColIsolation, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColAhead, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColBehind, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColRemote, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        WorktreeModel::ColDirty, QHeaderView::ResizeToContents);

    // Empty-state overlay: a centred hint shown over the (empty) table when
    // the active project has no running agents. Parented to the viewport so it
    // floats above the table area; repositioned on resize via an event filter.
    m_placeholder = new QLabel(
        i18n("No agents running in this project yet."), m_view->viewport());
    m_placeholder->setAlignment(Qt::AlignCenter);
    m_placeholder->setWordWrap(true);
    {
        QPalette pal = m_placeholder->palette();
        pal.setColor(QPalette::WindowText, pal.color(QPalette::Disabled,
                                                     QPalette::WindowText));
        m_placeholder->setPalette(pal);
    }
    m_placeholder->hide();
    m_view->viewport()->installEventFilter(this);

    m_discardBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("edit-clear")),
        i18nc("@action:button discard uncommitted changes", "Discard changes…"),
        this);
    m_discardBtn->setEnabled(false);
    m_commitBtn = new QPushButton(i18nc("@action:button", "Commit selected…"), this);
    m_commitBtn->setEnabled(false);
    m_landBtn = new QPushButton(i18nc("@action:button", "Land into main…"), this);
    m_landBtn->setEnabled(false);
    m_prBtn = new QPushButton(i18nc("@action:button", "Open PR…"), this);
    m_prBtn->setEnabled(false);
    auto *toolbar = new QHBoxLayout;
    toolbar->setContentsMargins(6, 4, 6, 4);
    toolbar->addWidget(m_discardBtn);
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
                // Discard is only meaningful when there are uncommitted changes.
                m_discardBtn->setEnabled(r != nullptr && r->dirty > 0);
            });
    connect(m_view, &QTableView::doubleClicked, this,
            [this](const QModelIndex &) { openCommitDialog(); });
    m_view->setContextMenuPolicy(Qt::CustomContextMenu);
    connect(m_view, &QTableView::customContextMenuRequested, this,
            &WorktreeDashboard::showRowContextMenu);
    connect(m_commitBtn, &QPushButton::clicked, this,
            &WorktreeDashboard::openCommitDialog);
    connect(m_landBtn, &QPushButton::clicked, this, &WorktreeDashboard::landSelected);
    connect(m_prBtn, &QPushButton::clicked, this, &WorktreeDashboard::openPRDialog);
    connect(m_discardBtn, &QPushButton::clicked, this,
            &WorktreeDashboard::discardSelected);
}

void WorktreeDashboard::setActiveProject(const QString &projectPath)
{
    if (projectPath == m_activeProject) {
        return;
    }
    m_activeProject = projectPath;
    refresh();
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
                               ? i18n("PR opened")
                               : i18n("PR opened: %1", url));
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
            this, i18nc("@title:window", "Land into workspace?"),
            i18n("Merge <b>%1</b> into the workspace's current branch?"
                 "<br><br>Conflicts (if any) will open in KDiff3 "
                 "instead of rolling back.", branch.toHtmlEscaped()),
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
                             this, i18nc("@title:window", "Could not land"),
                             error.value(QStringLiteral("message")).toString());
                         return;
                     }
                     const QString into =
                         result.value(QStringLiteral("into")).toString();
                     const QJsonArray confArr =
                         result.value(QStringLiteral("conflicts")).toArray();
                     if (confArr.isEmpty()) {
                         emit statusMessage(i18n("Merged %1 into %2", branch, into));
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
                                 emit statusMessage(
                                     i18n("Merged %1 into %2", branch, i));
                                 refresh();
                             });
                     connect(dlg, &ConflictDialog::aborted, this,
                             [this](const QString &) {
                                 emit statusMessage(
                                     i18n("Merge aborted; workspace restored"));
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
                                       ? i18n("Commit successful")
                                       : i18n("Committed to %1", branch));
                refresh();
            });
    dlg->show();
}

void WorktreeDashboard::discardSelected()
{
    const WorktreeRow *r = selectedRow();
    if (!r || r->threadId.isEmpty() || r->dirty == 0) {
        return;
    }
    const QString threadId = r->threadId;
    const QString label = r->number > 0 ? QStringLiteral("#%1").arg(r->number)
                                        : r->branch;
    if (QMessageBox::question(
            this, i18nc("@title:window", "Discard all changes?"),
            i18np("Permanently discard the 1 uncommitted change in worktree "
                  "<b>%2</b>?<br><br>This runs <tt>git reset --hard</tt> and "
                  "<tt>git clean</tt> — it cannot be undone.",
                  "Permanently discard all %1 uncommitted changes in worktree "
                  "<b>%2</b>?<br><br>This runs <tt>git reset --hard</tt> and "
                  "<tt>git clean</tt> — it cannot be undone.",
                  r->dirty, label.toHtmlEscaped()),
            QMessageBox::Yes | QMessageBox::No, QMessageBox::No)
        != QMessageBox::Yes) {
        return;
    }
    m_discardBtn->setEnabled(false);
    m_core->call(QStringLiteral("git.discardChanges"),
                 QJsonObject{{QStringLiteral("threadId"), threadId}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         QMessageBox::warning(
                             this, i18nc("@title:window", "Could not discard changes"),
                             error.value(QStringLiteral("message")).toString());
                         return;
                     }
                     emit statusMessage(i18n("Discarded uncommitted changes"));
                     refresh();
                 });
}

void WorktreeDashboard::removeSelected()
{
    const WorktreeRow *r = selectedRow();
    if (!r || r->threadId.isEmpty() || !r->isolated) {
        return;
    }
    const QString threadId = r->threadId;
    const QString label = r->number > 0 ? QStringLiteral("#%1").arg(r->number)
                                        : r->branch;
    QString warning =
        i18n("Remove the isolated worktree and delete branch <b>%1</b>?"
             "<br><br>The agent thread will be discarded and cannot be resumed.",
             (r->branch.isEmpty() ? label : r->branch).toHtmlEscaped());
    if (r->dirty > 0) {
        warning += QStringLiteral("<br><br><b>")
                   + i18np("There is 1 uncommitted change that will be lost.",
                           "There are %1 uncommitted changes that will be lost.",
                           r->dirty)
                   + QStringLiteral("</b>");
    }
    if (QMessageBox::question(
            this, i18nc("@title:window", "Remove worktree?"), warning,
            QMessageBox::Yes | QMessageBox::No, QMessageBox::No)
        != QMessageBox::Yes) {
        return;
    }
    m_core->call(QStringLiteral("git.removeWorktree"),
                 QJsonObject{{QStringLiteral("threadId"), threadId}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         QMessageBox::warning(
                             this, i18nc("@title:window", "Could not remove worktree"),
                             error.value(QStringLiteral("message")).toString());
                         return;
                     }
                     emit statusMessage(i18n("Worktree removed"));
                     refresh();
                 });
}

void WorktreeDashboard::showRowContextMenu(const QPoint &pos)
{
    const QModelIndex idx = m_view->indexAt(pos);
    if (!idx.isValid()) {
        return;
    }
    m_view->selectRow(idx.row());
    const WorktreeRow *r = selectedRow();
    if (!r) {
        return;
    }

    QMenu menu(this);
    QAction *commitAct = menu.addAction(
        i18nc("@action:inmenu", "Commit changes…"));
    commitAct->setEnabled(!r->threadId.isEmpty());
    menu.addSeparator();
    QAction *discardAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("edit-clear")),
        i18nc("@action:inmenu", "Discard changes…"));
    discardAct->setEnabled(r->dirty > 0 && !r->threadId.isEmpty());
    QAction *removeAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("edit-delete")),
        i18nc("@action:inmenu", "Remove worktree…"));
    // Only isolated worktrees can be removed (never the shared workspace).
    removeAct->setEnabled(r->isolated && !r->threadId.isEmpty());

    QAction *chosen = menu.exec(m_view->viewport()->mapToGlobal(pos));
    if (chosen == commitAct) {
        openCommitDialog();
    } else if (chosen == discardAct) {
        discardSelected();
    } else if (chosen == removeAct) {
        removeSelected();
    }
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

bool WorktreeDashboard::eventFilter(QObject *watched, QEvent *event)
{
    if (watched == m_view->viewport() && event->type() == QEvent::Resize
        && m_placeholder) {
        m_placeholder->resize(m_view->viewport()->size());
    }
    return QWidget::eventFilter(watched, event);
}

void WorktreeDashboard::updatePlaceholder()
{
    if (!m_placeholder) {
        return;
    }
    const bool empty = m_model->rowCount() == 0;
    if (empty) {
        m_placeholder->resize(m_view->viewport()->size());
    }
    m_placeholder->setVisible(empty);
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
                         // Workspace scoping: the dashboard reflects the
                         // currently-selected project, not every project
                         // the daemon knows about.
                         if (!m_activeProject.isEmpty()
                             && o.value(QStringLiteral("repoRoot")).toString()
                                    != m_activeProject) {
                             continue;
                         }
                         WorktreeRow r;
                         r.threadId = o.value(QStringLiteral("threadId")).toString();
                         r.number = o.value(QStringLiteral("number")).toInt();
                         r.branch = o.value(QStringLiteral("branch")).toString();
                         r.path = o.value(QStringLiteral("path")).toString();
                         r.isolated = o.value(QStringLiteral("isolated")).toBool();
                         r.ahead = o.value(QStringLiteral("ahead")).toInt();
                         r.behindBase = o.value(QStringLiteral("behindBase")).toInt();
                         r.dirty = o.value(QStringLiteral("dirtyCount")).toInt();
                         r.conflicts = o.value(QStringLiteral("hasConflicts")).toBool();
                         r.hasUpstream =
                             o.value(QStringLiteral("hasUpstream")).toBool();
                         r.remoteAhead =
                             o.value(QStringLiteral("remoteAhead")).toInt();
                         r.remoteBehind =
                             o.value(QStringLiteral("remoteBehind")).toInt();
                         r.error = o.value(QStringLiteral("error")).toString();
                         rows.append(r);
                     }
                     m_model->setRows(std::move(rows));
                     updatePlaceholder();
                     // Row data (esp. dirty count) may have changed under the
                     // current selection without the selection model firing —
                     // refresh the action enablement to match.
                     const WorktreeRow *sel = selectedRow();
                     m_commitBtn->setEnabled(sel != nullptr);
                     const bool merging =
                         sel != nullptr && sel->isolated && sel->ahead > 0;
                     m_landBtn->setEnabled(merging);
                     m_prBtn->setEnabled(merging);
                     m_discardBtn->setEnabled(sel != nullptr && sel->dirty > 0);
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
