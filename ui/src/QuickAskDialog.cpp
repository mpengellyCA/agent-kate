// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "QuickAskDialog.h"

#include "shell/ElidingLabel.h"

#include <KLocalizedString>

#include <QCursor>
#include <QGuiApplication>
#include <QLabel>
#include <QLineEdit>
#include <QScreen>
#include <QVBoxLayout>

QuickAskDialog::QuickAskDialog(QWidget *parent)
    : QDialog(parent, Qt::Dialog | Qt::FramelessWindowHint | Qt::WindowStaysOnTopHint)
{
    setObjectName(QStringLiteral("quickAskDialog"));
    // Deliberately no QSS and no custom paint: a frameless QDialog wears the
    // active colour scheme's window role, which is all the chrome this needs.
    setMinimumWidth(480);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(12, 10, 12, 10);
    layout->setSpacing(6);

    m_target = new ElidingLabel(this);
    m_target->setElideMode(Qt::ElideMiddle);
    // Secondary by role, not by hex — follows a runtime light/dark switch.
    m_target->setForegroundRole(QPalette::PlaceholderText);
    layout->addWidget(m_target);

    m_edit = new QLineEdit(this);
    m_edit->setPlaceholderText(i18n("Ask the agent… (Enter sends, Esc closes)"));
    m_edit->setClearButtonEnabled(true);
    layout->addWidget(m_edit);

    m_error = new QLabel(this);
    m_error->setWordWrap(true);
    m_error->setVisible(false);
    layout->addWidget(m_error);

    connect(m_edit, &QLineEdit::returnPressed, this, [this] {
        const QString text = m_edit->text().trimmed();
        if (!text.isEmpty()) {
            Q_EMIT submitted(text);
        }
    });
    // Esc → QDialog::reject → close. Nothing to save: the composer draft in
    // the main window was never touched.
}

void QuickAskDialog::setTargetName(const QString &name)
{
    m_target->setText(name.isEmpty()
                          ? i18n("Quick ask")
                          : i18nc("target agent of the quick-ask line", "To: %1", name));
}

void QuickAskDialog::popUp()
{
    m_error->setVisible(false);
    // Centre on the screen the cursor is on — the shortcut fires from
    // anywhere, and appearing on a different monitor reads as not appearing.
    if (QScreen *screen = QGuiApplication::screenAt(QCursor::pos())
                              ? QGuiApplication::screenAt(QCursor::pos())
                              : QGuiApplication::primaryScreen()) {
        const QRect avail = screen->availableGeometry();
        adjustSize();
        move(avail.center().x() - width() / 2,
             avail.top() + avail.height() / 4);
    }
    show();
    raise();
    activateWindow();
    m_edit->setFocus();
    m_edit->selectAll();
}

void QuickAskDialog::acceptSent()
{
    m_edit->clear();
    m_error->setVisible(false);
    accept();
}

void QuickAskDialog::showError(const QString &message)
{
    m_error->setText(message);
    m_error->setVisible(true);
}
