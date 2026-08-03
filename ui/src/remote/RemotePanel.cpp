#include "remote/RemotePanel.h"

#include "ipc/CoreClient.h"

#include <KLocalizedString>
#include <KMessageWidget>

#include <QDialog>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QHBoxLayout>
#include <QInputDialog>
#include <QJsonObject>
#include <QJsonValue>
#include <QLabel>
#include <QLineEdit>
#include <QPlainTextEdit>
#include <QPointer>
#include <QPushButton>
#include <QScrollArea>
#include <QVBoxLayout>

namespace {

QString errorText(const QJsonObject &error, const QString &fallback)
{
    const QString message = error.value(QStringLiteral("message")).toString();
    return message.isEmpty() ? fallback : message;
}

QString deviceState(const QJsonObject &device)
{
    if (device.value(QStringLiteral("revoked")).toBool()) {
        return i18n("revoked");
    }
    const QString seen = device.value(QStringLiteral("lastSeen")).toString();
    return seen.isEmpty() ? i18n("paired, not yet connected") : i18n("last seen %1", seen);
}

} // namespace

RemotePanel::RemotePanel(CoreClient *core, QWidget *parent)
    : QWidget(parent), m_core(core)
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(10, 10, 10, 10);

    auto *title = new QLabel(i18n("<b>Remote access</b> — paired devices can read the redacted "
                                  "agent surface and answer parked prompts."), this);
    title->setWordWrap(true);
    layout->addWidget(title);

    m_status = new KMessageWidget(this);
    m_status->setCloseButtonVisible(false);
    m_status->setText(i18n("Checking remote access…"));
    layout->addWidget(m_status);

    auto *bindRow = new QHBoxLayout;
    bindRow->addWidget(new QLabel(i18n("Bind address:"), this));
    m_bindAddr = new QLineEdit(this);
    m_bindAddr->setPlaceholderText(i18n("192.168.1.20:8443"));
    m_bindAddr->setToolTip(i18n("Choose one specific LAN or overlay-network address. "
                                "The core refuses wildcard binds unless a future explicit "
                                "advanced control is added."));
    bindRow->addWidget(m_bindAddr, 1);
    m_toggle = new QPushButton(i18n("Enable"), this);
    connect(m_toggle, &QPushButton::clicked, this, &RemotePanel::toggleEnabled);
    bindRow->addWidget(m_toggle);
    layout->addLayout(bindRow);

    m_fingerprint = new QLabel(this);
    m_fingerprint->setWordWrap(true);
    layout->addWidget(m_fingerprint);

    auto *devicesBox = new QScrollArea(this);
    devicesBox->setWidgetResizable(true);
    auto *devicesHost = new QWidget(devicesBox);
    m_devices = new QVBoxLayout(devicesHost);
    m_devices->setContentsMargins(0, 0, 0, 0);
    m_devicesEmpty = new QLabel(i18n("No paired devices."), devicesHost);
    m_devicesEmpty->setWordWrap(true);
    m_devices->addWidget(m_devicesEmpty);
    m_devices->addStretch(1);
    devicesBox->setWidget(devicesHost);
    layout->addWidget(new QLabel(i18n("Paired devices"), this));
    layout->addWidget(devicesBox, 1);

    auto *actions = new QHBoxLayout;
    m_pair = new QPushButton(i18n("Pair device…"), this);
    connect(m_pair, &QPushButton::clicked, this, &RemotePanel::pairDevice);
    actions->addWidget(m_pair);
    m_kill = new QPushButton(i18n("Switch off remote access"), this);
    connect(m_kill, &QPushButton::clicked, this, &RemotePanel::toggleKill);
    actions->addWidget(m_kill);
    m_audit = new QPushButton(i18n("Audit log…"), this);
    connect(m_audit, &QPushButton::clicked, this, &RemotePanel::showAudit);
    actions->addWidget(m_audit);
    actions->addStretch(1);
    layout->addLayout(actions);

    connect(m_core, &CoreClient::connected, this, &RemotePanel::refresh);
    if (m_core->isConnected()) {
        refresh();
    }
}

void RemotePanel::refresh()
{
    if (!m_core || !m_core->isConnected() || !m_available) {
        return;
    }
    m_core->call(QStringLiteral("remote.status"), {},
                 [this](const QJsonObject &result, const QJsonObject &error) {
        if (!error.isEmpty()) {
            if (error.value(QStringLiteral("code")).toInt() == -32601) {
                m_available = false;
            }
            showError(error, i18n("Remote access status is unavailable."));
            return;
        }
        applyStatus(result);
    }, this);
}

void RemotePanel::applyStatus(const QJsonObject &status)
{
    m_enabled = status.value(QStringLiteral("enabled")).toBool();
    m_killed = status.value(QStringLiteral("killSwitch")).toBool();
    const QString addr = status.value(QStringLiteral("addr")).toString();
    const QString fp = status.value(QStringLiteral("certFingerprint")).toString();
    const bool tampered = status.value(QStringLiteral("auditTampered")).toBool();

    if (tampered) {
        m_status->setMessageType(KMessageWidget::Error);
        m_status->setText(i18n("The remote audit chain does not verify. Review the audit log before trusting remote actions."));
    } else if (m_killed) {
        m_status->setMessageType(KMessageWidget::Warning);
        m_status->setText(i18n("Remote API access is switched off. The listener may remain up so paired devices receive this state."));
    } else if (m_enabled) {
        m_status->setMessageType(KMessageWidget::Positive);
        m_status->setText(i18n("Remote HTTPS listener is running at %1.", addr));
    } else {
        m_status->setMessageType(KMessageWidget::Information);
        m_status->setText(i18n("Remote access is off. Choose a specific host and port to enable it."));
    }
    m_status->animatedShow();
    m_toggle->setText(m_enabled ? i18n("Disable") : i18n("Enable"));
    m_pair->setEnabled(m_enabled && !m_killed);
    m_kill->setEnabled(m_enabled);
    m_kill->setText(m_killed ? i18n("Re-enable remote API") : i18n("Switch off remote access"));
    m_audit->setEnabled(m_available);
    m_fingerprint->setText(fp.isEmpty() ? QString() : i18n("TLS fingerprint: <code>%1</code>", fp.toHtmlEscaped()));
    renderDevices(status.value(QStringLiteral("devices")).toArray());
}

void RemotePanel::renderDevices(const QJsonArray &devices)
{
    while (m_devices->count() > 2) {
        auto *item = m_devices->takeAt(1);
        delete item->widget();
        delete item;
    }
    m_devicesEmpty->setVisible(devices.isEmpty());
    for (const QJsonValue &value : devices) {
        const QJsonObject device = value.toObject();
        auto *row = new QWidget(this);
        auto *rowLayout = new QHBoxLayout(row);
        rowLayout->setContentsMargins(0, 0, 0, 0);
        auto *label = new QLabel(i18n("<b>%1</b><br/><small>%2</small>",
                                      device.value(QStringLiteral("name")).toString().toHtmlEscaped(),
                                      deviceState(device).toHtmlEscaped()), row);
        rowLayout->addWidget(label, 1);
        if (!device.value(QStringLiteral("revoked")).toBool()) {
            auto *revoke = new QPushButton(i18n("Revoke"), row);
            const QString id = device.value(QStringLiteral("id")).toString();
            connect(revoke, &QPushButton::clicked, this, [this, id] {
                m_core->call(QStringLiteral("remote.revokeDevice"), {{QStringLiteral("deviceId"), id}},
                             [this](const QJsonObject &, const QJsonObject &error) {
                    if (!error.isEmpty()) showError(error, i18n("Could not revoke device."));
                    refresh();
                }, this);
            });
            rowLayout->addWidget(revoke);
        }
        m_devices->insertWidget(m_devices->count() - 1, row);
    }
}

void RemotePanel::toggleEnabled()
{
    QJsonObject params{{QStringLiteral("enabled"), !m_enabled}};
    if (!m_enabled) {
        params.insert(QStringLiteral("bindAddr"), m_bindAddr->text().trimmed());
    }
    m_core->call(QStringLiteral("remote.setEnabled"), params,
                 [this](const QJsonObject &, const QJsonObject &error) {
        if (!error.isEmpty()) showError(error, i18n("Could not change remote access."));
        refresh();
    }, this);
}

void RemotePanel::pairDevice()
{
    bool accepted = false;
    const QString name = QInputDialog::getText(this, i18n("Pair device"), i18n("Device name:"),
                                                QLineEdit::Normal, {}, &accepted).trimmed();
    if (!accepted || name.isEmpty()) return;
    m_core->call(QStringLiteral("remote.pairDevice"), {{QStringLiteral("name"), name}},
                 [this, name](const QJsonObject &result, const QJsonObject &error) {
        if (!error.isEmpty()) {
            showError(error, i18n("Could not pair device."));
            return;
        }
        const QString url = result.value(QStringLiteral("pairingUrl")).toString();
        auto *dialog = new QDialog(this);
        dialog->setAttribute(Qt::WA_DeleteOnClose);
        dialog->setWindowTitle(i18n("Pair %1", name));
        auto *layout = new QVBoxLayout(dialog);
        auto *hint = new QLabel(i18n("Open this one-time URL on the device. Keep it private; it includes the pairing token in its fragment."), dialog);
        hint->setWordWrap(true);
        layout->addWidget(hint);
        auto *urlField = new QLineEdit(url, dialog);
        urlField->setReadOnly(true);
        urlField->selectAll();
        layout->addWidget(urlField);
        auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, dialog);
        connect(buttons, &QDialogButtonBox::rejected, dialog, &QDialog::close);
        layout->addWidget(buttons);
        dialog->open();
        refresh();
    }, this);
}

void RemotePanel::toggleKill()
{
    m_core->call(QStringLiteral("remote.killSwitch"), {{QStringLiteral("on"), !m_killed}},
                 [this](const QJsonObject &, const QJsonObject &error) {
        if (!error.isEmpty()) showError(error, i18n("Could not change the remote kill switch."));
        refresh();
    }, this);
}

void RemotePanel::showAudit()
{
    m_core->call(QStringLiteral("remote.auditTail"), {{QStringLiteral("limit"), 100}},
                 [this](const QJsonObject &result, const QJsonObject &error) {
        if (!error.isEmpty()) {
            showError(error, i18n("Could not read remote audit log."));
            return;
        }
        auto *dialog = new QDialog(this);
        dialog->setAttribute(Qt::WA_DeleteOnClose);
        dialog->setWindowTitle(i18n("Remote access audit log"));
        auto *layout = new QVBoxLayout(dialog);
        auto *view = new QPlainTextEdit(dialog);
        view->setReadOnly(true);
        QStringList lines;
        for (const QJsonValue &value : result.value(QStringLiteral("entries")).toArray()) {
            const QJsonObject entry = value.toObject();
            lines << QStringLiteral("%1  %2  %3")
                         .arg(entry.value(QStringLiteral("at")).toString(),
                              entry.value(QStringLiteral("kind")).toString(),
                              entry.value(QStringLiteral("outcome")).toString());
        }
        view->setPlainText(lines.join(QLatin1Char('\n')));
        layout->addWidget(view);
        auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, dialog);
        connect(buttons, &QDialogButtonBox::rejected, dialog, &QDialog::close);
        layout->addWidget(buttons);
        dialog->resize(680, 420);
        dialog->open();
    }, this);
}

void RemotePanel::showError(const QJsonObject &error, const QString &fallback)
{
    m_status->setMessageType(KMessageWidget::Error);
    m_status->setText(errorText(error, fallback));
    m_status->animatedShow();
}
