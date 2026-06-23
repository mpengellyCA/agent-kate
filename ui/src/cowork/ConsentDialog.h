#pragma once

#include <QDialog>
#include <QJsonObject>
#include <QString>

class QComboBox;

// ConsentDialog presents a Cowork grant request (R0/R1: window-list, a11y-read,
// screenshot, screencast) and lets the user allow — choosing a scope — or deny.
// Deny is the default (safe) action. High-risk control (R2) uses the distinct
// ControlConsentDialog instead, never this one.
class ConsentDialog : public QDialog
{
    Q_OBJECT
public:
    explicit ConsentDialog(const QJsonObject &request, QWidget *parent = nullptr);

    bool allowed() const { return m_allowed; }
    QString scope() const;       // "once" | "session" | "timed" | "until_revoked"
    int expiresInSec() const;    // for "timed"

private:
    QComboBox *m_scope = nullptr;
    bool m_allowed = false;
};
