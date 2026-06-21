// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "TagEditorDialog.h"

#include <KEditListWidget>
#include <KLocalizedString>

#include <QCompleter>
#include <QDialogButtonBox>
#include <QLabel>
#include <QLineEdit>
#include <QVBoxLayout>

TagEditorDialog::TagEditorDialog(const QStringList &current,
                                 const QStringList &suggestions, QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18n("Edit Agent Tags"));
    resize(360, 380);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Tags help you organize agents in the roster. Type a tag and press "
             "Enter to add it; existing project tags auto-complete."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    m_list = new KEditListWidget(this);
    m_list->setItems(current);
    layout->addWidget(m_list, 1);

    if (!suggestions.isEmpty()) {
        auto *completer = new QCompleter(suggestions, this);
        completer->setCaseSensitivity(Qt::CaseInsensitive);
        completer->setCompletionMode(QCompleter::PopupCompletion);
        m_list->lineEdit()->setCompleter(completer);
    }

    auto *buttons =
        new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    layout->addWidget(buttons);
}

QStringList TagEditorDialog::tags() const
{
    return m_list ? m_list->items() : QStringList();
}
