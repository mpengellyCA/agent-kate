// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QList>
#include <QStringList>

class QImage;
class QJsonArray;
class QJsonObject;

// Attachment-building helpers split out of AgentPanel: the file-reading, binary
// sniffing, image base64-encoding and ranged-excerpt extraction that turn local
// paths into the {kind,name,mediaType,…} attachment objects the next message
// carries. None of this touches the panel's widget tree — it reads files and
// mutates a QJsonArray — so it lives here as free functions. The panel keeps the
// chip UI (rebuildAttachChips) and the rejection banner (showAttachNotice); these
// builders only assemble the data and report which files were skipped and why.
namespace agentkate
{
// buildPathAttachments appends an attachment object for each readable path in
// `paths` to `attachments`, de-duplicating against entries already present.
// `workspace` (may be empty) is used only to flag paths outside the project root
// with an "outside" marker. Returns the human-readable reasons any files were
// skipped (binary, too large, unreadable, a folder, gone), for the caller's
// banner.
QStringList buildPathAttachments(const QStringList &paths, const QString &workspace,
                                 QJsonArray &attachments);

// buildItemAttachments appends a ranged text-excerpt attachment for each item in
// `items` that carries a "line" key (widening by a few lines of context each
// side); items without a line range are collected into `wholeFile` for the
// caller to route through buildPathAttachments. De-duplicates by display name.
// Excerpts are truncated and budgeted exactly as whole files are — a line range
// bounds nothing in a minified file, whose single line is the entire file.
// Returns the human-readable reasons any items were skipped, for the caller's
// banner.
QStringList buildItemAttachments(const QJsonArray &items, QJsonArray &attachments,
                                 QStringList &wholeFile);

// buildImageAttachments appends an image attachment for each raw QImage in
// `images` — pixels off the clipboard (Ctrl+V) or a drag that carries an image
// but no file URL (a browser image, a Spectacle drag). Each is encoded as PNG
// and then travels the identical path a dropped image file takes: same
// per-image cap, same shared wire budget, same content-addressed copy in durable
// app data, so the chip thumbnail and chip-open behave the same. There is no
// origin file, so `path` points at that copy — it is the only file there is.
// Returns the human-readable reasons any images were skipped.
QStringList buildImageAttachments(const QList<QImage> &images, QJsonArray &attachments);

// resolveAttachmentPath returns the file that actually holds the bytes this
// attachment was sent with — the file to draw a thumbnail from and the file a
// chip click should open — or an empty string if neither copy survives.
//
// The origin path is preferred, because for a workspace file that is the copy
// worth opening (edits there count). But "the origin path exists" is not "the
// origin path still holds those bytes": capture tools reuse fixed names
// (/tmp/screenshot.png), so the next screenshot silently replaces the pixels
// behind an old chip. So the cached copy wins whenever it exists AND the origin
// is missing or a different size than the copy. Being outside the project is not
// on its own a reason to distrust it: an untouched ~/Downloads image is still
// the file the user recognises, and the copy is opaque.
//
// The recorded cachePath is resolved rather than trusted: copies used to be
// written to the cache dir and now live in app data, so when the recorded dir
// has nothing, the content-addressed basename is looked up in the other one.
QString resolveAttachmentPath(const QJsonObject &att);

// PrunePolicy is how the age rule and the size rule combine, which is a property
// of what the directory holds rather than of the numbers.
enum class PrunePolicy {
    // Derived data: either rule alone may delete a file. Losing one costs a
    // re-run or a thumbnail, so age is reason enough.
    Cache,
    // User data: a file is deleted only when the directory is over `maxBytes`
    // AND that file is past `maxAgeSecs` — age alone never takes anything,
    // because for these files there is nothing else to fall back on.
    Durable,
};

// pruneCacheDir deletes files in `dir` older than `maxAgeSecs`, then, if the
// directory still exceeds `maxBytes`, deletes oldest-first until it fits (under
// PrunePolicy::Durable the age sweep is skipped and the oldest-first pass stops
// at the age line, so both rules must agree). Files modified within the last
// hour are never deleted (they may be a chip a card is about to redraw, or a
// .tmp another attach is renaming into place), though their bytes still count
// against the cap. Returns files deleted.
int pruneCacheDir(const QString &dir, qint64 maxAgeSecs, qint64 maxBytes,
                  PrunePolicy policy = PrunePolicy::Cache);

// scheduleImageCachePrune arms a single delayed sweep of the image dirs a few
// seconds after startup, so it never competes with launch. Only the first call
// per process does anything.
//
// The two dirs are swept under different policies because they are different
// storage classes. Tool-result images are derived data (the tool can be re-run)
// and get the ordinary cache policy. Attachment copies are user data — the copy
// is what makes an attachment outlive its origin, and for pasted pixels it is
// the only copy in existence — so their sweep is a backstop against an unbounded
// dir, not a reclaim: very old AND only once the dir has grown past a gigabyte.
//
// GUI thread only: the sweep itself runs on the thread pool, but the target dirs
// are resolved here, because QStandardPaths needs a live QCoreApplication and the
// global pool outlives it.
void scheduleImageCachePrune();
} // namespace agentkate
