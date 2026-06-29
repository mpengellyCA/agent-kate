// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QAbstractTableModel>
#include <QDateTime>
#include <QStringList>
#include <QVector>

// One row of the log model. Field names mirror the JSON shape of
// gitstatus.LogEntry on the wire so reading from the RPC is a straight copy.
struct UiLogEntry {
    QString sha;
    QString shortSha;
    QString subject;
    QString author;
    QString authorEmail;
    QDateTime authorTime;
    QStringList parents;
    QStringList refs;
    int lane = 0;
    QList<int> lanesIn;
    QList<int> lanesOut;

    // Two rows are "the same" for diffing purposes when they point at the same
    // commit AND carry the same visible decoration. The sha identifies the
    // commit; refs (branch/tag chips) and the graph lanes are the only other
    // fields a HEAD move can change without rewriting the commit itself, so a
    // change in any of them must repaint the row. Author/subject/date are
    // immutable for a given sha, so they don't need to be compared.
    bool operator==(const UiLogEntry &o) const
    {
        return sha == o.sha && shortSha == o.shortSha && refs == o.refs
            && lane == o.lane && lanesIn == o.lanesIn && lanesOut == o.lanesOut;
    }
    bool operator!=(const UiLogEntry &o) const { return !(*this == o); }
};

// LogModel is the table model behind the log viewer's QTreeView. Columns are
// fixed (Graph, Subject, Author, Date, ShortSha); the graph column carries no
// display text — it exists so the LogGraphDelegate has a place to paint, and
// its sizeHint width tracks the widest lane count seen so far.
//
// Pagination is append-only: appendPage() tacks a new page onto the bottom,
// reset() clears everything (used on refresh / branch switch / path filter).
// applyHead() merges a fresh first page in place on a HEAD move so the user's
// selection and scroll position survive — see its declaration below.
class LogModel : public QAbstractTableModel
{
    Q_OBJECT
public:
    enum Column {
        ColGraph = 0,
        ColSubject,
        ColAuthor,
        ColDate,
        ColShortSha,
        ColumnCount,
    };

    // Custom roles let the delegates pull structured fields without restringing
    // them through Qt::DisplayRole. Numbers start past UserRole to stay clear
    // of Qt's reserved range.
    enum Role {
        ShaRole = Qt::UserRole + 1,
        LaneRole,
        LanesInRole,   // QList<int>
        LanesOutRole,  // QList<int>
        ParentsRole,   // QStringList
        RefsRole,      // QStringList
    };

    explicit LogModel(QObject *parent = nullptr);

    void reset();
    void appendPage(const QVector<UiLogEntry> &page);

    // applyHead merges a freshly fetched first page into the already-loaded
    // history WITHOUT a full reset, so selection / scroll / loaded pages and the
    // detail panel all survive a HEAD move. It distinguishes three cases:
    //
    //   * Commits were prepended (the common case — a new commit on top): the
    //     existing top rows are a suffix of the fresh page, so only the k new
    //     leading commits are inserted via beginInsertRows(0, k-1) and every
    //     other row is left untouched.
    //   * Only decoration moved (refs/lanes differ but the shas line up): emit
    //     dataChanged() for just the affected rows.
    //   * Histories genuinely diverged (a rewrite/force-push, or the fresh page
    //     shares no anchor with what we hold): fall back to a full reset, then
    //     the caller refetches.
    //
    // Returns true if the merge was applied in place; false if it fell back to a
    // reset (the caller should then refetch from the first page).
    bool applyHead(const QVector<UiLogEntry> &freshFirstPage);

    // Largest lane index seen so far across all rows — the graph delegate uses
    // it to size its column wide enough to fit the busiest row in the page.
    int maxLane() const { return m_maxLane; }

    // Total commits paged in so far. The viewer treats this as the next "skip"
    // offset when requesting the next page, so it must stay monotonic even after
    // the cap evicts the oldest rows off the bottom — hence the +m_evicted.
    int loadedCount() const { return m_rows.size() + m_evicted; }

    QString shaAt(int row) const;
    QString shortShaAt(int row) const;
    QString subjectAt(int row) const;
    int rowForSha(const QString &sha) const;

    // QAbstractTableModel
    int rowCount(const QModelIndex &parent = {}) const override;
    int columnCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &idx, int role = Qt::DisplayRole) const override;
    QVariant headerData(int section, Qt::Orientation orientation,
                        int role = Qt::DisplayRole) const override;

private:
    // Recompute m_maxLane from scratch over all rows. Cheap enough for the
    // first-page-sized scans applyHead() does, and avoids m_maxLane drifting
    // when lanes change in place rather than only growing on append.
    void recomputeMaxLane();

    // Evict the oldest rows (the bottom of the list) once m_rows grows past
    // kMaxRows, emitting beginRemoveRows/endRemoveRows so views stay consistent
    // and bumping m_evicted so loadedCount() stays monotonic. See the cap note
    // in the .cpp. Mirrors TranscriptModel::enforceCap().
    void enforceCap();

    QVector<UiLogEntry> m_rows;
    int m_maxLane = 0;
    // Count of oldest rows trimmed off the bottom by the cap. Kept so the page
    // "skip" offset (loadedCount()) doesn't rewind onto already-evicted commits.
    int m_evicted = 0;
};
