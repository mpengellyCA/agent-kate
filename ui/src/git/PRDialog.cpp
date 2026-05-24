// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "PRDialog.h"
#include "ipc/CoreClient.h"

#include <QCheckBox>
#include <QDialogButtonBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QLineEdit>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QVBoxLayout>

PRDialog::PRDialog(CoreClient *core, const QString &threadId, const QString &branch,
                   QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_threadId(threadId)
    , m_branch(branch)
{
    setWindowTitle(branch.isEmpty()
                       ? QStringLiteral("Open a pull request")
                       : QStringLiteral("Open PR from %1").arg(branch));
    resize(640, 560);

    m_title = new QLineEdit(this);
    m_title->setPlaceholderText(QStringLiteral("PR title"));

    m_body = new QPlainTextEdit(this);
    m_body->setPlaceholderText(QStringLiteral("PR body (Markdown)"));

    m_draft = new QCheckBox(QStringLiteral("Open as draft"), this);

    m_status = new QLabel(this);
    m_status->setWordWrap(true);
    m_status->setStyleSheet(QStringLiteral("color: palette(mid); font-size: small;"));
    m_status->setText(QStringLiteral("Loading draft from commit history…"));

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Cancel, this);
    m_createBtn = buttons->addButton(QStringLiteral("Create PR"),
                                     QDialogButtonBox::AcceptRole);
    m_createBtn->setEnabled(false); // re-enabled once the draft has loaded
    m_cancelBtn = buttons->button(QDialogButtonBox::Cancel);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(m_createBtn, &QPushButton::clicked, this, &PRDialog::onCreateClicked);

    auto *titleRow = new QHBoxLayout;
    titleRow->setContentsMargins(0, 0, 0, 0);
    titleRow->addWidget(new QLabel(QStringLiteral("Title:"), this));
    titleRow->addWidget(m_title, 1);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(14, 14, 14, 14);
    layout->setSpacing(10);
    layout->addLayout(titleRow);
    layout->addWidget(m_body, 1);
    layout->addWidget(m_draft);
    layout->addWidget(m_status);
    layout->addWidget(buttons);

    loadDraft();
}

void PRDialog::loadDraft()
{
    if (!m_core->isConnected()) {
        m_status->setText(QStringLiteral("Not connected to the core."));
        return;
    }
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.prDraft"),
                 QJsonObject{{QStringLiteral("threadId"), thread}},
                 [this, thread](const QJsonObject &result, const QJsonObject &error) {
                     if (thread != m_threadId) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         m_status->setText(
                             QStringLiteral("Could not draft a PR: %1")
                                 .arg(error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     m_title->setText(result.value(QStringLiteral("title")).toString());
                     m_body->setPlainText(
                         result.value(QStringLiteral("body")).toString());
                     m_status->setText(
                         QStringLiteral("Draft generated from commit history — "
                                        "edit freely before creating."));
                     m_createBtn->setEnabled(true);
                     m_title->setFocus();
                     m_title->selectAll();
                 });
}

void PRDialog::onCreateClicked()
{
    const QString title = m_title->text().trimmed();
    if (title.isEmpty()) {
        m_status->setText(QStringLiteral("A title is required."));
        m_title->setFocus();
        return;
    }
    m_createBtn->setEnabled(false);
    m_status->setText(QStringLiteral("Pushing branch and creating PR…"));
    const QString thread = m_threadId;
    m_core->call(QStringLiteral("git.openPR"),
                 QJsonObject{{QStringLiteral("threadId"), thread},
                             {QStringLiteral("title"), title},
                             {QStringLiteral("body"), m_body->toPlainText()},
                             {QStringLiteral("draft"), m_draft->isChecked()}},
                 [this, thread](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_createBtn->setEnabled(true);
                         m_status->setText(
                             QStringLiteral("Failed: %1")
                                 .arg(error.value(QStringLiteral("message"))
                                          .toString()));
                         return;
                     }
                     emit prOpened(result.value(QStringLiteral("url")).toString());
                     accept();
                 });
}
