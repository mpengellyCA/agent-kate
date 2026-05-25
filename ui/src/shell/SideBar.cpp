#include "SideBar.h"

#include <KConfigGroup>

#include <QBoxLayout>
#include <QStackedWidget>

SideBar::SideBar(KMultiTabBar::KMultiTabBarPosition pos, QWidget *parent)
    : QWidget(parent)
    , m_pos(pos)
{
    m_bar = new KMultiTabBar(pos, this);
    m_bar->setStyle(KMultiTabBar::VSNET);
    m_stack = new QStackedWidget(this);

    QBoxLayout *layout = nullptr;
    switch (pos) {
    case KMultiTabBar::Left:
        layout = new QHBoxLayout(this);
        layout->addWidget(m_bar);
        layout->addWidget(m_stack, 1);
        break;
    case KMultiTabBar::Right:
        layout = new QHBoxLayout(this);
        layout->addWidget(m_stack, 1);
        layout->addWidget(m_bar);
        break;
    case KMultiTabBar::Top:
        layout = new QVBoxLayout(this);
        layout->addWidget(m_bar);
        layout->addWidget(m_stack, 1);
        break;
    case KMultiTabBar::Bottom:
    default:
        layout = new QVBoxLayout(this);
        layout->addWidget(m_stack, 1);
        layout->addWidget(m_bar);
        break;
    }
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);

    m_stack->setVisible(false); // start collapsed; setRaisedId expands it
    updateCollapsedConstraint();
}

int SideBar::addPanel(const QIcon &icon, const QString &label, QWidget *panel)
{
    const int id = m_nextId++;
    m_bar->appendTab(icon, id, label);
    m_stack->addWidget(panel);
    m_ids.append(id);
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
    }
    return id;
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
    updateCollapsedConstraint();
    emit raisedChanged(id);
}

void SideBar::updateCollapsedConstraint()
{
    // When collapsed, the SideBar shrinks to just the strip so its host
    // (typically a QDockWidget) follows along. When expanded, constraints
    // are released so the host's resize handle works normally.
    const bool horizontal =
        (m_pos == KMultiTabBar::Bottom || m_pos == KMultiTabBar::Top);
    if (m_raised < 0) {
        const int hint = horizontal ? m_bar->sizeHint().height()
                                    : m_bar->sizeHint().width();
        if (horizontal) {
            setMinimumHeight(hint);
            setMaximumHeight(hint);
            setMinimumWidth(0);
            setMaximumWidth(QWIDGETSIZE_MAX);
        } else {
            setMinimumWidth(hint);
            setMaximumWidth(hint);
            setMinimumHeight(0);
            setMaximumHeight(QWIDGETSIZE_MAX);
        }
    } else {
        setMinimumSize(0, 0);
        setMaximumSize(QWIDGETSIZE_MAX, QWIDGETSIZE_MAX);
    }
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

void SideBar::saveState(KConfigGroup &grp) const
{
    grp.writeEntry("raised", m_raised);
}

void SideBar::restoreState(const KConfigGroup &grp)
{
    setRaisedId(grp.readEntry("raised", -1));
}
