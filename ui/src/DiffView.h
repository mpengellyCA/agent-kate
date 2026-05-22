#pragma once

#include <QString>
#include <QWidget>

// DiffView is a rich unified-diff viewer: per-file sections with old/new line
// numbers, +/- line backgrounds, and the actual code syntax-highlighted by the
// Kate engine's KSyntaxHighlighting. A file selector jumps between files.
class DiffView : public QWidget
{
    Q_OBJECT
public:
    explicit DiffView(const QString &unifiedDiff, QWidget *parent = nullptr);
};
