// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QJsonObject>
#include <QString>

class QPlainTextEdit;
class QLineEdit;

// ToolInspectorDialog is the comfortable, full-size reader for a single tool call
// (plan 13 phase 5). Tool rows in the transcript only expand to a truncated,
// mono-spaced dump; this modal gives the input and result room to breathe and
// makes an Edit read as a real diff.
//
// It is non-modal (show(), WA_DeleteOnClose) so the user can keep reading the
// chat with the inspector open, and it remembers its size in KConfig. Three tabs:
//   - Overview: a tool-aware summary driven by a small registry keyed on the tool
//     name (Bash → command + console output; Read → clickable path + content;
//     Edit/Write/MultiEdit → a synthesized unified diff in DiffView; Grep/Glob →
//     pattern + hits; anything else → a key/value form of the input's top level).
//   - Input: the full input JSON, monospace, syntax-highlighted.
//   - Result: the full stored result text, monospace, with a wrap toggle, an
//     inline find bar and a copy button; notes when the store cap clipped it.
//
// A file path in the Overview opens through the panel's existing file-open relay
// (openFileRequested → MainWindow → EditorArea), surfaced via the openFile signal.
class ToolInspectorDialog : public QDialog
{
    Q_OBJECT
public:
    // `inputJson` is the pretty-printed input JSON (TranscriptModel::ToolDetailRole).
    // `fullResult` is the stored (possibly 128 KB-capped) result text
    // (ToolFullResultRole); `resultCapped` flags that the store cap clipped it.
    ToolInspectorDialog(const QString &toolName, const QString &inputJson,
                        const QString &fullResult, bool resultCapped,
                        QWidget *parent = nullptr);
    ~ToolInspectorDialog() override;

Q_SIGNALS:
    // A file path in the Overview was activated — the panel relays it to the
    // window's file-open path (openFileRequested).
    void openFile(const QString &path);

private:
    // Build the tool-aware Overview tab from `input`. Returns the widget to host.
    QWidget *buildOverview(const QString &toolName, const QJsonObject &input,
                           const QString &fullResult);
    // Build the Result tab (mono view + wrap toggle + find bar + copy).
    QWidget *buildResult(const QString &fullResult, bool resultCapped);

    // Run the inline find on the Result view from m_findEdit.
    void runResultFind(bool forward);

    QPlainTextEdit *m_resultView = nullptr;
    QLineEdit *m_findEdit = nullptr;
};
