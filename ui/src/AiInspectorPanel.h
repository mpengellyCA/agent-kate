#pragma once

#include <QHash>
#include <QJsonObject>
#include <QString>
#include <QWidget>

class CoreClient;
class QComboBox;
class QStackedWidget;
class QTreeWidget;
class QTreeWidgetItem;
class QLabel;
class QJsonArray;

// AiInspectorPanel shows a live tool-call timeline and token/cost spend for the
// active agent. It consumes the same agent.event stream the transcript does,
// filtered to the active thread, pairing each tool_use with its tool_result and
// accumulating per-turn usage from "result" events. It is a LIVE view: switching
// agents resets it and it fills as new events arrive (the on-disk transcript
// remains the full history).
//
// A follow-mode switch turns it into the arena-wide view: "All threads" renders
// the core's `mcp.activity` feed as one merged orchestration timeline (time,
// source agent, tool, summary, duration, ok) — the surface that makes a
// controller launching and waiting on workers watchable in real time. That view
// is a bounded ring of its own (kMaxActivityRows), deliberately NOT the
// append-only transcript model: cross-thread traffic from a running ensemble is
// unbounded, and this panel must never become the thing that grows forever.
class AiInspectorPanel : public QWidget
{
    Q_OBJECT
public:
    explicit AiInspectorPanel(CoreClient *core, QWidget *parent = nullptr);

    // Point the inspector at a thread; clears the view when the thread changes.
    void setActiveThread(const QString &threadId);

    // Declare the engine behind the active thread. It decides how result-event
    // usage is read: per-turn spend (summed) or a cumulative context readout
    // (latest snapshot — kimi's /usage repeats most of itself every turn, and
    // summing it grew quadratically; audit F19b/F60). setActiveThread resolves
    // it from the core's session.listThreads; this is the seam that lands the
    // answer, public for callers (and tests) that already know the engine.
    void setThreadBackend(const QString &backend);

    // Human titles per thread id (the roster's), so the all-threads timeline
    // can name the agent behind each row instead of showing a bare id.
    void setAgentTitles(const QHash<QString, QString> &titlesByThread);

private:
    void handleEvents(const QJsonArray &events);
    void handleEvent(const QJsonObject &ev);
    void updateTotals();
    // Ask the core which engine runs threadId; stale replies (the panel moved
    // on) are ignored.
    void resolveThreadBackend(const QString &threadId);
    // Append one mcp.activity notification to the all-threads timeline.
    void appendActivity(const QJsonObject &params);
    // Short, human label for a thread in the all-threads view.
    QString threadLabel(const QString &threadId) const;
    void applyFollowMode();

    CoreClient *m_core = nullptr;
    QString m_threadId;
    QLabel *m_totals = nullptr;
    QComboBox *m_follow = nullptr;      // Active thread | All threads
    QStackedWidget *m_views = nullptr;  // the per-thread timeline, or the merged one
    QTreeWidget *m_activity = nullptr;  // merged mcp.activity feed (all threads)
    QHash<QString, QString> m_titles;   // threadId -> roster title
    QTreeWidget *m_timeline = nullptr;
    QHash<QString, QTreeWidgetItem *> m_rows; // tool_use id -> timeline row
    QHash<QString, QString> m_toolNameById;   // tool_use id -> tool name
    // Per-tool output totals — where the context tokens actually go (the UI
    // counterpart of the core's toolMeter, fed by the same events).
    struct ToolTotals {
        int calls = 0;
        qlonglong chars = 0;
    };
    QHash<QString, ToolTotals> m_perTool;

    // Whether result-event usage is a TURN's spend (summed into the totals) or
    // a cumulative readout (latest snapshot wins). Same gate as AgentPanel's
    // `billed`: the registry answers an unknown engine with claude-shaped
    // defaults, so only a harness that positively declares no usage reporting
    // is excluded (audit F19b/F60).
    bool m_billed = true;
    // Accumulated usage for the active thread (reset on thread switch).
    qlonglong m_inTok = 0;
    qlonglong m_outTok = 0;
    qlonglong m_cacheRead = 0;
    qlonglong m_cacheCreate = 0;
    double m_costUsd = 0.0;
    int m_toolCalls = 0;
    // Extras from the result event: the CLI reports num_turns and modelUsage
    // as session-cumulative snapshots (latest wins); denials accumulate.
    int m_numTurns = 0;
    int m_denials = 0;
    QJsonObject m_modelUsage; // model id -> {inputTokens, outputTokens, costUSD, …}
    // Context fill: latest turn's prompt-side tokens vs the main model's
    // context window — the number that predicts auto-compaction.
    qlonglong m_ctxPromptTokens = 0;
    qlonglong m_ctxWindow = 0;
};
