// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QByteArray>
#include <QDialog>
#include <QString>

class QTextBrowser;
class QFileSystemWatcher;
class QTimer;

// SubAgentTranscriptDialog renders one workflow sub-agent's transcript
// (agent-<id>.jsonl) as a readable chat, rather than dumping the raw JSON lines.
//
// A workflow's sub-agents write standard stream-json transcripts, so this parses
// the same shape the main feed does — assistant text (Markdown), tool calls, and
// tool results — and lays them out as a scrollable conversation.
//
// The file is append-only and grows while the sub-agent runs, so the dialog
// tails it live: a QFileSystemWatcher (plus a poll fallback) re-reads only the
// bytes appended since last time and appends their rendered blocks, staying
// pinned to the bottom when the reader is already there. Non-modal (show(),
// WA_DeleteOnClose) with its size remembered in KConfig.
class SubAgentTranscriptDialog : public QDialog
{
    Q_OBJECT
public:
    // `jsonlPath` is the sub-agent transcript file; `label` names the agent for
    // the window title (falls back to the file name when empty).
    SubAgentTranscriptDialog(const QString &jsonlPath, const QString &label,
                             QWidget *parent = nullptr);
    ~SubAgentTranscriptDialog() override;

private:
    // Read the bytes appended since the last read and append their rendered
    // chat blocks. Handles a truncated/rotated file by reloading from scratch.
    void pullNew();

    // Drop blocks off the FRONT until the document is back inside both caps
    // (block count and character count). Called after every append.
    void trimDocument();

    QTextBrowser *m_browser = nullptr;
    QString m_path;
    qint64 m_offset = 0;      // bytes already consumed from the file
    QByteArray m_partial;     // trailing bytes past the last newline (incomplete line)
    // The read is bounded (see the caps in the .cpp), so it can legitimately
    // land mid-line: `m_resync` means "discard through the next newline before
    // parsing anything", `m_skipped` means "tell the reader bytes were dropped".
    bool m_resync = false;
    bool m_skipped = false;
    QFileSystemWatcher *m_watcher = nullptr;
    QTimer *m_poll = nullptr;
};
