#pragma once

#include <QWidget>

class KConfigGroup;
class KMultiTabBar;
class QSplitter;
class QStackedWidget;

// ShellLayout owns the AgentKate window's tool-window arrangement:
//
//   ┌──┬──────────────────────────────────────────────────────┬──┐
//   │L │ centreH:  editor          │ agentPanel               │R │
//   │B │ ─────────────────────────────────────────────────────│B │
//   │A │                                                       │A │
//   │R │ bottomStack                                           │R │
//   ├──┴──────────────────────────────────────────────────────┴──┤
//   │ bottomBar (KMultiTabBar Bottom, spans full width)          │
//   └────────────────────────────────────────────────────────────┘
//
// The three KMultiTabBar strips pin to the window edges; their matching
// QStackedWidgets live inside QSplitters so the panel areas resize cleanly.
// Both pieces belong to the same SideBar instance — ShellLayout just slots
// them into the right places.
class ShellLayout : public QWidget
{
    Q_OBJECT
public:
    struct Slots {
        KMultiTabBar *leftBar = nullptr;
        QStackedWidget *leftStack = nullptr;
        KMultiTabBar *rightBar = nullptr;
        QStackedWidget *rightStack = nullptr;
        KMultiTabBar *bottomBar = nullptr;
        QStackedWidget *bottomStack = nullptr;
        QWidget *editor = nullptr;
        QWidget *agentPanel = nullptr;
    };

    explicit ShellLayout(const Slots &s, QWidget *parent = nullptr);

    QSplitter *outerSplitter() const { return m_outer; }     // left | centre | right
    QSplitter *centreVSplitter() const { return m_centreV; } // editor-area / bottom-panel
    QSplitter *centreHSplitter() const { return m_centreH; } // editor / agent panel

    void saveState(KConfigGroup &grp) const;
    void restoreState(const KConfigGroup &grp);

private:
    QSplitter *m_outer = nullptr;
    QSplitter *m_centreV = nullptr;
    QSplitter *m_centreH = nullptr;
};
