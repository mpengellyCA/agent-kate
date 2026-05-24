// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "LogViewer.h"

#include "CommitDetailPanel.h"
#include "LogGraphDelegate.h"
#include "LogModel.h"
#include "RefChipDelegate.h"
#include "ipc/CoreClient.h"

#include <QComboBox>
#include <QDateTime>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QItemSelectionModel>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
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
    m_threadPicker = new QComboBox(this);
    m_threadPicker->setMinimumContentsLength(20);
    m_threadPicker->setSizeAdjustPolicy(QComboBox::AdjustToContentsOnFirstShow);

    m_refreshBtn = new QPushButton(tr("Refresh"), this);

    auto *toolbar = new QHBoxLayout;
    toolbar->setContentsMargins(6, 4, 6, 4);
    toolbar->addWidget(m_threadPicker, 1);
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

    connect(m_threadPicker, qOverload<int>(&QComboBox::currentIndexChanged), this,
            [this](int idx) {
                const Source s = sourceForIndex(idx);
                if (s.threadId != m_source.threadId
                    || s.repoRoot != m_source.repoRoot
                    || s.branch != m_source.branch) {
                    m_source = s;
                    reloadFromFirstPage();
                }
            });
    connect(m_refreshBtn, &QPushButton::clicked, this, [this] {
        refreshThreads();
        reloadFromFirstPage();
    });
    connect(m_view->selectionModel(), &QItemSelectionModel::currentRowChanged, this,
            [this](const QModelIndex &, const QModelIndex &) { onSelectionChanged(); });
    connect(m_view->verticalScrollBar(), &QScrollBar::valueChanged, this,
            &LogViewer::onScrolled);
    connect(m_core, &CoreClient::notification, this, &LogViewer::onNotification);
    connect(m_core, &CoreClient::connected, this, [this] {
        m_threadsLoaded = false;
        refreshThreads();
    });
}

void LogViewer::setThreadId(const QString &threadId)
{
    if (threadId == m_source.threadId && m_source.repoRoot.isEmpty()) {
        return;
    }
    m_source = Source{threadId, QString(), QString()};
    // Try to reflect the selection in the picker; if the snapshot hasn't
    // arrived yet the next refreshThreads() will line it back up.
    const int idx = m_threadPicker->findData(threadId);
    if (idx >= 0) {
        QSignalBlocker block(m_threadPicker);
        m_threadPicker->setCurrentIndex(idx);
    }
    reloadFromFirstPage();
}

LogViewer::Source LogViewer::sourceForIndex(int idx) const
{
    if (idx < 0) {
        return {};
    }
    const QVariantMap m = m_threadPicker->itemData(idx).toMap();
    return Source{m.value(QStringLiteral("threadId")).toString(),
                  m.value(QStringLiteral("repoRoot")).toString(),
                  m.value(QStringLiteral("branch")).toString()};
}

void LogViewer::showEvent(QShowEvent *e)
{
    QWidget::showEvent(e);
    if (!m_threadsLoaded && m_core->isConnected()) {
        refreshThreads();
    }
}

void LogViewer::refreshThreads()
{
    if (!m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("git.snapshot"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     m_threadsLoaded = true;
                     const QJsonArray threads =
                         result.value(QStringLiteral("threads")).toArray();
                     const QJsonArray workspaces =
                         result.value(QStringLiteral("workspaces")).toArray();
                     const Source prev = m_source;
                     QSignalBlocker block(m_threadPicker);
                     m_threadPicker->clear();
                     // Workspace branches first so "main" is the easy default
                     // for users who haven't started any agent yet.
                     for (const QJsonValue &v : workspaces) {
                         const QJsonObject o = v.toObject();
                         const QString repoRoot =
                             o.value(QStringLiteral("repoRoot")).toString();
                         const QString branch =
                             o.value(QStringLiteral("branch")).toString();
                         if (repoRoot.isEmpty()) {
                             continue;
                         }
                         const QString label =
                             branch.isEmpty()
                                 ? tr("workspace · (detached)")
                                 : tr("workspace · %1").arg(branch);
                         QVariantMap d;
                         d.insert(QStringLiteral("repoRoot"), repoRoot);
                         d.insert(QStringLiteral("branch"), branch);
                         m_threadPicker->addItem(label, d);
                     }
                     for (const QJsonValue &v : threads) {
                         const QJsonObject o = v.toObject();
                         const QString id =
                             o.value(QStringLiteral("threadId")).toString();
                         const QString branch =
                             o.value(QStringLiteral("branch")).toString();
                         const int number =
                             o.value(QStringLiteral("number")).toInt();
                         QString label = branch.isEmpty() ? id : branch;
                         if (number > 0) {
                             label = QStringLiteral("#%1 · %2").arg(number).arg(label);
                         }
                         QVariantMap d;
                         d.insert(QStringLiteral("threadId"), id);
                         m_threadPicker->addItem(label, d);
                     }
                     int target = -1;
                     for (int i = 0; i < m_threadPicker->count(); ++i) {
                         const Source s = sourceForIndex(i);
                         if (s.threadId == prev.threadId
                             && s.repoRoot == prev.repoRoot
                             && s.branch == prev.branch
                             && (!s.threadId.isEmpty() || !s.repoRoot.isEmpty())) {
                             target = i;
                             break;
                         }
                     }
                     if (target < 0 && m_threadPicker->count() > 0) {
                         target = 0;
                     }
                     if (target >= 0) {
                         m_threadPicker->setCurrentIndex(target);
                         const Source s = sourceForIndex(target);
                         if (s.threadId != m_source.threadId
                             || s.repoRoot != m_source.repoRoot
                             || s.branch != m_source.branch) {
                             m_source = s;
                             // Picker signal is blocked, so fire the load here.
                             reloadFromFirstPage();
                         } else if (m_model->loadedCount() == 0) {
                             reloadFromFirstPage();
                         }
                     } else {
                         m_source = {};
                         m_model->reset();
                         m_detail->clear();
                     }
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
                     // Ask the view to resize the graph column so a widening
                     // lane fan-out doesn't get clipped.
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
        // Cheap signal: the snapshot may have grown a new worktree we don't
        // know about yet. Only refresh the picker if we don't have one
        // selected — refreshing while the user is actively reading would
        // wipe pagination.
        if (m_source.threadId.isEmpty() && m_source.repoRoot.isEmpty()) {
            refreshThreads();
        }
    }
}

void LogViewer::onScrolled(int value)
{
    QScrollBar *bar = m_view->verticalScrollBar();
    if (!bar) {
        return;
    }
    // Fetch the next page once the scrollbar is within 8 rows of the end —
    // QTableView keeps a steady fixed row height so a constant pixel margin
    // works fine here.
    const int rowH = m_view->verticalHeader()->defaultSectionSize();
    if (value >= bar->maximum() - rowH * 8) {
        loadNextPage();
    }
}
