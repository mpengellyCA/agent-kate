// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "LogViewer.h"

#include "AuthorChipDelegate.h"
#include "CommitDetailDialog.h"
#include "CommitDetailPanel.h"
#include "LogGraphDelegate.h"
#include "LogModel.h"
#include "RefChipDelegate.h"
#include "ipc/CoreClient.h"
#include "shell/ElidingLabel.h"

#include <KLocalizedString>

#include <QApplication>
#include <QClipboard>
#include <QComboBox>
#include <QDateTime>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QItemSelectionModel>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QKeySequence>
#include <QLabel>
#include <QLineEdit>
#include <QMenu>
#include <QPalette>
#include <QPushButton>
#include <QScrollBar>
#include <QShortcut>
#include <QSignalBlocker>
#include <QSplitter>
#include <QStackedWidget>
#include <QStringList>
#include <QTableView>
#include <QTimer>
#include <QVBoxLayout>

namespace
{
// Decode the "entries" array of a git.log reply into model rows. Shared by the
// paginated loader and the in-place HEAD refresh so both speak the same shape.
QVector<UiLogEntry> parseLogEntries(const QJsonArray &entries)
{
    QVector<UiLogEntry> page;
    page.reserve(entries.size());
    for (const QJsonValue &v : entries) {
        const QJsonObject o = v.toObject();
        UiLogEntry e;
        e.sha = o.value(QStringLiteral("sha")).toString();
        e.shortSha = o.value(QStringLiteral("shortSha")).toString();
        e.subject = o.value(QStringLiteral("subject")).toString();
        e.author = o.value(QStringLiteral("author")).toString();
        e.authorEmail = o.value(QStringLiteral("authorEmail")).toString();
        e.authorTime = QDateTime::fromString(
            o.value(QStringLiteral("authorTime")).toString(), Qt::ISODate);
        for (const QJsonValue &p : o.value(QStringLiteral("parents")).toArray()) {
            e.parents << p.toString();
        }
        for (const QJsonValue &r : o.value(QStringLiteral("refs")).toArray()) {
            e.refs << r.toString();
        }
        e.lane = o.value(QStringLiteral("lane")).toInt();
        for (const QJsonValue &l : o.value(QStringLiteral("lanesIn")).toArray()) {
            e.lanesIn << l.toInt();
        }
        for (const QJsonValue &l : o.value(QStringLiteral("lanesOut")).toArray()) {
            e.lanesOut << l.toInt();
        }
        page.append(std::move(e));
    }
    return page;
}
} // namespace

LogViewer::LogViewer(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    // ElidingLabel so a long "Agent worktree · <sha>" / branch string elides
    // with an ellipsis instead of pinning the toolbar's minimum width.
    m_sourceLabel = new ElidingLabel(this);
    m_sourceLabel->setElideMode(Qt::ElideRight);
    m_sourceLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);

    // Branch selector — scopes the log to any local/remote branch (read-only).
    m_branchCombo = new QComboBox(this);
    m_branchCombo->setToolTip(i18n("Show the history of a different branch"));
    m_branchCombo->setSizeAdjustPolicy(QComboBox::AdjustToContents);
    m_branchCombo->setMinimumContentsLength(10);

    // Path filter — narrows history to a file or directory (plumbed `path`).
    m_pathEdit = new QLineEdit(this);
    m_pathEdit->setPlaceholderText(i18n("Path filter…"));
    m_pathEdit->setClearButtonEnabled(true);
    m_pathEdit->setToolTip(
        i18n("Only show commits that touched this file or folder"));

    // Search — client-side filter over already-loaded subjects/authors.
    m_searchEdit = new QLineEdit(this);
    m_searchEdit->setPlaceholderText(i18n("Search subjects…"));
    m_searchEdit->setClearButtonEnabled(true);
    m_searchEdit->setToolTip(
        i18n("Filter the loaded commits by subject or author"));

    m_refreshBtn = new QPushButton(i18nc("@action:button", "Refresh"), this);

    auto *toolbar = new QHBoxLayout;
    toolbar->setContentsMargins(6, 4, 6, 4);
    toolbar->setSpacing(4);
    toolbar->addWidget(m_sourceLabel, 1);
    toolbar->addWidget(m_branchCombo);
    toolbar->addWidget(m_pathEdit);
    toolbar->addWidget(m_searchEdit);
    toolbar->addWidget(m_refreshBtn);

    m_model = new LogModel(this);
    m_view = new QTableView(this);
    m_view->setModel(m_model);
    m_view->setSelectionBehavior(QAbstractItemView::SelectRows);
    m_view->setSelectionMode(QAbstractItemView::SingleSelection);
    m_view->setShowGrid(false);
    m_view->setAlternatingRowColors(true);
    // Hover highlight on the row under the cursor (GitKraken-ish feedback).
    m_view->setMouseTracking(true);
    m_view->setAttribute(Qt::WA_Hover, true);
    m_view->verticalHeader()->setVisible(false);
    // Slightly taller rows so the graph nodes and chips breathe.
    m_view->verticalHeader()->setDefaultSectionSize(
        m_view->fontMetrics().height() + 10);
    m_view->horizontalHeader()->setStretchLastSection(false);
    m_view->horizontalHeader()->setSectionResizeMode(
        LogModel::ColGraph, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        LogModel::ColSubject, QHeaderView::Stretch);
    m_view->horizontalHeader()->setSectionResizeMode(
        LogModel::ColAuthor, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        LogModel::ColDate, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        LogModel::ColShortSha, QHeaderView::ResizeToContents);

    m_graphDelegate = new LogGraphDelegate(this);
    m_view->setItemDelegateForColumn(LogModel::ColGraph, m_graphDelegate);
    m_view->setItemDelegateForColumn(LogModel::ColSubject, new RefChipDelegate(this));
    m_view->setItemDelegateForColumn(LogModel::ColAuthor, new AuthorChipDelegate(this));

    // Right-click commit actions (copy hash / subject / patch).
    m_view->setContextMenuPolicy(Qt::CustomContextMenu);
    connect(m_view, &QTableView::customContextMenuRequested, this,
            &LogViewer::showContextMenu);
    // Ctrl+C on the focused log row copies the full SHA.
    auto *copySc = new QShortcut(QKeySequence::Copy, m_view);
    copySc->setContext(Qt::WidgetShortcut);
    connect(copySc, &QShortcut::activated, this,
            [this] { copySelectedSha(false); });

    // Empty / loading state overlay swapped in for the table when there is
    // nothing to show.
    m_emptyLabel = new QLabel(this);
    m_emptyLabel->setAlignment(Qt::AlignCenter);
    m_emptyLabel->setWordWrap(true);
    {
        QPalette pal = m_emptyLabel->palette();
        pal.setColor(QPalette::WindowText,
                     pal.color(QPalette::Disabled, QPalette::WindowText));
        m_emptyLabel->setPalette(pal);
    }
    m_stack = new QStackedWidget(this);
    m_stack->addWidget(m_view);       // index 0
    m_stack->addWidget(m_emptyLabel); // index 1

    m_detail = new CommitDetailPanel(m_core, this);

    auto *split = new QSplitter(Qt::Vertical, this);
    split->addWidget(m_stack);
    split->addWidget(m_detail);
    split->setStretchFactor(0, 2);
    split->setStretchFactor(1, 3);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addLayout(toolbar);
    layout->addWidget(split, 1);

    connect(m_refreshBtn, &QPushButton::clicked, this, [this] {
        resolveThreadForProject();
        reloadBranches();
        reloadFromFirstPage();
    });
    connect(m_branchCombo, QOverload<int>::of(&QComboBox::activated), this,
            &LogViewer::onBranchChanged);
    connect(m_pathEdit, &QLineEdit::editingFinished, this,
            &LogViewer::onPathFilterChanged);
    connect(m_pathEdit, &QLineEdit::returnPressed, this,
            &LogViewer::onPathFilterChanged);
    // Debounce the search: a full row scan (up to the 5000-row cap) on every
    // keystroke is wasteful, and progressive paging shouldn't fire mid-word.
    m_searchTimer = new QTimer(this);
    m_searchTimer->setSingleShot(true);
    m_searchTimer->setInterval(250);
    connect(m_searchTimer, &QTimer::timeout, this, &LogViewer::applySearchFilter);
    connect(m_searchEdit, &QLineEdit::textChanged, this,
            [this](const QString &) { m_searchTimer->start(); });
    // Double-click a commit → tabbed modal reader.
    connect(m_view, &QTableView::doubleClicked, this,
            &LogViewer::openCommitDialog);
    connect(m_view->selectionModel(), &QItemSelectionModel::currentRowChanged, this,
            [this](const QModelIndex &, const QModelIndex &) { onSelectionChanged(); });
    connect(m_view->verticalScrollBar(), &QScrollBar::valueChanged, this,
            &LogViewer::onScrolled);
    connect(m_core, &CoreClient::notification, this, &LogViewer::onNotification);
    connect(m_core, &CoreClient::connected, this, [this] { resolveThreadForProject(); });

    updateLabel();
}

void LogViewer::setActiveSource(const QString &projectPath, const QString &threadId)
{
    if (projectPath == m_activeProject && threadId == m_source.threadId
        && (!threadId.isEmpty() || m_source.repoRoot == projectPath)) {
        return;
    }
    m_activeProject = projectPath;
    if (!threadId.isEmpty()) {
        m_source = Source{threadId, QString(), QString(), QString()};
    } else {
        // Agent hasn't started — show the workspace branch for the project.
        m_source = Source{QString(), projectPath, QString(), QString()};
    }
    // A new source resets the user's filters — its branches/paths differ. The
    // clears run under a signal blocker so they don't fire applySearchFilter for
    // the old model, so reset the derived filter state (graph-lane suppression)
    // here rather than relying on that slot.
    {
        QSignalBlocker bp(m_pathEdit);
        QSignalBlocker bs(m_searchEdit);
        m_pathEdit->clear();
        m_searchEdit->clear();
    }
    if (m_searchTimer) {
        m_searchTimer->stop();
    }
    if (m_searchFilterActive) {
        m_searchFilterActive = false;
        m_graphDelegate->setSuppressLanes(false);
    }
    updateLabel();
    reloadBranches();
    reloadFromFirstPage();
    if (threadId.isEmpty()) {
        // Will upgrade us to the agent's thread once it exists.
        resolveThreadForProject();
    }
}

void LogViewer::updateLabel()
{
    QString text;
    if (!m_source.threadId.isEmpty()) {
        text = m_source.branch.isEmpty()
                   ? i18n("Agent worktree · %1", m_source.threadId.left(8))
                   : i18n("Agent worktree · %1", m_source.branch);
    } else if (!m_source.repoRoot.isEmpty()) {
        text = m_source.branch.isEmpty()
                   ? i18n("Workspace · (current branch)")
                   : i18n("Workspace · %1", m_source.branch);
    } else {
        text = i18n("No project selected");
    }
    m_sourceLabel->setText(text);
}

// resolveThreadForProject asks the core for snapshots and, if the active
// project has a thread, switches the source to it. This is the path that
// upgrades us from "workspace" to "agent worktree" once the agent starts.
void LogViewer::resolveThreadForProject()
{
    if (m_activeProject.isEmpty() || !m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("git.snapshot"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     const QJsonArray threads =
                         result.value(QStringLiteral("threads")).toArray();
                     QString matchedThread;
                     for (const QJsonValue &v : threads) {
                         const QJsonObject o = v.toObject();
                         if (o.value(QStringLiteral("repoRoot")).toString()
                             == m_activeProject) {
                             matchedThread =
                                 o.value(QStringLiteral("threadId")).toString();
                             if (!matchedThread.isEmpty()) {
                                 break;
                             }
                         }
                     }
                     // If we already have an explicit threadId set by
                     // MainWindow, only adopt a matched thread when ours
                     // is empty — never silently swap the user's view.
                     if (matchedThread.isEmpty()
                         || matchedThread == m_source.threadId
                         || !m_source.threadId.isEmpty()) {
                         // Refresh the workspace branch label if we're in
                         // workspace mode (HEAD might have moved).
                         if (m_source.threadId.isEmpty()
                             && m_source.repoRoot == m_activeProject) {
                             const QJsonArray workspaces =
                                 result.value(QStringLiteral("workspaces"))
                                     .toArray();
                             for (const QJsonValue &v : workspaces) {
                                 const QJsonObject o = v.toObject();
                                 if (o.value(QStringLiteral("repoRoot")).toString()
                                     == m_activeProject) {
                                     m_source.branch =
                                         o.value(QStringLiteral("branch")).toString();
                                     updateLabel();
                                     break;
                                 }
                             }
                         }
                         return;
                     }
                     m_source = Source{matchedThread, QString(), QString(),
                                       QString()};
                     updateLabel();
                     reloadBranches();
                     reloadFromFirstPage();
                 },
                 this); // lifetime guard against late reply after viewer destruction
}

void LogViewer::reloadFromFirstPage()
{
    ++m_loadToken;
    m_endReached = false;
    m_pageInFlight = false;
    m_model->reset();
    m_detail->clear();
    // A full reload (branch switch / refresh / source change) throws away the
    // old first-page baseline; clearing it keeps the next HEAD refresh from
    // comparing the new source's page against a stale one.
    m_firstPage.set({});
    if ((m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty())
        || !m_core->isConnected()) {
        updateEmptyState();
        return;
    }
    // Show a loading hint until the first page lands.
    m_emptyLabel->setText(i18n("Loading history…"));
    m_stack->setCurrentIndex(1);
    loadNextPage();
}

void LogViewer::addSourceParams(QJsonObject &params, bool includeFilters) const
{
    if (!m_source.threadId.isEmpty()) {
        params.insert(QStringLiteral("threadId"), m_source.threadId);
    } else if (!m_source.repoRoot.isEmpty()) {
        params.insert(QStringLiteral("repoRoot"), m_source.repoRoot);
    }
    if (includeFilters) {
        if (!m_source.branch.isEmpty()) {
            params.insert(QStringLiteral("branch"), m_source.branch);
        }
        if (!m_source.path.isEmpty()) {
            params.insert(QStringLiteral("path"), m_source.path);
        }
    }
}

void LogViewer::loadNextPage()
{
    if (m_pageInFlight || m_endReached
        || (m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty())
        || !m_core->isConnected()) {
        return;
    }
    m_pageInFlight = true;
    const int token = m_loadToken;
    const int skip = m_model->loadedCount();
    QJsonObject params{{QStringLiteral("skip"), skip},
                       {QStringLiteral("limit"), kPageSize}};
    addSourceParams(params, /*includeFilters=*/true);
    m_core->call(QStringLiteral("git.log"), params,
                 [this, token, skip](const QJsonObject &result, const QJsonObject &error) {
                     if (token != m_loadToken) {
                         return;
                     }
                     m_pageInFlight = false;
                     if (!error.isEmpty()) {
                         return;
                     }
                     const QJsonArray entries =
                         result.value(QStringLiteral("entries")).toArray();
                     if (entries.size() < kPageSize) {
                         m_endReached = true;
                     }
                     if (entries.isEmpty()) {
                         // History exhausted mid-search: let the filter path show
                         // the "no match" hint rather than a bare empty state.
                         if (m_searchFilterActive) {
                             applySearchFilter();
                         } else {
                             updateEmptyState();
                         }
                         return;
                     }
                     const QVector<UiLogEntry> page = parseLogEntries(entries);
                     const bool wasEmpty = m_model->loadedCount() == 0;
                     m_model->appendPage(page);
                     m_graphDelegate->setMaxLane(m_model->maxLane());
                     m_view->resizeColumnToContents(LogModel::ColGraph);
                     if (wasEmpty) {
                         // Seed the first-page baseline so the next HEAD refresh
                         // can short-circuit on an identical page (skip == 0
                         // means this reply *is* the first page).
                         if (skip == 0) {
                             m_firstPage.set(page);
                         }
                         m_stack->setCurrentIndex(0);
                         m_view->selectRow(0);
                     }
                     // Re-apply any active search over the freshly-paged rows so
                     // scroll-to-load doesn't reveal rows the filter should hide.
                     applySearchFilter();
                 },
                 this);
}

void LogViewer::refreshHead()
{
    // If we have nothing loaded yet (e.g. the very first invalidation races the
    // initial load, or a previous source was empty) there is no user state to
    // preserve — fall back to the normal first-page load.
    if (m_model->loadedCount() == 0) {
        reloadFromFirstPage();
        return;
    }
    if ((m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty())
        || !m_core->isConnected()) {
        return;
    }
    // Bump the load token so any in-flight loadNextPage() (which captured an
    // older "skip" offset) is dropped when its reply lands — otherwise it would
    // append at a stale offset after we prepend new commits, duplicating or
    // gapping rows. Our own reply below carries this fresh token.
    const int token = ++m_loadToken;
    m_pageInFlight = false;
    QJsonObject params{{QStringLiteral("skip"), 0},
                       {QStringLiteral("limit"), kPageSize}};
    addSourceParams(params, /*includeFilters=*/true);
    m_core->call(
        QStringLiteral("git.log"), params,
        [this, token](const QJsonObject &result, const QJsonObject &error) {
            if (token != m_loadToken) {
                return;
            }
            if (!error.isEmpty()) {
                return;
            }
            const QJsonArray entries =
                result.value(QStringLiteral("entries")).toArray();
            const QVector<UiLogEntry> page = parseLogEntries(entries);

            // Gate on the Reactive: an identical first page (the page didn't
            // actually change, only the working tree did) is dropped, so we skip
            // the merge, the repaint and the selection/scroll dance entirely.
            // operator== on UiLogEntry keys on sha + refs + lanes, so a no-op
            // working-tree save compares equal here and short-circuits.
            if (page == m_firstPage.get()) {
                return;
            }
            m_firstPage.set(page);

            // Capture user state anchored to STABLE ids (sha), not row numbers,
            // so a prepend that shifts every row down by k can't drop the
            // selection or jump the viewport:
            //   * selectedSha — re-select the same commit afterwards.
            //   * topSha — the commit at the top of the viewport; we scroll it
            //     back to the top so the visible window stays put even when new
            //     commits are inserted above it.
            const QModelIndex cur = m_view->selectionModel()->currentIndex();
            const QString selectedSha =
                cur.isValid() ? m_model->shaAt(cur.row()) : QString();
            QScrollBar *bar = m_view->verticalScrollBar();
            const int savedScroll = bar ? bar->value() : 0;
            const QModelIndex topIdx =
                m_view->indexAt(m_view->viewport()->rect().topLeft());
            const QString topSha =
                topIdx.isValid() ? m_model->shaAt(topIdx.row()) : QString();

            const bool merged = m_model->applyHead(page);
            m_graphDelegate->setMaxLane(m_model->maxLane());
            m_view->resizeColumnToContents(LogModel::ColGraph);

            if (!merged) {
                // Histories diverged: applyHead() already reset the model.
                // reloadFromFirstPage() clears the stale first-page baseline and
                // refetches cleanly from scratch.
                reloadFromFirstPage();
                return;
            }

            updateEmptyState();

            // Restore selection by sha (its row index may have shifted when new
            // commits were prepended). Block the selection model so re-selecting
            // the same commit doesn't re-trigger the detail fetch needlessly.
            if (!selectedSha.isEmpty()) {
                const int row = m_model->rowForSha(selectedSha);
                if (row >= 0) {
                    const QModelIndex idx = m_model->index(row, 0);
                    QSignalBlocker block(m_view->selectionModel());
                    m_view->selectionModel()->setCurrentIndex(
                        idx,
                        QItemSelectionModel::ClearAndSelect
                            | QItemSelectionModel::Rows);
                }
            }
            // Restore the viewport. If we still know which commit was at the top,
            // pin it back there so prepended commits push above the fold rather
            // than shoving the user's view down; otherwise fall back to the raw
            // scrollbar offset. setValue() clamps to the new maximum itself.
            const int topRow =
                topSha.isEmpty() ? -1 : m_model->rowForSha(topSha);
            if (topRow >= 0) {
                m_view->scrollTo(m_model->index(topRow, 0),
                                 QAbstractItemView::PositionAtTop);
            } else if (bar) {
                bar->setValue(savedScroll);
            }
            // Prepended commits may need hiding under an active search.
            applySearchFilter();
        },
        this);
}

void LogViewer::onSelectionChanged()
{
    const QModelIndex idx = m_view->selectionModel()->currentIndex();
    if (!idx.isValid()) {
        m_detail->clear();
        return;
    }
    const QString sha = m_model->shaAt(idx.row());
    if (sha.isEmpty()
        || (m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty())) {
        m_detail->clear();
        return;
    }
    m_detail->setCommit(m_source.threadId, m_source.repoRoot, sha);
}

void LogViewer::onNotification(const QString &method, const QJsonObject &params)
{
    if (method == QLatin1String("git.log.invalidated")) {
        // A real history change (HEAD moved / commit landed / refs moved) for
        // the thread we're showing. Merge the new first page in place instead
        // of nuking selection + scroll + loaded pages.
        const QString id = params.value(QStringLiteral("threadId")).toString();
        if (!id.isEmpty() && id == m_source.threadId) {
            refreshHead();
        }
        return;
    }
    if (method == QLatin1String("git.invalidated")) {
        // A plain git.invalidated fires on every working-tree change — most
        // often an agent file save. A dirty working tree does NOT change
        // history, so when we already have a resolved thread there is nothing
        // to reload; reacting here is exactly the full-reset flicker we are
        // killing. We only use this signal to upgrade from the workspace view
        // to the agent's thread once a worktree first appears.
        if (m_source.threadId.isEmpty()) {
            resolveThreadForProject();
        }
    }
}

void LogViewer::onScrolled(int value)
{
    QScrollBar *bar = m_view->verticalScrollBar();
    if (!bar) {
        return;
    }
    const int rowH = m_view->verticalHeader()->defaultSectionSize();
    if (value >= bar->maximum() - rowH * 8) {
        loadNextPage();
    }
}

void LogViewer::updateEmptyState()
{
    // Index 1 shows the QLabel overlay; index 0 shows the table. Only swap to
    // the "no commits" message once we have genuinely exhausted the history.
    if (m_model->loadedCount() > 0) {
        m_stack->setCurrentIndex(0);
        return;
    }
    if (m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty()) {
        m_emptyLabel->setText(i18n("No project selected."));
    } else if (m_endReached) {
        m_emptyLabel->setText(i18n("No commits yet."));
    } else {
        m_emptyLabel->setText(i18n("Loading history…"));
    }
    m_stack->setCurrentIndex(1);
}

void LogViewer::showContextMenu(const QPoint &pos)
{
    const QModelIndex idx = m_view->indexAt(pos);
    if (!idx.isValid() || m_model->shaAt(idx.row()).isEmpty()) {
        return;
    }
    // Make sure the row under the cursor is the one the actions operate on.
    m_view->selectRow(idx.row());

    QMenu menu(this);
    const QIcon copyIcon = QIcon::fromTheme(QStringLiteral("edit-copy"));
    QAction *openCommit = menu.addAction(
        QIcon::fromTheme(QStringLiteral("document-open")),
        i18nc("@action:inmenu", "Open Commit…"));
    menu.addSeparator();
    QAction *copyHash =
        menu.addAction(copyIcon, i18nc("@action:inmenu", "Copy Commit Hash"));
    QAction *copyShort =
        menu.addAction(copyIcon, i18nc("@action:inmenu", "Copy Short Hash"));
    QAction *copySubject =
        menu.addAction(copyIcon, i18nc("@action:inmenu", "Copy Subject"));
    menu.addSeparator();
    QAction *copyPatch =
        menu.addAction(copyIcon, i18nc("@action:inmenu", "Copy as Patch"));

    QAction *chosen = menu.exec(m_view->viewport()->mapToGlobal(pos));
    if (chosen == openCommit) {
        openCommitDialog(m_view->selectionModel()->currentIndex());
    } else if (chosen == copyHash) {
        copySelectedSha(false);
    } else if (chosen == copyShort) {
        copySelectedSha(true);
    } else if (chosen == copySubject) {
        copySelectedSubject();
    } else if (chosen == copyPatch) {
        copySelectedAsPatch();
    }
}

void LogViewer::copySelectedSha(bool shortForm)
{
    const QModelIndex idx = m_view->selectionModel()->currentIndex();
    if (!idx.isValid()) {
        return;
    }
    const QString sha = shortForm ? m_model->shortShaAt(idx.row())
                                  : m_model->shaAt(idx.row());
    if (!sha.isEmpty()) {
        QApplication::clipboard()->setText(sha);
    }
}

void LogViewer::copySelectedSubject()
{
    const QModelIndex idx = m_view->selectionModel()->currentIndex();
    if (!idx.isValid()) {
        return;
    }
    const QString subject = m_model->subjectAt(idx.row());
    if (!subject.isEmpty()) {
        QApplication::clipboard()->setText(subject);
    }
}

void LogViewer::copySelectedAsPatch()
{
    const QModelIndex idx = m_view->selectionModel()->currentIndex();
    if (!idx.isValid()) {
        return;
    }
    const QString sha = m_model->shaAt(idx.row());
    if (sha.isEmpty() || !m_core->isConnected()) {
        return;
    }
    // Reuse git.commit.diff for the full patch of this commit, scoped to the
    // active source (branch/path filters don't apply to a single commit's diff).
    QJsonObject params{{QStringLiteral("sha"), sha}};
    addSourceParams(params, /*includeFilters=*/false);
    m_core->call(QStringLiteral("git.commit.diff"), params,
                 [](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     const QString patch =
                         result.value(QStringLiteral("patch")).toString();
                     if (!patch.isEmpty()) {
                         QApplication::clipboard()->setText(patch);
                     }
                 });
}

// reloadBranches asks the core for the source's branch list and repopulates the
// selector, preserving the picked branch when it still exists. The first entry
// is always "(current branch)" — an empty value that scopes the log to HEAD.
void LogViewer::reloadBranches()
{
    if ((m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty())
        || !m_core->isConnected()) {
        QSignalBlocker block(m_branchCombo);
        m_branchCombo->clear();
        m_branchCombo->addItem(i18n("(current branch)"), QString());
        return;
    }
    const QString wanted = m_source.branch;
    // Capture the source this request was issued for. Switching agents mid-flight
    // changes m_source; a slow reply that predates the switch must not populate
    // the combo with the previous repo's branches.
    const QString reqThread = m_source.threadId;
    const QString reqRepo = m_source.repoRoot;
    QJsonObject params;
    addSourceParams(params, /*includeFilters=*/false);
    m_core->call(
        QStringLiteral("git.branches"), params,
        [this, wanted, reqThread, reqRepo](const QJsonObject &result,
                                           const QJsonObject &error) {
            if (reqThread != m_source.threadId || reqRepo != m_source.repoRoot) {
                return; // superseded by a source switch — discard.
            }
            QSignalBlocker block(m_branchCombo);
            m_branchCombo->clear();
            m_branchCombo->addItem(i18n("(current branch)"), QString());
            if (error.isEmpty()) {
                const QJsonArray branches =
                    result.value(QStringLiteral("branches")).toArray();
                for (const QJsonValue &v : branches) {
                    const QJsonObject o = v.toObject();
                    const QString name =
                        o.value(QStringLiteral("name")).toString();
                    if (name.isEmpty()) {
                        continue;
                    }
                    QString label = name;
                    if (o.value(QStringLiteral("current")).toBool()) {
                        label = i18nc("current branch in the selector",
                                      "%1 (current)", name);
                    }
                    m_branchCombo->addItem(label, name);
                }
            }
            // Re-select the user's branch if it's still present; else HEAD.
            int idx = wanted.isEmpty() ? 0 : m_branchCombo->findData(wanted);
            if (idx < 0) {
                idx = 0;
            }
            m_branchCombo->setCurrentIndex(idx);
        },
        this); // lifetime guard against late reply after viewer destruction
}

void LogViewer::onBranchChanged(int index)
{
    const QString branch = m_branchCombo->itemData(index).toString();
    if (branch == m_source.branch) {
        return;
    }
    m_source.branch = branch;
    updateLabel();
    reloadFromFirstPage();
}

void LogViewer::onPathFilterChanged()
{
    const QString path = m_pathEdit->text().trimmed();
    if (path == m_source.path) {
        return;
    }
    m_source.path = path;
    reloadFromFirstPage();
}

// applySearchFilter hides loaded rows whose subject/author don't contain the
// search text (case-insensitive). Client-side over the paged-in rows only — a
// deep search would need a git.log --grep param, which the plan defers unless
// this proves insufficient.
void LogViewer::applySearchFilter()
{
    const QString needle = m_searchEdit->text().trimmed();
    const int rows = m_model->rowCount();

    // Toggle the graph-lane suppression so filtered (non-contiguous) rows don't
    // paint misleading rails. Repaint the graph column when the state flips.
    const bool nowFiltering = !needle.isEmpty();
    if (nowFiltering != m_searchFilterActive) {
        m_searchFilterActive = nowFiltering;
        m_graphDelegate->setSuppressLanes(nowFiltering);
        m_view->viewport()->update();
    }

    if (needle.isEmpty()) {
        for (int r = 0; r < rows; ++r) {
            if (m_view->isRowHidden(r)) {
                m_view->setRowHidden(r, false);
            }
        }
        updateEmptyState();
        return;
    }

    int visible = 0;
    for (int r = 0; r < rows; ++r) {
        const QModelIndex subjectIdx = m_model->index(r, LogModel::ColSubject);
        const QModelIndex authorIdx = m_model->index(r, LogModel::ColAuthor);
        const bool match =
            m_model->data(subjectIdx, Qt::DisplayRole)
                    .toString()
                    .contains(needle, Qt::CaseInsensitive)
            || m_model->data(authorIdx, Qt::DisplayRole)
                   .toString()
                   .contains(needle, Qt::CaseInsensitive);
        m_view->setRowHidden(r, !match);
        if (match) {
            ++visible;
        }
    }

    // Search dead-end guard: if nothing in the loaded window matches but more
    // history is available, page deeper. loadNextPage's reply re-applies this
    // filter, so paging continues until a match surfaces or history is exhausted
    // (m_endReached / the model's row cap) — m_endReached prevents an infinite
    // loop when there are no more pages.
    if (visible == 0) {
        if (!m_endReached && !m_pageInFlight) {
            m_emptyLabel->setText(i18n("Searching deeper for “%1”…", needle));
            m_stack->setCurrentIndex(1);
            loadNextPage();
        } else {
            m_emptyLabel->setText(i18n("No commits match “%1”.", needle));
            m_stack->setCurrentIndex(1);
        }
    } else {
        m_stack->setCurrentIndex(0);
    }
}

void LogViewer::openCommitDialog(const QModelIndex &idx)
{
    if (!idx.isValid()) {
        return;
    }
    const QString sha = m_model->shaAt(idx.row());
    if (sha.isEmpty()) {
        return;
    }
    auto *dlg = new CommitDetailDialog(m_core, m_source.threadId,
                                       m_source.repoRoot, sha, this);
    dlg->show();
    dlg->raise();
    dlg->activateWindow();
}
