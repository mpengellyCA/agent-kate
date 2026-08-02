// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorktreeDashboard.h"
#include "CleanupDialog.h"
#include "WorktreeCardDelegate.h"
#include "git/CommitDialog.h"
#include "git/ConflictDialog.h"
#include "git/PRDialog.h"
#include "git/WorktreeDiffDialog.h"
#include "ipc/CoreClient.h"
#include "shell/FlowLayout.h"

#include <KLocalizedString>

#include <QDateTime>
#include <QEvent>
#include <QHBoxLayout>
#include <QItemSelectionModel>
#include <QIcon>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QListView>
#include <QMenu>
#include <QMessageBox>
#include <QPalette>
#include <QPushButton>
#include <QResizeEvent>
#include <QSignalBlocker>
#include <QVBoxLayout>

namespace {
// Slow fallback poll cadence while visible. The dashboard refreshes reactively
// on the core's git.invalidated notification (diff-suppressed in applySnapshot),
// so this timer only backstops anything the watcher misses — no need for 1 Hz.
constexpr int kPollIntervalMs = 20000;
// Cadence at which the visible cards are repainted so "updated Xs ago" stays
// roughly current without a full snapshot round-trip.
constexpr int kRelTimeMs = 30000;
} // namespace

WorktreeModel::WorktreeModel(QObject *parent)
    : QAbstractListModel(parent)
{
}

int WorktreeModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_rows.size();
}

QVariant WorktreeModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || index.row() < 0 || index.row() >= m_rows.size()) {
        return {};
    }
    const WorktreeRow &r = m_rows.at(index.row());
    if (role == WorktreeRoles::Row) {
        // Hand the delegate a pointer to the row it paints. Safe for the
        // duration of the paint call; the model outlives every paint.
        return QVariant::fromValue(
            static_cast<void *>(const_cast<WorktreeRow *>(&r)));
    }
    if (role == Qt::ToolTipRole) {
        if (!r.error.isEmpty()) {
            return r.error;
        }
        QString tip = i18n("thread %1\n%2", r.threadId, r.path);
        // Audit F50: a workspace row is indistinguishable from an isolated one
        // on the card, and the difference is what Discard destroys — say it.
        if (!r.isolated) {
            tip += QLatin1Char('\n') + WorktreeCopy::notIsolatedTooltip();
        }
        if (r.hasUpstream) {
            tip += QLatin1Char('\n')
                   + i18nc("remote tracking tooltip: ahead/behind counts vs origin",
                           "%1 ahead, %2 behind origin/%3.",
                           r.remoteAhead, r.remoteBehind, r.branch);
        }
        return tip;
    }
    return {};
}

void WorktreeModel::setRows(QList<WorktreeRow> rows)
{
    // The core sorts snapshots by threadId (cd8ec6a) and threadIds are
    // immutable, so old and new lists are both sorted on the same stable key.
    // A merge walk emits insertions / removals / dataChanged in place — no
    // beginResetModel, which would otherwise clear the QListView's selection
    // on every poll.
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
            // Same thread: only touch the row (and repaint it) when a
            // load-bearing field actually changed. An identical row is left
            // untouched so the QListView never repaints it.
            // Compare git-sourced fields (operator==) *and* the merged-in
            // title, which lives outside the snapshot and so is excluded from
            // operator== — a title-only change must still repaint.
            if (m_rows[i] != rows[j] || m_rows[i].title != rows[j].title) {
                m_rows[i] = std::move(rows[j]);
                emit dataChanged(index(i), index(i));
            }
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
    , m_view(new QListView(this))
    , m_model(new WorktreeModel(this))
    , m_pollTimer(new QTimer(this))
    , m_relTimeTimer(new QTimer(this))
{
    m_view->setModel(m_model);
    m_view->setItemDelegate(new WorktreeCardDelegate(m_view));
    m_view->setSelectionMode(QAbstractItemView::SingleSelection);
    m_view->setUniformItemSizes(true); // every card is the same height
    m_view->setMouseTracking(true);    // so hover repaints the card
    m_view->setResizeMode(QListView::Adjust);
    m_view->setFrameShape(QFrame::NoFrame);
    m_view->setVerticalScrollMode(QAbstractItemView::ScrollPerPixel);

    // Empty-state overlay: a centred hint shown over the (empty) list when the
    // active project has no agent worktrees. Parented to the viewport so it
    // floats above the list area; repositioned on resize via an event filter.
    m_placeholder = new QLabel(
        i18n("No agent worktrees in this project yet."), m_view->viewport());
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
    // "Land into workspace…", not "into main": git.land merges into whatever
    // branch the workspace is currently on (worktree.LandWithOptions reads
    // `git branch --show-current`), which is very often not main (audit F50).
    m_landBtn = new QPushButton(i18nc("@action:button", "Land into workspace…"), this);
    m_landBtn->setEnabled(false);
    m_prBtn = new QPushButton(i18nc("@action:button", "Open PR…"), this);
    m_prBtn->setEnabled(false);
    // Cleanup is always enabled — it analyses every worktree, not the selection.
    m_cleanupBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("edit-clear-history")),
        i18nc("@action:button", "Analyze && Clean up…"), this);
    // FlowLayout so the action buttons wrap onto extra rows when the panel is
    // dragged narrow instead of clipping. No stretch — the buttons flow
    // left-to-right and reflow downward as width shrinks.
    auto *toolbar = new FlowLayout;
    toolbar->setContentsMargins(6, 4, 6, 4);
    toolbar->addWidget(m_cleanupBtn);
    toolbar->addWidget(m_discardBtn);
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
    m_relTimeTimer->setInterval(kRelTimeMs);
    connect(m_relTimeTimer, &QTimer::timeout, this, [this] {
        // Nudge the visible cards to repaint so "updated Xs ago" advances.
        if (m_model->rowCount() > 0) {
            m_view->viewport()->update();
        }
    });
    connect(m_core, &CoreClient::notification, this, &WorktreeDashboard::onNotification);
    connect(m_core, &CoreClient::connected, this, &WorktreeDashboard::refresh);
    connect(m_view->selectionModel(), &QItemSelectionModel::selectionChanged, this,
            [this] { updateActionEnablement(); });
    connect(m_view, &QListView::doubleClicked, this,
            [this](const QModelIndex &) { openDiffDialog(); });
    m_view->setContextMenuPolicy(Qt::CustomContextMenu);
    connect(m_view, &QListView::customContextMenuRequested, this,
            &WorktreeDashboard::showRowContextMenu);
    connect(m_commitBtn, &QPushButton::clicked, this,
            &WorktreeDashboard::openCommitDialog);
    connect(m_landBtn, &QPushButton::clicked, this, &WorktreeDashboard::landSelected);
    connect(m_prBtn, &QPushButton::clicked, this, &WorktreeDashboard::openPRDialog);
    connect(m_discardBtn, &QPushButton::clicked, this,
            &WorktreeDashboard::discardSelected);
    connect(m_cleanupBtn, &QPushButton::clicked, this,
            &WorktreeDashboard::analyzeAndCleanup);

    // Drive the table from the canonical snapshot. set() drops an identical
    // payload (see refresh()), so this fires only when the snapshot truly
    // changed — an unchanged poll produces no setRows() and no repaint.
    m_snapshot.subscribe(this, [this](const QList<WorktreeRow> &rows) {
        applySnapshot(rows);
    });
    // The snapshot starts empty and an all-empty first reply is dropped by
    // set(), so applySnapshot() would not run to reveal the empty-state hint.
    // Show it up front; later snapshots keep it in sync via applySnapshot().
    updatePlaceholder();
}

void WorktreeDashboard::analyzeAndCleanup()
{
    auto *dlg = new CleanupDialog(m_core, m_activeProject, this);
    dlg->setAttribute(Qt::WA_DeleteOnClose);
    connect(dlg, &CleanupDialog::statusMessage, this,
            [this](const QString &text) { emit statusMessage(text); });
    connect(dlg, &CleanupDialog::cleaned, this, [this] { refresh(); });
    dlg->show();
}

void WorktreeDashboard::setActiveProject(const QString &projectPath)
{
    if (projectPath == m_activeProject) {
        return;
    }
    m_activeProject = projectPath;
    refresh();
}

void WorktreeDashboard::setAgentTitles(const QHash<QString, QString> &titlesByThread)
{
    if (titlesByThread == m_titlesByThread) {
        return;
    }
    m_titlesByThread = titlesByThread;
    // Re-merge the titles into the current rows without a fresh snapshot: run
    // the existing rows back through applySnapshot (setRows repaints only the
    // cards whose title actually changed).
    applySnapshot(m_snapshot.get());
}

void WorktreeDashboard::updateActionEnablement()
{
    const WorktreeRow *r = selectedRow();
    m_commitBtn->setEnabled(r != nullptr);
    // Land and Open PR only make sense for an isolated worktree on its own
    // branch with commits actually to merge / push.
    const bool merging = r != nullptr && r->isolated && r->ahead > 0;
    m_landBtn->setEnabled(merging);
    m_prBtn->setEnabled(merging);
    // Discard is only meaningful when there are uncommitted changes.
    m_discardBtn->setEnabled(r != nullptr && r->dirty > 0);
}

void WorktreeDashboard::openDiffDialog()
{
    const WorktreeRow *r = selectedRow();
    if (!r || r->threadId.isEmpty()) {
        return;
    }
    const bool canMerge = r->isolated && r->ahead > 0;
    auto *dlg = new WorktreeDiffDialog(m_core, r->threadId, r->branch, r->number,
                                       canMerge, this);
    // The dashboard owns the action dialogs, so the diff modal hands off to us.
    connect(dlg, &WorktreeDiffDialog::commitRequested, this,
            [this](const QString &) { openCommitDialog(); });
    connect(dlg, &WorktreeDiffDialog::landRequested, this,
            [this](const QString &) { landSelected(); });
    connect(dlg, &WorktreeDiffDialog::prRequested, this,
            [this](const QString &) { openPRDialog(); });
    dlg->show();
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
                 },
                 this); // lifetime guard against late reply after dashboard destruction
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
    // SAFETY (audit F29): a non-isolated row's dirty count is the porcelain
    // status of the user's REAL checkout — their own uncommitted work included —
    // and the core will happily `git reset --hard` it. The prompt therefore
    // branches: see WorktreeCopy::discardPrompt for what each branch must say.
    const WorktreeCopy::DiscardPrompt prompt =
        WorktreeCopy::discardPrompt(r->isolated, r->number, r->branch, r->path,
                                    r->dirty);
    QMessageBox box(this);
    box.setIcon(QMessageBox::Warning);
    box.setWindowTitle(prompt.title);
    box.setTextFormat(Qt::RichText);
    box.setText(prompt.body);
    // A named destructive button rather than "Yes": the label is the last thing
    // read before the work is gone. Cancel stays the default and the Esc action.
    QPushButton *go = box.addButton(prompt.confirmLabel, QMessageBox::DestructiveRole);
    QPushButton *cancel = box.addButton(QMessageBox::Cancel);
    box.setDefaultButton(cancel);
    box.setEscapeButton(cancel);
    box.exec();
    if (box.clickedButton() != go) {
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
                 },
                 this); // lifetime guard against late reply after dashboard destruction
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
                 },
                 this); // lifetime guard against late reply after dashboard destruction
}

void WorktreeDashboard::showRowContextMenu(const QPoint &pos)
{
    const QModelIndex idx = m_view->indexAt(pos);
    if (!idx.isValid()) {
        return;
    }
    m_view->setCurrentIndex(idx);
    const WorktreeRow *r = selectedRow();
    if (!r) {
        return;
    }

    QMenu menu(this);
    QAction *diffAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("vcs-diff")),
        i18nc("@action:inmenu", "View changes…"));
    diffAct->setEnabled(!r->threadId.isEmpty());
    QAction *termAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("utilities-terminal")),
        i18nc("@action:inmenu", "Open Terminal Here"));
    termAct->setEnabled(!r->path.isEmpty());
    menu.addSeparator();
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
    menu.addSeparator();
    // Project-wide bulk cleanup — always enabled, analyses every worktree.
    QAction *cleanupAct = menu.addAction(
        QIcon::fromTheme(QStringLiteral("edit-clear-history")),
        i18nc("@action:inmenu", "Analyze && Clean up…"));

    QAction *chosen = menu.exec(m_view->viewport()->mapToGlobal(pos));
    if (chosen == diffAct) {
        openDiffDialog();
    } else if (chosen == termAct) {
        emit openTerminalRequested(r->path);
    } else if (chosen == commitAct) {
        openCommitDialog();
    } else if (chosen == discardAct) {
        discardSelected();
    } else if (chosen == removeAct) {
        removeSelected();
    } else if (chosen == cleanupAct) {
        analyzeAndCleanup();
    }
}

void WorktreeDashboard::showEvent(QShowEvent *e)
{
    QWidget::showEvent(e);
    if (!m_pollTimer->isActive()) {
        m_pollTimer->start();
    }
    if (!m_relTimeTimer->isActive()) {
        m_relTimeTimer->start();
    }
    refresh();
}

void WorktreeDashboard::hideEvent(QHideEvent *e)
{
    QWidget::hideEvent(e);
    m_pollTimer->stop();
    m_relTimeTimer->stop();
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
    if (m_inFlight) {
        // A snapshot is already in flight; remember that fresher data arrived so
        // we re-issue once the reply lands rather than dropping this invalidation.
        m_refreshPending = true;
        return;
    }
    if (!m_core->isConnected()) {
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
                         // updatedAt is an RFC3339 timestamp (Go time.Time);
                         // flatten to secs-since-epoch for the card's second line.
                         const QDateTime up = QDateTime::fromString(
                             o.value(QStringLiteral("updatedAt")).toString(),
                             Qt::ISODate);
                         r.updatedAt = up.isValid() ? up.toSecsSinceEpoch() : 0;
                         r.error = o.value(QStringLiteral("error")).toString();
                         rows.append(r);
                     }
                     // Route through the Reactive snapshot: an identical list
                     // is dropped here, so applySnapshot() (and thus setRows /
                     // any repaint) runs only on a genuine change.
                     m_snapshot.set(std::move(rows));
                     // Catch up on any invalidation that arrived mid-flight.
                     if (m_refreshPending) {
                         m_refreshPending = false;
                         refresh();
                     }
                 },
                 this);
}

void WorktreeDashboard::applySnapshot(const QList<WorktreeRow> &rows)
{
    // Remember which thread is selected so we can re-select it after the
    // model swap; the merge inside setRows can shift row indices.
    const WorktreeRow *prev = selectedRow();
    const QString selectedThread = prev ? prev->threadId : QString();

    // Merge the roster titles in here (not in refresh) so setAgentTitles can
    // re-apply against the last snapshot without a fresh RPC. Titles live
    // outside the git snapshot's value-equality, so this never fights the
    // Reactive guard.
    QList<WorktreeRow> merged = rows;
    for (WorktreeRow &r : merged) {
        r.title = m_titlesByThread.value(r.threadId);
    }
    m_model->setRows(std::move(merged));

    // Re-select the same thread by id. Block the selection model so the swap
    // does not transiently fire selectionChanged (which would flicker the
    // toolbar button state); we refresh enablement explicitly below.
    {
        QSignalBlocker block(m_view->selectionModel());
        if (!selectedThread.isEmpty()) {
            for (int row = 0; row < m_model->rowCount(); ++row) {
                const WorktreeRow *r = m_model->rowAt(row);
                if (r && r->threadId == selectedThread) {
                    m_view->setCurrentIndex(m_model->index(row));
                    break;
                }
            }
        }
    }

    updatePlaceholder();
    // Row data (esp. dirty count) may have changed under the current
    // selection without the selection model firing — refresh the action
    // enablement to match.
    updateActionEnablement();
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
