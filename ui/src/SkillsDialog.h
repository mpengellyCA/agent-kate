#pragma once

#include <QDialog>
#include <QSet>
#include <QString>

class CoreClient;
class QJsonArray;
class QLabel;
class QListWidget;
class QListWidgetItem;
class QPushButton;

// SkillsDialog manages Claude Code skills for the active project. It lists the
// central Agent Kate skill catalog (XDG_DATA_HOME/agentkate/skills) on the
// left and the skills installed under target/.claude/skills on the right;
// installing a skill creates a symlink so future edits to the central copy
// propagate without re-installing.
class SkillsDialog : public QDialog
{
    Q_OBJECT
public:
    explicit SkillsDialog(CoreClient *core, const QString &target,
                          QWidget *parent = nullptr);

private:
    void refresh();
    void install();
    void uninstall();
    void openCatalogDir();
    void populateCatalog(const QJsonArray &items);
    void populateInstalled(const QJsonArray &items);
    void updateButtons();
    void setStatus(const QString &message);

    CoreClient *m_core = nullptr;
    QString m_target;
    QString m_catalogDir;
    QSet<QString> m_installedNames;

    QLabel *m_targetLabel = nullptr;
    QListWidget *m_catalogList = nullptr;
    QListWidget *m_installedList = nullptr;
    QPushButton *m_installButton = nullptr;
    QPushButton *m_uninstallButton = nullptr;
    QPushButton *m_openCatalogButton = nullptr;
    QLabel *m_status = nullptr;
};
