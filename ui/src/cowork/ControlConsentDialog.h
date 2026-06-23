#pragma once

#include <QDialog>
#include <QJsonObject>

class QPushButton;

// ControlConsentDialog is the DISTINCT high-risk (R2) consent surface for desktop
// CONTROL — input injection and semantic widget actions. It deliberately does not
// reuse the ordinary ConsentDialog: it renders the literal action, is styled as
// dangerous, and gates "Allow" behind a typed phrase so it cannot be rubber-stamped.
// It is per-action only (no "remember"). v1 ships this as a reviewable shell — no R2
// tool is wired to it yet (the control capabilities are v2/v3).
class ControlConsentDialog : public QDialog
{
    Q_OBJECT
public:
    explicit ControlConsentDialog(const QJsonObject &request, QWidget *parent = nullptr);

    bool allowed() const { return m_allowed; }
    QString scope() const { return QStringLiteral("once"); } // R2 is never remembered
    int expiresInSec() const { return 0; }

private:
    QPushButton *m_allow = nullptr;
    bool m_allowed = false;
};
