#include "AgentPanel.h"
#include "ipc/CoreClient.h"

#include <QAbstractButton>
#include <QCheckBox>
#include <QComboBox>
#include <QDir>
#include <QFile>
#include <QFileDialog>
#include <QFileInfo>
#include <QFrame>
#include <QHash>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonDocument>
#include <QKeySequence>
#include <QLabel>
#include <QLayout>
#include <QPalette>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QRadioButton>
#include <QScrollBar>
#include <QShortcut>
#include <QTextDocument>
#include <QTextEdit>
#include <QVBoxLayout>

namespace {
// Transcript styling, derived from the active palette so accent colours stay
// legible on whatever KDE colour scheme is in use. Body (.asst) text carries
// no colour override and inherits the widget's palette text colour.
QString transcriptCss(const QPalette &pal)
{
    const bool dark = pal.color(QPalette::Base).lightness() < 128;
    const QString you   = dark ? QStringLiteral("#7cb7ff") : QStringLiteral("#1a5fb4");
    const QString tool  = dark ? QStringLiteral("#79b8ff") : QStringLiteral("#1c71d8");
    const QString muted = dark ? QStringLiteral("#b4b2bc") : QStringLiteral("#5e5c64");
    const QString ok    = dark ? QStringLiteral("#5fd38a") : QStringLiteral("#1a7f37");
    const QString err   = dark ? QStringLiteral("#ff8a80") : QStringLiteral("#c01c28");
    const QString dim   = dark ? QStringLiteral("#8c8c94") : QStringLiteral("#8a8a8a");
    return QStringLiteral("p { margin: 6px 2px; }"
                          ".you  { color: %1; }"
                          ".tool { color: %2; font-family: monospace; }"
                          ".res  { color: %3; font-family: monospace; font-size: small; }"
                          ".sys  { color: %3; }"
                          ".ok   { color: %4; font-weight: bold; }"
                          ".err  { color: %5; font-weight: bold; }"
                          ".dim  { color: %6; font-size: small; }")
        .arg(you, tool, muted, ok, err, dim);
}

// markdownToHtml renders an assistant message (Markdown) to an HTML fragment
// that inserts cleanly into the transcript. Default-coloured text carries no
// explicit colour, so it inherits the transcript's palette text colour.
QString markdownToHtml(const QString &md)
{
    QTextDocument doc;
    doc.setMarkdown(md, QTextDocument::MarkdownDialectGitHub);
    const QString html = doc.toHtml();
    const int bodyOpen = html.indexOf(QLatin1String("<body"));
    const int bodyStart = bodyOpen >= 0 ? html.indexOf(QLatin1Char('>'), bodyOpen) + 1 : -1;
    const int bodyEnd = html.lastIndexOf(QLatin1String("</body>"));
    if (bodyStart > 0 && bodyEnd > bodyStart) {
        return html.mid(bodyStart, bodyEnd - bodyStart);
    }
    return html;
}

// permSummary renders a gated tool request as a short, human-readable line.
QString permSummary(const QString &toolName, const QJsonObject &input)
{
    if (toolName == QLatin1String("Bash")) {
        return input.value(QStringLiteral("command")).toString();
    }
    if (toolName == QLatin1String("WebFetch")) {
        return input.value(QStringLiteral("url")).toString();
    }
    if (toolName == QLatin1String("WebSearch")) {
        return input.value(QStringLiteral("query")).toString();
    }
    return QString::fromUtf8(QJsonDocument(input).toJson(QJsonDocument::Compact));
}

// clearLayout removes and deletes every item (and widget) in a layout.
void clearLayout(QLayout *layout)
{
    while (QLayoutItem *item = layout->takeAt(0)) {
        if (QWidget *w = item->widget()) {
            w->deleteLater();
        }
        delete item;
    }
}
} // namespace

AgentPanel::AgentPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    m_header = new QLabel(this);
    m_header->setTextFormat(Qt::RichText);
    m_header->setStyleSheet(
        QStringLiteral("padding: 9px 12px; border-bottom: 1px solid palette(mid);"));

    m_transcript = new QTextEdit(this);
    m_transcript->setReadOnly(true);
    m_transcript->setFrameShape(QFrame::NoFrame);
    m_transcript->document()->setDefaultStyleSheet(transcriptCss(palette()));

    // --- per-tool approval banner (hidden until a request arrives) ---------
    m_permBar = new QFrame(this);
    m_permBar->setObjectName(QStringLiteral("permBar"));
    m_permBar->setStyleSheet(QStringLiteral(
        "QFrame#permBar { border: 1px solid palette(highlight); border-radius: 6px; }"));
    m_permBar->setVisible(false);
    m_permLabel = new QLabel(m_permBar);
    m_permLabel->setTextFormat(Qt::RichText);
    m_permLabel->setWordWrap(true);
    m_permDeny = new QPushButton(QStringLiteral("Deny"), m_permBar);
    m_permDeny->setCursor(Qt::PointingHandCursor);
    m_permAllow = new QPushButton(QStringLiteral("Approve"), m_permBar);
    m_permAllow->setCursor(Qt::PointingHandCursor);
    auto *permLayout = new QHBoxLayout(m_permBar);
    permLayout->setContentsMargins(10, 8, 10, 8);
    permLayout->addWidget(m_permLabel, 1);
    permLayout->addWidget(m_permDeny);
    permLayout->addWidget(m_permAllow);

    // --- AskUserQuestion form (built dynamically, hidden until needed) -----
    m_questionBox = new QFrame(this);
    m_questionBox->setObjectName(QStringLiteral("questionBox"));
    m_questionBox->setStyleSheet(QStringLiteral(
        "QFrame#questionBox { border: 1px solid palette(highlight); border-radius: 6px; }"));
    m_questionBox->setVisible(false);
    m_questionLayout = new QVBoxLayout(m_questionBox);
    m_questionLayout->setContentsMargins(10, 10, 10, 10);
    m_questionLayout->setSpacing(4);

    m_input = new QPlainTextEdit(this);
    m_input->setPlaceholderText(
        QStringLiteral("Describe a task for the agent…   (Ctrl+Enter to send)"));
    m_input->setFixedHeight(94);

    m_modeCombo = new QComboBox(this);
    m_modeCombo->addItem(QStringLiteral("Accept edits"), QStringLiteral("acceptEdits"));
    m_modeCombo->addItem(QStringLiteral("Approve each tool"), QStringLiteral("default"));
    m_modeCombo->addItem(QStringLiteral("Auto"), QStringLiteral("auto"));
    m_modeCombo->addItem(QStringLiteral("Unsafe (bypass)"), QStringLiteral("bypassPermissions"));
    m_modeCombo->setToolTip(QStringLiteral("Permission mode for this agent (fixed once it starts)"));

    // Attachment chip bar — hidden until files are attached.
    m_attachBar = new QWidget(this);
    m_attachLayout = new QHBoxLayout(m_attachBar);
    m_attachLayout->setContentsMargins(0, 0, 0, 0);
    m_attachLayout->setSpacing(6);
    m_attachLayout->addStretch(1);
    m_attachBar->setVisible(false);

    m_attachBtn = new QPushButton(QStringLiteral("Attach…"), this);
    m_attachBtn->setCursor(Qt::PointingHandCursor);
    m_diffBtn = new QPushButton(QStringLiteral("Changes"), this);
    m_diffBtn->setCursor(Qt::PointingHandCursor);
    m_stopBtn = new QPushButton(QStringLiteral("Stop"), this);
    m_stopBtn->setCursor(Qt::PointingHandCursor);
    m_sendBtn = new QPushButton(this);
    m_sendBtn->setCursor(Qt::PointingHandCursor);

    auto *buttons = new QHBoxLayout;
    buttons->addWidget(m_modeCombo);
    buttons->addWidget(m_attachBtn);
    buttons->addWidget(m_diffBtn);
    buttons->addStretch(1);
    buttons->addWidget(m_stopBtn);
    buttons->addWidget(m_sendBtn);

    auto *body = new QVBoxLayout;
    body->setContentsMargins(12, 12, 12, 12);
    body->setSpacing(10);
    body->addWidget(m_transcript, 1);
    body->addWidget(m_permBar);
    body->addWidget(m_questionBox);
    body->addWidget(m_attachBar);
    body->addWidget(m_input);
    body->addLayout(buttons);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(m_header);
    layout->addLayout(body, 1);

    connect(m_sendBtn, &QPushButton::clicked, this, &AgentPanel::onSendClicked);
    connect(m_stopBtn, &QPushButton::clicked, this, &AgentPanel::onStopClicked);
    connect(m_diffBtn, &QPushButton::clicked, this, &AgentPanel::onChangesClicked);
    connect(m_attachBtn, &QPushButton::clicked, this, &AgentPanel::onAttachClicked);
    connect(m_permAllow, &QPushButton::clicked, this, [this] { answerPermission(true); });
    connect(m_permDeny, &QPushButton::clicked, this, [this] { answerPermission(false); });
    connect(m_core, &CoreClient::notification, this, &AgentPanel::onNotification);

    auto *sendShortcut = new QShortcut(QKeySequence(Qt::CTRL | Qt::Key_Return), m_input);
    connect(sendShortcut, &QShortcut::activated, this, &AgentPanel::onSendClicked);

    refresh();
}

AgentPanel::~AgentPanel()
{
    // Closing a panel ends its agent so the core does not keep it running.
    // A dormant thread has no live process — leave it for a later resume.
    if (!m_threadId.isEmpty() && !m_dormant && m_core->isConnected()) {
        m_core->call(QStringLiteral("agent.stop"),
                     QJsonObject{{QStringLiteral("threadId"), m_threadId}});
    }
}

void AgentPanel::setWorkspace(const QString &path)
{
    m_workspace = path;
    refresh();
}

void AgentPanel::setDormant(const QString &threadId, const QString &title)
{
    m_threadId = threadId;
    m_dormant = true;
    append(QStringLiteral(
               "<p class='dim'>— dormant agent · %1<br>"
               "Resume to continue. The agent keeps the whole conversation; "
               "earlier messages just are not shown here.</p>")
               .arg(title.toHtmlEscaped()));
    emit dormantChanged(true);
    refresh();
}

void AgentPanel::resume()
{
    if (!m_dormant || m_threadId.isEmpty()) {
        return;
    }
    append(QStringLiteral("<p class='sys'>resuming the Claude Code session…</p>"));
    m_core->call(QStringLiteral("agent.resume"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         append(QStringLiteral("<p class='err'>Could not resume: %1</p>")
                                    .arg(error.value(QStringLiteral("message"))
                                             .toString()
                                             .toHtmlEscaped()));
                     }
                 });
}

void AgentPanel::refresh()
{
    const bool running = !m_threadId.isEmpty() && !m_dormant;
    m_sendBtn->setText(m_dormant ? QStringLiteral("Resume agent")
                                 : (running ? QStringLiteral("Send")
                                            : QStringLiteral("Start agent")));
    m_stopBtn->setEnabled(running);
    m_diffBtn->setEnabled(running);
    m_modeCombo->setEnabled(m_threadId.isEmpty()); // mode is fixed once a thread exists

    QString dot;
    QString text;
    if (m_workspace.isEmpty()) {
        dot = QStringLiteral("#5d6471");
        text = QStringLiteral("Open a workspace folder to begin");
    } else if (m_dormant) {
        dot = QStringLiteral("#5d6471");
        text = QStringLiteral("Dormant — Resume to continue this session");
    } else if (!running) {
        dot = QStringLiteral("#8b91a0");
        text = QStringLiteral("Ready — describe a task below");
    } else if (!m_permQueue.isEmpty()) {
        dot = QStringLiteral("#f0c000");
        text = QStringLiteral("Needs your input");
    } else {
        const QString where = (m_isolated && !m_branch.isEmpty())
                                   ? QStringLiteral("branch %1").arg(m_branch)
                                   : QStringLiteral("in workspace");
        if (m_idle) {
            dot = QStringLiteral("#e0905f");
            text = QStringLiteral("Idle · %1 · send a follow-up").arg(where);
        } else {
            dot = QStringLiteral("#6cc08a");
            text = QStringLiteral("Working · %1").arg(where);
        }
    }
    m_header->setText(QStringLiteral("<span style='color:%1'>&#9679;</span>&nbsp;&nbsp;%2")
                          .arg(dot, text.toHtmlEscaped()));
    emit stateChanged(dot);
}

void AgentPanel::onSendClicked()
{
    // A dormant agent must be resumed first; any typed message is sent once the
    // session is back (see the "resumed" lifecycle handler).
    if (m_dormant) {
        resume();
        return;
    }
    const QString text = m_input->toPlainText().trimmed();
    if (text.isEmpty() && m_attachments.isEmpty()) {
        return;
    }
    if (m_workspace.isEmpty()) {
        emit statusMessage(QStringLiteral("Open a workspace folder first"));
        return;
    }
    m_input->clear();

    QString youLine = text.toHtmlEscaped().replace(QLatin1Char('\n'), QLatin1String("<br>"));
    if (!m_attachments.isEmpty()) {
        if (!youLine.isEmpty()) {
            youLine += QStringLiteral("<br>");
        }
        youLine += QStringLiteral("<span class='dim'>&#128206; %1 attachment(s)</span>")
                       .arg(m_attachments.size());
    }
    append(QStringLiteral("<p class='you'><b>You</b> &nbsp; %1</p>").arg(youLine));
    m_idle = false;

    // Detach the pending attachments for this message, then clear the bar.
    const QJsonArray attachments = m_attachments;
    m_attachments = QJsonArray();
    rebuildAttachChips();

    if (m_threadId.isEmpty()) {
        QString title = text.simplified();
        if (title.isEmpty()) {
            title = QStringLiteral("(attachments)");
        }
        if (title.length() > 26) {
            title = title.left(25) + QChar(0x2026);
        }
        emit titleChanged(title);

        m_core->call(QStringLiteral("agent.start"),
                     QJsonObject{{QStringLiteral("workspacePath"), m_workspace},
                                 {QStringLiteral("prompt"), text},
                                 {QStringLiteral("permissionMode"),
                                  m_modeCombo->currentData().toString()},
                                 {QStringLiteral("attachments"), attachments}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             append(QStringLiteral("<p class='err'>Failed to start agent: %1</p>")
                                        .arg(error.value(QStringLiteral("message"))
                                                 .toString()
                                                 .toHtmlEscaped()));
                             return;
                         }
                         m_threadId = result.value(QStringLiteral("threadId")).toString();
                         refresh();
                     });
    } else {
        m_core->call(QStringLiteral("agent.send"),
                     QJsonObject{{QStringLiteral("threadId"), m_threadId},
                                 {QStringLiteral("text"), text},
                                 {QStringLiteral("attachments"), attachments}});
    }
    refresh();
}

void AgentPanel::onAttachClicked()
{
    const QStringList paths = QFileDialog::getOpenFileNames(
        this, QStringLiteral("Attach files"),
        m_workspace.isEmpty() ? QDir::homePath() : m_workspace);

    static const QHash<QString, QString> imageTypes{
        {QStringLiteral("png"), QStringLiteral("image/png")},
        {QStringLiteral("jpg"), QStringLiteral("image/jpeg")},
        {QStringLiteral("jpeg"), QStringLiteral("image/jpeg")},
        {QStringLiteral("gif"), QStringLiteral("image/gif")},
        {QStringLiteral("webp"), QStringLiteral("image/webp")},
        {QStringLiteral("bmp"), QStringLiteral("image/bmp")}};

    for (const QString &path : paths) {
        QFile file(path);
        if (!file.open(QIODevice::ReadOnly)) {
            emit statusMessage(QStringLiteral("Could not read %1").arg(path));
            continue;
        }
        const QByteArray bytes = file.readAll();
        const QFileInfo info(path);
        const QString ext = info.suffix().toLower();

        QJsonObject att{{QStringLiteral("name"), info.fileName()}};
        if (imageTypes.contains(ext)) {
            if (bytes.size() > 5 * 1024 * 1024) {
                emit statusMessage(
                    QStringLiteral("%1 is too large to attach (>5 MB)").arg(info.fileName()));
                continue;
            }
            att[QStringLiteral("kind")] = QStringLiteral("image");
            att[QStringLiteral("mediaType")] = imageTypes.value(ext);
            att[QStringLiteral("dataB64")] = QString::fromLatin1(bytes.toBase64());
        } else {
            QByteArray textBytes = bytes;
            QString suffix;
            if (textBytes.size() > 256 * 1024) {
                textBytes.truncate(256 * 1024);
                suffix = QStringLiteral("\n… (truncated)");
            }
            att[QStringLiteral("kind")] = QStringLiteral("text");
            att[QStringLiteral("text")] = QString::fromUtf8(textBytes) + suffix;
        }
        m_attachments.append(att);
    }
    rebuildAttachChips();
}

void AgentPanel::rebuildAttachChips()
{
    // Drop existing chip widgets, keeping the trailing stretch.
    while (m_attachLayout->count() > 1) {
        QLayoutItem *item = m_attachLayout->takeAt(0);
        if (QWidget *w = item->widget()) {
            w->deleteLater();
        }
        delete item;
    }
    for (int i = 0; i < m_attachments.size(); ++i) {
        const QString name = m_attachments.at(i).toObject().value(QStringLiteral("name")).toString();
        auto *chip = new QPushButton(QStringLiteral("%1   ✕").arg(name), m_attachBar);
        chip->setCursor(Qt::PointingHandCursor);
        chip->setToolTip(QStringLiteral("Remove attachment"));
        connect(chip, &QPushButton::clicked, this, [this, i] {
            m_attachments.removeAt(i);
            rebuildAttachChips();
        });
        m_attachLayout->insertWidget(m_attachLayout->count() - 1, chip);
    }
    m_attachBar->setVisible(!m_attachments.isEmpty());
}

void AgentPanel::onStopClicked()
{
    if (m_threadId.isEmpty()) {
        return;
    }
    m_core->call(QStringLiteral("agent.stop"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId}});
}

void AgentPanel::onChangesClicked()
{
    if (m_threadId.isEmpty()) {
        return;
    }
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("agent.diff"),
                 QJsonObject{{QStringLiteral("threadId"), tid}},
                 [this, tid](const QJsonObject &result, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         emit statusMessage(QStringLiteral("Could not get diff: %1")
                                                .arg(error.value(QStringLiteral("message"))
                                                         .toString()));
                         return;
                     }
                     const QString diff = result.value(QStringLiteral("diff")).toString();
                     if (diff.trimmed().isEmpty()) {
                         emit statusMessage(
                             QStringLiteral("Agent %1 has not changed anything yet").arg(tid));
                         return;
                     }
                     emit openDiff(tid + QStringLiteral(" — changes.diff"), diff);
                 });
}

void AgentPanel::onNotification(const QString &method, const QJsonObject &params)
{
    if (method == QLatin1String("agent.event")) {
        if (!m_threadId.isEmpty()
            && params.value(QStringLiteral("threadId")).toString() == m_threadId) {
            renderEvent(params.value(QStringLiteral("event")).toObject());
        }
    } else if (method == QLatin1String("permission.requested")) {
        onPermissionRequested(params);
    } else if (method == QLatin1String("agent.reviewRequested")) {
        if (!m_threadId.isEmpty()
            && params.value(QStringLiteral("threadId")).toString() == m_threadId) {
            append(QStringLiteral("<p class='ok'>&#128203; Review requested: %1</p>")
                       .arg(params.value(QStringLiteral("summary")).toString().toHtmlEscaped()));
            emit statusMessage(QStringLiteral("Agent %1 requested a review").arg(m_threadId));
        }
    }
}

void AgentPanel::onPermissionRequested(const QJsonObject &params)
{
    if (m_threadId.isEmpty()
        || params.value(QStringLiteral("threadId")).toString() != m_threadId) {
        return;
    }
    m_permQueue.append(params);
    const QString tool = params.value(QStringLiteral("toolName")).toString();
    if (tool == QLatin1String("AskUserQuestion")) {
        append(QStringLiteral("<p class='sys'>&#10067; the agent is asking a question</p>"));
    } else {
        append(QStringLiteral("<p class='sys'>&#128274; permission requested: %1</p>")
                   .arg(tool.toHtmlEscaped()));
    }
    showNextPermission();
    refresh();
}

void AgentPanel::showNextPermission()
{
    if (m_permQueue.isEmpty()) {
        return;
    }
    // Answer one interaction at a time.
    if (m_permBar->isVisible() || m_questionBox->isVisible()) {
        return;
    }
    const QJsonObject req = m_permQueue.constFirst();
    if (req.value(QStringLiteral("toolName")).toString() == QLatin1String("AskUserQuestion")) {
        buildQuestionForm(req);
        return;
    }
    const QString tool = req.value(QStringLiteral("toolName")).toString();
    QString summary = permSummary(tool, req.value(QStringLiteral("input")).toObject());
    if (summary.length() > 240) {
        summary = summary.left(240) + QChar(0x2026);
    }
    m_permLabel->setText(
        QStringLiteral("&#128274;&nbsp; Allow the agent to use <b>%1</b>?<br><tt>%2</tt>")
            .arg(tool.toHtmlEscaped(), summary.toHtmlEscaped()));
    m_permBar->setVisible(true);
}

void AgentPanel::answerPermission(bool allow)
{
    if (m_permQueue.isEmpty()) {
        return;
    }
    const QJsonObject req = m_permQueue.takeFirst();
    m_core->call(QStringLiteral("permission.respond"),
                 QJsonObject{{QStringLiteral("requestId"), req.value(QStringLiteral("requestId"))},
                             {QStringLiteral("allow"), allow}});
    append(QStringLiteral("<p class='%1'>&#128274; %2 — %3</p>")
               .arg(allow ? QStringLiteral("ok") : QStringLiteral("err"),
                    req.value(QStringLiteral("toolName")).toString().toHtmlEscaped(),
                    allow ? QStringLiteral("approved") : QStringLiteral("denied")));
    m_permBar->setVisible(false);
    showNextPermission();
    refresh();
}

void AgentPanel::buildQuestionForm(const QJsonObject &req)
{
    m_questionReq = req;
    m_questionFields.clear();
    clearLayout(m_questionLayout);

    const QJsonObject input = req.value(QStringLiteral("input")).toObject();
    const QJsonArray questions = input.value(QStringLiteral("questions")).toArray();

    auto *intro =
        new QLabel(QStringLiteral("<b>&#10067;&nbsp; The agent needs your input</b>"), m_questionBox);
    intro->setTextFormat(Qt::RichText);
    m_questionLayout->addWidget(intro);

    for (const QJsonValue &qv : questions) {
        const QJsonObject q = qv.toObject();
        QuestionField field;
        field.question = q.value(QStringLiteral("question")).toString();
        field.multiSelect = q.value(QStringLiteral("multiSelect")).toBool();

        auto *container = new QWidget(m_questionBox);
        auto *qLayout = new QVBoxLayout(container);
        qLayout->setContentsMargins(0, 6, 0, 0);
        qLayout->setSpacing(2);

        auto *qLabel = new QLabel(field.question.toHtmlEscaped(), container);
        qLabel->setWordWrap(true);
        qLabel->setStyleSheet(QStringLiteral("font-weight: 600;"));
        qLayout->addWidget(qLabel);

        bool first = true;
        const QJsonArray options = q.value(QStringLiteral("options")).toArray();
        for (const QJsonValue &ov : options) {
            const QJsonObject o = ov.toObject();
            const QString label = o.value(QStringLiteral("label")).toString();
            const QString desc = o.value(QStringLiteral("description")).toString();

            QAbstractButton *btn = nullptr;
            if (field.multiSelect) {
                btn = new QCheckBox(label, container);
            } else {
                // Radio buttons sharing a parent widget are mutually exclusive,
                // so each question's container scopes its own selection.
                auto *radio = new QRadioButton(label, container);
                if (first) {
                    radio->setChecked(true);
                }
                btn = radio;
            }
            qLayout->addWidget(btn);

            if (!desc.isEmpty()) {
                auto *descLabel = new QLabel(desc, container);
                descLabel->setWordWrap(true);
                descLabel->setStyleSheet(
                    QStringLiteral("color: palette(mid); margin-left: 22px;"));
                qLayout->addWidget(descLabel);
            }
            field.options.append({label, btn});
            first = false;
        }

        m_questionLayout->addWidget(container);
        m_questionFields.append(field);
    }

    auto *submit = new QPushButton(QStringLiteral("Submit answers"), m_questionBox);
    submit->setCursor(Qt::PointingHandCursor);
    connect(submit, &QPushButton::clicked, this, &AgentPanel::onQuestionSubmit);
    m_questionLayout->addWidget(submit);

    m_questionBox->setVisible(true);
}

void AgentPanel::onQuestionSubmit()
{
    QJsonObject answers;
    for (const QuestionField &field : m_questionFields) {
        if (field.multiSelect) {
            QJsonArray picked;
            for (const auto &opt : field.options) {
                if (opt.second->isChecked()) {
                    picked.append(opt.first);
                }
            }
            answers[field.question] = picked;
        } else {
            for (const auto &opt : field.options) {
                if (opt.second->isChecked()) {
                    answers[field.question] = opt.first;
                    break;
                }
            }
        }
    }

    QJsonObject updatedInput;
    updatedInput[QStringLiteral("questions")] = m_questionReq.value(QStringLiteral("input"))
                                                    .toObject()
                                                    .value(QStringLiteral("questions"));
    updatedInput[QStringLiteral("answers")] = answers;

    m_core->call(
        QStringLiteral("permission.respond"),
        QJsonObject{{QStringLiteral("requestId"), m_questionReq.value(QStringLiteral("requestId"))},
                    {QStringLiteral("allow"), true},
                    {QStringLiteral("updatedInput"), updatedInput}});

    append(QStringLiteral("<p class='ok'>&#10067; answered the agent's question</p>"));

    if (!m_permQueue.isEmpty()) {
        m_permQueue.removeFirst();
    }
    m_questionBox->setVisible(false);
    showNextPermission();
    refresh();
}

void AgentPanel::renderEvent(const QJsonObject &ev)
{
    const QString type = ev.value(QStringLiteral("type")).toString();

    if (type == QLatin1String("system")) {
        // Only the init system event is worth showing in the transcript.
        if (ev.value(QStringLiteral("subtype")).toString() != QLatin1String("init")) {
            return;
        }
        QStringList mcp;
        const QJsonArray servers = ev.value(QStringLiteral("mcp_servers")).toArray();
        for (const QJsonValue &v : servers) {
            mcp << v.toObject().value(QStringLiteral("name")).toString() + QLatin1Char('=')
                       + v.toObject().value(QStringLiteral("status")).toString();
        }
        QString line = QStringLiteral("session started — model %1")
                            .arg(ev.value(QStringLiteral("model")).toString().toHtmlEscaped());
        if (!mcp.isEmpty()) {
            line += QStringLiteral(", MCP: ") + mcp.join(QStringLiteral(", ")).toHtmlEscaped();
        }
        append(QStringLiteral("<p class='sys'>%1</p>").arg(line));

    } else if (type == QLatin1String("assistant")) {
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            const QString bt = b.value(QStringLiteral("type")).toString();
            if (bt == QLatin1String("text")) {
                const QString t = b.value(QStringLiteral("text")).toString().trimmed();
                if (!t.isEmpty()) {
                    append(markdownToHtml(t));
                }
            } else if (bt == QLatin1String("tool_use")) {
                const QString name = b.value(QStringLiteral("name")).toString();
                // The permission gate and question tool are surfaced by their
                // own UI, so don't also list them as raw tool calls.
                if (name.contains(QLatin1String("request_permission"))
                    || name == QLatin1String("AskUserQuestion")) {
                    continue;
                }
                append(QStringLiteral("<p class='tool'>&#128295; %1</p>")
                           .arg(name.toHtmlEscaped()));
            }
        }

    } else if (type == QLatin1String("user")) {
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        for (const QJsonValue &bv : content) {
            if (bv.toObject().value(QStringLiteral("type")).toString()
                == QLatin1String("tool_result")) {
                append(QStringLiteral("<p class='res'>&#8627; tool result</p>"));
            }
        }

    } else if (type == QLatin1String("result")) {
        const bool err = ev.value(QStringLiteral("is_error")).toBool();
        append(QStringLiteral("<p class='%1'>%2 turn complete</p>")
                   .arg(err ? QStringLiteral("err") : QStringLiteral("ok"),
                        err ? QStringLiteral("✗") : QStringLiteral("✓")));
        m_idle = true;
        refresh();

    } else if (type == QLatin1String("_stderr")) {
        append(QStringLiteral("<p class='dim'>%1</p>")
                   .arg(ev.value(QStringLiteral("text")).toString().toHtmlEscaped()));

    } else if (type == QLatin1String("_lifecycle")) {
        const QString phase = ev.value(QStringLiteral("phase")).toString();
        const QString detail = ev.value(QStringLiteral("detail")).toString().toHtmlEscaped();
        if (phase == QLatin1String("started")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            append(QStringLiteral("<p class='dim'>— %1</p>").arg(detail));
            refresh();
        } else if (phase == QLatin1String("resumed")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            m_dormant = false;
            m_idle = true;
            append(QStringLiteral("<p class='dim'>— %1 · ready for a follow-up</p>")
                       .arg(detail));
            emit dormantChanged(false);
            refresh();
            // Deliver any message the human typed before pressing Resume.
            if (!m_input->toPlainText().trimmed().isEmpty() || !m_attachments.isEmpty()) {
                onSendClicked();
            }
        } else if (phase == QLatin1String("error")) {
            append(QStringLiteral("<p class='err'>agent failed: %1</p>").arg(detail));
            m_idle = false;
            if (!m_dormant) {
                m_threadId.clear(); // a fresh start failed — back to a blank panel
            }
            refresh();
        } else if (phase == QLatin1String("exited")) {
            append(QStringLiteral("<p class='dim'>— agent exited: %1</p>").arg(detail));
            m_idle = false;
            m_permQueue.clear();
            m_permBar->setVisible(false);
            m_questionBox->setVisible(false);
            // The process is gone but the Claude Code session persists — keep
            // the thread id and mark the agent resumable.
            if (!m_threadId.isEmpty()) {
                m_dormant = true;
                emit dormantChanged(true);
            }
            refresh();
        }
    }
}

void AgentPanel::append(const QString &html)
{
    m_transcript->append(html);
    m_transcript->verticalScrollBar()->setValue(m_transcript->verticalScrollBar()->maximum());
}
