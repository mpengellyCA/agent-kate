#pragma once

#include <QString>
#include <QWidget>

class LspManager;
class QAction;
class QTreeWidget;

// ProblemsPanel lists every LSP diagnostic across open files, grouped by file
// with severity icons and a per-severity filter toolbar. Activating an entry
// asks the window to reveal that file and line.
class ProblemsPanel : public QWidget
{
    Q_OBJECT
public:
    explicit ProblemsPanel(LspManager *lsp, QWidget *parent = nullptr);

Q_SIGNALS:
    void activated(const QString &path, int line);

private:
    void rebuild();

    LspManager *m_lsp = nullptr;
    QTreeWidget *m_tree = nullptr;
    QAction *m_showErrors = nullptr;
    QAction *m_showWarnings = nullptr;
    QAction *m_showInfo = nullptr;
};
