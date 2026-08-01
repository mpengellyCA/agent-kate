// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AttachmentBuilder.h"

#include <KLocalizedString>

#include <QCoreApplication>
#include <QCryptographicHash>
#include <QDateTime>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QBuffer>
#include <QHash>
#include <QImage>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QSet>
#include <QStandardPaths>
#include <QThreadPool>
#include <QTimer>

#include <algorithm>
#include <cstdio> // ::rename — atomic replace, which QFile::rename is not

namespace agentkate
{
namespace
{
// The combined encoded size of one message's attachments. A single JSON-RPC
// frame is capped at 16 MB (core/internal/ipc/server.go maxFrameBytes), and
// overflowing it is NOT a clean per-request error: the core's bufio.Scanner
// stops, its read loop exits and the UI connection is dropped. So the budget
// sits well inside the cap, leaving room for the rest of the message JSON.
// Per-image size is capped separately (5 MB) — this bounds the sum, which
// nothing did before: fifteen screenshots in one message reached the cliff.
constexpr qsizetype kMaxTotalAttachBytes = 12 * 1024 * 1024;

// The per-attachment text cap, applied to whole files and to ranged excerpts
// alike: a minified file is one "line" that is the entire file, so a line range
// bounds nothing on its own.
constexpr qsizetype kMaxTextBytes = 256 * 1024;

// The per-image cap, applied to file images and to raw (clipboard/drag) images
// alike — base64 inflates whatever passes it by a third on the wire.
constexpr qsizetype kMaxImageBytes = 5 * 1024 * 1024;

// The ceiling on how much of a file a RANGED excerpt may scan (audit F11).
//
// An excerpt is addressed by line, and there is no way to find line N without
// reading up to it — but "up to it" must still be bounded, or a search hit deep
// inside a multi-gigabyte log pulls the whole thing into the GUI process. Eight
// megabytes reaches past the end of any plausible source file; a hit beyond it
// is reported as a skip rather than served by an unbounded read.
constexpr qsizetype kMaxExcerptScanBytes = 8 * 1024 * 1024;

// readCapped reads at most `cap` bytes plus ONE more, so the caller can tell
// "exactly cap bytes" from "cap bytes and there was more". Nothing here ever
// calls readAll(): QFileInfo::size() is consulted first where the answer is a
// refusal, and this bounds the read where it is a truncation — including for
// the files whose reported size is a lie (procfs, character devices, a file
// another process is still appending to).
QByteArray readCapped(QFile &file, qsizetype cap)
{
    return file.read(cap + 1);
}

// Retention, split by storage CLASS rather than by where the bytes came from.
//
// Attachment copies are user data, not derived data: the whole reason they are
// written is that the origin may be gone (a reaped temp screenshot) or may never
// have existed (pasted pixels), and the card keeps no body of its own —
// compactAttachments strips dataB64 once the message is sent. Deleting one loses
// the picture, so the sweep over them is a last-resort backstop against an
// unbounded dir, not a routine reclaim: half a year old AND the dir past a
// gigabyte, oldest first.
constexpr qint64 kAttachMaxAgeSecs = 180 * 24 * 60 * 60;
constexpr qint64 kAttachMaxBytes = 1024 * 1024 * 1024;

// Tool-result images ARE derived data — the tool can be run again — so they keep
// the ordinary cache policy.
constexpr qint64 kToolCacheMaxAgeSecs = 30 * 24 * 60 * 60;
constexpr qint64 kToolCacheMaxBytes = 512 * 1024 * 1024;

// The floor no rule may cross. A file written moments ago is far more likely to
// be live — the copy a card is about to redraw from, or a half-written .tmp
// another attach is about to rename into place — than garbage, and a clock skew
// or a restored backup can make a fresh file look ancient.
constexpr qint64 kCacheMinAgeSecs = 60 * 60;

// wireCost is what this attachment actually costs the frame: the serialised
// object, not the character count of its body. The two differ a lot — the wire
// is escaped UTF-8, so non-ASCII text and every \n or " in it cost more than one
// byte per QString character, and name/path/cachePath are not free either.
qsizetype wireCost(const QJsonObject &att)
{
    return QJsonDocument(att).toJson(QJsonDocument::Compact).size();
}

// AttachBudget keeps the running wire total for ONE message across every attach
// action that contributes to it: a drop, then a file dialog, then a search-result
// drag all append to the same array, so a fresh call has to start from what is
// already in there rather than from zero.
class AttachBudget
{
public:
    explicit AttachBudget(const QJsonArray &attachments)
    {
        for (const QJsonValue &av : attachments) {
            m_total += wireCost(av.toObject());
        }
    }

    // admit books `att` and returns true when it still fits; a false return means
    // the caller must skip it with a reason. Over-budget has to be a skip rather
    // than a truncated send: overflowing the core's 16 MB frame cap is not a
    // clean per-request error, it drops the UI connection.
    bool admit(const QJsonObject &att)
    {
        const qsizetype cost = wireCost(att);
        if (m_total + cost > kMaxTotalAttachBytes) {
            return false;
        }
        m_total += cost;
        return true;
    }

private:
    qsizetype m_total = 0;
};

// overBudgetReason phrases the skip so the user can act on it — which file, and
// against which limit.
QString overBudgetReason(const QString &name)
{
    return i18n("%1 — would exceed the %2 MB total attachment limit", name,
                kMaxTotalAttachBytes / (1024 * 1024));
}

// storeImageCopy writes an attached image's bytes into `dir` under a
// content-addressed name and returns that path, so previews survive the origin
// being deleted.
//
// Screenshots are routinely temp files that a capture tool reaps moments later,
// and the You card's chip thumbnail is re-loaded FROM THE PATH (the card keeps
// no body — compactAttachments strips dataB64 so a screenshot-heavy thread does
// not sit in memory twice). Without a copy, every chip degrades to a generic
// glyph and clicking it reports the file as missing.
//
// For a file image this is deliberately a copy ALONGSIDE the origin path rather
// than a replacement: `path` keeps meaning "where it came from", so clicking a
// chip for a real workspace file still opens that file in the editor, where
// edits count. Mirrors the tool-result image cache in AgentPanel::renderEvent.
QString storeImageCopy(const QByteArray &bytes, const QString &ext, const QString &dir)
{
    if (dir.isEmpty() || !QDir().mkpath(dir)) {
        return QString(); // no store dir — degrade to origin-path-only behaviour
    }
    // Content-addressed: re-attaching the same file reuses one copy, and two
    // different files with the same basename cannot collide.
    const QString digest = QString::fromLatin1(
        QCryptographicHash::hash(bytes, QCryptographicHash::Sha256).toHex().left(16));
    const QString path =
        dir + QStringLiteral("/%1.%2").arg(digest, ext.isEmpty() ? QStringLiteral("img") : ext);
    const QFileInfo cached(path);
    if (cached.exists() && cached.size() == bytes.size()) {
        return path; // already cached by an earlier attach
    }
    // Absent, or present but the wrong length — a copy some crash or full disk
    // left half-written. Because the name is content-addressed, existence alone
    // would poison that digest forever, so a short file is rewritten. The write
    // goes to a private temp name and is renamed into place, so the final path
    // is never observed partial by a concurrent attach or by the delegate
    // redrawing a thumbnail.
    const QString tmp =
        path + QStringLiteral(".tmp%1").arg(QCoreApplication::applicationPid());
    QFile out(tmp);
    if (!out.open(QIODevice::WriteOnly)) {
        return QString();
    }
    // Every step checked: an unflushed short write publishes a truncated image
    // under a content-addressed name, which then looks cached forever.
    const bool ok = out.write(bytes) == bytes.size() && out.flush()
        && out.error() == QFileDevice::NoError;
    out.close();
    if (!ok) {
        QFile::remove(tmp); // never leave a truncated image to be drawn
        return QString();
    }
    // rename(2) replaces an existing target atomically on Linux. QFile::rename
    // cannot: it refuses an existing target, so it would need a remove first —
    // a window in which a concurrent attach or a thumbnail redraw sees the path
    // missing and gives up on it.
    if (::rename(QFile::encodeName(tmp).constData(),
                 QFile::encodeName(path).constData())
        != 0) {
        QFile::remove(tmp);
        return QString();
    }
    return path;
}

// attachmentStoreDir is where EVERY attachment copy lives — a file image's copy
// as much as raw pasted pixels. App data, not cache: the copy exists precisely
// so the attachment survives its origin being deleted, so a cache-class sweep
// deleting it after a fortnight would defeat the one thing it is for.
QString attachmentStoreDir()
{
    const QString base =
        QStandardPaths::writableLocation(QStandardPaths::AppDataLocation);
    return base.isEmpty() ? QString() : base + QStringLiteral("/attachments");
}

// legacyAttachmentStoreDir is where copies were written before that split. Never
// written to now, only read: cards from those sessions still name it.
QString legacyAttachmentStoreDir()
{
    const QString base =
        QStandardPaths::writableLocation(QStandardPaths::CacheLocation);
    return base.isEmpty() ? QString() : base + QStringLiteral("/attachments");
}

QString durableImageCopy(const QByteArray &bytes, const QString &ext)
{
    return storeImageCopy(bytes, ext, attachmentStoreDir());
}

// locateStoredCopy resolves a recorded cachePath to a file that actually exists.
// The name is content-addressed, so when the recorded directory has nothing the
// same basename in the other attachment dir is the same bytes: that is what
// keeps cards written before the cache→app-data move drawing their thumbnails.
QString locateStoredCopy(const QString &cached)
{
    if (cached.isEmpty()) {
        return QString();
    }
    if (QFileInfo(cached).isFile()) {
        return cached;
    }
    const QString name = QFileInfo(cached).fileName();
    if (name.isEmpty()) {
        return QString();
    }
    for (const QString &dir : {attachmentStoreDir(), legacyAttachmentStoreDir()}) {
        if (dir.isEmpty()) {
            continue;
        }
        const QString alt = dir + QLatin1Char('/') + name;
        if (alt != cached && QFileInfo(alt).isFile()) {
            return alt;
        }
    }
    return QString();
}
} // namespace

QStringList buildPathAttachments(const QStringList &paths, const QString &workspace,
                                 QJsonArray &attachments)
{
    QStringList skipped; // human-readable reasons, returned to the caller
    if (paths.isEmpty()) {
        return skipped;
    }
    static const QHash<QString, QString> imageTypes{
        {QStringLiteral("png"), QStringLiteral("image/png")},
        {QStringLiteral("jpg"), QStringLiteral("image/jpeg")},
        {QStringLiteral("jpeg"), QStringLiteral("image/jpeg")},
        {QStringLiteral("gif"), QStringLiteral("image/gif")},
        {QStringLiteral("webp"), QStringLiteral("image/webp")},
        {QStringLiteral("bmp"), QStringLiteral("image/bmp")}};

    // Existing whole-file attachments, keyed by absolute path, for dedup. The
    // budget is seeded from the same array, so it spans every attach action for
    // this message rather than resetting each time the file dialog is used.
    QSet<QString> existingPaths;
    AttachBudget budget(attachments);
    for (const QJsonValue &av : std::as_const(attachments)) {
        const QString p = av.toObject().value(QStringLiteral("path")).toString();
        if (!p.isEmpty()) {
            existingPaths.insert(p);
        }
    }

    for (const QString &path : paths) {
        const QFileInfo info(path);
        if (!info.exists() || info.isDir()) {
            // Directories aren't attachable as a single blob — skip with a note.
            // A missing path gets one too: pasting a file URL whose file was
            // since deleted or unmounted is common, and dropping it silently
            // looks exactly like the paste doing nothing at all.
            skipped << (info.isDir()
                            ? i18n("%1 — folders can't be attached", info.fileName())
                            : i18n("%1 — file no longer exists", info.fileName()));
            continue;
        }
        const QString abs = info.absoluteFilePath();
        if (existingPaths.contains(abs)) {
            continue; // already attached — skip quietly
        }
        const QString ext = info.suffix().toLower();
        const bool isImage = imageTypes.contains(ext);

        // SIZE FIRST, BYTES SECOND (audit F11). An image over the cap is refused
        // outright, and QFileInfo::size() answers that from the inode — a 2 GB
        // file must never be read into the GUI process just to discover it is
        // too big. The read below is bounded too, because a reported size can be
        // wrong (procfs, devices, a file still being appended to).
        if (isImage && info.size() > kMaxImageBytes) {
            skipped << i18n("%1 — image too large to attach (over 5 MB)",
                            info.fileName());
            continue;
        }

        QFile file(path);
        if (!file.open(QIODevice::ReadOnly)) {
            skipped << i18n("%1 — could not be read", info.fileName());
            continue;
        }
        // Images may be read up to the cap (+1, to catch a lying size); text is
        // truncated at the cap anyway, so nothing past it is ever worth reading.
        const QByteArray bytes =
            readCapped(file, isImage ? kMaxImageBytes : kMaxTextBytes);

        // A path outside the workspace root is allowed (external drops are the
        // point) but flagged so the chip can hint at it.
        const bool outside = !workspace.isEmpty()
            && !abs.startsWith(QDir(workspace).absolutePath() + QLatin1Char('/'));

        QJsonObject att{{QStringLiteral("name"), info.fileName()},
                        {QStringLiteral("path"), abs}};
        if (outside) {
            att[QStringLiteral("outside")] = true;
        }
        if (isImage) {
            if (bytes.size() > kMaxImageBytes) {
                skipped << i18n("%1 — image too large to attach (over 5 MB)",
                                info.fileName());
                continue;
            }
            att[QStringLiteral("kind")] = QStringLiteral("image");
            att[QStringLiteral("mediaType")] = imageTypes.value(ext);
            att[QStringLiteral("dataB64")] = QString::fromLatin1(bytes.toBase64());
        } else {
            // Binary sniff: a NUL in the first ~8 KB means this isn't text.
            if (bytes.left(8 * 1024).contains('\0')) {
                skipped << i18n("%1 — binary file, can't be added as text context",
                                info.fileName());
                continue;
            }
            QByteArray textBytes = bytes;
            QString suffix;
            if (textBytes.size() > kMaxTextBytes) {
                textBytes.truncate(kMaxTextBytes);
                suffix = QStringLiteral("\n… (truncated)");
            }
            att[QStringLiteral("kind")] = QStringLiteral("text");
            att[QStringLiteral("text")] = QString::fromUtf8(textBytes) + suffix;
        }
        // Budgeted once the body is known, and BEFORE any cache copy: a rejected
        // attachment must not leave a permanent file behind in the cache dir.
        // The cachePath added below is a couple of hundred bytes the budget did
        // not see, which the 4 MB of headroom under the frame cap absorbs.
        if (!budget.admit(att)) {
            skipped << overBudgetReason(info.fileName());
            continue;
        }
        if (isImage) {
            // Keep our own copy so the chip thumbnail and chip-open survive the
            // origin being deleted (temp screenshots, routinely).
            const QString cached = durableImageCopy(bytes, ext);
            if (!cached.isEmpty()) {
                att[QStringLiteral("cachePath")] = cached;
            }
        }
        existingPaths.insert(abs);
        attachments.append(att);
    }
    return skipped;
}

QStringList buildItemAttachments(const QJsonArray &items, QJsonArray &attachments,
                                 QStringList &wholeFile)
{
    QStringList skipped;
    // Names of existing ranged (text-excerpt) attachments, for dedup. The budget
    // is seeded from the same array: excerpts share the one frame with whatever
    // files were already dropped or picked for this message.
    QSet<QString> existingNames;
    AttachBudget budget(attachments);
    for (const QJsonValue &av : std::as_const(attachments)) {
        existingNames.insert(av.toObject().value(QStringLiteral("name")).toString());
    }

    for (const QJsonValue &iv : items) {
        const QJsonObject it = iv.toObject();
        const QString path = it.value(QStringLiteral("path")).toString();
        if (path.isEmpty()) {
            continue;
        }
        if (!it.contains(QStringLiteral("line"))) {
            wholeFile << path;
            continue;
        }
        const QFileInfo info(path);
        if (!info.exists() || info.isDir()) {
            continue;
        }
        QFile file(path);
        if (!file.open(QIODevice::ReadOnly | QIODevice::Text)) {
            skipped << i18n("%1 — could not be read", info.fileName());
            continue;
        }
        // Bounded read (audit F11): the excerpt is addressed by line, so the
        // scan has to walk from the start of the file — but only ever as far as
        // kMaxExcerptScanBytes, never readAll(). A search hit past that point is
        // reported as a skip; serving it would mean holding a multi-gigabyte log
        // in the GUI process to copy sixteen lines out of it.
        const QByteArray bytes = readCapped(file, kMaxExcerptScanBytes);
        const bool scanTruncated = bytes.size() > kMaxExcerptScanBytes;
        if (bytes.left(8 * 1024).contains('\0')) {
            skipped << i18n("%1 — binary file, can't be added as text context",
                            info.fileName());
            continue;
        }
        QStringList lines =
            QString::fromUtf8(bytes.left(kMaxExcerptScanBytes)).split(QLatin1Char('\n'));
        if (scanTruncated && !lines.isEmpty()) {
            // The cap lands mid-line; that half line is not a line of the file.
            lines.removeLast();
        }

        // line/endLine are 0-based; widen by ~8 lines of context each side.
        constexpr int kContext = 8;
        const int line = it.value(QStringLiteral("line")).toInt();
        const int endLine = it.value(QStringLiteral("endLine")).toInt(line);
        const int from = qMax(0, line - kContext);
        const int to = qMin(lines.size() - 1, endLine + kContext);
        if (from > to) {
            if (scanTruncated) {
                // Silence here reads as "the drop did nothing"; say why.
                skipped << i18n("%1 — file too large to excerpt (over %2 MB)",
                                info.fileName(),
                                kMaxExcerptScanBytes / (1024 * 1024));
            }
            continue;
        }
        QString excerpt;
        for (int i = from; i <= to; ++i) {
            excerpt += lines.at(i);
            excerpt += QLatin1Char('\n');
            if (excerpt.size() > kMaxTextBytes) {
                break; // a minified file is one line that is the whole file
            }
        }
        if (excerpt.size() > kMaxTextBytes) {
            excerpt.truncate(kMaxTextBytes);
            if (!excerpt.isEmpty() && excerpt.back().isHighSurrogate()) {
                excerpt.chop(1); // don't end on half a code point
            }
            excerpt += QStringLiteral("\n… (truncated)");
        }
        // The range rides purely in the display name; the core sees plain text.
        const QString name = QStringLiteral("%1:%2-%3")
                                 .arg(info.fileName())
                                 .arg(line + 1)
                                 .arg(endLine + 1);
        if (existingNames.contains(name)) {
            continue;
        }
        const QJsonObject att{
            {QStringLiteral("kind"), QStringLiteral("text")},
            {QStringLiteral("name"), name},
            {QStringLiteral("path"), info.absoluteFilePath()},
            {QStringLiteral("text"), excerpt},
        };
        if (!budget.admit(att)) {
            skipped << overBudgetReason(name);
            continue;
        }
        existingNames.insert(name);
        attachments.append(att);
    }
    return skipped;
}

QStringList buildImageAttachments(const QList<QImage> &images, QJsonArray &attachments)
{
    QStringList skipped;
    if (images.isEmpty()) {
        return skipped;
    }
    // Raw images have no path to dedup on, so the content-addressed name is the
    // key: pasting the same screenshot twice attaches it once. The budget is
    // seeded from the array for the same reason every other builder does it —
    // paste, drop and file dialog all fill one message, which rides one frame.
    QSet<QString> existingNames;
    AttachBudget budget(attachments);
    for (const QJsonValue &av : std::as_const(attachments)) {
        existingNames.insert(av.toObject().value(QStringLiteral("name")).toString());
    }

    for (const QImage &image : images) {
        if (image.isNull()) {
            // The caller offered an image the platform could not decode (a
            // format Qt has no plugin for). Silence here reads as "nothing
            // happened", so it is reported like any other refusal.
            skipped << i18n("image — couldn't be decoded");
            continue;
        }
        QByteArray bytes;
        {
            QBuffer buf(&bytes);
            if (!buf.open(QIODevice::WriteOnly) || !image.save(&buf, "PNG")) {
                skipped << i18n("pasted image — could not be encoded");
                continue;
            }
        }
        const QString digest = QString::fromLatin1(
            QCryptographicHash::hash(bytes, QCryptographicHash::Sha256).toHex().left(8));
        const QString name = QStringLiteral("image-%1.png").arg(digest);
        if (existingNames.contains(name)) {
            continue; // same pixels already attached — skip quietly
        }
        if (bytes.size() > kMaxImageBytes) {
            skipped << i18n("%1 — image too large to attach (over 5 MB)", name);
            continue;
        }
        QJsonObject att{
            {QStringLiteral("kind"), QStringLiteral("image")},
            {QStringLiteral("name"), name},
            {QStringLiteral("mediaType"), QStringLiteral("image/png")},
            {QStringLiteral("dataB64"), QString::fromLatin1(bytes.toBase64())},
        };
        // Budgeted before the cache write, exactly as file images are: a refused
        // attachment must not leave a file behind in the cache dir.
        if (!budget.admit(att)) {
            skipped << overBudgetReason(name);
            continue;
        }
        const QString stored = durableImageCopy(bytes, QStringLiteral("png"));
        if (!stored.isEmpty()) {
            att[QStringLiteral("cachePath")] = stored;
            // Unlike a dropped file there is no origin to preserve, so `path`
            // is the stored copy — otherwise the chip has nothing to open and
            // the You card, whose body is stripped, nothing to redraw from.
            att[QStringLiteral("path")] = stored;
        }
        existingNames.insert(name);
        attachments.append(att);
    }
    return skipped;
}

QString resolveAttachmentPath(const QJsonObject &att)
{
    const QString path = att.value(QStringLiteral("path")).toString();
    // Not the recorded string but the file it names — or, when the copies moved
    // out from under an old card, its twin in the other attachment dir.
    const QString cached =
        locateStoredCopy(att.value(QStringLiteral("cachePath")).toString());
    const QFileInfo cachedInfo(cached);
    const bool haveCache = !cached.isEmpty() && cachedInfo.isFile();
    const QFileInfo originInfo(path);
    if (path.isEmpty() || !originInfo.isFile()) {
        return haveCache ? cached : QString();
    }
    if (!haveCache) {
        return path;
    }
    // A size mismatch is the cheap, no-read signal that the bytes changed under
    // us — the next screenshot written over /tmp/screenshot.png. That test runs
    // for outside-the-project origins too: "outside" alone must not condemn an
    // untouched file, or a chip for a still-intact ~/Downloads image would open
    // an opaque cache copy instead of the file the user recognises.
    if (originInfo.size() != cachedInfo.size()) {
        return cached;
    }
    return path;
}

int pruneCacheDir(const QString &dir, qint64 maxAgeSecs, qint64 maxBytes,
                  PrunePolicy policy)
{
    QDir d(dir);
    if (dir.isEmpty() || !d.exists()) {
        return 0;
    }
    struct Candidate {
        QString path;
        qint64 size;
        qint64 age;
    };
    QList<Candidate> candidates;
    const QDateTime now = QDateTime::currentDateTime();
    qint64 total = 0;
    int removed = 0;
    const QFileInfoList entries =
        d.entryInfoList(QDir::Files | QDir::Hidden | QDir::NoSymLinks);
    for (const QFileInfo &fi : entries) {
        const qint64 size = fi.size();
        const qint64 age = fi.lastModified().secsTo(now);
        if (age < kCacheMinAgeSecs) {
            total += size; // untouchable, but it still occupies the cap
            continue;
        }
        if (policy == PrunePolicy::Cache && age > maxAgeSecs
            && QFile::remove(fi.absoluteFilePath())) {
            ++removed;
            continue;
        }
        total += size;
        candidates.append({fi.absoluteFilePath(), size, age});
    }
    if (total <= maxBytes) {
        return removed;
    }
    // Over the cap even after the age sweep: give up the oldest first, since
    // the newest copies are the ones a visible card is most likely to want.
    std::sort(candidates.begin(), candidates.end(),
              [](const Candidate &a, const Candidate &b) { return a.age > b.age; });
    for (const Candidate &c : std::as_const(candidates)) {
        if (total <= maxBytes) {
            break;
        }
        if (policy == PrunePolicy::Durable && c.age <= maxAgeSecs) {
            break; // sorted oldest-first, so nothing left is old enough either
        }
        if (QFile::remove(c.path)) {
            total -= c.size;
            ++removed;
        }
    }
    return removed;
}

void scheduleImageCachePrune()
{
    static bool armed = false;
    if (armed || !QCoreApplication::instance()) {
        return;
    }
    armed = true;
    QTimer::singleShot(8000, QCoreApplication::instance(), [] {
        // Resolve the target directories HERE, on the GUI thread, and hand the
        // task plain strings. QStandardPaths reads QCoreApplication's
        // organisation/application name, and the global QThreadPool is drained
        // (and its tasks may still be starting) AFTER ~QCoreApplication — so a
        // sweep still in flight at quit would otherwise call QStandardPaths with
        // no app object behind it. Copies by value: the task outlives this scope.
        // User data: swept only as a backstop, and gently (see kAttachMaxBytes).
        const QString storeDir = attachmentStoreDir();
        // Derived data: ordinary cache policy.
        const QString cacheBase =
            QStandardPaths::writableLocation(QStandardPaths::CacheLocation);
        const QString toolImagesDir =
            cacheBase.isEmpty() ? QString() : cacheBase + QStringLiteral("/tool-images");
        if (storeDir.isEmpty() && toolImagesDir.isEmpty()) {
            return;
        }
        // Off the GUI thread: the sweep stats every file in a store capped at
        // 1 GB and then unlinks, which is an unbounded stall on whatever thread
        // runs it. Nothing in here touches a QObject, the UI, or any global that
        // needs QCoreApplication — the work is QDir/QFile only over paths already
        // resolved above. The result is not awaited: a sweep that has not
        // finished simply runs longer.
        QThreadPool::globalInstance()->start([storeDir, toolImagesDir] {
            if (!storeDir.isEmpty()) {
                pruneCacheDir(storeDir, kAttachMaxAgeSecs, kAttachMaxBytes,
                              PrunePolicy::Durable);
            }
            if (!toolImagesDir.isEmpty()) {
                pruneCacheDir(toolImagesDir, kToolCacheMaxAgeSecs, kToolCacheMaxBytes);
            }
            // The legacy copy dir is deliberately NOT swept: nothing writes to it
            // any more, so it cannot grow, and what is in it is the only copy
            // behind some old card's chip.
        });
    });
}
} // namespace agentkate
