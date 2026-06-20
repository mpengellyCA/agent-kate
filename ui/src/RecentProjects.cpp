#include "RecentProjects.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QCryptographicHash>
#include <QDateTime>
#include <QDir>

namespace {
constexpr char kGroup[] = "Projects";
constexpr char kKey[] = "recent";
constexpr char kPinnedKey[] = "pinned";
constexpr char kLastOpenedKey[] = "lastOpened"; // per-project sub-key prefix

QString canonical(const QString &path)
{
    const QString abs = QDir(path).absolutePath();
    return abs;
}

// A stable, config-safe key for a project's per-entry metadata (timestamps).
// Paths contain characters that are awkward as config keys, so hash them.
QString metaKey(const QString &canonicalPath)
{
    const QByteArray h = QCryptographicHash::hash(
        canonicalPath.toUtf8(), QCryptographicHash::Sha1);
    return QString::fromLatin1(kLastOpenedKey) + QLatin1Char('_')
        + QString::fromLatin1(h.toHex());
}
} // namespace

namespace RecentProjects {

QStringList load()
{
    const KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    return cfg.readEntry(QLatin1String(kKey), QStringList{});
}

static void save(const QStringList &list)
{
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    cfg.writeEntry(QLatin1String(kKey), list);
    cfg.sync();
}

void remember(const QString &path)
{
    if (path.isEmpty()) {
        return;
    }
    const QString c = canonical(path);
    QStringList list = load();
    list.removeAll(c);
    list.prepend(c);
    // Pinned entries are exempt from the cap so favourites never fall off.
    const QStringList pins = pinned();
    while (list.size() > kMaxEntries) {
        // Drop the oldest non-pinned entry.
        bool removed = false;
        for (int i = list.size() - 1; i >= 0; --i) {
            if (!pins.contains(list.at(i))) {
                list.removeAt(i);
                removed = true;
                break;
            }
        }
        if (!removed) {
            break; // everything left is pinned
        }
    }
    save(list);

    // Stamp the open time.
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    cfg.writeEntry(metaKey(c), QDateTime::currentDateTimeUtc().toString(Qt::ISODate));
    cfg.sync();
}

void forget(const QString &path)
{
    const QString c = canonical(path);
    QStringList list = load();
    bool changed = (list.removeAll(c) > 0);
    if (changed) {
        save(list);
    }
    unpin(c);
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    if (cfg.hasKey(metaKey(c))) {
        cfg.deleteEntry(metaKey(c));
        cfg.sync();
    }
}

QString last()
{
    const QStringList list = load();
    return list.isEmpty() ? QString() : list.constFirst();
}

QStringList pinned()
{
    const KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    return cfg.readEntry(QLatin1String(kPinnedKey), QStringList{});
}

bool isPinned(const QString &path)
{
    return pinned().contains(canonical(path));
}

void pin(const QString &path)
{
    const QString c = canonical(path);
    QStringList pins = pinned();
    if (pins.contains(c)) {
        return;
    }
    pins.prepend(c);
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    cfg.writeEntry(QLatin1String(kPinnedKey), pins);
    cfg.sync();
}

void unpin(const QString &path)
{
    const QString c = canonical(path);
    QStringList pins = pinned();
    if (pins.removeAll(c) > 0) {
        KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
        cfg.writeEntry(QLatin1String(kPinnedKey), pins);
        cfg.sync();
    }
}

QDateTime lastOpened(const QString &path)
{
    const QString c = canonical(path);
    const KConfigGroup cfg = KSharedConfig::openConfig()->group(QLatin1String(kGroup));
    const QString iso = cfg.readEntry(metaKey(c), QString());
    if (iso.isEmpty()) {
        return QDateTime();
    }
    return QDateTime::fromString(iso, Qt::ISODate);
}

} // namespace RecentProjects
