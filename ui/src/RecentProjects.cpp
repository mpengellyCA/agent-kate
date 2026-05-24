#include "RecentProjects.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QDir>

namespace {
constexpr char kGroup[] = "Projects";
constexpr char kKey[] = "recent";

QString canonical(const QString &path)
{
    const QString abs = QDir(path).absolutePath();
    return abs;
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
    while (list.size() > kMaxEntries) {
        list.removeLast();
    }
    save(list);
}

void forget(const QString &path)
{
    const QString c = canonical(path);
    QStringList list = load();
    if (list.removeAll(c) > 0) {
        save(list);
    }
}

QString last()
{
    const QStringList list = load();
    return list.isEmpty() ? QString() : list.constFirst();
}

} // namespace RecentProjects
