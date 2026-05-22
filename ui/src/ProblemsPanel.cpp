#include "ProblemsPanel.h"
#include "lsp/LspManager.h"

#include <QFileInfo>
#include <QListWidget>
#include <QListWidgetItem>
#include <QVBoxLayout>

namespace {
QColor severityColor(int severity)
{
    switch (severity) {
    case 1:  return QColor(0xe3, 0x6b, 0x5f);
    case 2:  return QColor(0xe0, 0xa5, 0x3a);
    case 3:  return QColor(0x6f, 0x9b, 0xd6);
    default: return QColor(0x8b, 0x91, 0xa0);
    }
}
} // namespace

ProblemsPanel::ProblemsPanel(LspManager *lsp, QWidget *parent)
    : QWidget(parent)
    , m_lsp(lsp)
    , m_list(new QListWidget(this))
{
    m_list->setFrameShape(QFrame::NoFrame);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_list);

    connect(m_lsp, &LspManager::problemsChanged, this, &ProblemsPanel::rebuild);
    connect(m_list, &QListWidget::itemActivated, this, [this](QListWidgetItem *item) {
        const QString path = item->data(Qt::UserRole).toString();
        if (!path.isEmpty()) {
            emit activated(path, item->data(Qt::UserRole + 1).toInt());
        }
    });

    rebuild();
}

void ProblemsPanel::rebuild()
{
    m_list->clear();
    const QList<Problem> problems = m_lsp->problems();
    if (problems.isEmpty()) {
        auto *item = new QListWidgetItem(QStringLiteral("No problems detected"));
        item->setForeground(severityColor(4));
        item->setFlags(Qt::NoItemFlags);
        m_list->addItem(item);
        return;
    }
    for (const Problem &p : problems) {
        auto *item = new QListWidgetItem(
            QStringLiteral("%1:%2   %3")
                .arg(QFileInfo(p.path).fileName())
                .arg(p.line + 1)
                .arg(p.message.simplified()));
        item->setData(Qt::UserRole, p.path);
        item->setData(Qt::UserRole + 1, p.line);
        item->setForeground(severityColor(p.severity));
        item->setToolTip(QStringLiteral("%1:%2").arg(p.path).arg(p.line + 1));
        m_list->addItem(item);
    }
}
