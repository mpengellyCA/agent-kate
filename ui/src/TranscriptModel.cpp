// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TranscriptModel.h"

namespace {
// The in-RAM feed is capped at this many rows; once it grows past the cap the
// oldest rows are evicted (the full conversation stays on disk and reloads on
// reopen). Generous enough that ordinary sessions never reach it, low enough
// that a marathon session cannot grow the model into an OOM.
constexpr int kMaxRows = 5000;
} // namespace

TranscriptModel::TranscriptModel(QObject *parent)
    : QAbstractListModel(parent)
{
}

int TranscriptModel::rowForKey(int key) const
{
    const int row = key - m_base;
    return (row >= 0 && row < m_items.size()) ? row : -1;
}

void TranscriptModel::enforceCap()
{
    const int over = m_items.size() - kMaxRows;
    if (over <= 0) {
        return;
    }
    beginRemoveRows({}, 0, over - 1);
    m_items.remove(0, over);
    endRemoveRows();
    m_base += over;
}

int TranscriptModel::appendMessage(const QString &role, const QString &accentHex,
                                   const QString &bodyHtml, const QString &plain,
                                   bool replayed, const QString &timestamp)
{
    Item it;
    it.kind = Message;
    it.role = role;
    it.accentHex = accentHex;
    it.html = bodyHtml;
    it.plain = plain;
    it.replayed = replayed;
    it.timestamp = timestamp;
    it.stableId = m_nextId++;
    const int row = m_items.size();
    const int key = m_base + row;
    beginInsertRows({}, row, row);
    m_items.append(it);
    endInsertRows();
    enforceCap();
    return key;
}

int TranscriptModel::appendNote(const QString &html, const QString &noteKind)
{
    Item it;
    it.kind = Note;
    it.html = html;
    it.noteKind = noteKind;
    it.stableId = m_nextId++;
    const int row = m_items.size();
    const int key = m_base + row;
    beginInsertRows({}, row, row);
    m_items.append(it);
    endInsertRows();
    enforceCap();
    return key;
}

int TranscriptModel::appendTool(const QString &toolName, const QString &summary,
                                const QString &detail, bool visible)
{
    Item it;
    it.kind = Tool;
    it.toolName = toolName;
    it.toolSummary = summary;
    it.toolDetail = detail.trimmed();
    it.toolVisible = visible;
    it.stableId = m_nextId++;
    const int row = m_items.size();
    const int key = m_base + row;
    beginInsertRows({}, row, row);
    m_items.append(it);
    endInsertRows();
    enforceCap();
    return key;
}

void TranscriptModel::setToolResult(int key, const QString &shown,
                                    const QString &fullResult, bool truncated)
{
    const int row = rowForKey(key);
    if (row < 0) {
        return; // the tool row scrolled out of the in-RAM window before its result landed
    }
    Item &it = m_items[row];
    it.toolResult = shown;
    it.toolFullResult = fullResult;
    it.toolTruncated = truncated;
    it.toolDone = true;
    touched(row);
}

void TranscriptModel::expandToolResult(int row)
{
    if (row < 0 || row >= m_items.size()) {
        return;
    }
    Item &it = m_items[row];
    it.toolResult = it.toolFullResult;
    it.toolTruncated = false;
    touched(row);
}

void TranscriptModel::setExpanded(int row, bool on)
{
    if (row < 0 || row >= m_items.size() || m_items.at(row).toolExpanded == on) {
        return;
    }
    m_items[row].toolExpanded = on;
    touched(row);
}

void TranscriptModel::setToolsVisible(bool on)
{
    bool any = false;
    for (int i = 0; i < m_items.size(); ++i) {
        Item &it = m_items[i];
        if (it.kind == Tool && it.toolVisible != on) {
            it.toolVisible = on;
            // Bump id so the delegate re-measures (collapsed-to-0 height).
            it.stableId = m_nextId++;
            any = true;
        }
    }
    if (any && !m_items.isEmpty()) {
        emit dataChanged(index(0), index(m_items.size() - 1));
    }
}

void TranscriptModel::setFind(const QString &needle, int currentRow)
{
    if (needle == m_findNeedle && currentRow == m_findRow) {
        return;
    }
    m_findNeedle = needle;
    m_findRow = currentRow;
    // Highlighting is a paint-time concern; it does not change row geometry, so
    // no stable-id bump (heights are unchanged). Repaint every row.
    if (!m_items.isEmpty()) {
        emit dataChanged(index(0), index(m_items.size() - 1),
                         {Qt::DisplayRole});
    }
}

void TranscriptModel::touched(int row)
{
    m_items[row].stableId = m_nextId++;
    const QModelIndex idx = index(row);
    emit dataChanged(idx, idx);
}

int TranscriptModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_items.size();
}

QVariant TranscriptModel::data(const QModelIndex &idx, int role) const
{
    if (!idx.isValid() || idx.row() < 0 || idx.row() >= m_items.size()) {
        return {};
    }
    const Item &it = m_items.at(idx.row());
    switch (role) {
    case KindRole:
        return int(it.kind);
    case RoleTextRole:
        return it.role;
    case AccentRole:
        return it.accentHex;
    case HtmlRole:
        return it.html;
    case PlainRole:
        return it.plain;
    case ReplayedRole:
        return it.replayed;
    case TimestampRole:
        return it.timestamp;
    case NoteKindRole:
        return it.noteKind;
    case ToolNameRole:
        return it.toolName;
    case ToolSummaryRole:
        return it.toolSummary;
    case ToolDetailRole:
        return it.toolDetail;
    case ToolResultRole:
        return it.toolResult;
    case ToolFullResultRole:
        return it.toolFullResult;
    case ToolTruncatedRole:
        return it.toolTruncated;
    case ToolDoneRole:
        return it.toolDone;
    case ToolExpandedRole:
        return it.toolExpanded;
    case ToolVisibleRole:
        return it.toolVisible;
    case StableIdRole:
        return QVariant::fromValue<quintptr>(it.stableId);
    default:
        return {};
    }
}
