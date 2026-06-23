#include "PanelStack.h"

namespace {
// Clamp away the invalid (-1) components QWidget returns when a widget offers no
// recommendation, and fold in any explicit minimumSize the page may set.
QSize sanitize(QSize hint, QSize floor = QSize(0, 0))
{
    return QSize(qMax(0, hint.width()), qMax(0, hint.height())).expandedTo(floor);
}
} // namespace

PanelStack::PanelStack(QWidget *parent)
    : QStackedWidget(parent)
{
    // When the raised page changes the splitter must re-query our hints: the new
    // page may be narrower or wider than the one it replaced.
    connect(this, &QStackedWidget::currentChanged, this, [this] { updateGeometry(); });
}

QSize PanelStack::sizeHint() const
{
    if (QWidget *w = currentWidget()) {
        const QSize hint = w->sizeHint();
        if (hint.isValid()) {
            return hint;
        }
    }
    return QStackedWidget::sizeHint();
}

QSize PanelStack::minimumSizeHint() const
{
    if (QWidget *w = currentWidget()) {
        return sanitize(w->minimumSizeHint(), w->minimumSize());
    }
    return QSize(0, 0);
}
