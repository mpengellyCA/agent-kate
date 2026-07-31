// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "SubAgentTranscriptDialog.h"

#include "AgentChatHelpers.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QFile>
#include <QFileInfo>
#include <QFileSystemWatcher>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QPushButton>
#include <QScrollBar>
#include <QTextBrowser>
#include <QTimer>
#include <QVBoxLayout>

namespace {

// Cap each tool result so a giant dump doesn't dominate the conversation; the
// full text remains in the on-disk transcript.
constexpr int kResultClip = 800;
// Poll cadence — a backstop for the file watcher while the transcript grows.
constexpr int kPollMs = 1200;

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
            if (assistant) {
                html += QStringLiteral(
                            "<div style=\"margin:10px 0 2px\"><b style=\"color:%1\">"
                            "Agent</b></div><div style=\"margin:0 0 8px\">%2</div>")
                            .arg(c.accent.name(), agentkate::markdownToHtml(text));
            } else {
                // A user block is either the launch prompt or an echoed tool
                // result narration; the core also synthesizes "Attached file …"
                // text which isn't part of the conversation.
                if (text.startsWith(QLatin1String("Attached file `"))) {
                    continue;
                }
                html += QStringLiteral(
                            "<div style=\"margin:10px 0 2px\"><b style=\"color:%1\">"
                            "Prompt</b></div><div style=\"margin:0 0 8px\">%2</div>")
                            .arg(c.neutral.name(), agentkate::markdownToHtml(text));
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

    m_browser = new QTextBrowser(this);
    m_browser->setOpenExternalLinks(false);

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

void SubAgentTranscriptDialog::pullNew()
{
    QFile f(m_path);
    if (!f.open(QIODevice::ReadOnly)) {
        return;
    }
    // Some file watchers drop the path after a change; re-arm it.
    if (m_watcher && !m_watcher->files().contains(m_path)) {
        m_watcher->addPath(m_path);
    }

    const qint64 size = f.size();
    if (size < m_offset) {
        // Truncated / rewritten — start over so we don't render garbage.
        m_offset = 0;
        m_partial.clear();
        m_browser->clear();
    }
    if (size == m_offset && m_partial.isEmpty()) {
        return; // nothing new
    }

    f.seek(m_offset);
    QByteArray data = m_partial + f.readAll();
    m_offset = f.pos();
    f.close();

    const AkColors &c = ThemeManager::palette();
    QString html;
    int start = 0;
    for (int i = 0; i < data.size(); ++i) {
        if (data.at(i) == '\n') {
            html += renderLine(data.mid(start, i - start), c);
            start = i + 1;
        }
    }
    // Bytes past the last newline are an incomplete line — hold them for next time.
    m_partial = data.mid(start);

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
    sb->setValue(atBottom ? sb->maximum() : prev);
}
