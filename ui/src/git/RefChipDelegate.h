// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStyledItemDelegate>

// RefChipDelegate draws the subject column with ref chips (branch / tag /
// remote) prepended. Tag refs come over the wire prefixed with "tag:", remote
// refs as "origin/<name>" or other "<remote>/<name>"; everything else is a
// local branch.
class RefChipDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    explicit RefChipDelegate(QObject *parent = nullptr);

    void paint(QPainter *painter, const QStyleOptionViewItem &opt,
               const QModelIndex &idx) const override;
    QSize sizeHint(const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) const override;
};
