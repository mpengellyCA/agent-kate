// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AttachmentBuilder.h"

#include <KLocalizedString>

#include <QCoreApplication>
#include <QCryptographicHash>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QHash>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QSet>
#include <QStandardPaths>

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

// cacheImageCopy writes an attached image's bytes into our own cache dir and
// returns that path, so previews survive the origin being deleted.
//
// Screenshots are routinely temp files that a capture tool reaps moments later,
// and the You card's chip thumbnail is re-loaded FROM THE PATH (the card keeps
// no body — compactAttachments strips dataB64 so a screenshot-heavy thread does
// not sit in memory twice). Without a copy, every chip degrades to a generic
// glyph and clicking it reports the file as missing.
//
// This is deliberately a copy ALONGSIDE the origin path rather than a
// replacement: `path` keeps meaning "where it came from", so clicking a chip for
// a real workspace file still opens that file in the editor, where edits count.
// Mirrors the tool-result image cache in AgentPanel::renderEvent.
QString cacheImageCopy(const QByteArray &bytes, const QString &ext)
{
    const QString dir = QStandardPaths::writableLocation(QStandardPaths::CacheLocation)
        + QStringLiteral("/attachments");
    if (!QDir().mkpath(dir)) {
        return QString(); // no cache dir — degrade to origin-path-only behaviour
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
            if (info.isDir()) {
                skipped << i18n("%1 — folders can't be attached", info.fileName());
            }
            continue;
        }
        const QString abs = info.absoluteFilePath();
        if (existingPaths.contains(abs)) {
            continue; // already attached — skip quietly
        }
        QFile file(path);
        if (!file.open(QIODevice::ReadOnly)) {
            skipped << i18n("%1 — could not be read", info.fileName());
            continue;
        }
        const QByteArray bytes = file.readAll();
        const QString ext = info.suffix().toLower();

        // A path outside the workspace root is allowed (external drops are the
        // point) but flagged so the chip can hint at it.
        const bool outside = !workspace.isEmpty()
            && !abs.startsWith(QDir(workspace).absolutePath() + QLatin1Char('/'));

        QJsonObject att{{QStringLiteral("name"), info.fileName()},
                        {QStringLiteral("path"), abs}};
        if (outside) {
            att[QStringLiteral("outside")] = true;
        }
        const bool isImage = imageTypes.contains(ext);
        if (isImage) {
            if (bytes.size() > 5 * 1024 * 1024) {
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
            const QString cached = cacheImageCopy(bytes, ext);
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
        const QByteArray bytes = file.readAll();
        if (bytes.left(8 * 1024).contains('\0')) {
            skipped << i18n("%1 — binary file, can't be added as text context",
                            info.fileName());
            continue;
        }
        const QStringList lines =
            QString::fromUtf8(bytes).split(QLatin1Char('\n'));

        // line/endLine are 0-based; widen by ~8 lines of context each side.
        constexpr int kContext = 8;
        const int line = it.value(QStringLiteral("line")).toInt();
        const int endLine = it.value(QStringLiteral("endLine")).toInt(line);
        const int from = qMax(0, line - kContext);
        const int to = qMin(lines.size() - 1, endLine + kContext);
        if (from > to) {
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
} // namespace agentkate
