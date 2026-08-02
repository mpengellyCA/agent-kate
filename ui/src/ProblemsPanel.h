#pragma once

#include <QList>
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

    // The panel's own commands, for MainWindow::registerCommands (plan 27 §1).
    // These three live on a 16px icon-only toolbar inside the panel and in no
    // menu at all, so before the palette could be handed them there was no way
    // to reach "show warnings" by name — or to discover that the filter existed
    // without hovering three unlabelled buttons.
    QList<QAction *> commands() const;

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
