#pragma once

#include <QWidget>

class CoreClient;
class QJsonObject;
class QLabel;
class QLineEdit;
class QToolButton;
class QTreeWidget;
class QTreeWidgetItem;
class QTimer;

// SearchPanel is the global code-search dock — a ripgrep-backed search of the
// active project's worktree with case/regex/word toggles and include/exclude
// glob filters. Results are grouped by file; activating a row opens the file
// at that line in the editor.
class SearchPanel : public QWidget
{
    Q_OBJECT
public:
    explicit SearchPanel(CoreClient *core, QWidget *parent = nullptr);

    // Scope subsequent searches to root. Empty string disables searching.
    void setProjectRoot(const QString &root);

    // Focus the query field — wired to the global Ctrl+Shift+F action.
    void focusQuery();

    // Drive a search from outside the panel (e.g. the toolbar Search box).
    // Sets the query text — which triggers the existing debounced RPC path —
    // so the workspace-scoped root and all toggles are inherited. No-ops on
    // an empty/whitespace query.
    void search(const QString &query);

Q_SIGNALS:
    // Emitted when the user activates a result. The host opens the file at
    // the given line in the editor area.
    void resultActivated(const QString &path, int line);

private:
    void runSearch();
    void scheduleSearch();
    void onReply(const QJsonObject &result, const QJsonObject &error);
    void clearResults();

    CoreClient *m_core = nullptr;
    QString m_root;
    QLineEdit *m_query = nullptr;
    QToolButton *m_caseBtn = nullptr;
    QToolButton *m_wordBtn = nullptr;
    QToolButton *m_regexBtn = nullptr;
    QLineEdit *m_include = nullptr;
    QLineEdit *m_exclude = nullptr;
    QTreeWidget *m_results = nullptr;
    QLabel *m_status = nullptr;
    QTimer *m_debounce = nullptr;
    quint64 m_seq = 0; // request generation; stale replies drop
};
