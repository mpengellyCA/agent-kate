// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStyledItemDelegate>

// AuthorChipDelegate paints the log's Author column as a small accent-coloured
// initials chip followed by the elided author name (plan 13 phase 8 visual
// pass). The chip is drawn via the shared ChipPainter so it tracks the theme;
// the name uses the row's palette so selection stays legible.
class AuthorChipDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    explicit AuthorChipDelegate(QObject *parent = nullptr);

    void paint(QPainter *painter, const QStyleOptionViewItem &opt,
               const QModelIndex &idx) const override;
    QSize sizeHint(const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) const override;
};
