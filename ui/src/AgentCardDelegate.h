// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStyledItemDelegate>
#include <Qt>

// Item-data roles shared between AgentRoster (which writes them) and
// AgentCardDelegate (which paints them). Qt::UserRole holds the agent id (int)
// for agent rows or the project path (QString) for project rows.
namespace AgentRoles {
constexpr int Dormant   = Qt::UserRole + 1; // bool — resumable, drawn dimmed
constexpr int Title     = Qt::UserRole + 2; // raw title, no "#N" prefix
constexpr int Number    = Qt::UserRole + 3; // worktree number, 0 = none
constexpr int Subtitle  = Qt::UserRole + 4; // muted second line (derived in UI)
constexpr int Dot       = Qt::UserRole + 5; // status-dot color, hex string
constexpr int Attention = Qt::UserRole + 6; // bool — needs the user's input (display)
// AttentionRaw is the underlying "still blocked" truth, kept separate from
// Attention (which the delegate paints). The display marker is suppressed while
// the row is the current/selected one, but the raw flag persists so the marker
// can be restored the moment the user navigates away from a still-blocked agent.
constexpr int AttentionRaw = Qt::UserRole + 7; // bool — really needs input
constexpr int Pinned    = Qt::UserRole + 8; // bool — title user-set, don't auto-overwrite
constexpr int Tags      = Qt::UserRole + 9; // QStringList — organization labels
} // namespace AgentRoles

// AgentCardDelegate renders agent rows of the roster tree as multi-line cards:
// a status dot, a bold title with a "#N" worktree badge, and a muted subtitle
// line below. Project (top-level) rows fall through to the default painting so
// they keep their plain section-header look. Modeled on RefChipDelegate /
// LogGraphDelegate — paint() lets the style draw the background/selection, then
// custom-paints on top using palette roles so it tracks Breeze light/dark.
class AgentCardDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    explicit AgentCardDelegate(QObject *parent = nullptr);

    void paint(QPainter *painter, const QStyleOptionViewItem &opt,
               const QModelIndex &idx) const override;
    QSize sizeHint(const QStyleOptionViewItem &opt,
                   const QModelIndex &idx) const override;
};
