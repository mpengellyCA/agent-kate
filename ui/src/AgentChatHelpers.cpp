// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AgentChatHelpers.h"
#include "MarkdownUtil.h"
#include "state/HarnessTraits.h"

#include <QComboBox>
#include <QDialog>
#include <QDialogButtonBox>
#include <QFont>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QJsonValue>
#include <QLabel>
#include <QObject>
#include <QPushButton>
#include <QTextDocument>
#include <QVBoxLayout>

namespace
{
// The arena's own MCP servers: Cooperation (agent-to-agent coordination and
// orchestration) and Cowork (the KDE desktop). Their tool rows are the most
// interesting ones in a transcript, so they get real summaries instead of raw
// JSON. Argument vocabularies are the catalogues' own — snake_case for
// Cooperation, camelCase for Cowork (core/cmd/akcore/mcp.go, mcp_cowork.go).
const QLatin1String kCoopPrefix("mcp__cooperation__");
const QLatin1String kCoworkPrefix("mcp__cowork__");

// firstLine reduces a multi-line argument to its opening line, elided, so one
// tool row stays one line however long the message was.
QString firstLine(const QString &s)
{
    const QString flat = s.section(QLatin1Char('\n'), 0, 0).simplified();
    return flat.size() > 160 ? flat.left(159) + QStringLiteral("…") : flat;
}

// mcpSummary digests one Cooperation/Cowork tool call. An empty return means
// "no digest for this tool" and falls through to the generic handling.
QString mcpSummary(const QString &verb, const QJsonObject &input)
{
    const auto str = [&input](const char *key) {
        return input.value(QLatin1String(key)).toString();
    };
    // The tools name the target "thread_id"; the core's cross-subtree approval
    // prompt for the same verbs names it "targetThreadId" — both read alike.
    const auto target = [&str] {
        const QString id = str("thread_id");
        return id.isEmpty() ? str("targetThreadId") : id;
    };
    // --- Cooperation ---------------------------------------------------
    if (verb == QLatin1String("post_note")) {
        return firstLine(str("text"));
    }
    if (verb == QLatin1String("claim_file") || verb == QLatin1String("release_file")) {
        return str("path");
    }
    if (verb == QLatin1String("request_review")) {
        return firstLine(str("summary"));
    }
    if (verb == QLatin1String("launch_agent")) {
        QString engine = str("backend");
        const QString model = str("model");
        if (!model.isEmpty()) {
            engine = engine.isEmpty() ? model : engine + QLatin1Char('/') + model;
        }
        if (engine.isEmpty()) {
            engine = QObject::tr("same engine");
        }
        const QString title = str("title");
        return title.isEmpty() ? engine : engine + QStringLiteral(": ") + title;
    }
    if (verb == QLatin1String("send_agent")) {
        const QString msg = firstLine(str("message"));
        return msg.isEmpty() ? target() : target() + QStringLiteral(": ") + msg;
    }
    if (verb == QLatin1String("wait_agent") || verb == QLatin1String("close_agent")
        || verb == QLatin1String("discard_agent")) {
        return target();
    }
    if (verb == QLatin1String("list_agents")) {
        return input.value(QStringLiteral("all_workspaces")).toBool()
                   ? QObject::tr("every workspace")
                   : QObject::tr("this workspace");
    }
    if (verb == QLatin1String("read_notes")) {
        return QObject::tr("the cooperation board");
    }
    if (verb == QLatin1String("get_presence")) {
        return QObject::tr("who is working where");
    }
    if (verb == QLatin1String("list_open_files")) {
        return QObject::tr("open files");
    }
    if (verb == QLatin1String("whoami")) {
        return QObject::tr("this thread's identity");
    }
    // --- Cowork desktop --------------------------------------------------
    if (verb == QLatin1String("desktop_activate_element")
        || verb == QLatin1String("desktop_click_element")
        || verb == QLatin1String("desktop_set_text")) {
        // Never the text being typed — it may be a password.
        return str("elementId");
    }
    if (verb == QLatin1String("desktop_list_elements")
        || verb == QLatin1String("desktop_read_text")) {
        const QString win = str("targetWindowId");
        return win.isEmpty() ? QObject::tr("the active window") : win;
    }
    if (verb == QLatin1String("desktop_open_browser")) {
        const QString name = str("name");
        return name.isEmpty() ? QObject::tr("the default browser") : name;
    }
    if (verb == QLatin1String("desktop_click") || verb == QLatin1String("desktop_move_pointer")) {
        return QStringLiteral("%1, %2")
            .arg(input.value(QStringLiteral("x")).toInt())
            .arg(input.value(QStringLiteral("y")).toInt());
    }
    if (verb == QLatin1String("desktop_inject_input")
        || verb == QLatin1String("desktop_play_input")) {
        return QObject::tr("%n event(s)", nullptr,
                           input.value(QStringLiteral("events")).toArray().size());
    }
    return QString();
}
} // namespace

namespace agentkate
{
QString markdownToHtml(const QString &md)
{
    QTextDocument doc;
    doc.setMarkdown(agentkate::neutralizeMarkdownRawHtml(md),
                    QTextDocument::MarkdownDialectGitHub);
    const QString html = doc.toHtml();
    const int bodyOpen = html.indexOf(QLatin1String("<body"));
    const int bodyStart = bodyOpen >= 0 ? html.indexOf(QLatin1Char('>'), bodyOpen) + 1 : -1;
    const int bodyEnd = html.lastIndexOf(QLatin1String("</body>"));
    if (bodyStart > 0 && bodyEnd > bodyStart) {
        return html.mid(bodyStart, bodyEnd - bodyStart);
    }
    return html;
}

QString permSummary(const QString &toolName, const QJsonObject &input)
{
    if (toolName.startsWith(kCoopPrefix) || toolName.startsWith(kCoworkPrefix)) {
        // "mcp__<server>__<verb>" — summarize by the verb; anything the digest
        // does not cover falls through to the generic handling below.
        const QString summary =
            mcpSummary(toolName.section(QLatin1String("__"), 2), input);
        if (!summary.isEmpty()) {
            return summary;
        }
    }
    if (toolName == QLatin1String("Bash")) {
        return input.value(QStringLiteral("command")).toString();
    }
    if (toolName == QLatin1String("WebFetch")) {
        return input.value(QStringLiteral("url")).toString();
    }
    if (toolName == QLatin1String("WebSearch")) {
        return input.value(QStringLiteral("query")).toString();
    }
    for (const QString &key : {QStringLiteral("file_path"), QStringLiteral("path"),
                               QStringLiteral("pattern"), QStringLiteral("description")}) {
        const QString v = input.value(key).toString();
        if (!v.isEmpty()) {
            return v;
        }
    }
    return QString::fromUtf8(QJsonDocument(input).toJson(QJsonDocument::Compact));
}

QString toolResultText(const QJsonValue &content)
{
    if (content.isString()) {
        return content.toString();
    }
    QStringList parts;
    for (const QJsonValue &v : content.toArray()) {
        const QJsonObject o = v.toObject();
        if (o.value(QStringLiteral("type")).toString() == QLatin1String("text")) {
            parts << o.value(QStringLiteral("text")).toString();
        }
    }
    return parts.join(QLatin1Char('\n'));
}

QList<QPair<QString, QByteArray>> toolResultImages(const QJsonValue &content)
{
    QList<QPair<QString, QByteArray>> out;
    for (const QJsonValue &v : content.toArray()) {
        const QJsonObject o = v.toObject();
        if (o.value(QStringLiteral("type")).toString() != QLatin1String("image")) {
            continue;
        }
        const QJsonObject src = o.value(QStringLiteral("source")).toObject();
        if (src.value(QStringLiteral("type")).toString() != QLatin1String("base64")) {
            continue;
        }
        const QByteArray data = QByteArray::fromBase64(
            src.value(QStringLiteral("data")).toString().toLatin1());
        if (!data.isEmpty()) {
            out.append({src.value(QStringLiteral("media_type")).toString(), data});
        }
    }
    return out;
}

QString activityFor(const QString &tool)
{
    if (tool == QLatin1String("Bash")) {
        return QStringLiteral("Agent Kate is running commands…");
    }
    if (tool == QLatin1String("Edit") || tool == QLatin1String("Write")
        || tool == QLatin1String("MultiEdit") || tool == QLatin1String("NotebookEdit")) {
        return QStringLiteral("Agent Kate is writing code…");
    }
    if (tool == QLatin1String("Read") || tool == QLatin1String("Grep")
        || tool == QLatin1String("Glob")) {
        return QStringLiteral("Agent Kate is combing through the code…");
    }
    if (tool == QLatin1String("WebFetch") || tool == QLatin1String("WebSearch")) {
        return QStringLiteral("Agent Kate is scouring the web…");
    }
    if (tool == QLatin1String("Task") || tool == QLatin1String("TodoWrite")) {
        return QStringLiteral("Agent Kate is mapping out the work…");
    }
    if (tool.startsWith(kCoopPrefix)) {
        // Orchestration verbs read differently from board-and-locks chatter.
        const QString verb = tool.section(QLatin1String("__"), 2);
        if (verb.endsWith(QLatin1String("_agent")) || verb == QLatin1String("list_agents")) {
            return QStringLiteral("Agent Kate is directing its team…");
        }
        return QStringLiteral("Agent Kate is coordinating with the team…");
    }
    if (tool.startsWith(kCoworkPrefix)) {
        return QStringLiteral("Agent Kate is working at the desktop…");
    }
    if (tool.startsWith(QLatin1String("mcp__"))) {
        return QStringLiteral("Agent Kate is coordinating with the team…");
    }
    return QStringLiteral("Agent Kate is working with %1…").arg(tool);
}

QString resumeStrategyModel(const QString &strategy)
{
    if (strategy == QLatin1String("resume_opus_cold")) {
        return QStringLiteral("opus");
    }
    if (strategy == QLatin1String("resume_sonnet_cold")) {
        return QStringLiteral("sonnet");
    }
    if (strategy == QLatin1String("resume_haiku_cold")) {
        return QStringLiteral("haiku");
    }
    if (strategy == QLatin1String("resume_local")) {
        return QStringLiteral("local");
    }
    return QString();
}

QString askRecoveryModel(QWidget *parent)
{
    QDialog dlg(parent);
    dlg.setWindowTitle(QObject::tr("Resume — choose compactor"));

    auto *layout = new QVBoxLayout(&dlg);
    auto *msg = new QLabel(QObject::tr(
        "This thread has no current compacted summary, so resuming would "
        "replay its full transcript. Choose which model should produce a "
        "summary now:"));
    msg->setWordWrap(true);
    layout->addWidget(msg);

    QString choice;
    auto *btnLayout = new QHBoxLayout;
    auto add = [&](const QString &label, const QString &result, bool recommended) {
        auto *btn = new QPushButton(label, &dlg);
        if (recommended) {
            btn->setDefault(true);
            QFont f = btn->font();
            f.setBold(true);
            btn->setFont(f);
        }
        QObject::connect(btn, &QPushButton::clicked, &dlg, [&dlg, &choice, result] {
            choice = result;
            dlg.accept();
        });
        btnLayout->addWidget(btn);
    };
    add(QObject::tr("Opus"), QStringLiteral("opus"), false);
    add(QObject::tr("Sonnet (recommended)"), QStringLiteral("sonnet"), true);
    add(QObject::tr("Haiku"), QStringLiteral("haiku"), false);
    add(QObject::tr("Local"), QStringLiteral("local"), false);
    layout->addLayout(btnLayout);

    auto *bb = new QDialogButtonBox(QDialogButtonBox::Cancel, &dlg);
    QObject::connect(bb, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    layout->addWidget(bb);

    if (dlg.exec() == QDialog::Accepted) {
        return choice;
    }
    return QString();
}

bool modelAvailable(const QString &harnessId, const QString &providerId,
                    const QString &model)
{
    if (model.isEmpty()) {
        return true; // "" = the provider's / CLI's own default, always valid
    }
    const auto choices = HarnessRegistry::self()->modelChoices(harnessId, providerId);
    if (choices.all.isEmpty() && choices.recommended.isEmpty()) {
        return true; // nothing discovered yet — never nag from an empty catalogue
    }
    for (const QStringList &list : {choices.recommended, choices.all}) {
        for (const QString &entry : list) {
            if (entry.section(QLatin1Char('|'), 0, 0) == model) {
                return true;
            }
        }
    }
    return false;
}

QString askReplacementModel(QWidget *parent, const QString &harnessId,
                            const QString &providerId, const QString &oldModel)
{
    QDialog dlg(parent);
    dlg.setWindowTitle(QObject::tr("Model no longer available"));

    auto *layout = new QVBoxLayout(&dlg);
    auto *msg = new QLabel(QObject::tr(
        "The model \"%1\" this chat used is no longer offered by its provider. "
        "Choose a replacement to continue on:")
                               .arg(oldModel.isEmpty() ? QObject::tr("(default)") : oldModel));
    msg->setWordWrap(true);
    layout->addWidget(msg);

    auto *combo = new QComboBox(&dlg);
    const auto choices = HarnessRegistry::self()->modelChoices(harnessId, providerId);
    const auto addEntries = [combo](const QStringList &entries) {
        for (const QString &entry : entries) {
            const QString value = entry.section(QLatin1Char('|'), 0, 0);
            const QString name = entry.section(QLatin1Char('|'), 1);
            if (!value.isEmpty() && combo->findData(value) < 0) {
                combo->addItem(name.isEmpty() ? value : name, value);
            }
        }
    };
    addEntries(choices.recommended);
    if (!choices.recommended.isEmpty() && !choices.all.isEmpty()) {
        combo->insertSeparator(combo->count());
    }
    addEntries(choices.all);
    layout->addWidget(combo);

    auto *bb = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, &dlg);
    QObject::connect(bb, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    QObject::connect(bb, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    layout->addWidget(bb);

    if (dlg.exec() == QDialog::Accepted && combo->currentIndex() >= 0) {
        return combo->currentData().toString();
    }
    return QString();
}
} // namespace agentkate
