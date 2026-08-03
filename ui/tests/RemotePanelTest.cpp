// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// Headless coverage for the Remote Access panel (plan 18 B5).
//
// Everything here runs with no akcore, no socket and no network: the panel is
// driven through applyStatus/applyStatusError, and RemoteLogic is pure. The
// three properties worth pinning are
//
//   1. the bind choice — a wildcard is unreachable, container plumbing never
//      appears, and an encrypted overlay is offered first (security-model §7);
//   2. the state mapping — "is a network listener running" must never be
//      ambiguous, including when the kill switch or a broken audit chain
//      outranks it;
//   3. the token — it may appear in the pairing dialog and NOWHERE else, which
//      is asserted by walking every user-visible string the panel owns.

#include "ipc/CoreClient.h"
#include "remote/RemotePanel.h"

#include <KLocalizedString>

#include <QAbstractButton>
#include <QGroupBox>
#include <QJsonArray>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QPlainTextEdit>
#include <QtTest>

using RemoteLogic::Interface;
using RemoteLogic::State;

namespace {

Interface iface(const char *name, const char *address)
{
    Interface i;
    i.name = QString::fromLatin1(name);
    i.address = QString::fromLatin1(address);
    return i;
}

QStringList names(const QList<Interface> &list)
{
    QStringList out;
    for (const Interface &i : list) {
        out << i.name;
    }
    return out;
}

// Every string a user could read off the widget tree: label text, button text,
// line edits, plain-text views, tooltips, accessible names and window titles.
QStringList visibleStrings(const QWidget *root)
{
    QStringList out;
    QList<QWidget *> all = root->findChildren<QWidget *>();
    all.prepend(const_cast<QWidget *>(root));
    for (const QObject *obj : std::as_const(all)) {
        if (const auto *w = qobject_cast<const QWidget *>(obj)) {
            out << w->toolTip() << w->accessibleName() << w->accessibleDescription()
                << w->windowTitle() << w->statusTip() << w->whatsThis();
        }
        if (const auto *l = qobject_cast<const QLabel *>(obj)) {
            out << l->text();
        }
        if (const auto *b = qobject_cast<const QAbstractButton *>(obj)) {
            out << b->text();
        }
        if (const auto *e = qobject_cast<const QLineEdit *>(obj)) {
            out << e->text();
        }
        if (const auto *t = qobject_cast<const QPlainTextEdit *>(obj)) {
            out << t->toPlainText();
        }
        if (const auto *g = qobject_cast<const QGroupBox *>(obj)) {
            out << g->title();
        }
    }
    out.removeAll(QString());
    return out;
}

bool anyContains(const QStringList &haystack, const QString &needle)
{
    for (const QString &s : haystack) {
        if (s.contains(needle)) {
            return true;
        }
    }
    return false;
}

// The panel's action buttons are private members, so the test finds them the
// way a user does: by what they say.
QAbstractButton *buttonSaying(const QWidget *root, const QString &fragment)
{
    const auto buttons = root->findChildren<QAbstractButton *>();
    for (QAbstractButton *b : buttons) {
        if (b->text().contains(fragment)) {
            return b;
        }
    }
    return nullptr;
}

QJsonObject status(bool enabled, bool killed, bool tampered, const QString &addr = {})
{
    return QJsonObject{
        {QStringLiteral("enabled"), enabled},
        {QStringLiteral("killSwitch"), killed},
        {QStringLiteral("auditTampered"), tampered},
        {QStringLiteral("addr"), addr},
        {QStringLiteral("certFingerprint"), QStringLiteral("AA:BB:CC")},
        {QStringLiteral("devices"), QJsonArray{}},
    };
}

// The token as the core mints it: 256 bits, base64url. It must never leave the
// pairing dialog.
const QString kToken = QStringLiteral("dGhpcy1pcy1hLXRlc3QtdG9rZW4tMjU2LWJpdHMtbG9uZ19f");
const QString kPairingUrl = QStringLiteral("https://100.101.102.103:8443/#t=") + kToken;

} // namespace

class RemotePanelTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    // Without a domain every i18n() call warns; the app sets this in main().
    void initTestCase() { KLocalizedString::setApplicationDomain("agentkate"); }

    // interface ranking / filtering
    void dropsContainerAndBridgeNoise();
    void ranksOverlayFirstAndLoopbackLast();
    void recognisesTailscaleByAddressRange();
    void buildsBindAddrWithoutAnImplicitWildcard();

    // status -> widget state
    void mapsStatusToState();
    void headlineNamesTheAddressWhenOn();
    void panelShowsEachState();
    void degradesWhenTheCoreHasNoRemoteRpcs();
    void aTransientErrorDoesNotDisableThePanel();
    void listsPairedDevicesAndSkipsRevokedOnes();

    // the token
    void confirmationCarriesNoToken();
    void redactionDropsTheFragment();
    void tokenNeverReachesThePanel();
    void pairingDialogIsWhereTheTokenLives();

    // QR
    void encodesAPairingUrlAsAScannableSymbol();
    void refusesAPayloadItCannotEncode();
    void rendersAQuietZone();
};

// --- interface ranking ------------------------------------------------------

void RemotePanelTest::dropsContainerAndBridgeNoise()
{
    const QList<Interface> ranked = RemoteLogic::rankInterfaces({
        iface("docker0", "172.17.0.1"),
        iface("br-1a2b3c", "172.18.0.1"),
        iface("veth9f21", "10.0.0.9"),
        iface("virbr0", "192.168.122.1"),
        iface("wlan0", "192.168.1.20"),
    });
    QCOMPARE(names(ranked), QStringList{QStringLiteral("wlan0")});

    QVERIFY(RemoteLogic::isNoiseInterface(QStringLiteral("docker0")));
    QVERIFY(RemoteLogic::isNoiseInterface(QStringLiteral("br-abc")));
    QVERIFY(RemoteLogic::isNoiseInterface(QStringLiteral("veth123")));
    QVERIFY(RemoteLogic::isNoiseInterface(QStringLiteral("virbr0")));
    // A user's own bridge is NOT docker's: "br0" must survive.
    QVERIFY(!RemoteLogic::isNoiseInterface(QStringLiteral("br0")));
    QVERIFY(!RemoteLogic::isNoiseInterface(QStringLiteral("eth0")));
}

void RemotePanelTest::ranksOverlayFirstAndLoopbackLast()
{
    const QList<Interface> ranked = RemoteLogic::rankInterfaces({
        iface("lo", "127.0.0.1"),
        iface("eth0", "192.168.1.5"),
        iface("docker0", "172.17.0.1"),
        iface("tailscale0", "100.101.102.103"),
        iface("wlan0", "192.168.1.20"),
    });
    QCOMPARE(names(ranked),
             (QStringList{QStringLiteral("tailscale0"), QStringLiteral("eth0"),
                          QStringLiteral("wlan0"), QStringLiteral("lo")}));
    QVERIFY(ranked.first().overlay);
    QVERIFY(ranked.last().loopback);

    // The label must carry the *reason*, not just the ordering.
    QVERIFY(RemoteLogic::interfaceLabel(ranked.first()).contains(QStringLiteral("overlay")));
    QVERIFY(RemoteLogic::interfaceLabel(ranked.at(1)).contains(QStringLiteral("local network")));

    QVERIFY(RemoteLogic::isOverlayInterface(QStringLiteral("wg0"), QStringLiteral("10.9.8.7")));
    QVERIFY(RemoteLogic::isOverlayInterface(QStringLiteral("tun0"), QStringLiteral("10.9.8.7")));
    QVERIFY(!RemoteLogic::isOverlayInterface(QStringLiteral("eth0"), QStringLiteral("192.168.1.5")));
}

void RemotePanelTest::recognisesTailscaleByAddressRange()
{
    // Tailscale hands out 100.64.0.0/10, so a renamed interface is still ranked
    // as the overlay it is.
    QVERIFY(RemoteLogic::isOverlayInterface(QStringLiteral("ts-renamed"),
                                            QStringLiteral("100.90.1.2")));
    QVERIFY(!RemoteLogic::isOverlayInterface(QStringLiteral("eth0"),
                                             QStringLiteral("100.200.1.2")));
    const QList<Interface> ranked = RemoteLogic::rankInterfaces(
        {iface("eth0", "192.168.1.5"), iface("ts-renamed", "100.90.1.2")});
    QCOMPARE(ranked.first().name, QStringLiteral("ts-renamed"));
}

void RemotePanelTest::buildsBindAddrWithoutAnImplicitWildcard()
{
    QCOMPARE(RemoteLogic::bindAddr(QStringLiteral("100.101.102.103"), 8443),
             QStringLiteral("100.101.102.103:8443"));
    QCOMPARE(RemoteLogic::bindAddr(QStringLiteral("fd7a::1"), 8443),
             QStringLiteral("[fd7a::1]:8443"));
    // An all-adapter host is representable, but only the panel's separately
    // confirmed choice may send it with AllowAllInterfaces=true.
    QCOMPARE(RemoteLogic::bindAddr(QStringLiteral("0.0.0.0"), 8443),
             QStringLiteral("0.0.0.0:8443"));
    // An empty host must never become ":8443" — that is a wildcard bind, the one
    // thing the core refuses and the mistake that puts agents on a café network.
    QVERIFY(RemoteLogic::bindAddr(QString(), 8443).isEmpty());
    QVERIFY(RemoteLogic::bindAddr(QStringLiteral("   "), 8443).isEmpty());
    QVERIFY(RemoteLogic::bindAddr(QStringLiteral("192.168.1.5"), 0).isEmpty());
    QVERIFY(RemoteLogic::bindAddr(QStringLiteral("192.168.1.5"), 70000).isEmpty());
}

// --- status -> state --------------------------------------------------------

void RemotePanelTest::mapsStatusToState()
{
    QCOMPARE(RemoteLogic::stateFor(false, status(true, false, false)), State::Unavailable);
    QCOMPARE(RemoteLogic::stateFor(true, status(false, false, false)), State::Off);
    QCOMPARE(RemoteLogic::stateFor(true, status(true, false, false)), State::On);
    QCOMPARE(RemoteLogic::stateFor(true, status(true, true, false)), State::Killed);
    // A broken chain outranks everything, including "off": switching the
    // listener off does not un-edit the record of what a phone already did.
    QCOMPARE(RemoteLogic::stateFor(true, status(false, false, true)), State::Tampered);
    QCOMPARE(RemoteLogic::stateFor(true, status(true, true, true)), State::Tampered);
}

void RemotePanelTest::headlineNamesTheAddressWhenOn()
{
    const QString on =
        RemoteLogic::headline(State::On, QStringLiteral("100.101.102.103:8443"));
    QVERIFY(on.contains(QStringLiteral("ON")));
    QVERIFY(on.contains(QStringLiteral("100.101.102.103:8443")));
    QVERIFY(RemoteLogic::headline(State::Off, QString()).contains(QStringLiteral("OFF")));
    QVERIFY(!RemoteLogic::headline(State::Unavailable, QString()).isEmpty());

    // Killed and tampered must still answer "is a listener running" — the state
    // that outranks it does not make the question go away.
    QVERIFY(RemoteLogic::headline(State::Killed, QStringLiteral("10.0.0.1:8443"))
                .contains(QStringLiteral("10.0.0.1:8443")));
    QVERIFY(RemoteLogic::headline(State::Killed, QString())
                .contains(QStringLiteral("nothing is listening")));
    QVERIFY(RemoteLogic::headline(State::Tampered, QStringLiteral("10.0.0.1:8443"))
                .contains(QStringLiteral("is ON")));
    QVERIFY(RemoteLogic::headline(State::Tampered, QString())
                .contains(QStringLiteral("is OFF")));
}

void RemotePanelTest::panelShowsEachState()
{
    CoreClient core; // never started: no process, no socket, no calls leave here
    RemotePanel panel(&core);

    panel.applyStatus(status(false, false, false));
    QCOMPARE(panel.state(), State::Off);
    const QStringList offStrings = visibleStrings(&panel);
    QVERIFY(anyContains(offStrings, QStringLiteral("Remote access is OFF")));
    QVERIFY(!anyContains(offStrings, QStringLiteral("Remote access is ON")));

    panel.applyStatus(status(true, false, false, QStringLiteral("100.101.102.103:8443")));
    QCOMPARE(panel.state(), State::On);
    const QStringList onStrings = visibleStrings(&panel);
    QVERIFY(anyContains(onStrings, QStringLiteral("Remote access is ON")));
    QVERIFY(!anyContains(onStrings, QStringLiteral("Remote access is OFF")));
    QVERIFY(anyContains(onStrings, QStringLiteral("100.101.102.103:8443")));

    panel.applyStatus(status(true, true, false, QStringLiteral("100.101.102.103:8443")));
    QCOMPARE(panel.state(), State::Killed);
    // The kill switch must say what it does to a caller: 503, not silence.
    QVERIFY(anyContains(visibleStrings(&panel), QStringLiteral("503")));

    panel.applyStatus(status(true, false, true, QStringLiteral("100.101.102.103:8443")));
    QCOMPARE(panel.state(), State::Tampered);
    const QStringList tamperStrings = visibleStrings(&panel);
    QVERIFY(anyContains(tamperStrings, QStringLiteral("does not verify")));
    // Cowork's posture, in the same words: detection, not prevention.
    QVERIFY(anyContains(tamperStrings, QStringLiteral("not prevention")));
}

void RemotePanelTest::degradesWhenTheCoreHasNoRemoteRpcs()
{
    CoreClient core;
    RemotePanel panel(&core);
    panel.applyStatus(status(true, false, false, QStringLiteral("192.168.1.5:8443")));
    QCOMPARE(panel.state(), State::On);

    // -32601: an older akcore, with no remote.* surface at all.
    panel.applyStatusError(QJsonObject{{QStringLiteral("code"), -32601},
                                       {QStringLiteral("message"), QStringLiteral("method not found")}});
    QCOMPARE(panel.state(), State::Unavailable);
    QVERIFY(anyContains(visibleStrings(&panel), QStringLiteral("no remote access")));

    // Every control that would call a missing RPC is off — the panel degrades to
    // an explanation rather than four buttons that answer "method not found".
    for (const QString &label : {QStringLiteral("Turn remote access"), QStringLiteral("Pair a phone"),
                                 QStringLiteral("KILL SWITCH"), QStringLiteral("Remote activity log")}) {
        QAbstractButton *button = buttonSaying(&panel, label);
        QVERIFY2(button, qPrintable(QStringLiteral("no button saying: ") + label));
        QVERIFY2(!button->isEnabled(),
                 qPrintable(QStringLiteral("still enabled against an older core: ") + label));
    }
}

void RemotePanelTest::aTransientErrorDoesNotDisableThePanel()
{
    CoreClient core;
    RemotePanel panel(&core);
    panel.applyStatus(status(true, false, false, QStringLiteral("192.168.1.5:8443")));

    // A dropped socket is not "this core cannot do remote access"; the state
    // must survive it, or a blip would silently claim the listener is gone.
    panel.applyStatusError(QJsonObject{{QStringLiteral("code"), -32000},
                                       {QStringLiteral("message"), QStringLiteral("not connected to core")}});
    QCOMPARE(panel.state(), State::On);
    QVERIFY(anyContains(visibleStrings(&panel), QStringLiteral("not connected to core")));
}

void RemotePanelTest::listsPairedDevicesAndSkipsRevokedOnes()
{
    CoreClient core;
    RemotePanel panel(&core);
    QJsonObject s = status(true, false, false, QStringLiteral("192.168.1.5:8443"));
    s[QStringLiteral("devices")] = QJsonArray{
        QJsonObject{{QStringLiteral("id"), QStringLiteral("d-1")},
                    {QStringLiteral("name"), QStringLiteral("Galaxy S25 FE")},
                    {QStringLiteral("pairedAt"), QStringLiteral("2026-07-30T10:11:12Z")},
                    {QStringLiteral("revoked"), false}},
        QJsonObject{{QStringLiteral("id"), QStringLiteral("d-2")},
                    {QStringLiteral("name"), QStringLiteral("Old tablet")},
                    {QStringLiteral("pairedAt"), QStringLiteral("2026-06-01T10:11:12Z")},
                    {QStringLiteral("revoked"), true}},
    };
    panel.applyStatus(s);

    const QStringList strings = visibleStrings(&panel);
    QVERIFY(anyContains(strings, QStringLiteral("Galaxy S25 FE")));
    QVERIFY(!anyContains(strings, QStringLiteral("Old tablet")));
    // Revoking has to say that it drops live streams, not just future requests.
    QVERIFY(anyContains(strings, QStringLiteral("stream")));

    // Rebuilding the list must not leave stale rows behind.
    panel.applyStatus(status(true, false, false, QStringLiteral("192.168.1.5:8443")));
    QVERIFY(!anyContains(visibleStrings(&panel), QStringLiteral("Galaxy S25 FE")));
}

// --- the token --------------------------------------------------------------

void RemotePanelTest::confirmationCarriesNoToken()
{
    const QJsonObject reply{
        {QStringLiteral("token"), kToken},
        {QStringLiteral("pairingUrl"), kPairingUrl},
        {QStringLiteral("device"), QJsonObject{{QStringLiteral("id"), QStringLiteral("d-1")},
                                               {QStringLiteral("name"), QStringLiteral("Pixel")}}},
    };
    const QString confirmation = RemoteLogic::pairedConfirmation(reply);
    QVERIFY(confirmation.contains(QStringLiteral("Pixel")));
    QVERIFY(!confirmation.contains(kToken));
    QVERIFY(!confirmation.contains(QStringLiteral("#t=")));
    QVERIFY(!confirmation.contains(kPairingUrl));
}

void RemotePanelTest::redactionDropsTheFragment()
{
    QCOMPARE(RemoteLogic::redactPairingUrl(kPairingUrl),
             QStringLiteral("https://100.101.102.103:8443/"));
    QVERIFY(!RemoteLogic::redactPairingUrl(kPairingUrl).contains(kToken));
    // A URL with no fragment is returned unchanged rather than emptied.
    QCOMPARE(RemoteLogic::redactPairingUrl(QStringLiteral("https://host:1/")),
             QStringLiteral("https://host:1/"));
}

void RemotePanelTest::tokenNeverReachesThePanel()
{
    CoreClient core;
    RemotePanel panel(&core);
    QJsonObject s = status(true, false, false, QStringLiteral("100.101.102.103:8443"));
    s[QStringLiteral("devices")] =
        QJsonArray{QJsonObject{{QStringLiteral("id"), QStringLiteral("d-1")},
                               {QStringLiteral("name"), QStringLiteral("Pixel")},
                               {QStringLiteral("pairedAt"), QStringLiteral("2026-07-30T10:11:12Z")},
                               {QStringLiteral("revoked"), false}}};
    panel.applyStatus(s);

    // The panel has no API that takes a token, and nothing it renders is derived
    // from one: remote.status does not carry it, and the pairing reply is read
    // only inside the reply closure that opens the dialog.
    const QStringList strings = visibleStrings(&panel);
    QVERIFY(!anyContains(strings, kToken));
    QVERIFY(!anyContains(strings, QStringLiteral("#t=")));
}

void RemotePanelTest::pairingDialogIsWhereTheTokenLives()
{
    PairingDialog dlg(QStringLiteral("Pixel"), kPairingUrl, QStringLiteral("AA:BB:CC"));
    const QStringList strings = visibleStrings(&dlg);
    // The positive control: without this, the assertions above would pass even
    // if the URL were never shown anywhere at all.
    QVERIFY(anyContains(strings, kPairingUrl));
    // And the dialog must say the two things that make a one-time,
    // fragment-borne token safe to hand out.
    QVERIFY(anyContains(strings, QStringLiteral("shown once")));
    QVERIFY(anyContains(strings, QStringLiteral("fragment")));
    QVERIFY(anyContains(strings, QStringLiteral("self-signed")));
}

// --- QR ---------------------------------------------------------------------

void RemotePanelTest::encodesAPairingUrlAsAScannableSymbol()
{
    const RemoteLogic::QrMatrix m = RemoteLogic::encodeQr(kPairingUrl.toUtf8());
    QVERIFY(m.isValid());
    // 76 bytes needs version 5 (84 bytes at level M): 17 + 4*5 = 37 modules.
    QCOMPARE(m.size, 37);

    // The three finder patterns: a 7x7 ring with a 3x3 core, and a light
    // separator around it. If the placement were off by one these fail.
    const auto finderAt = [&m](int row, int col) {
        for (int dr = 0; dr < 7; ++dr) {
            for (int dc = 0; dc < 7; ++dc) {
                const int ring = qMax(qAbs(dr - 3), qAbs(dc - 3));
                if (m.dark(row + dr, col + dc) != (ring != 2)) {
                    return false;
                }
            }
        }
        return true;
    };
    QVERIFY(finderAt(0, 0));
    QVERIFY(finderAt(0, m.size - 7));
    QVERIFY(finderAt(m.size - 7, 0));

    // Timing patterns alternate, starting dark, along row 6 and column 6.
    for (int i = 8; i < m.size - 8; ++i) {
        QCOMPARE(m.dark(6, i), i % 2 == 0);
        QCOMPARE(m.dark(i, 6), i % 2 == 0);
    }
    // The always-dark module below the bottom-left finder.
    QVERIFY(m.dark(m.size - 8, 8));

    // Deterministic: the same payload must not produce a different symbol on a
    // second call (a stateful encoder would be a nasty way to fail).
    const RemoteLogic::QrMatrix again = RemoteLogic::encodeQr(kPairingUrl.toUtf8());
    QCOMPARE(again.modules, m.modules);

    // A short payload drops to a smaller version; a long one grows.
    QCOMPARE(RemoteLogic::encodeQr(QByteArray("hi")).size, 21);      // version 1
    QCOMPARE(RemoteLogic::encodeQr(QByteArray(200, 'x')).size, 57);  // version 10
}

void RemotePanelTest::refusesAPayloadItCannotEncode()
{
    // Beyond version 10 at level M. The dialog falls back to showing the link as
    // text rather than drawing a broken symbol.
    const RemoteLogic::QrMatrix m = RemoteLogic::encodeQr(QByteArray(400, 'x'));
    QVERIFY(!m.isValid());
    QVERIFY(RemoteLogic::renderQr(m, 320).isNull());
}

void RemotePanelTest::rendersAQuietZone()
{
    const RemoteLogic::QrMatrix m = RemoteLogic::encodeQr(QByteArray("agent kate"));
    const QImage img = RemoteLogic::renderQr(m, 320);
    QVERIFY(!img.isNull());
    QVERIFY(img.width() == img.height());
    // Scaled up in whole modules, quiet zone included.
    const int modules = m.size + 8;
    QCOMPARE(img.width() % modules, 0);
    // Black on white regardless of the app's theme: a dark-themed QR does not
    // scan. The corners are quiet zone, so they are white.
    QCOMPARE(img.pixelColor(0, 0), QColor(Qt::white));
    QCOMPARE(img.pixelColor(img.width() - 1, img.height() - 1), QColor(Qt::white));
    // ...and the top-left finder, past the quiet zone, is black.
    const int scale = img.width() / modules;
    QCOMPARE(img.pixelColor(4 * scale, 4 * scale), QColor(Qt::black));
}

QTEST_MAIN(RemotePanelTest)
#include "RemotePanelTest.moc"
