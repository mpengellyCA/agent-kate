// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "SafeContent.h"

#include <QDesktopServices>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QStandardPaths>
#include <QUrl>

#include <KLocalizedString>
#include <KMessageBox>

namespace agentkate
{
namespace
{
// The attachment store, recomputed here rather than pulled from
// AttachmentBuilder so this module has no dependency on the send path.
// LOCKSTEP with AttachmentBuilder.cpp attachmentStoreDir() /
// legacyAttachmentStoreDir(): if those move, these must move with them, or
// thumbnails in replayed transcripts silently stop rendering.
QStringList attachmentRoots()
{
    QStringList out;
    const QString data = QStandardPaths::writableLocation(QStandardPaths::AppDataLocation);
    if (!data.isEmpty()) {
        out << data + QStringLiteral("/attachments");
    }
    const QString cache = QStandardPaths::writableLocation(QStandardPaths::CacheLocation);
    if (!cache.isEmpty()) {
        out << cache + QStringLiteral("/attachments");
    }
    return out;
}

// Canonicalise a directory for prefix matching. Empty means "unusable" — the
// caller must then refuse, never fall back to matching the raw string (a
// non-canonical root would let ../ escape it).
QString canonicalDir(const QString &dir)
{
    if (dir.isEmpty()) {
        return {};
    }
    const QString c = QFileInfo(dir).canonicalFilePath();
    if (c.isEmpty()) {
        return {};
    }
    return c.endsWith(QLatin1Char('/')) ? c : c + QLatin1Char('/');
}

QStringList &globalRoots()
{
    static QStringList roots;
    static bool seeded = false;
    if (!seeded) {
        seeded = true;
        const QStringList atts = attachmentRoots();
        for (const QString &d : atts) {
            // The store may not exist yet on a fresh install; it is created on
            // the first attachment, and allowMediaRoot is idempotent, so the
            // seed is retried lazily below.
            const QString c = canonicalDir(d);
            if (!c.isEmpty()) {
                roots << c;
            }
        }
    }
    return roots;
}

// True when `file` (already canonical) sits under one of `roots` (already
// canonical, trailing slash). A root that failed to canonicalise never gets
// here, so an unresolvable root can never match everything.
bool underRoot(const QString &file, const QStringList &roots)
{
    for (const QString &r : roots) {
        if (file.startsWith(r)) {
            return true;
        }
    }
    return false;
}

// The one value every refusal funnels through. See blockedImageBytes' comment
// in the header: an image refusal has to be a decodable image or
// QTextImageHandler reads the path itself.
QVariant refusal(int type)
{
    if (type == QTextDocument::ImageResource) {
        return QVariant(blockedImageBytes());
    }
    return QVariant(QByteArray(""));
}
} // namespace

bool isSafeExternalScheme(const QUrl &url)
{
    if (!url.isValid() || url.isEmpty()) {
        return false;
    }
    const QString s = url.scheme();
    return s == QLatin1String("http") || s == QLatin1String("https")
        || s == QLatin1String("mailto");
}

void openModelLink(QWidget *parent, const QUrl &url)
{
    // Nothing parseable to open: refuse. (Fail closed — this is reached with
    // whatever string the model put in the markdown href.)
    if (!url.isValid() || url.isEmpty()) {
        return;
    }
    if (isSafeExternalScheme(url)) {
        QDesktopServices::openUrl(url);
        return;
    }
    // Anything else — file://, a scheme-less absolute path, smb://, or any
    // custom handler some installed application registered — is exactly the
    // case where the link TEXT the human read has nothing to do with what the
    // OS is about to launch. One human decision, defaulting to No, on a dialog
    // that shows the real target.
    const QString target = url.toString(QUrl::PrettyDecoded);
    const QString scheme = url.scheme().isEmpty() ? i18nc("no URL scheme", "(none)")
                                                  : url.scheme();
    const auto answer = KMessageBox::warningTwoActions(
        parent,
        i18n("This link came from the agent's message. It does not open in a "
             "browser — it hands this target to whatever application your "
             "system has registered for it:\n\n%1\n\nScheme: %2",
             target, scheme),
        i18nc("@title:window", "Open link from agent?"),
        // Deliberately NO "don't ask again" key: a suppressible prompt is an
        // authority the agent could get granted once and then reuse forever.
        KGuiItem(i18nc("@action:button", "Open Anyway")),
        KStandardGuiItem::cancel());
    if (answer == KMessageBox::PrimaryAction) {
        QDesktopServices::openUrl(url);
    }
}

void allowMediaRoot(const QString &dir)
{
    const QString c = canonicalDir(dir);
    if (c.isEmpty()) {
        return; // unresolvable: refuse to add, rather than add a loose prefix
    }
    QStringList &roots = globalRoots();
    if (!roots.contains(c)) {
        roots << c;
    }
}

QStringList allowedMediaRoots()
{
    // Retry the attachment-store seed: on a fresh profile those directories do
    // not exist until the first attachment is written, and canonicalFilePath
    // returns nothing for a path that is not there yet.
    const QStringList atts = attachmentRoots();
    for (const QString &d : atts) {
        allowMediaRoot(d);
    }
    return globalRoots();
}

QByteArray blockedImageBytes()
{
    // A 1x1 fully transparent PNG, written out by hand rather than encoded at
    // runtime: this is the value a security gate returns, so it must not depend
    // on an image plugin being present (a failed encode would be an empty
    // QByteArray, which is exactly the "no bytes" that lets QTextImageHandler
    // fall back to reading the refused path). Pinned by SafeContentTest.
    static const unsigned char kPng[] = {
        0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
        0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
        0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
        0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
        0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
        0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82};
    static const QByteArray bytes(reinterpret_cast<const char *>(kPng), sizeof(kPng));
    return bytes;
}

QVariant loadGuardedResource(const QUrl &name, const QStringList &roots, int type)
{
    if (!name.isValid()) {
        // Unparseable: refuse rather than answer "not ours", which would hand
        // the string back to a base implementation that resolves it itself.
        return refusal(type);
    }
    const QString scheme = name.scheme();
    // Self-contained or app-owned bytes: no filesystem, nothing to leak.
    if (scheme == QLatin1String("data") || scheme == QLatin1String("qrc")) {
        return {}; // let the caller's base implementation handle these
    }
    // Remote schemes never load in Qt6 rich text anyway; refuse explicitly so a
    // future Qt that grows a network loader cannot turn this into exfiltration.
    if (!scheme.isEmpty() && scheme != QLatin1String("file")) {
        return refusal(type);
    }

    QStringList allowed = allowedMediaRoots();
    for (const QString &r : roots) {
        const QString c = canonicalDir(r);
        if (!c.isEmpty()) {
            allowed << c;
        }
    }

    QString path = name.isLocalFile() ? name.toLocalFile() : name.path();
    if (path.isEmpty()) {
        return refusal(type);
    }
    QStringList candidates;
    if (QDir::isAbsolutePath(path)) {
        candidates << path;
    } else {
        // A relative image name (a markdown preview of a file next to its
        // images). It is resolved against the allowed roots and nothing else —
        // never against the process working directory.
        for (const QString &r : allowed) {
            candidates << r + path;
        }
    }
    for (const QString &cand : std::as_const(candidates)) {
        const QFileInfo fi(cand);
        const QString canonical = fi.canonicalFilePath();
        if (canonical.isEmpty()) {
            continue; // missing, or a symlink we cannot resolve: refuse
        }
        if (!underRoot(canonical, allowed)) {
            continue;
        }
        const QFileInfo target(canonical);
        // isFile() is true for REGULAR files only: a /dev/zero, a FIFO or a
        // directory is refused here, which is what keeps the GUI thread from
        // blocking forever on a read that never ends.
        if (!target.isFile() || target.size() > kMaxResourceBytes) {
            continue;
        }
        QFile f(canonical);
        if (!f.open(QIODevice::ReadOnly)) {
            continue;
        }
        const QByteArray bytes = f.read(kMaxResourceBytes);
        if (bytes.isEmpty()) {
            continue;
        }
        return QVariant(bytes);
    }
    // Refused. Never "no bytes" — see blockedImageBytes' comment in the header:
    // both QTextDocument and QTextImageHandler re-open the path themselves when
    // the resource comes back empty, so a refusal has to be a value they accept.
    return refusal(type);
}

TailRead readBoundedTail(const QString &path, qint64 &offset, qint64 maxBytes)
{
    TailRead out;
    if (path.isEmpty() || maxBytes <= 0) {
        return out;
    }
    const QFileInfo info(path);
    // isFile() is REGULAR-files-only: a FIFO or device node under this path
    // would park the GUI thread inside read() forever, and its size() is a lie
    // (0), so every bound below would be meaningless too. Fail closed.
    if (!info.exists() || !info.isFile()) {
        return out;
    }
    const qint64 size = info.size();
    if (offset < 0 || size < offset) {
        // Truncated or rewritten underneath us: whatever the caller parsed no
        // longer describes this file.
        offset = 0;
        out.restarted = true;
    }
    if (size - offset > maxBytes) {
        // The writer outran us. Jump to the last window rather than reading the
        // backlog — the alternative is either an unbounded read or falling
        // further behind on every poll.
        offset = size - maxBytes;
        out.gap = true;
    }
    QFile f(path);
    if (!f.open(QIODevice::ReadOnly)) {
        return out;
    }
    if (!f.seek(offset)) {
        return out;
    }
    out.bytes = f.read(maxBytes);
    if (out.bytes.isEmpty()) {
        return out;
    }
    offset = f.pos();
    return out;
}

GuardedTextBrowser::GuardedTextBrowser(QWidget *parent) : QTextBrowser(parent) {}

void GuardedTextBrowser::addLocalRoot(const QString &dir)
{
    if (!dir.isEmpty() && !m_roots.contains(dir)) {
        m_roots << dir;
    }
}

QVariant GuardedTextBrowser::loadResource(int type, const QUrl &name)
{
    const QVariant v = loadGuardedResource(name, m_roots, type);
    if (!v.isValid()) {
        // "Not ours": data:/qrc: only — no filesystem in either.
        return QTextBrowser::loadResource(type, name);
    }
    // Served or refused, this is the answer. The base implementation is never
    // reached with a path, so it never gets to do its own QFile read.
    return v;
}

GuardedTextDocument::GuardedTextDocument(QObject *parent) : QTextDocument(parent) {}

void GuardedTextDocument::addLocalRoot(const QString &dir)
{
    if (!dir.isEmpty() && !m_roots.contains(dir)) {
        m_roots << dir;
    }
}

QVariant GuardedTextDocument::loadResource(int type, const QUrl &name)
{
    const QVariant v = loadGuardedResource(name, m_roots, type);
    if (!v.isValid()) {
        return QTextDocument::loadResource(type, name); // data:/qrc: only
    }
    return v;
}
} // namespace agentkate
