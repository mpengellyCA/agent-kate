// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "ForkAgentDialog.h"

#include <KLocalizedString>

#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QVBoxLayout>

ForkAgentDialog::ForkAgentDialog(const QString &sourceTitle, const QString &sourceModel,
                                 const QString &sourceEffort, QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18nc("@title:window", "Fork Agent"));

    auto *root = new QVBoxLayout(this);
    root->setSpacing(12);

    auto *heading = new QLabel(
        sourceTitle.isEmpty()
            ? i18n("Continue this conversation on a different model")
            : i18n("Continue <b>%1</b> on a different model", sourceTitle.toHtmlEscaped()),
        this);
    heading->setTextFormat(Qt::RichText);
    heading->setWordWrap(true);
    root->addWidget(heading);

    auto *form = new QFormLayout;
    form->setLabelAlignment(Qt::AlignLeft);

    m_name = new QLineEdit(this);
    const QString defaultName =
        sourceTitle.isEmpty() ? i18n("Fork") : i18n("Fork of %1", sourceTitle);
    m_name->setText(defaultName);
    m_name->setToolTip(i18n("The name for the new agent in your roster."));
    form->addRow(i18n("Name"), m_name);

    m_model = new QComboBox(this);
    m_model->addItem(i18n("Keep the same"), QString());
    m_model->addItem(i18n("Smartest (Opus)"), QStringLiteral("opus"));
    m_model->addItem(i18n("Balanced (Sonnet)"), QStringLiteral("sonnet"));
    m_model->addItem(i18n("Fastest (Haiku)"), QStringLiteral("haiku"));
    m_model->addItem(i18n("Fable"), QStringLiteral("fable"));
    m_model->setToolTip(i18n("Which model the forked conversation continues on."));
    // Prefill from the source agent so a fork that changes only the effort keeps
    // the model, and vice versa.
    if (const int idx = m_model->findData(sourceModel); idx >= 0) {
        m_model->setCurrentIndex(idx);
    }
    form->addRow(i18n("Model"), m_model);

    m_effort = new QComboBox(this);
    m_effort->addItem(i18n("Keep the same"), QString());
    m_effort->addItem(i18n("Low"), QStringLiteral("low"));
    m_effort->addItem(i18n("Medium"), QStringLiteral("medium"));
    m_effort->addItem(i18n("High"), QStringLiteral("high"));
    m_effort->addItem(i18n("Extra-high"), QStringLiteral("xhigh"));
    m_effort->addItem(i18n("Maximum"), QStringLiteral("max"));
    m_effort->setToolTip(i18n("How hard the forked conversation thinks."));
    if (const int idx = m_effort->findData(sourceEffort); idx >= 0) {
        m_effort->setCurrentIndex(idx);
    }
    form->addRow(i18n("Thinking effort"), m_effort);

    root->addLayout(form);

    auto *subtext = new QLabel(
        i18n("The fork remembers the whole conversation and starts in its own private "
             "copy. Only committed changes are carried over — any uncommitted edits in "
             "the original stay behind. The original agent keeps running, untouched."),
        this);
    subtext->setWordWrap(true);
    subtext->setEnabled(false); // muted, informational
    root->addWidget(subtext);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    buttons->button(QDialogButtonBox::Ok)->setText(i18n("Fork"));
    buttons->button(QDialogButtonBox::Ok)->setIcon(QIcon::fromTheme(QStringLiteral("edit-copy")));
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    root->addWidget(buttons);

    m_name->setFocus();
    m_name->selectAll();
    resize(440, 320);
}

ForkChoices ForkAgentDialog::choices() const
{
    ForkChoices c;
    c.name = m_name->text().trimmed();
    c.modelId = m_model->currentData().toString();
    c.effort = m_effort->currentData().toString();
    return c;
}
