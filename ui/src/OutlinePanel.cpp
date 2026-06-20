#include "OutlinePanel.h"

#include <QIcon>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QVBoxLayout>

namespace {
// LSP SymbolKind → a Breeze theme icon name.
QString iconForKind(int kind)
{
    switch (kind) {
    case 5:  // Class
    case 23: // Struct
    case 11: // Interface
    case 10: // Enum
        return QStringLiteral("code-class");
    case 6:  // Method
    case 12: // Function
    case 9:  // Constructor
        return QStringLiteral("code-function");
    case 8:  // Field
    case 7:  // Property
    case 13: // Variable
    case 14: // Constant
    case 22: // EnumMember
        return QStringLiteral("code-variable");
    case 2:  // Module
    case 3:  // Namespace
    case 4:  // Package
        return QStringLiteral("code-context");
    default:
        return QStringLiteral("code-context");
    }
}
} // namespace

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
    item->setIcon(0, QIcon::fromTheme(iconForKind(symbol.kind)));
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
