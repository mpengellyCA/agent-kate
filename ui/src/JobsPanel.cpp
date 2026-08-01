// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "JobsPanel.h"

#include "SubAgentTranscriptDialog.h"

#include <KLocalizedString>

#include <QComboBox>
#include <QDateTime>
#include <QFont>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QSet>
#include <QStringList>
#include <QTimer>
#include <QTreeWidget>
#include <QVBoxLayout>

#include <algorithm>

using agentkate::AgentJob;

namespace
{
// Item data roles carrying the identity of the job a row stands for. The row
// itself is display text; every action re-resolves through these.
constexpr int kRoleThread = Qt::UserRole + 1;
constexpr int kRoleJobId = Qt::UserRole + 2;
constexpr int kRoleKind = Qt::UserRole + 3;
constexpr int kRoleOutput = Qt::UserRole + 4;
constexpr int kRoleStarted = Qt::UserRole + 5;
constexpr int kRoleDone = Qt::UserRole + 6;
// Job row vs. agent group row. Explicit rather than inferred from a non-empty
// job id: a job whose id never arrived is still a job, and would otherwise
// render as an inert row that looks selectable but does nothing.
constexpr int kRoleIsJob = Qt::UserRole + 7;

enum Column { ColJob = 0, ColKind, ColState, ColElapsed, ColCount };

QString kindLabel(AgentJob::Kind kind)
{
    switch (kind) {
    case AgentJob::Kind::Shell:
        return i18nc("background job kind", "Shell");
    case AgentJob::Kind::Subagent:
        return i18nc("background job kind", "Sub-agent");
    case AgentJob::Kind::Workflow:
        return i18nc("background job kind", "Workflow");
    }
    return QString();
}

// elapsedText renders a duration coarsely — a background job's age is read at a
// glance, and a ticking seconds counter would be noise. endedMs is the job's
// finish stamp, or 0 for work still running (measure against now).
QString elapsedText(qint64 startedMs, qint64 endedMs = 0)
{
    if (startedMs <= 0) {
        return QString();
    }
    const qint64 until = endedMs > 0 ? endedMs : QDateTime::currentMSecsSinceEpoch();
    // A clock step or an out-of-order stamp must not render as a negative age.
    const qint64 secs = std::max<qint64>(0, until - startedMs) / 1000;
    if (secs < 60) {
        return i18nc("job age in seconds", "%1s", secs);
    }
    if (secs < 3600) {
        return i18nc("job age in minutes", "%1m", secs / 60);
    }
    return i18nc("job age, hours and minutes", "%1h %2m", secs / 3600, (secs % 3600) / 60);
}
} // namespace

JobsPanel::JobsPanel(QWidget *parent)
    : QWidget(parent)
{
    auto *root = new QVBoxLayout(this);
    root->setContentsMargins(6, 6, 6, 6);
    root->setSpacing(6);

    m_header = new QLabel(this);
    m_header->setWordWrap(true);
    root->addWidget(m_header);

    auto *tools = new QHBoxLayout;
    tools->setSpacing(6);

    m_stateFilter = new QComboBox(this);
    m_stateFilter->addItem(i18nc("job state filter", "All"));
    m_stateFilter->addItem(i18nc("job state filter", "Running"));
    m_stateFilter->addItem(i18nc("job state filter", "Finished"));
    connect(m_stateFilter, &QComboBox::currentIndexChanged, this, &JobsPanel::rebuild);
    tools->addWidget(m_stateFilter);

    m_textFilter = new QLineEdit(this);
    m_textFilter->setClearButtonEnabled(true);
    m_textFilter->setPlaceholderText(i18n("Filter jobs…"));
    connect(m_textFilter, &QLineEdit::textChanged, this, &JobsPanel::rebuild);
    tools->addWidget(m_textFilter, 1);

    m_openBtn = new QPushButton(i18n("Open"), this);
    m_openBtn->setEnabled(false);
    connect(m_openBtn, &QPushButton::clicked, this, &JobsPanel::openSelected);
    tools->addWidget(m_openBtn);

    m_gotoBtn = new QPushButton(i18n("Go to agent"), this);
    m_gotoBtn->setEnabled(false);
    connect(m_gotoBtn, &QPushButton::clicked, this, [this] {
        if (QTreeWidgetItem *item = m_tree->currentItem()) {
            const QString thread = item->data(ColJob, kRoleThread).toString();
            if (!thread.isEmpty()) {
                Q_EMIT goToAgentRequested(thread);
            }
        }
    });
    tools->addWidget(m_gotoBtn);

    m_clearBtn = new QPushButton(i18n("Clear finished"), this);
    m_clearBtn->setToolTip(
        i18n("Forget finished jobs on every agent. Running work is left alone."));
    connect(m_clearBtn, &QPushButton::clicked, this, &JobsPanel::clearFinishedRequested);
    tools->addWidget(m_clearBtn);

    root->addLayout(tools);

    m_tree = new QTreeWidget(this);
    m_tree->setColumnCount(ColCount);
    m_tree->setHeaderLabels({i18n("Job"), i18n("Kind"), i18n("State"), i18n("Elapsed")});
    m_tree->setRootIsDecorated(true);
    m_tree->setUniformRowHeights(true);
    m_tree->setAllColumnsShowFocus(true);
    m_tree->header()->setStretchLastSection(false);
    m_tree->header()->setSectionResizeMode(ColJob, QHeaderView::Stretch);
    m_tree->header()->setSectionResizeMode(ColKind, QHeaderView::ResizeToContents);
    m_tree->header()->setSectionResizeMode(ColState, QHeaderView::ResizeToContents);
    m_tree->header()->setSectionResizeMode(ColElapsed, QHeaderView::ResizeToContents);
    connect(m_tree, &QTreeWidget::itemDoubleClicked, this, &JobsPanel::openSelected);
    connect(m_tree, &QTreeWidget::currentItemChanged, this, [this](QTreeWidgetItem *cur) {
        // Only job rows are actionable; an agent group row is just a header.
        const bool isJob = cur && cur->data(ColJob, kRoleIsJob).toBool();
        m_openBtn->setEnabled(isJob);
        m_gotoBtn->setEnabled(cur && !cur->data(ColJob, kRoleThread).toString().isEmpty());
    });
    root->addWidget(m_tree, 1);

    // Ticks only while something runs; rebuild() starts and stops it.
    m_tick = new QTimer(this);
    m_tick->setInterval(5000);
    connect(m_tick, &QTimer::timeout, this, &JobsPanel::tickElapsed);

    rebuild();
}

void JobsPanel::setAgentJobs(const QString &threadId, const QVector<AgentJob> &jobs)
{
    if (threadId.isEmpty()) {
        return; // an agent with no thread yet has no jobs to attribute
    }
    if (jobs.isEmpty()) {
        if (m_byThread.remove(threadId) == 0) {
            return; // nothing known, nothing to redraw
        }
    } else {
        m_byThread.insert(threadId, jobs);
    }
    rebuild();
}

void JobsPanel::setAgentTitles(const QHash<QString, QString> &titlesByThread)
{
    if (m_titles == titlesByThread) {
        return;
    }
    m_titles = titlesByThread;
    rebuild();
}

bool JobsPanel::matches(const AgentJob &job, const QString &agentTitle) const
{
    switch (m_stateFilter->currentIndex()) {
    case 1:
        if (job.done) {
            return false;
        }
        break;
    case 2:
        if (!job.done) {
            return false;
        }
        break;
    default:
        break;
    }
    const QString needle = m_textFilter->text().trimmed();
    if (needle.isEmpty()) {
        return true;
    }
    // Match the agent name too, so "worker" finds everything one agent is doing.
    return job.description.contains(needle, Qt::CaseInsensitive)
        || agentTitle.contains(needle, Qt::CaseInsensitive)
        || kindLabel(job.kind).contains(needle, Qt::CaseInsensitive);
}

void JobsPanel::rebuild()
{
    // Preserve the selected job and the set of collapsed agents across a
    // rebuild — jobs churn constantly and losing your place each time would
    // make the panel unusable while work is actually happening.
    // Job rows and agent group rows are restored by different keys: a group row
    // carries no job id, and matching on "" would hand the selection to the
    // first id-less JOB row of that agent instead of back to the header.
    const QTreeWidgetItem *sel = m_tree->currentItem();
    const QString selThread = sel ? sel->data(ColJob, kRoleThread).toString() : QString();
    const QString selJob = sel ? sel->data(ColJob, kRoleJobId).toString() : QString();
    const bool selWasJob = sel && sel->data(ColJob, kRoleIsJob).toBool();
    QSet<QString> collapsed;
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *g = m_tree->topLevelItem(i);
        if (!g->isExpanded()) {
            collapsed.insert(g->data(ColJob, kRoleThread).toString());
        }
    }

    m_tree->clear();
    int running = 0;
    int finished = 0;
    QTreeWidgetItem *toSelect = nullptr;

    // Stable ordering: agents by title, jobs newest-first within an agent, and
    // running before finished so live work is always at the top of its group.
    QStringList threads = m_byThread.keys();
    std::sort(threads.begin(), threads.end(), [this](const QString &a, const QString &b) {
        const QString ta = m_titles.value(a, a);
        const QString tb = m_titles.value(b, b);
        const int byTitle = ta.compare(tb, Qt::CaseInsensitive);
        // Agents may share a title (two "Builder"s), and the keys come out of a
        // QHash in no order at all — without the id tie-break those groups would
        // swap places on every rebuild.
        return byTitle != 0 ? byTitle < 0 : a < b;
    });

    for (const QString &thread : std::as_const(threads)) {
        const QString title = m_titles.value(thread, thread);
        QVector<AgentJob> shown;
        for (const AgentJob &job : m_byThread.value(thread)) {
            if (matches(job, title)) {
                shown.append(job);
            }
            // Counts describe everything known, not the filtered view — the
            // header is a status line, not a description of the filter.
            if (job.done) {
                ++finished;
            } else {
                ++running;
            }
        }
        if (shown.isEmpty()) {
            continue;
        }
        std::sort(shown.begin(), shown.end(), [](const AgentJob &a, const AgentJob &b) {
            if (a.done != b.done) {
                return !a.done;
            }
            if (a.startedMs != b.startedMs) {
                return a.startedMs > b.startedMs;
            }
            // Same-millisecond starts are common (a turn launching several
            // shells at once) and std::sort is not stable, so without a total
            // order those rows would swap places on every rebuild.
            return a.id < b.id;
        });

        int groupRunning = 0;
        for (const AgentJob &job : std::as_const(shown)) {
            if (!job.done) {
                ++groupRunning;
            }
        }
        auto *group = new QTreeWidgetItem(m_tree);
        group->setText(ColJob, title);
        group->setText(ColState,
                       groupRunning > 0
                           ? i18ncp("running jobs in an agent group", "%1 running", "%1 running",
                                    groupRunning)
                           : QString());
        group->setData(ColJob, kRoleThread, thread);
        group->setData(ColJob, kRoleIsJob, false);
        group->setFirstColumnSpanned(false);
        QFont bold = group->font(ColJob);
        bold.setBold(true);
        group->setFont(ColJob, bold);
        if (!selWasJob && !selThread.isEmpty() && thread == selThread) {
            toSelect = group;
        }

        for (const AgentJob &job : std::as_const(shown)) {
            auto *row = new QTreeWidgetItem(group);
            row->setText(ColJob,
                         job.description.isEmpty()
                             ? i18nc("a background job with no description", "(untitled)")
                             : job.description.simplified());
            row->setText(ColKind, kindLabel(job.kind));
            row->setText(ColState,
                         job.failed ? i18nc("job state", "Failed")
                                    : (job.done ? i18nc("job state", "Done")
                                                : i18nc("job state", "Running")));
            // A finished row shows the run's true duration; a running one its
            // age so far. A terminal job with no end stamp (never observed
            // finishing) draws nothing rather than an age that would keep
            // growing after the work stopped — a 4 s job as "2h 0m".
            row->setText(ColElapsed,
                         job.done ? (job.endedMs > 0
                                         ? elapsedText(job.startedMs, job.endedMs)
                                         : QString())
                                  : elapsedText(job.startedMs));
            row->setData(ColJob, kRoleThread, thread);
            row->setData(ColJob, kRoleJobId, job.id);
            row->setData(ColJob, kRoleKind, static_cast<int>(job.kind));
            row->setData(ColJob, kRoleOutput, job.outputFile);
            row->setData(ColJob, kRoleStarted, job.startedMs);
            row->setData(ColJob, kRoleDone, job.done);
            row->setData(ColJob, kRoleIsJob, true);
            if (!job.outputFile.isEmpty()) {
                row->setToolTip(ColJob, job.outputFile);
            }
            if (selWasJob && !selJob.isEmpty() && thread == selThread
                && job.id == selJob) {
                toSelect = row;
            }
        }
        group->setExpanded(!collapsed.contains(thread));
    }

    // Unfiltered like the job counts above it — a filtered agent count against
    // unfiltered jobs reads as "3 running · 0 agents".
    updateHeader(running, finished, m_byThread.size());
    if (toSelect) {
        m_tree->setCurrentItem(toSelect);
    }
    if (running > 0) {
        if (!m_tick->isActive()) {
            m_tick->start();
        }
    } else {
        m_tick->stop();
    }
    m_clearBtn->setEnabled(finished > 0);
}

void JobsPanel::tickElapsed()
{
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *group = m_tree->topLevelItem(i);
        for (int j = 0; j < group->childCount(); ++j) {
            QTreeWidgetItem *row = group->child(j);
            if (row->data(ColJob, kRoleDone).toBool()) {
                continue; // a finished job's age stopped moving
            }
            row->setText(ColElapsed,
                         elapsedText(row->data(ColJob, kRoleStarted).toLongLong()));
        }
    }
}

void JobsPanel::updateHeader(int running, int finished, int agents)
{
    if (running == 0 && finished == 0) {
        m_header->setText(
            i18n("No background work. Detached shells, sub-agents and Workflow runs "
                 "from every agent appear here."));
        return;
    }
    m_header->setText(i18nc("jobs panel summary: running, finished, agent count",
                            "%1 running · %2 finished · %3 agents", running, finished,
                            agents));
}

void JobsPanel::openSelected()
{
    QTreeWidgetItem *item = m_tree->currentItem();
    if (!item) {
        return;
    }
    if (!item->data(ColJob, kRoleIsJob).toBool()) {
        return; // an agent group row
    }
    const auto kind = static_cast<AgentJob::Kind>(item->data(ColJob, kRoleKind).toInt());
    const QString thread = item->data(ColJob, kRoleThread).toString();

    // Only the owning agent panel holds the workflow's launch blob, so its
    // monitor is opened there rather than rebuilt from a row.
    if (kind == AgentJob::Kind::Workflow) {
        Q_EMIT openWorkflowRequested(thread);
        return;
    }

    const QString output = item->data(ColJob, kRoleOutput).toString();
    if (output.isEmpty()) {
        Q_EMIT statusMessage(i18n("No output yet — try again in a moment"));
        return;
    }
    // Same split the in-chat tray uses: a shell's output file is plain text, a
    // sub-agent's is a stream-json transcript worth rendering as a live chat.
    if (kind == AgentJob::Kind::Shell) {
        Q_EMIT openFileRequested(thread, output);
        return;
    }
    auto *dlg = new SubAgentTranscriptDialog(output, item->text(ColJob), this);
    dlg->show();
}
