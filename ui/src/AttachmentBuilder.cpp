// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AttachmentBuilder.h"

#include <KLocalizedString>

#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QSet>

namespace agentkate
{
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

    // Existing whole-file attachments, keyed by absolute path, for dedup.
    QSet<QString> existingPaths;
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
        if (imageTypes.contains(ext)) {
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
            if (textBytes.size() > 256 * 1024) {
                textBytes.truncate(256 * 1024);
                suffix = QStringLiteral("\n… (truncated)");
            }
            att[QStringLiteral("kind")] = QStringLiteral("text");
            att[QStringLiteral("text")] = QString::fromUtf8(textBytes) + suffix;
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
    // Names of existing ranged (text-excerpt) attachments, for dedup.
    QSet<QString> existingNames;
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
        }
        // The range rides purely in the display name; the core sees plain text.
        const QString name = QStringLiteral("%1:%2-%3")
                                 .arg(info.fileName())
                                 .arg(line + 1)
                                 .arg(endLine + 1);
        if (existingNames.contains(name)) {
            continue;
        }
        existingNames.insert(name);
        attachments.append(QJsonObject{
            {QStringLiteral("kind"), QStringLiteral("text")},
            {QStringLiteral("name"), name},
            {QStringLiteral("path"), info.absoluteFilePath()},
            {QStringLiteral("text"), excerpt},
        });
    }
    return skipped;
}
} // namespace agentkate
