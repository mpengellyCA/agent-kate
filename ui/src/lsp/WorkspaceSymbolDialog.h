#pragma once

#include "LspManager.h" // Symbol

#include <QDialog>
#include <QList>
#include <QString>

class LspManager;
class QLineEdit;
class QListWidget;
class QTimer;

// WorkspaceSymbolDialog is the "Go to Symbol in Workspace…" (Ctrl+T) palette. It
// queries workspace/symbol incrementally and emits the chosen symbol's location.
class WorkspaceSymbolDialog : public QDialog
{
    Q_OBJECT
public:
    explicit WorkspaceSymbolDialog(LspManager *lsp, QWidget *parent = nullptr);

Q_SIGNALS:
    void symbolChosen(const QString &path, int line);

private:
    void runQuery();
    void populate(const QList<Symbol> &symbols);

    LspManager *m_lsp = nullptr;
    QLineEdit *m_query = nullptr;
    QListWidget *m_list = nullptr;
    QTimer *m_debounce = nullptr;
};
