// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>

class QComboBox;
class QLineEdit;

// The choices the Fork Agent dialog collects. Empty model/effort mean "keep the
// source agent's" — the fork only overrides what the user actually changed.
struct ForkChoices {
    QString name;    // fork's roster title
    QString modelId; // "" = keep source's; else a live model value/alias
    QString effort;  // "" | low | medium | high | xhigh | max
};

// ForkAgentDialog — take a running conversation and continue it on a different
// model or thinking effort without losing context. The model and effort pickers
// are prefilled from the source agent; a subtext notes that only committed work
// is carried into the fork's private copy.
class ForkAgentDialog : public QDialog
{
    Q_OBJECT
public:
    // sourceTitle names the agent being forked (used for the heading and the
    // default fork name). sourceModel/sourceEffort prefill the pickers with the
    // source's current settings ("" = the agent's default). backend/providerId
    // select which engine's live model + effort vocabularies the pickers offer.
    ForkAgentDialog(const QString &sourceTitle, const QString &sourceModel,
                    const QString &sourceEffort, const QString &backend,
                    const QString &providerId, QWidget *parent = nullptr);

    ForkChoices choices() const;

private:
    QLineEdit *m_name = nullptr;
    QComboBox *m_model = nullptr;
    QComboBox *m_effort = nullptr;
};
