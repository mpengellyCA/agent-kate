// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QSortFilterProxyModel>

class QFileSystemModel;

// FileFilterProxyModel wraps the project tree's QFileSystemModel to provide a
// recursive name filter: a row survives if its own name matches OR any
// descendant matches, so matching files stay reachable through their parent
// folders. With an empty pattern it is a transparent pass-through. The root
// index is always kept so the tree's rootIndex remains valid.
class FileFilterProxyModel : public QSortFilterProxyModel
{
    Q_OBJECT
public:
    explicit FileFilterProxyModel(QFileSystemModel *source, QObject *parent = nullptr);

    void setFilterText(const QString &text);
    bool isFiltering() const { return !m_pattern.isEmpty(); }

protected:
    bool filterAcceptsRow(int sourceRow, const QModelIndex &sourceParent) const override;

private:
    bool nameMatches(const QModelIndex &sourceIndex) const;
    bool anyDescendantMatches(const QModelIndex &sourceIndex) const;

    QFileSystemModel *m_fs = nullptr;
    QString m_pattern;
};
