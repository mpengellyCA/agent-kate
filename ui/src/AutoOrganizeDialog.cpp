// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AutoOrganizeDialog.h"

#include <KLocalizedString>

#include <QCheckBox>
#include <QDialogButtonBox>
#include <QGridLayout>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QScrollArea>
#include <QVBoxLayout>
#include <QWidget>

AutoOrganizeDialog::AutoOrganizeDialog(const QVector<Proposal> &proposals,
                                       QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18n("Auto-organize Agents"));
    resize(540, 460);

    auto *layout = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Claude proposed tags for these agents. Review and edit the tags "
             "(space-separated), untick any agent you want to leave alone, then "
             "Apply. Nothing is changed until you click Apply."),
        this);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    // Scrollable grid of rows: [apply checkbox] [agent label] [tags field].
    auto *scroll = new QScrollArea(this);
    scroll->setWidgetResizable(true);
    auto *body = new QWidget(scroll);
    auto *grid = new QGridLayout(body);
    grid->setColumnStretch(1, 1);
    grid->setColumnStretch(2, 2);

    grid->addWidget(new QLabel(i18n("Apply"), body), 0, 0);
    grid->addWidget(new QLabel(i18n("Agent"), body), 0, 1);
    grid->addWidget(new QLabel(i18n("Tags"), body), 0, 2);

    int r = 1;
    for (const Proposal &p : proposals) {
        Row row;
        row.threadId = p.threadId;

        row.apply = new QCheckBox(body);
        row.apply->setChecked(true);
        grid->addWidget(row.apply, r, 0, Qt::AlignTop);

        auto *label = new QLabel(p.label, body);
        label->setWordWrap(true);
        grid->addWidget(label, r, 1);

        row.edit = new QLineEdit(p.tags.join(QLatin1Char(' ')), body);
        row.edit->setPlaceholderText(i18n("space-separated tags"));
        grid->addWidget(row.edit, r, 2);

        m_rows.append(row);
        ++r;
    }
    grid->setRowStretch(r, 1);

    scroll->setWidget(body);
    layout->addWidget(scroll, 1);

    auto *buttons =
        new QDialogButtonBox(QDialogButtonBox::Apply | QDialogButtonBox::Cancel, this);
    buttons->button(QDialogButtonBox::Apply)->setText(i18n("Apply"));
    connect(buttons->button(QDialogButtonBox::Apply), &QPushButton::clicked, this,
            &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    layout->addWidget(buttons);
}

QVector<AutoOrganizeDialog::Result> AutoOrganizeDialog::results() const
{
    QVector<Result> out;
    for (const Row &row : m_rows) {
        if (!row.apply->isChecked()) {
            continue;
        }
        QStringList tags;
        const QStringList parts =
            row.edit->text().split(QLatin1Char(' '), Qt::SkipEmptyParts);
        for (const QString &p : parts) {
            const QString t = p.trimmed();
            if (!t.isEmpty()) {
                tags.append(t);
            }
        }
        out.append(Result{row.threadId, tags});
    }
    return out;
}
