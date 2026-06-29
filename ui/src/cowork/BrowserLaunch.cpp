#include "BrowserLaunch.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QDBusConnection>
#include <QDBusInterface>
#include <QDBusVariant>
#include <QFileInfo>
#include <QProcess>
#include <QStandardPaths>

namespace BrowserLaunch {

namespace {

constexpr QLatin1String kFirefox("firefox");
constexpr QLatin1String kChromium("chromium");

// Known browsers we probe for, in display order. Several binaries can map to the
// same product (chromium / chromium-browser); the first one found wins.
struct Candidate {
    const char *binary;
    const char *name;
    const char *family;
};
const Candidate kCandidates[] = {
    {"zen", "Zen", "firefox"},
    {"zen-browser", "Zen", "firefox"},
    {"firefox", "Firefox", "firefox"},
    {"firefox-esr", "Firefox ESR", "firefox"},
    {"librewolf", "LibreWolf", "firefox"},
    {"floorp", "Floorp", "firefox"},
    {"waterfox", "Waterfox", "firefox"},
    {"helium", "Helium", "chromium"},
    {"chromium", "Chromium", "chromium"},
    {"chromium-browser", "Chromium", "chromium"},
    {"google-chrome-stable", "Google Chrome", "chromium"},
    {"google-chrome", "Google Chrome", "chromium"},
    {"brave", "Brave", "chromium"},
    {"brave-browser", "Brave", "chromium"},
    {"vivaldi-stable", "Vivaldi", "chromium"},
    {"vivaldi", "Vivaldi", "chromium"},
    {"microsoft-edge-stable", "Microsoft Edge", "chromium"},
    {"microsoft-edge", "Microsoft Edge", "chromium"},
};

KConfigGroup configGroup()
{
    return KSharedConfig::openConfig()->group(QStringLiteral("Cowork"));
}

// Chromium checks org.a11y.Status at launch to decide whether to export an AT-SPI
// tree; the --force-renderer-accessibility flag alone is NOT sufficient (some
// forks, e.g. Helium, gate the AX tree on this). Enable it on the session bus
// before launching a Chromium browser so agents can actually read the page. We
// leave it enabled afterwards (harmless); the agent control path manages its own
// capture/restore of this status separately.
void enableAtspiStatus()
{
    QDBusInterface props(QStringLiteral("org.a11y.Bus"), QStringLiteral("/org/a11y/bus"),
                         QStringLiteral("org.freedesktop.DBus.Properties"),
                         QDBusConnection::sessionBus());
    if (!props.isValid()) {
        return;
    }
    props.call(QStringLiteral("Set"), QStringLiteral("org.a11y.Status"),
               QStringLiteral("IsEnabled"), QVariant::fromValue(QDBusVariant(true)));
    props.call(QStringLiteral("Set"), QStringLiteral("org.a11y.Status"),
               QStringLiteral("ScreenReaderEnabled"), QVariant::fromValue(QDBusVariant(true)));
}

// Custom browsers are stored as "name\x1fcommand\x1ffamily" strings.
constexpr QChar kSep(u'\x1f');

} // namespace

QList<Browser> detected()
{
    QList<Browser> out;
    QStringList seenNames;
    for (const Candidate &c : kCandidates) {
        const QString name = QString::fromLatin1(c.name);
        if (seenNames.contains(name)) {
            continue;
        }
        const QString path = QStandardPaths::findExecutable(QString::fromLatin1(c.binary));
        if (path.isEmpty()) {
            continue;
        }
        seenNames << name;
        out.append({name, QString::fromLatin1(c.binary), QString::fromLatin1(c.family)});
    }
    return out;
}

QList<Browser> custom()
{
    QList<Browser> out;
    const QStringList raw = configGroup().readEntry("customBrowsers", QStringList());
    for (const QString &line : raw) {
        const QStringList parts = line.split(kSep);
        if (parts.size() == 3 && !parts[0].isEmpty() && !parts[1].isEmpty()) {
            const QString family = parts[2] == kChromium ? QString(kChromium) : QString(kFirefox);
            out.append({parts[0], parts[1], family});
        }
    }
    return out;
}

void addCustom(const Browser &b)
{
    if (b.command.isEmpty()) {
        return;
    }
    QList<Browser> existing = custom();
    for (const Browser &e : existing) {
        if (e.command == b.command && e.family == b.family) {
            return; // already saved
        }
    }
    QStringList raw = configGroup().readEntry("customBrowsers", QStringList());
    raw << (b.name + kSep + b.command + kSep + b.family);
    configGroup().writeEntry("customBrowsers", raw);
    configGroup().sync();
}

QList<Browser> all()
{
    QList<Browser> out = detected();
    QStringList seenCmds;
    for (const Browser &b : out) {
        seenCmds << b.command;
    }
    for (const Browser &b : custom()) {
        if (!seenCmds.contains(b.command)) {
            seenCmds << b.command;
            out.append(b);
        }
    }
    return out;
}

QStringList names()
{
    QStringList out;
    for (const Browser &b : all()) {
        out << b.name;
    }
    return out;
}

Browser find(const QString &nameOrCommand)
{
    const QString needle = nameOrCommand.trimmed();
    for (const Browser &b : all()) {
        if (b.name.compare(needle, Qt::CaseInsensitive) == 0
            || b.command.compare(needle, Qt::CaseInsensitive) == 0) {
            return b;
        }
    }
    return {};
}

Browser preferred()
{
    const QList<Browser> list = all();
    const QString pref = configGroup().readEntry("agentBrowser", QString());
    if (!pref.isEmpty()) {
        for (const Browser &b : list) {
            if (b.command == pref) {
                return b;
            }
        }
    }
    return list.isEmpty() ? Browser{} : list.first();
}

void setPreferred(const QString &command)
{
    configGroup().writeEntry("agentBrowser", command);
    configGroup().sync();
}

QString guessFamily(const QString &commandOrPath)
{
    const QString base = QFileInfo(commandOrPath).fileName().toLower();
    static const char *firefoxHints[] = {"firefox", "zen", "librewolf", "floorp", "waterfox", "mullvad"};
    for (const char *h : firefoxHints) {
        if (base.contains(QLatin1String(h))) {
            return QString(kFirefox);
        }
    }
    // Most other modern browsers (Helium, Chrome, Brave, Vivaldi, Edge, Opera, Thorium…)
    // are Chromium-based, so default unknowns there — that's where the a11y flag lives.
    return QString(kChromium);
}

bool launch(const Browser &b, QString *error)
{
    if (b.command.isEmpty()) {
        if (error) {
            *error = QStringLiteral("no browser command configured");
        }
        return false;
    }
    // Resolve the command so a missing browser fails here with a clear message rather
    // than silently in the detached child.
    QString program = b.command;
    if (!QFileInfo(program).isAbsolute()) {
        const QString resolved = QStandardPaths::findExecutable(program);
        if (resolved.isEmpty()) {
            if (error) {
                *error = QStringLiteral("could not find “%1” on your PATH").arg(b.command);
            }
            return false;
        }
        program = resolved;
    } else if (!QFileInfo(program).isExecutable()) {
        if (error) {
            *error = QStringLiteral("“%1” is not an executable").arg(program);
        }
        return false;
    }

    // We launch through `env` so the Firefox-family environment variable is applied to
    // the child regardless of QProcess detached-environment support across Qt versions.
    QStringList args;
    if (b.family == kFirefox) {
        args << QStringLiteral("GNOME_ACCESSIBILITY=1");
    }
    args << program;
    if (b.family == kChromium) {
        // org.a11y.Status must be on at launch, and =complete forces the full AX
        // tree explicitly. Some Chromium builds/forks (e.g. Helium) gate the bare
        // flag behind activation heuristics, so set both.
        enableAtspiStatus();
        args << QStringLiteral("--force-renderer-accessibility=complete");
    }

    QProcess proc;
    proc.setProgram(QStringLiteral("env"));
    proc.setArguments(args);
    qint64 pid = 0;
    if (!proc.startDetached(&pid)) {
        if (error) {
            *error = QStringLiteral("could not launch %1").arg(b.name);
        }
        return false;
    }
    return true;
}

} // namespace BrowserLaunch
