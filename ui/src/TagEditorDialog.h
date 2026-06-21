// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>
#include <QStringList>

class KEditListWidget;

// TagEditorDialog edits the full tag set of one agent. It presents a native
// KEditListWidget (add / remove / reorder) seeded with the agent's current
// tags, and offers completion over every tag already in use in the project so
// users converge on a shared vocabulary instead of inventing near-duplicates.
// On accept, tags() returns the edited set (trimmed, de-duplicated by the
// widget; the core normalizes again authoritatively).
class TagEditorDialog : public QDialog
{
    Q_OBJECT
public:
    // current: the agent's existing tags. suggestions: every tag in use in the
    // project, used to drive completion in the entry field.
    TagEditorDialog(const QStringList &current, const QStringList &suggestions,
                    QWidget *parent = nullptr);

    // The edited tag set, in display order.
    QStringList tags() const;

private:
    KEditListWidget *m_list = nullptr;
};
