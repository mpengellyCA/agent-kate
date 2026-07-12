// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorkflowMonitorDialog.h"

#include "WorkflowMonitorView.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QHBoxLayout>
#include <QPushButton>
#include <QVBoxLayout>

WorkflowMonitorDialog::WorkflowMonitorDialog(const QString &inputJson,
                                             const QString &resultText, QWidget *parent)
    : QDialog(parent)
{
    setAttribute(Qt::WA_DeleteOnClose);
    setWindowTitle(i18nc("@title:window", "Workflow progress"));

    auto *view = new WorkflowMonitorView(inputJson, resultText, this);

    auto *close = new QPushButton(i18n("Close"), this);
    connect(close, &QPushButton::clicked, this, &QDialog::accept);
    auto *btnRow = new QHBoxLayout;
    btnRow->addStretch(1);
    btnRow->addWidget(close);

    auto *root = new QVBoxLayout(this);
    root->setContentsMargins(0, 0, 8, 8);
    root->addWidget(view, 1);
    root->addLayout(btnRow);

    const KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("WorkflowMonitorDialog"));
    resize(cfg.readEntry("size", QSize(820, 640)));
}

WorkflowMonitorDialog::~WorkflowMonitorDialog()
{
    KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("WorkflowMonitorDialog"));
    cfg.writeEntry("size", size());
}
