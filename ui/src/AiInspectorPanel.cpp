#include "AiInspectorPanel.h"

#include "AgentChatHelpers.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QHeaderView>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLocale>
#include <QTreeWidget>
#include <QVBoxLayout>

namespace {
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
    layout->addWidget(m_timeline, 1);

    updateTotals();

    connect(m_core, &CoreClient::notification, this,
            [this](const QString &method, const QJsonObject &params) {
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

void AiInspectorPanel::setActiveThread(const QString &threadId)
{
    if (threadId == m_threadId) {
        return;
    }
    m_threadId = threadId;
    // A live view: each thread starts fresh (the on-disk transcript has history).
    m_timeline->clear();
    m_rows.clear();
    m_inTok = m_outTok = m_cacheRead = m_cacheCreate = 0;
    m_costUsd = 0.0;
    m_toolCalls = 0;
    m_numTurns = 0;
    m_denials = 0;
    m_modelUsage = QJsonObject();
    updateTotals();
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
                m_rows.insert(id, item);
            }
            ++m_toolCalls;
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
            QTreeWidgetItem *item = m_rows.value(id, nullptr);
            if (!item) {
                continue;
            }
            const QString text = agentkate::toolResultText(b.value(QStringLiteral("content")));
            item->setText(2, resultSummary(text, b.value(QStringLiteral("is_error")).toBool()));
        }
    } else if (type == QLatin1String("result")) {
        const QJsonObject usage = ev.value(QStringLiteral("usage")).toObject();
        m_inTok += usage.value(QStringLiteral("input_tokens")).toVariant().toLongLong();
        m_outTok += usage.value(QStringLiteral("output_tokens")).toVariant().toLongLong();
        m_cacheRead +=
            usage.value(QStringLiteral("cache_read_input_tokens")).toVariant().toLongLong();
        m_cacheCreate +=
            usage.value(QStringLiteral("cache_creation_input_tokens")).toVariant().toLongLong();
        m_costUsd += ev.value(QStringLiteral("total_cost_usd")).toDouble();
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
        updateTotals();
    }
}

void AiInspectorPanel::updateTotals()
{
    if (m_threadId.isEmpty()) {
        m_totals->setText(i18n("Select an agent to inspect its tool calls and token spend."));
        return;
    }
    const QLocale loc;
    QString line = i18nc("inspector usage summary",
                         "%1 tool calls · in %2 · out %3 · cache %4",
                         loc.toString(m_toolCalls), loc.toString(m_inTok),
                         loc.toString(m_outTok), loc.toString(m_cacheRead + m_cacheCreate));
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
