// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorkflowMonitorView.h"

#include "SubAgentTranscriptDialog.h"
#include "WorkflowMonitor.h"
#include "theme/ThemeManager.h"

#include <KLocalizedString>

#include <QFontDatabase>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QLabel>
#include <QListWidget>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QScrollBar>
#include <QSplitter>
#include <QTreeWidget>
#include <QVBoxLayout>

namespace {

// Item data roles on the tree's sub-agent rows (and group key on phase rows).
constexpr int kAgentIdRole = Qt::UserRole;
constexpr int kPathRole = Qt::UserRole + 1;
constexpr int kDetailRole = Qt::UserRole + 2;
constexpr int kGroupKeyRole = Qt::UserRole + 3;

// Set a column's text only when it changed, so an unchanged cell isn't marked
// dirty (which would repaint/flicker it on every refresh tick).
void setTextIfChanged(QTreeWidgetItem *item, int col, const QString &text)
{
    if (item->text(col) != text) {
        item->setText(col, text);
    }
}

// Human-friendly token count: 1234 -> "1.2k", 1_000_000 -> "1.0M".
QString fmtTokens(qint64 n)
{
    if (n <= 0) {
        return QString();
    }
    if (n < 1000) {
        return QString::number(n);
    }
    if (n < 1000000) {
        return QStringLiteral("%1k").arg(n / 1000.0, 0, 'f', 1);
    }
    return QStringLiteral("%1M").arg(n / 1000000.0, 0, 'f', 1);
}

// Compact H:MM:SS / M:SS from a millisecond duration.
QString fmtDuration(qint64 ms)
{
    if (ms <= 0) {
        return QString();
    }
    const qint64 total = ms / 1000;
    const qint64 h = total / 3600;
    const qint64 m = (total % 3600) / 60;
    const qint64 s = total % 60;
    if (h > 0) {
        return QStringLiteral("%1:%2:%3").arg(h).arg(m, 2, 10, QLatin1Char('0'))
            .arg(s, 2, 10, QLatin1Char('0'));
    }
    return QStringLiteral("%1:%2").arg(m).arg(s, 2, 10, QLatin1Char('0'));
}

// State word + colour for a sub-agent's `state` string.
QColor stateColor(const QString &state, const AkColors &c)
{
    if (state == QLatin1String("done")) {
        return c.positive;
    }
    if (state == QLatin1String("error") || state == QLatin1String("failed")) {
        return c.negative;
    }
    if (state == QLatin1String("running")) {
        return c.info;
    }
    return c.neutral; // queued / unknown
}

} // namespace

WorkflowMonitorView::WorkflowMonitorView(const QString &inputJson,
                                         const QString &resultText, QWidget *parent)
    : QWidget(parent)
{
    m_monitor = new WorkflowMonitor(inputJson, resultText, this);

    auto *root = new QVBoxLayout(this);
    root->setContentsMargins(8, 8, 8, 8);
    root->setSpacing(6);

    m_header = new QLabel(this);
    m_header->setTextFormat(Qt::RichText);
    m_header->setWordWrap(true);
    m_header->setTextInteractionFlags(Qt::TextSelectableByMouse);
    root->addWidget(m_header);

    m_summary = new QLabel(this);
    m_summary->setWordWrap(true);
    m_summary->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_summary->setVisible(false);
    root->addWidget(m_summary);

    if (!m_monitor->isValid()) {
        // Not a followable launch (e.g. an inline/synchronous workflow, or the
        // result blob lacked a transcript dir). Say so plainly and stop.
        m_header->setText(
            i18n("This workflow result doesn't reference a background run to "
                 "follow. Live progress is only available for workflows launched "
                 "in the background."));
        return;
    }

    auto *split = new QSplitter(Qt::Vertical, this);

    m_tree = new QTreeWidget(split);
    m_tree->setColumnCount(6);
    m_tree->setHeaderLabels({i18n("Sub-agent"), i18n("Model"), i18n("State"),
                             i18n("Tokens"), i18n("Tools"), i18n("Activity")});
    m_tree->setRootIsDecorated(true);
    m_tree->setAlternatingRowColors(true);
    m_tree->setUniformRowHeights(true);
    m_tree->header()->setStretchLastSection(true);
    m_tree->header()->setSectionResizeMode(0, QHeaderView::Interactive);
    m_tree->setColumnWidth(0, 200);
    connect(m_tree, &QTreeWidget::itemSelectionChanged, this,
            &WorkflowMonitorView::syncDetail);
    split->addWidget(m_tree);

    auto *detailHost = new QWidget(split);
    auto *detailLay = new QVBoxLayout(detailHost);
    detailLay->setContentsMargins(0, 0, 0, 0);
    detailLay->setSpacing(4);
    auto *detailBar = new QHBoxLayout;
    detailBar->setContentsMargins(0, 0, 0, 0);
    auto *detailCaption = new QLabel(i18n("Selected sub-agent"), detailHost);
    QFont capFont = detailCaption->font();
    capFont.setBold(true);
    detailCaption->setFont(capFont);
    detailBar->addWidget(detailCaption);
    detailBar->addStretch(1);
    m_openBtn = new QPushButton(i18n("Open transcript"), detailHost);
    m_openBtn->setEnabled(false);
    m_openBtn->setCursor(Qt::PointingHandCursor);
    connect(m_openBtn, &QPushButton::clicked, this,
            &WorkflowMonitorView::openSelectedTranscript);
    detailBar->addWidget(m_openBtn);
    detailLay->addLayout(detailBar);

    m_detail = new QPlainTextEdit(detailHost);
    m_detail->setReadOnly(true);
    m_detail->setFont(QFontDatabase::systemFont(QFontDatabase::FixedFont));
    m_detail->setPlaceholderText(i18n("Select a sub-agent to see its prompt, "
                                      "current activity and result."));
    detailLay->addWidget(m_detail, 1);
    split->addWidget(detailHost);

    split->setStretchFactor(0, 3);
    split->setStretchFactor(1, 2);
    root->addWidget(split, 1);

    m_logsCaption = new QLabel(i18n("Log"), this);
    QFont logFont = m_logsCaption->font();
    logFont.setBold(true);
    m_logsCaption->setFont(logFont);
    m_logsCaption->setVisible(false);
    root->addWidget(m_logsCaption);
    m_logs = new QListWidget(this);
    m_logs->setMaximumHeight(96);
    m_logs->setVisible(false);
    root->addWidget(m_logs);

    connect(m_monitor, &WorkflowMonitor::changed, this,
            &WorkflowMonitorView::rebuild);
    connect(ThemeManager::instance(), &ThemeManager::changed, this,
            &WorkflowMonitorView::rebuild);
    rebuild();
}

bool WorkflowMonitorView::isValid() const
{
    return m_monitor && m_monitor->isValid();
}

void WorkflowMonitorView::rebuild()
{
    if (!m_tree) {
        return;
    }
    const WorkflowMonitor::Snapshot &s = m_monitor->snapshot();
    const AkColors &c = ThemeManager::palette();

    // --- header badge + counts --------------------------------------------
    QString badge;
    QColor badgeColor;
    switch (s.state) {
    case WorkflowMonitor::State::Completed:
        badge = i18n("Completed");
        badgeColor = c.positive;
        break;
    case WorkflowMonitor::State::Failed:
        badge = i18n("Failed");
        badgeColor = c.negative;
        break;
    case WorkflowMonitor::State::Running:
        badge = i18n("Running");
        badgeColor = c.info;
        break;
    default:
        badge = i18n("Starting…");
        badgeColor = c.neutral;
        break;
    }
    QStringList facts;
    facts << i18n("%1 sub-agents", s.agentCount);
    if (s.totalToolCalls > 0) {
        facts << i18n("%1 tool calls", s.totalToolCalls);
    }
    if (s.totalTokens > 0) {
        facts << i18n("%1 tokens", fmtTokens(s.totalTokens));
    }
    if (s.durationMs > 0) {
        facts << fmtDuration(s.durationMs);
    }
    QString header = QStringLiteral(
                         "<b style=\"color:%1\">%2</b>&nbsp;&nbsp;<span "
                         "style=\"color:%3\">%4</span>")
                         .arg(badgeColor.name(), badge.toHtmlEscaped(),
                              c.info.name(), facts.join(QStringLiteral(" · ")).toHtmlEscaped());
    if (!s.runId.isEmpty()) {
        header += QStringLiteral("<br><span style=\"color:%1\">%2</span>")
                      .arg(c.info.name(), s.runId.toHtmlEscaped());
    }
    if (!s.planPhases.isEmpty() && s.state == WorkflowMonitor::State::Running) {
        header += QStringLiteral("<br><span style=\"color:%1\">%2</span>")
                      .arg(c.info.name(),
                           i18n("Plan: %1", s.planPhases.join(QStringLiteral(" → ")))
                               .toHtmlEscaped());
    }
    m_header->setText(header);

    m_summary->setVisible(!s.summary.isEmpty());
    if (!s.summary.isEmpty()) {
        m_summary->setText(s.summary);
    }

    reconcileTree();
    syncDetail();

    // --- logs -------------------------------------------------------------
    const bool hasLogs = !s.logs.isEmpty();
    m_logsCaption->setVisible(hasLogs);
    m_logs->setVisible(hasLogs);
    if (hasLogs) {
        // Rebuild only when the set changed (cheap; logs are few).
        if (m_logs->count() != s.logs.size()) {
            m_logs->clear();
            m_logs->addItems(s.logs);
        }
    }
}

void WorkflowMonitorView::reconcileTree()
{
    const WorkflowMonitor::Snapshot &s = m_monitor->snapshot();
    const AkColors &c = ThemeManager::palette();

    // Remember the selected agent so a row that moves group (e.g. Running →
    // Completed, which recreates it) keeps its selection.
    const QString keepId = m_tree->currentItem()
                               ? m_tree->currentItem()->data(0, kAgentIdRole).toString()
                               : QString();

    // Index current phase groups by their stable key (phase title) so we can
    // update them in place instead of clearing the tree (which flickers).
    QHash<QString, QTreeWidgetItem *> haveGroups;
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *g = m_tree->topLevelItem(i);
        haveGroups.insert(g->data(0, kGroupKeyRole).toString(), g);
    }

    for (int idx = 0; idx < s.phases.size(); ++idx) {
        const WorkflowMonitor::Phase &phase = s.phases[idx];
        QTreeWidgetItem *group = haveGroups.take(phase.title);
        if (!group) {
            group = new QTreeWidgetItem;
            group->setFlags(Qt::ItemIsEnabled);
            group->setData(0, kGroupKeyRole, phase.title);
            QFont gf = group->font(0);
            gf.setBold(true);
            group->setFont(0, gf);
            m_tree->insertTopLevelItem(idx, group);
            group->setFirstColumnSpanned(true); // applied once the item is in the tree
            group->setExpanded(true);
        } else if (m_tree->indexOfTopLevelItem(group) != idx) {
            // Move the existing group to its target position.
            m_tree->takeTopLevelItem(m_tree->indexOfTopLevelItem(group));
            m_tree->insertTopLevelItem(idx, group);
            group->setExpanded(true);
        }

        QString title = phase.title;
        if (!phase.detail.isEmpty()) {
            title += QStringLiteral(" — ") + phase.detail;
        }
        setTextIfChanged(group, 0,
                         QStringLiteral("%1  (%2)").arg(title).arg(phase.agents.size()));

        // Reconcile this group's sub-agent rows by agent id.
        QHash<QString, QTreeWidgetItem *> haveAgents;
        for (int i = 0; i < group->childCount(); ++i) {
            QTreeWidgetItem *ch = group->child(i);
            haveAgents.insert(ch->data(0, kAgentIdRole).toString(), ch);
        }
        for (int ai = 0; ai < phase.agents.size(); ++ai) {
            const WorkflowMonitor::SubAgent &a = phase.agents[ai];
            QTreeWidgetItem *item = haveAgents.take(a.agentId);
            if (!item) {
                item = new QTreeWidgetItem;
                item->setData(0, kAgentIdRole, a.agentId);
                group->insertChild(ai, item);
            } else if (group->indexOfChild(item) != ai) {
                group->takeChild(group->indexOfChild(item));
                group->insertChild(ai, item);
            }

            setTextIfChanged(item, 0, a.label.isEmpty() ? a.agentId.left(8) : a.label);
            setTextIfChanged(item, 1, a.model);
            if (item->text(2) != a.state) {
                item->setText(2, a.state);
                item->setForeground(2, stateColor(a.state, c));
            }
            setTextIfChanged(item, 3, fmtTokens(a.tokens));
            setTextIfChanged(item, 4,
                             a.toolCalls > 0 ? QString::number(a.toolCalls) : QString());
            if (item->text(5) != a.lastActivity) {
                item->setText(5, a.lastActivity);
                item->setToolTip(5, a.lastActivity);
            }
            item->setData(0, kPathRole, a.jsonlPath);

            // Compose the detail-pane text and stash it on the row.
            QStringList detail;
            if (!a.label.isEmpty()) {
                detail << QStringLiteral("# %1").arg(a.label);
            }
            if (!a.model.isEmpty()) {
                detail << i18n("Model: %1", a.model);
            }
            detail << i18n("State: %1", a.state);
            if (!a.lastActivity.isEmpty()) {
                detail << QString() << i18n("Latest activity:") << a.lastActivity;
            }
            if (!a.promptPreview.isEmpty()) {
                detail << QString() << i18n("Prompt:") << a.promptPreview;
            }
            if (!a.resultPreview.isEmpty()) {
                detail << QString() << i18n("Result:") << a.resultPreview;
            }
            item->setData(0, kDetailRole, detail.join(QLatin1Char('\n')));
        }
        // Drop rows for agents no longer in this group (e.g. moved to Completed).
        for (QTreeWidgetItem *stale : haveAgents) {
            delete group->takeChild(group->indexOfChild(stale));
        }
    }

    // Drop phase groups no longer present.
    for (QTreeWidgetItem *stale : haveGroups) {
        delete m_tree->takeTopLevelItem(m_tree->indexOfTopLevelItem(stale));
    }

    // Restore selection if the selected agent survived but was recreated in a
    // different group (deleting its old row cleared the current item).
    if (!keepId.isEmpty() && !m_tree->currentItem()) {
        for (int i = 0; i < m_tree->topLevelItemCount() && !m_tree->currentItem(); ++i) {
            QTreeWidgetItem *g = m_tree->topLevelItem(i);
            for (int j = 0; j < g->childCount(); ++j) {
                if (g->child(j)->data(0, kAgentIdRole).toString() == keepId) {
                    m_tree->setCurrentItem(g->child(j));
                    break;
                }
            }
        }
    }
}

void WorkflowMonitorView::syncDetail()
{
    QTreeWidgetItem *item = m_tree ? m_tree->currentItem() : nullptr;
    if (!item || item->data(0, kAgentIdRole).toString().isEmpty()) {
        m_selectedPath.clear();
        m_selectedLabel.clear();
        m_openBtn->setEnabled(false);
        if (!m_detail->toPlainText().isEmpty()) {
            m_detail->clear();
        }
        return;
    }
    m_selectedPath = item->data(0, kPathRole).toString();
    m_selectedLabel = item->text(0);
    m_openBtn->setEnabled(!m_selectedPath.isEmpty());
    // Only rewrite the pane when the text actually changed, keeping the reader's
    // scroll position while the sub-agent's activity updates live.
    const QString detail = item->data(0, kDetailRole).toString();
    if (m_detail->toPlainText() != detail) {
        const int scroll = m_detail->verticalScrollBar()->value();
        m_detail->setPlainText(detail);
        m_detail->verticalScrollBar()->setValue(scroll);
    }
}

void WorkflowMonitorView::openSelectedTranscript()
{
    if (m_selectedPath.isEmpty()) {
        return;
    }
    auto *dlg = new SubAgentTranscriptDialog(m_selectedPath, m_selectedLabel, this);
    dlg->show();
}
