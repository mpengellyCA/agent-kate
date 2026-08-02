#include "AiInspectorPanel.h"

#include "AgentChatHelpers.h"
#include "ipc/CoreClient.h"
#include "state/HarnessTraits.h"

#include <KLocalizedString>

#include <algorithm>

#include <QComboBox>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLocale>
#include <QStackedWidget>
#include <QTime>
#include <QTreeWidget>
#include <QVBoxLayout>

namespace {
// The merged all-threads timeline is a bounded ring: an ensemble mid-run emits
// bridge traffic indefinitely, and the oldest rows are the least interesting.
constexpr int kMaxActivityRows = 500;

// est_tokens ≈ chars/4 — the same rough ranking estimate the core's toolMeter
// uses; good enough to compare tools against each other.
QString resultSummary(const QString &text, bool isError)
{
    if (isError) {
        return i18nc("tool result status", "error");
    }
    const int chars = text.size();
    return i18nc("tool result size", "%1 chars · ~%2 tok",
                 QLocale().toString(chars), QLocale().toString(chars / 4));
}
} // namespace

AiInspectorPanel::AiInspectorPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(6, 6, 6, 6);
    layout->setSpacing(6);

    // Follow mode: this agent, or the whole arena. Kept as a compact row above
    // the totals so the panel reads the same in both modes.
    auto *followRow = new QHBoxLayout;
    followRow->setContentsMargins(0, 0, 0, 0);
    followRow->addWidget(new QLabel(i18n("Following:"), this));
    m_follow = new QComboBox(this);
    m_follow->addItem(i18n("Active thread"), false);
    m_follow->addItem(i18n("All threads"), true);
    m_follow->setToolTip(i18n(
        "“All threads” shows every agent's MCP traffic as one timeline — what a "
        "controller launches, waits on and reports, live."));
    followRow->addWidget(m_follow, 1);
    layout->addLayout(followRow);

    m_totals = new QLabel(this);
    m_totals->setWordWrap(true);
    m_totals->setTextFormat(Qt::PlainText);
    layout->addWidget(m_totals);

    m_timeline = new QTreeWidget(this);
    m_timeline->setHeaderLabels({i18n("Tool"), i18n("Detail"), i18n("Result")});
    m_timeline->setRootIsDecorated(false);
    m_timeline->setUniformRowHeights(true);
    m_timeline->setSelectionMode(QAbstractItemView::NoSelection);
    m_timeline->header()->setStretchLastSection(false);
    m_timeline->header()->setSectionResizeMode(0, QHeaderView::ResizeToContents);
    m_timeline->header()->setSectionResizeMode(1, QHeaderView::Stretch);
    m_timeline->header()->setSectionResizeMode(2, QHeaderView::ResizeToContents);

    m_activity = new QTreeWidget(this);
    m_activity->setHeaderLabels(
        {i18n("Time"), i18n("Agent"), i18n("Tool"), i18n("Detail"), i18n("Took")});
    m_activity->setRootIsDecorated(false);
    m_activity->setUniformRowHeights(true);
    m_activity->setSelectionMode(QAbstractItemView::NoSelection);
    m_activity->header()->setStretchLastSection(false);
    m_activity->header()->setSectionResizeMode(0, QHeaderView::ResizeToContents);
    m_activity->header()->setSectionResizeMode(1, QHeaderView::ResizeToContents);
    m_activity->header()->setSectionResizeMode(2, QHeaderView::ResizeToContents);
    m_activity->header()->setSectionResizeMode(3, QHeaderView::Stretch);
    m_activity->header()->setSectionResizeMode(4, QHeaderView::ResizeToContents);

    m_views = new QStackedWidget(this);
    m_views->addWidget(m_timeline);
    m_views->addWidget(m_activity);
    layout->addWidget(m_views, 1);

    updateTotals();
    connect(m_follow, &QComboBox::currentIndexChanged, this,
            &AiInspectorPanel::applyFollowMode);

    connect(m_core, &CoreClient::notification, this,
            [this](const QString &method, const QJsonObject &params) {
                // The all-threads timeline is fed by the core's cross-thread
                // feed, which is collected even while the per-thread view is
                // showing — switching modes then has history to show rather
                // than an empty pane.
                if (method == QLatin1String("mcp.activity")) {
                    appendActivity(params);
                    return;
                }
                if (method != QLatin1String("agent.event")) {
                    return;
                }
                if (params.value(QStringLiteral("threadId")).toString() != m_threadId
                    || m_threadId.isEmpty()) {
                    return;
                }
                handleEvents(params.value(QStringLiteral("events")).toArray());
            });
}

void AiInspectorPanel::applyFollowMode()
{
    const bool all = m_follow->currentData().toBool();
    m_views->setCurrentWidget(all ? m_activity : m_timeline);
    updateTotals();
}

void AiInspectorPanel::setAgentTitles(const QHash<QString, QString> &titlesByThread)
{
    if (titlesByThread == m_titles) {
        return;
    }
    m_titles = titlesByThread;
    // Re-label the rows already on screen; a worker's title usually lands after
    // its first activity row, so without this the feed keeps the bare id.
    for (int i = 0; i < m_activity->topLevelItemCount(); ++i) {
        QTreeWidgetItem *row = m_activity->topLevelItem(i);
        row->setText(1, threadLabel(row->data(0, Qt::UserRole).toString()));
    }
}

QString AiInspectorPanel::threadLabel(const QString &threadId) const
{
    if (threadId.isEmpty()) {
        return i18nc("mcp activity from no particular thread", "—");
    }
    const QString title = m_titles.value(threadId);
    // Thread ids are "t-<hex>"; the first few hex digits are enough to tell two
    // agents apart at a glance, and the full id is in the tooltip.
    const QString shortId = threadId.left(8);
    return title.isEmpty() ? shortId
                           : i18nc("agent title and short thread id", "%1 (%2)",
                                   title, shortId);
}

void AiInspectorPanel::appendActivity(const QJsonObject &params)
{
    const QString threadId = params.value(QStringLiteral("threadId")).toString();
    const QString tool = params.value(QStringLiteral("tool")).toString();
    const bool ok = params.value(QStringLiteral("ok")).toBool();
    const QString summary = ok
        ? params.value(QStringLiteral("argsSummary")).toString()
        : i18nc("failed MCP call: the summary, then the error", "%1 — %2",
                params.value(QStringLiteral("argsSummary")).toString(),
                params.value(QStringLiteral("error")).toString());
    const int ms = params.value(QStringLiteral("durationMs")).toInt();

    auto *row = new QTreeWidgetItem(
        m_activity,
        {QTime::currentTime().toString(QStringLiteral("HH:mm:ss")),
         threadLabel(threadId),
         ok ? tool : i18nc("failed tool call marker", "%1 ✗", tool), summary.simplified(),
         ms >= 1000 ? i18nc("duration in seconds", "%1s", QString::number(ms / 1000.0, 'f', 1))
                    : i18nc("duration in milliseconds", "%1ms", ms)});
    row->setData(0, Qt::UserRole, threadId);
    row->setToolTip(1, threadId);
    if (!ok) {
        row->setForeground(2, palette().color(QPalette::Disabled, QPalette::WindowText));
    }
    // Bounded ring: drop the oldest rows once the cap is passed.
    while (m_activity->topLevelItemCount() > kMaxActivityRows) {
        delete m_activity->takeTopLevelItem(0);
    }
    if (m_views->currentWidget() == m_activity) {
        m_activity->scrollToItem(row);
    }
}

void AiInspectorPanel::setActiveThread(const QString &threadId)
{
    if (threadId == m_threadId) {
        return;
    }
    m_threadId = threadId;
    // A live view: each thread starts fresh (the on-disk transcript has history).
    m_timeline->clear();
    m_rows.clear();
    m_toolNameById.clear();
    m_perTool.clear();
    m_inTok = m_outTok = m_cacheRead = m_cacheCreate = 0;
    m_costUsd = 0.0;
    m_toolCalls = 0;
    m_numTurns = 0;
    m_denials = 0;
    m_modelUsage = QJsonObject();
    m_ctxPromptTokens = 0;
    m_ctxWindow = 0;
    // Until the thread's record arrives the default engine's traits apply —
    // "unknown means billed", exactly as the AgentPanel sibling reads it.
    m_billed = HarnessRegistry::self()->traits(QString()).usageReporting;
    resolveThreadBackend(threadId);
    updateTotals();
}

void AiInspectorPanel::resolveThreadBackend(const QString &threadId)
{
    if (threadId.isEmpty()) {
        return;
    }
    m_core->call(
        QStringLiteral("session.listThreads"), QJsonObject{},
        [this, threadId](const QJsonObject &result, const QJsonObject &error) {
            if (!error.isEmpty() || threadId != m_threadId) {
                return; // failed, or the panel has moved on — keep the default
            }
            const QJsonArray threads = result.value(QStringLiteral("threads")).toArray();
            for (const QJsonValue &v : threads) {
                const QJsonObject rec = v.toObject();
                if (rec.value(QStringLiteral("threadId")).toString() == threadId) {
                    setThreadBackend(rec.value(QStringLiteral("backend")).toString());
                    return;
                }
            }
        },
        this); // context guard: a late reply must not touch a destroyed panel
}

void AiInspectorPanel::setThreadBackend(const QString &backend)
{
    const bool billed = HarnessRegistry::self()->traits(backend).usageReporting;
    if (billed == m_billed) {
        return;
    }
    m_billed = billed;
    updateTotals(); // the totals line labels spend and readout differently
}

void AiInspectorPanel::handleEvents(const QJsonArray &events)
{
    for (const QJsonValue &v : events) {
        handleEvent(v.toObject());
    }
}

void AiInspectorPanel::handleEvent(const QJsonObject &ev)
{
    const QString type = ev.value(QStringLiteral("type")).toString();
    if (type == QLatin1String("assistant")) {
        const QJsonArray content = ev.value(QStringLiteral("message"))
                                       .toObject()
                                       .value(QStringLiteral("content"))
                                       .toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            if (b.value(QStringLiteral("type")).toString() != QLatin1String("tool_use")) {
                continue;
            }
            const QString name = b.value(QStringLiteral("name")).toString();
            const QString id = b.value(QStringLiteral("id")).toString();
            const QString detail =
                agentkate::permSummary(name, b.value(QStringLiteral("input")).toObject())
                    .simplified();
            auto *item = new QTreeWidgetItem(m_timeline, {name, detail, i18n("running…")});
            if (!id.isEmpty()) {
                item->setData(0, Qt::UserRole, id);
                m_rows.insert(id, item);
                m_toolNameById.insert(id, name);
            }
            ++m_toolCalls;
            // Bounded ring, same discipline as the all-threads feed — and the
            // evicted row's pending-result lookups go with it, or they would
            // dangle on the deleted item.
            while (m_timeline->topLevelItemCount() > kMaxActivityRows) {
                QTreeWidgetItem *oldest = m_timeline->takeTopLevelItem(0);
                const QString oldId = oldest->data(0, Qt::UserRole).toString();
                if (!oldId.isEmpty()) {
                    m_rows.remove(oldId);
                    m_toolNameById.remove(oldId);
                }
                delete oldest;
            }
            m_timeline->scrollToItem(item);
        }
        updateTotals();
    } else if (type == QLatin1String("user")) {
        const QJsonArray content = ev.value(QStringLiteral("message"))
                                       .toObject()
                                       .value(QStringLiteral("content"))
                                       .toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            if (b.value(QStringLiteral("type")).toString() != QLatin1String("tool_result")) {
                continue;
            }
            const QString id = b.value(QStringLiteral("tool_use_id")).toString();
            // take, not value: a resolved call needs no further lookups, and a
            // long session must not keep one hash entry per call ever made.
            QTreeWidgetItem *item = m_rows.take(id);
            if (!item) {
                continue;
            }
            const QString text = agentkate::toolResultText(b.value(QStringLiteral("content")));
            item->setText(2, resultSummary(text, b.value(QStringLiteral("is_error")).toBool()));
            // Roll the output size into the per-tool totals — where the
            // context tokens actually go (mirrors the core's toolMeter).
            const QString name = m_toolNameById.take(id);
            if (!name.isEmpty()) {
                ToolTotals &tot = m_perTool[name];
                ++tot.calls;
                tot.chars += text.size();
            }
        }
        updateTotals();
    } else if (type == QLatin1String("result")) {
        const QJsonObject usage = ev.value(QStringLiteral("usage")).toObject();
        const qlonglong inTok =
            usage.value(QStringLiteral("input_tokens")).toVariant().toLongLong();
        const qlonglong outTok =
            usage.value(QStringLiteral("output_tokens")).toVariant().toLongLong();
        const qlonglong cacheRead =
            usage.value(QStringLiteral("cache_read_input_tokens")).toVariant().toLongLong();
        const qlonglong cacheCreate =
            usage.value(QStringLiteral("cache_creation_input_tokens")).toVariant().toLongLong();
        const double costUsd = ev.value(QStringLiteral("total_cost_usd")).toDouble();
        if (m_billed) {
            // Per-turn spend: sum into the session totals.
            m_inTok += inTok;
            m_outTok += outTok;
            m_cacheRead += cacheRead;
            m_cacheCreate += cacheCreate;
            m_costUsd += costUsd;
        } else {
            // A cumulative readout (kimi's /usage) repeats most of itself every
            // turn: the latest snapshot IS the session total, and summing it
            // grew quadratically (audit F19b/F60).
            m_inTok = inTok;
            m_outTok = outTok;
            m_cacheRead = cacheRead;
            m_cacheCreate = cacheCreate;
            m_costUsd = costUsd;
        }
        // num_turns and modelUsage are session-cumulative in each result —
        // take the latest snapshot; permission_denials arrive per turn.
        const int turns = ev.value(QStringLiteral("num_turns")).toInt();
        if (turns > 0) {
            m_numTurns = turns;
        }
        m_denials += ev.value(QStringLiteral("permission_denials")).toArray().size();
        const QJsonObject perModel = ev.value(QStringLiteral("modelUsage")).toObject();
        if (!perModel.isEmpty()) {
            m_modelUsage = perModel;
        }
        // Context fill: this turn's prompt-side tokens against the context
        // window of whichever model carries the main conversation (largest
        // prompt-side share of modelUsage).
        const qlonglong promptTotal =
            usage.value(QStringLiteral("input_tokens")).toVariant().toLongLong()
            + usage.value(QStringLiteral("cache_read_input_tokens")).toVariant().toLongLong()
            + usage.value(QStringLiteral("cache_creation_input_tokens"))
                  .toVariant()
                  .toLongLong();
        if (promptTotal > 0) {
            m_ctxPromptTokens = promptTotal;
        }
        qlonglong best = -1;
        for (auto it = perModel.constBegin(); it != perModel.constEnd(); ++it) {
            const QJsonObject u = it.value().toObject();
            const qlonglong promptSide =
                u.value(QStringLiteral("inputTokens")).toVariant().toLongLong()
                + u.value(QStringLiteral("cacheReadInputTokens")).toVariant().toLongLong()
                + u.value(QStringLiteral("cacheCreationInputTokens")).toVariant().toLongLong();
            const qlonglong window =
                u.value(QStringLiteral("contextWindow")).toVariant().toLongLong();
            if (window > 0 && promptSide > best) {
                best = promptSide;
                m_ctxWindow = window;
            }
        }
        updateTotals();
    }
}

void AiInspectorPanel::updateTotals()
{
    if (m_follow && m_follow->currentData().toBool()) {
        m_totals->setText(
            m_activity->topLevelItemCount() == 0
                ? i18n("Watching every agent's MCP traffic. Nothing yet — this fills as "
                       "agents post notes, claim files, and launch or wait on each other.")
                : i18ncp("all-threads activity summary", "%1 cross-agent call so far.",
                         "%1 cross-agent calls so far.", m_activity->topLevelItemCount()));
        return;
    }
    if (m_threadId.isEmpty()) {
        m_totals->setText(i18n("Select an agent to inspect its tool calls and token spend."));
        return;
    }
    const QLocale loc;
    // A non-billed engine's numbers are a context readout, not a spend — call
    // them what they are (mirrors the AgentPanel sibling's per-turn line).
    QString line = m_billed
        ? i18nc("inspector usage summary",
                "%1 tool calls · in %2 · out %3 · cache %4",
                loc.toString(m_toolCalls), loc.toString(m_inTok),
                loc.toString(m_outTok), loc.toString(m_cacheRead + m_cacheCreate))
        : i18nc("inspector usage summary (cumulative context readout)",
                "%1 tool calls · context %2 tokens",
                loc.toString(m_toolCalls),
                loc.toString(m_inTok + m_cacheRead + m_cacheCreate));
    if (m_costUsd > 0.0) {
        line += i18nc("inspector cost suffix", " · $%1", loc.toString(m_costUsd, 'f', 4));
    }
    if (m_numTurns > 0) {
        line += i18ncp("inspector turn count suffix", " · %1 turn", " · %1 turns",
                       m_numTurns);
    }
    if (m_denials > 0) {
        line += i18ncp("inspector denial count suffix", " · %1 denial",
                       " · %1 denials", m_denials);
    }
    if (m_ctxPromptTokens > 0 && m_ctxWindow > 0) {
        line += i18nc("inspector context-fill line: percent, window size",
                      "\ncontext %1% full (of %2 tokens)",
                      int((m_ctxPromptTokens * 100) / m_ctxWindow),
                      loc.toString(m_ctxWindow));
    }
    // Where the context went, by tool: the biggest output producers first
    // (est. tokens ≈ chars/4, mirroring the core's toolMeter).
    if (!m_perTool.isEmpty()) {
        QStringList names = m_perTool.keys();
        std::sort(names.begin(), names.end(), [this](const QString &a, const QString &b) {
            return m_perTool.value(a).chars > m_perTool.value(b).chars;
        });
        QStringList parts;
        for (int i = 0; i < names.size() && i < 4; ++i) {
            const ToolTotals &t = m_perTool.value(names.at(i));
            parts << i18nc("per-tool spend entry: name, calls, est tokens",
                           "%1 ×%2 ~%3 tok", names.at(i), t.calls,
                           loc.toString(qlonglong(t.chars / 4)));
        }
        line += i18nc("inspector per-tool spend line", "\nby tool: %1",
                      parts.join(QStringLiteral(" · ")));
    }
    // One line per model the session touched (the CLI splits usage by model —
    // subagents and background tasks can run on cheaper tiers than the main
    // loop). Values are session-cumulative snapshots straight from the CLI.
    const QStringList models = m_modelUsage.keys();
    for (const QString &model : models) {
        const QJsonObject u = m_modelUsage.value(model).toObject();
        QString entry = i18nc(
            "inspector per-model usage line: model, in tokens, out tokens",
            "%1 · in %2 · out %3", model,
            loc.toString(u.value(QStringLiteral("inputTokens")).toVariant().toLongLong()),
            loc.toString(u.value(QStringLiteral("outputTokens")).toVariant().toLongLong()));
        const double cost = u.value(QStringLiteral("costUSD")).toDouble();
        if (cost > 0.0) {
            entry += i18nc("inspector cost suffix", " · $%1", loc.toString(cost, 'f', 4));
        }
        line += QLatin1Char('\n') + entry;
    }
    m_totals->setText(line);
}
