#pragma once

#include "lsp/LspManager.h" // Location

#include <QString>
#include <QWidget>

class QListWidget;

// ReferencesPanel lists the results of a find-references request. Activating an
// entry asks the window to reveal that location.
class ReferencesPanel : public QWidget
{
    Q_OBJECT
public:
    explicit ReferencesPanel(QWidget *parent = nullptr);

    void setLocations(const QList<Location> &locations);

Q_SIGNALS:
    void activated(const QString &path, int line);

private:
    QListWidget *m_list = nullptr;
};
