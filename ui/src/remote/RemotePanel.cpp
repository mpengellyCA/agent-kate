// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "remote/RemotePanel.h"

#include "ipc/CoreClient.h"
#include "shell/FlowLayout.h"

#include <KColorScheme>
#include <KConfigGroup>
#include <KLocalizedString>
#include <KMessageBox>
#include <KMessageWidget>
#include <KSharedConfig>

#include <QAbstractSocket>
#include <QClipboard>
#include <QComboBox>
#include <QDateTime>
#include <QDialogButtonBox>
#include <QFont>
#include <QFrame>
#include <QGroupBox>
#include <QGuiApplication>
#include <QHBoxLayout>
#include <QHostAddress>
#include <QIcon>
#include <QInputDialog>
#include <QJsonValue>
#include <QLabel>
#include <QLineEdit>
#include <QLocale>
#include <QNetworkInterface>
#include <QPalette>
#include <QPixmap>
#include <QPlainTextEdit>
#include <QPointer>
#include <QPushButton>
#include <QScrollArea>
#include <QSizePolicy>
#include <QSpinBox>
#include <QTimer>
#include <QToolButton>
#include <QVBoxLayout>

#include <algorithm>
#include <cstdlib>
#include <utility>

namespace {

// JSON-RPC "method not found". The one error code that means something
// permanent: this core simply has no remote.* surface, so the panel must stop
// asking rather than blink a transient warning every poll.
constexpr int kMethodNotFound = -32601;

// How often the panel re-reads remote.status while it is visible. The core
// broadcasts nothing for remote.* (there is no remote.statusChanged, and
// Server::SetOnDevicesChanged has no caller), so a phone exchanging its token or
// a `scripts/ak-remote.py` invocation would otherwise leave this panel showing a
// stale answer to "is a network listener running". Polling only while visible
// keeps that honest without costing anything when the panel is closed.
constexpr int kPollIntervalMs = 8000;

// Tint a hint label with the muted Mid role via its palette rather than a
// per-widget stylesheet, which is the app's palette-only theming convention
// (mirrors CoworkPanel::tintHint).
void tintHint(QLabel *label)
{
    QPalette p = label->palette();
    p.setColor(QPalette::WindowText, p.color(QPalette::Mid));
    label->setPalette(p);
}

// Paint a widget's foreground with a KColorScheme role, so "this is dangerous"
// and "this is running" read correctly under every colour scheme instead of
// being hardcoded red.
void tintForeground(QWidget *w, KColorScheme::ForegroundRole role)
{
    const KColorScheme scheme(QPalette::Active, KColorScheme::View);
    QPalette p = w->palette();
    const QBrush brush = scheme.foreground(role);
    p.setBrush(QPalette::WindowText, brush);
    p.setBrush(QPalette::ButtonText, brush);
    w->setPalette(p);
}

// ---------------------------------------------------------------------------
// QR encoder — ISO/IEC 18004, byte mode, error correction level M, versions 1-10
// ---------------------------------------------------------------------------
//
// Written out here rather than taken as a dependency. The alternative to a
// scannable code is typing a 256-bit token into a phone, so the QR is not
// optional; but adding a library to CMake (or requiring `qrencode` to be
// installed) for ~250 lines of fully specified arithmetic would make a core
// pairing flow depend on the packaging of the machine it runs on. The structure
// below follows the spec's own order — bitstream, Reed-Solomon, interleave,
// place, mask — so each step can be checked against it.

struct EccSpec {
    int totalCodewords;
    int ecPerBlock;
    int group1Blocks;
    int group1Data;
    int group2Blocks;
    int group2Data;
};

// Level M only, versions 1-10 (14 to 213 payload bytes). A pairing URL —
// "https://" + host:port + "/#t=" + 43 base64url characters — is about 76.
constexpr EccSpec kEcc[10] = {
    {26, 10, 1, 16, 0, 0},   {44, 16, 1, 28, 0, 0},   {70, 26, 1, 44, 0, 0},
    {100, 18, 2, 32, 0, 0},  {134, 24, 2, 43, 0, 0},  {172, 16, 4, 27, 0, 0},
    {196, 18, 4, 31, 0, 0},  {242, 22, 2, 38, 2, 39}, {292, 22, 3, 36, 2, 37},
    {346, 26, 4, 43, 1, 44},
};
constexpr int kMaxVersion = 10;

int qrDataCodewords(int version)
{
    const EccSpec &s = kEcc[version - 1];
    return s.group1Blocks * s.group1Data + s.group2Blocks * s.group2Data;
}

// Byte mode's character-count field is 8 bits through version 9 and 16 bits from
// version 10. Getting this wrong produces a symbol that scans as garbage rather
// than one that fails to scan, so it is isolated here.
int qrCountBits(int version)
{
    return version < 10 ? 8 : 16;
}

// Alignment-pattern centre coordinates per version (spec table E.1).
QVector<int> qrAlignCentres(int version)
{
    switch (version) {
    case 1:  return {};
    case 2:  return {6, 18};
    case 3:  return {6, 22};
    case 4:  return {6, 26};
    case 5:  return {6, 30};
    case 6:  return {6, 34};
    case 7:  return {6, 22, 38};
    case 8:  return {6, 24, 42};
    case 9:  return {6, 26, 46};
    default: return {6, 28, 50};
    }
}

// GF(2^8) with QR's primitive polynomial x^8+x^4+x^3+x^2+1 (0x11D).
struct GaloisField {
    quint8 exp[512] = {};
    quint8 log[256] = {};
    GaloisField()
    {
        int x = 1;
        for (int i = 0; i < 255; ++i) {
            exp[i] = quint8(x);
            log[x] = quint8(i);
            x <<= 1;
            if (x & 0x100) {
                x ^= 0x11D;
            }
        }
        for (int i = 255; i < 512; ++i) {
            exp[i] = exp[i - 255];
        }
    }
    quint8 mul(quint8 a, quint8 b) const
    {
        return (a == 0 || b == 0) ? quint8(0) : exp[int(log[a]) + int(log[b])];
    }
};

const GaloisField &gf()
{
    static const GaloisField g;
    return g;
}

// The Reed-Solomon generator polynomial of the given degree, highest power
// first: the product of (x - a^i) for i in [0, degree).
QVector<quint8> qrGenerator(int degree)
{
    QVector<quint8> poly{1};
    for (int i = 0; i < degree; ++i) {
        QVector<quint8> next(poly.size() + 1, 0);
        for (int j = 0; j < poly.size(); ++j) {
            next[j] ^= poly[j];                                  // multiply by x
            next[j + 1] ^= gf().mul(poly[j], gf().exp[i]);        // and by a^i
        }
        poly = next;
    }
    return poly;
}

// The error-correction codewords for one block: the remainder of the data
// polynomial divided by the generator.
QVector<quint8> qrRemainder(const QVector<quint8> &data, int ecLen)
{
    const QVector<quint8> gen = qrGenerator(ecLen);
    QVector<quint8> rem(ecLen, 0);
    for (quint8 b : data) {
        const quint8 factor = b ^ rem[0];
        rem.removeFirst();
        rem.append(0);
        for (int i = 0; i < ecLen; ++i) {
            rem[i] ^= gf().mul(gen[i + 1], factor);
        }
    }
    return rem;
}

// The symbol under construction. `reserved` marks function modules: they are
// never masked and never carry data.
struct QrCanvas {
    int size = 0;
    QVector<bool> dark;
    QVector<bool> reserved;

    explicit QrCanvas(int version)
        : size(17 + 4 * version), dark(size * size, false), reserved(size * size, false)
    {
    }
    void put(int row, int col, bool isDark, bool isFunction)
    {
        if (row < 0 || col < 0 || row >= size || col >= size) {
            return;
        }
        dark[row * size + col] = isDark;
        if (isFunction) {
            reserved[row * size + col] = true;
        }
    }
    bool at(int row, int col) const { return dark[row * size + col]; }
    bool isFunction(int row, int col) const { return reserved[row * size + col]; }
};

void qrDrawFinder(QrCanvas &cv, int centreRow, int centreCol)
{
    // Nine modules across so the light separator is drawn in the same pass.
    for (int dr = -4; dr <= 4; ++dr) {
        for (int dc = -4; dc <= 4; ++dc) {
            const int dist = std::max(std::abs(dr), std::abs(dc));
            cv.put(centreRow + dr, centreCol + dc, dist != 2 && dist != 4, true);
        }
    }
}

void qrDrawAlignment(QrCanvas &cv, int centreRow, int centreCol)
{
    for (int dr = -2; dr <= 2; ++dr) {
        for (int dc = -2; dc <= 2; ++dc) {
            cv.put(centreRow + dr, centreCol + dc,
                   std::max(std::abs(dr), std::abs(dc)) != 1, true);
        }
    }
}

bool qrBit(int value, int index)
{
    return ((value >> index) & 1) != 0;
}

// Format information: 5 data bits (EC level M == 00, then the mask) protected by
// a BCH(15,5) code and XORed with 0x5412. Computed rather than tabulated so a
// mistyped table entry cannot silently produce an unreadable symbol.
void qrDrawFormat(QrCanvas &cv, int mask)
{
    const int data = (0b00 << 3) | mask; // 0b00 == error-correction level M
    int rem = data;
    for (int i = 0; i < 10; ++i) {
        rem = (rem << 1) ^ ((rem >> 9) * 0x537);
    }
    const int bits = ((data << 10) | rem) ^ 0x5412;

    for (int i = 0; i <= 5; ++i) {
        cv.put(i, 8, qrBit(bits, i), true);
    }
    cv.put(7, 8, qrBit(bits, 6), true);
    cv.put(8, 8, qrBit(bits, 7), true);
    cv.put(8, 7, qrBit(bits, 8), true);
    for (int i = 9; i < 15; ++i) {
        cv.put(8, 14 - i, qrBit(bits, i), true);
    }
    for (int i = 0; i < 8; ++i) {
        cv.put(8, cv.size - 1 - i, qrBit(bits, i), true);
    }
    for (int i = 8; i < 15; ++i) {
        cv.put(cv.size - 15 + i, 8, qrBit(bits, i), true);
    }
    cv.put(cv.size - 8, 8, true, true); // the always-dark module
}

// Version information (versions 7 and up): 6 version bits under a BCH(18,6)
// code, mirrored into the two corners.
void qrDrawVersion(QrCanvas &cv, int version)
{
    if (version < 7) {
        return;
    }
    int rem = version;
    for (int i = 0; i < 12; ++i) {
        rem = (rem << 1) ^ ((rem >> 11) * 0x1F25);
    }
    const int bits = (version << 12) | rem;
    for (int i = 0; i < 18; ++i) {
        const bool bit = qrBit(bits, i);
        const int a = cv.size - 11 + i % 3;
        const int b = i / 3;
        cv.put(b, a, bit, true);
        cv.put(a, b, bit, true);
    }
}

void qrDrawFunctionPatterns(QrCanvas &cv, int version)
{
    for (int i = 0; i < cv.size; ++i) {
        cv.put(6, i, i % 2 == 0, true); // horizontal timing
        cv.put(i, 6, i % 2 == 0, true); // vertical timing
    }
    qrDrawFinder(cv, 3, 3);
    qrDrawFinder(cv, 3, cv.size - 4);
    qrDrawFinder(cv, cv.size - 4, 3);

    const QVector<int> centres = qrAlignCentres(version);
    const int last = int(centres.size()) - 1;
    for (int i = 0; i <= last; ++i) {
        for (int j = 0; j <= last; ++j) {
            // The three corners are finder patterns already.
            if ((i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0)) {
                continue;
            }
            qrDrawAlignment(cv, centres[i], centres[j]);
        }
    }
    // Reserve the format/version areas by drawing them once with mask 0; the
    // real format bits are written after the mask is chosen.
    qrDrawFormat(cv, 0);
    qrDrawVersion(cv, version);
}

void qrDrawCodewords(QrCanvas &cv, const QVector<quint8> &data)
{
    int bit = 0;
    const int total = int(data.size()) * 8;
    for (int right = cv.size - 1; right >= 1; right -= 2) {
        if (right == 6) {
            right = 5; // column 6 is the vertical timing pattern
        }
        for (int vert = 0; vert < cv.size; ++vert) {
            for (int j = 0; j < 2; ++j) {
                const int col = right - j;
                const bool upward = ((right + 1) & 2) == 0;
                const int row = upward ? cv.size - 1 - vert : vert;
                if (!cv.isFunction(row, col) && bit < total) {
                    cv.put(row, col, qrBit(data[bit >> 3], 7 - (bit & 7)), false);
                    ++bit;
                }
            }
        }
    }
}

bool qrMaskBit(int mask, int row, int col)
{
    switch (mask) {
    case 0:  return (row + col) % 2 == 0;
    case 1:  return row % 2 == 0;
    case 2:  return col % 3 == 0;
    case 3:  return (row + col) % 3 == 0;
    case 4:  return (row / 2 + col / 3) % 2 == 0;
    case 5:  return (row * col) % 2 + (row * col) % 3 == 0;
    case 6:  return ((row * col) % 2 + (row * col) % 3) % 2 == 0;
    default: return ((row + col) % 2 + (row * col) % 3) % 2 == 0;
    }
}

void qrApplyMask(QrCanvas &cv, int mask)
{
    for (int r = 0; r < cv.size; ++r) {
        for (int c = 0; c < cv.size; ++c) {
            if (!cv.isFunction(r, c) && qrMaskBit(mask, r, c)) {
                cv.dark[r * cv.size + c] = !cv.dark[r * cv.size + c];
            }
        }
    }
}

// The spec's four penalty rules, used only to pick between the eight masks —
// every mask yields a valid symbol, so an imprecision here costs scanning
// margin, never correctness.
int qrPenalty(const QrCanvas &cv)
{
    const int n = cv.size;
    int score = 0;

    // Rule 1 — runs of five or more same-coloured modules.
    for (int pass = 0; pass < 2; ++pass) {
        for (int a = 0; a < n; ++a) {
            int run = 1;
            for (int b = 1; b < n; ++b) {
                const bool cur = pass == 0 ? cv.at(a, b) : cv.at(b, a);
                const bool prev = pass == 0 ? cv.at(a, b - 1) : cv.at(b - 1, a);
                if (cur == prev) {
                    ++run;
                } else {
                    if (run >= 5) {
                        score += 3 + (run - 5);
                    }
                    run = 1;
                }
            }
            if (run >= 5) {
                score += 3 + (run - 5);
            }
        }
    }

    // Rule 2 — 2x2 blocks of one colour.
    for (int r = 0; r + 1 < n; ++r) {
        for (int c = 0; c + 1 < n; ++c) {
            const bool v = cv.at(r, c);
            if (v == cv.at(r, c + 1) && v == cv.at(r + 1, c) && v == cv.at(r + 1, c + 1)) {
                score += 3;
            }
        }
    }

    // Rule 3 — the finder-lookalike 1:1:3:1:1 sequence with four light modules
    // on one side, in either direction, in any row or column.
    static const bool pattern[11] = {true,  false, true,  true, true, false,
                                     true,  false, false, false, false};
    for (int pass = 0; pass < 2; ++pass) {
        for (int a = 0; a < n; ++a) {
            for (int b = 0; b + 10 < n; ++b) {
                bool forward = true;
                bool backward = true;
                for (int k = 0; k < 11; ++k) {
                    const bool m = pass == 0 ? cv.at(a, b + k) : cv.at(b + k, a);
                    if (m != pattern[k]) {
                        forward = false;
                    }
                    if (m != pattern[10 - k]) {
                        backward = false;
                    }
                }
                if (forward) {
                    score += 40;
                }
                if (backward) {
                    score += 40;
                }
            }
        }
    }

    // Rule 4 — deviation of the dark-module proportion from 50%.
    int darkCount = 0;
    for (bool d : cv.dark) {
        if (d) {
            ++darkCount;
        }
    }
    const int percent = darkCount * 100 / (n * n);
    score += (std::abs(percent - 50) / 5) * 10;
    return score;
}

} // namespace

// ---------------------------------------------------------------------------
// RemoteLogic
// ---------------------------------------------------------------------------

namespace RemoteLogic
{

bool QrMatrix::dark(int row, int col) const
{
    if (!isValid() || row < 0 || col < 0 || row >= size || col >= size) {
        return false;
    }
    return modules.at(row * size + col);
}

bool isNoiseInterface(const QString &name)
{
    // Container, VM and bridge plumbing. Every one of these is reachable only
    // from software on this machine, so offering it as "where your phone
    // connects" produces a listener nobody can find.
    static const char *const kNoise[] = {"docker", "br-",    "veth",  "virbr", "vnet",
                                         "podman", "cni-",   "lxcbr", "vmnet", "kube",
                                         "flannel", "tap"};
    for (const char *prefix : kNoise) {
        if (name.startsWith(QLatin1String(prefix), Qt::CaseInsensitive)) {
            return true;
        }
    }
    return false;
}

bool isOverlayInterface(const QString &name, const QString &address)
{
    static const char *const kOverlay[] = {"tailscale", "wg", "tun", "utun", "zt", "nebula"};
    for (const char *prefix : kOverlay) {
        if (name.startsWith(QLatin1String(prefix), Qt::CaseInsensitive)) {
            return true;
        }
    }
    // Second signal: Tailscale hands out 100.64.0.0/10 (CGNAT), so a renamed or
    // userspace-mode interface is still recognised for what it is.
    const QStringList octets = address.split(QLatin1Char('.'));
    if (octets.size() == 4 && octets.at(0) == QLatin1String("100")) {
        const int second = octets.at(1).toInt();
        return second >= 64 && second <= 127;
    }
    return false;
}

QList<Interface> rankInterfaces(const QList<Interface> &found)
{
    QList<Interface> kept;
    for (const Interface &raw : found) {
        if (raw.address.isEmpty()) {
            continue;
        }
        Interface item = raw;
        item.loopback = raw.loopback || item.name == QLatin1String("lo")
            || item.address.startsWith(QLatin1String("127."));
        item.overlay = isOverlayInterface(item.name, item.address);
        // Loopback is never noise, whatever it is called; noise is dropped
        // outright rather than ranked last, because a docker0 address in this
        // list is a wrong answer, not a worse one.
        if (!item.loopback && isNoiseInterface(item.name)) {
            continue;
        }
        kept.append(item);
    }

    // Overlay first (docs/security-model.md §7: the supported answer off a
    // network you control), then the LAN, then loopback — which binds nothing
    // reachable but is exactly right behind an SSH or `tailscale serve` tunnel.
    // Stable, so the machine's own interface order breaks ties and the list does
    // not reshuffle between refreshes.
    std::stable_sort(kept.begin(), kept.end(), [](const Interface &a, const Interface &b) {
        const auto rank = [](const Interface &i) { return i.overlay ? 0 : (i.loopback ? 2 : 1); };
        return rank(a) < rank(b);
    });
    return kept;
}

QList<Interface> localInterfaces()
{
    QList<Interface> found;
    const QList<QNetworkInterface> ifaces = QNetworkInterface::allInterfaces();
    for (const QNetworkInterface &ni : ifaces) {
        const QNetworkInterface::InterfaceFlags flags = ni.flags();
        if (!flags.testFlag(QNetworkInterface::IsUp) || !flags.testFlag(QNetworkInterface::IsRunning)) {
            continue;
        }
        const QList<QNetworkAddressEntry> entries = ni.addressEntries();
        for (const QNetworkAddressEntry &entry : entries) {
            const QHostAddress ip = entry.ip();
            const auto protocol = ip.protocol();
            if (protocol != QAbstractSocket::IPv4Protocol
                && protocol != QAbstractSocket::IPv6Protocol) {
                continue;
            }
            Interface item;
            item.name = ni.humanReadableName();
            item.address = ip.toString();
            // A scoped IPv6 address is a legitimate local adapter address. The
            // zone is part of the host spelling, otherwise a link-local address
            // names no interface at all.
            if (protocol == QAbstractSocket::IPv6Protocol && !ip.scopeId().isEmpty()) {
                item.address += QLatin1Char('%') + ip.scopeId();
            }
            item.loopback = flags.testFlag(QNetworkInterface::IsLoopBack);
            found.append(item);
        }
    }
    return rankInterfaces(found);
}

QString interfaceLabel(const Interface &iface)
{
    if (iface.overlay) {
        return i18n("%1 — %2  (encrypted overlay: the right choice off your own network)",
                    iface.name, iface.address);
    }
    if (iface.loopback) {
        return i18n("%1 — %2  (this machine only: for an SSH or tailscale serve tunnel)",
                    iface.name, iface.address);
    }
    return i18n("%1 — %2  (local network)", iface.name, iface.address);
}

QString bindAddr(const QString &host, int port)
{
    // An empty host returns an empty string rather than ":port", because ":port"
    // is an implicit wildcard bind. Explicit all-adapter choices are handled
    // separately, with a confirmation and AllowAllInterfaces=true.
    if (host.trimmed().isEmpty() || port <= 0 || port > 65535) {
        return {};
    }
    if (host.contains(QLatin1Char(':'))) {
        return QStringLiteral("[%1]:%2").arg(host).arg(port); // IPv6 literal
    }
    return QStringLiteral("%1:%2").arg(host).arg(port);
}

State stateFor(bool available, const QJsonObject &status)
{
    if (!available) {
        return State::Unavailable;
    }
    // Ordered by how loudly each must be said. A broken audit chain outranks
    // everything, including "off": the record of what a phone already did has
    // been edited, and switching the listener off does not un-edit it.
    if (status.value(QStringLiteral("auditTampered")).toBool()) {
        return State::Tampered;
    }
    if (status.value(QStringLiteral("killSwitch")).toBool()) {
        return State::Killed;
    }
    return status.value(QStringLiteral("enabled")).toBool() ? State::On : State::Off;
}

QString headline(State state, const QString &addr)
{
    switch (state) {
    case State::Unavailable:
        return i18n("Remote access is unavailable.");
    case State::Off:
        return i18n("Remote access is OFF — nothing is listening.");
    case State::On:
        return addr.isEmpty() ? i18n("Remote access is ON.")
                              : i18n("Remote access is ON — https://%1", addr);
    // Killed and Tampered still have to answer "is a network listener running",
    // because that is the question this headline exists for. The core reports an
    // empty address exactly when nothing is bound, so it doubles as the answer.
    case State::Killed:
        return addr.isEmpty()
            ? i18n("Kill switch ENGAGED — nothing is listening, and every remote request is refused.")
            : i18n("Kill switch ENGAGED — the listener at https://%1 refuses everything.", addr);
    case State::Tampered:
        return addr.isEmpty()
            ? i18n("The remote audit log does not verify. Remote access is OFF.")
            : i18n("The remote audit log does not verify. Remote access is ON — https://%1", addr);
    }
    return {};
}

QString redactPairingUrl(const QString &url)
{
    // The token rides in the fragment, so dropping everything from '#' drops the
    // token. Anything outside the pairing dialog that wants to show a pairing
    // URL goes through here.
    const int hash = url.indexOf(QLatin1Char('#'));
    return hash < 0 ? url : url.left(hash);
}

QString pairedConfirmation(const QJsonObject &pairReply)
{
    // Deliberately built from the device name alone. The reply also carries
    // `token` and `pairingUrl`; this function reads neither, which is what makes
    // "the token never reaches the panel" a property of the code rather than a
    // habit.
    const QString name =
        pairReply.value(QStringLiteral("device")).toObject().value(QStringLiteral("name")).toString();
    if (name.isEmpty()) {
        return i18n("Device paired. The pairing link was shown once and is not stored.");
    }
    return i18n("“%1” is paired. The pairing link was shown once and is not stored.", name);
}

QString auditLine(const QJsonObject &entry)
{
    const QDateTime at =
        QDateTime::fromString(entry.value(QStringLiteral("at")).toString(), Qt::ISODateWithMs);
    const QString when =
        at.isValid() ? at.toLocalTime().toString(QStringLiteral("yyyy-MM-dd HH:mm:ss")) : QString();
    QString line = QStringLiteral("%1  %2")
                       .arg(when, entry.value(QStringLiteral("kind")).toString());
    const QString device = entry.value(QStringLiteral("deviceName")).toString();
    if (!device.isEmpty()) {
        line += QStringLiteral("  ") + device;
    }
    const QString thread = entry.value(QStringLiteral("threadId")).toString();
    if (!thread.isEmpty()) {
        line += QStringLiteral("  ") + thread;
    }
    const QString detail = entry.value(QStringLiteral("detail")).toString();
    if (!detail.isEmpty()) {
        line += QStringLiteral("  ") + detail;
    }
    const QString outcome = entry.value(QStringLiteral("outcome")).toString();
    if (!outcome.isEmpty()) {
        line += QStringLiteral("  [") + outcome + QLatin1Char(']');
    }
    return line;
}

QrMatrix encodeQr(const QByteArray &data)
{
    int version = 0;
    for (int v = 1; v <= kMaxVersion; ++v) {
        if (4 + qrCountBits(v) + 8 * int(data.size()) <= qrDataCodewords(v) * 8) {
            version = v;
            break;
        }
    }
    if (version == 0) {
        return {}; // too long for version 10; the caller falls back to text
    }

    // --- bitstream ---------------------------------------------------------
    const int capacityBits = qrDataCodewords(version) * 8;
    QVector<bool> bits;
    bits.reserve(capacityBits);
    const auto append = [&bits](int value, int count) {
        for (int i = count - 1; i >= 0; --i) {
            bits.append(qrBit(value, i));
        }
    };
    append(0b0100, 4); // byte mode
    append(int(data.size()), qrCountBits(version));
    for (char c : data) {
        append(quint8(c), 8);
    }
    for (int i = 0; i < 4 && bits.size() < capacityBits; ++i) {
        bits.append(false); // terminator
    }
    while (bits.size() % 8 != 0) {
        bits.append(false);
    }
    QVector<quint8> codewords;
    codewords.reserve(qrDataCodewords(version));
    for (int i = 0; i < bits.size(); i += 8) {
        quint8 b = 0;
        for (int j = 0; j < 8; ++j) {
            b = quint8((b << 1) | (bits.at(i + j) ? 1 : 0));
        }
        codewords.append(b);
    }
    for (int pad = 0; codewords.size() < qrDataCodewords(version); ++pad) {
        codewords.append(pad % 2 == 0 ? 0xEC : 0x11);
    }

    // --- blocks, error correction, interleave ------------------------------
    const EccSpec &spec = kEcc[version - 1];
    QVector<QVector<quint8>> dataBlocks;
    QVector<QVector<quint8>> ecBlocks;
    int offset = 0;
    const auto addBlock = [&](int len) {
        QVector<quint8> block = codewords.mid(offset, len);
        offset += len;
        ecBlocks.append(qrRemainder(block, spec.ecPerBlock));
        dataBlocks.append(block);
    };
    for (int i = 0; i < spec.group1Blocks; ++i) {
        addBlock(spec.group1Data);
    }
    for (int i = 0; i < spec.group2Blocks; ++i) {
        addBlock(spec.group2Data);
    }

    QVector<quint8> interleaved;
    interleaved.reserve(spec.totalCodewords);
    const int longest = std::max(spec.group1Data, spec.group2Data);
    for (int i = 0; i < longest; ++i) {
        for (const QVector<quint8> &block : std::as_const(dataBlocks)) {
            if (i < block.size()) {
                interleaved.append(block.at(i));
            }
        }
    }
    for (int i = 0; i < spec.ecPerBlock; ++i) {
        for (const QVector<quint8> &block : std::as_const(ecBlocks)) {
            interleaved.append(block.at(i));
        }
    }

    // --- placement and masking ---------------------------------------------
    QrCanvas canvas(version);
    qrDrawFunctionPatterns(canvas, version);
    qrDrawCodewords(canvas, interleaved);

    int bestMask = 0;
    int bestScore = -1;
    for (int mask = 0; mask < 8; ++mask) {
        qrApplyMask(canvas, mask);
        qrDrawFormat(canvas, mask);
        const int score = qrPenalty(canvas);
        if (bestScore < 0 || score < bestScore) {
            bestScore = score;
            bestMask = mask;
        }
        qrApplyMask(canvas, mask); // XOR is its own inverse
    }
    qrApplyMask(canvas, bestMask);
    qrDrawFormat(canvas, bestMask);

    QrMatrix out;
    out.size = canvas.size;
    out.modules = canvas.dark;
    return out;
}

QImage renderQr(const QrMatrix &matrix, int targetPixels)
{
    if (!matrix.isValid()) {
        return {};
    }
    // Black on white, never palette colours: a scanner expects dark-on-light,
    // and under the app's dark theme a palette-derived code would be inverted
    // and unreadable by half the phones that try it. The 4-module quiet zone is
    // mandatory, not decoration.
    constexpr int quiet = 4;
    const int modules = matrix.size + 2 * quiet;
    QImage img(modules, modules, QImage::Format_RGB32);
    img.fill(Qt::white);
    for (int r = 0; r < matrix.size; ++r) {
        for (int c = 0; c < matrix.size; ++c) {
            if (matrix.dark(r, c)) {
                img.setPixel(c + quiet, r + quiet, qRgb(0, 0, 0));
            }
        }
    }
    const int scale = std::max(1, targetPixels / modules);
    return img.scaled(modules * scale, modules * scale, Qt::IgnoreAspectRatio,
                      Qt::FastTransformation);
}

} // namespace RemoteLogic

// ---------------------------------------------------------------------------
// PairingDialog
// ---------------------------------------------------------------------------

PairingDialog::PairingDialog(const QString &deviceName, const QString &pairingUrl,
                             const QString &certFingerprint, QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18n("Pair a device"));
    auto *v = new QVBoxLayout(this);

    auto *intro = new QLabel(i18n("Scan this with <b>%1</b>, or open the link on it.",
                                  deviceName.toHtmlEscaped()),
                             this);
    intro->setWordWrap(true);
    v->addWidget(intro);

    const RemoteLogic::QrMatrix matrix = RemoteLogic::encodeQr(pairingUrl.toUtf8());
    const QImage image = RemoteLogic::renderQr(matrix, 320);
    if (!image.isNull()) {
        auto *code = new QLabel(this);
        code->setPixmap(QPixmap::fromImage(image));
        code->setAlignment(Qt::AlignCenter);
        code->setFrameShape(QFrame::StyledPanel);
        code->setAccessibleName(i18n("Pairing QR code"));
        v->addWidget(code, 0, Qt::AlignCenter);
    } else {
        auto *noCode = new QLabel(
            i18n("This link is too long to show as a QR code — use the text below."), this);
        noCode->setWordWrap(true);
        v->addWidget(noCode);
    }

    // The URL, selectable and copyable. This field, and the QR above it, are the
    // only places the token is ever rendered.
    auto *urlRow = new QHBoxLayout;
    auto *urlEdit = new QLineEdit(pairingUrl, this);
    urlEdit->setReadOnly(true);
    urlEdit->setCursorPosition(0);
    urlEdit->setAccessibleName(i18n("Pairing link"));
    auto *copyBtn = new QPushButton(i18n("Copy link"), this);
    copyBtn->setIcon(QIcon::fromTheme(QStringLiteral("edit-copy")));
    connect(copyBtn, &QPushButton::clicked, this, [pairingUrl] {
        QGuiApplication::clipboard()->setText(pairingUrl);
    });
    urlRow->addWidget(urlEdit, 1);
    urlRow->addWidget(copyBtn);
    v->addLayout(urlRow);

    auto *once = new QLabel(
        i18n("<b>This link is shown once.</b> Agent Kate keeps only a hash of the token, "
             "so if you lose the link you must pair the device again."),
        this);
    once->setWordWrap(true);
    v->addWidget(once);

    auto *fragment = new QLabel(
        i18n("The token rides in the link's <b>#fragment</b>, which a browser never sends to "
             "a server — so it cannot reach an access log or a proxy. The phone swaps it for "
             "a cookie once and erases it from the address bar."),
        this);
    fragment->setWordWrap(true);
    v->addWidget(fragment);

    auto *cert = new QLabel(this);
    cert->setWordWrap(true);
    cert->setTextInteractionFlags(Qt::TextSelectableByMouse);
    if (certFingerprint.isEmpty()) {
        cert->setText(i18n("The certificate is self-signed, so the phone's browser will warn "
                           "you the first time. That is expected."));
    } else {
        cert->setText(i18n("The certificate is self-signed, so the phone's browser will warn "
                           "you the first time. On a network you do not trust, check that the "
                           "fingerprint it shows matches:\n%1",
                           certFingerprint));
    }
    tintHint(cert);
    v->addWidget(cert);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    v->addWidget(buttons);
}

// ---------------------------------------------------------------------------
// RemotePanel
// ---------------------------------------------------------------------------

RemotePanel::RemotePanel(CoreClient *core, QWidget *parent)
    : QWidget(parent), m_core(core)
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(8, 8, 8, 8);

    auto *title =
        new QLabel(i18n("<b>Remote access</b> — reach your agents from your phone"), this);
    title->setWordWrap(true);
    layout->addWidget(title);

    // The unmistakable indicator. A network listener on the user's own machine
    // must never be a thing they have to infer from a button's label, so the
    // state is said twice: once as a coloured headline, once as a sentence
    // explaining what it means.
    m_headline = new QLabel(this);
    m_headline->setWordWrap(true);
    m_headline->setTextFormat(Qt::PlainText);
    QFont headlineFont = m_headline->font();
    headlineFont.setBold(true);
    m_headline->setFont(headlineFont);
    layout->addWidget(m_headline);

    m_status = new KMessageWidget(this);
    m_status->setCloseButtonVisible(false);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    m_notice = new KMessageWidget(this);
    m_notice->setCloseButtonVisible(true);
    m_notice->setWordWrap(true);
    m_notice->hide();
    layout->addWidget(m_notice);

    // --- where to listen ---------------------------------------------------
    auto *bindBox = new QGroupBox(i18n("Where to listen"), this);
    auto *bindLayout = new QVBoxLayout(bindBox);

    auto *bindRow = new FlowLayout(0, 6, 6);
    m_iface = new QComboBox(bindBox);
    m_iface->setSizeAdjustPolicy(QComboBox::AdjustToMinimumContentsLengthWithIcon);
    m_iface->setMinimumContentsLength(24);
    m_port = new QSpinBox(bindBox);
    m_port->setRange(1024, 65535);
    m_port->setValue(8443);
    m_port->setToolTip(i18n("The port the phone connects to."));
    m_rescanBtn = new QToolButton(bindBox);
    m_rescanBtn->setIcon(QIcon::fromTheme(QStringLiteral("view-refresh")));
    m_rescanBtn->setToolTip(
        i18n("Look for network interfaces again (after connecting a VPN, say)."));
    connect(m_rescanBtn, &QToolButton::clicked, this, &RemotePanel::rebuildInterfaces);
    m_toggleBtn = new QPushButton(i18n("Turn remote access ON"), bindBox);
    connect(m_toggleBtn, &QPushButton::clicked, this, &RemotePanel::toggleEnabled);
    bindRow->addWidget(m_iface);
    bindRow->addWidget(m_port);
    bindRow->addWidget(m_rescanBtn);
    bindRow->addWidget(m_toggleBtn);
    bindLayout->addLayout(bindRow);

    m_ifaceHint = new QLabel(
        i18n("A Tailscale or WireGuard interface is listed first on purpose. “All network "
             "adapters” is available only as an explicit final choice and exposes the listener "
             "on every current network. The certificate is self-signed; on a network you do not "
             "control, use an encrypted overlay and verify its fingerprint."),
        bindBox);
    m_ifaceHint->setWordWrap(true);
    tintHint(m_ifaceHint);
    bindLayout->addWidget(m_ifaceHint);

    m_certLabel = new QLabel(bindBox);
    m_certLabel->setWordWrap(true);
    m_certLabel->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_certLabel->hide();
    tintHint(m_certLabel);
    bindLayout->addWidget(m_certLabel);
    layout->addWidget(bindBox);

    // --- paired devices ----------------------------------------------------
    auto *devicesBox = new QGroupBox(i18n("Paired devices"), this);
    auto *devicesOuter = new QVBoxLayout(devicesBox);
    auto *scroll = new QScrollArea(devicesBox);
    scroll->setWidgetResizable(true);
    scroll->setFrameShape(QFrame::NoFrame);
    auto *host = new QWidget(scroll);
    m_devicesLayout = new QVBoxLayout(host);
    m_devicesLayout->setContentsMargins(0, 0, 0, 0);
    m_devicesLayout->setSpacing(4);
    m_devicesEmpty = new QLabel(i18n("No device is paired."), host);
    m_devicesEmpty->setWordWrap(true);
    tintHint(m_devicesEmpty);
    m_devicesLayout->addWidget(m_devicesEmpty);
    m_devicesLayout->addStretch(1);
    scroll->setWidget(host);
    devicesOuter->addWidget(scroll);

    m_pairBtn = new QPushButton(i18n("Pair a phone…"), devicesBox);
    m_pairBtn->setIcon(QIcon::fromTheme(QStringLiteral("smartphone")));
    connect(m_pairBtn, &QPushButton::clicked, this, &RemotePanel::pairDevice);
    devicesOuter->addWidget(m_pairBtn);
    layout->addWidget(devicesBox, 1);

    // --- panic button ------------------------------------------------------
    m_killBtn = new QPushButton(this);
    m_killBtn->setIcon(QIcon::fromTheme(QStringLiteral("process-stop")));
    connect(m_killBtn, &QPushButton::clicked, this, &RemotePanel::toggleKill);
    layout->addWidget(m_killBtn);

    m_auditBtn = new QPushButton(i18n("Remote activity log…"), this);
    m_auditBtn->setIcon(QIcon::fromTheme(QStringLiteral("view-list-text")));
    connect(m_auditBtn, &QPushButton::clicked, this, &RemotePanel::showAuditLog);
    layout->addWidget(m_auditBtn);

    // --- what this actually means -----------------------------------------
    auto *honesty = new QLabel(
        i18n("A paired phone can approve tool calls, answer questions, approve plans, interrupt "
             "agents and read redacted transcripts. Sending prompts is enabled only once its "
             "cross-surface transcript echo is ready. A stolen unlocked phone is "
             "therefore equivalent to a stolen unlocked desktop session — if you lose one, "
             "revoke it here. It deliberately cannot create agents, name a file path, or "
             "answer Cowork desktop-control prompts. The full reasoning is in "
             "docs/security-model.md, section 7, “Remote access”."),
        this);
    honesty->setWordWrap(true);
    tintHint(honesty);
    layout->addWidget(honesty);

    // The core sends no remote.* notifications, so this panel asks. It asks only
    // while it is on screen.
    m_poll = new QTimer(this);
    m_poll->setInterval(kPollIntervalMs);
    connect(m_poll, &QTimer::timeout, this, &RemotePanel::refresh);

    applyState();

    if (m_core) {
        connect(m_core, &CoreClient::connected, this, &RemotePanel::refresh);
        if (m_core->isConnected()) {
            refresh();
        }
    }
}

void RemotePanel::showEvent(QShowEvent *event)
{
    QWidget::showEvent(event);
    // Adapter enumeration and the persisted-choice lookup can query host state.
    // Keep construction side-effect-free so restoring the main window never
    // blocks on either; the picker is populated before the panel is presented.
    if (!m_bindChoiceLoaded) {
        const KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Remote"));
        m_port->setValue(cfg.readEntry("Port", 8443));
        m_bindChoiceLoaded = true;
        rebuildInterfaces();
    }
    applyState();
    refresh();
    m_poll->start();
}

void RemotePanel::hideEvent(QHideEvent *event)
{
    QWidget::hideEvent(event);
    m_poll->stop();
}

void RemotePanel::refresh()
{
    if (!m_core || !m_core->isConnected()) {
        return;
    }
    QPointer<RemotePanel> self(this);
    m_core->call(QStringLiteral("remote.status"), {},
                 [this, self](const QJsonObject &res, const QJsonObject &err) {
                     if (!self) {
                         return;
                     }
                     if (!err.isEmpty()) {
                         applyStatusError(err);
                         return;
                     }
                     applyStatus(res);
                 },
                 this);
}

void RemotePanel::applyStatus(const QJsonObject &status)
{
    m_available = true;
    m_enabled = status.value(QStringLiteral("enabled")).toBool();
    m_addr = status.value(QStringLiteral("addr")).toString();
    m_fingerprint = status.value(QStringLiteral("certFingerprint")).toString();
    m_killed = status.value(QStringLiteral("killSwitch")).toBool();
    m_tampered = status.value(QStringLiteral("auditTampered")).toBool();
    m_state = RemoteLogic::stateFor(true, status);
    applyState();
    renderDevices(status.value(QStringLiteral("devices")).toArray());
}

void RemotePanel::applyStatusError(const QJsonObject &error)
{
    if (error.value(QStringLiteral("code")).toInt() == kMethodNotFound) {
        // Permanent for this core: an older akcore has no remote.* at all. Stop
        // polling and say why, rather than blinking a warning every eight
        // seconds at someone who cannot act on it.
        m_available = false;
        m_enabled = false;
        m_killed = false;
        m_tampered = false;
        m_addr.clear();
        m_fingerprint.clear();
        m_state = RemoteLogic::State::Unavailable;
        m_poll->stop();
        applyState();
        renderDevices({});
        return;
    }
    // Anything else is transient — a dropped socket, a core still starting. The
    // state stays as it was; the panel just says it could not refresh.
    const QString message = error.value(QStringLiteral("message")).toString();
    setNotice(message.isEmpty() ? i18n("Could not read the remote-access status.")
                                : i18n("Could not read the remote-access status: %1", message),
              int(KMessageWidget::Warning));
}

void RemotePanel::applyState()
{
    m_headline->setText(RemoteLogic::headline(m_state, m_addr));
    // KColorScheme consults the active desktop palette. It is meaningful only
    // once this panel is on screen; keeping the hidden construction path
    // palette-free also makes the state/QR test seam genuinely headless.
    const bool colourVisible = isVisible();

    switch (m_state) {
    case RemoteLogic::State::Unavailable:
        if (colourVisible) tintForeground(m_headline, KColorScheme::InactiveText);
        m_status->setMessageType(KMessageWidget::Error);
        m_status->setText(i18n("This build of Agent Kate's core has no remote access. "
                               "Rebuild or update it to reach your agents from a phone."));
        break;
    case RemoteLogic::State::Tampered:
        if (colourVisible) tintForeground(m_headline, KColorScheme::NegativeText);
        m_status->setMessageType(KMessageWidget::Error);
        m_status->setText(i18n("The remote audit log's hash chain does not verify — something "
                               "running as you has edited the record of what a phone did. This "
                               "is tamper detection, not prevention: it catches careless edits, "
                               "it cannot stop a determined local process."));
        break;
    case RemoteLogic::State::Killed:
        if (colourVisible) tintForeground(m_headline, KColorScheme::NegativeText);
        m_status->setMessageType(KMessageWidget::Error);
        m_status->setText(i18n("Kill switch engaged: the API answers 503 to every request, "
                               "including from paired devices. Nothing is unpaired — release "
                               "the switch to let your devices back in."));
        break;
    case RemoteLogic::State::On:
        if (colourVisible) tintForeground(m_headline, KColorScheme::NeutralText);
        m_status->setMessageType(KMessageWidget::Warning);
        m_status->setText(m_addr.isEmpty()
                              ? i18n("A TLS listener is running on this machine. Only paired "
                                     "devices are let in.")
                              : i18n("A TLS listener is running at https://%1. Only paired "
                                     "devices are let in.", m_addr));
        break;
    case RemoteLogic::State::Off:
        if (colourVisible) tintForeground(m_headline, KColorScheme::InactiveText);
        m_status->setMessageType(KMessageWidget::Information);
        m_status->setText(i18n("Nothing is listening on any network interface. Choose where to "
                               "listen and turn it on."));
        break;
    }

    m_toggleBtn->setText(m_enabled ? i18n("Turn remote access OFF") : i18n("Turn remote access ON"));
    m_toggleBtn->setIcon(QIcon::fromTheme(m_enabled ? QStringLiteral("network-disconnect")
                                                    : QStringLiteral("network-connect")));
    m_toggleBtn->setEnabled(m_available && (m_enabled || m_iface->count() > 0));

    // The interface cannot move under a running listener: the core stops and
    // rebinds, which would silently break every paired device's saved address.
    // Turning it off first makes that consequence a decision rather than a
    // surprise.
    const bool canChooseBind = m_available && !m_enabled;
    m_iface->setEnabled(canChooseBind);
    m_port->setEnabled(canChooseBind);
    m_rescanBtn->setEnabled(canChooseBind);
    m_iface->setToolTip(canChooseBind
                            ? i18n("Which network interface the listener binds. There is no "
                                   "wildcard: the core refuses one.")
                            : i18n("Turn remote access off to move it to another interface."));

    if (m_fingerprint.isEmpty()) {
        m_certLabel->hide();
    } else {
        m_certLabel->setText(i18n("Certificate SHA-256: %1", m_fingerprint));
        m_certLabel->show();
    }

    m_pairBtn->setEnabled(m_available && m_enabled && !m_killed);
    m_pairBtn->setToolTip(m_pairBtn->isEnabled()
                              ? i18n("Mint a one-time pairing link for a device.")
                              : i18n("Turn remote access on first — the pairing link has to "
                                     "name the address the phone will connect to."));

    m_killBtn->setText(m_killed ? i18n("Release the kill switch")
                                : i18n("KILL SWITCH — cut off every remote device"));
    m_killBtn->setIcon(QIcon::fromTheme(m_killed ? QStringLiteral("media-playback-start")
                                                 : QStringLiteral("process-stop")));
    m_killBtn->setToolTip(m_killed
                              ? i18n("Let paired devices back in. Nothing was unpaired.")
                              : i18n("Immediately refuse every remote request with 503 and drop "
                                     "live streams. Takes effect at once, without asking."));
    if (colourVisible) {
        tintForeground(m_killBtn,
                       m_killed ? KColorScheme::PositiveText : KColorScheme::NegativeText);
    }
    m_killBtn->setEnabled(m_available);
    m_auditBtn->setEnabled(m_available);
}

void RemotePanel::setNotice(const QString &text, int messageType)
{
    m_notice->setMessageType(static_cast<KMessageWidget::MessageType>(messageType));
    m_notice->setText(text);
    m_notice->show();
}

void RemotePanel::rebuildInterfaces()
{
    // Deliberately does NOT touch the port: this also runs from the rescan
    // button, and re-reading the saved port there would throw away a number the
    // user had just typed.
    const QString previous = m_iface->currentData().toString();
    const KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Remote"));
    const QString savedHost = cfg.readEntry("BindHost", QString());

    m_iface->clear();
    const QList<RemoteLogic::Interface> ifaces = RemoteLogic::localInterfaces();
    for (const RemoteLogic::Interface &iface : ifaces) {
        m_iface->addItem(RemoteLogic::interfaceLabel(iface), iface.address);
    }
    if (m_iface->count() == 0) {
        m_iface->addItem(i18n("(no usable network interface found)"), QString());
    } else {
        // This remains a separate, labelled choice. It is never an implicit
        // fallback when the adapter scan finds nothing.
        m_iface->insertSeparator(m_iface->count());
        m_iface->addItem(i18n("All network adapters (IPv4 — wider exposure)"),
                         QStringLiteral("0.0.0.0"));
        m_iface->addItem(i18n("All network adapters (IPv6 — wider exposure)"),
                         QStringLiteral("::"));
    }
    const QString wanted = previous.isEmpty() ? savedHost : previous;
    const int index = m_iface->findData(wanted);
    if (index >= 0) {
        m_iface->setCurrentIndex(index);
    }
}

void RemotePanel::rememberBindChoice()
{
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Remote"));
    cfg.writeEntry("BindHost", m_iface->currentData().toString());
    cfg.writeEntry("Port", m_port->value());
    cfg.sync();
}

void RemotePanel::toggleEnabled()
{
    if (!m_core) {
        return;
    }
    QPointer<RemotePanel> self(this);
    if (m_enabled) {
        m_core->call(QStringLiteral("remote.setEnabled"),
                     {{QStringLiteral("enabled"), false}},
                     [this, self](const QJsonObject &, const QJsonObject &err) {
                         if (!self) {
                             return;
                         }
                         if (!err.isEmpty()) {
                             setNotice(i18n("Could not turn remote access off: %1",
                                            err.value(QStringLiteral("message")).toString()),
                                       int(KMessageWidget::Error));
                             return;
                         }
                         setNotice(i18n("Remote access is off. Paired devices stay paired."),
                                   int(KMessageWidget::Information));
                         refresh();
                     },
                     this);
        return;
    }

    const QString host = m_iface->currentData().toString();
    const QString addr = RemoteLogic::bindAddr(host, m_port->value());
    if (addr.isEmpty()) {
        setNotice(i18n("Pick a network interface first — the core will not bind a wildcard "
                       "address, on purpose."),
                  int(KMessageWidget::Error));
        return;
    }
    const bool allInterfaces = host == QLatin1String("0.0.0.0") || host == QLatin1String("::");
    if (allInterfaces) {
        const auto answer = KMessageBox::warningTwoActions(
            this,
            i18n("This exposes Remote Access on every currently connected network. "
                 "Only continue when you understand every network this computer is on. "
                 "An encrypted overlay is safer away from your own LAN."),
            i18n("Listen on all network adapters"), KGuiItem(i18n("Listen on all adapters")),
            KStandardGuiItem::cancel());
        if (answer != KMessageBox::PrimaryAction) {
            return;
        }
    }
    rememberBindChoice();
    m_core->call(QStringLiteral("remote.setEnabled"),
                 {{QStringLiteral("enabled"), true}, {QStringLiteral("bindAddr"), addr},
                  {QStringLiteral("allowAllInterfaces"), allInterfaces}},
                 [this, self](const QJsonObject &res, const QJsonObject &err) {
                     if (!self) {
                         return;
                     }
                     if (!err.isEmpty()) {
                         setNotice(i18n("Could not start the listener: %1",
                                        err.value(QStringLiteral("message")).toString()),
                                   int(KMessageWidget::Error));
                         return;
                     }
                     setNotice(i18n("Listening at https://%1. Pair a phone to use it.",
                                    res.value(QStringLiteral("addr")).toString()),
                               int(KMessageWidget::Positive));
                     refresh();
                 },
                 this);
}

void RemotePanel::pairDevice()
{
    if (!m_core) {
        return;
    }
    bool ok = false;
    const QString name =
        QInputDialog::getText(this, i18n("Pair a device"),
                              i18n("What should this device be called? The name appears in the "
                                   "audit log next to everything it does."),
                              QLineEdit::Normal, i18n("My phone"), &ok);
    if (!ok || name.trimmed().isEmpty()) {
        return;
    }

    QPointer<RemotePanel> self(this);
    m_core->call(QStringLiteral("remote.pairDevice"), {{QStringLiteral("name"), name.trimmed()}},
                 [this, self](const QJsonObject &res, const QJsonObject &err) {
                     if (!self) {
                         return;
                     }
                     if (!err.isEmpty()) {
                         setNotice(i18n("Could not pair the device: %1",
                                        err.value(QStringLiteral("message")).toString()),
                                   int(KMessageWidget::Error));
                         return;
                     }
                     // The pairing URL — and the token inside its fragment —
                     // lives on this stack frame and inside the dialog it is
                     // handed to. It is never stored on the panel, never put in
                     // a message widget, and never logged.
                     const QString deviceName = res.value(QStringLiteral("device"))
                                                    .toObject()
                                                    .value(QStringLiteral("name"))
                                                    .toString();
                     PairingDialog dlg(deviceName,
                                       res.value(QStringLiteral("pairingUrl")).toString(),
                                       m_fingerprint, this);
                     dlg.exec();
                     setNotice(RemoteLogic::pairedConfirmation(res),
                               int(KMessageWidget::Positive));
                     refresh();
                 },
                 this);
}

void RemotePanel::renderDevices(const QJsonArray &devices)
{
    // Drop the existing rows, keeping the empty-state label and the trailing
    // stretch (they always sit last) — CoworkPanel's grant-list shape.
    while (m_devicesLayout->count() > 0) {
        QLayoutItem *item = m_devicesLayout->itemAt(0);
        if (!item || item->widget() == m_devicesEmpty || item->spacerItem()) {
            break;
        }
        m_devicesLayout->takeAt(0);
        delete item->widget();
        delete item;
    }

    int shown = 0;
    for (const QJsonValue &dv : devices) {
        const QJsonObject dev = dv.toObject();
        if (dev.value(QStringLiteral("revoked")).toBool()) {
            continue; // the store keeps revoked rows as history
        }
        const QString id = dev.value(QStringLiteral("id")).toString();
        const QString name = dev.value(QStringLiteral("name")).toString();
        const QDateTime paired = QDateTime::fromString(
            dev.value(QStringLiteral("pairedAt")).toString(), Qt::ISODateWithMs);

        auto *row = new QWidget;
        auto *rowLayout = new QHBoxLayout(row);
        rowLayout->setContentsMargins(0, 0, 0, 0);
        rowLayout->setSpacing(6);

        auto *sentence = new QLabel(row);
        sentence->setWordWrap(true);
        sentence->setTextFormat(Qt::RichText);
        sentence->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Preferred);
        const QString when = paired.isValid()
            ? QLocale().toString(paired.toLocalTime(), QLocale::ShortFormat)
            : dev.value(QStringLiteral("pairedAt")).toString();
        sentence->setText(i18n("<b>%1</b> — paired %2",
                               name.toHtmlEscaped(), when.toHtmlEscaped()));
        rowLayout->addWidget(sentence, 1);

        auto *revoke = new QToolButton(row);
        revoke->setText(i18n("Revoke"));
        revoke->setIcon(QIcon::fromTheme(QStringLiteral("edit-delete")));
        revoke->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
        revoke->setToolTip(i18n("Cut this device off now, including any stream it is holding."));
        // Keyed on the device id, never the row's position: the list is rebuilt
        // from every poll and an index would revoke whatever moved into the slot.
        connect(revoke, &QToolButton::clicked, this,
                [this, id, name] { revokeDevice(id, name); });
        rowLayout->addWidget(revoke, 0, Qt::AlignTop);

        m_devicesLayout->insertWidget(shown, row);
        ++shown;
    }
    m_devicesEmpty->setVisible(shown == 0);
    m_devicesEmpty->setText(m_available
                                ? i18n("No device is paired.")
                                : i18n("Paired devices cannot be listed by this core."));
}

void RemotePanel::revokeDevice(const QString &deviceId, const QString &name)
{
    if (!m_core || deviceId.isEmpty()) {
        return;
    }
    const auto answer = KMessageBox::warningContinueCancel(
        this,
        i18n("Cut off “%1”?\n\nIt loses access immediately: any live stream it is holding is "
             "dropped, not merely its next request. To use it again you must pair it afresh.",
             name),
        i18n("Revoke device"),
        KGuiItem(i18n("Revoke"), QStringLiteral("edit-delete")));
    if (answer != KMessageBox::Continue) {
        return;
    }
    QPointer<RemotePanel> self(this);
    m_core->call(QStringLiteral("remote.revokeDevice"),
                 {{QStringLiteral("deviceId"), deviceId},
                  {QStringLiteral("reason"), QStringLiteral("revoked from the Remote Access panel")}},
                 [this, self, name](const QJsonObject &, const QJsonObject &err) {
                     if (!self) {
                         return;
                     }
                     if (!err.isEmpty()) {
                         setNotice(i18n("Could not revoke “%1”: %2", name,
                                        err.value(QStringLiteral("message")).toString()),
                                   int(KMessageWidget::Error));
                         return;
                     }
                     setNotice(i18n("“%1” is revoked. Its live streams were dropped.", name),
                               int(KMessageWidget::Positive));
                     refresh();
                 },
                 this);
}

void RemotePanel::toggleKill()
{
    if (!m_core) {
        return;
    }
    // Deliberately no confirmation dialog, unlike CoworkPanel's kill-switch.
    // This one fails in the safe direction — an accidental press refuses remote
    // requests and unpairs nothing — and a panic button you must first read a
    // dialog to use is not a panic button.
    const bool turningOn = !m_killed;
    QPointer<RemotePanel> self(this);
    m_core->call(QStringLiteral("remote.killSwitch"), {{QStringLiteral("on"), turningOn}},
                 [this, self, turningOn](const QJsonObject &, const QJsonObject &err) {
                     if (!self) {
                         return;
                     }
                     if (!err.isEmpty()) {
                         setNotice(i18n("Could not change the kill switch: %1",
                                        err.value(QStringLiteral("message")).toString()),
                                   int(KMessageWidget::Error));
                         return;
                     }
                     setNotice(turningOn
                                   ? i18n("Kill switch engaged. Every remote request is answered "
                                          "503 until you release it.")
                                   : i18n("Kill switch released. Paired devices are let back in."),
                               turningOn ? int(KMessageWidget::Error)
                                         : int(KMessageWidget::Positive));
                     refresh();
                 },
                 this);
}

void RemotePanel::refreshAudit()
{
    if (!m_core) {
        return;
    }
    QPointer<RemotePanel> self(this);
    m_core->call(QStringLiteral("remote.auditTail"),
                 {{QStringLiteral("sinceSeq"), 0}, {QStringLiteral("limit"), 200}},
                 [this, self](const QJsonObject &res, const QJsonObject &err) {
                     if (!self || !err.isEmpty()) {
                         return;
                     }
                     m_auditEntries = res.value(QStringLiteral("entries")).toArray();
                     m_tampered = res.value(QStringLiteral("tampered")).toBool();
                     renderAudit();
                 },
                 this);
}

void RemotePanel::renderAudit()
{
    if (!m_audit) {
        return;
    }
    QStringList lines;
    lines.reserve(m_auditEntries.size());
    for (const QJsonValue &ev : std::as_const(m_auditEntries)) {
        lines << RemoteLogic::auditLine(ev.toObject());
    }
    m_audit->setPlainText(lines.isEmpty()
                              ? i18n("Nothing has been done remotely yet.")
                              : lines.join(QLatin1Char('\n')));
    if (m_auditWarning) {
        m_auditWarning->setVisible(m_tampered);
    }
}

void RemotePanel::showAuditLog()
{
    QDialog dlg(this);
    dlg.setWindowTitle(i18n("Remote activity log"));
    dlg.resize(640, 440);
    auto *v = new QVBoxLayout(&dlg);

    auto *intro = new QLabel(
        i18n("Every remote action that changes something — approvals, prompts, interrupts, "
             "stops, pairings and revocations — is appended to a hash-chained log. Reading a "
             "transcript is not recorded."),
        &dlg);
    intro->setWordWrap(true);
    v->addWidget(intro);

    m_auditWarning = new KMessageWidget(&dlg);
    m_auditWarning->setMessageType(KMessageWidget::Error);
    m_auditWarning->setCloseButtonVisible(false);
    m_auditWarning->setWordWrap(true);
    m_auditWarning->setText(i18n("The chain does not verify — this file has been edited by "
                                 "something running as you. That is what the chain is for: it "
                                 "detects tampering, it cannot prevent it."));
    m_auditWarning->setVisible(m_tampered);
    v->addWidget(m_auditWarning);

    m_audit = new QPlainTextEdit(&dlg);
    m_audit->setReadOnly(true);
    m_audit->setMaximumBlockCount(2000);
    v->addWidget(m_audit, 1);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, &dlg);
    connect(buttons, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    connect(buttons, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    v->addWidget(buttons);

    refreshAudit();
    renderAudit();
    dlg.exec();

    // The view lives only for the dialog's lifetime.
    m_audit = nullptr;
    m_auditWarning = nullptr;
}
