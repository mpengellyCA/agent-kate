#pragma once

#include <QJsonArray>
#include <QWidget>

class CoreClient;
class KMessageWidget;
class QLineEdit;
class QLabel;
class QPushButton;
class QVBoxLayout;

// RemotePanel is the desktop-only cockpit for the separate paired-device
// surface. It intentionally uses a manually entered, specific host:port: the
// core will not guess a LAN interface or silently widen to a wildcard bind.
// QR sharing remains a hardware-validation spike; the one-time pairing URL is
// shown as selectable text in a transient dialog instead.
class RemotePanel : public QWidget
{
    Q_OBJECT
public:
    explicit RemotePanel(CoreClient *core, QWidget *parent = nullptr);

public Q_SLOTS:
    void refresh();

private Q_SLOTS:
    void toggleEnabled();
    void pairDevice();
    void toggleKill();
    void showAudit();

private:
    void applyStatus(const QJsonObject &status);
    void renderDevices(const QJsonArray &devices);
    void showError(const QJsonObject &error, const QString &fallback);

    CoreClient *m_core = nullptr;
    bool m_enabled = false;
    bool m_killed = false;
    bool m_available = true;

    KMessageWidget *m_status = nullptr;
    QLineEdit *m_bindAddr = nullptr;
    QLabel *m_fingerprint = nullptr;
    QLabel *m_devicesEmpty = nullptr;
    QVBoxLayout *m_devices = nullptr;
    QPushButton *m_toggle = nullptr;
    QPushButton *m_pair = nullptr;
    QPushButton *m_kill = nullptr;
    QPushButton *m_audit = nullptr;
};
