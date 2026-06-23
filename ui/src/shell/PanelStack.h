#pragma once

#include <QStackedWidget>

// PanelStack is a QStackedWidget whose size hints track only the *current*
// page, not the element-wise maximum over every page (the stock behaviour).
//
// A stock QStackedWidget reports max-of-all-pages for sizeHint/minimumSizeHint
// — by design, so switching pages never forces the window to grow. But when the
// stack lives inside a QSplitter (as every Agent Kate pane does), that means the
// pane can never be dragged narrower than its *widest* panel, even while a small
// panel is raised: the user sees a "rigid" pane floored by whichever panel that
// stack can ever hold. Reporting only the current page's hints lets the pane
// shrink to whatever is shown, and keeps hidden (heavy) pages from contributing
// to drag-time relayout.
class PanelStack : public QStackedWidget
{
    Q_OBJECT
public:
    explicit PanelStack(QWidget *parent = nullptr);

    QSize sizeHint() const override;
    QSize minimumSizeHint() const override;
};
