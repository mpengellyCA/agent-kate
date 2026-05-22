#pragma once

#include <QHash>
#include <QString>
#include <QStringList>
#include <QWidget>

namespace KTextEditor {
class Document;
class Editor;
class View;
}
class QStackedWidget;
class QTabWidget;

// EditorArea hosts editor tabs grouped by a caller-chosen key (a project path
// or an agent id). Each group has its own QTabWidget of KTextEditor views and
// diff views; setActiveGroup swaps the visible group. With no group active it
// shows a placeholder.
class EditorArea : public QWidget
{
    Q_OBJECT
public:
    explicit EditorArea(QWidget *parent = nullptr);

    void setActiveGroup(const QString &groupKey);
    void openFile(const QString &groupKey, const QString &path, int line = -1);
    void openDiff(const QString &groupKey, const QString &title, const QString &text);
    bool saveCurrent();
    QStringList openFilePaths() const;
    KTextEditor::View *currentView() const;

Q_SIGNALS:
    void openFilesChanged();
    void statusMessage(const QString &text);
    void currentFileChanged(const QString &path);
    void documentOpened(KTextEditor::Document *doc, const QString &path);
    void documentClosed(KTextEditor::Document *doc);

private:
    QTabWidget *groupTabs(const QString &key, bool create);
    QTabWidget *activeTabs() const;
    void closeTabIn(QTabWidget *tabs, int index);
    void emitCurrentFile();
    void updateVisible();

    QStackedWidget *m_stack = nullptr;
    QWidget *m_placeholder = nullptr;
    KTextEditor::Editor *m_editor = nullptr;
    QHash<QString, QTabWidget *> m_groups;
    QString m_activeGroup;
};
