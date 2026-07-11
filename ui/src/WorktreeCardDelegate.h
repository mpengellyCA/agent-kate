// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStyledItemDelegate>

// WorktreeCardDelegate paints each worktree as a rounded card (plan 13 phase 9),
// mirroring the roster's AgentCardDelegate:
//   line 1 — "#N branch" (bold) + agent title (muted) + status pills, right to
//            left: ⚠ conflicts (negative), ✎ dirty count (amber), ↑↓ remote,
//            ↑ahead ↓behind vs base;
//   line 2 — elided worktree path + "updated Xs ago".
// The card border is tinted by state: conflicts → negative, dirty → amber,
// clean → neutral. Pills go through the shared ChipPainter helper; every colour
// comes from QPalette / ThemeManager semantic colours so KDE schemes keep
// working.
class WorktreeCardDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    explicit WorktreeCardDelegate(QObject *parent = nullptr);

    void paint(QPainter *painter, const QStyleOptionViewItem &opt,
               const QModelIndex &idx) const override;
    QSize sizeHint(const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) const override;
};
