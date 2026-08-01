// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QMetaType>
#include <QString>
#include <QVector>

// AgentJob is one unit of background work owned by an agent thread: a detached
// shell, an async sub-agent, or a Workflow run. It is the shared vocabulary
// between the agent panel that observes the work (AgentPanel::handleTaskEvent,
// WorkflowMonitor) and the JobsPanel that aggregates it across every agent, so
// neither has to include the other.
//
// Deliberately a plain value: the panel emits a snapshot vector on every change
// rather than handing out pointers into its own state, so a job row can never
// outlive the agent that owns it.
namespace agentkate
{
struct AgentJob {
    // Shell = a detached `Bash run_in_background`, whose output file is plain
    // text; Subagent = an async agent whose output file is a stream-json
    // transcript; Workflow = a Workflow tool run, watched via WorkflowMonitor.
    // The distinction is what decides how "open" renders it.
    enum class Kind { Shell, Subagent, Workflow };

    QString id;          // CLI task_id, or the workflow's run id
    Kind kind = Kind::Shell;
    QString description; // human label ("Build and test", the workflow summary)
    QString outputFile;  // transcript / log path; empty until the CLI reports it
    qint64 startedMs = 0;
    // Wall-clock stamp of the transition into done/failed; 0 while the job is
    // still running (and for a terminal job whose end was never observed, e.g. a
    // record restored without one). Without it a finished row can only be drawn
    // as age-since-start, which keeps growing after the work stopped.
    qint64 endedMs = 0;
    bool done = false;
    bool failed = false;

    // Value equality over every published field, so the owning panel can decide
    // whether a snapshot is worth re-emitting by comparing it against the last
    // one. It has to cover fields no chip draws — a finished job's late output
    // path, a failure — or those changes would never reach the Jobs panel.
    friend bool operator==(const AgentJob &a, const AgentJob &b)
    {
        return a.id == b.id && a.kind == b.kind && a.description == b.description
            && a.outputFile == b.outputFile && a.startedMs == b.startedMs
            && a.endedMs == b.endedMs && a.done == b.done && a.failed == b.failed;
    }
};
} // namespace agentkate

Q_DECLARE_METATYPE(agentkate::AgentJob)
Q_DECLARE_METATYPE(QVector<agentkate::AgentJob>)
