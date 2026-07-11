// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QAbstractListModel>
#include <QJsonArray>
#include <QList>
#include <QString>

// TranscriptModel backs the virtualized chat feed (plan 10, phase 2). One row
// per feed entry — a quiet status note, a role-tagged message, or a collapsible
// tool call. Markdown is rendered to HTML exactly once (on insert) and cached in
// `html`; the delegate paints that HTML via a QTextDocument so the feed is never
// re-parsed on resize. Mutations emit dataChanged for a single row so a change
// re-measures only that row, not the whole transcript.
//
// There is no token-by-token streaming: the core delivers ordered *batches* of
// events, each assistant text block is a whole new message row, and a tool's
// result arrives later keyed by its tool_use id. So the model is append-only
// plus a targeted setToolResult / setExpanded.
//
// The in-RAM feed is capped: once it grows past kMaxRows the oldest rows are
// evicted (the full conversation is always re-readable from the on-disk
// transcript), so a long session cannot grow the model without bound. Because
// eviction shifts every row's position, a deferred reference to a tool row — the
// tool_result that lands after a network round-trip — must use the *stable key*
// returned by the append* calls, never a raw row index.
class TranscriptModel : public QAbstractListModel
{
    Q_OBJECT
public:
    enum Kind {
        Note,    // a quiet, dim/coloured status line
        Message, // a role-tagged card (You / Agent Kate) with an HTML body
        Tool,    // a collapsible tool call
    };

    // Custom roles the delegate reads. Past UserRole to stay clear of Qt's
    // reserved range.
    enum Role {
        KindRole = Qt::UserRole + 1,
        RoleTextRole,  // "You" / "Agent Kate"
        AccentRole,    // hex accent for the role label
        HtmlRole,      // pre-rendered body / note HTML
        PlainRole,     // raw source for copy + search
        ReplayedRole,  // bool — suppress the live timestamp
        TimestampRole, // short local time string
        NoteKindRole,  // colour bucket for notes
        AttachmentsRole, // QJsonArray of compact attachment chips on a You message
        // Tool-specific.
        ToolNameRole,
        ToolSummaryRole,
        ToolDetailRole,    // pretty-printed input JSON
        ToolResultRole,    // shown (possibly truncated) result text
        ToolFullResultRole,
        ToolTruncatedRole, // bool — result was clipped
        ToolDoneRole,      // bool — result has arrived (✓ vs ⋯)
        ToolExpandedRole,  // bool — detail revealed
        ToolVisibleRole,   // bool — honours the showTools setting
        StableIdRole,      // quintptr — per-row identity for the delegate cache
    };

    struct Item {
        Kind kind = Note;
        QString role;
        QString accentHex;
        QString html;
        QString plain;
        bool replayed = false;
        QString timestamp;
        QString noteKind;
        // Compact attachment chips for a You message (name/kind/path/mediaType/
        // outside — never the body). Empty for every other row. Painted as a chip
        // row under the message body; clicking a chip opens the file.
        QJsonArray attachments;
        // Tool fields.
        QString toolName;
        QString toolSummary;
        QString toolDetail;
        QString toolResult;
        QString toolFullResult;
        bool toolTruncated = false;
        bool toolDone = false;
        bool toolExpanded = false;
        bool toolVisible = true;
        // Monotonic identity, bumped on every mutation so the delegate's
        // (id → height) cache misses and re-measures exactly this row.
        quintptr stableId = 0;
    };

    explicit TranscriptModel(QObject *parent = nullptr);

    // --- feed construction (mirrors the old addMessageCard / addNote API) ---
    // Returns a stable key identifying the new item. The key survives row
    // eviction (unlike a row index); hold it to address the row later, e.g. to
    // deliver a tool_result. Numerically equal to the row index until the first
    // eviction.
    int appendMessage(const QString &role, const QString &accentHex,
                      const QString &bodyHtml, const QString &plain, bool replayed,
                      const QString &timestamp,
                      const QJsonArray &attachments = {});
    int appendNote(const QString &html, const QString &noteKind);
    int appendTool(const QString &toolName, const QString &summary,
                   const QString &detail, bool visible);

    // Fill in a tool row's result once its tool_result event arrives. `key` is
    // the stable key from appendTool (a tool_result arrives after a round-trip,
    // by which time eviction may have shifted positions); a no-op if that row
    // has already been evicted.
    void setToolResult(int key, const QString &shown, const QString &fullResult,
                       bool truncated);
    // Expand the truncated result to its full text in place (the old
    // "Show full output" affordance). `row` is a *current* position: these are
    // called synchronously by the delegate from a live QModelIndex, so no
    // eviction can intervene and no key translation is needed.
    void expandToolResult(int row);
    void setExpanded(int row, bool on);
    void setToolsVisible(bool on);

    // Active in-conversation find state. The delegate highlights `needle` and
    // draws a focus ring on `currentRow`. Empty needle clears highlighting.
    void setFind(const QString &needle, int currentRow);
    QString findNeedle() const { return m_findNeedle; }
    int findCurrentRow() const { return m_findRow; }

    const Item &itemAt(int row) const { return m_items.at(row); }
    int count() const { return m_items.size(); }

    // QAbstractListModel
    int rowCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &idx, int role = Qt::DisplayRole) const override;

private:
    // Bump a row's stable id and emit dataChanged so the delegate re-measures it.
    void touched(int row);

    // Translate a stable key (from append*) to the item's current position, or
    // -1 if the row has since been evicted.
    int rowForKey(int key) const;
    // Evict oldest rows once the feed grows past kMaxRows, keeping in-RAM size
    // bounded. Called after every append.
    void enforceCap();

    QList<Item> m_items;
    quintptr m_nextId = 1;
    // Number of rows evicted off the front so far; the absolute key of m_items[0]
    // is m_base. key - m_base == current row index.
    int m_base = 0;
    QString m_findNeedle;
    int m_findRow = -1;
};
