#pragma once

#include <QString>
#include <QWidget>

class QStackedWidget;
class QTextBrowser;
class QToolButton;

// DiffView is a rich unified-diff viewer: per-file sections with old/new line
// numbers, +/- line backgrounds, and the actual code syntax-highlighted by the
// Kate engine's KSyntaxHighlighting. A file selector jumps between files.
//
// A toolbar toggle switches between the inline (default) rendering and a
// synced side-by-side view; the choice is remembered in KConfig. Binary and
// renamed files are detected and surfaced with friendly placeholder rows.
class DiffView : public QWidget
{
    Q_OBJECT
public:
    explicit DiffView(const QString &unifiedDiff, QWidget *parent = nullptr);

    // Override the text shown when the diff is empty (default: "No changes.").
    void setEmptyMessage(const QString &message);

private:
    void rebuild();

    QString m_unifiedDiff;
    QString m_emptyMessage;
    bool m_sideBySide = false;

    QStackedWidget *m_stack = nullptr;
    QTextBrowser *m_inline = nullptr;
    QTextBrowser *m_leftPane = nullptr;
    QTextBrowser *m_rightPane = nullptr;
    QToolButton *m_splitBtn = nullptr;
};
