#pragma once

#include <QString>
#include <QWidget>

class LspManager;
class QListWidget;

// ProblemsPanel lists every LSP diagnostic across open files. Activating an
// entry asks the window to reveal that file and line.
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
    QListWidget *m_list = nullptr;
};
