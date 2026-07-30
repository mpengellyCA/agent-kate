#pragma once

#include <QHash>
#include <QJsonObject>
#include <QString>
#include <QWidget>

class CoreClient;
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
class AiInspectorPanel : public QWidget
{
    Q_OBJECT
public:
    explicit AiInspectorPanel(CoreClient *core, QWidget *parent = nullptr);

    // Point the inspector at a thread; clears the view when the thread changes.
    void setActiveThread(const QString &threadId);

private:
    void handleEvents(const QJsonArray &events);
    void handleEvent(const QJsonObject &ev);
    void updateTotals();

    CoreClient *m_core = nullptr;
    QString m_threadId;
    QLabel *m_totals = nullptr;
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
