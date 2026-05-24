// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "LogModel.h"

#include <QLocale>
#include <QVariant>

LogModel::LogModel(QObject *parent)
    : QAbstractTableModel(parent)
{
}

void LogModel::reset()
{
    beginResetModel();
    m_rows.clear();
    m_maxLane = 0;
    endResetModel();
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
}

QString LogModel::shaAt(int row) const
{
    if (row < 0 || row >= m_rows.size()) {
        return {};
    }
    return m_rows.at(row).sha;
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
            // Locale-aware short date+time so the column stays narrow but
            // still useful at a glance. Tooltip carries the ISO form.
            return e.authorTime.isValid()
                       ? QLocale().toString(e.authorTime.toLocalTime(),
                                            QLocale::ShortFormat)
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
    case ColSubject:  return tr("Subject");
    case ColAuthor:   return tr("Author");
    case ColDate:     return tr("Date");
    case ColShortSha: return tr("Commit");
    default:          return {};
    }
}
