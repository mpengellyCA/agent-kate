#pragma once

#include <QStringList>
#include <QWidget>

class CoreClient;
class KHistoryComboBox;
class QJsonObject;
class QLabel;
class QLineEdit;
class QPoint;
class QToolButton;
class QTreeWidget;
class QTreeWidgetItem;
class QTimer;

// SearchPanel is the global code-search dock — a ripgrep-backed search of the
// active project's worktree with case/regex/word toggles and include/exclude
// glob filters. Results are grouped by file; activating a row opens the file
// at that line and column in the editor. The query field is a KHistoryComboBox
// so recent searches recall with Up/Down and persist across restarts.
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

    // Step to the next / previous match row, opening it in the editor. Wired to
    // the global F3 / Shift+F3 actions. Wrap around at the ends.
    void focusNextResult();
    void focusPrevResult();

Q_SIGNALS:
    // Emitted when the user activates a result. The host opens the file at the
    // given line and column in the editor area.
    void resultActivated(const QString &path, int line, int column);

    // Emitted when the user presses Esc in the query field — the host returns
    // focus to the active editor view.
    void escapeToEditor();

    // Emitted from the context menu (and available to drops) when the user
    // wants the selected results' files added to the active chat as context.
    void attachToChatRequested(const QStringList &paths);

protected:
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void runSearch();
    void scheduleSearch();
    void onReply(const QJsonObject &result, const QJsonObject &error);
    void clearResults();
    void onContextMenu(const QPoint &pos);
    // Distinct file paths backing the current selection (whole-file
    // granularity), falling back to the row under the cursor when nothing is
    // selected.
    QStringList selectedResultPaths() const;
    void commitHistory();
    void setBusy(bool busy);
    void stopSearch();
    void activateMatchRow(QTreeWidgetItem *row);
    QTreeWidgetItem *stepMatchRow(int direction);
    QString queryText() const;

    CoreClient *m_core = nullptr;
    QString m_root;
    KHistoryComboBox *m_query = nullptr;
    QToolButton *m_caseBtn = nullptr;
    QToolButton *m_wordBtn = nullptr;
    QToolButton *m_regexBtn = nullptr;
    QToolButton *m_stopBtn = nullptr;
    QLineEdit *m_include = nullptr;
    QLineEdit *m_exclude = nullptr;
    QTreeWidget *m_results = nullptr;
    QLabel *m_status = nullptr;
    QTimer *m_debounce = nullptr;
    quint64 m_seq = 0; // request generation; stale replies drop
    bool m_busy = false;
};
