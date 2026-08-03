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
        Note,     // a quiet, dim/coloured status line
        Message,  // a role-tagged card (You / Agent Kate) with an HTML body
        Tool,     // a collapsible tool call
        Thinking, // the model's reasoning — collapsed header, expandable body
        Checklist, // the agent's plan / todo list, updated in place
    };

    // Message presentation is semantic. Labels and colours are resolved by the
    // delegate/theme instead of callers relying on a display-string convention.
    enum class Speaker { User, Agent };
    enum class MessageRunPosition { Single, First, Middle, Last };

    // Custom roles the delegate reads. Past UserRole to stay clear of Qt's
    // reserved range.
    enum Role {
        KindRole = Qt::UserRole + 1,
        RoleTextRole,  // display label, derived from Speaker for accessibility
        SpeakerRole,
        MessageRunPositionRole,
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
        ToolErrorRole,     // bool — the result was a FAILURE (✗ vs ✓)
        ToolExpandedRole,  // bool — detail revealed
        ToolVisibleRole,   // bool — honours the showTools setting
        ChecklistRole,     // QJsonArray of {content, status} items
        StableIdRole,      // quintptr — per-row identity for the delegate cache
    };

    struct Item {
        Kind kind = Note;
        Speaker speaker = Speaker::Agent;
        MessageRunPosition runPosition = MessageRunPosition::Single;
        QString html;
        QString plain;
        bool replayed = false;
        QString timestamp;
        QString noteKind;
        // Compact attachment chips for a You message (name/kind/path/mediaType/
        // outside — never the body). Empty for every other row. Painted as a chip
        // row under the message body; clicking a chip opens the file.
        QJsonArray attachments;
        // Tool fields. A Thinking row reuses toolSummary (the one-line
        // preview) and toolExpanded (whether the body is revealed); its body
        // lives in html/plain like a message.
        QString toolName;
        QString toolSummary;
        QString toolDetail;
        QString toolResult;
        QString toolFullResult;
        bool toolTruncated = false;
        bool toolDone = false;
        // The tool_result carried is_error (audit F40). A failure used to paint
        // exactly like a success, so finding the one tool that failed in a long
        // turn meant expanding every row.
        bool toolError = false;
        bool toolExpanded = false;
        bool toolVisible = true;
        // Checklist items ({content, status} objects, status one of
        // pending / in_progress / completed). Empty for every other row.
        QJsonArray checklist;
        // Lowercased searchText, built once and cached (audit F58): find runs
        // per keystroke over every row, and a Tool row's searchText re-joined
        // up to 128 KB of retained result on every call. Invalidated through
        // touched() — the same seam that busts the delegate's height cache.
        mutable QString searchLower;
        mutable bool searchLowerValid = false;
        // Whether this row matches the active find needle. Kept only for the
        // kinds the delegate renders highlighted (Message/Note); maintained by
        // setFind / touched / append so neither setFind nor the delegate has to
        // re-scan the row's text to answer it.
        bool findMatched = false;
        // Per-row identity, assigned once at append and STABLE for the life of
        // the row. A mutation does not mint a new one: it emits
        // heightInvalidated(stableId) instead, so the delegate drops exactly
        // this row's measurement/layout entry rather than leaving a dead one
        // behind on every streaming tick.
        quintptr stableId = 0;
    };

    explicit TranscriptModel(QObject *parent = nullptr);

    // --- feed construction (mirrors the old addMessageCard / addNote API) ---
    // Returns a stable key identifying the new item. The key survives row
    // eviction (unlike a row index); hold it to address the row later, e.g. to
    // deliver a tool_result. Numerically equal to the row index until the first
    // eviction.
    int appendMessage(Speaker speaker, const QString &bodyHtml, const QString &plain,
                      bool replayed,
                      const QString &timestamp,
                      const QJsonArray &attachments = {});
    // A status note. `html` is the rendered line; a plain-text form is derived
    // from it once, here, so find and "Copy text" can reach a note's words —
    // every error, compaction and rate-limit line lives in one (audit F48).
    // `timestamp` is the short local time shown at the row's right edge; empty
    // for a replayed note, which has no honest live time to show.
    int appendNote(const QString &html, const QString &noteKind,
                   const QString &timestamp = QString());
    int appendTool(const QString &toolName, const QString &summary,
                   const QString &detail, bool visible);
    // A thinking card: collapsed to `preview` until expanded to the rendered
    // `bodyHtml` (plain kept for copy).
    int appendThinking(const QString &bodyHtml, const QString &plain,
                       const QString &preview);
    // The agent's plan. `items` is an array of {content, status} objects. A
    // new plan usually *updates* the existing card via setChecklist.
    int appendChecklist(const QJsonArray &items);
    // Replace a checklist card's items in place (the plan evolved). Returns
    // false if the row was evicted — append a fresh card instead.
    bool setChecklist(int key, const QJsonArray &items);

    // Replace a message row's body in place. Two callers, both from the
    // token-by-token stream: each coalesced batch of text deltas re-renders the
    // provisional row, and the authoritative `assistant` event overwrites it
    // with the final text rather than appending a duplicate card. Returns false
    // if the row was evicted — the caller appends a fresh card instead.
    bool setMessageBody(int key, const QString &bodyHtml, const QString &plain);

    // Show partial result text on a tool row WITHOUT marking it done: live
    // subagent text forwarded under a Task tool's parent_tool_use_id, which
    // keeps arriving until the real tool_result lands and calls setToolResult.
    void setToolProgress(int key, const QString &shown);

    // Fill in a tool row's result once its tool_result event arrives. `key` is
    // the stable key from appendTool (a tool_result arrives after a round-trip,
    // by which time eviction may have shifted positions); a no-op if that row
    // has already been evicted.
    // `isError` is the tool_result's own is_error flag (both engines set it —
    // claude natively, kimi's translator on a failed tool call).
    void setToolResult(int key, const QString &shown, const QString &fullResult,
                       bool truncated, bool isError = false);
    // Attach image chips to a tool row (a tool_result carrying image blocks —
    // e.g. a screenshot). Chips are {name, kind, path} like a message's; the
    // delegate paints them under the tool header.
    void setToolAttachments(int key, const QJsonArray &attachments);
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

    // Everything this row shows the user, as one searchable string. Find used
    // to scan Message prose only (audit F48), so tool names, paths, commands,
    // results, reasoning and — worst — NOTES were invisible to it: every error,
    // compaction, rate-limit and API-failure line lives in a note, and
    // searching for the error text on screen answered "No matches".
    QString searchText(int row) const;

    // searchText lowercased, cached on the row (audit F58). Find's per-keystroke
    // scan goes through this so a keystroke costs one contains() per row over an
    // already-built string, never a fresh join of a Tool row's retained result.
    QString searchTextLower(int row) const;

    // Whether the row's body is drawn highlighted for the active find needle —
    // the cached flag setFind maintains, so the delegate does not re-scan the
    // row's plain text on every paint. False for kinds find never highlights.
    bool findMatch(int row) const;

Q_SIGNALS:
    // A row's content changed in a way that can change its measured height or
    // its laid-out body: the delegate must drop whatever it cached for this
    // stable id. Emitted alongside (just before) the dataChanged that makes the
    // view re-query sizeHint. The delegate binds to this itself the first time
    // it measures a row of this model, so no external wiring is required.
    void heightInvalidated(quintptr stableId);

public:
    // QAbstractListModel
    int rowCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &idx, int role = Qt::DisplayRole) const override;

private:
    // Invalidate a row's cached measurement and emit dataChanged so the delegate
    // re-measures it. The row's stable id does NOT change.
    void touched(int row);

    // Recompute one row's cached find-match flag against the active needle.
    // Returns true when the flag flipped (the row's rendered form — highlighted
    // plain text vs its HTML — and therefore its height changed).
    bool updateFindMatch(int row);

    // Translate a stable key (from append*) to the item's current position, or
    // -1 if the row has since been evicted.
    int rowForKey(int key) const;
    // Evict oldest rows once the feed grows past kMaxRows, keeping in-RAM size
    // bounded. Called after every append.
    void enforceCap();
    void normalizeFirstMessageRun();

    QList<Item> m_items;
    quintptr m_nextId = 1;
    // Number of rows evicted off the front so far; the absolute key of m_items[0]
    // is m_base. key - m_base == current row index.
    int m_base = 0;
    QString m_findNeedle;
    // m_findNeedle lowercased once per needle change, so the per-row match test
    // is a case-sensitive contains over the cached lowercased search text.
    QString m_findNeedleLower;
    int m_findRow = -1;
};
