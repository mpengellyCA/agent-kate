// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QFrame>
#include <QString>

class QLabel;
class ElidingLabel;

// CapabilityTile — one large, checkable "device toggle" for a single Cowork
// capability (plan 13 phase 10). It replaces the dense checkbox column with a
// control-center feel: a theme icon, a human title, a one-line description, and
// a clear on/off state rendered with the palette's Highlight role (never a
// hardcoded colour, so KDE schemes keep working).
//
// Read-tier capabilities render plain. Control-tier ("dangerous") capabilities
// carry a warning accent border and a ⚠ marker in the title, plus the caller's
// tooltip — the visual cue that flipping this lets the agent act *as you*.
//
// The tile owns no RPC: it emits toggled(key, on) on user interaction and the
// panel relays that to cowork.setPolicy. setChecked() is silent (no signal), so
// refreshing from the policy never echoes back a write.
class CapabilityTile : public QFrame
{
    Q_OBJECT
public:
    CapabilityTile(const QString &key, const QString &title, const QString &description,
                   const QString &iconName, bool dangerous, QWidget *parent = nullptr);

    QString key() const { return m_key; }
    bool isChecked() const { return m_checked; }
    // Set the on/off state without emitting toggled() (used when syncing from policy).
    void setChecked(bool on);

Q_SIGNALS:
    void toggled(const QString &key, bool on);

protected:
    void mousePressEvent(QMouseEvent *event) override;
    void keyPressEvent(QKeyEvent *event) override;
    bool event(QEvent *event) override; // palette changes → restyle

private:
    void restyle();

    QString m_key;
    bool m_dangerous = false;
    bool m_checked = false;

    QLabel *m_icon = nullptr;
    QLabel *m_title = nullptr;
    ElidingLabel *m_desc = nullptr;
};
