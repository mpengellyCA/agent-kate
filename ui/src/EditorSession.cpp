// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "EditorSession.h"

#include <KConfigGroup>

#include <QDir>
#include <QFileInfo>

namespace {
constexpr char kAgentPrefix[] = "agent:";
constexpr char kPendingPrefix[] = "pending:";
constexpr char kVersionKey[] = "version";
constexpr char kProjectKeyName[] = "project";
constexpr char kFilesKey[] = "openFiles";
constexpr char kActiveKey[] = "active";

// KConfig group names live between literal brackets in the ini file, so a '['
// or ']' in a path would corrupt the group header, and stray whitespace or
// control characters round-trip unpredictably. Percent-encode everything
// outside a conservative safe set. '/' stays literal (project-path groups have
// always been written that way and read back fine), while ':' is encoded so the
// "agent:<project>:<threadId>" split is unambiguous even for a path containing
// a colon.
QString sanitize(const QString &s)
{
    QString out;
    out.reserve(s.size());
    for (const QChar c : s) {
        const ushort u = c.unicode();
        const bool safe = (u >= 'a' && u <= 'z') || (u >= 'A' && u <= 'Z')
            || (u >= '0' && u <= '9') || c == QLatin1Char('/') || c == QLatin1Char('.')
            || c == QLatin1Char('_') || c == QLatin1Char('-');
        if (safe) {
            out.append(c);
        } else {
            out.append(QLatin1Char('%'));
            out.append(QString::number(u, 16).rightJustified(u > 0xff ? 4 : 2,
                                                             QLatin1Char('0'))
                           .toUpper());
        }
    }
    return out;
}

// Inverse of sanitize(), so projectForKey() hands back a real filesystem path
// (a project directory with a space in it must not come back as "%20").
QString unsanitize(const QString &s)
{
    QString out;
    out.reserve(s.size());
    for (int i = 0; i < s.size(); ++i) {
        if (s.at(i) != QLatin1Char('%')) {
            out.append(s.at(i));
            continue;
        }
        // sanitize() writes 2 hex digits below U+0100 and 4 above it.
        bool decoded = false;
        for (const int width : {2, 4}) {
            if (decoded || i + width >= s.size()) {
                continue;
            }
            bool ok = false;
            const ushort u = s.mid(i + 1, width).toUShort(&ok, 16);
            if (ok && (width == 2 ? u <= 0xff : u > 0xff)) {
                out.append(QChar(u));
                i += width;
                decoded = true;
            }
        }
        if (!decoded) {
            out.append(s.at(i)); // not an escape we wrote — keep it verbatim
        }
    }
    return out;
}
} // namespace

namespace EditorSession {

QString normalizedProject(const QString &projectPath)
{
    if (projectPath.isEmpty()) {
        return {};
    }
    // absolutePath() cleans "." / ".." segments and strips the trailing slash
    // (except for root), so "/p/" and "/p" land on the same key.
    return QDir(projectPath).absolutePath();
}

QString projectKey(const QString &projectPath)
{
    const QString project = normalizedProject(projectPath);
    return project.isEmpty() ? QString() : sanitize(project);
}

QString agentKey(const QString &projectPath, const QString &threadId)
{
    const QString project = normalizedProject(projectPath);
    if (project.isEmpty() || threadId.isEmpty()) {
        return {};
    }
    return QLatin1String(kAgentPrefix) + sanitize(project) + QLatin1Char(':')
        + sanitize(threadId);
}

QString pendingKey(const QString &projectPath, int agentId)
{
    const QString project = normalizedProject(projectPath);
    if (project.isEmpty() || agentId < 0) {
        return {};
    }
    return QLatin1String(kPendingPrefix) + sanitize(project) + QLatin1Char(':')
        + QString::number(agentId);
}

bool isPersistable(const QString &key)
{
    return !key.isEmpty() && !key.startsWith(QLatin1String(kPendingPrefix));
}

QString projectForKey(const QString &key)
{
    if (key.isEmpty()) {
        return {};
    }
    if (key.startsWith(QLatin1String(kAgentPrefix))
        || key.startsWith(QLatin1String(kPendingPrefix))) {
        const int prefixLen = key.startsWith(QLatin1String(kAgentPrefix))
            ? int(sizeof(kAgentPrefix) - 1)
            : int(sizeof(kPendingPrefix) - 1);
        // The suffix (thread id / agent number) never contains ':' — sanitize()
        // encodes any colon inside the project part — so the last ':' splits.
        const int cut = key.lastIndexOf(QLatin1Char(':'));
        if (cut <= prefixLen) {
            return {};
        }
        return unsanitize(key.mid(prefixLen, cut - prefixLen));
    }
    return unsanitize(key); // project-mode key is the project path itself
}

bool isContained(const QString &path, const QStringList &roots)
{
    if (path.isEmpty()) {
        return false;
    }
    const QString file = QDir::cleanPath(path);
    for (const QString &rawRoot : roots) {
        const QString root = normalizedProject(rawRoot);
        if (root.isEmpty()) {
            continue;
        }
        if (file == root) {
            return true;
        }
        const QString prefix =
            root.endsWith(QLatin1Char('/')) ? root : root + QLatin1Char('/');
        if (file.startsWith(prefix)) {
            return true;
        }
    }
    return false;
}

Session read(const KConfigGroup &sessions, const QString &key, const QStringList &roots)
{
    Session out;
    if (key.isEmpty() || !sessions.hasGroup(key)) {
        return out;
    }
    const KConfigGroup grp = sessions.group(key);
    // Legacy "agent-N" groups (and anything else unversioned) are ignored
    // wholesale: their ids map to no stable identity, so their contents are
    // just as likely to belong to another project.
    if (grp.readEntry(QLatin1String(kVersionKey), 0) != kSchemaVersion) {
        return out;
    }
    const QString active = grp.readEntry(QLatin1String(kActiveKey), QString());
    const auto replayable = [&roots](const QString &path) {
        return !path.isEmpty() && isContained(path, roots) && QFileInfo::exists(path);
    };
    const QStringList files = grp.readEntry(QLatin1String(kFilesKey), QStringList());
    for (const QString &path : files) {
        if (out.files.size() >= kMaxRestore) {
            break;
        }
        if (replayable(path) && !out.files.contains(path)) {
            out.files.append(path);
        }
    }
    // Tab order is the human's, so the cap takes from the tail — but the file
    // they were last looking at always comes back, even from beyond the cap.
    if (replayable(active)) {
        out.active = active;
        if (!out.files.contains(active)) {
            if (out.files.size() >= kMaxRestore) {
                out.files.removeLast();
            }
            out.files.append(active);
        }
    }
    return out;
}

void write(KConfigGroup &sessions, const QString &key, const QStringList &files,
           const QString &active)
{
    if (!isPersistable(key)) {
        return;
    }
    KConfigGroup grp = sessions.group(key);
    grp.writeEntry(QLatin1String(kVersionKey), kSchemaVersion);
    grp.writeEntry(QLatin1String(kProjectKeyName), projectForKey(key));
    grp.writeEntry(QLatin1String(kFilesKey), files);
    grp.writeEntry(QLatin1String(kActiveKey), active);
}

void sweep(KConfigGroup &sessions)
{
    const QStringList keys = sessions.groupList();
    for (const QString &key : keys) {
        KConfigGroup grp = sessions.group(key);
        if (grp.readEntry(QLatin1String(kVersionKey), 0) != kSchemaVersion) {
            grp.deleteGroup(); // legacy agent-N and other unreadable leftovers
            continue;
        }
        // A group with nothing to replay is pure clutter, and one whose project
        // directory is gone has nothing to replay into. (A project sitting on a
        // temporarily unmounted volume loses its remembered tabs — the trade for
        // a config file that stays bounded instead of growing forever.)
        const bool empty = grp.readEntry(QLatin1String(kFilesKey), QStringList()).isEmpty();
        const QString project = grp.readEntry(QLatin1String(kProjectKeyName), QString());
        if (empty || (!project.isEmpty() && !QFileInfo(project).isDir())) {
            grp.deleteGroup();
        }
    }
}

} // namespace EditorSession
