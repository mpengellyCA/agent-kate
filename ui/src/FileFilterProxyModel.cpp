// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "FileFilterProxyModel.h"

#include <QFileSystemModel>

FileFilterProxyModel::FileFilterProxyModel(QFileSystemModel *source, QObject *parent)
    : QSortFilterProxyModel(parent)
    , m_fs(source)
{
    setSourceModel(source);
    setFilterCaseSensitivity(Qt::CaseInsensitive);
    setRecursiveFilteringEnabled(true);
}

void FileFilterProxyModel::setFilterText(const QString &text)
{
    const QString trimmed = text.trimmed();
    if (trimmed == m_pattern) {
        return;
    }
    beginFilterChange();
    m_pattern = trimmed;
    endFilterChange(QSortFilterProxyModel::Direction::Rows);
}

bool FileFilterProxyModel::nameMatches(const QModelIndex &sourceIndex) const
{
    if (m_pattern.isEmpty() || !m_fs) {
        return true;
    }
    const QString name = m_fs->fileName(sourceIndex);
    return name.contains(m_pattern, Qt::CaseInsensitive);
}

bool FileFilterProxyModel::anyDescendantMatches(const QModelIndex &sourceIndex) const
{
    if (!m_fs || !m_fs->isDir(sourceIndex)) {
        return false;
    }
    const int rows = m_fs->rowCount(sourceIndex);
    for (int r = 0; r < rows; ++r) {
        const QModelIndex child = m_fs->index(r, 0, sourceIndex);
        if (nameMatches(child) || anyDescendantMatches(child)) {
            return true;
        }
    }
    return false;
}

bool FileFilterProxyModel::filterAcceptsRow(int sourceRow,
                                            const QModelIndex &sourceParent) const
{
    if (m_pattern.isEmpty() || !m_fs) {
        return true;
    }
    const QModelIndex idx = m_fs->index(sourceRow, 0, sourceParent);
    if (!idx.isValid()) {
        return true;
    }
    // A directory survives if it (or any lazily-loaded descendant) matches; a
    // file survives only on its own name. setRecursiveFilteringEnabled also
    // keeps ancestors of matching children visible, but QFileSystemModel's lazy
    // population means children may not be loaded yet — so we additionally walk
    // whatever is already fetched. The model fetches more as folders expand,
    // and ProjectTree re-applies the filter on directoryLoaded.
    if (nameMatches(idx)) {
        return true;
    }
    return anyDescendantMatches(idx);
}
