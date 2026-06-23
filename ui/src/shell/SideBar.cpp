#include "SideBar.h"

#include "PanelStack.h"

#include <KConfigGroup>

#include <QWidget>

SideBar::SideBar(KMultiTabBar::KMultiTabBarPosition pos, QWidget *parent)
    : QObject(parent)
    , m_pos(pos)
    , m_bar(new KMultiTabBar(pos, parent))
    , m_stack(new PanelStack(parent))
{
    m_bar->setStyle(KMultiTabBar::VSNET);
    // The panel stack starts collapsed; placing it inside a QSplitter that
    // honours child visibility makes the splitter handle the resize for us.
    m_stack->setVisible(false);
}

int SideBar::addPanel(const QIcon &icon, const QString &label, QWidget *panel)
{
    const int id = m_nextId++;
    m_bar->appendTab(icon, id, label);
    m_stack->addWidget(panel);
    m_ids.append(id);
    m_meta.insert(id, PanelMeta{panel, icon, label});
    if (auto *tab = m_bar->tab(id)) {
        tab->setToolTip(label);
        // KMultiTabBarTab is checkable: Qt toggles the raised state before
        // emitting clicked. So isTabRaised(id) here reflects the post-click
        // state — true means "user wants this shown", false means "user
        // clicked the already-raised tab to collapse the strip".
        connect(tab, &KMultiTabBarTab::clicked, this, [this, id] {
            if (m_bar->isTabRaised(id)) {
                setRaisedId(id);
            } else {
                setRaisedId(-1);
            }
        });
        // Right-click → request a context menu from the host. The tab is a
        // QToolButton, so customContextMenuRequested gives us tab-local
        // coordinates that we promote to screen coordinates for the receiver.
        tab->setContextMenuPolicy(Qt::CustomContextMenu);
        QWidget *tabWidget = tab;
        connect(tabWidget, &QWidget::customContextMenuRequested, this,
                [this, id, tabWidget](const QPoint &pos) {
                    emit tabContextMenuRequested(id, tabWidget->mapToGlobal(pos));
                });
    }
    return id;
}

QWidget *SideBar::panelWidget(int id) const
{
    return m_meta.value(id).widget;
}

QIcon SideBar::panelIcon(int id) const
{
    return m_meta.value(id).icon;
}

QString SideBar::panelLabel(int id) const
{
    return m_meta.value(id).label;
}

SideBar::PanelMeta SideBar::takePanel(int id)
{
    if (!m_ids.contains(id)) {
        return {};
    }
    PanelMeta meta = m_meta.take(id);
    if (m_raised == id) {
        setRaisedId(-1);
    }
    m_ids.removeAll(id);
    if (meta.widget) {
        m_stack->removeWidget(meta.widget);
        meta.widget->setParent(nullptr);
        meta.widget->hide();
    }
    m_bar->removeTab(id);
    return meta;
}

void SideBar::setRaisedId(int id)
{
    if (id >= 0 && !m_ids.contains(id)) {
        return;
    }
    m_raised = id;
    for (int otherId : m_ids) {
        m_bar->setTab(otherId, otherId == id);
    }
    if (id < 0) {
        m_stack->setVisible(false);
    } else {
        m_stack->setCurrentIndex(m_ids.indexOf(id));
        m_stack->setVisible(true);
    }
    emit raisedChanged(id);
}

void SideBar::setPanelVisible(int id, bool visible)
{
    if (visible) {
        setRaisedId(id);
    } else if (m_raised == id) {
        setRaisedId(-1);
    }
}

bool SideBar::isPanelVisible(int id) const
{
    return m_raised == id;
}

int SideBar::raisedId() const
{
    return m_raised;
}

int SideBar::panelIdAt(int index) const
{
    if (index < 0 || index >= m_ids.size()) {
        return -1;
    }
    return m_ids.at(index);
}

void SideBar::saveState(KConfigGroup &grp) const
{
    grp.writeEntry("raised", m_raised);
}

void SideBar::restoreState(const KConfigGroup &grp)
{
    setRaisedId(grp.readEntry("raised", -1));
}
