#pragma once

#include <QString>
#include <QWidget>

class QTreeView;
class QFileSystemModel;

// ProjectTree is the workspace file browser. Activating a file emits
// fileActivated so the MainWindow can open it in the editor.
class ProjectTree : public QWidget
{
    Q_OBJECT
public:
    explicit ProjectTree(QWidget *parent = nullptr);

    void setRoot(const QString &path);

Q_SIGNALS:
    void fileActivated(const QString &path);

private:
    QTreeView *m_tree = nullptr;
    QFileSystemModel *m_model = nullptr;
};
