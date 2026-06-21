// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>
#include <QStringList>
#include <QVector>

class QLineEdit;
class QCheckBox;

// AutoOrganizeDialog previews Sonnet's proposed tag assignments for a project's
// agents and lets the user edit or skip each one before anything is written.
// Auto-organize never applies silently: the user must confirm here. Each row
// shows the agent's title and an editable, space-separated tag field; a
// per-row checkbox decides whether that row is applied.
//
// On accept, results() returns one entry per *checked* row — the caller applies
// them via agent.setTags.
class AutoOrganizeDialog : public QDialog
{
    Q_OBJECT
public:
    struct Proposal {
        QString threadId;
        QString label;   // agent display title for the row
        QStringList tags; // proposed tags (already lowercased by the core)
    };
    struct Result {
        QString threadId;
        QStringList tags;
    };

    AutoOrganizeDialog(const QVector<Proposal> &proposals, QWidget *parent = nullptr);

    // The checked rows with their (possibly edited) tag sets.
    QVector<Result> results() const;

private:
    struct Row {
        QString threadId;
        QCheckBox *apply = nullptr;
        QLineEdit *edit = nullptr;
    };
    QVector<Row> m_rows;
};
