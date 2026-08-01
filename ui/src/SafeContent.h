// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QByteArray>
#include <QStringList>
#include <QTextBrowser>
#include <QTextDocument>
#include <QVariant>

class QUrl;
class QWidget;

// SafeContent is the one place that decides what MODEL-AUTHORED content is
// allowed to reach the operating system.
//
// The threat: repository content shapes what an agent writes, and what the agent
// writes is rendered in the transcript, where the human reads it and approves
// permission prompts. A markdown link's text is fully decoupled from its target,
// and Qt's rich text will happily load a local image from any path. Neither may
// become authority the human did not grant with a click:
//   * links: only http/https/mailto open straight through (the policy
//     RichTextView has always had); anything else needs an explicit confirmation
//     that names the real target.
//   * images: only files under an allowed root, only regular files, only up to a
//     byte cap — a `![x](/dev/zero)` in an assistant message otherwise hangs the
//     GUI thread on an unbounded synchronous read.
//
// Everything here fails closed: an unparseable URL, an unreadable root, a path
// that cannot be canonicalised — all refuse rather than pass.
namespace agentkate
{
// The schemes a model-authored link may open with no further ceremony. Keep in
// step with RichTextView's preview policy — both call this.
bool isSafeExternalScheme(const QUrl &url);

// Open a link that came out of model output. Whitelisted schemes go straight to
// the OS handler; everything else (file://, scheme-less paths, smb://, any
// installed custom handler) asks the human first, showing the real target.
// Refuses silently when there is nothing valid to open.
void openModelLink(QWidget *parent, const QUrl &url);

// Roots that rendered content may load images from: the attachment store (both
// the current and the legacy location) plus whatever project directories panels
// have registered. Directories are canonicalised; an unresolvable one is
// dropped rather than kept as a prefix that might match too much.
void allowMediaRoot(const QString &dir);
QStringList allowedMediaRoots();

// The largest image resource that may be read into a document. A cap is what
// makes the read bounded even when the path IS allowed.
constexpr qint64 kMaxResourceBytes = 8 * 1024 * 1024;

// WHAT A REFUSAL LOOKS LIKE, and why it is not "no bytes".
//
// Two INDEPENDENT fallbacks re-open the path behind a resource provider that
// hands nothing back, so refusing by returning nothing refuses nothing:
//   1. QTextDocument::loadResource() does its own QFile read when the provider
//      returns a NULL QVariant. That is why refusals are empty-BUT-NOT-null.
//   2. QTextImageHandler (the thing that actually draws an <img>) calls
//      QImage(name) on the markup's own string whenever the resource did not
//      yield a DECODABLE image — whatever the provider returned. Probed on
//      Qt 6.11: a guard returning an empty QByteArray still rendered a 40x40
//      PNG from outside every root, in a QTextBrowser and in a bare document.
// So a refused ImageResource comes back as real, decodable bytes that show
// nothing: a 1x1 fully transparent PNG. The handler is satisfied, and the path
// in the markup is never touched. Other resource types keep the
// empty-but-not-null QByteArray, which is enough for fallback 1.
QByteArray blockedImageBytes();

// Resolve one document resource under the allowlist. Returns the file's bytes
// when it is allowed, and a REFUSAL (see blockedImageBytes above) for
// everything else: remote schemes, non-regular files (devices, FIFOs), paths
// outside every root, files over the cap, unreadable files. An invalid QVariant
// means "not ours" — data:/qrc: only, which the caller's base implementation
// handles. `roots` are extra roots on top of allowedMediaRoots() (e.g. the
// directory of the document being previewed). `type` is the QTextDocument
// ResourceType the caller was asked for; it only picks the shape of a refusal.
// Exposed for the regression test.
QVariant loadGuardedResource(const QUrl &name, const QStringList &roots,
                             int type = QTextDocument::ImageResource);

// TailRead is the outcome of one bounded step of "follow a file that another
// process is still appending to".
struct TailRead {
    QByteArray bytes;       // newly read bytes; never more than the caller's cap
    bool restarted = false; // the file shrank/rotated — drop any parse state
    bool gap = false;       // the file outran the cap — `bytes` start mid-file
};

// Read the next chunk of an append-only file, starting at `offset` (updated in
// place to the new read position).
//
// The reason this is here and not an inline readAll(): the files a live view
// tails are written by AGENTS, so their size is attacker-influenced. readAll()
// on a sub-agent transcript that ran away (or on a path pointed at something
// enormous) allocates the whole thing in the GUI process. This bounds every
// step instead:
//   * only REGULAR files are read — a FIFO or device node would otherwise block
//     the GUI thread on a read that never returns;
//   * at most `maxBytes` per call, so one poll cannot outgrow memory;
//   * a file that grew by more than `maxBytes` since the last call is SKIPPED
//     FORWARD to its last `maxBytes` and flagged `gap`, so following a fast
//     writer stays O(cap) instead of falling permanently behind;
//   * a file that shrank is flagged `restarted` with the offset rewound to 0.
// Anything unreadable returns empty bytes and leaves `offset` untouched.
TailRead readBoundedTail(const QString &path, qint64 &offset, qint64 maxBytes);

// A QTextBrowser that renders model-authored (or otherwise untrusted) rich text.
// It routes every resource through loadGuardedResource, so the base class never
// gets to do its unbounded QFile read.
class GuardedTextBrowser : public QTextBrowser
{
    Q_OBJECT
public:
    explicit GuardedTextBrowser(QWidget *parent = nullptr);

    // Add a root this browser may load images from, on top of the global ones.
    void addLocalRoot(const QString &dir);

protected:
    QVariant loadResource(int type, const QUrl &name) override;

private:
    QStringList m_roots;
};

// The same guard for a document that is NOT inside a widget: a QTextDocument
// laid out and painted by hand (an item delegate drawing rows).
//
// A widget-less document is not safer than a browser — it is the same loader
// with no owner to intercept it. `![x](/home/you/.ssh/id_rsa.png)` in an
// assistant message reaches QTextImageHandler either way, and a delegate's
// documents are built from exactly that markdown. Anything that renders
// model-authored rich text uses this or GuardedTextBrowser; nothing renders it
// in a bare QTextDocument.
class GuardedTextDocument : public QTextDocument
{
    Q_OBJECT
public:
    explicit GuardedTextDocument(QObject *parent = nullptr);

    // Add a root this document may load images from, on top of the global ones.
    void addLocalRoot(const QString &dir);

protected:
    QVariant loadResource(int type, const QUrl &name) override;

private:
    QStringList m_roots;
};
} // namespace agentkate
