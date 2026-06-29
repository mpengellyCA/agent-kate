// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QLayout>
#include <QList>
#include <QRect>
#include <QSize>
#include <QStyle>

// FlowLayout — arranges its items left-to-right and wraps to the next line when
// it runs out of horizontal width, the way text flows. Drop it in place of a
// QHBoxLayout for toolbars / chip rows inside docked panels: when the panel is
// dragged narrow the buttons reflow onto extra lines instead of clipping or
// pinning the strip to a minimum width.
//
// The key to that behaviour is hasHeightForWidth()==true together with a
// minimumSize() of roughly one item (not the sum) — so the panel is free to
// shrink and the layout simply grows taller. Adapted from the canonical Qt
// "Flow Layout" example.
class FlowLayout : public QLayout
{
public:
    explicit FlowLayout(QWidget *parent, int margin = -1, int hSpacing = -1, int vSpacing = -1);
    explicit FlowLayout(int margin = -1, int hSpacing = -1, int vSpacing = -1);
    ~FlowLayout() override;

    void addItem(QLayoutItem *item) override;
    int horizontalSpacing() const;
    int verticalSpacing() const;
    Qt::Orientations expandingDirections() const override;
    bool hasHeightForWidth() const override;
    int heightForWidth(int width) const override;
    int count() const override;
    QLayoutItem *itemAt(int index) const override;
    QLayoutItem *takeAt(int index) override;
    QSize minimumSize() const override;
    void setGeometry(const QRect &rect) override;
    QSize sizeHint() const override;

private:
    int doLayout(const QRect &rect, bool testOnly) const;
    int smartSpacing(QStyle::PixelMetric pm) const;

    QList<QLayoutItem *> m_items;
    int m_hSpace;
    int m_vSpace;
};
