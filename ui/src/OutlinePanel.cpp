#include "OutlinePanel.h"

#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QVBoxLayout>

OutlinePanel::OutlinePanel(QWidget *parent)
    : QWidget(parent)
    , m_tree(new QTreeWidget(this))
{
    m_tree->setHeaderHidden(true);
    m_tree->setIndentation(14);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_tree);

    connect(m_tree, &QTreeWidget::itemActivated, this, [this](QTreeWidgetItem *item) {
        if (!m_path.isEmpty()) {
            emit activated(m_path, item->data(0, Qt::UserRole).toInt());
        }
    });
}

QTreeWidgetItem *OutlinePanel::makeItem(const Symbol &symbol) const
{
    auto *item = new QTreeWidgetItem();
    QString text = symbol.name;
    if (!symbol.detail.isEmpty()) {
        text += QStringLiteral("   ") + symbol.detail;
    }
    item->setText(0, text);
    item->setData(0, Qt::UserRole, symbol.line);
    for (const Symbol &child : symbol.children) {
        item->addChild(makeItem(child));
    }
    item->setExpanded(true);
    return item;
}

void OutlinePanel::setSymbols(const QString &path, const QList<Symbol> &symbols)
{
    m_path = path;
    m_tree->clear();
    for (const Symbol &symbol : symbols) {
        m_tree->addTopLevelItem(makeItem(symbol));
    }
}
