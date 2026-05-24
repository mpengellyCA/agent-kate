// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "LogViewer.h"

#include "CommitDetailPanel.h"
#include "LogGraphDelegate.h"
#include "LogModel.h"
#include "RefChipDelegate.h"
#include "ipc/CoreClient.h"

#include <QDateTime>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QItemSelectionModel>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QLabel>
#include <QPushButton>
#include <QScrollBar>
#include <QSplitter>
#include <QStringList>
#include <QTableView>
#include <QVBoxLayout>

LogViewer::LogViewer(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    m_sourceLabel = new QLabel(this);
    m_sourceLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);

    m_refreshBtn = new QPushButton(tr("Refresh"), this);

    auto *toolbar = new QHBoxLayout;
    toolbar->setContentsMargins(6, 4, 6, 4);
    toolbar->addWidget(m_sourceLabel, 1);
    toolbar->addWidget(m_refreshBtn);

    m_model = new LogModel(this);
    m_view = new QTableView(this);
    m_view->setModel(m_model);
    m_view->setSelectionBehavior(QAbstractItemView::SelectRows);
    m_view->setSelectionMode(QAbstractItemView::SingleSelection);
    m_view->setShowGrid(false);
    m_view->setAlternatingRowColors(true);
    m_view->verticalHeader()->setVisible(false);
    m_view->verticalHeader()->setDefaultSectionSize(20);
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

    m_detail = new CommitDetailPanel(m_core, this);

    auto *split = new QSplitter(Qt::Vertical, this);
    split->addWidget(m_view);
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
        reloadFromFirstPage();
    });
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
        m_source = Source{threadId, QString(), QString()};
    } else {
        // Agent hasn't started — show the workspace branch for the project.
        m_source = Source{QString(), projectPath, QString()};
    }
    updateLabel();
    reloadFromFirstPage();
    if (threadId.isEmpty()) {
        // Will upgrade us to the agent's thread once it exists.
        resolveThreadForProject();
    }
}

void LogViewer::updateLabel()
{
    if (!m_source.threadId.isEmpty()) {
        m_sourceLabel->setText(tr("Agent worktree · %1").arg(m_source.threadId.left(8)));
    } else if (!m_source.repoRoot.isEmpty()) {
        m_sourceLabel->setText(m_source.branch.isEmpty()
                                   ? tr("Workspace · (detached)")
                                   : tr("Workspace · %1").arg(m_source.branch));
    } else {
        m_sourceLabel->setText(tr("No project selected"));
    }
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
                     m_source = Source{matchedThread, QString(), QString()};
                     updateLabel();
                     reloadFromFirstPage();
                 });
}

void LogViewer::reloadFromFirstPage()
{
    ++m_loadToken;
    m_endReached = false;
    m_pageInFlight = false;
    m_model->reset();
    m_detail->clear();
    if ((m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty())
        || !m_core->isConnected()) {
        return;
    }
    loadNextPage();
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
    if (!m_source.threadId.isEmpty()) {
        params.insert(QStringLiteral("threadId"), m_source.threadId);
    } else {
        params.insert(QStringLiteral("repoRoot"), m_source.repoRoot);
        if (!m_source.branch.isEmpty()) {
            params.insert(QStringLiteral("branch"), m_source.branch);
        }
    }
    m_core->call(QStringLiteral("git.log"), params,
                 [this, token](const QJsonObject &result, const QJsonObject &error) {
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
                         return;
                     }
                     QVector<UiLogEntry> page;
                     page.reserve(entries.size());
                     for (const QJsonValue &v : entries) {
                         const QJsonObject o = v.toObject();
                         UiLogEntry e;
                         e.sha = o.value(QStringLiteral("sha")).toString();
                         e.shortSha = o.value(QStringLiteral("shortSha")).toString();
                         e.subject = o.value(QStringLiteral("subject")).toString();
                         e.author = o.value(QStringLiteral("author")).toString();
                         e.authorEmail =
                             o.value(QStringLiteral("authorEmail")).toString();
                         e.authorTime = QDateTime::fromString(
                             o.value(QStringLiteral("authorTime")).toString(),
                             Qt::ISODate);
                         for (const QJsonValue &p :
                              o.value(QStringLiteral("parents")).toArray()) {
                             e.parents << p.toString();
                         }
                         for (const QJsonValue &r :
                              o.value(QStringLiteral("refs")).toArray()) {
                             e.refs << r.toString();
                         }
                         e.lane = o.value(QStringLiteral("lane")).toInt();
                         for (const QJsonValue &l :
                              o.value(QStringLiteral("lanesIn")).toArray()) {
                             e.lanesIn << l.toInt();
                         }
                         for (const QJsonValue &l :
                              o.value(QStringLiteral("lanesOut")).toArray()) {
                             e.lanesOut << l.toInt();
                         }
                         page.append(std::move(e));
                     }
                     const bool wasEmpty = m_model->loadedCount() == 0;
                     m_model->appendPage(page);
                     m_graphDelegate->setMaxLane(m_model->maxLane());
                     m_view->resizeColumnToContents(LogModel::ColGraph);
                     if (wasEmpty) {
                         m_view->selectRow(0);
                     }
                 });
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
        const QString id = params.value(QStringLiteral("threadId")).toString();
        if (!id.isEmpty() && id == m_source.threadId) {
            reloadFromFirstPage();
        }
        return;
    }
    if (method == QLatin1String("git.invalidated")) {
        // A new worktree may have just been registered for the active
        // agent. Try to upgrade from workspace → thread.
        if (m_source.threadId.isEmpty()) {
            resolveThreadForProject();
        } else {
            reloadFromFirstPage();
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
