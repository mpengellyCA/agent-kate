// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "state/AgentJob.h"

#include <QHash>
#include <QString>
#include <QWidget>

class QComboBox;
class QLabel;
class QLineEdit;
class QPushButton;
class QTimer;
class QTreeWidget;
class QTreeWidgetItem;

// JobsPanel is the one place that answers "what is running right now" across
// every agent and project: detached shells, async sub-agents and Workflow runs,
// grouped under the agent that owns them.
//
// Before this, background work was only ever visible inside the chat that
// started it — a chip tray that was per-agent, unbounded, and grew until it
// buried the composer. This panel holds the finished work the tray no longer
// draws, and is the only surface where a job belonging to a *different* agent
// can be seen at all.
//
// It owns no job state of its own: each agent panel publishes a full snapshot
// of its jobs on every change (AgentPanel::jobsChanged) and the panel replaces
// that agent's rows wholesale. So it never reasons about deltas, and a row can
// never outlive the agent that owns it — closing an agent publishes an empty
// set. Acting on a job (open its output, clear finished work) is routed back to
// the owning agent rather than done here, because that is where the truth is.
class JobsPanel : public QWidget
{
    Q_OBJECT
public:
    explicit JobsPanel(QWidget *parent = nullptr);

    // Replace everything known about one agent's background work. An empty
    // vector removes the agent from the view (its panel closed, or it never had
    // any work), which is how rows are reaped.
    void setAgentJobs(const QString &threadId, const QVector<agentkate::AgentJob> &jobs);
    // Human agent titles keyed by thread id, so rows are named by agent rather
    // than by an opaque id. Fed by AgentDock::agentTitlesChanged.
    void setAgentTitles(const QHash<QString, QString> &titlesByThread);

Q_SIGNALS:
    // A shell's output file is plain text — the shell asks the window to open it
    // in the editor. (A sub-agent's is a stream-json transcript, which this
    // panel renders itself in a live dialog.) The owning thread rides along
    // because this panel shows other agents' work: without it the window would
    // open the log into whichever agent's editor group happens to be on screen,
    // which is not the one the log belongs to (plan 17 scopes tabs per agent).
    void openFileRequested(const QString &threadId, const QString &path);
    void statusMessage(const QString &text);
    // A Workflow row: only the owning agent panel holds the run's launch blob,
    // so opening its monitor is routed back there rather than reconstructed.
    void openWorkflowRequested(const QString &threadId);
    // Select the owning agent in the roster.
    void goToAgentRequested(const QString &threadId);
    // Ask every agent to forget its finished jobs. Routed rather than done
    // locally: this panel mirrors the agents' state, so clearing here alone
    // would be undone by the next snapshot they publish.
    void clearFinishedRequested();

private:
    void rebuild();
    // Refresh only the elapsed column of running rows. Rebuilding on a tick
    // would collapse the user's expansion and selection every few seconds.
    void tickElapsed();
    void openSelected();
    void updateHeader(int running, int finished, int agents);
    // True when a job passes the state filter and the text filter.
    bool matches(const agentkate::AgentJob &job, const QString &agentTitle) const;

    // Jobs by owning thread id, and the titles to name them with.
    QHash<QString, QVector<agentkate::AgentJob>> m_byThread;
    QHash<QString, QString> m_titles;

    QLabel *m_header = nullptr;
    QComboBox *m_stateFilter = nullptr;
    QLineEdit *m_textFilter = nullptr;
    QPushButton *m_clearBtn = nullptr;
    QPushButton *m_openBtn = nullptr;
    QPushButton *m_gotoBtn = nullptr;
    QTreeWidget *m_tree = nullptr;
    QTimer *m_tick = nullptr;
};
