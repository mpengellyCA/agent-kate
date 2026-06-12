#include "AgentPanel.h"
#include "ipc/CoreClient.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QAbstractButton>
#include <QCheckBox>
#include <QComboBox>
#include <QDialog>
#include <QDialogButtonBox>
#include <QDir>
#include <QDragEnterEvent>
#include <QDragMoveEvent>
#include <QDropEvent>
#include <QEvent>
#include <QMimeData>
#include <QUrl>
#include <QFile>
#include <QFileDialog>
#include <QFileInfo>
#include <QFormLayout>
#include <QFrame>
#include <QHBoxLayout>
#include <QIcon>
#include <QJsonArray>
#include <QJsonDocument>
#include <QKeyEvent>
#include <QLabel>
#include <QLayout>
#include <QPaintEvent>
#include <QPainter>
#include <QPalette>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QRadioButton>
#include <QScrollArea>
#include <QScrollBar>
#include <QSignalBlocker>
#include <QMenu>
#include <QTextDocument>
#include <QTimer>
#include <QToolButton>
#include <QVBoxLayout>
#include <QWidgetAction>

namespace {
bool isDark(const QWidget *w)
{
    return w->palette().color(QPalette::Base).lightness() < 128;
}

// noteColor picks a palette-aware colour for a quiet status line.
QString noteColor(const QString &kind, bool dark)
{
    if (kind == QLatin1String("ok")) {
        return dark ? QStringLiteral("#5fd38a") : QStringLiteral("#1a7f37");
    }
    if (kind == QLatin1String("err")) {
        return dark ? QStringLiteral("#ff8a80") : QStringLiteral("#c01c28");
    }
    return dark ? QStringLiteral("#9a9aa3") : QStringLiteral("#6b6b72"); // sys / dim
}

// markdownToHtml renders an assistant message (Markdown) to an HTML fragment.
// Default-coloured text carries no explicit colour, so it inherits the card's
// palette text colour.
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

// permSummary renders a tool's input as a short, human-readable line.
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
    for (const QString &key : {QStringLiteral("file_path"), QStringLiteral("path"),
                               QStringLiteral("pattern"), QStringLiteral("description")}) {
        const QString v = input.value(key).toString();
        if (!v.isEmpty()) {
            return v;
        }
    }
    return QString::fromUtf8(QJsonDocument(input).toJson(QJsonDocument::Compact));
}

// toolResultText pulls plain text out of a tool_result content value, which may
// be a bare string or an array of content blocks.
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

// activityFor maps a tool name to a personable status line for the
// "Agent Kate at work" indicator.
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
    if (tool.startsWith(QLatin1String("mcp__"))) {
        return QStringLiteral("Agent Kate is coordinating with the team…");
    }
    return QStringLiteral("Agent Kate is working with %1…").arg(tool);
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

// ToolCard is a collapsible card for one tool call: its header is the minimal
// summary, and clicking it reveals the full input and result.
class ToolCard : public QFrame
{
public:
    ToolCard(const QString &tool, const QString &summary, const QString &detail,
             QWidget *parent)
        : QFrame(parent)
        , m_tool(tool)
        , m_summary(summary)
    {
        setObjectName(QStringLiteral("toolCard"));
        setStyleSheet(QStringLiteral(
            "QFrame#toolCard { border: 1px solid palette(mid); border-radius: 7px; }"
            "QToolButton { border: none; text-align: left; padding: 5px 8px; }"));

        auto *outer = new QVBoxLayout(this);
        outer->setContentsMargins(2, 2, 2, 2);
        outer->setSpacing(0);

        m_header = new QToolButton(this);
        m_header->setCheckable(true);
        m_header->setCursor(Qt::PointingHandCursor);
        m_header->setSizePolicy(QSizePolicy::Expanding, QSizePolicy::Fixed);
        updateHeader();
        outer->addWidget(m_header);

        m_detail = new QWidget(this);
        auto *dv = new QVBoxLayout(m_detail);
        dv->setContentsMargins(10, 2, 10, 8);
        dv->setSpacing(6);
        if (!detail.trimmed().isEmpty()) {
            auto *in = new QLabel(detail.trimmed(), m_detail);
            in->setWordWrap(true);
            in->setTextInteractionFlags(Qt::TextSelectableByMouse);
            in->setStyleSheet(QStringLiteral("font-family: monospace; font-size: small;"));
            dv->addWidget(in);
        }
        m_result = new QLabel(m_detail);
        m_result->setWordWrap(true);
        m_result->setTextInteractionFlags(Qt::TextSelectableByMouse);
        m_result->setStyleSheet(QStringLiteral(
            "font-family: monospace; font-size: small;"));
        m_result->setForegroundRole(QPalette::WindowText);
        m_result->setVisible(false);
        dv->addWidget(m_result);
        m_detail->setVisible(false);
        outer->addWidget(m_detail);

        connect(m_header, &QToolButton::toggled, m_header, [this](bool on) {
            m_detail->setVisible(on);
            updateHeader();
        });
    }

    void setResult(const QString &text)
    {
        QString t = text.trimmed();
        if (t.size() > 4000) {
            t = t.left(4000) + QStringLiteral("\n… (truncated)");
        }
        m_result->setText(t.isEmpty() ? QStringLiteral("(no output)") : t);
        m_result->setVisible(true);
        m_done = true;
        updateHeader();
    }

private:
    void updateHeader()
    {
        const QString arrow =
            m_header->isChecked() ? QStringLiteral("▾") : QStringLiteral("▸");
        const QString mark = m_done ? QStringLiteral("✓") : QStringLiteral("⋯");
        m_header->setText(QStringLiteral("%1  %2  🔧 %3   %4")
                              .arg(arrow, mark, m_tool, m_summary));
    }

    QToolButton *m_header = nullptr;
    QWidget *m_detail = nullptr;
    QLabel *m_result = nullptr;
    QString m_tool;
    QString m_summary;
    bool m_done = false;
};

// WorkingIndicator is the animated "Agent Kate at work" status: a rotating spinner
// and a personable, activity-aware line, shown while the agent computes.
class WorkingIndicator : public QWidget
{
public:
    explicit WorkingIndicator(QWidget *parent)
        : QWidget(parent)
        , m_generic{
              QStringLiteral("Agent Kate is thinking it through…"),
              QStringLiteral("Agent Kate is battling the problem…"),
              QStringLiteral("Agent Kate is breaking it down…"),
              QStringLiteral("Agent Kate is connecting the dots…"),
              QStringLiteral("Agent Kate is wrangling the logic…"),
              QStringLiteral("Agent Kate is plotting the next move…"),
              QStringLiteral("Agent Kate is untangling the details…"),
          }
    {
        setFixedHeight(30);
        setSizePolicy(QSizePolicy::Expanding, QSizePolicy::Fixed);
        m_timer = new QTimer(this);
        m_timer->setInterval(70);
        connect(m_timer, &QTimer::timeout, this, [this] {
            m_angle = (m_angle + 26) % 360;
            if (++m_ticks % 48 == 0) {
                ++m_genericIndex;
            }
            update();
        });
        setVisible(false);
    }

    void setActive(bool on)
    {
        if (on == m_active) {
            return;
        }
        m_active = on;
        setVisible(on);
        if (on) {
            m_timer->start();
        } else {
            m_timer->stop();
        }
    }

    void setActivity(const QString &message)
    {
        if (message == m_activity) {
            return;
        }
        m_activity = message;
        update();
    }

protected:
    void paintEvent(QPaintEvent *) override
    {
        QPainter p(this);
        p.setRenderHint(QPainter::Antialiasing);
        const bool dark = palette().color(QPalette::Base).lightness() < 128;
        const QColor accent =
            dark ? QColor(0x5f, 0xd3, 0xbf) : QColor(0x1a, 0x7f, 0x6b);

        // Rotating arc spinner.
        const qreal d = 15;
        const qreal cx = 9 + d / 2;
        const qreal cy = height() / 2.0;
        QPen pen(accent, 2.4);
        pen.setCapStyle(Qt::RoundCap);
        p.setPen(pen);
        p.drawArc(QRectF(cx - d / 2, cy - d / 2, d, d),
                  -m_angle * 16, -280 * 16);

        // Personable, activity-aware message.
        const QString msg = m_activity.isEmpty()
            ? m_generic.at(m_genericIndex % m_generic.size())
            : m_activity;
        QColor textCol = palette().color(QPalette::WindowText);
        textCol.setAlpha(200);
        p.setPen(textCol);
        const int textX = int(cx + d / 2 + 12);
        p.drawText(QRect(textX, 0, width() - textX, height()),
                   Qt::AlignVCenter | Qt::AlignLeft, msg);
    }

private:
    QTimer *m_timer = nullptr;
    QStringList m_generic;
    QString m_activity;
    int m_angle = 0;
    int m_ticks = 0;
    int m_genericIndex = 0;
    bool m_active = false;
};

AgentPanel::AgentPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    setAcceptDrops(true); // ProjectTree (and external sources) drop file URLs here
    m_header = new QLabel(this);
    m_header->setTextFormat(Qt::RichText);
    m_header->setStyleSheet(
        QStringLiteral("padding: 9px 12px; border-bottom: 1px solid palette(mid);"));

    // --- conversation feed: a scrollable column of cards ------------------
    m_feed = new QWidget;
    m_feedLayout = new QVBoxLayout(m_feed);
    m_feedLayout->setContentsMargins(4, 4, 4, 4);
    m_feedLayout->setSpacing(8);
    m_feedLayout->addStretch(1); // trailing stretch keeps cards top-aligned

    m_feedScroll = new QScrollArea(this);
    m_feedScroll->setWidget(m_feed);
    m_feedScroll->setWidgetResizable(true);
    m_feedScroll->setFrameShape(QFrame::NoFrame);
    m_feedScroll->setHorizontalScrollBarPolicy(Qt::ScrollBarAlwaysOff);

    // Sticky-bottom: the feed auto-scrolls to keep the latest entry in view.
    // Scrolling upward releases the stick; scrolling back to the bottom reclaims
    // it. rangeChanged covers the case where a card's wrapped height arrives a
    // frame after insertion (long tool output, markdown reflow, …).
    QScrollBar *bar = m_feedScroll->verticalScrollBar();
    connect(bar, &QScrollBar::valueChanged, this, [this, bar](int v) {
        m_stickBottom = (v >= bar->maximum() - 48);
    });
    connect(bar, &QScrollBar::rangeChanged, this, [this, bar](int, int max) {
        if (m_stickBottom) {
            bar->setValue(max);
        }
    });

    m_working = new WorkingIndicator(this);

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

    // --- promote-to-worktree bar (shown while a thread runs non-isolated) ---
    m_promoteBar = new QFrame(this);
    m_promoteBar->setObjectName(QStringLiteral("promoteBar"));
    m_promoteBar->setStyleSheet(QStringLiteral(
        "QFrame#promoteBar { border: 1px solid palette(mid); border-radius: 6px; }"));
    m_promoteBar->setVisible(false);
    auto *promoteLabel = new QLabel(
        QStringLiteral("Running directly in the workspace — this agent is not isolated."),
        m_promoteBar);
    promoteLabel->setWordWrap(true);
    m_promoteBtn = new QPushButton(QStringLiteral("Promote to worktree"), m_promoteBar);
    m_promoteBtn->setCursor(Qt::PointingHandCursor);
    auto *promoteLayout = new QHBoxLayout(m_promoteBar);
    promoteLayout->setContentsMargins(10, 6, 10, 6);
    promoteLayout->addWidget(promoteLabel, 1);
    promoteLayout->addWidget(m_promoteBtn);

    m_input = new QPlainTextEdit(this);
    m_input->setFixedHeight(94);
    m_input->installEventFilter(this); // for the configurable send key

    m_modeCombo = new QComboBox(this);
    m_modeCombo->addItem(QStringLiteral("Accept edits"), QStringLiteral("acceptEdits"));
    m_modeCombo->addItem(QStringLiteral("Approve each tool"), QStringLiteral("default"));
    m_modeCombo->addItem(QStringLiteral("Auto"), QStringLiteral("auto"));
    m_modeCombo->addItem(QStringLiteral("Unsafe (bypass)"), QStringLiteral("bypassPermissions"));
    m_modeCombo->setToolTip(QStringLiteral("Permission mode for this agent (fixed once it starts)"));
    // Sticky: the last choice becomes the default for the next agent — except
    // for "Unsafe (bypass)", which resets to "Auto" so it's never re-armed
    // accidentally on the next conversation.
    {
        QString saved = KSharedConfig::openConfig()
                            ->group(QStringLiteral("Agent"))
                            .readEntry("permissionMode", QStringLiteral("acceptEdits"));
        if (saved == QLatin1String("bypassPermissions")) {
            saved = QStringLiteral("auto");
        }
        const int savedIdx = m_modeCombo->findData(saved);
        if (savedIdx >= 0) {
            m_modeCombo->setCurrentIndex(savedIdx);
        }
    }
    connect(m_modeCombo, &QComboBox::currentIndexChanged, this, [this] {
        const QString mode = m_modeCombo->currentData().toString();
        // Don't persist the unsafe choice — next agent falls back to Auto.
        if (mode == QLatin1String("bypassPermissions")) {
            return;
        }
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("permissionMode", mode);
    });

    m_isolationCombo = new QComboBox(this);
    m_isolationCombo->addItem(QStringLiteral("Auto isolation"), QStringLiteral("auto"));
    m_isolationCombo->addItem(QStringLiteral("Isolated worktree"),
                              QStringLiteral("isolated"));
    m_isolationCombo->addItem(QStringLiteral("In the workspace"),
                              QStringLiteral("workspace"));
    m_isolationCombo->setToolTip(QStringLiteral(
        "Where this agent runs, fixed once it starts:\n"
        "• Auto isolation — its own git worktree when the repo has commits,\n"
        "  otherwise directly in the workspace\n"
        "• Isolated worktree — always its own worktree on branch agentkate/<id>\n"
        "• In the workspace — directly in the project, no isolation"));
    // Sticky: the last choice becomes the default for the next agent.
    {
        const QString saved = KSharedConfig::openConfig()
                                  ->group(QStringLiteral("Agent"))
                                  .readEntry("isolation", QStringLiteral("auto"));
        const int savedIdx = m_isolationCombo->findData(saved);
        if (savedIdx >= 0) {
            m_isolationCombo->setCurrentIndex(savedIdx);
        }
    }
    connect(m_isolationCombo, &QComboBox::currentIndexChanged, this, [this] {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("isolation", m_isolationCombo->currentData().toString());
    });

    m_effortCombo = new QComboBox(this);
    // An empty value passes no --effort flag, leaving Claude Code's own default.
    m_effortCombo->addItem(QStringLiteral("Default effort"), QString());
    m_effortCombo->addItem(QStringLiteral("Low effort"), QStringLiteral("low"));
    m_effortCombo->addItem(QStringLiteral("Medium effort"), QStringLiteral("medium"));
    m_effortCombo->addItem(QStringLiteral("High effort"), QStringLiteral("high"));
    m_effortCombo->addItem(QStringLiteral("Extra-high effort"), QStringLiteral("xhigh"));
    m_effortCombo->addItem(QStringLiteral("Max effort"), QStringLiteral("max"));
    m_effortCombo->setToolTip(QStringLiteral(
        "Reasoning effort for this agent, fixed once it starts.\n"
        "Higher levels let Claude Code think longer before it acts.\n"
        "Default effort leaves Claude Code's own configured level untouched."));
    // Sticky: the last choice becomes the default for the next agent.
    {
        const QString saved = KSharedConfig::openConfig()
                                  ->group(QStringLiteral("Agent"))
                                  .readEntry("effort", QString());
        const int savedIdx = m_effortCombo->findData(saved);
        if (savedIdx >= 0) {
            m_effortCombo->setCurrentIndex(savedIdx);
        }
    }
    connect(m_effortCombo, &QComboBox::currentIndexChanged, this, [this] {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("effort", m_effortCombo->currentData().toString());
    });

    // Compaction strategy. Keeping a thread resumable cheaply needs a
    // condensed summary on disk — otherwise the next resume re-caches the
    // whole transcript. The five options encode (when, model) combos; the
    // strip flag asks LLM-based compactors to pre-trim noisy events.
    m_compactCombo = new QComboBox(this);
    m_compactCombo->addItem(QStringLiteral("Compact on Exit (Hot Opus)"),
                            QStringLiteral("exit_opus_hot"));
    m_compactCombo->addItem(QStringLiteral("Compact on Exit (Cold Sonnet)"),
                            QStringLiteral("exit_sonnet_cold"));
    m_compactCombo->addItem(QStringLiteral("Compact on Resume (Cold Sonnet)"),
                            QStringLiteral("resume_sonnet_cold"));
    m_compactCombo->addItem(QStringLiteral("Compact on Resume (Cold Haiku)"),
                            QStringLiteral("resume_haiku_cold"));
    m_compactCombo->addItem(QStringLiteral("Compact on Resume (Local)"),
                            QStringLiteral("resume_local"));
    m_compactCombo->setToolTip(QStringLiteral(
        "When and how this thread's transcript is condensed for resumption.\n"
        "Hot Opus is most accurate and cheapest in dollars but spends Opus quota.\n"
        "Sonnet on a Max plan uses a separate quota bucket. Local is free but\n"
        "behaviourally-lossless only — preserves decisions, drops tool outputs."));
    {
        const QString saved = KSharedConfig::openConfig()
                                  ->group(QStringLiteral("Agent"))
                                  .readEntry("compactStrategy",
                                             QStringLiteral("exit_opus_hot"));
        const int savedIdx = m_compactCombo->findData(saved);
        if (savedIdx >= 0) {
            m_compactCombo->setCurrentIndex(savedIdx);
        }
    }
    m_compactStrip = new QCheckBox(QStringLiteral("Strip"), this);
    m_compactStrip->setToolTip(QStringLiteral(
        "Pre-trim noisy events (stale reads, lifecycle, etc.) before handing\n"
        "the transcript to an LLM compactor. No effect on the Local strategy."));
    m_compactStrip->setChecked(KSharedConfig::openConfig()
                                   ->group(QStringLiteral("Agent"))
                                   .readEntry("compactStrip", false));
    connect(m_compactCombo, &QComboBox::currentIndexChanged, this, [this] {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("compactStrategy", m_compactCombo->currentData().toString());
        pushCompactStrategy();
    });
    connect(m_compactStrip, &QCheckBox::toggled, this, [this](bool on) {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("compactStrip", on);
        pushCompactStrategy();
    });

    // "Compact now ▾" — one-shot compaction with any backend, independent of
    // the scheduled strategy above. Hot Opus needs a live thread; the other
    // backends just need the recorded Claude Code session id.
    m_compactNowBtn = new QToolButton(this);
    m_compactNowBtn->setText(QStringLiteral("Compact now"));
    m_compactNowBtn->setPopupMode(QToolButton::InstantPopup);
    m_compactNowBtn->setToolButtonStyle(Qt::ToolButtonTextOnly);
    m_compactNowBtn->setCursor(Qt::PointingHandCursor);
    m_compactNowBtn->setToolTip(QStringLiteral(
        "Compact this thread now with the backend of your choice.\n"
        "Hot Opus runs inline on the live thread and needs it running.\n"
        "Cold backends (Opus/Sonnet/Haiku) re-read the saved transcript.\n"
        "Local is free and behaviourally-lossless."));
    {
        auto *menu = new QMenu(m_compactNowBtn);
        auto add = [this, menu](const QString &label, const QString &token) {
            QAction *a = menu->addAction(label);
            connect(a, &QAction::triggered, this, [this, token] { runCompactNow(token); });
            return a;
        };
        add(QStringLiteral("Hot Opus (live thread)"), QStringLiteral("hot"));
        menu->addSeparator();
        add(QStringLiteral("Cold Opus"), QStringLiteral("opus"));
        add(QStringLiteral("Cold Sonnet"), QStringLiteral("sonnet"));
        add(QStringLiteral("Cold Haiku"), QStringLiteral("haiku"));
        add(QStringLiteral("Local (programmatic)"), QStringLiteral("local"));
        m_compactNowBtn->setMenu(menu);
    }

    // Attachment chip bar — hidden until files are attached.
    m_attachBar = new QWidget(this);
    m_attachLayout = new QHBoxLayout(m_attachBar);
    m_attachLayout->setContentsMargins(0, 0, 0, 0);
    m_attachLayout->setSpacing(6);
    m_attachLayout->addStretch(1);
    m_attachBar->setVisible(false);

    m_attachBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("mail-attachment")), QStringLiteral("Attach…"), this);
    m_attachBtn->setCursor(Qt::PointingHandCursor);
    m_diffBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("vcs-diff")), QStringLiteral("Changes"), this);
    m_diffBtn->setCursor(Qt::PointingHandCursor);
    m_stopBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("process-stop")), QStringLiteral("Stop"), this);
    m_stopBtn->setCursor(Qt::PointingHandCursor);
    m_sendBtn = new QPushButton(this);
    m_sendBtn->setIcon(QIcon::fromTheme(QStringLiteral("document-send")));
    m_sendBtn->setCursor(Qt::PointingHandCursor);

    // "Setup ▾" — collapses the fixed-at-start configs (mode, isolation,
    // effort) into one popup so the toolbar isn't dominated by combos that
    // grey out the moment the agent starts.
    auto buildSetupMenu = [this] {
        auto *menu = new QMenu(this);
        auto *panel = new QWidget(menu);
        auto *form = new QFormLayout(panel);
        form->setContentsMargins(10, 8, 10, 8);
        form->addRow(QStringLiteral("Permission"), m_modeCombo);
        form->addRow(QStringLiteral("Isolation"), m_isolationCombo);
        form->addRow(QStringLiteral("Effort"), m_effortCombo);
        auto *action = new QWidgetAction(menu);
        action->setDefaultWidget(panel);
        menu->addAction(action);
        return menu;
    };
    auto *setupBtn = new QToolButton(this);
    setupBtn->setText(QStringLiteral("Setup"));
    setupBtn->setIcon(QIcon::fromTheme(QStringLiteral("configure")));
    setupBtn->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    setupBtn->setPopupMode(QToolButton::InstantPopup);
    setupBtn->setCursor(Qt::PointingHandCursor);
    setupBtn->setToolTip(QStringLiteral(
        "Permission, isolation, and reasoning effort for this agent.\n"
        "These are fixed once the agent starts."));
    setupBtn->setMenu(buildSetupMenu());

    // "Compaction ▾" — strategy + strip live + a "Compact now" submenu for
    // one-shot runs. Replaces the standalone combo/checkbox/compact-now trio.
    auto buildCompactionMenu = [this] {
        auto *menu = new QMenu(this);
        auto *panel = new QWidget(menu);
        auto *form = new QFormLayout(panel);
        form->setContentsMargins(10, 8, 10, 8);
        form->addRow(QStringLiteral("Strategy"), m_compactCombo);
        form->addRow(QString(), m_compactStrip);
        auto *panelAction = new QWidgetAction(menu);
        panelAction->setDefaultWidget(panel);
        menu->addAction(panelAction);
        menu->addSeparator();
        auto *nowMenu = menu->addMenu(QStringLiteral("Compact now"));
        m_compactNowMenu = nowMenu; // kept so updateActionStates can disable it
        auto add = [this, nowMenu](const QString &label, const QString &token) {
            QAction *a = nowMenu->addAction(label);
            connect(a, &QAction::triggered, this, [this, token] { runCompactNow(token); });
            return a;
        };
        add(QStringLiteral("Hot Opus (live thread)"), QStringLiteral("hot"));
        nowMenu->addSeparator();
        add(QStringLiteral("Cold Opus"), QStringLiteral("opus"));
        add(QStringLiteral("Cold Sonnet"), QStringLiteral("sonnet"));
        add(QStringLiteral("Cold Haiku"), QStringLiteral("haiku"));
        add(QStringLiteral("Local (programmatic)"), QStringLiteral("local"));
        return menu;
    };
    auto *compactionBtn = new QToolButton(this);
    compactionBtn->setText(QStringLiteral("Compaction"));
    compactionBtn->setIcon(QIcon::fromTheme(QStringLiteral("edit-clear-history")));
    compactionBtn->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    compactionBtn->setPopupMode(QToolButton::InstantPopup);
    compactionBtn->setCursor(Qt::PointingHandCursor);
    compactionBtn->setToolTip(QStringLiteral(
        "When and how this thread's transcript is condensed for resumption,\n"
        "plus a one-shot \"Compact now\" with any backend."));
    compactionBtn->setMenu(buildCompactionMenu());

    // The standalone Compact-now button is now folded into the Compaction
    // menu, but its QToolButton is still constructed above for compatibility
    // with the existing enable/disable wiring. Hide it from the toolbar.
    m_compactNowBtn->hide();

    auto *buttons = new QHBoxLayout;
    buttons->addWidget(setupBtn);
    buttons->addWidget(compactionBtn);
    buttons->addWidget(m_attachBtn);
    buttons->addWidget(m_diffBtn);
    buttons->addStretch(1);
    buttons->addWidget(m_stopBtn);
    buttons->addWidget(m_sendBtn);

    auto *body = new QVBoxLayout;
    body->setContentsMargins(12, 12, 12, 12);
    body->setSpacing(10);
    body->addWidget(m_feedScroll, 1);
    body->addWidget(m_working);
    body->addWidget(m_permBar);
    body->addWidget(m_questionBox);
    body->addWidget(m_promoteBar);
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
    connect(m_promoteBtn, &QPushButton::clicked, this, &AgentPanel::onPromoteClicked);
    connect(m_attachBtn, &QPushButton::clicked, this, &AgentPanel::onAttachClicked);
    connect(m_permAllow, &QPushButton::clicked, this, [this] { answerPermission(true); });
    connect(m_permDeny, &QPushButton::clicked, this, [this] { answerPermission(false); });
    connect(m_core, &CoreClient::notification, this, &AgentPanel::onNotification);

    applyChatSettings();
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

void AgentPanel::applyChatSettings()
{
    const KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    const bool enterSends = cfg.readEntry("enterSends", true);
    m_input->setPlaceholderText(
        enterSends
            ? QStringLiteral("Describe a task for the agent…   "
                             "(Enter to send · Ctrl/Shift+Enter for a new line)")
            : QStringLiteral("Describe a task for the agent…   (Ctrl+Enter to send)"));

    const bool showTools = cfg.readEntry("showTools", true);
    for (ToolCard *card : std::as_const(m_toolCards)) {
        card->setVisible(showTools);
    }
}

bool AgentPanel::eventFilter(QObject *obj, QEvent *event)
{
    if (obj == m_input && event->type() == QEvent::KeyPress) {
        auto *key = static_cast<QKeyEvent *>(event);
        if (key->key() == Qt::Key_Return || key->key() == Qt::Key_Enter) {
            const bool ctrl = key->modifiers().testFlag(Qt::ControlModifier);
            const bool shift = key->modifiers().testFlag(Qt::ShiftModifier);
            const bool enterSends = KSharedConfig::openConfig()
                                        ->group(QStringLiteral("Agent"))
                                        .readEntry("enterSends", true);
            if (enterSends) {
                // Enter sends; Ctrl/Shift+Enter inserts a newline.
                if (ctrl || shift) {
                    m_input->insertPlainText(QStringLiteral("\n"));
                } else {
                    onSendClicked();
                }
                return true;
            }
            // Ctrl+Enter sends; plain Enter falls through to a newline.
            if (ctrl) {
                onSendClicked();
                return true;
            }
        }
    }
    return QWidget::eventFilter(obj, event);
}

void AgentPanel::setDormant(const QString &threadId, const QString &title, bool isolated)
{
    m_threadId = threadId;
    m_dormant = true;
    m_isolated = isolated;
    loadTranscript();
    // Pull the thread's persisted compaction strategy and reflect it in the
    // dropdown — overrides whatever sticky default the panel was showing.
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("agent.summaryStatus"),
                 QJsonObject{{QStringLiteral("threadId"), tid}},
                 [this, tid](const QJsonObject &result, const QJsonObject &) {
                     if (tid != m_threadId) {
                         return;
                     }
                     const QString strategy =
                         result.value(QStringLiteral("strategy")).toString();
                     if (!strategy.isEmpty()) {
                         const int idx = m_compactCombo->findData(strategy);
                         if (idx >= 0) {
                             QSignalBlocker blocker(m_compactCombo);
                             m_compactCombo->setCurrentIndex(idx);
                         }
                     }
                     QSignalBlocker blocker(m_compactStrip);
                     m_compactStrip->setChecked(
                         result.value(QStringLiteral("strip")).toBool(false));
                 });
    addNote(QStringLiteral("dormant agent · %1 — Resume to continue.")
                .arg(title.toHtmlEscaped()),
            QStringLiteral("sys"));
    emit dormantChanged(true);
    refresh();
}

void AgentPanel::loadTranscript()
{
    if (m_threadId.isEmpty() || !m_core->isConnected()) {
        return;
    }
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("agent.transcript"),
                 QJsonObject{{QStringLiteral("threadId"), tid}},
                 [this, tid](const QJsonObject &result, const QJsonObject &error) {
                     if (tid != m_threadId) {
                         return; // panel moved to another thread before this returned
                     }
                     if (!error.isEmpty()) {
                         addNote(QStringLiteral("Could not load history: %1")
                                     .arg(error.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("dim"));
                         return;
                     }
                     const QJsonArray events =
                         result.value(QStringLiteral("events")).toArray();
                     if (events.isEmpty()) {
                         return;
                     }
                     for (const QJsonValue &v : events) {
                         renderEvent(v.toObject());
                     }
                     addNote(QStringLiteral("— prior conversation restored —"),
                             QStringLiteral("dim"));
                     scrollFeedToBottom();
                 });
}

void AgentPanel::pushCompactStrategy()
{
    if (m_threadId.isEmpty() || !m_core || !m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("agent.setCompactStrategy"),
                 QJsonObject{
                     {QStringLiteral("threadId"), m_threadId},
                     {QStringLiteral("strategy"), m_compactCombo->currentData().toString()},
                     {QStringLiteral("strip"), m_compactStrip->isChecked()},
                 });
}

void AgentPanel::runCompactNow(const QString &model)
{
    if (m_threadId.isEmpty() || !m_core || !m_core->isConnected()) {
        addNote(QStringLiteral("Cannot compact: no thread or core is offline."),
                QStringLiteral("err"));
        return;
    }
    if (model == QLatin1String("hot") && m_dormant) {
        addNote(QStringLiteral("Hot Opus needs a running thread. Resume first, "
                               "or pick a cold backend."),
                QStringLiteral("err"));
        return;
    }
    addNote(QStringLiteral("compacting with <b>%1</b>…").arg(model.toHtmlEscaped()),
            QStringLiteral("sys"));
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("agent.compactNow"),
                 QJsonObject{
                     {QStringLiteral("threadId"), tid},
                     {QStringLiteral("model"), model},
                 },
                 [this, tid](const QJsonObject &res, const QJsonObject &err) {
                     if (tid != m_threadId) {
                         return;
                     }
                     if (!err.isEmpty()) {
                         addNote(QStringLiteral("Compaction failed: %1")
                                     .arg(err.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("err"));
                         return;
                     }
                     addNote(QStringLiteral("compacted via %1 (%2 turns, %3 bytes).")
                                 .arg(res.value(QStringLiteral("strategy"))
                                          .toString()
                                          .toHtmlEscaped())
                                 .arg(res.value(QStringLiteral("turns")).toInt())
                                 .arg(res.value(QStringLiteral("bodyBytes")).toInt()),
                             QStringLiteral("ok"));
                 });
}

void AgentPanel::doResume()
{
    addNote(QStringLiteral("resuming the Claude Code session…"), QStringLiteral("sys"));
    m_core->call(QStringLiteral("agent.resume"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         addNote(QStringLiteral("Could not resume: %1")
                                     .arg(error.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("err"));
                     }
                 });
}

// askRecoveryModel pops a modal asking which model should produce a missing
// compacted summary before resume. Returns "opus"|"sonnet"|"haiku"|"local",
// or "" if the user cancelled (in which case the caller should resume on the
// full transcript and pay the re-cache cost knowingly).
static QString askRecoveryModel(QWidget *parent)
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

void AgentPanel::resume()
{
    if (!m_dormant || m_threadId.isEmpty()) {
        return;
    }
    const QString tid = m_threadId;
    // Check whether a current compacted summary already exists. If not, ask
    // the user which model should produce one before we resume — see the
    // recovery dialog. If yes, resume straight away.
    m_core->call(QStringLiteral("agent.summaryStatus"),
                 QJsonObject{{QStringLiteral("threadId"), tid}},
                 [this, tid](const QJsonObject &result, const QJsonObject &error) {
                     if (tid != m_threadId) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         // Status call failed — don't block the user; resume
                         // on the full transcript and log the issue.
                         addNote(QStringLiteral("Could not check summary status (%1); "
                                                "resuming on the full transcript.")
                                     .arg(error.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("dim"));
                         doResume();
                         return;
                     }
                     const bool stale =
                         result.value(QStringLiteral("stale")).toBool(true);
                     if (!stale) {
                         doResume();
                         return;
                     }
                     const QString model = askRecoveryModel(this);
                     if (model.isEmpty()) {
                         addNote(QStringLiteral("Resuming without compaction. "
                                                "The next turn will pay the full re-cache cost."),
                                 QStringLiteral("dim"));
                         doResume();
                         return;
                     }
                     addNote(QStringLiteral("compacting with <b>%1</b>…").arg(model.toHtmlEscaped()),
                             QStringLiteral("sys"));
                     m_core->call(QStringLiteral("agent.compactNow"),
                                  QJsonObject{
                                      {QStringLiteral("threadId"), tid},
                                      {QStringLiteral("model"), model},
                                  },
                                  [this, tid](const QJsonObject &res, const QJsonObject &cErr) {
                                      if (tid != m_threadId) {
                                          return;
                                      }
                                      if (!cErr.isEmpty()) {
                                          addNote(QStringLiteral("Compaction failed: %1. Resuming anyway.")
                                                      .arg(cErr.value(QStringLiteral("message"))
                                                               .toString()
                                                               .toHtmlEscaped()),
                                                  QStringLiteral("err"));
                                      } else {
                                          addNote(QStringLiteral("compacted (%1 turns, %2 bytes).")
                                                      .arg(res.value(QStringLiteral("turns")).toInt())
                                                      .arg(res.value(QStringLiteral("bodyBytes")).toInt()),
                                                  QStringLiteral("dim"));
                                      }
                                      doResume();
                                  });
                 });
}

void AgentPanel::refresh()
{
    const bool running = !m_threadId.isEmpty() && !m_dormant;
    m_sendBtn->setText(m_dormant ? QStringLiteral("Resume agent")
                                 : (running ? QStringLiteral("Send")
                                            : QStringLiteral("Start agent")));
    m_stopBtn->setEnabled(running);
    m_stopBtn->setToolTip((running && !m_idle)
                              ? QStringLiteral("Interrupt the in-flight response "
                                               "now (resumable)")
                              : QStringLiteral("Stop this agent (resumable)"));
    m_diffBtn->setEnabled(running);
    // Compact-now needs a thread on disk (running or dormant). The Hot Opus
    // menu item is the only one that further needs the thread to be live.
    if (m_compactNowBtn) {
        m_compactNowBtn->setEnabled(!m_threadId.isEmpty());
        if (auto *menu = m_compactNowBtn->menu()) {
            const auto actions = menu->actions();
            if (!actions.isEmpty()) {
                actions.first()->setEnabled(running); // "Hot Opus (live thread)"
            }
        }
    }
    if (m_compactNowMenu) {
        m_compactNowMenu->setEnabled(!m_threadId.isEmpty());
        const auto actions = m_compactNowMenu->actions();
        if (!actions.isEmpty()) {
            actions.first()->setEnabled(running); // "Hot Opus (live thread)"
        }
    }
    // The permission, isolation and effort modes are fixed once a thread exists.
    m_modeCombo->setEnabled(m_threadId.isEmpty());
    m_isolationCombo->setEnabled(m_threadId.isEmpty());
    m_effortCombo->setEnabled(m_threadId.isEmpty());

    // Offer promotion while a thread runs non-isolated in the workspace.
    m_promoteBar->setVisible(!m_threadId.isEmpty() && !m_isolated && !m_promoting);

    // "Agent Kate at work" indicator: animate while a turn is actually computing.
    m_working->setActive(running && !m_idle && m_permQueue.isEmpty());

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
    emit subtitleChanged(text);
}

// --- conversation feed ------------------------------------------------------

void AgentPanel::appendToFeed(QWidget *entry)
{
    // Insert before the trailing stretch. The scrollbar's rangeChanged handler
    // takes care of riding the bottom when m_stickBottom is true.
    m_feedLayout->insertWidget(m_feedLayout->count() - 1, entry);
}

void AgentPanel::scrollFeedToBottom()
{
    m_stickBottom = true;
    QScrollBar *bar = m_feedScroll->verticalScrollBar();
    bar->setValue(bar->maximum());
    // A second tick after the event loop has processed layout — long cards or
    // markdown reflow can push the maximum further than the value we just set.
    QTimer::singleShot(0, this, [this] {
        QScrollBar *b = m_feedScroll->verticalScrollBar();
        b->setValue(b->maximum());
    });
}

void AgentPanel::addMessageCard(const QString &role, const QString &accentHex,
                                const QString &bodyHtml)
{
    auto *card = new QFrame(m_feed);
    card->setObjectName(QStringLiteral("msgCard"));
    card->setStyleSheet(QStringLiteral(
        "QFrame#msgCard { background: palette(alternate-base); border-radius: 8px; }"));
    auto *v = new QVBoxLayout(card);
    v->setContentsMargins(12, 9, 12, 11);
    v->setSpacing(3);

    auto *roleLabel = new QLabel(role, card);
    roleLabel->setStyleSheet(
        QStringLiteral("color: %1; font-weight: bold;").arg(accentHex));
    v->addWidget(roleLabel);

    auto *bodyLabel = new QLabel(bodyHtml, card);
    bodyLabel->setTextFormat(Qt::RichText);
    bodyLabel->setWordWrap(true);
    bodyLabel->setTextInteractionFlags(Qt::TextSelectableByMouse
                                       | Qt::LinksAccessibleByMouse);
    bodyLabel->setOpenExternalLinks(true);
    // QLabel's minimumSizeHint with word-wrap includes the widest unbreakable
    // token (inline <code>, long URLs, paths in numbered list items). That
    // propagates up and prevents the panel from ever shrinking below the
    // widest line. Ignoring the horizontal sizeHint lets the parent's width
    // drive wrapping instead.
    // Horizontal Ignored: don't let widest unbreakable token pin the panel's
    // min width. Vertical Preferred (not MinimumExpanding) so the label takes
    // exactly its wrapped height — MinimumExpanding made the label gobble any
    // vertical slack and render text vcentered, looking like growing padding.
    QSizePolicy bp(QSizePolicy::Ignored, QSizePolicy::Preferred, QSizePolicy::Label);
    bp.setHeightForWidth(true);
    bodyLabel->setSizePolicy(bp);
    bodyLabel->setMinimumWidth(0);
    bodyLabel->setAlignment(Qt::AlignLeft | Qt::AlignTop);
    v->addWidget(bodyLabel);

    appendToFeed(card);
}

void AgentPanel::addNote(const QString &html, const QString &kind)
{
    auto *note = new QLabel(html, m_feed);
    note->setTextFormat(Qt::RichText);
    note->setWordWrap(true);
    QSizePolicy np(QSizePolicy::Ignored, QSizePolicy::Preferred, QSizePolicy::Label);
    np.setHeightForWidth(true);
    note->setSizePolicy(np);
    note->setMinimumWidth(0);
    note->setAlignment(Qt::AlignLeft | Qt::AlignTop);
    note->setStyleSheet(QStringLiteral("color: %1; font-size: small; padding: 1px 8px;")
                            .arg(noteColor(kind, isDark(this))));
    appendToFeed(note);
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
    if (!m_core->isConnected()) {
        // Without this guard, the message lands in the feed but the RPC is
        // dropped silently by CoreClient — the user just sees a dead chat.
        emit statusMessage(QStringLiteral("Core is not connected — restart Agent Kate"));
        addNote(QStringLiteral("Core process is not connected — the message was not sent. "
                               "Restart Agent Kate to recover."),
                QStringLiteral("err"));
        return;
    }
    m_input->clear();

    QString youLine = text.toHtmlEscaped().replace(QLatin1Char('\n'), QLatin1String("<br>"));
    if (!m_attachments.isEmpty()) {
        if (!youLine.isEmpty()) {
            youLine += QStringLiteral("<br>");
        }
        youLine += QStringLiteral("<span style='color:palette(mid)'>&#128206; %1 "
                                  "attachment(s)</span>")
                       .arg(m_attachments.size());
    }
    addMessageCard(QStringLiteral("You"),
                   isDark(this) ? QStringLiteral("#7cb7ff") : QStringLiteral("#1a5fb4"),
                   youLine);
    m_idle = false;
    m_working->setActivity(QString()); // a new turn starts in generic mode

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
                                 {QStringLiteral("isolation"),
                                  m_isolationCombo->currentData().toString()},
                                 {QStringLiteral("effort"),
                                  m_effortCombo->currentData().toString()},
                                 {QStringLiteral("attachments"), attachments}},
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         if (!error.isEmpty()) {
                             addNote(QStringLiteral("Failed to start agent: %1")
                                         .arg(error.value(QStringLiteral("message"))
                                                  .toString()
                                                  .toHtmlEscaped()),
                                     QStringLiteral("err"));
                             return;
                         }
                         m_threadId = result.value(QStringLiteral("threadId")).toString();
                         // Apply the user's chosen compaction strategy now
                         // that the thread exists on the server.
                         pushCompactStrategy();
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
    attachPaths(paths);
}

void AgentPanel::attachPaths(const QStringList &paths)
{
    if (paths.isEmpty()) {
        return;
    }
    static const QHash<QString, QString> imageTypes{
        {QStringLiteral("png"), QStringLiteral("image/png")},
        {QStringLiteral("jpg"), QStringLiteral("image/jpeg")},
        {QStringLiteral("jpeg"), QStringLiteral("image/jpeg")},
        {QStringLiteral("gif"), QStringLiteral("image/gif")},
        {QStringLiteral("webp"), QStringLiteral("image/webp")},
        {QStringLiteral("bmp"), QStringLiteral("image/bmp")}};

    for (const QString &path : paths) {
        const QFileInfo info(path);
        if (!info.exists() || info.isDir()) {
            // Directories aren't attachable as a single blob — silently skip.
            if (info.isDir()) {
                emit statusMessage(
                    QStringLiteral("Skipped %1: directories cannot be attached")
                        .arg(info.fileName()));
            }
            continue;
        }
        QFile file(path);
        if (!file.open(QIODevice::ReadOnly)) {
            emit statusMessage(QStringLiteral("Could not read %1").arg(path));
            continue;
        }
        const QByteArray bytes = file.readAll();
        const QString ext = info.suffix().toLower();

        QJsonObject att{{QStringLiteral("name"), info.fileName()},
                        {QStringLiteral("path"), info.absoluteFilePath()}};
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

void AgentPanel::dragEnterEvent(QDragEnterEvent *event)
{
    if (event->mimeData()->hasUrls()) {
        event->acceptProposedAction();
    }
}

void AgentPanel::dragMoveEvent(QDragMoveEvent *event)
{
    if (event->mimeData()->hasUrls()) {
        event->acceptProposedAction();
    }
}

void AgentPanel::dropEvent(QDropEvent *event)
{
    if (!event->mimeData()->hasUrls()) {
        return;
    }
    QStringList paths;
    const auto urls = event->mimeData()->urls();
    for (const QUrl &u : urls) {
        if (u.isLocalFile()) {
            paths << u.toLocalFile();
        }
    }
    if (!paths.isEmpty()) {
        attachPaths(paths);
        event->acceptProposedAction();
    }
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
    if (m_threadId.isEmpty() || m_dormant) {
        return;
    }
    // While a turn is in flight (generating, or paused on a tool/permission),
    // Stop is a hard interrupt: abort the response now so no more tokens are
    // billed, leaving the session resumable. When idle, it's the graceful
    // "end this agent" stop.
    const bool turnInFlight = !m_idle;
    if (turnInFlight) {
        addNote(QStringLiteral("&#9209; interrupting…"), QStringLiteral("sys"));
        m_core->call(QStringLiteral("agent.interrupt"),
                     QJsonObject{{QStringLiteral("threadId"), m_threadId}});
    } else {
        m_core->call(QStringLiteral("agent.stop"),
                     QJsonObject{{QStringLiteral("threadId"), m_threadId}});
    }
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

void AgentPanel::onPromoteClicked()
{
    if (m_threadId.isEmpty() || m_isolated || m_promoting) {
        return;
    }
    m_promoting = true;
    addNote(QStringLiteral("promoting to an isolated worktree — the agent will "
                           "restart in its own branch…"),
            QStringLiteral("sys"));
    m_core->call(QStringLiteral("agent.promote"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         m_promoting = false;
                         addNote(QStringLiteral("Could not promote: %1")
                                     .arg(error.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("err"));
                         refresh();
                     }
                 });
    refresh();
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
            addNote(QStringLiteral("&#128203; Review requested: %1")
                        .arg(params.value(QStringLiteral("summary")).toString().toHtmlEscaped()),
                    QStringLiteral("ok"));
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
        addNote(QStringLiteral("&#10067; the agent is asking a question"),
                QStringLiteral("sys"));
    } else {
        addNote(QStringLiteral("&#128274; permission requested: %1").arg(tool.toHtmlEscaped()),
                QStringLiteral("sys"));
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
    addNote(QStringLiteral("&#128274; %1 — %2")
                .arg(req.value(QStringLiteral("toolName")).toString().toHtmlEscaped(),
                     allow ? QStringLiteral("approved") : QStringLiteral("denied")),
            allow ? QStringLiteral("ok") : QStringLiteral("err"));
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

    addNote(QStringLiteral("&#10067; answered the agent's question"), QStringLiteral("ok"));

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
        // Only the init system event is worth showing in the feed.
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
        addNote(line, QStringLiteral("sys"));

    } else if (type == QLatin1String("assistant")) {
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            const QString bt = b.value(QStringLiteral("type")).toString();
            if (bt == QLatin1String("text")) {
                const QString t = b.value(QStringLiteral("text")).toString().trimmed();
                if (!t.isEmpty()) {
                    addMessageCard(QStringLiteral("Agent Kate"),
                                   isDark(this) ? QStringLiteral("#5fd3bf")
                                                : QStringLiteral("#1a7f6b"),
                                   markdownToHtml(t));
                    m_working->setActivity(QString()); // text → generic reasoning
                }
            } else if (bt == QLatin1String("tool_use")) {
                const QString name = b.value(QStringLiteral("name")).toString();
                // The permission gate and question tool are surfaced by their
                // own UI, so don't also list them as raw tool calls.
                if (name.contains(QLatin1String("request_permission"))
                    || name == QLatin1String("AskUserQuestion")) {
                    continue;
                }
                const QJsonObject input = b.value(QStringLiteral("input")).toObject();
                QString summary = permSummary(name, input).simplified();
                if (summary.length() > 96) {
                    summary = summary.left(95) + QChar(0x2026);
                }
                const QString detail = QString::fromUtf8(
                    QJsonDocument(input).toJson(QJsonDocument::Indented));
                auto *card = new ToolCard(name, summary, detail, m_feed);
                const bool show = KSharedConfig::openConfig()
                                      ->group(QStringLiteral("Agent"))
                                      .readEntry("showTools", true);
                card->setVisible(show);
                const QString id = b.value(QStringLiteral("id")).toString();
                if (!id.isEmpty()) {
                    m_toolCards.insert(id, card);
                }
                appendToFeed(card);
                m_working->setActivity(activityFor(name));
            }
        }

    } else if (type == QLatin1String("user")) {
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            if (b.value(QStringLiteral("type")).toString() == QLatin1String("tool_result")) {
                const QString id = b.value(QStringLiteral("tool_use_id")).toString();
                if (ToolCard *card = m_toolCards.value(id, nullptr)) {
                    card->setResult(toolResultText(b.value(QStringLiteral("content"))));
                }
            }
        }

    } else if (type == QLatin1String("result")) {
        const bool err = ev.value(QStringLiteral("is_error")).toBool();
        addNote(err ? QStringLiteral("✗ turn ended with an error")
                    : QStringLiteral("✓ turn complete"),
                err ? QStringLiteral("err") : QStringLiteral("ok"));
        m_idle = true;
        refresh();

    } else if (type == QLatin1String("_stderr")) {
        addNote(ev.value(QStringLiteral("text")).toString().toHtmlEscaped(),
                QStringLiteral("dim"));

    } else if (type == QLatin1String("_lifecycle")) {
        const QString phase = ev.value(QStringLiteral("phase")).toString();
        const QString detail = ev.value(QStringLiteral("detail")).toString().toHtmlEscaped();
        if (phase == QLatin1String("started")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            addNote(detail, QStringLiteral("sys"));
            refresh();
        } else if (phase == QLatin1String("resumed")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            m_dormant = false;
            m_idle = true;
            addNote(detail + QStringLiteral(" · ready for a follow-up"),
                    QStringLiteral("sys"));
            emit dormantChanged(false);
            refresh();
            // Deliver any message the human typed before pressing Resume.
            if (!m_input->toPlainText().trimmed().isEmpty() || !m_attachments.isEmpty()) {
                onSendClicked();
            }
        } else if (phase == QLatin1String("promoted")) {
            m_isolated = true;
            m_branch = ev.value(QStringLiteral("branch")).toString();
            m_promoting = false;
            addNote(detail, QStringLiteral("sys"));
            refresh();
        } else if (phase == QLatin1String("error")) {
            addNote(QStringLiteral("agent failed: %1").arg(detail), QStringLiteral("err"));
            m_idle = false;
            m_promoting = false;
            if (!m_dormant) {
                m_threadId.clear(); // a fresh start failed — back to a blank panel
            }
            refresh();
        } else if (phase == QLatin1String("exited")
                   || phase == QLatin1String("interrupted")) {
            const bool wasInterrupt = phase == QLatin1String("interrupted");
            addNote(wasInterrupt
                        ? QStringLiteral("&#9209; stopped (resumable) — send a "
                                         "follow-up to continue this session")
                        : QStringLiteral("agent exited: %1").arg(detail),
                    wasInterrupt ? QStringLiteral("sys") : QStringLiteral("dim"));
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
