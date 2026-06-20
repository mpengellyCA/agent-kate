#pragma once

#include <QJsonArray>

class LspClient;
class LspManager;
class QPoint;
class QWidget;

// LspActions builds and shows the Quick-Fix / code-action menu (the lightbulb).
// Given the raw LSP CodeAction[] result, it presents a QMenu of titles and runs
// the chosen action through the manager's shared edit/command applier.
namespace LspActions {
// Show a context menu of code actions at the global point `at` (or the cursor if
// invalid). Returns immediately; the chosen action runs via the manager.
void showMenu(LspManager *manager, LspClient *client, const QJsonArray &actions,
              QWidget *parent, const QPoint &at);
}
