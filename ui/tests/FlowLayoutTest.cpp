// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "shell/FlowLayout.h"

#include <QPushButton>
#include <QtTest/QtTest>

// Verifies the core responsiveness contract: as the host gets narrower the
// FlowLayout wraps onto more rows (greater heightForWidth), and its minimum
// width stays ~one item — never the sum — so it can never pin a panel wide.
class FlowLayoutTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void wrapsWhenNarrow();
    void minimumIsOneItemNotSum();
    void takeAtRemovesItems();
};

static FlowLayout *populated(QWidget *host, int n, QSize each)
{
    auto *flow = new FlowLayout(host, /*margin=*/0, /*h=*/4, /*v=*/4);
    for (int i = 0; i < n; ++i) {
        auto *b = new QPushButton(host);
        b->setFixedSize(each);
        flow->addWidget(b);
    }
    return flow;
}

void FlowLayoutTest::wrapsWhenNarrow()
{
    QWidget host;
    FlowLayout *flow = populated(&host, 6, QSize(60, 24));

    // Wide enough for one row vs. narrow enough for one item per row.
    const int wide = flow->heightForWidth(6 * (60 + 4) + 20);
    const int narrow = flow->heightForWidth(70);
    QVERIFY2(narrow > wide, "rows should grow taller as width shrinks");
    // ~6 rows of 24px when stacked one-per-line.
    QVERIFY(narrow >= 6 * 24);
}

void FlowLayoutTest::minimumIsOneItemNotSum()
{
    QWidget host;
    populated(&host, 8, QSize(50, 20));
    auto *flow = static_cast<FlowLayout *>(host.layout());
    // One item is 50 wide; eight summed would be >=400. The minimum must be
    // close to a single item so the panel is free to shrink.
    QVERIFY2(flow->minimumSize().width() < 120,
             "minimum width must be ~one item, not the sum of all");
}

void FlowLayoutTest::takeAtRemovesItems()
{
    QWidget host;
    FlowLayout *flow = populated(&host, 3, QSize(40, 20));
    QCOMPARE(flow->count(), 3);
    delete flow->takeAt(0);
    QCOMPARE(flow->count(), 2);
}

QTEST_MAIN(FlowLayoutTest)
#include "FlowLayoutTest.moc"
