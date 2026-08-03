// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QImage>
#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QString>
#include <QVector>
#include <QWidget>

class CoreClient;
class KMessageWidget;
class QComboBox;
class QLabel;
class QPlainTextEdit;
class QPushButton;
class QSpinBox;
class QTimer;
class QToolButton;
class QVBoxLayout;

// RemoteLogic holds every decision this panel makes that is NOT a widget: which
// interface to offer first, what a status reply means, how an audit row reads,
// and the QR encoder. None of it touches CoreClient, so all of it is testable
// headless with no core running (ui/tests/RemotePanelTest.cpp) — the same shape
// plan 18 A1 used for NotifyPolicy, and for the same reason: the interesting
// parts of a security surface should not need a live socket to assert.
namespace RemoteLogic
{

// One candidate bind address. `name` is the kernel interface name, which is what
// the user recognises ("wlan0", "tailscale0"); `address` is the IPv4 literal the
// core actually binds.
struct Interface {
    QString name;
    QString address;
    bool overlay = false;  // Tailscale / WireGuard / other encrypted overlay
    bool loopback = false; // this machine only (an SSH or `tailscale serve` tunnel)
};
// Virtual-machine, container and bridge plumbing. These addresses are reachable
// only from software running on this box, so offering them as "where your phone
// connects" is noise at best and a listener nobody can find at worst.
bool isNoiseInterface(const QString &name);

// An encrypted overlay network — the supported answer for any network the user
// does not control (docs/security-model.md §7). Matched on the interface name
// and, as a second signal, on Tailscale's 100.64.0.0/10 CGNAT range.
bool isOverlayInterface(const QString &name, const QString &address);

// Filter the noise out and rank what is left: overlay networks first (they are
// the honest recommendation), then ordinary LAN interfaces, then loopback last.
QList<Interface> rankInterfaces(const QList<Interface> &found);

// rankInterfaces() over this machine's live QNetworkInterface list.
QList<Interface> localInterfaces();

// The combo-box label — carries the *reason* an overlay is first, because a
// silently reordered list teaches nobody anything.
QString interfaceLabel(const Interface &iface);

// "HOST:PORT", the only shape remote.setEnabled accepts. IPv6 literals are
// bracketed; an empty host never becomes an implicit wildcard. The explicitly
// labelled all-adapters choices are confirmed in RemotePanel before this is
// called with 0.0.0.0 or ::.
QString bindAddr(const QString &host, int port);

// What the panel is showing. Ordered by how loudly it must be said, not by
// how the fields arrive.
enum class State {
    Unavailable, // this core has no remote.* RPCs, or the server failed to build
    Off,         // nothing is listening
    On,          // a TLS listener is up on a network interface
    Killed,      // kill switch engaged — the API answers 503
    Tampered,    // the audit chain does not verify
};

// Map a remote.status reply onto the panel's state. `available` is false when
// the RPC itself is missing (an older core), which outranks everything: with no
// RPCs there is no status to interpret.
State stateFor(bool available, const QJsonObject &status);

// The one sentence at the top of the panel. Never ambiguous about whether a
// network listener is running, which is the whole point of this surface.
QString headline(State state, const QString &addr);

// Strip the token out of a pairing URL. Anything that is not the pairing dialog
// itself must route a URL through this before it can be displayed or logged.
QString redactPairingUrl(const QString &url);

// The panel's own after-pairing confirmation, built from the whole reply so a
// test can prove the token cannot survive the call.
QString pairedConfirmation(const QJsonObject &pairReply);

// One line of the remote audit log.
QString auditLine(const QJsonObject &entry);

// --- QR ------------------------------------------------------------------
// A QR symbol as a square bit matrix, dark == true, WITHOUT the quiet zone.
struct QrMatrix {
    int size = 0;
    QVector<bool> modules; // row-major, size*size
    bool isValid() const { return size > 0 && modules.size() == size * size; }
    bool dark(int row, int col) const;
};

// Encode `data` as a byte-mode QR symbol at error-correction level M, choosing
// the smallest of versions 1-10 that fits (up to 213 bytes — a pairing URL is
// about 76). Returns an invalid matrix if the payload is too long; the caller
// then falls back to showing the URL as text.
QrMatrix encodeQr(const QByteArray &data);

// Render a matrix to a black-on-white image with the mandatory 4-module quiet
// zone, scaled up to roughly `targetPixels` square with nearest-neighbour so
// the modules stay crisp.
QImage renderQr(const QrMatrix &matrix, int targetPixels);

} // namespace RemoteLogic

// PairingDialog is the ONE place a pairing token is ever displayed. It receives
// the URL on the stack, shows it as a QR code plus selectable text, and keeps no
// copy: nothing in this class outlives exec().
class PairingDialog : public QDialog
{
    Q_OBJECT
public:
    PairingDialog(const QString &deviceName, const QString &pairingUrl,
                  const QString &certFingerprint, QWidget *parent = nullptr);
};

// RemotePanel is the user's control surface for remote access (plan 18 B5): the
// on/off switch for the core's HTTPS listener, which interface it binds, the
// paired devices and their revocation, the kill switch, and the audit log.
//
// It is CoworkPanel's sibling and deliberately shares its posture: the authority
// lives in the core (every remote.* RPC is requireUI-gated), this panel renders
// that authority and relays the user's decisions, and the audit log is framed as
// tamper *detection* rather than prevention.
//
// The core broadcasts nothing for remote.* — there is no remote.statusChanged —
// so this panel polls while it is visible instead of pretending to be live.
class RemotePanel : public QWidget
{
    Q_OBJECT
public:
    explicit RemotePanel(CoreClient *core, QWidget *parent = nullptr);

    // --- seams for the headless test -------------------------------------
    // applyStatus/applyStatusError are the only two ways state reaches this
    // widget, so driving them directly exercises every visual branch with no
    // core, no socket and no event loop.
    void applyStatus(const QJsonObject &status);
    void applyStatusError(const QJsonObject &error);
    RemoteLogic::State state() const { return m_state; }

public Q_SLOTS:
    void refresh();

protected:
    void showEvent(QShowEvent *event) override;
    void hideEvent(QHideEvent *event) override;

private Q_SLOTS:
    void toggleEnabled();
    void pairDevice();
    void toggleKill();
    void showAuditLog();

private:
    void rebuildInterfaces();
    void renderDevices(const QJsonArray &devices);
    void revokeDevice(const QString &deviceId, const QString &name);
    void applyState();
    // messageType is a KMessageWidget::MessageType, kept as an int so this
    // header need not pull in the KF6 widget.
    void setNotice(const QString &text, int messageType);
    void rememberBindChoice();

    CoreClient *m_core = nullptr;

    RemoteLogic::State m_state = RemoteLogic::State::Off;
    bool m_available = true; // false once remote.* answers "method not found"
    bool m_enabled = false;
    bool m_killed = false;
    bool m_tampered = false;
    bool m_bindChoiceLoaded = false;
    QString m_addr;
    QString m_fingerprint;

    KMessageWidget *m_status = nullptr; // the state banner, driven by applyState
    KMessageWidget *m_notice = nullptr; // transient feedback for one action
    QLabel *m_headline = nullptr;
    QComboBox *m_iface = nullptr;
    QSpinBox *m_port = nullptr;
    QToolButton *m_rescanBtn = nullptr;
    QPushButton *m_toggleBtn = nullptr;
    QLabel *m_ifaceHint = nullptr;
    QLabel *m_certLabel = nullptr;
    QVBoxLayout *m_devicesLayout = nullptr;
    QLabel *m_devicesEmpty = nullptr;
    QPushButton *m_pairBtn = nullptr;
    QPushButton *m_killBtn = nullptr;
    QPushButton *m_auditBtn = nullptr;
    QTimer *m_poll = nullptr;

    // The audit view lives only while its dialog is open (CoworkPanel's shape).
    QPlainTextEdit *m_audit = nullptr;
    KMessageWidget *m_auditWarning = nullptr;
    QJsonArray m_auditEntries;
    void refreshAudit();
    void renderAudit();
};
