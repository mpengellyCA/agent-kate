// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QLabel>

// ElidingLabel — a QLabel that shortens its text with an ellipsis when it is
// given less width than the text needs, instead of forcing its container wider.
// The full text is always available as a tooltip. Use it for dynamic strings
// that can get long — file paths, branch names, model/cost summaries — inside
// narrow docked panels, where a plain QLabel would pin the panel's minimum
// width to the full string.
//
// Note: QLabel::setText is non-virtual, so call setText through an ElidingLabel*
// (not a QLabel*) for the eliding behaviour to take effect.
class ElidingLabel : public QLabel
{
    Q_OBJECT
public:
    explicit ElidingLabel(QWidget *parent = nullptr);
    explicit ElidingLabel(const QString &text, QWidget *parent = nullptr);

    void setText(const QString &text);
    QString fullText() const { return m_full; }

    void setElideMode(Qt::TextElideMode mode);
    Qt::TextElideMode elideMode() const { return m_mode; }

    QSize minimumSizeHint() const override;
    QSize sizeHint() const override;

protected:
    void paintEvent(QPaintEvent *event) override;

private:
    QString m_full;
    Qt::TextElideMode m_mode = Qt::ElideRight;
};
