// Regression guard for the panel-shell sizing fix (docs/plans/10-panel-responsiveness.md).
//
// A stock QStackedWidget reports the element-wise MAX of every page's minimum
// size, so a pane in a QSplitter can never shrink below its widest panel even
// while a small panel is raised — the "rigid pane" the user reported. PanelStack
// reports only the *current* page's hints. These tests pin that contract.

#include "shell/PanelStack.h"

#include <QStackedWidget>
#include <QWidget>
#include <QtTest>

class PanelStackTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void minimumTracksCurrentPage();
    void contrastWithStockStack();
    void emptyStackHasZeroMinimum();
};

void PanelStackTest::minimumTracksCurrentPage()
{
    PanelStack stack;
    auto *narrow = new QWidget;
    narrow->setMinimumSize(50, 30);
    auto *wide = new QWidget;
    wide->setMinimumSize(400, 30);
    stack.addWidget(narrow);
    stack.addWidget(wide);

    stack.setCurrentWidget(wide);
    QCOMPARE(stack.minimumSizeHint().width(), 400);

    // The fix: with the narrow page raised the stack no longer carries the wide
    // page's 400px floor, so the pane can be dragged down to 50.
    stack.setCurrentWidget(narrow);
    QCOMPARE(stack.minimumSizeHint().width(), 50);
}

void PanelStackTest::contrastWithStockStack()
{
    // Documents the stock max-aggregation PanelStack overrides: even with the
    // narrow page current, a plain QStackedWidget is floored by the wide page.
    QStackedWidget stock;
    auto *narrow = new QWidget;
    narrow->setMinimumSize(50, 30);
    auto *wide = new QWidget;
    wide->setMinimumSize(400, 30);
    stock.addWidget(narrow);
    stock.addWidget(wide);
    stock.setCurrentWidget(narrow);
    QVERIFY(stock.minimumSizeHint().width() >= 400);
}

void PanelStackTest::emptyStackHasZeroMinimum()
{
    PanelStack stack;
    QCOMPARE(stack.minimumSizeHint(), QSize(0, 0));
}

QTEST_MAIN(PanelStackTest)
#include "PanelStackTest.moc"
