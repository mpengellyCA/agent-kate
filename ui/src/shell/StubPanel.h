#pragma once

#include <QString>
#include <QWidget>

// StubPanel is the placeholder body for the new tool windows added by Phase 4
// (Search, Cooperation, AI Inspector, Output, Tasks). Each one is just a
// centred label naming the panel and a short hint about what will live there
// once the panel is wired to real data.
class StubPanel : public QWidget
{
    Q_OBJECT
public:
    StubPanel(const QString &title, const QString &hint, QWidget *parent = nullptr);
};
