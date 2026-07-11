// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "LogModel.h"

#include <KFormat>
#include <KLocalizedString>

#include <QLocale>
#include <QVariant>

namespace {
// The in-RAM history is capped at this many rows; once it grows past the cap
// the oldest commits (which live at the BOTTOM — applyHead prepends newer ones
// on top, appendPage tacks older ones below) are evicted. The full history
// stays in git and re-paginates on demand. Generous enough that ordinary
// browsing never reaches it, low enough that endless "load more" on a deep
// repo cannot grow the model into an OOM. Mirrors TranscriptModel's kMaxRows.
constexpr int kMaxRows = 5000;
} // namespace

LogModel::LogModel(QObject *parent)
    : QAbstractTableModel(parent)
{
}

void LogModel::reset()
{
    beginResetModel();
    m_rows.clear();
    m_maxLane = 0;
    m_evicted = 0;
    endResetModel();
}

void LogModel::enforceCap()
{
    const int over = m_rows.size() - kMaxRows;
    if (over <= 0) {
        return;
    }
    // Evict the oldest commits — the trailing rows — so the most recent history
    // (and the user's selection/scroll, which anchor near the top) survive. The
    // evicted count keeps loadedCount() monotonic so the next page's "skip"
    // offset still lines up with what git would return below the kept rows.
    const int first = m_rows.size() - over;
    const int last = m_rows.size() - 1;
    beginRemoveRows({}, first, last);
    m_rows.remove(first, over);
    endRemoveRows();
    m_evicted += over;
    // A trimmed-away row may have held the busiest lane; keep the column honest.
    recomputeMaxLane();
}

void LogModel::appendPage(const QVector<UiLogEntry> &page)
{
    if (page.isEmpty()) {
        return;
    }
    const int first = m_rows.size();
    const int last = first + page.size() - 1;
    beginInsertRows({}, first, last);
    m_rows.reserve(first + page.size());
    for (const UiLogEntry &e : page) {
        m_rows.append(e);
        if (e.lane > m_maxLane) {
            m_maxLane = e.lane;
        }
        for (int l : e.lanesIn) {
            if (l > m_maxLane) {
                m_maxLane = l;
            }
        }
        for (int l : e.lanesOut) {
            if (l > m_maxLane) {
                m_maxLane = l;
            }
        }
    }
    endInsertRows();
    enforceCap();
}

void LogModel::recomputeMaxLane()
{
    int maxLane = 0;
    for (const UiLogEntry &e : m_rows) {
        if (e.lane > maxLane) {
            maxLane = e.lane;
        }
        for (int l : e.lanesIn) {
            if (l > maxLane) {
                maxLane = l;
            }
        }
        for (int l : e.lanesOut) {
            if (l > maxLane) {
                maxLane = l;
            }
        }
    }
    m_maxLane = maxLane;
}

bool LogModel::applyHead(const QVector<UiLogEntry> &freshFirstPage)
{
    // An empty fresh page means the history vanished (branch reset to nothing,
    // detached with no commits): only a reset can represent that faithfully.
    if (freshFirstPage.isEmpty()) {
        if (m_rows.isEmpty()) {
            return true; // already empty — nothing to do, no reset needed.
        }
        reset();
        return false;
    }
    // Nothing loaded yet — there's no state to preserve, so just adopt the page
    // as the initial load. appendPage() emits a clean beginInsertRows.
    if (m_rows.isEmpty()) {
        appendPage(freshFirstPage);
        return true;
    }

    // Find how many commits were PREPENDED: the largest k such that the fresh
    // page's tail (from index k) lines up by sha with our current top rows.
    // k == 0 means the tops already share an anchor (no new commits, maybe only
    // decoration changed); k == freshFirstPage.size() with no overlap means the
    // histories diverged.
    const int fresh = freshFirstPage.size();
    int k = -1;
    for (int cand = 0; cand <= fresh; ++cand) {
        // Does freshFirstPage[cand .. cand+n-1] match m_rows[0 .. n-1] by sha,
        // for as many rows as both sides cover within this fresh page?
        const int n = qMin(fresh - cand, m_rows.size());
        if (n <= 0) {
            // The fresh page is entirely new commits sitting on top of ours and
            // its last entry's parent should be our first sha; we can't see that
            // here, so require at least one shared anchor below to be safe.
            continue;
        }
        bool aligned = true;
        for (int i = 0; i < n; ++i) {
            if (freshFirstPage.at(cand + i).sha != m_rows.at(i).sha) {
                aligned = false;
                break;
            }
        }
        if (aligned) {
            k = cand;
            break;
        }
    }

    // No alignment at any offset → genuine divergence (rewrite / force-push /
    // different branch). Reset and let the caller refetch.
    if (k < 0) {
        reset();
        return false;
    }

    // Case A: k new commits were prepended. Insert just those at the top; the
    // existing rows (and the selection/scroll anchored to them) keep their
    // identity and shift down by k.
    if (k > 0) {
        beginInsertRows({}, 0, k - 1);
        // Prepend in one shot, preserving order.
        m_rows = freshFirstPage.mid(0, k) + m_rows;
        endInsertRows();
    }

    // Case B (and the shared tail of case A): decoration may have moved on rows
    // both sides hold. After the prepend the old rows live at [k ..], so compare
    // freshFirstPage[k + i] against m_rows[k + i] and emit dataChanged for any
    // that differ by refs/lanes — this catches a branch ref hopping to another
    // commit, or lane reshuffles, without a new commit object appearing.
    int firstChanged = -1;
    int lastChanged = -1;
    const int compareCount = qMin(fresh - k, m_rows.size() - k);
    for (int i = 0; i < compareCount; ++i) {
        const int row = k + i;
        const UiLogEntry &incoming = freshFirstPage.at(row);
        if (m_rows.at(row) != incoming) {
            m_rows[row] = incoming;
            if (firstChanged < 0) {
                firstChanged = row;
            }
            lastChanged = row;
        }
    }

    // Lanes/refs may have grown or shrunk the busiest row; keep m_maxLane honest
    // so the graph column sizes correctly after the merge.
    recomputeMaxLane();

    if (firstChanged >= 0) {
        Q_EMIT dataChanged(index(firstChanged, 0),
                           index(lastChanged, ColumnCount - 1));
    }
    // A prepend (case A) can push the model over the cap; trim the oldest rows
    // off the bottom so a long-lived viewer can't grow without bound.
    enforceCap();
    return true;
}

QString LogModel::shaAt(int row) const
{
    if (row < 0 || row >= m_rows.size()) {
        return {};
    }
    return m_rows.at(row).sha;
}

QString LogModel::shortShaAt(int row) const
{
    if (row < 0 || row >= m_rows.size()) {
        return {};
    }
    return m_rows.at(row).shortSha;
}

QString LogModel::subjectAt(int row) const
{
    if (row < 0 || row >= m_rows.size()) {
        return {};
    }
    return m_rows.at(row).subject;
}

int LogModel::rowForSha(const QString &sha) const
{
    for (int i = 0; i < m_rows.size(); ++i) {
        if (m_rows.at(i).sha == sha) {
            return i;
        }
    }
    return -1;
}

int LogModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_rows.size();
}

int LogModel::columnCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : ColumnCount;
}

QVariant LogModel::data(const QModelIndex &idx, int role) const
{
    if (!idx.isValid() || idx.row() < 0 || idx.row() >= m_rows.size()) {
        return {};
    }
    const UiLogEntry &e = m_rows.at(idx.row());
    if (role >= ShaRole) {
        switch (role) {
        case ShaRole:      return e.sha;
        case LaneRole:     return e.lane;
        case LanesInRole:  return QVariant::fromValue(e.lanesIn);
        case LanesOutRole: return QVariant::fromValue(e.lanesOut);
        case ParentsRole:  return e.parents;
        case RefsRole:     return e.refs;
        default:           return {};
        }
    }
    if (role == Qt::DisplayRole) {
        switch (idx.column()) {
        case ColGraph:
            // The graph column draws itself; no text is shown but returning
            // an empty string still lets the delegate's paint() run.
            return QString();
        case ColSubject:
            return e.subject;
        case ColAuthor:
            return e.author;
        case ColDate:
            // Relative "3 days ago" so the column reads at a glance and stays
            // narrow; the absolute ISO form lives in the tooltip below.
            return e.authorTime.isValid()
                       ? KFormat().formatRelativeDateTime(
                             e.authorTime.toLocalTime(), QLocale::NarrowFormat)
                       : QString();
        case ColShortSha:
            return e.shortSha;
        default:
            return {};
        }
    }
    if (role == Qt::ToolTipRole) {
        switch (idx.column()) {
        case ColSubject:
            return e.subject;
        case ColAuthor:
            return e.authorEmail.isEmpty()
                       ? e.author
                       : QStringLiteral("%1 <%2>").arg(e.author, e.authorEmail);
        case ColDate:
            return e.authorTime.isValid()
                       ? e.authorTime.toLocalTime().toString(Qt::ISODate)
                       : QString();
        case ColShortSha:
            return e.sha;
        default:
            return {};
        }
    }
    if (role == Qt::TextAlignmentRole && idx.column() == ColShortSha) {
        return int(Qt::AlignRight | Qt::AlignVCenter);
    }
    return {};
}

QVariant LogModel::headerData(int section, Qt::Orientation orientation, int role) const
{
    if (orientation != Qt::Horizontal || role != Qt::DisplayRole) {
        return {};
    }
    switch (section) {
    case ColGraph:    return QString();
    case ColSubject:  return i18nc("git log column", "Subject");
    case ColAuthor:   return i18nc("git log column", "Author");
    case ColDate:     return i18nc("git log column", "Date");
    case ColShortSha: return i18nc("git log column", "Commit");
    default:          return {};
    }
}
