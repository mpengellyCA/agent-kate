#include "ReferencesPanel.h"

#include <QFileInfo>
#include <QListWidget>
#include <QListWidgetItem>
#include <QVBoxLayout>

ReferencesPanel::ReferencesPanel(QWidget *parent)
    : QWidget(parent)
    , m_list(new QListWidget(this))
{
    m_list->setFrameShape(QFrame::NoFrame);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_list);

    connect(m_list, &QListWidget::itemActivated, this, [this](QListWidgetItem *item) {
        const QString path = item->data(Qt::UserRole).toString();
        if (!path.isEmpty()) {
            emit activated(path, item->data(Qt::UserRole + 1).toInt());
        }
    });
}

void ReferencesPanel::setLocations(const QList<Location> &locations)
{
    m_list->clear();
    if (locations.isEmpty()) {
        auto *item = new QListWidgetItem(QStringLiteral("No references found"));
        item->setFlags(Qt::NoItemFlags);
        m_list->addItem(item);
        return;
    }
    for (const Location &loc : locations) {
        auto *item = new QListWidgetItem(
            QStringLiteral("%1:%2").arg(QFileInfo(loc.path).fileName()).arg(loc.line + 1));
        item->setData(Qt::UserRole, loc.path);
        item->setData(Qt::UserRole + 1, loc.line);
        item->setToolTip(QStringLiteral("%1:%2").arg(loc.path).arg(loc.line + 1));
        m_list->addItem(item);
    }
}
