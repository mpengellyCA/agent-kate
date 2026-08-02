// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QByteArray>
#include <QDateTime>
#include <QElapsedTimer>
#include <QHash>
#include <QObject>
#include <QString>
#include <QStringList>
#include <QVector>

class QFileSystemWatcher;
class QTimer;
class QJsonObject;

namespace agentkate
{
struct TailRead;
}

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

    // Incremental journal follow state: a byte offset into journal.jsonl plus
    // the parse state accumulated from it. Each poll reads ONLY the bytes
    // appended since the previous one (bounded, via readBoundedTail), so the
    // cost of a refresh scales with new activity, not with run history. A file
    // that shrank (truncated/rotated) invalidates everything derived from it,
    // so the whole state resets. Exposed for the regression test.
    struct JournalState {
        qint64 offset = 0;         // next byte to read
        QByteArray remainder;      // trailing partial line, carried to the next poll
        QVector<QString> order;    // agent ids, first-seen order
        QHash<QString, bool> done; // agentId -> a "result" entry was seen
    };

    // One bounded step: read the bytes appended to `path` since `st.offset` and
    // fold them into `st`. Returns true when the file shrank and `st` was
    // rebuilt from the new content. Exposed for the regression test.
    static bool pollJournal(JournalState &st, const QString &path);

    // Re-scan disk now and, if the snapshot changed, emit changed(). Also called
    // by the watcher/poll internally; safe to call directly (e.g. on first show).
    void refresh();

Q_SIGNALS:
    void changed();

private:
    void parseAnchors(const QString &resultText);
    void parseScriptPhases(const QString &inputJson);
    void startWatching();

    static void applyJournalChunk(JournalState &st, const agentkate::TailRead &chunk);

    Snapshot buildFromFinalJson(const QJsonObject &root) const;
    Snapshot buildFromLive();

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

    JournalState m_journal;

    // Per-agent transcript tail cache: tailActivity re-reads an agent's file
    // only when its size or mtime moved since the last poll.
    struct AgentTail {
        qint64 size = -1;
        QDateTime mtime;
        QString lastActivity;
        QString preview;
    };
    QHash<QString, AgentTail> m_agentTails;

    // The transcript dir existed at some point — its later disappearance (with
    // no final json) means the run died, not that it hasn't started yet.
    bool m_sawTranscriptDir = false;
    QElapsedTimer m_sinceChange; // since the snapshot last changed (poll backoff)

    QFileSystemWatcher *m_watcher = nullptr;
    QTimer *m_poll = nullptr;
    QTimer *m_debounce = nullptr;
};
