#pragma once

#include <KMultiTabBar>
#include <QHash>
#include <QIcon>
#include <QList>
#include <QObject>
#include <QString>

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
    int  panelCount() const { return m_ids.size(); }
    int  panelIdAt(int index) const; // -1 if index is out of range
    QWidget *panelWidget(int id) const;
    QIcon panelIcon(int id) const;
    QString panelLabel(int id) const;

    // Remove the panel from this strip without destroying its widget. The
    // returned widget has been reparented to nullptr and is ready to be
    // re-added to another SideBar (via addPanel) or shown as a top-level
    // window. Returns nullptr if the id is unknown.
    struct PanelMeta { QWidget *widget = nullptr; QIcon icon; QString label; };
    PanelMeta takePanel(int id);

    void saveState(KConfigGroup &grp) const;
    void restoreState(const KConfigGroup &grp);

Q_SIGNALS:
    void raisedChanged(int id);
    // Fired when the user right-clicks a tab. globalPos is in screen
    // coordinates so the receiver can pop a context menu directly.
    void tabContextMenuRequested(int id, const QPoint &globalPos);

private:
    KMultiTabBar::KMultiTabBarPosition m_pos;
    KMultiTabBar *m_bar = nullptr;
    QStackedWidget *m_stack = nullptr;
    QList<int> m_ids;
    QHash<int, PanelMeta> m_meta;
    int m_raised = -1;
    int m_nextId = 0;
};
