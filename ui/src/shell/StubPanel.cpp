#include "StubPanel.h"

#include <QLabel>
#include <QVBoxLayout>

StubPanel::StubPanel(const QString &title, const QString &hint, QWidget *parent)
    : QWidget(parent)
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(24, 24, 24, 24);
    layout->setAlignment(Qt::AlignCenter);

    auto *titleLabel = new QLabel(title, this);
    QFont f = titleLabel->font();
    f.setPointSizeF(f.pointSizeF() * 1.2);
    f.setBold(true);
    titleLabel->setFont(f);
    titleLabel->setAlignment(Qt::AlignCenter);

    auto *hintLabel = new QLabel(hint, this);
    hintLabel->setAlignment(Qt::AlignCenter);
    hintLabel->setWordWrap(true);
    QPalette p = hintLabel->palette();
    p.setColor(QPalette::WindowText, p.color(QPalette::PlaceholderText));
    hintLabel->setPalette(p);

    layout->addStretch();
    layout->addWidget(titleLabel);
    layout->addSpacing(8);
    layout->addWidget(hintLabel);
    layout->addStretch();
}
