#pragma once

#include <QByteArray>
#include <QCoreApplication>
#include <QDir>
#include <QFile>
#include <QList>
#include <QString>
#include <QStringList>

#include <cerrno>
#include <csignal>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

// Where the UI puts the Unix socket it hands to akcore.
//
// The core REFUSES to bind a socket whose directory is not a real, user-owned,
// 0700-or-tighter directory (ipc.assertPrivateDir, audit F20a) — so a UI that
// picks a directory the core will reject does not merely weaken privacy, it
// makes the app fail to start. This header is the UI's half of that contract:
// it applies the same four rules the core applies, and it fails CLOSED, so the
// only paths it ever hands out are ones the core will accept.
//
// Header-only on purpose: CoreClient.cpp is compiled into four different test
// binaries, and a new .cpp would have to be added to every one of them.
namespace akipc {

// sun_path is 108 bytes INCLUDING the terminating NUL, so 107 usable bytes.
// This is a hard kernel limit, not a style rule: bind() on a longer path fails
// with ENAMETOOLONG, and QLocalSocket reports it as a generic connect failure
// long after the core has already died. $TMPDIR is not always short (test
// harnesses and sandboxes routinely nest it several directories deep), which is
// exactly when the fallback path is taken — so the limit is checked, and a
// candidate that would cross it is skipped in favour of a shorter one.
constexpr int kMaxUnixSocketPath = 107;

// isPrivateDir mirrors core/internal/ipc/server.go assertPrivateDir exactly:
// a real directory (lstat, so a symlink is NOT one), owned by us, with no
// group or other bits. Every condition it cannot evaluate answers "not
// private" — "we could not check" and "it is safe" must never be the same
// answer.
inline bool isPrivateDir(const QString &path)
{
    if (path.isEmpty()) {
        return false;
    }
    const QByteArray enc = QFile::encodeName(path);
    struct stat st {};
    if (::lstat(enc.constData(), &st) != 0) {
        return false;
    }
    // lstat does not follow symlinks, so a symlinked directory is S_ISLNK here
    // and fails this test — the core refuses to bind through one too.
    if (!S_ISDIR(st.st_mode)) {
        return false;
    }
    if (st.st_uid != ::getuid()) {
        return false;
    }
    if (st.st_mode & (S_IRWXG | S_IRWXO)) {
        return false;
    }
    return true;
}

// ensurePrivateDir creates dir 0700 if absent, then verifies it. An existing
// directory is never "fixed" (no chmod, no chown): a directory we did not make
// private ourselves may already have been observed by someone else, so the only
// safe answer is to refuse it.
inline bool ensurePrivateDir(const QString &dir)
{
    const QByteArray enc = QFile::encodeName(dir);
    if (::mkdir(enc.constData(), 0700) != 0 && errno != EEXIST) {
        return false;
    }
    return isPrivateDir(dir);
}

// sweepDeadSockets unlinks `agentkate-<pid>.sock` entries in dir whose pid is no
// longer running.
//
// The socket name carries our pid so two instances cannot collide on one path.
// The cost of that is litter: akcore unlinks its socket on a graceful exit, but
// a crash or a SIGKILL leaves the entry behind under a name no later run will
// ever reuse. In $XDG_RUNTIME_DIR that is invisible (tmpfs, cleared at logout);
// in the $TMPDIR/agentkate-<uid> fallback, which survives logout and on many
// systems reboot, it accumulates one dead entry per hard stop, forever.
//
// Conservative on every axis, because this function DELETES:
//   - only names matching our own exact pattern, with a fully numeric pid;
//   - only entries that are really sockets (lstat + S_ISSOCK — never a symlink,
//     never a regular file, never a directory);
//   - only pids that are gone. kill(pid, 0) failing with EPERM means the pid IS
//     alive and owned by someone else, which is not ours to clean up.
//
// Best-effort throughout: an unlink that fails is ignored. This is tidying, and
// tidying must never be able to stop the app from starting.
inline void sweepDeadSockets(const QString &dir)
{
    const QStringList entries =
        QDir(dir).entryList({QStringLiteral("agentkate-*.sock")}, QDir::System | QDir::NoDotAndDotDot);
    for (const QString &name : entries) {
        // "agentkate-" ... ".sock" -> the pid in between.
        const QString pidText = name.mid(10, name.size() - 10 - 5);
        bool numeric = false;
        const qint64 pid = pidText.toLongLong(&numeric);
        if (!numeric || pid <= 0) {
            continue;
        }
        if (pid == QCoreApplication::applicationPid()) {
            continue; // ours (a reused pid after a crash) — the bind will replace it
        }
        if (::kill(static_cast<pid_t>(pid), 0) == 0 || errno == EPERM) {
            continue; // still running
        }
        const QString path = QDir(dir).filePath(name);
        const QByteArray enc = QFile::encodeName(path);
        struct stat st {};
        if (::lstat(enc.constData(), &st) != 0 || !S_ISSOCK(st.st_mode)
            || st.st_uid != ::getuid()) {
            continue;
        }
        ::unlink(enc.constData());
    }
}

// privateSocketPath returns the socket path for this UI process, or an empty
// string with *error set when no private directory can be had.
//
// Candidates, in order:
//   1. $XDG_RUNTIME_DIR — per-user and 0700 by construction on every systemd
//      session; the core's own default lives here too.
//   2. $TMPDIR/agentkate-<uid> — created 0700 by us. Used when there is no
//      runtime dir (a bare `ssh` session, a stripped service environment).
//   3. /tmp/agentkate-<uid> — the same thing at a path whose length we control,
//      for the case where $TMPDIR is itself so deeply nested that the socket
//      would not fit in sun_path.
//
// A candidate is taken only if it is private AND the resulting path fits.
inline QString privateSocketPath(QString *error = nullptr)
{
    const QString leaf =
        QStringLiteral("agentkate-%1.sock").arg(QCoreApplication::applicationPid());
    const QString userDir = QStringLiteral("agentkate-%1").arg(::getuid());

    const QString runtime = qEnvironmentVariable("XDG_RUNTIME_DIR");
    const QString tmpDir = QDir(QDir::tempPath()).filePath(userDir);
    const QString slashTmp = QDir(QStringLiteral("/tmp")).filePath(userDir);

    struct Candidate {
        QString dir;
        bool create;
    };
    const QList<Candidate> candidates{
        {runtime, false}, {tmpDir, true}, {slashTmp, true},
    };

    bool sawPrivate = false;
    QStringList tried;
    for (const Candidate &c : candidates) {
        if (c.dir.isEmpty() || tried.contains(c.dir)) {
            continue;
        }
        tried.append(c.dir);
        const bool ok = c.create ? ensurePrivateDir(c.dir) : isPrivateDir(c.dir);
        if (!ok) {
            continue;
        }
        sawPrivate = true;
        const QString path = QDir(c.dir).filePath(leaf);
        if (QFile::encodeName(path).size() > kMaxUnixSocketPath) {
            continue; // private, but the kernel would refuse to bind it
        }
        // Only once we have committed to this directory: clear out sockets left
        // by runs that were killed before akcore could unlink its own.
        sweepDeadSockets(c.dir);
        return path;
    }

    if (error) {
        *error = sawPrivate
            ? QStringLiteral("no private socket directory short enough for the "
                             "%1-byte Unix socket path limit (tried %2)")
                  .arg(kMaxUnixSocketPath)
                  .arg(tried.join(QStringLiteral(", ")))
            : QStringLiteral("no private (0700, user-owned) directory available "
                             "for the core socket (tried %1)")
                  .arg(tried.join(QStringLiteral(", ")));
    }
    return {};
}

} // namespace akipc
