// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QHash>
#include <QStyledItemDelegate>

class QFileSystemModel;
class QAbstractItemModel;

// GitStatusDelegate paints column 0 of the project tree, layering a small VCS
// emblem and a subtle text tint over the Breeze file/folder icon to reflect
// each entry's git status. It owns no data: ProjectTree feeds it an absolute
// path → status map (rolled up so directories show the strongest child state),
// and the delegate resolves the row's absolute path back through the file
// system model. All colours come from QPalette roles, never hard-coded, so the
// active Breeze colour scheme is honoured.
class GitStatusDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    // Status codes mirror gitstatus/status.go, kept as a small enum so the map
    // is cheap to build and roll up by max().
    enum Status {
        Clean = 0,
        Untracked = 1,
        Added = 2,
        Renamed = 3,
        Modified = 4,
        Deleted = 5,
        Conflicted = 6,
    };

    explicit GitStatusDelegate(QFileSystemModel *fsModel, QObject *parent = nullptr);

    // Replace the path → status map. Keys are absolute, native-separated paths.
    void setStatuses(QHash<QString, int> statuses);
    bool hasStatuses() const { return !m_statuses.isEmpty(); }

    void paint(QPainter *painter, const QStyleOptionViewItem &option,
               const QModelIndex &index) const override;

private:
    int statusForIndex(const QModelIndex &index) const;

    QFileSystemModel *m_fsModel = nullptr;
    QHash<QString, int> m_statuses;
};
