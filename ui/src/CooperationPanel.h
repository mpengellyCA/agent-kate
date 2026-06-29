#pragma once

#include <QJsonObject>
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

    CoreClient *m_core = nullptr;
    QTimer *m_debounce = nullptr;
    QLabel *m_summary = nullptr;
    QTreeWidget *m_presence = nullptr;
    QTreeWidget *m_files = nullptr;
    QTreeWidget *m_claims = nullptr;
    QTreeWidget *m_notes = nullptr;
    QTreeWidget *m_reviews = nullptr;
};
