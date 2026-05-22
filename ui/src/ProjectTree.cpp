#include "ProjectTree.h"

#include <QFileSystemModel>
#include <QTreeView>
#include <QVBoxLayout>

ProjectTree::ProjectTree(QWidget *parent)
    : QWidget(parent)
    , m_tree(new QTreeView(this))
    , m_model(new QFileSystemModel(this))
{
    m_tree->setModel(m_model);
    m_tree->setHeaderHidden(true);
    // Show only the name column.
    for (int col = 1; col < m_model->columnCount(); ++col) {
        m_tree->hideColumn(col);
    }

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_tree);

    connect(m_tree, &QTreeView::activated, this, [this](const QModelIndex &idx) {
        if (!m_model->isDir(idx)) {
            emit fileActivated(m_model->filePath(idx));
        }
    });
}

void ProjectTree::setRoot(const QString &path)
{
    const QModelIndex idx = m_model->setRootPath(path);
    m_tree->setRootIndex(idx);
}
