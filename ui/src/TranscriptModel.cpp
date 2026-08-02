// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TranscriptModel.h"

#include <QJsonObject>
#include <QStringList>
#include <QTextDocumentFragment>

namespace {
// The in-RAM feed is capped at this many rows; once it grows past the cap the
// oldest rows are evicted (the full conversation stays on disk and reloads on
// reopen). Generous enough that ordinary sessions never reach it, low enough
// that a marathon session cannot grow the model into an OOM.
constexpr int kMaxRows = 5000;

// A note's words, recovered from the rendered line the panel handed us. Notes
// are authored as small HTML fragments (escaped text plus glyph entities and
// the odd <b>/<tt>), so their searchable/copyable form has to come back through
// an HTML parse — Qt's own, never a hand-rolled tag stripper (audit F16). Run
// once per note at append, not per keystroke of the find bar.
QString notePlainText(const QString &html)
{
    if (!html.contains(QLatin1Char('<')) && !html.contains(QLatin1Char('&'))) {
        return html; // nothing to unescape — the common case, and free
    }
    return QTextDocumentFragment::fromHtml(html).toPlainText();
}
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
                                   bool replayed, const QString &timestamp,
                                   const QJsonArray &attachments)
{
    Item it;
    it.kind = Message;
    it.role = role;
    it.accentHex = accentHex;
    it.html = bodyHtml;
    it.plain = plain;
    it.replayed = replayed;
    it.timestamp = timestamp;
    it.attachments = attachments;
    it.stableId = m_nextId++;
    const int row = m_items.size();
    const int key = m_base + row;
    beginInsertRows({}, row, row);
    m_items.append(it);
    endInsertRows();
    enforceCap();
    return key;
}

int TranscriptModel::appendNote(const QString &html, const QString &noteKind,
                                const QString &timestamp)
{
    Item it;
    it.kind = Note;
    it.html = html;
    it.plain = notePlainText(html);
    it.noteKind = noteKind;
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

int TranscriptModel::appendThinking(const QString &bodyHtml, const QString &plain,
                                    const QString &preview)
{
    Item it;
    it.kind = Thinking;
    it.html = bodyHtml;
    it.plain = plain;
    it.toolSummary = preview;
    it.stableId = m_nextId++;
    const int row = m_items.size();
    const int key = m_base + row;
    beginInsertRows({}, row, row);
    m_items.append(it);
    endInsertRows();
    enforceCap();
    return key;
}

int TranscriptModel::appendChecklist(const QJsonArray &items)
{
    Item it;
    it.kind = Checklist;
    it.checklist = items;
    it.stableId = m_nextId++;
    const int row = m_items.size();
    const int key = m_base + row;
    beginInsertRows({}, row, row);
    m_items.append(it);
    endInsertRows();
    enforceCap();
    return key;
}

bool TranscriptModel::setChecklist(int key, const QJsonArray &items)
{
    const int row = rowForKey(key);
    if (row < 0 || m_items.at(row).kind != Checklist) {
        return false; // evicted (or the key is stale) — caller appends anew
    }
    m_items[row].checklist = items;
    touched(row);
    return true;
}

bool TranscriptModel::setMessageBody(int key, const QString &bodyHtml,
                                     const QString &plain)
{
    const int row = rowForKey(key);
    if (row < 0 || m_items.at(row).kind != Message) {
        return false; // evicted (or the key is stale) — caller appends anew
    }
    Item &it = m_items[row];
    it.html = bodyHtml;
    it.plain = plain;
    touched(row);
    return true;
}

void TranscriptModel::setToolProgress(int key, const QString &shown)
{
    const int row = rowForKey(key);
    if (row < 0 || m_items.at(row).kind != Tool) {
        return;
    }
    Item &it = m_items[row];
    it.toolResult = shown;
    // Deliberately NOT toolDone: the tool is still running, and the real
    // tool_result has not arrived. toolFullResult stays empty so the
    // "show full output" affordance only appears once there is a full output.
    touched(row);
}

void TranscriptModel::setToolResult(int key, const QString &shown,
                                    const QString &fullResult, bool truncated,
                                    bool isError)
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
    it.toolError = isError;
    touched(row);
}

void TranscriptModel::setToolAttachments(int key, const QJsonArray &attachments)
{
    const int row = rowForKey(key);
    if (row < 0 || m_items.at(row).kind != Tool) {
        return;
    }
    m_items[row].attachments = attachments;
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
            // Drop the delegate's entry so it re-measures (collapsed-to-0 height).
            emit heightInvalidated(it.stableId);
            any = true;
        }
    }
    if (any && !m_items.isEmpty()) {
        emit dataChanged(index(0), index(m_items.size() - 1));
    }
}

QString TranscriptModel::searchText(int row) const
{
    if (row < 0 || row >= m_items.size()) {
        return {};
    }
    const Item &it = m_items.at(row);
    switch (it.kind) {
    case Message:
    case Note:
        return it.plain;
    case Thinking:
        // The preview is what a collapsed row shows; the body is what it holds.
        return it.toolSummary + QLatin1Char('\n') + it.plain;
    case Tool: {
        // The full result, not the clipped one: a user searching for an error
        // string is looking for it in the output, and the shown copy stops at
        // the display clip.
        QStringList parts{it.toolName, it.toolSummary, it.toolDetail};
        parts << (it.toolFullResult.isEmpty() ? it.toolResult : it.toolFullResult);
        return parts.join(QLatin1Char('\n'));
    }
    case Checklist: {
        QStringList parts;
        for (const QJsonValue &v : it.checklist) {
            parts << v.toObject().value(QStringLiteral("content")).toString();
        }
        return parts.join(QLatin1Char('\n'));
    }
    }
    return {};
}

void TranscriptModel::setFind(const QString &needle, int currentRow)
{
    if (needle == m_findNeedle && currentRow == m_findRow) {
        return;
    }
    const QString oldNeedle = m_findNeedle;
    m_findNeedle = needle;
    m_findRow = currentRow;
    if (m_items.isEmpty()) {
        return;
    }
    // A Message or Note row whose plain text matches the needle is rendered as
    // highlighted PLAIN text rather than its rendered HTML, and the two have
    // different heights. So a row whose match state flips between the old and new
    // needle must be re-measured: invalidate the delegate's height-cache entry
    // for it. (Rows that match both needles, or other kinds, keep the same
    // height — a pure highlight/currentRow change is paint-only.)
    bool geometryChanged = false;
    if (oldNeedle != needle) {
        const auto matches = [](const QString &text, const QString &n) {
            return !n.isEmpty() && text.contains(n, Qt::CaseInsensitive);
        };
        for (int i = 0; i < m_items.size(); ++i) {
            if (m_items[i].kind != Message && m_items[i].kind != Note) {
                continue;
            }
            if (matches(m_items[i].plain, oldNeedle) != matches(m_items[i].plain, needle)) {
                emit heightInvalidated(m_items.at(i).stableId);
                geometryChanged = true;
            }
        }
    }
    // A role-less dataChanged makes the view re-query sizeHint (re-measuring the
    // rows whose stable id we bumped); restrict to a repaint when only the
    // highlight/current-match moved and no row changed height.
    if (geometryChanged) {
        emit dataChanged(index(0), index(m_items.size() - 1));
    } else {
        emit dataChanged(index(0), index(m_items.size() - 1), {Qt::DisplayRole});
    }
}

// touched invalidates one row's cached geometry. It deliberately does NOT mint a
// new stable id: a streamed message is touched every 50ms flush tick, and a fresh
// id each time left the delegate's (id → height) cache holding one dead entry per
// tick — a slow leak evicted only by wiping the whole cache. Telling the delegate
// "this exact row changed" costs one signal and keeps the id a real identity.
void TranscriptModel::touched(int row)
{
    emit heightInvalidated(m_items.at(row).stableId);
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
    case AttachmentsRole:
        return it.attachments;
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
    case ToolErrorRole:
        return it.toolError;
    case ToolExpandedRole:
        return it.toolExpanded;
    case ToolVisibleRole:
        return it.toolVisible;
    case ChecklistRole:
        return it.checklist;
    case StableIdRole:
        return QVariant::fromValue<quintptr>(it.stableId);
    default:
        return {};
    }
}
