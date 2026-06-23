#include "ShellLayout.h"

#include <KConfigGroup>
#include <KMultiTabBar>

#include <QHBoxLayout>
#include <QSplitter>
#include <QStackedWidget>
#include <QVBoxLayout>

ShellLayout::ShellLayout(const Slots &s, QWidget *parent)
    : QWidget(parent)
{
    auto *root = new QVBoxLayout(this);
    root->setContentsMargins(0, 0, 0, 0);
    root->setSpacing(0);

    // Top portion: leftBar | (outerSplitter) | rightBar.
    auto *topRow = new QHBoxLayout();
    topRow->setContentsMargins(0, 0, 0, 0);
    topRow->setSpacing(0);
    if (s.leftBar) {
        topRow->addWidget(s.leftBar);
    }

    m_outer = new QSplitter(Qt::Horizontal, this);
    m_outer->setChildrenCollapsible(false);
    m_outer->setHandleWidth(2);
    // Non-opaque resize: a handle drag rubber-bands and relayouts the heavy
    // child (chat / document) once on release instead of every pixel — smooth
    // regardless of content weight. See docs/plans/10-panel-responsiveness.md.
    m_outer->setOpaqueResize(false);

    if (s.leftStack) {
        m_outer->addWidget(s.leftStack);
        // Hidden stacks contribute zero size to the splitter; allowing the
        // collapse here is fine since the bar can re-raise.
        m_outer->setCollapsible(m_outer->count() - 1, true);
    }

    m_centreV = new QSplitter(Qt::Vertical, m_outer);
    m_centreV->setChildrenCollapsible(false);
    m_centreV->setHandleWidth(2);
    m_centreV->setOpaqueResize(false);

    m_centreH = new QSplitter(Qt::Horizontal, m_centreV);
    m_centreH->setChildrenCollapsible(false);
    m_centreH->setHandleWidth(2);
    m_centreH->setOpaqueResize(false);
    if (s.editor) {
        m_centreH->addWidget(s.editor);
    }
    if (s.agentPanel) {
        m_centreH->addWidget(s.agentPanel);
    }
    m_centreV->addWidget(m_centreH);

    if (s.bottomStack) {
        m_centreV->addWidget(s.bottomStack);
        m_centreV->setCollapsible(m_centreV->count() - 1, true);
    }
    m_outer->addWidget(m_centreV);

    if (s.rightStack) {
        m_outer->addWidget(s.rightStack);
        m_outer->setCollapsible(m_outer->count() - 1, true);
    }
    topRow->addWidget(m_outer, 1);

    if (s.rightBar) {
        topRow->addWidget(s.rightBar);
    }

    root->addLayout(topRow, 1);
    if (s.bottomBar) {
        root->addWidget(s.bottomBar);
    }

    // Reasonable initial proportions; restoreState overrides these.
    m_outer->setSizes({260, 900, 260});
    m_centreH->setSizes({700, 500});
    m_centreV->setSizes({600, 200});
}

void ShellLayout::saveState(KConfigGroup &grp) const
{
    grp.writeEntry("outer", m_outer->saveState());
    grp.writeEntry("centreH", m_centreH->saveState());
    grp.writeEntry("centreV", m_centreV->saveState());
}

void ShellLayout::restoreState(const KConfigGroup &grp)
{
    const QByteArray outer = grp.readEntry("outer", QByteArray());
    const QByteArray h = grp.readEntry("centreH", QByteArray());
    const QByteArray v = grp.readEntry("centreV", QByteArray());
    if (!outer.isEmpty()) {
        m_outer->restoreState(outer);
    }
    if (!h.isEmpty()) {
        m_centreH->restoreState(h);
    }
    if (!v.isEmpty()) {
        m_centreV->restoreState(v);
    }
}
