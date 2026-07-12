// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>

// WorkflowMonitorDialog is the dedicated, full-size window for following a
// background Workflow launched from an agent. It is the standalone counterpart to
// the inline "Progress" tab in the tool inspector: opened from the panel's
// "Workflow" chip so the run can be watched without hunting for its tool row.
//
// Non-modal (show(), WA_DeleteOnClose) so the user keeps working with it open,
// and it remembers its size in KConfig — mirroring ToolInspectorDialog. It hosts
// a WorkflowMonitorView and relays that view's file-open requests.
class WorkflowMonitorDialog : public QDialog
{
    Q_OBJECT
public:
    // `inputJson` is the Workflow tool's pretty input JSON; `resultText` is its
    // launch result blob (carrying the run's Task ID / Transcript dir / Run ID).
    WorkflowMonitorDialog(const QString &inputJson, const QString &resultText,
                          QWidget *parent = nullptr);
    ~WorkflowMonitorDialog() override;
};
