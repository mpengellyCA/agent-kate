// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "SessionProjects.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QFileInfo>

namespace {
const char *kGroup = "Session";
const char *kKey = "openProjects";
} // namespace

namespace SessionProjects {

QStringList load()
{
    const QStringList stored = KSharedConfig::openConfig()
                                   ->group(QString::fromLatin1(kGroup))
                                   .readEntry(kKey, QStringList());
    QStringList out;
    for (const QString &path : stored) {
        if (path.isEmpty() || out.contains(path)) {
            continue;
        }
        const QFileInfo info(path);
        if (info.exists() && info.isDir()) {
            out << path;
        }
    }
    return out;
}

void save(const QStringList &paths)
{
    KConfigGroup g = KSharedConfig::openConfig()->group(QString::fromLatin1(kGroup));
    g.writeEntry(kKey, paths);
    g.sync();
}

} // namespace SessionProjects
