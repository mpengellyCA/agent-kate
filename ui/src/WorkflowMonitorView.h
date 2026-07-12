// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QWidget>

class WorkflowMonitor;
class QLabel;
class QTreeWidget;
class QTreeWidgetItem;
class QPlainTextEdit;
class QPushButton;
class QListWidget;

// WorkflowMonitorView renders a WorkflowMonitor's live status: a header strip
// (state badge, run id, counts, phase plan), a phase→sub-agent tree, a detail
// pane for the selected sub-agent (prompt / result / current activity + an
// Open-transcript action), and the workflow's narrator logs.
//
// It owns its WorkflowMonitor and rebuilds on every changed() signal, preserving
// the selected sub-agent and scroll position so the tree doesn't jump while the
// workflow churns. The same widget backs both the dedicated dialog and the
// inline "Progress" tab in the tool inspector.
class WorkflowMonitorView : public QWidget
{
    Q_OBJECT
public:
    WorkflowMonitorView(const QString &inputJson, const QString &resultText,
                        QWidget *parent = nullptr);

    // True when the launch result named a followable transcript dir.
    bool isValid() const;

private:
    void rebuild();
    // Reconcile the phase→sub-agent tree in place against the current snapshot,
    // touching only rows whose data actually changed (no clear()/rebuild), so a
    // long list doesn't flicker as activity updates.
    void reconcileTree();
    // Refresh the detail pane + Open button from the current selection, leaving
    // the pane untouched (and its scroll intact) when nothing changed.
    void syncDetail();
    // Open the selected sub-agent's transcript as a readable chat.
    void openSelectedTranscript();

    WorkflowMonitor *m_monitor = nullptr;

    QLabel *m_header = nullptr;
    QLabel *m_summary = nullptr;
    QTreeWidget *m_tree = nullptr;
    QPlainTextEdit *m_detail = nullptr;
    QPushButton *m_openBtn = nullptr;
    QLabel *m_logsCaption = nullptr;
    QListWidget *m_logs = nullptr;

    // The transcript path + label of the currently selected sub-agent (for Open).
    QString m_selectedPath;
    QString m_selectedLabel;
};
