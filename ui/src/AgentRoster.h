#pragma once

#include <QString>
#include <QWidget>

class QTreeWidget;
class QTreeWidgetItem;

// AgentRoster is the left-hand navigation tree: projects as top-level rows,
// their agents nested beneath, each with a live status dot. Selecting an agent
// activates its conversation; selecting a project focuses that project.
class AgentRoster : public QWidget
{
    Q_OBJECT
public:
    explicit AgentRoster(QWidget *parent = nullptr);

    void addProject(const QString &path, const QString &name);
    void addAgent(const QString &projectPath, int agentId, const QString &title);
    void setAgentTitle(int agentId, const QString &title);
    void setAgentStatus(int agentId, const QString &dotColorHex);
    void setAgentDormant(int agentId, bool dormant);
    void removeAgent(int agentId);
    void removeProject(const QString &path);
    void setCurrentAgent(int agentId);

Q_SIGNALS:
    void openProjectRequested();
    void newAgentRequested(const QString &projectPath);
    void closeProjectRequested(const QString &projectPath);
    void agentActivated(int agentId);
    void projectFocused(const QString &projectPath);
    void resumeRequested(int agentId);
    void commitRequested(int agentId);
    void prRequested(int agentId);
    void landRequested(int agentId);
    void discardRequested(int agentId);
    void closeRequested(int agentId);

private:
    QTreeWidgetItem *projectItem(const QString &path) const;
    QTreeWidgetItem *agentItem(int agentId) const;
    QString selectedProject() const;

    QTreeWidget *m_tree = nullptr;
};
