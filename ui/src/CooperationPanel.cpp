#include "CooperationPanel.h"

#include "ipc/CoreClient.h"

#include <KLocalizedString>

#include <QDateTime>
#include <QGroupBox>
#include <QHeaderView>
#include <QJsonArray>
#include <QLabel>
#include <QScrollArea>
#include <QTime>
#include <QTimer>
#include <QTreeWidget>
#include <QVBoxLayout>

namespace {
// The Recent activity strip answers "what just happened", so it stays short;
// the AI Inspector's all-threads mode is the full timeline.
constexpr int kMaxActivityRows = 25;

// A compact section: a titled group box wrapping a flat, header-styled tree.
QTreeWidget *makeSection(const QString &title, const QStringList &headers, QWidget *parent,
                         QVBoxLayout *into)
{
    auto *box = new QGroupBox(title, parent);
    auto *boxLayout = new QVBoxLayout(box);
    boxLayout->setContentsMargins(4, 4, 4, 4);
    auto *tree = new QTreeWidget(box);
    tree->setHeaderLabels(headers);
    tree->setRootIsDecorated(false);
    tree->setUniformRowHeights(true);
    tree->setSelectionMode(QAbstractItemView::NoSelection);
    tree->setFocusPolicy(Qt::NoFocus);
    tree->header()->setStretchLastSection(true);
    tree->setSizeAdjustPolicy(QAbstractScrollArea::AdjustToContents);
    boxLayout->addWidget(tree);
    into->addWidget(box);
    return tree;
}

QString shortTime(const QString &iso)
{
    const QDateTime t = QDateTime::fromString(iso, Qt::ISODate);
    if (!t.isValid()) {
        return {};
    }
    return t.toLocalTime().toString(QStringLiteral("HH:mm"));
}

// "human" reads better than a bare owner key; agent thread ids pass through.
QString ownerLabel(const QString &owner)
{
    return owner == QLatin1String("human") ? i18n("You") : owner;
}
} // namespace

CooperationPanel::CooperationPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    auto *outer = new QVBoxLayout(this);
    outer->setContentsMargins(0, 0, 0, 0);

    auto *scroll = new QScrollArea(this);
    scroll->setWidgetResizable(true);
    scroll->setFrameShape(QFrame::NoFrame);
    outer->addWidget(scroll);

    auto *content = new QWidget(scroll);
    auto *layout = new QVBoxLayout(content);
    layout->setContentsMargins(6, 6, 6, 6);
    layout->setSpacing(8);

    m_summary = new QLabel(i18n("Waiting for the core…"), content);
    m_summary->setWordWrap(true);
    m_summary->setTextFormat(Qt::PlainText);
    layout->addWidget(m_summary);

    m_presence = makeSection(i18n("Presence"), {i18n("Who"), i18n("Focused on")}, content, layout);
    m_files = makeSection(i18n("Open files"), {i18n("File"), i18n("Open by")}, content, layout);
    m_claims = makeSection(i18n("Soft locks"), {i18n("File"), i18n("Claimed by")}, content, layout);
    m_reviews = makeSection(i18n("Review requests"),
                            {i18n("Agent"), i18n("Summary"), i18n("When")}, content, layout);
    m_notes = makeSection(i18n("Notes board"),
                          {i18n("Author"), i18n("Note"), i18n("When")}, content, layout);
    // What agents just did to each other. coop.getState is a snapshot of the
    // board's *state*; this is the traffic that produced it, including the
    // orchestration calls (launch/wait/close) that leave no state behind.
    m_activityView = makeSection(i18n("Recent activity"),
                                 {i18n("When"), i18n("Agent"), i18n("Did")}, content, layout);
    layout->addStretch(1);
    scroll->setWidget(content);

    // Coalesce bursts (presence/open-file churn fires coop.changed rapidly).
    m_debounce = new QTimer(this);
    m_debounce->setSingleShot(true);
    m_debounce->setInterval(150);
    connect(m_debounce, &QTimer::timeout, this, [this] {
        refresh();
        applyActivity();
    });

    connect(m_core, &CoreClient::connected, this, &CooperationPanel::refresh);
    connect(m_core, &CoreClient::notification, this,
            [this](const QString &method, const QJsonObject &params) {
                if (method == QLatin1String("coop.changed")) {
                    scheduleRefresh();
                    return;
                }
                if (method != QLatin1String("mcp.activity")) {
                    return;
                }
                const QString tool = params.value(QStringLiteral("tool")).toString();
                if (tool.isEmpty()) {
                    return;
                }
                m_activity.append(
                    {QTime::currentTime().toString(QStringLiteral("HH:mm:ss")),
                     params.value(QStringLiteral("threadId")).toString(),
                     tool,
                     params.value(QStringLiteral("ok")).toBool()
                         ? params.value(QStringLiteral("argsSummary")).toString()
                         : params.value(QStringLiteral("error")).toString(),
                     params.value(QStringLiteral("ok")).toBool()});
                while (m_activity.size() > kMaxActivityRows) {
                    m_activity.removeFirst();
                }
                scheduleRefresh(); // same 150 ms coalescing as the state sections
            });
    if (m_core->isConnected()) {
        refresh();
    }
}

void CooperationPanel::scheduleRefresh()
{
    m_debounce->start();
}

void CooperationPanel::refresh()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("coop.getState"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         return;
                     }
                     applyState(result);
                 },
                 this); // lifetime guard against a late reply after teardown
}

void CooperationPanel::applyActivity()
{
    if (!m_activityView) {
        return;
    }
    m_activityView->clear();
    // Newest first: the strip is short, so the interesting end goes on top
    // rather than requiring a scroll.
    for (int i = m_activity.size() - 1; i >= 0; --i) {
        const ActivityRow &r = m_activity.at(i);
        auto *item = new QTreeWidgetItem(
            m_activityView,
            {r.time, r.thread.left(8),
             r.ok ? i18nc("cooperation activity: tool and its summary", "%1 %2", r.tool,
                          r.summary)
                  : i18nc("failed cooperation call: tool and error", "%1 ✗ %2", r.tool,
                          r.summary)});
        item->setToolTip(1, r.thread);
    }
}

void CooperationPanel::applyState(const QJsonObject &state)
{
    m_presence->clear();
    const QJsonArray presence = state.value(QStringLiteral("presence")).toArray();
    for (const QJsonValue &v : presence) {
        const QJsonObject o = v.toObject();
        new QTreeWidgetItem(m_presence,
                            {ownerLabel(o.value(QStringLiteral("owner")).toString()),
                             o.value(QStringLiteral("focusedFile")).toString()});
    }

    m_files->clear();
    const QJsonArray files = state.value(QStringLiteral("openFiles")).toArray();
    for (const QJsonValue &v : files) {
        const QJsonObject o = v.toObject();
        new QTreeWidgetItem(m_files,
                            {o.value(QStringLiteral("path")).toString(),
                             ownerLabel(o.value(QStringLiteral("owner")).toString())});
    }

    m_claims->clear();
    const QJsonArray claims = state.value(QStringLiteral("claims")).toArray();
    for (const QJsonValue &v : claims) {
        const QJsonObject o = v.toObject();
        new QTreeWidgetItem(m_claims,
                            {o.value(QStringLiteral("path")).toString(),
                             ownerLabel(o.value(QStringLiteral("owner")).toString())});
    }

    m_reviews->clear();
    const QJsonArray reviews = state.value(QStringLiteral("reviews")).toArray();
    for (const QJsonValue &v : reviews) {
        const QJsonObject o = v.toObject();
        new QTreeWidgetItem(m_reviews,
                            {o.value(QStringLiteral("thread")).toString(),
                             o.value(QStringLiteral("summary")).toString(),
                             shortTime(o.value(QStringLiteral("time")).toString())});
    }

    m_notes->clear();
    const QJsonArray notes = state.value(QStringLiteral("notes")).toArray();
    for (const QJsonValue &v : notes) {
        const QJsonObject o = v.toObject();
        new QTreeWidgetItem(m_notes,
                            {ownerLabel(o.value(QStringLiteral("author")).toString()),
                             o.value(QStringLiteral("text")).toString(),
                             shortTime(o.value(QStringLiteral("time")).toString())});
    }

    const int active = presence.size() + claims.size();
    if (presence.isEmpty() && files.isEmpty() && claims.isEmpty() && reviews.isEmpty()
        && notes.isEmpty()) {
        m_summary->setText(
            i18n("No cooperation activity yet. Agents share presence, file locks, "
                 "notes and review requests here as they work."));
    } else {
        m_summary->setText(i18np("%1 active participant.", "%1 active participants.",
                                 qMax(1, active)));
    }
}
