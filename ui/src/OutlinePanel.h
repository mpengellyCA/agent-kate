#pragma once

#include "lsp/LspManager.h" // Symbol

#include <QString>
#include <QWidget>

class QTreeWidget;
class QTreeWidgetItem;

// OutlinePanel shows a file's document-symbol tree. Activating a symbol asks
// the window to reveal that line.
class OutlinePanel : public QWidget
{
    Q_OBJECT
public:
    explicit OutlinePanel(QWidget *parent = nullptr);

    void setSymbols(const QString &path, const QList<Symbol> &symbols);

Q_SIGNALS:
    void activated(const QString &path, int line);

private:
    QTreeWidgetItem *makeItem(const Symbol &symbol) const;

    QTreeWidget *m_tree = nullptr;
    QString m_path;
};
