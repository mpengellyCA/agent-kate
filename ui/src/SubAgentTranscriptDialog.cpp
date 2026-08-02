// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "SubAgentTranscriptDialog.h"

#include "AgentChatHelpers.h"
#include "SafeContent.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QFileInfo>
#include <QFileSystemWatcher>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QPushButton>
#include <QScrollBar>
#include <QTextBrowser>
#include <QTextCursor>
#include <QTextDocument>
#include <QTimer>
#include <QVBoxLayout>

namespace {

// Cap each tool result so a giant dump doesn't dominate the conversation; the
// full text remains in the on-disk transcript.
constexpr int kResultClip = 800;
// Poll cadence — a backstop for the file watcher while the transcript grows.
// Nothing tells this dialog when its sub-agent finishes (it is handed a path,
// not a job), so idleness stands in for "finished": every pull that finds no
// new bytes doubles the interval up to the cap, and any new bytes snap it back
// to the base cadence.
constexpr int kPollMs = 1200;
constexpr int kPollMaxMs = 15000;
// A text block past this size is a dump, not prose: it renders as escaped plain
// text. markdownToHtml is a full md4c parse + QTextDocument + toHtml round trip
// on the GUI thread, and the block's length is the sub-agent's choice — escaping
// keeps the per-pull cost a plain O(n) copy instead.
constexpr int kMarkdownMaxChars = 16 * 1024;

// --- Bounds. This dialog tails a file a SUB-AGENT writes, so its size, its line
// lengths and its growth rate are all attacker-influenced (repo content shapes
// what the agent does, and a runaway loop needs no attacker at all). Each of the
// four ways that could exhaust the GUI process gets its own cap: one read, one
// held fragment, and the document along both of its axes.
//
// How much of the file one pull may read. The first pull starts at offset 0, so
// without this a 2 GB transcript is a 2 GB allocation in the GUI process before
// a single line is rendered.
constexpr qint64 kMaxTailBytes = 1 * 1024 * 1024;
// The longest incomplete trailing line held between pulls. A file with no
// newlines is otherwise an unbounded accumulator: every pull appends its window
// to m_partial and none of it ever renders.
constexpr int kMaxPartialBytes = 256 * 1024;
// The document is a live tail, not an archive: old blocks are dropped off the
// front so a sub-agent that keeps writing cannot grow the QTextDocument without
// bound. (Probed: Qt trims from the front safely with tables/lists/pre in the
// tail.)
//
// TWO caps, because a block count bounds the wrong thing on its own. Blocks are
// PARAGRAPHS, and nothing bounds how long one paragraph is: 4000 blocks of one
// 900 KB message each is 3.6 GB of QTextDocument that never trips
// maximumBlockCount, and the sub-agent picks both how many messages it writes
// and how long each one is. So: MANY-and-small is bounded by the block count,
// FEW-and-huge by the character count. Both bind at roughly the same scale for
// ordinary output (4000 blocks of ~50 characters), and trimming either one
// drops the OLDEST blocks — this is a live tail, and the file on disk stays the
// complete record.
constexpr int kMaxDocBlocks = 4000;
constexpr int kMaxDocChars = 200000;

// The role blocks of one message, appended to `html`.
void renderBlocks(const QJsonValue &content, const QString &role, QString &html,
                  const AkColors &c)
{
    QJsonArray blocks;
    if (content.isArray()) {
        blocks = content.toArray();
    } else if (content.isString()) {
        QJsonObject t;
        t.insert(QStringLiteral("type"), QStringLiteral("text"));
        t.insert(QStringLiteral("text"), content.toString());
        blocks.append(t);
    }

    const bool assistant = role == QLatin1String("assistant");
    for (const QJsonValue &bv : blocks) {
        const QJsonObject b = bv.toObject();
        const QString bt = b.value(QStringLiteral("type")).toString();

        if (bt == QLatin1String("text")) {
            const QString text = b.value(QStringLiteral("text")).toString();
            if (text.isEmpty()) {
                continue;
            }
            // A user block is either the launch prompt or an echoed tool result
            // narration; the core also synthesizes "Attached file …" text which
            // isn't part of the conversation.
            if (!assistant && text.startsWith(QLatin1String("Attached file `"))) {
                continue;
            }
            const QString body = text.size() > kMarkdownMaxChars
                ? QStringLiteral("<div style=\"white-space:pre-wrap\">%1</div>")
                      .arg(text.toHtmlEscaped())
                : agentkate::markdownToHtml(text);
            if (assistant) {
                html += QStringLiteral(
                            "<div style=\"margin:10px 0 2px\"><b style=\"color:%1\">"
                            "Agent</b></div><div style=\"margin:0 0 8px\">%2</div>")
                            .arg(c.accent.name(), body);
            } else {
                html += QStringLiteral(
                            "<div style=\"margin:10px 0 2px\"><b style=\"color:%1\">"
                            "Prompt</b></div><div style=\"margin:0 0 8px\">%2</div>")
                            .arg(c.neutral.name(), body);
            }

        } else if (bt == QLatin1String("tool_use")) {
            const QString name = b.value(QStringLiteral("name")).toString();
            QString summary =
                agentkate::permSummary(name, b.value(QStringLiteral("input")).toObject())
                    .simplified();
            html += QStringLiteral(
                        "<div style=\"margin:4px 0 4px 12px;color:%1\">&#128295; "
                        "<b>%2</b>%3</div>")
                        .arg(c.info.name(), name.toHtmlEscaped(),
                             summary.isEmpty()
                                 ? QString()
                                 : QStringLiteral(" — %1").arg(summary.toHtmlEscaped()));

        } else if (bt == QLatin1String("tool_result")) {
            QString text =
                agentkate::toolResultText(b.value(QStringLiteral("content"))).trimmed();
            if (text.isEmpty()) {
                continue;
            }
            bool clipped = false;
            if (text.size() > kResultClip) {
                text = text.left(kResultClip);
                clipped = true;
            }
            html += QStringLiteral(
                        "<pre style=\"margin:0 0 8px 24px;color:%1;white-space:"
                        "pre-wrap\">%2%3</pre>")
                        .arg(c.info.name(), text.toHtmlEscaped(),
                             clipped ? QStringLiteral(" …") : QString());

        } else if (bt == QLatin1String("image")) {
            html += QStringLiteral(
                        "<div style=\"margin:4px 0 4px 12px;color:%1\">[image]</div>")
                        .arg(c.info.name());
        }
    }
}

// kimiBlocks translates one line of a kimi subagent's wire log into the same
// Claude-shaped content blocks renderBlocks already speaks (plan 16 P6).
//
// The two CLIs write different files for the same thing. Claude's
// subagents/agent-<id>.jsonl is the transcript shape — {"message":{role,
// content:[…]}} — while kimi's <session-dir>/agents/<id>/wire.jsonl is its
// engine's own wire protocol. Probed on 0.30.0, the parts that carry
// conversation are:
//
//   context.append_message   {"message":{role,content:[…]}}  ← already the shape
//   context.append_loop_event with event.type:
//       content.part  {"part":{"type":"text"|"think","text"/"think":…}}
//       tool.call     {name, args}
//       tool.result   {result:{output}}
//
// Everything else (metadata, config.update, llm.request, usage.record,
// step.begin/end, tools snapshots) is engine bookkeeping with nothing to show.
QJsonArray kimiBlocks(const QJsonObject &o, QString &role)
{
    const QString type = o.value(QStringLiteral("type")).toString();
    if (type == QLatin1String("context.append_message")) {
        return {}; // handled by the caller: it is already the transcript shape
    }
    if (type != QLatin1String("context.append_loop_event")) {
        return {};
    }
    const QJsonObject ev = o.value(QStringLiteral("event")).toObject();
    const QString evType = ev.value(QStringLiteral("type")).toString();
    QJsonArray blocks;
    if (evType == QLatin1String("content.part")) {
        const QJsonObject part = ev.value(QStringLiteral("part")).toObject();
        const QString partType = part.value(QStringLiteral("type")).toString();
        // Thinking is rendered as ordinary agent text, prefixed, rather than
        // dropped: in a subagent's log the reasoning is often the only thing
        // between two tool calls.
        const QString text = partType == QLatin1String("think")
            ? part.value(QStringLiteral("think")).toString()
            : part.value(QStringLiteral("text")).toString();
        if (text.isEmpty()) {
            return {};
        }
        role = QStringLiteral("assistant");
        blocks.append(QJsonObject{
            {QStringLiteral("type"), QStringLiteral("text")},
            {QStringLiteral("text"), partType == QLatin1String("think")
                                         ? QStringLiteral("*(thinking)* ") + text
                                         : text}});
        return blocks;
    }
    if (evType == QLatin1String("tool.call")) {
        role = QStringLiteral("assistant");
        blocks.append(QJsonObject{
            {QStringLiteral("type"), QStringLiteral("tool_use")},
            {QStringLiteral("name"), ev.value(QStringLiteral("name"))},
            {QStringLiteral("input"), ev.value(QStringLiteral("args"))}});
        return blocks;
    }
    if (evType == QLatin1String("tool.result")) {
        const QJsonObject result = ev.value(QStringLiteral("result")).toObject();
        const QJsonValue output = result.value(QStringLiteral("output"));
        if (output.isNull() || output.isUndefined()) {
            return {};
        }
        role = QStringLiteral("user");
        blocks.append(QJsonObject{
            {QStringLiteral("type"), QStringLiteral("tool_result")},
            {QStringLiteral("content"), output}});
        return blocks;
    }
    return {};
}

// Render one JSONL line into a chat HTML fragment (empty for lines that carry no
// renderable message).
QString renderLine(const QByteArray &line, const AkColors &c)
{
    if (line.trimmed().isEmpty()) {
        return QString();
    }
    const QJsonObject o = QJsonDocument::fromJson(line).object();
    const QJsonObject msg = o.value(QStringLiteral("message")).toObject();
    if (!msg.isEmpty()) {
        // Claude's transcript lines, and kimi's context.append_message — the
        // same shape, so one branch serves both.
        QString html;
        renderBlocks(msg.value(QStringLiteral("content")),
                     msg.value(QStringLiteral("role")).toString(), html, c);
        return html;
    }
    QString role;
    const QJsonArray blocks = kimiBlocks(o, role);
    if (blocks.isEmpty()) {
        return QString();
    }
    QString html;
    renderBlocks(blocks, role, html, c);
    return html;
}

} // namespace

SubAgentTranscriptDialog::SubAgentTranscriptDialog(const QString &jsonlPath,
                                                   const QString &label, QWidget *parent)
    : QDialog(parent), m_path(jsonlPath)
{
    setAttribute(Qt::WA_DeleteOnClose);
    const QString name =
        label.isEmpty() ? QFileInfo(jsonlPath).fileName() : label;
    setWindowTitle(i18nc("@title:window", "Sub-agent transcript — %1", name));

    // Guarded: every byte in this dialog is a helper agent's own output, and a
    // bare QTextBrowser resolves an image name to an unbounded synchronous read
    // of any local path — `![x](/dev/zero)` hangs the GUI thread, and a
    // file:///…/private.png renders someone's picture into the transcript
    // (audit F15). Links are handled by the caller's policy, never navigated.
    m_browser = new agentkate::GuardedTextBrowser(this);
    // Bounded document: this view follows a file that grows for as long as the
    // sub-agent runs, so it keeps a tail rather than the whole history (audit
    // F11 class — the same "bound what is READ, not just what is admitted"
    // rule the attachment path now follows). maximumBlockCount is only half of
    // it; trimDocument() enforces the character cap that bounds block SIZE.
    m_browser->document()->setMaximumBlockCount(kMaxDocBlocks);
    m_browser->setOpenExternalLinks(false);
    m_browser->setOpenLinks(false);
    connect(m_browser, &QTextBrowser::anchorClicked, this,
            [this](const QUrl &url) { agentkate::openModelLink(this, url); });

    auto *close = new QPushButton(i18n("Close"), this);
    connect(close, &QPushButton::clicked, this, &QDialog::accept);
    auto *btnRow = new QHBoxLayout;
    btnRow->addStretch(1);
    btnRow->addWidget(close);

    auto *root = new QVBoxLayout(this);
    root->addWidget(m_browser, 1);
    root->addLayout(btnRow);

    const KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("SubAgentTranscriptDialog"));
    resize(cfg.readEntry("size", QSize(760, 640)));

    // Initial fill, then tail the file live as the sub-agent keeps writing.
    pullNew();

    m_watcher = new QFileSystemWatcher(this);
    if (QFileInfo::exists(m_path)) {
        m_watcher->addPath(m_path);
    }
    connect(m_watcher, &QFileSystemWatcher::fileChanged, this,
            &SubAgentTranscriptDialog::pullNew);
    m_poll = new QTimer(this);
    m_poll->setObjectName(QStringLiteral("pollTimer")); // regression-test handle
    m_poll->setInterval(kPollMs);
    connect(m_poll, &QTimer::timeout, this, &SubAgentTranscriptDialog::pullNew);
    m_poll->start();
}

SubAgentTranscriptDialog::~SubAgentTranscriptDialog()
{
    KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("SubAgentTranscriptDialog"));
    cfg.writeEntry("size", size());
}

void SubAgentTranscriptDialog::showEvent(QShowEvent *event)
{
    QDialog::showEvent(event);
    if (m_poll && !m_poll->isActive()) {
        // Back from hidden: catch up on whatever was appended meanwhile, and
        // resume at the base cadence — being shown again is a change signal.
        m_poll->setInterval(kPollMs);
        m_poll->start();
        pullNew();
    }
}

void SubAgentTranscriptDialog::hideEvent(QHideEvent *event)
{
    QDialog::hideEvent(event);
    // Nobody is reading: no cadence at all. The watcher stays armed so the
    // offset keeps up cheaply, and showEvent() pulls whatever it missed.
    if (m_poll) {
        m_poll->stop();
    }
}

void SubAgentTranscriptDialog::pullNew()
{
    // Some file watchers drop the path after a change; re-arm it.
    if (m_watcher && QFileInfo::exists(m_path) && !m_watcher->files().contains(m_path)) {
        m_watcher->addPath(m_path);
    }

    // Bounded read: never more than kMaxTailBytes per pull, regular files only,
    // and a file that outran the cap is skipped forward rather than swallowed.
    const agentkate::TailRead tail =
        agentkate::readBoundedTail(m_path, m_offset, kMaxTailBytes);
    // Idle backoff (null during the constructor's initial fill): a finished
    // sub-agent stops appending, so consecutive empty pulls stretch the cadence
    // 1.2 s → 2.4 → 4.8 → … → 15 s; any new bytes (or a rewrite) snap it back.
    if (m_poll) {
        if (!tail.bytes.isEmpty() || tail.restarted) {
            m_poll->setInterval(kPollMs);
        } else {
            m_poll->setInterval(qMin(m_poll->interval() * 2, kPollMaxMs));
        }
    }
    if (tail.restarted) {
        // Truncated / rewritten — start over so we don't render garbage.
        m_partial.clear();
        m_resync = false;
        m_browser->clear();
    }
    if (tail.gap) {
        // We jumped over bytes, so whatever we hold is no longer contiguous and
        // the new window almost certainly begins mid-line.
        m_partial.clear();
        m_resync = true;
        m_skipped = true;
    }
    if (tail.bytes.isEmpty() && m_partial.isEmpty()) {
        return; // nothing new
    }

    QByteArray data = m_partial + tail.bytes;
    m_partial.clear();
    if (m_resync) {
        // Drop the fragment up to the first line boundary: a partial JSON line
        // is not renderable, and guessing at one is how garbage gets rendered.
        const int nl = data.indexOf('\n');
        if (nl < 0) {
            return; // still inside the over-long line; keep discarding
        }
        data = data.mid(nl + 1);
        m_resync = false;
    }

    const AkColors &c = ThemeManager::palette();
    QString html;
    if (m_skipped) {
        m_skipped = false;
        html += QStringLiteral("<div style=\"margin:8px 0;color:%1\"><i>%2</i></div>")
                    .arg(c.neutral.name(),
                         i18n("… earlier output skipped (the sub-agent wrote faster "
                              "than this view reads)"));
    }
    int start = 0;
    for (int i = 0; i < data.size(); ++i) {
        if (data.at(i) == '\n') {
            html += renderLine(data.mid(start, i - start), c);
            start = i + 1;
        }
    }
    // Bytes past the last newline are an incomplete line — hold them for next
    // time, unless the "line" has grown past anything a transcript record could
    // be, in which case hold nothing and resync at the next newline.
    if (data.size() - start > kMaxPartialBytes) {
        m_resync = true;
        m_skipped = true;
    } else {
        m_partial = data.mid(start);
    }

    if (html.isEmpty()) {
        return;
    }

    // Stay pinned to the bottom when the reader is already there (live-follow);
    // otherwise leave their scroll position undisturbed.
    QScrollBar *sb = m_browser->verticalScrollBar();
    const bool atBottom = sb->value() >= sb->maximum() - 4;
    const int prev = sb->value();
    // append() starts a fresh paragraph for the fragment, preserving the chat's
    // block structure. insertHtml() at the end cursor merges block-level markup
    // into the previous paragraph, running successive events together.
    m_browser->append(html);
    trimDocument();
    sb->setValue(atBottom ? sb->maximum() : prev);
}

void SubAgentTranscriptDialog::trimDocument()
{
    QTextDocument *doc = m_browser->document();
    if (doc->characterCount() <= kMaxDocChars) {
        return; // the common case: nothing to do, and no cursor allocated
    }
    QTextCursor cur(doc);
    cur.beginEditBlock();
    // Delete whole blocks off the front — the oldest output — until the tail
    // fits. Selecting to the start of the NEXT block takes the block separator
    // with it, so the document does not accumulate empty paragraphs.
    while (doc->characterCount() > kMaxDocChars && doc->blockCount() > 1) {
        cur.movePosition(QTextCursor::Start);
        if (!cur.movePosition(QTextCursor::NextBlock, QTextCursor::KeepAnchor)) {
            break; // cannot advance: stop rather than spin
        }
        cur.removeSelectedText();
    }
    cur.endEditBlock();
    if (doc->characterCount() > kMaxDocChars) {
        // One single block is over the whole budget — a sub-agent that emitted
        // a megabytes-long line. There is nothing left to drop off the front, so
        // drop the document: the file on disk is still the record.
        m_browser->clear();
        // The held fragment belongs to the text we just threw away, and it
        // starts mid-line, so resync at the next newline rather than parse it.
        m_partial.clear();
        m_resync = true;
        m_skipped = true; // the next pull tells the reader output was dropped
    }
}
