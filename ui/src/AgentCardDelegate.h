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
constexpr int Subtitle  = Qt::UserRole + 4; // full status detail — tooltip only now
constexpr int Attention = Qt::UserRole + 6; // bool — needs the user's input (display)
// AttentionRaw is the underlying "still blocked" truth, kept separate from
// Attention (which the delegate paints). The display marker is suppressed while
// the row is the current/selected one, but the raw flag persists so the marker
// can be restored the moment the user navigates away from a still-blocked agent.
constexpr int AttentionRaw = Qt::UserRole + 7; // bool — really needs input
constexpr int Pinned    = Qt::UserRole + 8; // bool — title user-set, don't auto-overwrite
constexpr int Tags      = Qt::UserRole + 9; // QStringList — organization labels
// v2 roster card roles (plan 13 phase 7). StatusRole (not "Status" — Xlib
// #defines that macro on X11) holds an AgentStatus enum value as int.
constexpr int StatusRole = Qt::UserRole + 10; // AgentStatus — the source of truth
constexpr int Preview   = Qt::UserRole + 11; // last chat line ("You: …" for user)
constexpr int LastActivity = Qt::UserRole + 12; // secs-since-epoch of last message

// AgentStatus is the single source of truth for a card's status badge, replacing
// the old raw-hex Dot role. The delegate maps each state to a symbol + a
// palette/ThemeManager semantic colour, so KDE schemes keep working.
// (Not named "Status" — Xlib #defines that as a macro on X11.)
enum class AgentStatus {
    Idle = 0,   // has no live turn — "ready" / "send a follow-up"
    Working,    // a turn is computing (animated arc)
    NeedsInput, // a permission prompt is waiting on the user
    Dormant,    // resumable, no live process
    Error,      // the last start/turn failed
};
} // namespace AgentRoles

// AgentCardDelegate renders agent rows of the roster tree as rounded cards
// (plan 13 phase 7): line 1 is a status badge (symbol + semantic colour) + bold
// title + right-aligned relative time; lines 2-3 are a two-line elided chat
// preview; line 4 carries the "#N" worktree badge and tag chips. Project
// (top-level) rows fall through to the default painting so they keep their plain
// section-header look. Modeled on RefChipDelegate / LogGraphDelegate — paint()
// lets the style draw the selection, then custom-paints on top using palette
// roles + ThemeManager semantic colours so it tracks Breeze light/dark.
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
