// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "ForkAgentDialog.h"
#include "state/HarnessTraits.h"

#include <KLocalizedString>

#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QVBoxLayout>

ForkAgentDialog::ForkAgentDialog(const QString &sourceTitle, const QString &sourceModel,
                                 const QString &sourceEffort, const QString &backend,
                                 const QString &providerId, QWidget *parent)
    : QDialog(parent)
{
    const HarnessTraits traits = HarnessRegistry::self()->traits(backend);
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
    // Live catalogue for the source agent's engine/provider: recommended group,
    // then the full list. No hardcoded model names.
    {
        const auto choices = HarnessRegistry::self()->modelChoices(backend, providerId);
        const auto addEntries = [this](const QStringList &entries) {
            for (const QString &entry : entries) {
                const QString value = entry.section(QLatin1Char('|'), 0, 0);
                const QString name = entry.section(QLatin1Char('|'), 1);
                if (!value.isEmpty() && m_model->findData(value) < 0) {
                    m_model->addItem(name.isEmpty() ? value : name, value);
                }
            }
        };
        addEntries(choices.recommended);
        if (!choices.recommended.isEmpty() && !choices.all.isEmpty()) {
            m_model->insertSeparator(m_model->count());
        }
        addEntries(choices.all);
    }
    m_model->setToolTip(i18n("Which model the forked conversation continues on."));
    // Prefill from the source agent so a fork that changes only the effort keeps
    // the model, and vice versa.
    if (const int idx = m_model->findData(sourceModel); idx >= 0) {
        m_model->setCurrentIndex(idx);
    }
    form->addRow(i18n("Model"), m_model);

    m_effort = new QComboBox(this);
    m_effort->addItem(i18n("Keep the same"), QString());
    for (const QString &effort : traits.efforts) {
        m_effort->addItem(HarnessRegistry::effortLabel(effort), effort);
    }
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
