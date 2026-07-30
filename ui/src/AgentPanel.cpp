#include "AgentPanel.h"
#include "AgentCardDelegate.h"
#include "AgentChatHelpers.h"
#include "AttachmentBuilder.h"
#include "ImageView.h"
#include "ProviderConfig.h"
#include "ToolInspectorDialog.h"
#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "WorkflowMonitor.h"
#include "WorkflowMonitorDialog.h"
#include "ipc/CoreClient.h"
#include "shell/FlowLayout.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KMessageWidget>
#include <KSharedConfig>

#include <QAbstractButton>
#include <QClipboard>
#include <QCryptographicHash>
#include <QDesktopServices>
#include <QGuiApplication>
#include <QLineEdit>
#include <QLocale>
#include <QPixmap>
#include <QRegularExpression>
#include <QShortcut>
#include <QDateTime>
#include <QTime>
#include <QCheckBox>
#include <QComboBox>
#include <QDialog>
#include <QDialogButtonBox>
#include <QDir>
#include <QDragEnterEvent>
#include <QDragLeaveEvent>
#include <QDragMoveEvent>
#include <QDropEvent>
#include <QEvent>
#include <QMimeData>
#include <QSet>
#include <QUrl>
#include <QFile>
#include <QFileDialog>
#include <QFileInfo>
#include <QFormLayout>
#include <QFrame>
#include <QHBoxLayout>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonDocument>
#include <QKeyEvent>
#include <QLabel>
#include <QLayout>
#include <QListView>
#include <QMouseEvent>
#include <QPaintEvent>
#include <QPainter>
#include <QPalette>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QRadioButton>
#include <QAbstractItemView>
#include <QScrollBar>
#include <QSignalBlocker>
#include <QStyleOptionViewItem>
#include <QMenu>
#include <QMessageBox>
#include <QPointer>
#include <QTextDocument>
#include <QTimer>
#include <QToolButton>
#include <QVBoxLayout>
#include <QWidgetAction>

namespace {
// Custom drag MIME carrying per-hit line ranges, mirrored in SearchPanel.cpp.
constexpr char kAttachMime[] = "application/x-agentkate-attachment+json";

// Tool-result clipping. The transcript shows the first kToolResultDisplayClip
// chars inline and reveals more via "Show full output". The retained copy is
// itself capped at kToolResultStoreCap so a single huge result (a big Read, a
// verbose command, an AT-SPI page dump) cannot grow the in-RAM transcript
// without bound — the on-disk transcript always keeps the true full text.
constexpr int kToolResultDisplayClip = 4000;
constexpr int kToolResultStoreCap = 128 * 1024;

bool isDark(const QWidget *w)
{
    return w->palette().color(QPalette::Base).lightness() < 128;
}

// (Note colouring now lives in TranscriptDelegate, which paints the feed —
// see noteColor() there.)
//
// The pure stream-json formatting helpers (markdownToHtml, permSummary,
// toolResultText, activityFor) now live in AgentChatHelpers; the resume
// recovery dialog + strategy mapping live there too.

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
            // The message text only rotates every 48 ticks; on those ticks
            // repaint the whole widget, otherwise invalidate just the small
            // spinner rect so the wide message isn't re-rendered every 70ms.
            if (++m_ticks % 48 == 0) {
                ++m_genericIndex;
                update();
            } else {
                update(spinnerRect());
            }
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
        syncTimer();
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
    // Pause the spinner while the panel is off-screen (backgrounded, or not the
    // current stack page): a hidden widget can't show its animation, so ticking
    // 14×/s only to repaint nothing is pure waste. Resume on show if still active.
    void showEvent(QShowEvent *e) override
    {
        QWidget::showEvent(e);
        syncTimer();
    }
    void hideEvent(QHideEvent *e) override
    {
        QWidget::hideEvent(e);
        syncTimer();
    }

    void paintEvent(QPaintEvent *) override
    {
        QPainter p(this);
        p.setRenderHint(QPainter::Antialiasing);
        // This spinner is shown only while a turn is actively computing, so it
        // wears the active-agent colour from the theme.
        const QColor accent = ThemeManager::palette().agentRunning;

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
    // The bounding box of the rotating arc (matching paintEvent's geometry),
    // padded for the pen width — the only region the per-tick repaint touches.
    QRect spinnerRect() const
    {
        const int d = 15;
        const int cx = 9 + d / 2;
        const int cy = height() / 2;
        const int pad = 3; // pen width / round caps / antialiasing
        return QRect(cx - d / 2 - pad, cy - d / 2 - pad, d + 2 * pad, d + 2 * pad);
    }

    // Run the animation timer only while active and actually on-screen.
    void syncTimer()
    {
        if (m_active && isVisible()) {
            if (!m_timer->isActive()) {
                m_timer->start();
            }
        } else {
            m_timer->stop();
        }
    }

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
    // Rich-text header (can't be an ElidingLabel): let it wrap and shrink so a
    // long branch/cost/token suffix reflows onto a second line instead of
    // pinning the panel's minimum width.
    m_header->setWordWrap(true);
    m_header->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Preferred);
    m_header->setStyleSheet(
        QStringLiteral("padding: 9px 12px; border-bottom: 1px solid palette(mid);"));

    // --- conversation feed: a virtualized model/view (plan 10 phase 2) -----
    // A QListView over a TranscriptModel, painted by a TranscriptDelegate that
    // draws each row's cached HTML via a QTextDocument with a per-(row,width)
    // height cache. The view only measures the rows it shows, so a resize costs
    // O(visible rows) instead of relaying out the whole transcript.
    m_model = new TranscriptModel(this);
    m_delegate = new TranscriptDelegate(this);
    m_view = new QListView(this);
    m_view->setModel(m_model);
    m_view->setItemDelegate(m_delegate);
    m_view->setUniformItemSizes(false);
    m_view->setWordWrap(true);
    m_view->setSelectionMode(QAbstractItemView::NoSelection);
    m_view->setEditTriggers(QAbstractItemView::NoEditTriggers);
    m_view->setFocusPolicy(Qt::NoFocus);
    m_view->setFrameShape(QFrame::NoFrame);
    m_view->setHorizontalScrollBarPolicy(Qt::ScrollBarAlwaysOff);
    m_view->setVerticalScrollMode(QAbstractItemView::ScrollPerPixel);
    m_view->setResizeMode(QListView::Adjust); // re-layout rows on viewport resize
    m_view->setMouseTracking(true);
    m_view->setContextMenuPolicy(Qt::CustomContextMenu);
    m_view->viewport()->setAttribute(Qt::WA_Hover);
    connect(m_view, &QListView::customContextMenuRequested, this,
            [this](const QPoint &pos) {
                const QModelIndex idx = m_view->indexAt(pos);
                if (idx.isValid()) {
                    showFeedContextMenu(idx, m_view->viewport()->mapToGlobal(pos));
                }
            });

    // --- in-place selectable text overlay (plan 13 phase 1) ----------------
    // A click on a message body opens a persistent, frameless QTextBrowser over
    // that row's text so an arbitrary substring can be selected and copied. The
    // delegate paints the row and the overlay from the same document setup, so
    // opening it causes no visual jump. Only one overlay is open at a time.
    connect(m_delegate, &TranscriptDelegate::messageBodyClicked, this,
            &AgentPanel::openSelectionOverlay);
    connect(m_delegate, &TranscriptDelegate::editorCreated, this,
            [this](QWidget *editor) {
                m_selectionEditor = editor;
                editor->installEventFilter(this); // Esc to dismiss
                editor->setFocus(Qt::MouseFocusReason);
            });
    connect(m_delegate, &TranscriptDelegate::anchorActivated, this,
            [](const QString &href) {
                if (!href.isEmpty()) {
                    QDesktopServices::openUrl(QUrl(href));
                }
            });
    // A click on an attachment chip under a You message opens that file.
    connect(m_delegate, &TranscriptDelegate::attachmentActivated, this,
            &AgentPanel::openAttachment);
    // The tool card's "open in inspector" glyph opens the full-size modal.
    connect(m_delegate, &TranscriptDelegate::inspectToolRequested, this,
            &AgentPanel::openToolInspector);
    // Close the overlay when its row's data changes (streaming/mutation) or the
    // model resets, so a stale editor never lingers over a re-laid-out row.
    connect(m_model, &QAbstractItemModel::dataChanged, this,
            [this](const QModelIndex &tl, const QModelIndex &br) {
                if (m_selectionRow.isValid() && m_selectionRow.row() >= tl.row()
                    && m_selectionRow.row() <= br.row()) {
                    closeSelectionOverlay();
                }
            });
    connect(m_model, &QAbstractItemModel::modelReset, this,
            &AgentPanel::closeSelectionOverlay);
    connect(m_model, &QAbstractItemModel::rowsAboutToBeRemoved, this,
            &AgentPanel::closeSelectionOverlay);

    // Sticky-bottom: the feed auto-scrolls to keep the latest entry in view.
    // Scrolling upward releases the stick; scrolling back to the bottom reclaims
    // it. rangeChanged covers the case where a row's measured height arrives a
    // frame after insertion (long tool output, markdown reflow, …).
    QScrollBar *bar = m_view->verticalScrollBar();
    connect(bar, &QScrollBar::valueChanged, this, [this, bar](int v) {
        m_stickBottom = (v >= bar->maximum() - 48);
        updateJumpButton();
    });
    connect(bar, &QScrollBar::rangeChanged, this, [this, bar](int, int max) {
        if (m_stickBottom) {
            bar->setValue(max);
        }
        updateJumpButton();
    });

    // --- "jump to latest" floating button over the feed viewport -----------
    m_jumpBtn = new QToolButton(m_view->viewport());
    m_jumpBtn->setIcon(QIcon::fromTheme(QStringLiteral("go-down")));
    m_jumpBtn->setToolTip(i18n("Jump to latest"));
    m_jumpBtn->setCursor(Qt::PointingHandCursor);
    m_jumpBtn->setAutoRaise(false);
    m_jumpBtn->setVisible(false);
    m_view->viewport()->installEventFilter(this); // reposition on resize
    connect(m_jumpBtn, &QToolButton::clicked, this, [this] {
        m_jumpUnread = false;
        scrollFeedToBottom();
        updateJumpButton();
    });

    // Settle-time exact re-measure after an interactive resize: during the drag
    // every row hands back a cheap cached estimate; once it stops, refine just
    // the visible rows so heights are pixel-correct without an O(N) layout storm.
    m_resizeSettle = new QTimer(this);
    m_resizeSettle->setSingleShot(true);
    m_resizeSettle->setInterval(80);
    connect(m_resizeSettle, &QTimer::timeout, this, &AgentPanel::remeasureVisibleRows);

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
        QStringLiteral("Working directly in your files — changes apply straight away."),
        m_promoteBar);
    promoteLabel->setWordWrap(true);
    m_promoteBtn = new QPushButton(QStringLiteral("Move to a private copy"), m_promoteBar);
    m_promoteBtn->setToolTip(QStringLiteral(
        "Move this agent's work into a private copy (a git worktree) so its "
        "changes stay separate until you choose to keep them."));
    m_promoteBtn->setCursor(Qt::PointingHandCursor);
    auto *promoteLayout = new QHBoxLayout(m_promoteBar);
    promoteLayout->setContentsMargins(10, 6, 10, 6);
    promoteLayout->addWidget(promoteLabel, 1);
    promoteLayout->addWidget(m_promoteBtn);

    m_input = new QPlainTextEdit(this);
    m_input->setFixedHeight(94);
    m_input->installEventFilter(this); // for the configurable send key
    // QPlainTextEdit delivers drops to its viewport and would otherwise insert
    // a dropped file's path as text; filter the viewport so file/attachment
    // drops attach as context instead (see eventFilter).
    m_input->viewport()->installEventFilter(this);

    // Debounced draft autosave: persist the composer text so a closed/reopened
    // panel (or a crash) doesn't lose an unsent message.
    m_draftTimer = new QTimer(this);
    m_draftTimer->setSingleShot(true);
    m_draftTimer->setInterval(400);
    connect(m_draftTimer, &QTimer::timeout, this, &AgentPanel::saveDraft);
    connect(m_input, &QPlainTextEdit::textChanged, this,
            [this] { m_draftTimer->start(); });

    // --- in-conversation find bar (hidden until Ctrl+F) --------------------
    m_findBar = new QFrame(this);
    m_findBar->setObjectName(QStringLiteral("findBar"));
    m_findBar->setVisible(false);
    m_findEdit = new QLineEdit(m_findBar);
    m_findEdit->setPlaceholderText(i18n("Find in conversation…"));
    m_findEdit->setClearButtonEnabled(true);
    auto *findPrev = new QToolButton(m_findBar);
    findPrev->setAutoRaise(true);
    findPrev->setIcon(QIcon::fromTheme(QStringLiteral("go-up")));
    findPrev->setToolTip(i18n("Previous match"));
    auto *findNext = new QToolButton(m_findBar);
    findNext->setAutoRaise(true);
    findNext->setIcon(QIcon::fromTheme(QStringLiteral("go-down")));
    findNext->setToolTip(i18n("Next match"));
    m_findStatus = new QLabel(m_findBar);
    m_findStatus->setStyleSheet(QStringLiteral("color: palette(mid); font-size: small;"));
    auto *findClose = new QToolButton(m_findBar);
    findClose->setAutoRaise(true);
    findClose->setIcon(QIcon::fromTheme(QStringLiteral("dialog-close")));
    findClose->setToolTip(i18n("Close find bar"));
    auto *findLayout = new QHBoxLayout(m_findBar);
    findLayout->setContentsMargins(6, 4, 6, 4);
    findLayout->setSpacing(4);
    findLayout->addWidget(m_findEdit, 1);
    findLayout->addWidget(m_findStatus);
    findLayout->addWidget(findPrev);
    findLayout->addWidget(findNext);
    findLayout->addWidget(findClose);
    connect(m_findEdit, &QLineEdit::textChanged, this, [this] { runFind(0); });
    connect(m_findEdit, &QLineEdit::returnPressed, this, [this] { runFind(1); });
    connect(findNext, &QToolButton::clicked, this, [this] { runFind(1); });
    connect(findPrev, &QToolButton::clicked, this, [this] { runFind(-1); });
    connect(findClose, &QToolButton::clicked, this, [this] { toggleFindBar(); });
    // Panel-local Find shortcut: scoped to this widget tree so it never
    // collides with the project-wide SearchPanel Find on the main window.
    auto *findSc = new QShortcut(QKeySequence::Find, this);
    findSc->setContext(Qt::WidgetWithChildrenShortcut);
    connect(findSc, &QShortcut::activated, this, [this] { toggleFindBar(); });
    auto *escSc = new QShortcut(QKeySequence(Qt::Key_Escape), m_findBar);
    escSc->setContext(Qt::WidgetWithChildrenShortcut);
    connect(escSc, &QShortcut::activated, this, [this] {
        if (m_findBar->isVisible()) {
            toggleFindBar();
        }
    });

    m_modeCombo = new QComboBox(this);
    m_modeCombo->addItem(QStringLiteral("Apply edits automatically"), QStringLiteral("acceptEdits"));
    m_modeCombo->addItem(QStringLiteral("Ask before each step"), QStringLiteral("default"));
    m_modeCombo->addItem(QStringLiteral("Work freely"), QStringLiteral("auto"));
    m_modeCombo->addItem(QStringLiteral("Expert — never ask"), QStringLiteral("bypassPermissions"));
    m_modeCombo->setToolTip(QStringLiteral(
        "How much the agent checks with you before it acts. Fixed once it starts."));
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
    m_isolationCombo->addItem(QStringLiteral("Automatic"), QStringLiteral("auto"));
    m_isolationCombo->addItem(QStringLiteral("In a private copy (sandbox)"),
                              QStringLiteral("isolated"));
    m_isolationCombo->addItem(QStringLiteral("Directly in my files"),
                              QStringLiteral("workspace"));
    m_isolationCombo->setToolTip(QStringLiteral(
        "Whether the agent works on a private copy of your project or directly\n"
        "in your files. Fixed once it starts:\n"
        "• Automatic — a private copy when the project is a git repo, else your files\n"
        "• In a private copy — always its own sandbox; merge it back when you're happy\n"
        "• Directly in my files — no sandbox; changes land in your project immediately"));
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
    m_effortCombo->addItem(QStringLiteral("Default"), QString());
    m_effortCombo->addItem(QStringLiteral("Low"), QStringLiteral("low"));
    m_effortCombo->addItem(QStringLiteral("Medium"), QStringLiteral("medium"));
    m_effortCombo->addItem(QStringLiteral("High"), QStringLiteral("high"));
    m_effortCombo->addItem(QStringLiteral("Extra-high"), QStringLiteral("xhigh"));
    m_effortCombo->addItem(QStringLiteral("Maximum"), QStringLiteral("max"));
    m_effortCombo->setToolTip(QStringLiteral(
        "How long the agent thinks before it acts. Higher is more thorough but\n"
        "slower. Default leaves the model's own configured level untouched.\n"
        "Fixed once it starts."));
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

    // Provider selector. Routes this agent's `claude` harness at a third-party
    // Anthropic-compatible API (Fireworks, OpenRouter, …) instead of Anthropic's
    // own. "Claude (direct)" (the default) injects nothing — identical to before.
    // Fixed once the agent starts, like the other setup combos. Profiles are
    // managed in Options → Configure API Providers…. The model combo's contents
    // follow this choice (Claude tiers vs the provider's own model ids).
    m_providerCombo = new QComboBox(this);
    m_providerCombo->setToolTip(QStringLiteral(
        "Which API endpoint this agent talks to, fixed once it starts.\n"
        "Claude (direct) uses Anthropic. Other entries route the harness at a\n"
        "third-party Anthropic-compatible API with your stored key.\n"
        "Manage these in Options ▸ Configure API Providers…"));
    {
        const QList<ProviderProfile> profiles = ProviderStore::load();
        for (const ProviderProfile &p : profiles) {
            m_providerCombo->addItem(p.name, p.id);
        }
        const QString saved = KSharedConfig::openConfig()
                                  ->group(QStringLiteral("Agent"))
                                  .readEntry("provider", ProviderStore::directId());
        const int savedIdx = m_providerCombo->findData(saved);
        if (savedIdx >= 0) {
            m_providerCombo->setCurrentIndex(savedIdx);
        }
    }
    connect(m_providerCombo, &QComboBox::currentIndexChanged, this, [this] {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("provider", m_providerCombo->currentData().toString());
        rebuildModelCombo();
    });

    // Backend selector: which agent harness runs this thread — Claude Code (the
    // default, unchanged behaviour) or Kimi Code. Fixed once it starts, like the
    // other setup combos. Choosing Kimi disables the provider/effort pickers
    // (unsupported there) and turns the model combo into free text.
    m_backendCombo = new QComboBox(this);
    m_backendCombo->addItem(QStringLiteral("Claude Code"), QStringLiteral("claude"));
    m_backendCombo->addItem(QStringLiteral("Kimi Code"), QStringLiteral("kimi"));
    m_backendCombo->setToolTip(QStringLiteral(
        "Which agent program runs this thread, fixed once it starts.\n"
        "Kimi Code needs the `kimi` CLI installed; provider routing, thinking\n"
        "effort, when-to-ask, forking and compaction don't apply to Kimi\n"
        "threads — Kimi always asks before gated actions."));
    // Sticky: the last choice becomes the default for the next agent. Set before
    // connecting so restoring "kimi" doesn't fire the handler mid-construction.
    {
        const QString saved = KSharedConfig::openConfig()
                                  ->group(QStringLiteral("Agent"))
                                  .readEntry("backend", QStringLiteral("claude"));
        const int savedIdx = m_backendCombo->findData(saved);
        if (savedIdx >= 0) {
            m_backendCombo->setCurrentIndex(savedIdx);
        }
    }
    connect(m_backendCombo, &QComboBox::currentIndexChanged, this, [this] {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("backend", m_backendCombo->currentData().toString());
        rebuildModelCombo();
        refresh();
    });

    // Model selector. For Claude direct each item carries a tier token the core
    // resolves to a concrete --model id (its resolveModel is the single source of
    // truth, so the UI never hard-codes versioned model strings); for a provider
    // the items are that provider's own model ids, sent verbatim. An empty token
    // passes no --model flag. Fixed once the agent starts. rebuildModelCombo()
    // populates it to match the selected provider.
    m_modelCombo = new QComboBox(this);
    m_modelCombo->setToolTip(QStringLiteral(
        "Model for this agent, fixed once it starts.\n"
        "Default leaves the provider's own configured/main model untouched."));
    connect(m_modelCombo, &QComboBox::currentIndexChanged, this, [this] {
        // Only the Claude-direct tier choice is sticky; a provider's model ids
        // must not be persisted as the Claude model token, and neither must the
        // free-text Kimi model (an editable combo).
        if (m_modelCombo->isEditable()) {
            return;
        }
        if (m_providerCombo &&
            m_providerCombo->currentData().toString() != ProviderStore::directId()) {
            return;
        }
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("model", m_modelCombo->currentData().toString());
    });
    rebuildModelCombo();

    // Cowork desktop access. Wiring the agentkate-cowork MCP server into the agent is
    // fixed at start (it is written into the MCP config when the claude process
    // launches), so this is a start-time choice like the combos above. Deliberately
    // NOT sticky: standing desktop access by default would be a footgun — the user
    // opts in per agent. Making the tools available is harmless on its own; every
    // action is still gated by the consent prompts and the Cowork panel toggles.
    m_coworkCheck = new QCheckBox(QStringLiteral("See && control my desktop (Cowork)"), this);
    m_coworkCheck->setToolTip(QStringLiteral(
        "Give this agent the Cowork desktop tools (see windows, screenshot, read the\n"
        "screen, click controls, type) from its very first message. Fixed once the\n"
        "agent starts. Every action still needs your consent or a Cowork panel toggle."));

    // Compaction strategy. Keeping a thread resumable cheaply needs a
    // condensed summary on disk — otherwise the next resume re-caches the
    // whole transcript. The five options encode (when, model) combos; the
    // strip flag asks LLM-based compactors to pre-trim noisy events.
    m_compactCombo = new QComboBox(this);
    m_compactCombo->addItem(QStringLiteral("When it stops — best quality"),
                            QStringLiteral("exit_opus_hot"));
    m_compactCombo->addItem(QStringLiteral("When it stops — cheaper"),
                            QStringLiteral("exit_sonnet_cold"));
    m_compactCombo->addItem(QStringLiteral("On resume — balanced"),
                            QStringLiteral("resume_sonnet_cold"));
    m_compactCombo->addItem(QStringLiteral("On resume — cheapest"),
                            QStringLiteral("resume_haiku_cold"));
    m_compactCombo->addItem(QStringLiteral("On resume — on this computer"),
                            QStringLiteral("resume_local"));
    m_compactCombo->setToolTip(QStringLiteral(
        "When the agent's long conversation gets summarized so it stays cheap to\n"
        "continue later. \"Best quality\" summarizes the moment it stops; the\n"
        "\"on resume\" options defer the work until you reopen it. \"On this\n"
        "computer\" is free and runs locally (keeps decisions, drops tool output)."));
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
    m_compactStrip = new QCheckBox(QStringLiteral("Trim noisy logs first"), this);
    m_compactStrip->setToolTip(QStringLiteral(
        "Drop low-value events (stale file reads, lifecycle noise) before the\n"
        "summary is made, for a tighter result. No effect on the on-this-computer\n"
        "option."));
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

    // Attachment chip bar — hidden until files are attached. A FlowLayout so the
    // chips wrap onto further rows when the panel is dragged narrow.
    m_attachBar = new QWidget(this);
    m_attachLayout = new FlowLayout(m_attachBar, 0, 6, 6);
    m_attachBar->setVisible(false);

    // Inline banner explaining why a dropped/attached file was rejected. More
    // visible and persistent than a status-bar toast, and dismissable.
    m_attachNotice = new KMessageWidget(this);
    m_attachNotice->setMessageType(KMessageWidget::Information);
    m_attachNotice->setIcon(QIcon::fromTheme(QStringLiteral("dialog-information")));
    m_attachNotice->setCloseButtonVisible(true);
    m_attachNotice->setWordWrap(true);
    m_attachNotice->setVisible(false);

    // Queue chip bar — shows follow-ups typed while a turn is in progress.
    // Hidden until something is queued. Each chip removes its message on click.
    m_queueBar = new QFrame(this);
    m_queueLayout = new FlowLayout(m_queueBar, 0, 6, 6);
    m_queueBar->setVisible(false);

    // Workflow chip — appears once this thread launches a background Workflow.
    // Opens the dedicated monitor so the fan-out run can be watched without
    // hunting for its tool row; its label tracks the run's live state.
    m_workflowBar = new QFrame(this);
    auto *workflowLayout = new FlowLayout(m_workflowBar, 0, 6, 6);
    m_workflowChip = new QPushButton(QStringLiteral("⧉ Workflow"), m_workflowBar);
    m_workflowChip->setCursor(Qt::PointingHandCursor);
    m_workflowChip->setToolTip(QStringLiteral(
        "A background workflow launched by this agent — click to watch its "
        "sub-agents and progress."));
    connect(m_workflowChip, &QPushButton::clicked, this,
            &AgentPanel::openWorkflowMonitor);
    workflowLayout->addWidget(m_workflowChip);
    m_workflowBar->setVisible(false);

    m_attachBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("mail-attachment")), QStringLiteral("Attach…"), this);
    m_attachBtn->setCursor(Qt::PointingHandCursor);
    m_diffBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("vcs-diff")), QStringLiteral("Changes"), this);
    m_diffBtn->setCursor(Qt::PointingHandCursor);
    // "Fork…" — continue this conversation on a different model or effort in a
    // brand-new agent, keeping the full context. Enabled once a session exists.
    m_forkBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("edit-copy")), QStringLiteral("Fork…"), this);
    m_forkBtn->setCursor(Qt::PointingHandCursor);
    m_forkBtn->setToolTip(QStringLiteral(
        "Continue this conversation as a new agent on a different model or "
        "thinking effort, keeping the full context. The original is untouched."));
    // "Stop & close" is the terminal action: it summarises the conversation and
    // closes the agent (archived — reversible from the Sessions browser). To just
    // cancel the current response and keep going, use Interrupt instead.
    m_stopBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("process-stop")),
        QStringLiteral("Stop && close"), this);
    m_stopBtn->setCursor(Qt::PointingHandCursor);
    // Interrupt is the prominent action while a turn runs: it cancels the
    // in-flight response now (no more tokens billed) while keeping the session
    // hot, so you can type a new message straight away to redirect the agent.
    // Shown only while a turn is running (Esc also fires it — see the composer).
    m_interruptBtn = new QPushButton(
        QIcon::fromTheme(QStringLiteral("media-playback-stop")),
        QStringLiteral("Interrupt"), this);
    m_interruptBtn->setCursor(Qt::PointingHandCursor);
    m_interruptBtn->setToolTip(QStringLiteral(
        "Cancel the in-flight response now (Esc), keeping the session — then type "
        "a new message to redirect the agent."));
    m_interruptBtn->hide();
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
        form->addRow(QStringLiteral("Agent backend"), m_backendCombo);
        form->addRow(QStringLiteral("When to ask"), m_modeCombo);
        form->addRow(QStringLiteral("Where it works"), m_isolationCombo);
        form->addRow(QStringLiteral("Thinking effort"), m_effortCombo);
        form->addRow(QStringLiteral("AI provider"), m_providerCombo);
        form->addRow(QStringLiteral("Model"), m_modelCombo);
        form->addRow(QStringLiteral("Desktop access"), m_coworkCheck);
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
        "How this agent works — what it's allowed to do, where it works, how hard\n"
        "it thinks, and which model. These are fixed once the agent starts."));
    setupBtn->setMenu(buildSetupMenu());

    // "Compaction ▾" — strategy + strip live + a "Compact now" submenu for
    // one-shot runs. Replaces the standalone combo/checkbox/compact-now trio.
    auto buildCompactionMenu = [this] {
        auto *menu = new QMenu(this);
        auto *panel = new QWidget(menu);
        auto *form = new QFormLayout(panel);
        form->setContentsMargins(10, 8, 10, 8);
        form->addRow(QStringLiteral("When to summarize"), m_compactCombo);
        form->addRow(QString(), m_compactStrip);
        auto *panelAction = new QWidgetAction(menu);
        panelAction->setDefaultWidget(panel);
        menu->addAction(panelAction);
        menu->addSeparator();
        auto *nowMenu = menu->addMenu(QStringLiteral("Summarize now"));
        m_compactNowMenu = nowMenu; // kept so updateActionStates can disable it
        auto add = [this, nowMenu](const QString &label, const QString &token) {
            QAction *a = nowMenu->addAction(label);
            connect(a, &QAction::triggered, this, [this, token] { runCompactNow(token); });
            return a;
        };
        add(QStringLiteral("Best quality, on the live agent"), QStringLiteral("hot"));
        nowMenu->addSeparator();
        add(QStringLiteral("High quality (Opus)"), QStringLiteral("opus"));
        add(QStringLiteral("Balanced (Sonnet)"), QStringLiteral("sonnet"));
        add(QStringLiteral("Cheapest (Haiku)"), QStringLiteral("haiku"));
        add(QStringLiteral("On this computer"), QStringLiteral("local"));
        return menu;
    };
    auto *compactionBtn = new QToolButton(this);
    compactionBtn->setText(QStringLiteral("Memory"));
    compactionBtn->setIcon(QIcon::fromTheme(QStringLiteral("edit-clear-history")));
    compactionBtn->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    compactionBtn->setPopupMode(QToolButton::InstantPopup);
    compactionBtn->setCursor(Qt::PointingHandCursor);
    compactionBtn->setToolTip(QStringLiteral(
        "How this agent's long conversation is summarized so it stays affordable\n"
        "to continue later — plus a one-shot \"Summarize now\"."));
    compactionBtn->setMenu(buildCompactionMenu());

    // The standalone Compact-now button is now folded into the Compaction
    // menu, but its QToolButton is still constructed above for compatibility
    // with the existing enable/disable wiring. Hide it from the toolbar.
    m_compactNowBtn->hide();

    // FlowLayout so the toolbar's buttons wrap onto a second row when the panel
    // is dragged narrow instead of clipping. No stretch — a flow layout has none.
    auto *buttons = new FlowLayout(0, 6, 6);
    buttons->addWidget(setupBtn);
    buttons->addWidget(compactionBtn);
    buttons->addWidget(m_attachBtn);
    buttons->addWidget(m_diffBtn);
    buttons->addWidget(m_forkBtn);
    // "Stop & close" sits with the setup/config group — it's the deliberate,
    // less-frequent end-this-agent action. Interrupt is the prominent in-flight
    // control, grouped next to Send.
    buttons->addWidget(m_stopBtn);
    buttons->addWidget(m_interruptBtn);
    buttons->addWidget(m_sendBtn);

    auto *body = new QVBoxLayout;
    body->setContentsMargins(12, 12, 12, 12);
    body->setSpacing(10);
    body->addWidget(m_findBar);
    body->addWidget(m_view, 1);
    body->addWidget(m_working);
    body->addWidget(m_permBar);
    body->addWidget(m_questionBox);
    body->addWidget(m_promoteBar);
    body->addWidget(m_queueBar);
    body->addWidget(m_workflowBar);
    body->addWidget(m_attachNotice);
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
    connect(m_interruptBtn, &QPushButton::clicked, this, &AgentPanel::onInterruptClicked);
    connect(m_diffBtn, &QPushButton::clicked, this, &AgentPanel::onChangesClicked);
    connect(m_forkBtn, &QPushButton::clicked, this, [this] { Q_EMIT forkRequested(); });
    connect(m_promoteBtn, &QPushButton::clicked, this, &AgentPanel::onPromoteClicked);
    connect(m_attachBtn, &QPushButton::clicked, this, &AgentPanel::onAttachClicked);
    connect(m_permAllow, &QPushButton::clicked, this, [this] { answerPermission(true); });
    connect(m_permDeny, &QPushButton::clicked, this, [this] { answerPermission(false); });
    // Queued so a notification can never be delivered re-entrantly while this
    // panel is being torn down (deleteLater'd on agent/project removal). Qt
    // drops any still-queued events after the receiver is destroyed.
    connect(m_core, &CoreClient::notification, this, &AgentPanel::onNotification,
            Qt::QueuedConnection);

    applyChatSettings();
    refresh();
}

AgentPanel::~AgentPanel()
{
    // Closing a panel ends its agent so the core does not keep it running.
    // A dormant thread has no live process — leave it for a later resume.
    if (!m_threadId.isEmpty() && !m_dormant && m_core->isConnected()) {
        m_core->call(QStringLiteral("agent.stop"),
                     QJsonObject{{QStringLiteral("threadId"), m_threadId}},
                     nullptr, this);
    }
}

void AgentPanel::setWorkspace(const QString &path)
{
    m_workspace = path;
    restoreDraft(); // recover an unsent composer draft for this workspace/thread
    refresh();
}

QString AgentPanel::currentModel() const
{
    if (!m_modelCombo) {
        return QString();
    }
    if (!m_modelCombo->isEditable()) {
        return m_modelCombo->currentData().toString();
    }
    // The Kimi backend's combo is editable: a picked dropdown item shows its
    // display label ("K3 — kimi-code/k3") but must send its data value; text
    // that matches no item is a hand-typed model id, sent verbatim.
    const int idx = m_modelCombo->currentIndex();
    if (idx >= 0 && m_modelCombo->currentText() == m_modelCombo->itemText(idx)) {
        return m_modelCombo->itemData(idx).toString();
    }
    return m_modelCombo->currentText().trimmed();
}

QString AgentPanel::currentEffort() const
{
    return m_effortCombo ? m_effortCombo->currentData().toString() : QString();
}

void AgentPanel::preselectBackend(const QString &backend)
{
    if (backend.isEmpty() || !m_threadId.isEmpty()) {
        return; // combo is frozen once a thread exists
    }
    const int idx = m_backendCombo->findData(backend);
    if (idx >= 0) {
        m_backendCombo->setCurrentIndex(idx);
    }
}

void AgentPanel::preselectModel(const QString &modelId)
{
    if (modelId.isEmpty() || !m_threadId.isEmpty()) {
        return; // combo is frozen once a thread exists
    }
    const int idx = m_modelCombo->findData(modelId);
    if (idx >= 0) {
        m_modelCombo->setCurrentIndex(idx);
    }
}

void AgentPanel::preselectIsolation(const QString &isolation)
{
    if (isolation.isEmpty() || !m_threadId.isEmpty()) {
        return;
    }
    const int idx = m_isolationCombo->findData(isolation);
    if (idx >= 0) {
        m_isolationCombo->setCurrentIndex(idx);
    }
}

void AgentPanel::preselectPermission(const QString &mode)
{
    if (mode.isEmpty() || !m_threadId.isEmpty()) {
        return;
    }
    const int idx = m_modeCombo->findData(mode);
    if (idx >= 0) {
        m_modeCombo->setCurrentIndex(idx);
    }
}

void AgentPanel::preselectEffort(const QString &effort)
{
    if (!m_threadId.isEmpty()) {
        return;
    }
    const int idx = m_effortCombo->findData(effort);
    if (idx >= 0) {
        m_effortCombo->setCurrentIndex(idx);
    }
}

void AgentPanel::setComposerText(const QString &text)
{
    if (text.isEmpty() || !m_input) {
        return;
    }
    m_input->setPlainText(text);
    m_input->setFocus();
}

void AgentPanel::rebuildModelCombo()
{
    if (!m_modelCombo) {
        return; // provider combo may fire before the model combo is built
    }
    const bool kimi = m_backendCombo
        && m_backendCombo->currentData().toString() == QLatin1String("kimi");

    QSignalBlocker block(m_modelCombo);
    m_modelCombo->clear();

    if (kimi) {
        // Kimi takes an optional free-text model id (empty = the CLI's own
        // configured default). Once a kimi session has run, the CLI's real
        // model list (discovered from the handshake and persisted from the
        // init event) fills the dropdown; the combo stays editable so a new
        // id can still be typed before any session existed.
        m_modelCombo->setEditable(true);
        m_modelCombo->lineEdit()->clear();
        m_modelCombo->lineEdit()->setPlaceholderText(
            i18n("kimi default (e.g. kimi-code/kimi-for-coding)"));
        const QStringList models = KSharedConfig::openConfig()
                                       ->group(QStringLiteral("Agent"))
                                       .readEntry("kimiOpt-model", QStringList());
        for (const QString &entry : models) {
            const QString value = entry.section(QLatin1Char('|'), 0, 0);
            const QString name = entry.section(QLatin1Char('|'), 1);
            if (!value.isEmpty()) {
                m_modelCombo->addItem(
                    name.isEmpty() ? value
                                   : QStringLiteral("%1 — %2").arg(name, value),
                    value);
            }
        }
        m_modelCombo->setCurrentIndex(-1);
        m_modelCombo->lineEdit()->clear();
        return;
    }
    m_modelCombo->setEditable(false);

    const QString providerId = m_providerCombo
        ? m_providerCombo->currentData().toString()
        : ProviderStore::directId();
    if (providerId.isEmpty() || providerId == ProviderStore::directId()) {
        // Claude tiers — the core's resolveModel maps these to concrete ids.
        m_modelCombo->addItem(QStringLiteral("Default model"), QString());
        m_modelCombo->addItem(QStringLiteral("Opus"), QStringLiteral("opus"));
        m_modelCombo->addItem(QStringLiteral("Sonnet"), QStringLiteral("sonnet"));
        m_modelCombo->addItem(QStringLiteral("Haiku"), QStringLiteral("haiku"));
        m_modelCombo->addItem(QStringLiteral("Fable"), QStringLiteral("fable"));
        const QString saved = KSharedConfig::openConfig()
                                  ->group(QStringLiteral("Agent"))
                                  .readEntry("model", QString());
        const int idx = m_modelCombo->findData(saved);
        if (idx >= 0) {
            m_modelCombo->setCurrentIndex(idx);
        }
        return;
    }

    // Provider mode: list the provider's own model ids, sent verbatim as --model.
    // "Provider default" (empty) passes no --model, letting the provider's main
    // model (ANTHROPIC_MODEL) take effect.
    const ProviderProfile p = ProviderStore::byId(providerId);
    m_modelCombo->addItem(QStringLiteral("Provider default"), QString());
    QStringList added;
    for (const QString &slot : ProviderStore::modelSlots()) {
        const QString model = p.models.value(slot).trimmed();
        if (model.isEmpty() || added.contains(model)) {
            continue;
        }
        added << model;
        m_modelCombo->addItem(model, model);
    }
}

void AgentPanel::reloadProviders()
{
    if (!m_providerCombo || !m_threadId.isEmpty()) {
        return; // frozen once a thread exists
    }
    const QString current = m_providerCombo->currentData().toString();
    {
        QSignalBlocker block(m_providerCombo);
        m_providerCombo->clear();
        const QList<ProviderProfile> profiles = ProviderStore::load();
        for (const ProviderProfile &p : profiles) {
            m_providerCombo->addItem(p.name, p.id);
        }
        int idx = m_providerCombo->findData(current);
        if (idx < 0) {
            idx = m_providerCombo->findData(ProviderStore::directId());
        }
        if (idx >= 0) {
            m_providerCombo->setCurrentIndex(idx);
        }
    }
    rebuildModelCombo();
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
    m_model->setToolsVisible(showTools);
}

bool AgentPanel::eventFilter(QObject *obj, QEvent *event)
{
    // Keep the floating "jump to latest" button anchored to the bottom-right
    // of the feed viewport as it resizes.
    if (m_view && obj == m_view->viewport()
        && event->type() == QEvent::Resize) {
        positionJumpButton();
        // Defer the exact row re-measure until the resize settles.
        if (m_resizeSettle) {
            m_resizeSettle->start();
        }
        return QWidget::eventFilter(obj, event);
    }
    // A press on the feed viewport that lands outside the open selection overlay
    // dismisses it (the overlay is a child of the viewport, so its own presses
    // never reach here). Don't consume the event — the click also (re)targets the
    // row it landed on.
    if (m_view && obj == m_view->viewport() && m_selectionEditor
        && event->type() == QEvent::MouseButtonPress) {
        const QPoint pos = static_cast<QMouseEvent *>(event)->pos();
        if (!m_selectionEditor->geometry().contains(pos)) {
            closeSelectionOverlay();
        }
    }
    // The open overlay closes on Esc; consumed so it doesn't bubble further. (We
    // don't close on focus-out: right-clicking the overlay for its native copy
    // menu takes focus away, and that must not dismiss the selection.)
    if (m_selectionEditor && obj == m_selectionEditor) {
        if (event->type() == QEvent::KeyPress
            && static_cast<QKeyEvent *>(event)->key() == Qt::Key_Escape) {
            closeSelectionOverlay();
            return true;
        }
    }
    // File/attachment drops onto the chat input must attach as context, not
    // insert the path as text. The drop is delivered to the input's viewport;
    // forward acceptable drops to the panel's handlers and consume them, while
    // letting plain-text drops fall through to normal insertion.
    if (m_input && obj == m_input->viewport()) {
        switch (event->type()) {
        case QEvent::DragEnter:
            if (canAcceptDrop(static_cast<QDragEnterEvent *>(event)->mimeData())) {
                dragEnterEvent(static_cast<QDragEnterEvent *>(event));
                return true;
            }
            break;
        case QEvent::DragMove:
            if (canAcceptDrop(static_cast<QDragMoveEvent *>(event)->mimeData())) {
                dragMoveEvent(static_cast<QDragMoveEvent *>(event));
                return true;
            }
            break;
        case QEvent::DragLeave:
            dragLeaveEvent(static_cast<QDragLeaveEvent *>(event));
            break;
        case QEvent::Drop:
            if (canAcceptDrop(static_cast<QDropEvent *>(event)->mimeData())) {
                dropEvent(static_cast<QDropEvent *>(event));
                return true;
            }
            break;
        default:
            break;
        }
    }
    if (obj == m_input && event->type() == QEvent::KeyPress) {
        auto *key = static_cast<QKeyEvent *>(event);
        // Esc while a turn is in flight interrupts it (keeps the session hot) so
        // you can redirect the agent without reaching for the toolbar button.
        if (key->key() == Qt::Key_Escape && !m_threadId.isEmpty() && !m_dormant
            && !m_idle) {
            onInterruptClicked();
            return true;
        }
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

void AgentPanel::setDormant(const QString &threadId, const QString &title, bool isolated,
                            const QString &backend)
{
    m_threadId = threadId;
    Q_EMIT threadIdChanged(m_threadId);
    m_dormant = true;
    m_isolated = isolated;
    m_backend = backend;
    loadTranscript();
    // Pull the thread's persisted compaction strategy and reflect it in the
    // dropdown — overrides whatever sticky default the panel was showing.
    // Kimi threads have no compaction support, so there's nothing to pull.
    if (m_backend != QLatin1String("kimi")) {
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
                     },
                     this);
    }
    addNote(QStringLiteral("dormant agent · %1 — Resume to continue.")
                .arg(title.toHtmlEscaped()),
            QStringLiteral("sys"));
    restoreDraft(); // any unsent draft for this thread
    emit dormantChanged(true);
    refresh();
}

void AgentPanel::adoptRunningThread(const QString &threadId, const QString &sourceThreadId,
                                    const QString &title, bool isolated,
                                    const QString &backend)
{
    m_threadId = threadId;
    Q_EMIT threadIdChanged(m_threadId);
    m_dormant = false;
    m_idle = true; // the fork is live and waiting for its first turn/follow-up
    m_isolated = isolated;
    m_backend = backend;
    // A fork bills its own fresh session — start the cost meter from zero.
    m_sessionCostUsd = 0.0;
    m_sessionInTokens = 0;
    m_sessionOutTokens = 0;
    // Replay the inherited conversation from the source agent (the fork's own
    // session id is minted asynchronously, so its transcript file isn't ready yet).
    loadTranscriptFrom(sourceThreadId);
    addNote(QStringLiteral("forked from %1 — the conversation continues here.")
                .arg(title.toHtmlEscaped()),
            QStringLiteral("sys"));
    emit dormantChanged(false);
    refresh();
}

void AgentPanel::loadTranscript()
{
    loadTranscriptFrom(m_threadId);
}

void AgentPanel::loadTranscriptFrom(const QString &fromThreadId)
{
    if (fromThreadId.isEmpty() || m_threadId.isEmpty() || !m_core->isConnected()) {
        return;
    }
    // The reply guard keys on THIS panel's thread (which may differ from the
    // source we're pulling the transcript from — a fork replays its parent's).
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("agent.transcript"),
                 QJsonObject{{QStringLiteral("threadId"), fromThreadId}},
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
                     // The attachment sidecar lets replayed You cards regain their
                     // named chips — the transcript keeps only inlined content.
                     m_replayAttachTurns =
                         result.value(QStringLiteral("attachments")).toArray();
                     // Guard cumulative cost + live timestamps: replayed result
                     // events must not be counted, and replayed cards carry no
                     // synthetic send time.
                     m_replaying = true;
                     m_replayLastPreview.clear();
                     m_replayLastEpoch = 0;
                     m_replayEventEpoch = 0;
                     for (const QJsonValue &v : events) {
                         renderEvent(v.toObject());
                     }
                     m_replaying = false;
                     m_replayAttachTurns = QJsonArray();
                     // One preview update for the whole replay: the final line,
                     // stamped with its real event time (0 = leave unstamped, so
                     // the card shows no time rather than a wrong "just now").
                     if (!m_replayLastPreview.isEmpty()) {
                         // >0: real event time. -1: no usable timestamp — leave
                         // the card unstamped rather than lying "just now".
                         emit previewChanged(m_replayLastPreview,
                                             m_replayLastEpoch > 0 ? m_replayLastEpoch : -1);
                     }
                     addNote(QStringLiteral("— prior conversation restored —"),
                             QStringLiteral("dim"));
                     scrollFeedToBottom();
                 },
                 this);
}

void AgentPanel::pushCompactStrategy()
{
    // Kimi threads have no compaction support — the core rejects the call.
    if (m_backend == QLatin1String("kimi")) {
        return;
    }
    if (m_threadId.isEmpty() || !m_core || !m_core->isConnected()) {
        return;
    }
    m_core->call(QStringLiteral("agent.setCompactStrategy"),
                 QJsonObject{
                     {QStringLiteral("threadId"), m_threadId},
                     {QStringLiteral("strategy"), m_compactCombo->currentData().toString()},
                     {QStringLiteral("strip"), m_compactStrip->isChecked()},
                 },
                 nullptr, this);
}

void AgentPanel::runCompactNow(const QString &model)
{
    // Kimi threads have no compaction support — the core rejects the call.
    if (m_backend == QLatin1String("kimi")) {
        addNote(QStringLiteral("Compaction is not supported for Kimi Code agents."),
                QStringLiteral("dim"));
        return;
    }
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
    addNote(QStringLiteral("summarizing with <b>%1</b>…").arg(model.toHtmlEscaped()),
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
                 },
                 this);
}

void AgentPanel::doResume()
{
    addNote(m_backend == QLatin1String("kimi")
                ? QStringLiteral("resuming the Kimi Code session…")
                : QStringLiteral("resuming the Claude Code session…"),
            QStringLiteral("sys"));
    QJsonObject params{{QStringLiteral("threadId"), m_threadId}};
    // Re-attach the provider (with its API token) when this panel started the
    // thread on one this session — the core never persists the token, so a
    // KWallet-held key would otherwise be unavailable on resume. When the panel
    // doesn't know the provider (e.g. after an app restart), the core rebuilds it
    // from the persisted snapshot and resolves the token from its env var.
    if (!m_startedProviderId.isEmpty()) {
        const ProviderProfile prof = ProviderStore::byId(m_startedProviderId);
        const QJsonObject pj = ProviderStore::toJson(prof);
        if (!pj.isEmpty()) {
            params.insert(QStringLiteral("provider"), pj);
        }
    }
    m_core->call(QStringLiteral("agent.resume"), params,
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         addNote(QStringLiteral("Could not resume: %1")
                                     .arg(error.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("err"));
                     }
                 },
                 this);
}

void AgentPanel::resume()
{
    if (!m_dormant || m_threadId.isEmpty()) {
        return;
    }
    // Kimi threads have no compaction support — resume straight away instead of
    // checking summary status / offering a pre-resume compact.
    if (m_backend == QLatin1String("kimi")) {
        doResume();
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
                     // A stored summary already exists — reuse it silently.
                     // Resume is meant to be one click; we don't re-ask just
                     // because a turn post-dates the summary.
                     if (result.value(QStringLiteral("hasSummary")).toBool(false)) {
                         doResume();
                         return;
                     }
                     // No summary on disk. If a "Compact on Resume" strategy is
                     // configured, follow it automatically; otherwise fall back
                     // to asking which model should produce one.
                     const QString strategy =
                         result.value(QStringLiteral("strategy")).toString();
                     const QString autoModel = agentkate::resumeStrategyModel(strategy);
                     const QString model =
                         autoModel.isEmpty() ? agentkate::askRecoveryModel(this) : autoModel;
                     if (model.isEmpty()) {
                         addNote(QStringLiteral("Resuming without compaction. "
                                                "The next turn will pay the full re-cache cost."),
                                 QStringLiteral("dim"));
                         doResume();
                         return;
                     }
                     addNote(QStringLiteral("summarizing with <b>%1</b>…").arg(model.toHtmlEscaped()),
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
                                  },
                                  this);
                 },
                 this);
}

void AgentPanel::refresh()
{
    const bool running = !m_threadId.isEmpty() && !m_dormant;
    m_sendBtn->setText(m_dormant ? QStringLiteral("Resume agent")
                                 : (running ? QStringLiteral("Send")
                                            : QStringLiteral("Start agent")));
    // "Stop & close" is terminal: it summarises then archives the agent (out of
    // the roster, restorable from the Sessions browser). Available whenever a
    // thread exists — running or dormant.
    m_stopBtn->setEnabled(!m_threadId.isEmpty());
    m_stopBtn->setToolTip(QStringLiteral(
        "Summarize the conversation and close this agent. It moves to the "
        "Sessions browser, where you can restore it later."));
    // Interrupt only makes sense while a turn is actually in flight — show it
    // then, hide it otherwise so it doesn't read as a second idle Stop.
    if (m_interruptBtn) {
        m_interruptBtn->setVisible(running && !m_idle);
    }
    m_diffBtn->setEnabled(running);
    // Forking is Claude-only: a Kimi thread can't be forked (the core rejects
    // agent.fork), so the button stays disabled for one.
    const bool threadKimi = m_backend == QLatin1String("kimi");
    if (m_forkBtn) {
        m_forkBtn->setEnabled(!m_threadId.isEmpty() && !threadKimi);
        m_forkBtn->setToolTip(
            threadKimi
                ? QStringLiteral("Forking is not supported for Kimi Code agents.")
                : QStringLiteral(
                    "Continue this conversation as a new agent on a different model or "
                    "thinking effort, keeping the full context. The original is untouched."));
    }
    // Compact-now needs a thread on disk (running or dormant). The Hot Opus
    // menu item is the only one that further needs the thread to be live.
    // Kimi threads have no compaction support at all.
    if (m_compactNowBtn) {
        m_compactNowBtn->setEnabled(!m_threadId.isEmpty() && !threadKimi);
        if (auto *menu = m_compactNowBtn->menu()) {
            const auto actions = menu->actions();
            if (!actions.isEmpty()) {
                actions.first()->setEnabled(running); // "Hot Opus (live thread)"
            }
        }
    }
    if (m_compactNowMenu) {
        m_compactNowMenu->setEnabled(!m_threadId.isEmpty() && !threadKimi);
        const auto actions = m_compactNowMenu->actions();
        if (!actions.isEmpty()) {
            actions.first()->setEnabled(running); // "Hot Opus (live thread)"
        }
    }
    // Permission, isolation, effort, model and desktop access are fixed once a
    // thread exists (they are baked into the agent's launch). A not-yet-started
    // Kimi pick further disables the pickers that don't apply to that backend —
    // including "When to ask": kimi permissions flow over ACP and always ask,
    // so offering the mode combo would silently ignore the user's choice.
    // Picker state only matters before a thread exists — a bound thread's
    // backend is m_backend, and the combo may hold a stale sticky default.
    const bool pickerKimi = m_threadId.isEmpty() && m_backendCombo
        && m_backendCombo->currentData().toString() == QLatin1String("kimi");
    m_compactCombo->setEnabled(!threadKimi && !pickerKimi);
    m_compactStrip->setEnabled(!threadKimi && !pickerKimi);
    m_backendCombo->setEnabled(m_threadId.isEmpty());
    m_modeCombo->setEnabled(m_threadId.isEmpty() && !pickerKimi);
    m_isolationCombo->setEnabled(m_threadId.isEmpty());
    m_effortCombo->setEnabled(m_threadId.isEmpty() && !pickerKimi);
    m_providerCombo->setEnabled(m_threadId.isEmpty() && !pickerKimi);
    m_modelCombo->setEnabled(m_threadId.isEmpty());
    m_coworkCheck->setEnabled(m_threadId.isEmpty() && !pickerKimi);

    // Offer promotion while a thread runs non-isolated in the workspace.
    m_promoteBar->setVisible(!m_threadId.isEmpty() && !m_isolated && !m_promoting);

    // "Agent Kate at work" indicator: animate while a turn is actually computing.
    m_working->setActive(running && !m_idle && m_permQueue.isEmpty());

    QString dot;
    QString text;
    // Map the same state the header text describes onto the roster card's status
    // enum (the single source of truth for the badge symbol + semantic colour).
    AgentRoles::AgentStatus st = AgentRoles::AgentStatus::Idle;
    if (m_errored) {
        // A failed start/turn: surface Error until the next send/resume clears it.
        dot = QStringLiteral("#d05050");
        text = QStringLiteral("Failed — check the conversation, then try again");
        st = AgentRoles::AgentStatus::Error;
    } else if (m_workspace.isEmpty()) {
        dot = QStringLiteral("#5d6471");
        text = QStringLiteral("Open a workspace folder to begin");
        st = AgentRoles::AgentStatus::Idle;
    } else if (m_dormant) {
        dot = QStringLiteral("#5d6471");
        text = QStringLiteral("Dormant — Resume to continue this session");
        st = AgentRoles::AgentStatus::Dormant;
    } else if (!running) {
        dot = QStringLiteral("#8b91a0");
        text = QStringLiteral("Ready — describe a task below");
        st = AgentRoles::AgentStatus::Idle;
    } else if (!m_permQueue.isEmpty()) {
        dot = QStringLiteral("#f0c000");
        text = QStringLiteral("Needs your input");
        st = AgentRoles::AgentStatus::NeedsInput;
    } else {
        const QString where = (m_isolated && !m_branch.isEmpty())
                                   ? QStringLiteral("branch %1").arg(m_branch)
                                   : QStringLiteral("in workspace");
        if (m_idle) {
            dot = QStringLiteral("#e0905f");
            text = QStringLiteral("Idle · %1 · send a follow-up").arg(where);
            st = AgentRoles::AgentStatus::Idle;
        } else {
            dot = QStringLiteral("#6cc08a");
            text = QStringLiteral("Working · %1").arg(where);
            st = AgentRoles::AgentStatus::Working;
        }
    }
    // Badge Kimi Code threads so the roster card shows which backend drives
    // this agent ("" / "claude" threads stay unmarked — the common case).
    if (threadKimi) {
        text.prepend(QStringLiteral("Kimi · "));
    }
    // Append the running session cost as a quiet suffix once any has accrued.
    // Kept on the same subtitle so the roster card reflects it too.
    if (m_sessionCostUsd > 0.0) {
        text += QStringLiteral(" · $%1")
                    .arg(QLocale().toString(m_sessionCostUsd, 'f', 4));
    }
    // Surface the running token total (the AI Inspector breaks it down per turn).
    const qlonglong sessionTok = m_sessionInTokens + m_sessionOutTokens;
    if (sessionTok > 0) {
        text += QStringLiteral(" · %1 tok").arg(QLocale().toString(sessionTok));
    }
    m_header->setText(QStringLiteral("<span style='color:%1'>&#9679;</span>&nbsp;&nbsp;%2")
                          .arg(dot, text.toHtmlEscaped()));
    emit statusChanged(int(st));
    emit subtitleChanged(text);
    // Roster card affordance, derived from the same state computed above.
    emit attentionChanged(running && !m_permQueue.isEmpty());
}

// --- conversation feed ------------------------------------------------------

void AgentPanel::scrollFeedToBottom()
{
    m_stickBottom = true;
    QScrollBar *bar = m_view->verticalScrollBar();
    bar->setValue(bar->maximum());
    // A second tick after the event loop has measured the freshly-inserted rows —
    // a long row's height can push the maximum further than the value just set.
    QTimer::singleShot(0, this, [this] {
        QScrollBar *b = m_view->verticalScrollBar();
        b->setValue(b->maximum());
    });
}

void AgentPanel::addMessageCard(const QString &role, const QString &accentHex,
                                const QString &bodyHtml, const QString &plainText,
                                bool replayed, const QJsonArray &attachments)
{
    const QString ts = replayed
        ? QString()
        : QLocale().toString(QTime::currentTime(), QLocale::ShortFormat);
    m_model->appendMessage(role, accentHex, bodyHtml, plainText, replayed, ts,
                           attachments);

    // Feed the roster card its two-line preview: the latest message line, with a
    // "You: " prefix on the user's own messages. plainText is the raw Markdown
    // source (safe for QTextLayout); fall back to nothing rather than raw HTML.
    // During replay we DON'T emit per-message — that would repaint the card N
    // times and stamp its "last activity" as now for every historical line.
    // Instead we retain the final line and emit once at replay end.
    if (!plainText.isEmpty()) {
        const bool fromUser = role == QLatin1String("You");
        const QString flat = plainText.simplified();
        const QString preview =
            fromUser ? i18nc("@item roster preview, user message", "You: %1", flat)
                     : flat;
        if (replayed) {
            m_replayLastPreview = preview;
            m_replayLastEpoch = m_replayEventEpoch;
        } else {
            emit previewChanged(preview);
        }
    }

    // A fresh row while the user is scrolled up flags unread content on the
    // jump-to-latest button.
    if (!m_stickBottom) {
        m_jumpUnread = true;
        updateJumpButton();
    }
}

void AgentPanel::addNote(const QString &html, const QString &kind)
{
    m_model->appendNote(html, kind);
    if (!m_stickBottom) {
        m_jumpUnread = true;
        updateJumpButton();
    }
}

void AgentPanel::addThinkingCard(const QString &thought)
{
    const QString trimmed = thought.trimmed();
    if (trimmed.isEmpty()) {
        return;
    }
    // Preview: the first line, elide-ready (the delegate elides to width).
    QString preview = trimmed.section(QLatin1Char('\n'), 0, 0).simplified();
    if (preview.length() > 120) {
        preview = preview.left(119) + QChar(0x2026);
    }
    m_model->appendThinking(agentkate::markdownToHtml(trimmed), trimmed, preview);
    if (!m_stickBottom) {
        m_jumpUnread = true;
        updateJumpButton();
    }
}

void AgentPanel::updateChecklistCard(const QJsonArray &todos)
{
    if (m_checklistKey >= 0 && m_model->setChecklist(m_checklistKey, todos)) {
        return; // updated in place
    }
    m_checklistKey = m_model->appendChecklist(todos);
    if (!m_stickBottom) {
        m_jumpUnread = true;
        updateJumpButton();
    }
}

void AgentPanel::showFeedContextMenu(const QModelIndex &idx, const QPoint &globalPos)
{
    const auto kind =
        TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt());
    // Tool rows: a single "Open in inspector…" entry (mirrors the header glyph).
    if (kind == TranscriptModel::Tool
        && idx.data(TranscriptModel::ToolVisibleRole).toBool()) {
        QMenu toolMenu(this);
        QAction *inspect = toolMenu.addAction(
            QIcon::fromTheme(QStringLiteral("document-preview")),
            i18n("Open in inspector…"));
        const QPersistentModelIndex pidx(idx);
        connect(inspect, &QAction::triggered, this, [this, pidx] {
            if (pidx.isValid()) {
                openToolInspector(pidx);
            }
        });
        toolMenu.exec(globalPos);
        return;
    }
    if (kind != TranscriptModel::Message) {
        return;
    }
    const QString src = idx.data(TranscriptModel::PlainRole).toString();
    if (src.isEmpty()) {
        return;
    }
    QMenu menu(this);
    QAction *copyMsg = menu.addAction(
        QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy message"));
    connect(copyMsg, &QAction::triggered, this, [src] {
        QGuiApplication::clipboard()->setText(src);
    });
    // Extract ```-fenced code spans straight from the Markdown source (don't
    // parse the rendered HTML).
    static const QRegularExpression fence(
        QStringLiteral("```[^\\n]*\\n(.*?)```"),
        QRegularExpression::DotMatchesEverythingOption);
    QStringList blocks;
    auto it = fence.globalMatch(src);
    while (it.hasNext()) {
        blocks << it.next().captured(1);
    }
    if (!blocks.isEmpty()) {
        QAction *copyCode = menu.addAction(
            QIcon::fromTheme(QStringLiteral("edit-copy")),
            i18np("Copy code block", "Copy %1 code blocks", blocks.size()));
        connect(copyCode, &QAction::triggered, this, [blocks] {
            QGuiApplication::clipboard()->setText(blocks.join(QStringLiteral("\n\n")));
        });
    }
    menu.exec(globalPos);
}

void AgentPanel::openSelectionOverlay(const QModelIndex &idx)
{
    if (!idx.isValid()) {
        return;
    }
    // Don't cover the in-flight last row while a turn is running: it may still be
    // mutating, and the overlay would be torn down immediately by dataChanged.
    if (isRunning() && !m_idle && idx.row() == m_model->count() - 1) {
        return;
    }
    // Re-opening the same row is a no-op (keeps the current selection).
    if (m_selectionRow == idx && m_selectionEditor) {
        return;
    }
    closeSelectionOverlay();
    m_selectionRow = idx;
    // openPersistentEditor drives createEditor → editorCreated, which stores the
    // handle and gives it focus so Ctrl+C works right away.
    m_view->openPersistentEditor(idx);
}

void AgentPanel::closeSelectionOverlay()
{
    if (!m_selectionRow.isValid()) {
        m_selectionEditor.clear();
        return;
    }
    const QModelIndex idx = m_selectionRow;
    m_selectionRow = QPersistentModelIndex();
    m_selectionEditor.clear();
    if (idx.isValid()) {
        m_view->closePersistentEditor(idx);
    }
}

void AgentPanel::remeasureVisibleRows()
{
    if (!m_view || !m_delegate || !m_delegate->hasStaleHeights()) {
        return;
    }
    m_delegate->clearStaleFlag();
    const int width = m_view->viewport()->width();
    if (width <= 0 || m_model->count() == 0) {
        return;
    }
    QStyleOptionViewItem opt;
    opt.initFrom(m_view);
    opt.font = m_view->font();
    // Walk the rows intersecting the viewport and measure each exactly. A small
    // overscan above/below keeps scrolling smooth right after a resize.
    const QModelIndex top = m_view->indexAt(QPoint(2, 2));
    int first = top.isValid() ? top.row() : 0;
    first = qMax(0, first - 4);
    const int vh = m_view->viewport()->height();
    bool changed = false;
    for (int row = first; row < m_model->count(); ++row) {
        const QModelIndex idx = m_model->index(row);
        QRect r = m_view->visualRect(idx);
        // Stop once we are a screenful past the bottom of the viewport.
        if (r.isValid() && r.top() > vh + 8) {
            break;
        }
        opt.rect = QRect(0, 0, width, 0);
        m_delegate->measureExact(idx, width, opt);
        changed = true;
    }
    if (changed) {
        // Relayout with the corrected visible-row heights. Off-screen rows keep
        // their cheap estimate until they scroll into view (then sizeHint
        // measures them exactly on first sight).
        m_view->doItemsLayout();
    }
}

// --- jump-to-latest floating button ----------------------------------------

void AgentPanel::positionJumpButton()
{
    if (!m_jumpBtn || !m_view) {
        return;
    }
    QWidget *vp = m_view->viewport();
    const QSize sz = m_jumpBtn->sizeHint();
    const int margin = 12;
    m_jumpBtn->move(vp->width() - sz.width() - margin,
                    vp->height() - sz.height() - margin);
}

void AgentPanel::updateJumpButton()
{
    if (!m_jumpBtn) {
        return;
    }
    const bool show = !m_stickBottom;
    if (show) {
        m_jumpBtn->setToolTip(m_jumpUnread ? i18n("Jump to latest — new messages")
                                           : i18n("Jump to latest"));
        positionJumpButton();
        m_jumpBtn->raise();
    } else {
        m_jumpUnread = false;
    }
    m_jumpBtn->setVisible(show);
}

// --- draft persistence ------------------------------------------------------

QString AgentPanel::draftKey() const
{
    if (!m_threadId.isEmpty()) {
        return QStringLiteral("draft-") + m_threadId;
    }
    if (m_workspace.isEmpty()) {
        return QString();
    }
    // Before a thread exists, scope the draft to the workspace path so it
    // survives until the agent.start migrates it to draft-<threadId>.
    const QByteArray h = QCryptographicHash::hash(m_workspace.toUtf8(),
                                                  QCryptographicHash::Md5);
    return QStringLiteral("draft-new-") + QString::fromLatin1(h.toHex().left(12));
}

void AgentPanel::saveDraft()
{
    const QString key = draftKey();
    if (key.isEmpty()) {
        return;
    }
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    const QString text = m_input->toPlainText();
    if (text.trimmed().isEmpty()) {
        cfg.deleteEntry(key);
    } else {
        cfg.writeEntry(key, text);
    }
    cfg.sync();
}

void AgentPanel::restoreDraft()
{
    const QString key = draftKey();
    if (key.isEmpty()) {
        return;
    }
    const QString saved = KSharedConfig::openConfig()
                              ->group(QStringLiteral("Agent"))
                              .readEntry(key, QString());
    if (!saved.isEmpty() && m_input->toPlainText().trimmed().isEmpty()) {
        QSignalBlocker blocker(m_input); // restoring isn't an edit to re-persist
        m_input->setPlainText(saved);
    }
}

void AgentPanel::clearDraft()
{
    const QString key = draftKey();
    if (key.isEmpty()) {
        return;
    }
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    cfg.deleteEntry(key);
    cfg.sync();
}

// --- in-conversation find ---------------------------------------------------

void AgentPanel::clearFindHighlights()
{
    // The delegate paints the highlight from the model's find state; clearing it
    // restores every row's stored HTML on the next repaint.
    m_model->setFind(QString(), -1);
    m_findHits.clear();
    m_findIndex = -1;
}

void AgentPanel::toggleFindBar()
{
    if (!m_findBar) {
        return;
    }
    const bool show = !m_findBar->isVisible();
    m_findBar->setVisible(show);
    if (show) {
        m_findEdit->setFocus();
        m_findEdit->selectAll();
        runFind(0);
    } else {
        clearFindHighlights();
        if (m_findStatus) {
            m_findStatus->clear();
        }
        m_input->setFocus();
    }
}

void AgentPanel::runFind(int direction)
{
    if (!m_findBar || !m_findBar->isVisible()) {
        return;
    }
    const QString needle = m_findEdit->text();
    if (needle.isEmpty()) {
        clearFindHighlights();
        if (m_findStatus) {
            m_findStatus->clear();
        }
        return;
    }
    // Recompute the matching rows: a message row matches if its plain source
    // contains the needle. The delegate paints the per-row highlight from the
    // model's find state, so no HTML rewriting happens here.
    m_findHits.clear();
    for (int row = 0; row < m_model->count(); ++row) {
        const TranscriptModel::Item &it = m_model->itemAt(row);
        if (it.kind == TranscriptModel::Message
            && it.plain.contains(needle, Qt::CaseInsensitive)) {
            m_findHits.append(row);
        }
    }
    if (m_findHits.isEmpty()) {
        m_findIndex = -1;
        m_model->setFind(needle, -1);
        if (m_findStatus) {
            m_findStatus->setText(i18n("No matches"));
        }
        return;
    }
    if (m_findIndex < 0) {
        m_findIndex = (direction < 0) ? m_findHits.size() - 1 : 0;
    } else {
        m_findIndex = (m_findIndex + direction + m_findHits.size()) % m_findHits.size();
    }
    const int curRow = m_findHits.at(m_findIndex);
    m_model->setFind(needle, curRow);
    m_stickBottom = false;
    m_view->scrollTo(m_model->index(curRow), QAbstractItemView::PositionAtCenter);
    updateJumpButton();
    if (m_findStatus) {
        m_findStatus->setText(i18nc("current match / total matches", "%1 / %2",
                                    m_findIndex + 1, m_findHits.size()));
    }
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

    // Resolve the API provider for a fresh start up front, while the composer
    // still holds the message — a missing key aborts cleanly without losing it.
    // (akcore inherits this UI's environment, so if the key can't be resolved
    // here it cannot be resolved at launch either.)
    QJsonObject providerJson;
    QString startedProviderId;
    // A Kimi Code start skips provider routing entirely (the core rejects a
    // provider combined with backend=kimi).
    const bool startingKimi = m_threadId.isEmpty() && m_backendCombo
        && m_backendCombo->currentData().toString() == QLatin1String("kimi");
    if (m_threadId.isEmpty() && m_providerCombo && !startingKimi) {
        const ProviderProfile prof =
            ProviderStore::byId(m_providerCombo->currentData().toString());
        if (prof.routed()) {
            providerJson = ProviderStore::toJson(prof);
            if (!providerJson.contains(QStringLiteral("authToken"))) {
                addNote(QStringLiteral(
                            "No API key is configured for provider “%1”. Set one in "
                            "Options ▸ Configure API Providers… (or its %2 environment "
                            "variable), then try again.")
                            .arg(prof.name.toHtmlEscaped(),
                                 prof.envVar.isEmpty() ? QStringLiteral("API key")
                                                       : prof.envVar.toHtmlEscaped()),
                        QStringLiteral("err"));
                return;
            }
            startedProviderId = prof.id;
        }
    }
    // A turn is in progress: queue the follow-up instead of sending it now.
    // The `claude` CLI buffers a second stdin user message until the current
    // turn ends (verified: it is consumed at the turn boundary, never injected
    // mid-turn), so sending now would just queue it invisibly inside the CLI.
    // Holding it here keeps the queue visible and editable. We drain one per
    // `result`. Mirrors the m_permQueue defer pattern.
    if (!m_threadId.isEmpty() && !m_idle) {
        m_input->clear();
        clearDraft();
        m_sendQueue.append(QueuedMsg{text, m_attachments});
        m_attachments = QJsonArray();
        rebuildAttachChips();
        rebuildQueueChips();
        addNote(QStringLiteral("&#128338; queued — sends when the current turn finishes"),
                QStringLiteral("dim"));
        refresh();
        return;
    }

    m_input->clear();
    clearDraft();

    // Detach the pending attachments for this message, then clear the bar.
    const QJsonArray attachments = m_attachments;
    m_attachments = QJsonArray();
    rebuildAttachChips();

    if (m_threadId.isEmpty()) {
        // A fresh session — start the cost meter from zero.
        m_sessionCostUsd = 0.0;
        m_sessionInTokens = 0;
        m_sessionOutTokens = 0;
        addYouCard(text, attachments);
        m_idle = false;
        m_working->setActivity(QString()); // a new turn starts in generic mode

        QString title = text.simplified();
        if (title.isEmpty()) {
            title = QStringLiteral("(attachments)");
        }
        if (title.length() > 26) {
            title = title.left(25) + QChar(0x2026);
        }
        emit titleChanged(title);

        QJsonObject startParams{
            {QStringLiteral("workspacePath"), m_workspace},
            {QStringLiteral("prompt"), text},
            {QStringLiteral("backend"), m_backendCombo->currentData().toString()},
            // Kimi has no permission modes — it always asks over ACP. Send
            // empty rather than a mode the core would silently drop.
            {QStringLiteral("permissionMode"),
             startingKimi ? QString() : m_modeCombo->currentData().toString()},
            {QStringLiteral("isolation"), m_isolationCombo->currentData().toString()},
            // Effort, provider routing and Cowork don't apply to Kimi Code
            // (the core rejects them); its model is the editable combo's text.
            {QStringLiteral("effort"),
             startingKimi ? QString() : m_effortCombo->currentData().toString()},
            {QStringLiteral("model"),
             startingKimi ? m_modelCombo->currentText().trimmed()
                          : m_modelCombo->currentData().toString()},
            {QStringLiteral("coworkEnabled"), !startingKimi && m_coworkCheck->isChecked()},
            {QStringLiteral("attachments"), attachments}};
        if (!providerJson.isEmpty()) {
            startParams.insert(QStringLiteral("provider"), providerJson);
        }
        m_startedProviderId = startedProviderId;
        m_core->call(QStringLiteral("agent.start"), startParams,
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
                         Q_EMIT threadIdChanged(m_threadId);
                         // The core echoes the backend that actually started.
                         m_backend = result.value(QStringLiteral("backend")).toString();
                         // Apply the user's chosen compaction strategy now
                         // that the thread exists on the server.
                         pushCompactStrategy();
                         refresh();
                     },
                     this);
        refresh();
    } else {
        deliverMessage(text, attachments);
    }
}

// compactAttachments strips the heavy body (dataB64 / text) from a live
// attachment array, keeping only the metadata the feed row + delegate need to
// draw named, clickable chips. This is exactly what the core sidecar persists,
// so a live send and a replayed card carry identical chip data.
QJsonArray AgentPanel::compactAttachments(const QJsonArray &attachments)
{
    QJsonArray out;
    for (const QJsonValue &av : attachments) {
        const QJsonObject a = av.toObject();
        QJsonObject c{{QStringLiteral("name"), a.value(QStringLiteral("name"))},
                      {QStringLiteral("kind"), a.value(QStringLiteral("kind"))}};
        if (a.contains(QStringLiteral("path"))) {
            c[QStringLiteral("path")] = a.value(QStringLiteral("path"));
        }
        if (a.contains(QStringLiteral("mediaType"))) {
            c[QStringLiteral("mediaType")] = a.value(QStringLiteral("mediaType"));
        }
        if (a.value(QStringLiteral("outside")).toBool()) {
            c[QStringLiteral("outside")] = true;
        }
        out.append(c);
    }
    return out;
}

// addYouCard renders an outgoing user message as a "You" card in the feed. Any
// attachments are stored (compactly) on the row so the delegate paints one named
// chip per file under the message body.
void AgentPanel::addYouCard(const QString &text, const QJsonArray &attachments)
{
    const QString youLine =
        text.toHtmlEscaped().replace(QLatin1Char('\n'), QLatin1String("<br>"));
    addMessageCard(QStringLiteral("You"),
                   isDark(this) ? QStringLiteral("#7cb7ff") : QStringLiteral("#1a5fb4"),
                   youLine, text, m_replaying, compactAttachments(attachments));
}

// openAttachment opens a clicked attachment chip. An image is previewed in a
// lightweight modeless dialog reusing ImageView; a text/file attachment asks the
// window to open it in the editor. A file whose origin path is gone (moved/
// deleted since it was attached) degrades to a friendly status note.
void AgentPanel::openAttachment(const QJsonObject &att)
{
    const QString path = att.value(QStringLiteral("path")).toString();
    const QString name = att.value(QStringLiteral("name")).toString();
    const bool image =
        att.value(QStringLiteral("kind")).toString() == QLatin1String("image");

    if (path.isEmpty() || !QFileInfo::exists(path)) {
        emit statusMessage(
            i18n("Can't open “%1” — the file has moved or been deleted since it was "
                 "attached.",
                 name.isEmpty() ? path : name));
        return;
    }

    if (image) {
        auto *dlg = new QDialog(this);
        dlg->setAttribute(Qt::WA_DeleteOnClose);
        dlg->setWindowTitle(name.isEmpty() ? i18n("Attachment") : name);
        auto *lay = new QVBoxLayout(dlg);
        lay->setContentsMargins(0, 0, 0, 0);
        auto *view = new ImageView(path, dlg);
        if (!view->isValid()) {
            delete dlg;
            emit statusMessage(i18n("Can't preview “%1” — it isn't a readable image.",
                                    name.isEmpty() ? path : name));
            return;
        }
        lay->addWidget(view);
        dlg->resize(640, 520);
        dlg->show();
        return;
    }

    // Text / file: open it in the editor (MainWindow makes the editor visible).
    emit openFileRequested(path);
}

void AgentPanel::openToolInspector(const QModelIndex &idx)
{
    if (!idx.isValid()
        || TranscriptModel::Kind(idx.data(TranscriptModel::KindRole).toInt())
               != TranscriptModel::Tool) {
        return;
    }
    const QString name = idx.data(TranscriptModel::ToolNameRole).toString();
    const QString input = idx.data(TranscriptModel::ToolDetailRole).toString();
    const QString full = idx.data(TranscriptModel::ToolFullResultRole).toString();
    // The retained result is capped at kToolResultStoreCap; a stored size at the
    // cap means the true output was longer (the on-disk transcript has it all).
    const bool capped = full.size() >= kToolResultStoreCap;

    auto *dlg = new ToolInspectorDialog(name, input, full, capped, this);
    connect(dlg, &ToolInspectorDialog::openFile, this,
            &AgentPanel::openFileRequested);
    dlg->show();
}

void AgentPanel::noteWorkflowLaunch(const QString &inputJson, const QString &resultText)
{
    m_workflowInput = inputJson;
    m_workflowResult = resultText;

    // A fresh monitor drives the chip's live label (running → done) and stops
    // polling once the run reaches a terminal state. Replaces any prior one so
    // the chip always reflects the most recent workflow.
    if (m_workflowMonitor) {
        m_workflowMonitor->deleteLater();
        m_workflowMonitor = nullptr;
    }
    m_workflowMonitor = new WorkflowMonitor(m_workflowInput, m_workflowResult, this);
    connect(m_workflowMonitor, &WorkflowMonitor::changed, this,
            &AgentPanel::updateWorkflowChip);
    updateWorkflowChip();
}

void AgentPanel::updateWorkflowChip()
{
    if (!m_workflowBar || !m_workflowChip) {
        return;
    }
    if (!m_workflowMonitor || !m_workflowMonitor->isValid()) {
        m_workflowBar->setVisible(false);
        return;
    }
    QString label;
    switch (m_workflowMonitor->snapshot().state) {
    case WorkflowMonitor::State::Completed:
        label = QStringLiteral("⧉ Workflow · done");
        break;
    case WorkflowMonitor::State::Failed:
        label = QStringLiteral("⧉ Workflow · failed");
        break;
    case WorkflowMonitor::State::Running:
        label = QStringLiteral("⧉ Workflow · running");
        break;
    default:
        label = QStringLiteral("⧉ Workflow");
        break;
    }
    m_workflowChip->setText(label);
    m_workflowBar->setVisible(true);
}

void AgentPanel::openWorkflowMonitor()
{
    if (m_workflowResult.isEmpty()) {
        return;
    }
    auto *dlg = new WorkflowMonitorDialog(m_workflowInput, m_workflowResult, this);
    dlg->show();
}

// deliverMessage sends a message to the live thread right now: it shows the
// You card, marks the turn busy, and issues agent.send. Used both for an
// immediate send and to drain one queued follow-up per turn boundary.
void AgentPanel::deliverMessage(const QString &text, const QJsonArray &attachments)
{
    addYouCard(text, attachments);
    m_idle = false;
    m_errored = false; // a fresh turn clears any prior failure state
    m_working->setActivity(QString()); // a new turn starts in generic mode
    m_core->call(QStringLiteral("agent.send"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId},
                             {QStringLiteral("text"), text},
                             {QStringLiteral("attachments"), attachments}},
                 nullptr, this);
    refresh();
}

// drainSendQueue fires the next queued follow-up once the thread is idle. It
// is called on every `result`; sending sets m_idle = false again, so the rest
// of the queue waits for the following turn boundary — one message per turn.
void AgentPanel::drainSendQueue()
{
    if (m_sendQueue.isEmpty() || m_threadId.isEmpty() || m_dormant || !m_idle) {
        return;
    }
    const QueuedMsg q = m_sendQueue.takeFirst();
    rebuildQueueChips();
    deliverMessage(q.text, q.attachments);
}

// restoreQueuedToComposer moves any still-queued follow-ups back into the
// composer so a stopped/failed turn never silently eats the human's text. The
// messages join with blank lines; if the composer already holds a draft the
// queued text is prepended so nothing typed is clobbered. Attachments from the
// queued messages are restored to the pending bar (deduped by path).
void AgentPanel::restoreQueuedToComposer()
{
    if (m_sendQueue.isEmpty()) {
        return;
    }
    QStringList parts;
    QJsonArray restoredAttachments;
    QSet<QString> seenPaths;
    for (const QueuedMsg &q : m_sendQueue) {
        if (!q.text.isEmpty()) {
            parts << q.text;
        }
        for (const QJsonValue &a : q.attachments) {
            const QString path = a.toObject().value(QStringLiteral("path")).toString();
            if (path.isEmpty() || !seenPaths.contains(path)) {
                seenPaths.insert(path);
                restoredAttachments.append(a);
            }
        }
    }
    m_sendQueue.clear();
    rebuildQueueChips();

    const QString queued = parts.join(QStringLiteral("\n\n"));
    if (!queued.isEmpty()) {
        const QString existing = m_input->toPlainText();
        m_input->setPlainText(existing.trimmed().isEmpty()
                                  ? queued
                                  : queued + QStringLiteral("\n\n") + existing);
    }
    // Fold the queued attachments back in front of any already-pending ones.
    for (const QJsonValue &a : m_attachments) {
        const QString path = a.toObject().value(QStringLiteral("path")).toString();
        if (path.isEmpty() || !seenPaths.contains(path)) {
            restoredAttachments.append(a);
        }
    }
    m_attachments = restoredAttachments;
    rebuildAttachChips();
}

// rebuildQueueChips redraws the queued-message chip bar from m_sendQueue.
// Each chip shows the message (truncated) and removes it on click.
void AgentPanel::rebuildQueueChips()
{
    // Drop every existing chip widget (the FlowLayout holds only chips — no
    // trailing stretch), then re-add one per queued message.
    while (QLayoutItem *item = m_queueLayout->takeAt(0)) {
        if (QWidget *w = item->widget()) {
            w->deleteLater();
        }
        delete item;
    }
    for (int i = 0; i < m_sendQueue.size(); ++i) {
        QString label = m_sendQueue.at(i).text.simplified();
        if (label.isEmpty()) {
            label = QStringLiteral("(attachments)");
        }
        if (label.length() > 28) {
            label = label.left(27) + QChar(0x2026);
        }
        auto *chip = new QPushButton(QStringLiteral("⏳ %1   ✕").arg(label), m_queueBar);
        chip->setCursor(Qt::PointingHandCursor);
        chip->setToolTip(QStringLiteral("Queued message — click to remove before it sends"));
        connect(chip, &QPushButton::clicked, this, [this, i] {
            if (i < m_sendQueue.size()) {
                m_sendQueue.removeAt(i);
                rebuildQueueChips();
                refresh();
            }
        });
        m_queueLayout->addWidget(chip);
    }
    m_queueBar->setVisible(!m_sendQueue.isEmpty());
}

void AgentPanel::onAttachClicked()
{
    const QStringList paths = QFileDialog::getOpenFileNames(
        this, QStringLiteral("Attach files"),
        m_workspace.isEmpty() ? QDir::homePath() : m_workspace);
    attachPaths(paths);
}

void AgentPanel::showAttachNotice(const QString &text)
{
    if (!m_attachNotice) {
        return;
    }
    m_attachNotice->setText(text);
    m_attachNotice->animatedShow();
}

void AgentPanel::attachPaths(const QStringList &paths)
{
    if (paths.isEmpty()) {
        return;
    }
    if (m_attachNotice) {
        m_attachNotice->hide(); // clear any stale rejection from a prior attempt
    }
    // The file I/O + JSON assembly lives in AttachmentBuilder; the panel keeps
    // the chip UI and the rejection banner.
    const QStringList skipped =
        agentkate::buildPathAttachments(paths, m_workspace, m_attachments);
    if (!skipped.isEmpty()) {
        showAttachNotice(skipped.size() == 1
                             ? i18n("Couldn't attach %1", skipped.first())
                             : i18n("Couldn't attach some files:\n• %1",
                                    skipped.join(QStringLiteral("\n• "))));
    }
    rebuildAttachChips();
}

void AgentPanel::attachItems(const QJsonArray &items)
{
    // The ranged-excerpt extraction lives in AttachmentBuilder; non-ranged items
    // come back in `wholeFile` to degrade to attachPaths, and any per-item
    // failures come back as `skipped` reasons for the banner.
    QStringList wholeFile;
    const QStringList skipped =
        agentkate::buildItemAttachments(items, m_attachments, wholeFile);
    for (const QString &reason : skipped) {
        showAttachNotice(i18n("Couldn't attach %1", reason));
    }
    rebuildAttachChips();
    if (!wholeFile.isEmpty()) {
        attachPaths(wholeFile);
    }
}

bool AgentPanel::canAcceptDrop(const QMimeData *mime) const
{
    if (!mime) {
        return false;
    }
    if (mime->hasFormat(QLatin1String(kAttachMime))) {
        return true;
    }
    if (mime->hasUrls()) {
        const auto urls = mime->urls();
        for (const QUrl &u : urls) {
            if (u.isLocalFile()) {
                return true;
            }
        }
    }
    return false;
}

void AgentPanel::dragEnterEvent(QDragEnterEvent *event)
{
    if (canAcceptDrop(event->mimeData())) {
        m_dragActive = true;
        update();
        event->acceptProposedAction();
    }
}

void AgentPanel::dragMoveEvent(QDragMoveEvent *event)
{
    if (canAcceptDrop(event->mimeData())) {
        event->acceptProposedAction();
    }
}

void AgentPanel::dragLeaveEvent(QDragLeaveEvent *event)
{
    m_dragActive = false;
    update();
    event->accept();
}

void AgentPanel::dropEvent(QDropEvent *event)
{
    m_dragActive = false;
    update();

    const QMimeData *mime = event->mimeData();
    if (!canAcceptDrop(mime)) {
        return;
    }

    const int before = m_attachments.size();
    if (mime->hasFormat(QLatin1String(kAttachMime))) {
        // Ranged payload from the search results — preserves line spans.
        const QJsonArray items =
            QJsonDocument::fromJson(mime->data(QLatin1String(kAttachMime)))
                .array();
        attachItems(items);
    } else {
        QStringList paths;
        const auto urls = mime->urls();
        for (const QUrl &u : urls) {
            if (u.isLocalFile()) {
                paths << u.toLocalFile();
            }
        }
        attachPaths(paths);
    }
    event->acceptProposedAction();

    const int added = m_attachments.size() - before;
    if (added > 0) {
        emit statusMessage(i18np("Attached %1 item as context",
                                 "Attached %1 items as context", added));
    } else {
        emit statusMessage(i18n("Nothing attached"));
    }
}

void AgentPanel::paintEvent(QPaintEvent *event)
{
    QWidget::paintEvent(event);
    if (!m_dragActive) {
        return;
    }
    QPainter p(this);
    p.setRenderHint(QPainter::Antialiasing);
    const QColor hl = palette().color(QPalette::Highlight);
    QPen pen(hl, 2);
    p.setPen(pen);
    const QRectF border = rect().adjusted(1, 1, -1, -1);
    p.drawRoundedRect(border, 8, 8);
    p.setPen(hl);
    p.drawText(rect(), Qt::AlignCenter,
               i18n("Drop files to attach as context"));
}

void AgentPanel::rebuildAttachChips()
{
    // Drop every existing chip widget (the FlowLayout holds only chips — no
    // trailing stretch), then re-add one per attachment.
    while (QLayoutItem *item = m_attachLayout->takeAt(0)) {
        if (QWidget *w = item->widget()) {
            w->deleteLater();
        }
        delete item;
    }
    for (int i = 0; i < m_attachments.size(); ++i) {
        const QJsonObject att = m_attachments.at(i).toObject();
        const QString name = att.value(QStringLiteral("name")).toString();
        auto *chip = new QPushButton(QStringLiteral("%1   ✕").arg(name), m_attachBar);
        chip->setCursor(Qt::PointingHandCursor);
        chip->setToolTip(att.value(QStringLiteral("outside")).toBool()
                             ? i18n("Outside project — click to remove")
                             : i18n("Remove attachment"));
        // For image attachments, decode the stored base64 into a small preview
        // icon so the chip shows a thumbnail rather than just the filename.
        if (att.value(QStringLiteral("kind")).toString() == QLatin1String("image")) {
            const QByteArray raw = QByteArray::fromBase64(
                att.value(QStringLiteral("dataB64")).toString().toLatin1());
            QPixmap pm;
            if (pm.loadFromData(raw)) {
                chip->setIcon(QIcon(pm.scaled(28, 28, Qt::KeepAspectRatio,
                                              Qt::SmoothTransformation)));
                chip->setIconSize(QSize(28, 28));
            }
        }
        connect(chip, &QPushButton::clicked, this, [this, i] {
            m_attachments.removeAt(i);
            rebuildAttachChips();
        });
        m_attachLayout->addWidget(chip);
    }
    m_attachBar->setVisible(!m_attachments.isEmpty());
}

void AgentPanel::onStopClicked()
{
    if (m_threadId.isEmpty()) {
        return;
    }
    // "Stop & close" is terminal: it summarises the conversation, then archives
    // the agent out of the roster (restorable from the Sessions browser). To
    // just cancel the current response and keep going, use Interrupt instead.
    // Confirm only when a turn is actually in flight — the user could lose an
    // in-progress response; a quiet idle/dormant agent closes without a prompt.
    const bool turnInFlight = !m_dormant && !m_idle;
    if (turnInFlight) {
        if (QMessageBox::question(
                this, i18n("Stop & close agent"),
                i18n("This agent is still working. Stop it now, summarize the "
                     "conversation, and close the agent? You can restore it later "
                     "from the Sessions browser."))
            != QMessageBox::Yes) {
            return;
        }
    }
    addNote(QStringLiteral("&#9209; stopping &amp; closing — summarizing…"),
            QStringLiteral("sys"));
    m_stopBtn->setEnabled(false);
    const QString tid = m_threadId;
    // QPointer guard: stopClose can take a moment (hot-compaction), and the panel
    // could be torn down before the reply lands.
    QPointer<AgentPanel> self(this);
    m_core->call(QStringLiteral("agent.stopClose"),
                 QJsonObject{{QStringLiteral("threadId"), tid}},
                 [self](const QJsonObject &, const QJsonObject &error) {
                     if (!self) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         self->addNote(QStringLiteral("Could not close agent: %1")
                                           .arg(error.value(QStringLiteral("message"))
                                                    .toString()
                                                    .toHtmlEscaped()),
                                       QStringLiteral("err"));
                         self->m_stopBtn->setEnabled(true);
                         return;
                     }
                     // Archived on the core — drop the panel and roster entry. We
                     // clear m_threadId first so ~AgentPanel doesn't re-issue a
                     // stop against the now-unknown thread.
                     self->m_threadId.clear();
                     Q_EMIT self->closeRequested();
                 },
                 this);
}

void AgentPanel::onInterruptClicked()
{
    // Only meaningful while a turn is in flight (generating, or paused on a
    // tool/permission). Aborts the response now — no more tokens billed — but
    // keeps the Claude session, so the next message redirects the same agent.
    if (m_threadId.isEmpty() || m_dormant || m_idle) {
        return;
    }
    addNote(QStringLiteral("&#9209; interrupting…"), QStringLiteral("sys"));
    m_core->call(QStringLiteral("agent.interrupt"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId}},
                 nullptr, this);
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
                 },
                 this);
}

void AgentPanel::onPromoteClicked()
{
    if (m_threadId.isEmpty() || m_isolated || m_promoting) {
        return;
    }
    m_promoting = true;
    addNote(QStringLiteral("moving to a private copy — the agent will restart in "
                           "its own sandbox…"),
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
                 },
                 this);
    refresh();
}

void AgentPanel::onNotification(const QString &method, const QJsonObject &params)
{
    if (method == QLatin1String("agent.event")) {
        if (m_threadId.isEmpty()
            || params.value(QStringLiteral("threadId")).toString() != m_threadId) {
            return;
        }
        // The core coalesces the per-line stream-json flood into one
        // notification carrying an ordered batch; render each event in order.
        // Fall back to the legacy single-"event" key for forward/backward
        // safety, though every current sender emits the "events" array.
        const QJsonValue evs = params.value(QStringLiteral("events"));
        if (evs.isArray()) {
            const QJsonArray events = evs.toArray();
            for (const QJsonValue &v : events) {
                renderEvent(v.toObject());
            }
        } else if (params.contains(QStringLiteral("event"))) {
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
    QString summary = agentkate::permSummary(tool, req.value(QStringLiteral("input")).toObject());
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
                             {QStringLiteral("allow"), allow}},
                 nullptr, this);
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
                    {QStringLiteral("updatedInput"), updatedInput}},
        nullptr, this);

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

    // The transcript event carries an ISO-8601 "timestamp"; during replay we
    // remember it so the end-of-replay preview can stamp the card's last-activity
    // with the real time rather than lying "just now". 0 if absent/unparseable.
    if (m_replaying) {
        const QString iso = ev.value(QStringLiteral("timestamp")).toString();
        const QDateTime when =
            iso.isEmpty() ? QDateTime()
                          : QDateTime::fromString(iso, Qt::ISODateWithMs);
        m_replayEventEpoch = when.isValid() ? when.toSecsSinceEpoch() : 0;
    }

    if (type == QLatin1String("system")) {
        // Only the init system event is worth showing in the feed.
        if (ev.value(QStringLiteral("subtype")).toString() != QLatin1String("init")) {
            return;
        }
        // A kimi init event carries the session's config options (model /
        // thinking / mode enumerations straight from the CLI). Persist each
        // as "value|name" pairs so the pickers can offer the real lists on
        // the next agent instead of free-text fields.
        const QJsonArray configOptions =
            ev.value(QStringLiteral("configOptions")).toArray();
        if (!configOptions.isEmpty()) {
            KConfigGroup cfg =
                KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
            for (const QJsonValue &ov : configOptions) {
                const QJsonObject opt = ov.toObject();
                const QString id = opt.value(QStringLiteral("id")).toString();
                if (id.isEmpty()) {
                    continue;
                }
                QStringList entries;
                const QJsonArray values = opt.value(QStringLiteral("options")).toArray();
                for (const QJsonValue &vv : values) {
                    const QJsonObject val = vv.toObject();
                    entries << val.value(QStringLiteral("value")).toString()
                                   + QLatin1Char('|')
                                   + val.value(QStringLiteral("name")).toString();
                }
                if (!entries.isEmpty()) {
                    cfg.writeEntry(QStringLiteral("kimiOpt-") + id, entries);
                }
            }
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
                                   agentkate::markdownToHtml(t), t, m_replaying);
                    m_working->setActivity(QString()); // text → generic reasoning
                }
            } else if (bt == QLatin1String("thinking")) {
                addThinkingCard(b.value(QStringLiteral("thinking")).toString());
            } else if (bt == QLatin1String("redacted_thinking")) {
                // The reasoning exists but is encrypted — show that it
                // happened rather than dropping it silently.
                addThinkingCard(i18n("(this thinking block was redacted for safety)"));
            } else if (bt == QLatin1String("tool_use")) {
                const QString name = b.value(QStringLiteral("name")).toString();
                // The permission gate and question tool are surfaced by their
                // own UI, so don't also list them as raw tool calls.
                if (name.contains(QLatin1String("request_permission"))
                    || name == QLatin1String("AskUserQuestion")) {
                    continue;
                }
                // The agent's todo list renders as the live plan checklist,
                // not a generic tool row. Its tool_result carries only an ack,
                // so the tool_use id is deliberately not tracked.
                if (name == QLatin1String("TodoWrite")) {
                    updateChecklistCard(b.value(QStringLiteral("input"))
                                            .toObject()
                                            .value(QStringLiteral("todos"))
                                            .toArray());
                    m_working->setActivity(agentkate::activityFor(name));
                    continue;
                }
                const QJsonObject input = b.value(QStringLiteral("input")).toObject();
                QString summary = agentkate::permSummary(name, input).simplified();
                if (summary.length() > 96) {
                    summary = summary.left(95) + QChar(0x2026);
                }
                const QString detail = QString::fromUtf8(
                    QJsonDocument(input).toJson(QJsonDocument::Indented));
                const bool show = KSharedConfig::openConfig()
                                      ->group(QStringLiteral("Agent"))
                                      .readEntry("showTools", true);
                const int key = m_model->appendTool(name, summary, detail, show);
                const QString id = b.value(QStringLiteral("id")).toString();
                if (!id.isEmpty()) {
                    m_toolRows.insert(id, key);
                }
                // Remember a Workflow launch by its row key so the paired
                // tool_result (which carries the run anchors) can be captured.
                if (name == QLatin1String("Workflow")) {
                    m_workflowToolKeys.insert(key);
                    m_workflowInputByKey.insert(key, detail);
                }
                if (!m_stickBottom) {
                    m_jumpUnread = true;
                    updateJumpButton();
                }
                m_working->setActivity(agentkate::activityFor(name));
            }
        }

    } else if (type == QLatin1String("user")) {
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        // A "user" event is either the human's own message (text / image / an
        // inlined "Attached file …" block) or the tool_result blocks the CLI
        // echoes back after a tool runs. On replay we reconstruct the human's You
        // card (the live path already drew it via addYouCard); tool_results are
        // folded into their tool rows in both cases.
        if (m_replaying) {
            QStringList userLines;
            bool hasInlineAttachment = false; // image block or "Attached file …" text
            bool hasUserBlock = false;        // any non-tool_result block
            for (const QJsonValue &bv : content) {
                const QJsonObject b = bv.toObject();
                const QString bt = b.value(QStringLiteral("type")).toString();
                if (bt == QLatin1String("tool_result")) {
                    continue;
                }
                hasUserBlock = true;
                if (bt == QLatin1String("image")) {
                    hasInlineAttachment = true;
                } else if (bt == QLatin1String("text")) {
                    const QString t = b.value(QStringLiteral("text")).toString();
                    // buildUserContent (core) synthesizes "Attached file `name`:\n
                    // ```\n…```" text blocks for text attachments — those are not
                    // part of what the human typed, so drop them from the card
                    // body; the sidecar re-supplies them as chips.
                    if (t.startsWith(QLatin1String("Attached file `"))) {
                        hasInlineAttachment = true;
                    } else if (!t.isEmpty()) {
                        userLines << t;
                    }
                }
            }
            if (hasUserBlock) {
                const QString userText = userLines.join(QStringLiteral("\n"));
                QJsonArray attachments;
                // Pair this message with the front sidecar turn when it carried
                // attachments and its recorded prompt matches — keeping the two in
                // step even if some replayed user messages had no attachments.
                if (hasInlineAttachment && !m_replayAttachTurns.isEmpty()) {
                    const QJsonObject turn = m_replayAttachTurns.first().toObject();
                    attachments = turn.value(QStringLiteral("attachments")).toArray();
                    // Positional (FIFO) pairing: an inline-attachment message must
                    // correspond to the next sidecar turn. Consume the front turn
                    // even on a text mismatch — leaving it stuck would mis-pair
                    // every later attachment message and cascade the desync.
                    if (turn.value(QStringLiteral("text")).toString() != userText) {
                        qWarning("AgentPanel: replay attachment turn text mismatch "
                                 "(front sidecar didn't match user message); "
                                 "pairing positionally to keep the FIFO in sync");
                    }
                    m_replayAttachTurns.removeFirst();
                }
                addYouCard(userText, attachments);
            }
        }
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            if (b.value(QStringLiteral("type")).toString() == QLatin1String("tool_result")) {
                const QString id = b.value(QStringLiteral("tool_use_id")).toString();
                const int key = m_toolRows.value(id, -1);
                if (key >= 0) {
                    // Clip very long results to keep the row cheap to lay out;
                    // the "Show full output" affordance expands them on demand.
                    QString full =
                        agentkate::toolResultText(b.value(QStringLiteral("content"))).trimmed();
                    const bool truncated = full.size() > kToolResultDisplayClip;
                    QString shown = truncated ? full.left(kToolResultDisplayClip) : full;
                    // Cap the retained copy so a giant result can't bloat the
                    // transcript; the on-disk transcript keeps the true full text.
                    if (full.size() > kToolResultStoreCap) {
                        full.truncate(kToolResultStoreCap);
                    }
                    m_model->setToolResult(key, shown, full, truncated);
                    // A Workflow launch result carries the run's Task ID /
                    // Transcript dir / Run ID — capture it as this thread's latest
                    // followable workflow and reveal the chip.
                    if (m_workflowToolKeys.remove(key)) {
                        noteWorkflowLaunch(m_workflowInputByKey.take(key), full);
                    }
                    // A tool_use has exactly one tool_result; the mapping is dead
                    // once applied. Dropping it bounds m_toolRows and lets the key
                    // fall away with the row when it is eventually evicted.
                    m_toolRows.remove(id);
                }
            }
        }

    } else if (type == QLatin1String("result")) {
        const bool err = ev.value(QStringLiteral("is_error")).toBool();
        // The result event carries the turn's usage + billed cost verbatim from
        // the `claude` CLI (field names match the Anthropic Messages API). Show
        // a compact per-turn line and fold it into the running session totals.
        const QJsonObject usage = ev.value(QStringLiteral("usage")).toObject();
        const qlonglong inTok = usage.value(QStringLiteral("input_tokens")).toVariant().toLongLong();
        const qlonglong outTok = usage.value(QStringLiteral("output_tokens")).toVariant().toLongLong();
        const qlonglong cacheRead =
            usage.value(QStringLiteral("cache_read_input_tokens")).toVariant().toLongLong();
        const qlonglong cacheCreate =
            usage.value(QStringLiteral("cache_creation_input_tokens")).toVariant().toLongLong();
        const double costUsd = ev.value(QStringLiteral("total_cost_usd")).toDouble();
        const qlonglong durationMs =
            ev.value(QStringLiteral("duration_ms")).toVariant().toLongLong();
        const bool haveUsage = inTok || outTok || cacheRead || cacheCreate || costUsd > 0.0;

        QString head = err ? i18n("turn ended with an error") : i18n("turn complete");
        // Tools the human declined this turn — worth a visible trace, since a
        // denial usually explains why the agent changed course.
        const int denied = ev.value(QStringLiteral("permission_denials")).toArray().size();
        if (haveUsage) {
            const QLocale loc;
            const qlonglong promptTotal = inTok + cacheRead + cacheCreate;
            const int cacheHitPct =
                promptTotal > 0 ? int((cacheRead * 100) / promptTotal) : 0;
            // e.g. "turn complete · 1,204 in / 318 out · 86% cache hit · $0.0042 · 3.1s"
            QString line = i18nc("turn-usage summary",
                                 "%1 · %2 in / %3 out · %4% cache hit",
                                 head, loc.toString(inTok), loc.toString(outTok),
                                 cacheHitPct);
            if (costUsd > 0.0) {
                line += i18nc("turn cost suffix", " · $%1", loc.toString(costUsd, 'f', 4));
            }
            if (durationMs > 0) {
                line += i18nc("turn duration suffix", " · %1s",
                              loc.toString(durationMs / 1000.0, 'f', 1));
            }
            if (denied > 0) {
                line += i18ncp("denied-permissions suffix", " · %1 tool denied",
                               " · %1 tools denied", denied);
            }
            addNote(line.toHtmlEscaped(), err ? QStringLiteral("err") : QStringLiteral("dim"));
            // Accumulate session totals — but never while replaying the
            // transcript, or historical turns would be double-counted.
            if (!m_replaying) {
                m_sessionCostUsd += costUsd;
                m_sessionInTokens += inTok;
                m_sessionOutTokens += outTok;
            }
        } else {
            addNote(err ? QStringLiteral("✗ ") + head : QStringLiteral("✓ ") + head,
                    err ? QStringLiteral("err") : QStringLiteral("ok"));
        }
        // An error result often carries the CLI's explanation in `result`
        // (e.g. what error_max_turns or an API failure actually hit) — text no
        // assistant event ever showed. Surface it instead of dropping it.
        if (err) {
            const QString why = ev.value(QStringLiteral("result")).toString().trimmed();
            if (!why.isEmpty()) {
                addNote(why.toHtmlEscaped(), QStringLiteral("err"));
            }
        }
        m_idle = true;
        refresh();
        // The turn boundary is the moment a queued follow-up can fire.
        drainSendQueue();

    } else if (type == QLatin1String("_stderr")) {
        addNote(ev.value(QStringLiteral("text")).toString().toHtmlEscaped(),
                QStringLiteral("dim"));

    } else if (type == QLatin1String("_lifecycle")) {
        const QString phase = ev.value(QStringLiteral("phase")).toString();
        const QString detail = ev.value(QStringLiteral("detail")).toString().toHtmlEscaped();
        if (phase == QLatin1String("started")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            m_workdir = ev.value(QStringLiteral("workdir")).toString();
            emit worktreePathChanged(worktreePath());
            m_errored = false; // a clean start clears any prior failure state
            addNote(detail, QStringLiteral("sys"));
            refresh();
        } else if (phase == QLatin1String("resumed")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            m_workdir = ev.value(QStringLiteral("workdir")).toString();
            emit worktreePathChanged(worktreePath());
            m_dormant = false;
            m_idle = true;
            m_errored = false; // resuming clears any prior failure state
            // A resumed process bills a fresh session — restart the meter so the
            // header doesn't show a stale/zero cost as new turns accrue.
            m_sessionCostUsd = 0.0;
            m_sessionInTokens = 0;
            m_sessionOutTokens = 0;
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
            m_workdir = ev.value(QStringLiteral("workdir")).toString();
            emit worktreePathChanged(worktreePath());
            m_promoting = false;
            addNote(detail, QStringLiteral("sys"));
            refresh();
        } else if (phase == QLatin1String("turn_aborted")) {
            // Interrupt landed in-band: the turn was cancelled but the process
            // stays resident and the session stays hot. Reset to idle and keep
            // the composer live — the next Send goes down the same stdin with no
            // resume cost. Clear any pending permission prompt for the dead turn.
            addNote(QStringLiteral("&#9209; interrupted — session kept, "
                                   "send a follow-up to continue"),
                    QStringLiteral("sys"));
            m_idle = true;
            m_permQueue.clear();
            m_permBar->setVisible(false);
            m_questionBox->setVisible(false);
            refresh();
            // A follow-up queued during the interrupt can fire now.
            drainSendQueue();
        } else if (phase == QLatin1String("error")) {
            addNote(QStringLiteral("agent failed: %1").arg(detail), QStringLiteral("err"));
            m_idle = false;
            m_promoting = false;
            m_errored = true; // roster card shows Error until the next send/resume
            if (!m_dormant) {
                m_threadId.clear(); // a fresh start failed — back to a blank panel
            }
            // A follow-up queued during the failed turn can never drain now —
            // hand the text back to the composer instead of stranding it.
            if (!m_sendQueue.isEmpty()) {
                restoreQueuedToComposer();
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
            // The session stopped before the queued follow-ups could fire.
            // Don't discard the human's text — put it back in the composer so
            // it can be re-sent (into a resumed session) with one keystroke.
            if (!m_sendQueue.isEmpty()) {
                restoreQueuedToComposer();
                addNote(QStringLiteral("agent stopped — your queued message is "
                                       "back in the composer"),
                        QStringLiteral("dim"));
            }
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
