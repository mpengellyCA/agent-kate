#include "AgentRoster.h"

#include <QAction>
#include <QColor>
#include <QFont>
#include <QHBoxLayout>
#include <QIcon>
#include <QMenu>
#include <QPainter>
#include <QPixmap>
#include <QPushButton>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QVBoxLayout>

namespace {
// Project items store their path at Qt::UserRole; agent items store their id
// at Qt::UserRole, a "dormant" bool at Qt::UserRole + 1, the raw title at
// Qt::UserRole + 2, and the worktree number (int, 0 = unknown) at
// Qt::UserRole + 3. Title and number are stored separately so the visible
// label can be recomposed when either changes.
constexpr int RoleTitle  = Qt::UserRole + 2;
constexpr int RoleNumber = Qt::UserRole + 3;

QString composeLabel(int number, const QString &title)
{
    if (number > 0) {
        return QStringLiteral("#%1  %2").arg(number).arg(title);
    }
    return title;
}

QIcon dotIcon(const QString &hex)
{
    QPixmap pm(14, 14);
    pm.fill(Qt::transparent);
    QPainter p(&pm);
    p.setRenderHint(QPainter::Antialiasing);
    p.setPen(Qt::NoPen);
    p.setBrush(QColor(hex));
    p.drawEllipse(2, 2, 10, 10);
    p.end();
    return QIcon(pm);
}
} // namespace

AgentRoster::AgentRoster(QWidget *parent)
    : QWidget(parent)
    , m_tree(new QTreeWidget(this))
{
    auto *openButton = new QPushButton(QStringLiteral("Open Project…"), this);
    openButton->setCursor(Qt::PointingHandCursor);
    connect(openButton, &QPushButton::clicked, this, &AgentRoster::openProjectRequested);

    auto *newButton = new QPushButton(QStringLiteral("+  New Agent"), this);
    newButton->setCursor(Qt::PointingHandCursor);
    connect(newButton, &QPushButton::clicked, this,
            [this] { emit newAgentRequested(selectedProject()); });

    m_tree->setHeaderHidden(true);
    m_tree->setIndentation(14);
    m_tree->setContextMenuPolicy(Qt::CustomContextMenu);

    connect(m_tree, &QTreeWidget::currentItemChanged, this,
            [this](QTreeWidgetItem *item, QTreeWidgetItem *) {
                if (!item) {
                    return;
                }
                if (item->parent()) {
                    emit agentActivated(item->data(0, Qt::UserRole).toInt());
                } else {
                    emit projectFocused(item->data(0, Qt::UserRole).toString());
                }
            });

    connect(m_tree, &QTreeWidget::customContextMenuRequested, this, [this](const QPoint &pos) {
        QTreeWidgetItem *item = m_tree->itemAt(pos);
        if (!item) {
            return;
        }
        QMenu menu(this);
        if (item->parent()) {
            const int id = item->data(0, Qt::UserRole).toInt();
            const bool dormant = item->data(0, Qt::UserRole + 1).toBool();
            QAction *resumeAct = nullptr;
            if (dormant) {
                resumeAct = menu.addAction(QStringLiteral("Resume agent"));
                menu.addSeparator();
            }
            QAction *commitAct = menu.addAction(QStringLiteral("Commit changes…"));
            QAction *prAct = menu.addAction(QStringLiteral("Create pull request…"));
            QAction *landAct = menu.addAction(QStringLiteral("Merge into local main…"));
            QAction *discardAct = menu.addAction(QStringLiteral("Discard worktree"));
            menu.addSeparator();
            QAction *closeAct = menu.addAction(QStringLiteral("Close agent"));
            QAction *chosen = menu.exec(m_tree->viewport()->mapToGlobal(pos));
            if (chosen && chosen == resumeAct) {
                emit resumeRequested(id);
            } else if (chosen == commitAct) {
                emit commitRequested(id);
            } else if (chosen == prAct) {
                emit prRequested(id);
            } else if (chosen == landAct) {
                emit landRequested(id);
            } else if (chosen == discardAct) {
                emit discardRequested(id);
            } else if (chosen == closeAct) {
                emit closeRequested(id);
            }
        } else {
            const QString path = item->data(0, Qt::UserRole).toString();
            QAction *newAct = menu.addAction(QStringLiteral("New agent in this project"));
            QAction *closeAct = menu.addAction(QStringLiteral("Close project"));
            QAction *chosen = menu.exec(m_tree->viewport()->mapToGlobal(pos));
            if (chosen == newAct) {
                emit newAgentRequested(path);
            } else if (chosen == closeAct) {
                emit closeProjectRequested(path);
            }
        }
    });

    auto *buttons = new QHBoxLayout;
    buttons->setSpacing(6);
    buttons->addWidget(openButton);
    buttons->addWidget(newButton);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(8, 8, 8, 8);
    layout->setSpacing(8);
    layout->addLayout(buttons);
    layout->addWidget(m_tree, 1);
}

void AgentRoster::addProject(const QString &path, const QString &name)
{
    if (projectItem(path)) {
        return;
    }
    auto *item = new QTreeWidgetItem(m_tree);
    item->setText(0, name);
    item->setData(0, Qt::UserRole, path);
    item->setToolTip(0, path);
    QFont font = item->font(0);
    font.setBold(true);
    item->setFont(0, font);
    item->setExpanded(true);
}

void AgentRoster::addAgent(const QString &projectPath, int agentId, const QString &title)
{
    QTreeWidgetItem *project = projectItem(projectPath);
    if (!project) {
        return;
    }
    auto *item = new QTreeWidgetItem(project);
    item->setData(0, Qt::UserRole, agentId);
    item->setData(0, RoleTitle, title);
    item->setData(0, RoleNumber, 0);
    item->setText(0, composeLabel(0, title));
    item->setIcon(0, dotIcon(QStringLiteral("#8b91a0")));
    project->setExpanded(true);
}

void AgentRoster::setAgentTitle(int agentId, const QString &title)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        item->setData(0, RoleTitle, title);
        item->setText(0, composeLabel(item->data(0, RoleNumber).toInt(), title));
    }
}

void AgentRoster::setAgentNumber(int agentId, int number)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    if (item->data(0, RoleNumber).toInt() == number) {
        return;
    }
    item->setData(0, RoleNumber, number);
    item->setText(0, composeLabel(number, item->data(0, RoleTitle).toString()));
}

void AgentRoster::setAgentStatus(int agentId, const QString &dotColorHex)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        item->setIcon(0, dotIcon(dotColorHex));
    }
}

void AgentRoster::setAgentDormant(int agentId, bool dormant)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        item->setData(0, Qt::UserRole + 1, dormant);
        QFont font = item->font(0);
        font.setItalic(dormant); // dormant agents read as resumable history
        item->setFont(0, font);
    }
}

void AgentRoster::removeAgent(int agentId)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        delete item;
    }
}

void AgentRoster::removeProject(const QString &path)
{
    if (QTreeWidgetItem *item = projectItem(path)) {
        delete item; // also deletes its agent children
    }
}

void AgentRoster::setCurrentAgent(int agentId)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        m_tree->setCurrentItem(item);
    }
}

QTreeWidgetItem *AgentRoster::projectItem(const QString &path) const
{
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *item = m_tree->topLevelItem(i);
        if (item->data(0, Qt::UserRole).toString() == path) {
            return item;
        }
    }
    return nullptr;
}

QTreeWidgetItem *AgentRoster::agentItem(int agentId) const
{
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *project = m_tree->topLevelItem(i);
        for (int j = 0; j < project->childCount(); ++j) {
            QTreeWidgetItem *agent = project->child(j);
            if (agent->data(0, Qt::UserRole).toInt() == agentId) {
                return agent;
            }
        }
    }
    return nullptr;
}

QString AgentRoster::selectedProject() const
{
    QTreeWidgetItem *item = m_tree->currentItem();
    if (!item) {
        return QString();
    }
    if (item->parent()) {
        return item->parent()->data(0, Qt::UserRole).toString();
    }
    return item->data(0, Qt::UserRole).toString();
}
