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
};

// LogModel is the table model behind the log viewer's QTreeView. Columns are
// fixed (Graph, Subject, Author, Date, ShortSha); the graph column carries no
// display text — it exists so the LogGraphDelegate has a place to paint, and
// its sizeHint width tracks the widest lane count seen so far.
//
// Pagination is append-only: appendPage() tacks a new page onto the bottom,
// reset() clears everything (used on refresh / branch switch / path filter).
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

    // Largest lane index seen so far across all rows — the graph delegate uses
    // it to size its column wide enough to fit the busiest row in the page.
    int maxLane() const { return m_maxLane; }

    // Total rows currently loaded. The viewer treats this as the next "skip"
    // offset when requesting the next page.
    int loadedCount() const { return m_rows.size(); }

    QString shaAt(int row) const;
    int rowForSha(const QString &sha) const;

    // QAbstractTableModel
    int rowCount(const QModelIndex &parent = {}) const override;
    int columnCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &idx, int role = Qt::DisplayRole) const override;
    QVariant headerData(int section, Qt::Orientation orientation,
                        int role = Qt::DisplayRole) const override;

private:
    QVector<UiLogEntry> m_rows;
    int m_maxLane = 0;
};
