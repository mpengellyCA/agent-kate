// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QObject>
#include <QString>
#include <QStringList>
#include <QVector>

class QFileSystemWatcher;
class QTimer;
class QJsonObject;

// WorkflowMonitor watches a background Workflow (a `Workflow` tool launched by an
// agent) and reports its live status by reading the run's on-disk artifacts.
//
// AgentKate is not a Claude Code session, so `/workflows` is unreachable — the
// only visibility into a running workflow is the files it writes under the
// project's transcript tree. Two shapes exist:
//
//   * While running — `<P>/subagents/workflows/<runId>/`:
//       - journal.jsonl: append-only {type:"started"|"result",agentId,…} per
//         sub-agent (coarse running/done counts).
//       - agent-<id>.jsonl: each sub-agent's full stream-json transcript, tailed
//         for its current activity (latest tool_use / text).
//   * On completion — `<P>/workflows/<runId>.json`: the rich final snapshot with
//     phases, per-agent labels/models/tokens/tool-calls/results, totals, logs.
//
// The monitor parses the `Workflow` tool's launch result for the run anchors,
// re-scans on a QFileSystemWatcher (plus a low-frequency poll fallback, since
// watchers miss rapid appends), and emits changed() only when the derived
// snapshot actually differs — so the views repaint without flicker.
class WorkflowMonitor : public QObject
{
    Q_OBJECT
public:
    enum class State { Unknown, Running, Completed, Failed };

    // One sub-agent (a workflow's agent() call). Fields the live path can't know
    // (label/model/token totals) stay empty/zero until the final snapshot lands.
    struct SubAgent {
        QString agentId;
        QString label;        // human label (final json); short id fallback live
        QString model;
        QString state;        // "running" | "done" | "queued" | "error"
        qint64 tokens = 0;
        int toolCalls = 0;
        QString lastActivity; // one-line "what it's doing" (tailed / lastToolSummary)
        QString promptPreview;
        QString resultPreview;
        QString jsonlPath;    // agent-<id>.jsonl (opened via the Open transcript action)
    };

    // A phase group. While running with no on-disk phase→agent mapping, groups are
    // synthetic state buckets ("Running"/"Completed"); once complete they mirror
    // the script's declared phases.
    struct Phase {
        QString title;
        QString detail;
        QVector<SubAgent> agents;
    };

    struct Snapshot {
        State state = State::Unknown;
        QString runId;
        QString taskId;
        QString scriptFile;
        QString transcriptDir;
        QString summary;
        int agentCount = 0;
        qint64 totalTokens = 0;
        int totalToolCalls = 0;
        qint64 durationMs = 0;       // 0 until known (completed)
        QStringList planPhases;      // the script's declared phase titles (running view)
        QVector<Phase> phases;
        QStringList logs;            // narrator log() lines
    };

    // `inputJson` is the pretty-printed Workflow tool input (carries the script,
    // whose meta.phases we read); `resultText` is the launch result blob (carries
    // Task ID / Transcript dir / Run ID). Either may be empty.
    WorkflowMonitor(const QString &inputJson, const QString &resultText,
                    QObject *parent = nullptr);
    ~WorkflowMonitor() override;

    // True once a transcript dir was parsed from the launch result — i.e. this
    // really is a Workflow launch we can follow.
    bool isValid() const { return !m_transcriptDir.isEmpty(); }

    const Snapshot &snapshot() const { return m_snapshot; }
    QString runId() const { return m_runId; }

    // Re-scan disk now and, if the snapshot changed, emit changed(). Also called
    // by the watcher/poll internally; safe to call directly (e.g. on first show).
    void refresh();

Q_SIGNALS:
    void changed();

private:
    void parseAnchors(const QString &resultText);
    void parseScriptPhases(const QString &inputJson);
    void startWatching();

    Snapshot buildFromFinalJson(const QJsonObject &root) const;
    Snapshot buildFromLive() const;

    // Tail the last ~64 KB of a sub-agent transcript for its latest activity
    // (returns a one-line summary; fills `preview` with the latest assistant text).
    static QString tailActivity(const QString &jsonlPath, QString &preview);

    static QString fingerprint(const Snapshot &s);

    QString m_runId;
    QString m_taskId;
    QString m_scriptFile;
    QString m_transcriptDir;   // …/subagents/workflows/<runId>
    QString m_finalJsonPath;   // …/workflows/<runId>.json (exists only when done)
    QVector<QPair<QString, QString>> m_scriptPhases; // (title, detail) from meta.phases

    Snapshot m_snapshot;
    QString m_fingerprint;

    QFileSystemWatcher *m_watcher = nullptr;
    QTimer *m_poll = nullptr;
    QTimer *m_debounce = nullptr;
};
