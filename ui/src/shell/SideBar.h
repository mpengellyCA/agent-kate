#pragma once

#include <KMultiTabBar>
#include <QList>
#include <QObject>

class KConfigGroup;
class QStackedWidget;
class QWidget;

// SideBar coordinates a KMultiTabBar (the activity strip) with a
// QStackedWidget (the panel area). At most one tab is "raised" at a time;
// clicking the raised tab collapses the panel stack (Kate semantics).
//
// SideBar is a QObject so the strip and the panel stack can be placed in
// separate layout slots — ShellLayout pins each KMultiTabBar to an edge of
// the window while the matching QStackedWidget lives inside a QSplitter so
// users can drag-resize the panel area.
class SideBar : public QObject
{
    Q_OBJECT
public:
    explicit SideBar(KMultiTabBar::KMultiTabBarPosition pos, QWidget *parent);

    KMultiTabBar  *tabBar() const     { return m_bar; }
    QStackedWidget *panelStack() const { return m_stack; }

    // Append a panel to the strip. Returns the id used by setPanelVisible /
    // setRaisedId. The widget is reparented to the internal stack.
    int  addPanel(const QIcon &icon, const QString &label, QWidget *panel);

    void setPanelVisible(int id, bool visible);
    bool isPanelVisible(int id) const;
    int  raisedId() const;       // -1 when the strip is collapsed
    void setRaisedId(int id);    // -1 collapses the panel area

    void saveState(KConfigGroup &grp) const;
    void restoreState(const KConfigGroup &grp);

Q_SIGNALS:
    void raisedChanged(int id);

private:
    KMultiTabBar::KMultiTabBarPosition m_pos;
    KMultiTabBar *m_bar = nullptr;
    QStackedWidget *m_stack = nullptr;
    QList<int> m_ids;
    int m_raised = -1;
    int m_nextId = 0;
};
