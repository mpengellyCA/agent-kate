#pragma once

#include <QJsonObject>
#include <QList>
#include <QWidget>

class CoreClient;
class QTreeWidget;
class QTimer;
class QLabel;

// CooperationPanel renders the live Cooperation MCP board for the human: agent +
// human presence, open files, advisory soft-lock claims, the notes board, and the
// review backlog. It refreshes from coop.getState whenever the core broadcasts a
// coop.changed notification (any agent- or human-driven mutation), coalescing
// bursts through a short debounce so a flurry of presence updates is one fetch.
class CooperationPanel : public QWidget
{
    Q_OBJECT
public:
    explicit CooperationPanel(CoreClient *core, QWidget *parent = nullptr);

private:
    void scheduleRefresh();
    void refresh();
    void applyState(const QJsonObject &state);
    // Redraw the Recent activity section from m_activity. Runs on the same
    // debounce as the state sections — a busy ensemble emits mcp.activity far
    // faster than a human reads, and each row is a widget.
    void applyActivity();

    CoreClient *m_core = nullptr;
    QTimer *m_debounce = nullptr;
    QLabel *m_summary = nullptr;
    QTreeWidget *m_presence = nullptr;
    QTreeWidget *m_files = nullptr;
    QTreeWidget *m_claims = nullptr;
    QTreeWidget *m_notes = nullptr;
    QTreeWidget *m_reviews = nullptr;
    QTreeWidget *m_activityView = nullptr;
    // The newest cross-agent calls, oldest first, capped — this is a "what just
    // happened" strip, not a log; the AI Inspector's all-threads mode is the
    // full timeline.
    struct ActivityRow {
        QString time;
        QString thread;
        QString tool;
        QString summary;
        bool ok = true;
    };
    QList<ActivityRow> m_activity;
};
