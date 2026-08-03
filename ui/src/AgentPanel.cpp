#include "AgentPanel.h"
#include "AgentCardDelegate.h"
#include "AgentChatHelpers.h"
#include "AttachmentBuilder.h"
// For IsolationCopy — the shared isolation wording (see NewAgentDialog.h). This
// panel shows no dialog from that header; it uses the copy, which lives beside
// the probe that decides which of it is true.
#include "NewAgentDialog.h"
#include "SafeContent.h"
#include "ImageView.h"
#include "ProviderConfig.h"
#include "SubAgentTranscriptDialog.h"
#include "ToolInspectorDialog.h"
#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "WorkflowMonitor.h"
#include "WorkflowMonitorDialog.h"
#include "ipc/CoreClient.h"
#include "shell/FlowLayout.h"
#include "state/EngineAvailability.h"
#include "state/RateLimitState.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KMessageWidget>
#include <KSharedConfig>

#include <QAbstractButton>
#include <QClipboard>
#include <QCryptographicHash>
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
#include <QImage>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonDocument>
#include <QKeyEvent>
#include <QLabel>
#include <QLayout>
#include <QListView>
#include <QListWidget>
#include <QStandardPaths>
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
#include <QStandardItemModel>
#include <QStyleOptionViewItem>
#include <QStyledItemDelegate>
#include <QMenu>
#include <QMessageBox>
#include <QPointer>
#include <QTextDocument>
#include <QTimer>
#include <QToolButton>
#include <QVariant>
#include <QVBoxLayout>

#include <utility>
#include <QWidgetAction>

#include <functional>
#include <utility>

namespace {
// Grey out one row of a combo. QComboBox has no per-entry enabled flag; its
// default model is a QStandardItemModel, so the item carries it — the idiom
// applyModelEffortSupport below already uses for efforts a model cannot run.
void setComboEntryEnabled(QComboBox *combo, int index, bool on,
                          const QString &whyNot = QString())
{
    auto *model = qobject_cast<QStandardItemModel *>(combo->model());
    QStandardItem *item = model ? model->item(index) : nullptr;
    if (!item) {
        return; // a custom model: annotated but selectable, which is still honest
    }
    item->setEnabled(on);
    if (!on && !whyNot.isEmpty()) {
        item->setToolTip(whyNot);
    }
}

// Move off a refused entry. A combo whose current row is disabled still SHOWS
// it and still reports its data, so leaving the selection there would hand the
// launch exactly the choice the row was disabled to prevent.
void selectFirstEnabled(QComboBox *combo)
{
    auto *model = qobject_cast<QStandardItemModel *>(combo->model());
    if (!model) {
        return;
    }
    const int cur = combo->currentIndex();
    if (cur >= 0 && model->item(cur) && model->item(cur)->isEnabled()) {
        return;
    }
    for (int i = 0; i < combo->count(); ++i) {
        if (model->item(i) && model->item(i)->isEnabled()) {
            combo->setCurrentIndex(i);
            return;
        }
    }
    // Nothing is startable at all — the choice is moot and the roster's
    // missing-engines banner is the surface that says why.
}

// Custom drag MIME carrying per-hit line ranges, mirrored in SearchPanel.cpp.
constexpr char kAttachMime[] = "application/x-agentkate-attachment+json";

// Tool-result clipping. The transcript shows the first kToolResultDisplayClip
// chars inline and reveals more via "Show full output". The retained copy is
// itself capped at kToolResultStoreCap so a single huge result (a big Read, a
// verbose command, an AT-SPI page dump) cannot grow the in-RAM transcript
// without bound — the on-disk transcript always keeps the true full text.
constexpr int kToolResultDisplayClip = 4000;
constexpr int kToolResultStoreCap = 128 * 1024;

// Characters of tool summary the permission bar shows before it elides. The
// core builds its worker-launch prompt to exactly this budget (audit F1), so
// the two must stay in step. Whatever the clip drops is reachable in full via
// the bar's Details… view (audit F28).
constexpr int kPermSummaryBudget = 240;

// How many sent messages the composer's Up-arrow history keeps. Session-only
// (never written to disk — a prompt can hold anything the human typed).
constexpr int kComposerHistoryMax = 50;

// Attachment / frame budgets, both anchored on the core's 16 MB JSON-RPC frame
// cap (core/internal/ipc/server.go maxFrameBytes). An oversize frame never
// reaches a handler, so the message is simply lost — the human's text and files
// have to be refused HERE, where they can still be edited.
//
// kMaxTotalAttachBytes mirrors AttachmentBuilder's budget for the paths where
// attachments are MERGED rather than built — two queued messages that were each
// within budget union past it. kMaxSendFrameBytes is the last line: the whole
// request, message text included, measured as it will actually be serialized.
//
// Ordering invariant: kMaxSendFrameBytes < CoreClient::kMaxFrameBytes (15 MiB)
// < the core's maxFrameBytes (16 MiB). These guards must nest strictly, and this
// one measures only `params` while CoreClient measures the whole JSON-RPC frame
// (jsonrpc/id/method wrapper plus the newline, ~64 bytes more) — at an equal
// limit a request accepted here would be refused by the transport, which is the
// failure mode this guard exists to prevent.
constexpr qsizetype kMaxTotalAttachBytes = 12 * 1024 * 1024;
constexpr qsizetype kMaxSendFrameBytes = 14 * 1024 * 1024;

// The wire cost of one already-built attachment object. Deliberately measures
// the serialized object rather than just its body, so the metadata (name, path,
// cachePath) is counted too.
qsizetype attachmentWireCost(const QJsonObject &att)
{
    return QJsonDocument(att).toJson(QJsonDocument::Compact).size();
}

bool isDark(const QWidget *w)
{
    return w->palette().color(QPalette::Base).lightness() < 128;
}

// The suffixes AttachmentBuilder recognises as images. Used only to decide
// whether a pasted/dropped FILE is worth routing as an image attachment; the
// builder still owns the media-type mapping and every size check.
bool isImagePath(const QString &path)
{
    static const QSet<QString> exts{
        QStringLiteral("png"),  QStringLiteral("jpg"), QStringLiteral("jpeg"),
        QStringLiteral("gif"),  QStringLiteral("webp"), QStringLiteral("bmp")};
    return exts.contains(QFileInfo(path).suffix().toLower());
}

// True when a paste/drag carries an image at all: pixels (a Spectacle capture,
// an image dragged out of a browser — no file exists anywhere) or image files
// by URL. Deliberately not "hasUrls", so pasting a text file's URL still pastes.
bool mimeHasImagePayload(const QMimeData *mime)
{
    if (!mime) {
        return false;
    }
    if (mime->hasImage()) {
        return true;
    }
    const auto urls = mime->urls();
    for (const QUrl &u : urls) {
        if (u.isLocalFile() && isImagePath(u.toLocalFile())) {
            return true;
        }
    }
    return false;
}

// rawImageBeatsText decides, for a clipboard offering BOTH pixels and text,
// which one the user meant. Offering both is the norm, not the exception:
// LibreOffice, most office suites and most drawing apps put a rendered bitmap of
// the selection next to its real text form, so treating hasImage() as decisive
// swallows the text — a copied cell range pastes as a picture of itself.
//
// The image wins only when the text cannot be the payload: empty, or the single
// URL/path token a browser puts alongside a right-click-Copy-Image. Anything
// with a line break or internal whitespace is real text and pastes as text.
bool rawImageBeatsText(const QMimeData *mime)
{
    if (!mime->hasText()) {
        return true;
    }
    const QString text = mime->text().trimmed();
    if (text.isEmpty()) {
        return true;
    }
    for (const QChar c : text) {
        if (c.isSpace()) { // covers the line breaks of a multi-line copy too
            return false;
        }
    }
    if (text.startsWith(QLatin1Char('/'))) {
        return true; // a bare absolute path, as a file manager offers alongside
    }
    // A known scheme only. "Any scheme longer than one character" made every
    // colon-bearing token a URL, so a single spreadsheet cell holding "ratio:16"
    // pasted as a picture of itself.
    static const QSet<QString> kUrlSchemes{
        QStringLiteral("http"),  QStringLiteral("https"), QStringLiteral("file"),
        QStringLiteral("ftp"),   QStringLiteral("sftp"),  QStringLiteral("data")};
    return kUrlSchemes.contains(QUrl(text).scheme());
}

// ComposerEdit is the chat input. QPlainTextEdit pastes an image as its text
// form — which for pixels is nothing at all, and for an image file is a path
// dumped into the message — so a paste carrying an image is handed to the panel
// to attach instead. The handler returns false for everything it does not take,
// and that falls through to the base class, so text paste is untouched.
// No Q_OBJECT: it has no signals or slots, and this TU is not moc'd for it.
class ComposerEdit : public QPlainTextEdit
{
public:
    using QPlainTextEdit::QPlainTextEdit;

    void setImageHandler(std::function<bool(const QMimeData *)> handler)
    {
        m_handler = std::move(handler);
    }

protected:
    bool canInsertFromMimeData(const QMimeData *source) const override
    {
        return mimeHasImagePayload(source)
            || QPlainTextEdit::canInsertFromMimeData(source);
    }

    void insertFromMimeData(const QMimeData *source) override
    {
        if (m_handler && m_handler(source)) {
            return;
        }
        QPlainTextEdit::insertFromMimeData(source);
    }

private:
    std::function<bool(const QMimeData *)> m_handler;
};

// Role carrying a slash command's argument hint on its popup item.
constexpr int kSlashHintRole = Qt::UserRole + 1;

// SlashItemDelegate paints the autocomplete row's argument hint ("<branch>")
// after the command in the disabled text colour, so the part the user still has
// to supply reads as a placeholder rather than as part of the command name.
// QListWidget items are plain text — hence a delegate rather than rich text.
class SlashItemDelegate : public QStyledItemDelegate
{
public:
    using QStyledItemDelegate::QStyledItemDelegate;

protected:
    void paint(QPainter *painter, const QStyleOptionViewItem &option,
               const QModelIndex &index) const override
    {
        const QString hint = index.data(kSlashHintRole).toString();
        if (hint.isEmpty()) {
            QStyledItemDelegate::paint(painter, option, index);
            return;
        }
        // Draw the row normally, then the hint in the leftover width. The main
        // text is elided to make room, so a long description never overpaints
        // the hint or vice versa.
        QStyleOptionViewItem opt = option;
        initStyleOption(&opt, index);
        const int hintW = opt.fontMetrics.horizontalAdvance(hint)
            + opt.fontMetrics.horizontalAdvance(QLatin1Char(' '));
        QStyleOptionViewItem main = opt;
        main.rect.setRight(qMax(main.rect.left(), main.rect.right() - hintW));
        QStyledItemDelegate::paint(painter, main, index);

        painter->save();
        painter->setPen(opt.palette.color(QPalette::Disabled, QPalette::Text));
        painter->drawText(QRect(main.rect.right(), opt.rect.top(), hintW, opt.rect.height()),
                          Qt::AlignVCenter | Qt::AlignRight, hint);
        painter->restore();
    }
};

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

// compactionOutcome renders an agent.compactNow reply as one line of feed text.
// Every call site shares it so no future one drifts back into claiming a turn
// count that does not exist: an engine that rewrites its own context in place
// stores NO summary, so it has no turns and no body to report, and printing
// "(0 turns, 0 bytes)" would read as a no-op rather than a success. The core's
// `compactedInPlace` flag is what tells the two apart.
QString compactionOutcome(const QJsonObject &res)
{
    if (res.value(QStringLiteral("compactedInPlace")).toBool()) {
        return i18n("compacted in place — same session, no summary stored.");
    }
    const int turns = res.value(QStringLiteral("turns")).toInt();
    const int bytes = res.value(QStringLiteral("bodyBytes")).toInt();
    const QString strategy =
        res.value(QStringLiteral("strategy")).toString().toHtmlEscaped();
    if (strategy.isEmpty()) {
        return i18n("compacted (%1 turns, %2 bytes).", turns, bytes);
    }
    return i18n("compacted via %1 (%2 turns, %3 bytes).", strategy, turns, bytes);
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
            ++m_ticks;
            // The message text only rotates every 48 ticks; the elapsed clock
            // needs a full repaint about once a second (14 ticks ≈ 0.98s).
            // Other ticks invalidate just the small spinner rect so the wide
            // message isn't re-rendered every 70ms.
            if (m_ticks % 48 == 0) {
                ++m_genericIndex;
                update();
            } else if (m_startedMs > 0 && m_ticks % 14 == 0) {
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

    // The current turn's start time (epoch ms). Latched by the panel at send
    // time — NOT on setActive, which also toggles around permission prompts
    // and would restart the clock mid-turn. 0 hides the elapsed readout.
    void setTurnStart(qint64 epochMs)
    {
        m_startedMs = epochMs;
        update();
    }

    // Average of this session's past turn durations; 0 hides the suffix. An
    // honest "running 4m · turns avg 2m" beats a fake ETA.
    void setAverageTurnMs(qint64 ms)
    {
        m_avgMs = ms;
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

        // Right-aligned honest timing: elapsed this turn, plus the session's
        // average turn length once known. No ETAs — just what's measured.
        QString timing;
        if (m_startedMs > 0) {
            const qint64 elapsed = QDateTime::currentMSecsSinceEpoch() - m_startedMs;
            if (elapsed >= 3000) {
                timing = fmtDur(elapsed);
                if (m_avgMs > 0) {
                    timing = i18nc("working indicator timing: elapsed, average",
                                   "%1 · turns avg %2", timing, fmtDur(m_avgMs));
                }
            }
        }
        int rightW = 0;
        if (!timing.isEmpty()) {
            QColor dim = textCol;
            dim.setAlpha(140);
            p.setPen(dim);
            const QFontMetrics fm(font());
            rightW = fm.horizontalAdvance(timing) + 10;
            p.drawText(QRect(width() - rightW, 0, rightW - 4, height()),
                       Qt::AlignVCenter | Qt::AlignRight, timing);
            p.setPen(textCol);
        }
        const QFontMetrics fm(font());
        p.drawText(QRect(textX, 0, width() - textX - rightW, height()),
                   Qt::AlignVCenter | Qt::AlignLeft,
                   fm.elidedText(msg, Qt::ElideRight, width() - textX - rightW));
    }

private:
    // fmtDur renders a duration the way a person says it: "42s", "2m 10s",
    // "1h 12m".
    static QString fmtDur(qint64 ms)
    {
        const qint64 secs = ms / 1000;
        if (secs < 60) {
            return i18nc("duration in seconds", "%1s", secs);
        }
        if (secs < 3600) {
            return i18nc("duration minutes+seconds", "%1m %2s", secs / 60, secs % 60);
        }
        return i18nc("duration hours+minutes", "%1h %2m", secs / 3600,
                     (secs % 3600) / 60);
    }

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
    qint64 m_startedMs = 0; // current turn's start (epoch ms); 0 = no readout
    qint64 m_avgMs = 0;     // average past turn duration; 0 = unknown
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
    // StrongFocus with NoSelection retained (plan 27 §4): the transcript is the
    // core content of the app, and NoFocus made it unreachable by Tab, dead to
    // PgUp/PgDn/arrows and silent under Orca. Focus enables keyboard scrolling
    // and lets the accessibility layer walk the rows (the model serves
    // Qt::AccessibleTextRole) without changing the read-only interaction model.
    m_view->setFocusPolicy(Qt::StrongFocus);
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

    // Empty state (audit F44). Every secondary panel — Jobs, Cooperation, Agent
    // Activity, Cowork, the roster — tells a first-time user what it is for
    // while it is empty; the one panel they actually land on was a blank box.
    // A label over the viewport rather than a seeded note, so a restored agent's
    // replayed transcript never has to step around it and it cannot survive
    // into a conversation. Text is composed lazily (updateFeedEmptyState) — it
    // names whichever isolation the picker is currently on, and that picker is
    // built further down this constructor.
    m_feedEmptyHint = new QLabel(m_view->viewport());
    m_feedEmptyHint->setAlignment(Qt::AlignCenter);
    m_feedEmptyHint->setWordWrap(true);
    m_feedEmptyHint->setTextFormat(Qt::RichText);
    // Bind the foreground ROLE, not a baked colour, so it follows a runtime
    // Breeze light/dark switch (same rule as the roster's hint).
    m_feedEmptyHint->setForegroundRole(QPalette::PlaceholderText);
    m_feedEmptyHint->setAttribute(Qt::WA_TransparentForMouseEvents);
    m_feedEmptyHint->setVisible(false);
    connect(m_model, &QAbstractItemModel::rowsInserted, this,
            &AgentPanel::updateFeedEmptyState);
    connect(m_model, &QAbstractItemModel::rowsRemoved, this,
            &AgentPanel::updateFeedEmptyState);
    connect(m_model, &QAbstractItemModel::modelReset, this,
            &AgentPanel::updateFeedEmptyState);

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
            [this](const QString &href) {
                if (!href.isEmpty()) {
                    // Model-authored href: scheme policy, not the OS handler
                    // (audit F14). The link text the human clicked is not
                    // evidence of where it goes.
                    agentkate::openModelLink(this, QUrl(href));
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
    // SECURITY (audit F28): the label is a clipped one-liner, so the bar must
    // also offer the FULL request. Without it the only answer to "what is the
    // rest of that command?" was Deny.
    m_permDetails = new QPushButton(i18n("Details…"), m_permBar);
    m_permDetails->setCursor(Qt::PointingHandCursor);
    m_permDetails->setToolTip(i18n("Show the complete, unabridged tool input"));
    m_permDeny = new QPushButton(QStringLiteral("Deny"), m_permBar);
    m_permDeny->setCursor(Qt::PointingHandCursor);
    m_permAllow = new QPushButton(QStringLiteral("Approve"), m_permBar);
    m_permAllow->setCursor(Qt::PointingHandCursor);
    auto *permLayout = new QHBoxLayout(m_permBar);
    permLayout->setContentsMargins(10, 8, 10, 8);
    permLayout->addWidget(m_permLabel, 1);
    permLayout->addWidget(m_permDetails);
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

    auto *composer = new ComposerEdit(this);
    // Ctrl+V with a screenshot on the clipboard attaches it rather than pasting
    // an empty string; text paste is unaffected (see ComposerEdit).
    composer->setImageHandler(
        [this](const QMimeData *source) { return handleComposerPaste(source); });
    m_input = composer;
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

    // Token-by-token text arrives one delta at a time; the feed is virtualized
    // and each row change costs a re-measure, so deltas accumulate in
    // m_streamBlocks and land on their rows on this tick. 50 ms reads as
    // continuous typing while capping a fast stream at 20 repaints a second.
    m_streamFlush = new QTimer(this);
    m_streamFlush->setSingleShot(true);
    m_streamFlush->setInterval(50);
    connect(m_streamFlush, &QTimer::timeout, this, &AgentPanel::flushStreamedText);
    connect(m_input, &QPlainTextEdit::textChanged, this, [this] {
        updateSlashPopup();
        // Typing leaves the history walk (audit F50) — the edited text is the
        // human's again, not an entry to keep stepping through. The guard keeps
        // OUR own programmatic setPlainText from cancelling the walk it makes.
        if (m_historyNavigating) {
            // …and it must not persist the walked-to entry as the DRAFT either.
            // Walking history and closing the panel used to replace the unsent
            // message the user had been writing with an old sent one — the
            // history feature eating the very draft the draft feature exists to
            // protect. The walk is transient; only real edits are saved (the
            // Down that ends the walk restores the draft, which is what is
            // already on disk).
            return;
        }
        m_draftTimer->start();
        m_historyIndex = -1;
        m_historyDraft.clear();
    });

    // Slash-command autocomplete popup: an overlay list above the composer,
    // fed by the harness's own command list (see updateSlashPopup). Palette
    // colours only; it inherits the theme like every other child widget.
    m_slashPopup = new QListWidget(this);
    m_slashPopup->setVisible(false);
    m_slashPopup->setFrameShape(QFrame::StyledPanel);
    m_slashPopup->setSelectionMode(QAbstractItemView::SingleSelection);
    m_slashPopup->setHorizontalScrollBarPolicy(Qt::ScrollBarAlwaysOff);
    m_slashPopup->setFocusPolicy(Qt::NoFocus); // keys stay in the composer
    m_slashPopup->setItemDelegate(new SlashItemDelegate(m_slashPopup));
    connect(m_slashPopup, &QListWidget::itemClicked, this,
            [this](QListWidgetItem *) { acceptSlashCompletion(); });

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
    m_modeCombo->setToolTip(QStringLiteral(
        "How much the agent checks with you before it acts. You can change it\n"
        "while the agent runs; it applies from the next action."));
    rebuildModeCombo();
    connect(m_modeCombo, &QComboBox::currentIndexChanged, this, [this] {
        const QString mode = m_modeCombo->currentData().toString();
        // Sticky per harness: the last choice becomes the default for the next
        // agent — except the auto-approve-everything choices (claude's
        // bypassPermissions and dontAsk, kimi's yolo), which are never re-armed
        // accidentally on the next conversation.
        if (mode != QLatin1String("bypassPermissions") && mode != QLatin1String("dontAsk")
            && mode != QLatin1String("yolo")) {
            KSharedConfig::openConfig()
                ->group(QStringLiteral("Agent"))
                .writeEntry(currentTraits().stickyModeKey(), mode);
        }
        maybePushOption(QStringLiteral("permissionMode"), mode);
    });

    m_isolationCombo = new QComboBox(this);
    // The wording is IsolationCopy's, not this file's (audit F30/F49). Three
    // controls choose isolation and this is the one people actually use — the
    // Ctrl+N path — so it was the one still calling "auto" "Automatic … a
    // private copy when the project is a git repo", which is false for a git
    // repo with nothing committed: there "auto" hands the agent the user's own
    // files. Every label and the tooltip now come from the shared namespace, so
    // this combo, the guided dialog and the ensemble editor say one thing.
    for (const char *mode : {"auto", "isolated", "workspace"}) {
        const QString id = QString::fromLatin1(mode);
        m_isolationCombo->addItem(IsolationCopy::modeLabel(id), id);
    }
    m_isolationCombo->setToolTip(IsolationCopy::modeTooltip());
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
        // The empty-state hint describes what will happen to the user's files
        // when they send (audit F44), and it reads this very combo. Without
        // this, changing isolation left the previous promise on screen — a
        // stale sentence about the user's own files is exactly the falsehood
        // F44's copy rule was written to avoid.
        updateFeedEmptyState();
    });

    m_effortCombo = new QComboBox(this);
    m_effortCombo->setToolTip(QStringLiteral(
        "How long the agent thinks before it acts. Higher is more thorough but\n"
        "slower. Default leaves the model's own configured level untouched.\n"
        "Some engines can change it while the agent runs; others fix it at start."));
    rebuildEffortCombo();
    connect(m_effortCombo, &QComboBox::currentIndexChanged, this, [this] {
        const QString effort = m_effortCombo->currentData().toString();
        // Sticky per harness: the last choice becomes the default next time.
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry(currentTraits().stickyEffortKey(), effort);
        maybePushOption(QStringLiteral("effort"), effort);
    });

    // Engine selector: ONE "who runs this agent" picker. Each entry is a
    // harness (Claude Code, Kimi Code, … — from the core's capability
    // registry), optionally overlaid with a third-party API provider for
    // harnesses that support routing ("Claude Code via Fireworks"). Fixed once
    // the agent starts. The mode/effort/model pickers follow this choice.
    m_engineCombo = new QComboBox(this);
    m_engineCombo->setToolTip(QStringLiteral(
        "Which agent engine runs this thread, fixed once it starts: the agent\n"
        "program, optionally routed at a third-party Anthropic-compatible API\n"
        "with your stored key. Manage providers in Options ▸ Configure API\n"
        "Providers…"));
    rebuildEngineCombo();
    connect(m_engineCombo, &QComboBox::currentIndexChanged, this, [this] {
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .writeEntry("engine", m_engineCombo->currentData().toString());
        // A discovered-model engine may have no cached option lists yet —
        // probe once now so the pickers fill before the agent ever starts.
        // The HarnessRegistry::changed connection below rebuilds the combos
        // when the result lands.
        HarnessRegistry::self()->ensureDiscovered(m_core, selectedHarnessId());
        HarnessRegistry::self()->ensureModels(m_core, selectedHarnessId(), selectedProviderId());
        rebuildModelCombo();
        rebuildModeCombo();
        rebuildEffortCombo();
        refresh();
    });
    // The mode/effort combos were first built before the engine combo existed
    // (their ctor order); re-run them now the restored engine is known, so a
    // sticky non-default harness shows its own mode/thinking lists.
    rebuildModeCombo();
    rebuildEffortCombo();
    // A late harness-list fetch can revise the engine list (a new
    // harness, changed traits) — rebuild the pickers while they are still
    // free; a bound thread's pickers re-evaluate on the next refresh().
    connect(HarnessRegistry::self(), &HarnessRegistry::changed, this, [this] {
        if (m_threadId.isEmpty()) {
            rebuildEngineCombo();
            rebuildModelCombo();
            rebuildModeCombo();
            rebuildEffortCombo();
        }
        refresh();
    });
    // A sticky discovered-model engine restored above bypasses the change
    // handler — probe its option lists now too (a no-op for "tiers" engines,
    // a cached vocabulary, or a core that is not connected yet).
    HarnessRegistry::self()->ensureDiscovered(m_core, selectedHarnessId());
    HarnessRegistry::self()->ensureModels(m_core, selectedHarnessId(), selectedProviderId());

    // Model selector. For Claude direct each item carries a tier token the core
    // resolves to a concrete --model id (its resolveModel is the single source of
    // truth, so the UI never hard-codes versioned model strings); for a provider
    // the items are that provider's own model ids, sent verbatim. An empty token
    // passes no --model flag. Fixed once the agent starts. rebuildModelCombo()
    // populates it to match the selected provider.
    m_modelCombo = new QComboBox(this);
    m_modelCombo->setToolTip(QStringLiteral(
        "Model for this agent. You can switch it while the agent runs; the\n"
        "new model takes over from the next message.\n"
        "Default leaves the provider's own configured/main model untouched."));
    connect(m_modelCombo, &QComboBox::currentIndexChanged, this, [this] {
        // Only the direct tier choice is sticky; a provider's model ids must
        // not be persisted as the tier token, and neither must a discovered
        // free-text model (an editable combo).
        if (!m_modelCombo->isEditable()
            && selectedProviderId() == ProviderStore::directId()) {
            KSharedConfig::openConfig()
                ->group(QStringLiteral("Agent"))
                .writeEntry("model", m_modelCombo->currentData().toString());
        }
        maybePushOption(QStringLiteral("model"), currentModel());
        // A different model may support a different set of thinking efforts.
        applyModelEffortSupport();
    });
    rebuildModelCombo();

    // Cowork desktop access. The agentkate-cowork MCP server is wired into every
    // agent, but stays empty of tools until this is ticked — so unlike engine or
    // isolation, it can be changed WHILE the agent runs, and the tools appear (or
    // vanish) in place. Deliberately NOT sticky: standing desktop access by default
    // would be a footgun — the user opts in per agent. Making the tools available is
    // harmless on its own; every action is still gated by the consent prompts and the
    // Cowork panel toggles.
    m_coworkCheck = new QCheckBox(QStringLiteral("See && control my desktop (Cowork)"), this);
    m_coworkCheck->setToolTip(QStringLiteral(
        "Give this agent the Cowork desktop tools (see windows, screenshot, read the\n"
        "screen, click controls, type). Can be turned on or off while the agent runs.\n"
        "Turning it on also asks the desktop for screen and input permission right\n"
        "away. Every action still needs your consent or a Cowork panel toggle."));
    connect(m_coworkCheck, &QCheckBox::toggled, this, &AgentPanel::onCoworkToggled);

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

    // Background-work tray: one chip per CLI background task (a
    // run_in_background shell or an async subagent). Hidden until a task
    // starts; chips flip to ✓ when their task completes.
    m_jobsBar = new QFrame(this);
    m_jobsFlow = new FlowLayout(m_jobsBar, 0, 6, 6);
    m_jobsBar->setVisible(false);
    // Job rows are keyed on the thread, so a panel that gains or changes one has
    // to re-publish immediately — otherwise the retained records would surface
    // under the new id only at the next task event, while the old id's rows sat
    // in the Jobs panel forever.
    connect(this, &AgentPanel::threadIdChanged, this, &AgentPanel::updateJobsBar);

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
        form->addRow(QStringLiteral("Engine"), m_engineCombo);
        form->addRow(QStringLiteral("When to ask"), m_modeCombo);
        form->addRow(QStringLiteral("Where it works"), m_isolationCombo);
        form->addRow(QStringLiteral("Thinking effort"), m_effortCombo);
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

    // "Helpers ▾" — the conversations this agent's subagents had, which live in
    // their own files rather than in this transcript (plan 16 P6). The button
    // only appears for an engine that writes them, and its menu is rebuilt on
    // open from the core, since subagents appear as the agent delegates.
    m_subagentsBtn = new QToolButton(this);
    m_subagentsBtn->setText(QStringLiteral("Helpers"));
    m_subagentsBtn->setIcon(QIcon::fromTheme(QStringLiteral("system-users")));
    m_subagentsBtn->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_subagentsBtn->setPopupMode(QToolButton::InstantPopup);
    m_subagentsBtn->setCursor(Qt::PointingHandCursor);
    m_subagentsBtn->setToolTip(QStringLiteral(
        "Open the conversation one of this agent's own helper agents had.\n"
        "They run in their own context, so their work is not in this transcript."));
    auto *subagentMenu = new QMenu(this);
    m_subagentsBtn->setMenu(subagentMenu);
    connect(subagentMenu, &QMenu::aboutToShow, this,
            [this, subagentMenu] { refreshSubagentMenu(subagentMenu); });

    // FlowLayout so the toolbar's buttons wrap onto a second row when the panel
    // is dragged narrow instead of clipping. No stretch — a flow layout has none.
    auto *buttons = new FlowLayout(0, 6, 6);
    buttons->addWidget(setupBtn);
    buttons->addWidget(compactionBtn);
    buttons->addWidget(m_attachBtn);
    buttons->addWidget(m_diffBtn);
    buttons->addWidget(m_subagentsBtn);
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
    body->addWidget(m_jobsBar);
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
    connect(m_permDetails, &QPushButton::clicked, this,
            &AgentPanel::showPermissionDetails);
    // Queued so a notification can never be delivered re-entrantly while this
    // panel is being torn down (deleteLater'd on agent/project removal). Qt
    // drops any still-queued events after the receiver is destroyed.
    connect(m_core, &CoreClient::notification, this, &AgentPanel::onNotification,
            Qt::QueuedConnection);
    // The shared usage-limit state runs the one timer that fires when a window
    // rolls over or an armed resume comes due — the only two moments this
    // panel's parked state changes with no event of its own to hang off (a
    // parked agent emits nothing until it is unparked). Repaint only when OUR
    // answer actually moved: changed() fires for every agent's report, and this
    // panel's header must not re-layout on the whole fleet's traffic. `this` is
    // the connection context, so nothing is delivered after destruction.
    connect(agentkate::RateLimitState::self(), &agentkate::RateLimitState::changed,
            this, [this] {
                if (rateLimitParked() != m_rateParkedShown) {
                    refresh();
                }
            });

    applyChatSettings();
    refresh();
}

AgentPanel::~AgentPanel()
{
    // A closed agent cannot still be waiting on a usage window: leaving its
    // report behind would hold the roster's "N agents paused" strip up forever
    // (audit F43).
    if (!m_threadId.isEmpty()) {
        agentkate::RateLimitState::self()->forget(m_threadId);
    }
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
    // Rendered transcript content may show images from this project (plus the
    // attachment store, which is always allowed) and from nowhere else — see
    // SafeContent, audit F15.
    agentkate::allowMediaRoot(path);
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
    // A discovered-models combo is editable: a picked dropdown item shows its
    // display label ("K3 — kimi-code/k3") but must send its data value; text
    // that matches no item is a hand-typed model id, sent verbatim.
    const int idx = m_modelCombo->currentIndex();
    if (idx >= 0 && m_modelCombo->currentText() == m_modelCombo->itemText(idx)) {
        const QString data = m_modelCombo->itemData(idx).toString();
        if (!data.isEmpty()) {
            return data;
        }
        // A dataless current item is a stray hand-typed entry (or one Qt
        // auto-inserted): fall through and send its text verbatim.
    }
    return m_modelCombo->currentText().trimmed();
}

QString AgentPanel::currentEffort() const
{
    return m_effortCombo ? m_effortCombo->currentData().toString() : QString();
}

void AgentPanel::preselectBackend(const QString &backend)
{
    preselectEngine(backend, QString());
}

void AgentPanel::preselectEngine(const QString &backend, const QString &providerId)
{
    if (backend.isEmpty() || !m_threadId.isEmpty()) {
        return; // combo is frozen once a thread exists
    }
    QString data = backend;
    if (!providerId.isEmpty() && providerId != ProviderStore::directId()) {
        data += QLatin1Char('|') + providerId;
    }
    int idx = m_engineCombo->findData(data);
    if (idx < 0) {
        idx = m_engineCombo->findData(backend); // provider gone — bare harness
    }
    if (idx >= 0) {
        m_engineCombo->setCurrentIndex(idx);
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

// refreshSubagentMenu rebuilds the Helpers menu from the core each time it is
// opened: subagents appear as the agent delegates, so a menu built once would
// be stale by the time it is used.
void AgentPanel::refreshSubagentMenu(QMenu *menu)
{
    menu->clear();
    if (m_threadId.isEmpty() || !m_core || !m_core->isConnected()) {
        menu->addAction(i18n("(start the agent first)"))->setEnabled(false);
        return;
    }
    QAction *loading = menu->addAction(i18n("Looking…"));
    loading->setEnabled(false);
    QPointer<AgentPanel> self(this);
    QPointer<QMenu> liveMenu(menu);
    m_core->call(
        QStringLiteral("agent.subagentTranscripts"),
        QJsonObject{{QStringLiteral("threadId"), m_threadId}},
        [self, liveMenu](const QJsonObject &result, const QJsonObject &error) {
            if (!self || !liveMenu) {
                return;
            }
            liveMenu->clear();
            if (!error.isEmpty()) {
                liveMenu->addAction(error.value(QStringLiteral("message")).toString())
                    ->setEnabled(false);
                return;
            }
            const QJsonArray list = result.value(QStringLiteral("transcripts")).toArray();
            if (list.isEmpty()) {
                liveMenu->addAction(i18n("This agent has not used any helpers yet"))
                    ->setEnabled(false);
                return;
            }
            for (const QJsonValue &v : list) {
                const QJsonObject o = v.toObject();
                const QString id = o.value(QStringLiteral("id")).toString();
                const QString label = o.value(QStringLiteral("label")).toString();
                const QString path = o.value(QStringLiteral("path")).toString();
                const QString text =
                    label.isEmpty() ? id
                                    : i18nc("subagent menu entry: profile and id",
                                            "%1 (%2)", label, id);
                QAction *act = liveMenu->addAction(text);
                connect(act, &QAction::triggered, self, [self, path, text] {
                    if (!self) {
                        return;
                    }
                    auto *dlg = new SubAgentTranscriptDialog(path, text, self);
                    dlg->setAttribute(Qt::WA_DeleteOnClose);
                    dlg->show();
                });
            }
        },
        this);
}

void AgentPanel::preselectLaunchOptions(const QStringList &fallbackModels,
                                        const QStringList &disallowedTools,
                                        const QStringList &addDirs,
                                        bool strictMcpConfig, double maxBudgetUsd)
{
    if (!m_threadId.isEmpty()) {
        return; // the CLI is already running; these are launch-time only
    }
    m_fallbackModels = fallbackModels;
    m_disallowedTools = disallowedTools;
    m_addDirs = addDirs;
    m_strictMcpConfig = strictMcpConfig;
    m_maxBudgetUsd = maxBudgetUsd;
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

QString AgentPanel::composerText() const
{
    return m_input ? m_input->toPlainText() : QString();
}

bool AgentPanel::quickAsk(const QString &text)
{
    const QString ask = text.trimmed();
    if (ask.isEmpty() || !m_input) {
        return false;
    }

    if (m_dormant) {
        // There is no resume/send path while the core is offline. Refuse
        // before touching either the dialog's ask or the dormant draft.
        if (!m_core || !m_core->isConnected()) {
            return false;
        }
        // Do NOT replace the dormant composer's draft with the ask. The
        // resumed lifecycle handler sends m_pendingQuickAsk once the process
        // is genuinely live, then puts this exact composer text back. Keeping
        // the draft in place also means a failed resume cannot lose it.
        if (m_pendingQuickAsk.isEmpty()) {
            m_pendingQuickAsk = ask;
        } else {
            // Two quick-asks before the resume event are still two distinct
            // human messages. Preserve both in their arrival order.
            m_pendingQuickAsk += QStringLiteral("\n\n") + ask;
        }
        resume();
        return true;
    }

    // A live agent can use the normal send path immediately. Swap only long
    // enough for it to capture/queue the ask, then restore the exact draft —
    // whitespace is user text too, so never use trimmed() as the restoration
    // predicate.
    const QString draft = m_input->toPlainText();
    m_input->setPlainText(ask);
    onSendClicked();
    const QString after = m_input->toPlainText();
    if (after.trimmed().isEmpty()) {
        m_input->setPlainText(draft);
        return true;
    }
    if (after != ask) {
        // The established send path rewrote the composer itself. It owns that
        // result; do not clobber it with an older snapshot.
        return true;
    }
    // Refused before a side effect. Put the original draft back exactly as it
    // was and let the quick-ask dialog retain its text for a retry.
    m_input->setPlainText(draft);
    return false;
}

void AgentPanel::restorePendingQuickAskToComposer()
{
    if (m_pendingQuickAsk.isEmpty() || !m_input) {
        return;
    }
    const QString ask = std::exchange(m_pendingQuickAsk, QString());
    const QString draft = m_input->toPlainText();
    m_input->setPlainText(draft.isEmpty()
                              ? ask
                              : ask + QStringLiteral("\n\n") + draft);
    addNote(i18n("Quick ask could not resume — it and your draft are in the "
                 "composer."), QStringLiteral("err"));
}

void AgentPanel::focusComposer()
{
    if (m_input) {
        m_input->setFocus();
    }
}

void AgentPanel::updateSlashPopup()
{
    if (!m_slashPopup || !m_input) {
        return;
    }
    const QString text = m_input->toPlainText();
    // Active while the composer holds exactly one line that starts with "/"
    // and the command word is still being typed (no space yet).
    const bool active = text.startsWith(QLatin1Char('/'))
        && !text.contains(QLatin1Char('\n')) && !text.contains(QLatin1Char(' '))
        && text.length() <= 64 && !m_slashCommands.isEmpty();
    if (!active) {
        hideSlashPopup();
        return;
    }
    const QString prefix = text.mid(1);
    m_slashPopup->clear();
    for (const auto &cmd : std::as_const(m_slashCommands)) {
        if (!cmd.name.startsWith(prefix, Qt::CaseInsensitive)) {
            continue;
        }
        const QString label = cmd.description.isEmpty()
            ? QStringLiteral("/") + cmd.name
            : QStringLiteral("/%1 — %2").arg(cmd.name, cmd.description);
        auto *item = new QListWidgetItem(label, m_slashPopup);
        item->setData(Qt::UserRole, cmd.name);
        // The argument hint is painted separately, in the disabled colour —
        // it is what the user must still type, not part of the command.
        item->setData(kSlashHintRole, cmd.hint);
    }
    if (m_slashPopup->count() == 0) {
        hideSlashPopup();
        return;
    }
    m_slashPopup->setCurrentRow(0);
    // Overlay just above the composer, matching its width.
    const int rowH = qMax(1, m_slashPopup->sizeHintForRow(0));
    const int visible = qMin(m_slashPopup->count(), 8);
    const int h = visible * rowH + 2 * m_slashPopup->frameWidth();
    const QPoint inputTopLeft = m_input->mapTo(this, QPoint(0, 0));
    m_slashPopup->setGeometry(inputTopLeft.x(), inputTopLeft.y() - h,
                              m_input->width(), h);
    m_slashPopup->raise();
    m_slashPopup->setVisible(true);
}

void AgentPanel::acceptSlashCompletion()
{
    if (!m_slashPopup || !m_slashPopup->isVisible()) {
        return;
    }
    QListWidgetItem *item = m_slashPopup->currentItem();
    hideSlashPopup();
    if (!item) {
        return;
    }
    const QString name = item->data(Qt::UserRole).toString();
    m_input->setPlainText(QStringLiteral("/") + name + QStringLiteral(" "));
    QTextCursor cur = m_input->textCursor();
    cur.movePosition(QTextCursor::End);
    m_input->setTextCursor(cur);
    m_input->setFocus();
}

void AgentPanel::hideSlashPopup()
{
    if (m_slashPopup) {
        m_slashPopup->setVisible(false);
    }
}

HarnessTraits AgentPanel::currentTraits() const
{
    // A bound thread's backend is authoritative; before one exists the engine
    // picker decides (it may hold a sticky non-default engine).
    return HarnessRegistry::self()->traits(
        m_threadId.isEmpty() ? selectedHarnessId() : m_backend);
}

QString AgentPanel::selectedHarnessId() const
{
    if (!m_threadId.isEmpty()) {
        return m_backend;
    }
    return m_engineCombo
        ? m_engineCombo->currentData().toString().section(QLatin1Char('|'), 0, 0)
        : QString();
}

QString AgentPanel::selectedProviderId() const
{
    if (!m_threadId.isEmpty()) {
        return m_startedProviderId;
    }
    const QString provider = m_engineCombo
        ? m_engineCombo->currentData().toString().section(QLatin1Char('|'), 1)
        : QString();
    return provider.isEmpty() ? ProviderStore::directId() : provider;
}

void AgentPanel::rebuildEngineCombo()
{
    if (!m_engineCombo) {
        return;
    }
    QSignalBlocker block(m_engineCombo);
    const QString before = m_engineCombo->currentData().toString();
    m_engineCombo->clear();
    for (const HarnessTraits &t : HarnessRegistry::self()->all()) {
        // An engine whose CLI is not installed is still LISTED — it is how the
        // engine is discoverable at all — but it says so and cannot be picked,
        // instead of being a choice that could only end in "executable file not
        // found" after the user had written a task (audit F37). The label is
        // EngineAvailability's, not this file's: the roster's quick menu and
        // the New Agent dialog render the same string, so the three cannot
        // drift into three spellings of "dead".
        const bool present = EngineAvailability::isPresent(t.id);
        const QString engineLabel =
            EngineAvailability::pickerLabel(t.id, t.displayName);
        m_engineCombo->addItem(engineLabel, t.id);
        setComboEntryEnabled(
            m_engineCombo, m_engineCombo->count() - 1, present,
            i18n("Agent Kate drives an agent command-line program, and this "
                 "engine's is not installed on this machine."));
        if (!t.providerRouting) {
            continue;
        }
        // Provider overlays: the same harness routed at a third-party
        // Anthropic-compatible API ("Claude Code via Fireworks"). The two
        // presets ship with no key, so on a fresh profile these were offered as
        // routes that could only ever abort at send (audit F46). ProviderStore
        // owns the wording — a route with no resolvable key is labelled, not
        // hidden, so the user can see the option exists and go configure it —
        // and, like a missing CLI, it is not selectable while it cannot start.
        const QList<ProviderProfile> profiles = ProviderStore::load();
        for (const ProviderProfile &p : profiles) {
            if (!p.routed()) {
                continue; // the direct entry IS the base harness row
            }
            m_engineCombo->addItem(
                i18nc("engine entry: harness via provider", "%1 via %2",
                      engineLabel, ProviderStore::pickerLabel(p)),
                t.id + QLatin1Char('|') + p.id);
            setComboEntryEnabled(
                m_engineCombo, m_engineCombo->count() - 1,
                present && ProviderStore::keyResolvable(p),
                present ? i18n("No API key is stored for %1 — add one under "
                               "Options ▸ Configure API Providers….", p.name)
                        : i18n("Agent Kate drives an agent command-line "
                               "program, and this engine's is not installed on "
                               "this machine."));
        }
    }
    // Sticky, with one-time migration from the legacy backend+provider keys.
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    QString saved = !before.isEmpty() ? before : cfg.readEntry("engine", QString());
    if (saved.isEmpty()) {
        const QString backend = cfg.readEntry("backend", QString());
        const QString provider = cfg.readEntry("provider", ProviderStore::directId());
        saved = backend;
        if (!backend.isEmpty() && provider != ProviderStore::directId()) {
            saved += QLatin1Char('|') + provider;
        }
    }
    int idx = m_engineCombo->findData(saved);
    if (idx < 0) {
        // e.g. the sticky provider was deleted — fall back to its harness.
        idx = m_engineCombo->findData(saved.section(QLatin1Char('|'), 0, 0));
    }
    if (idx >= 0) {
        m_engineCombo->setCurrentIndex(idx);
    }
    // The sticky choice can be an engine that has since been uninstalled, or a
    // provider whose key has gone. Landing on it would start an agent that
    // cannot run, so fall through to the first entry that can — the same
    // fallback applyModelEffortSupport does for a refused effort tier.
    selectFirstEnabled(m_engineCombo);
}

void AgentPanel::rebuildModeCombo()
{
    if (!m_modeCombo) {
        return;
    }
    const HarnessTraits t = currentTraits();
    QSignalBlocker block(m_modeCombo);
    m_modeCombo->clear();
    if (t.permissionModes.isEmpty()) {
        m_modeCombo->addItem(i18n("CLI default"), QString());
        return;
    } else {
        for (const QString &mode : t.permissionModes) {
            m_modeCombo->addItem(HarnessRegistry::modeLabel(mode), mode);
        }
    }
    // Sticky per harness — but never re-arm an auto-approve-everything mode
    // ("Expert — never ask" / yolo) on the next conversation.
    QString saved = KSharedConfig::openConfig()
                        ->group(QStringLiteral("Agent"))
                        .readEntry(t.stickyModeKey(), QString());
    if (saved == QLatin1String("bypassPermissions")
        || saved == QLatin1String("dontAsk")) {
        saved = QStringLiteral("auto");
    } else if (saved == QLatin1String("yolo")) {
        saved.clear();
    }
    // Nothing sticky yet (or a value this harness no longer offers): land on
    // the harness's named default. The list is the engine's own vocabulary in
    // the CLI's order, so falling through to index 0 would hand a fresh profile
    // whichever mode that engine happens to list first.
    const int idx = m_modeCombo->findData(saved);
    const int fallback = m_modeCombo->findData(t.defaultPermissionMode());
    if (idx >= 0) {
        m_modeCombo->setCurrentIndex(idx);
    } else if (fallback >= 0) {
        m_modeCombo->setCurrentIndex(fallback);
    }
}

void AgentPanel::rebuildEffortCombo()
{
    if (!m_effortCombo) {
        return;
    }
    const HarnessTraits t = currentTraits();
    QSignalBlocker block(m_effortCombo);
    m_effortCombo->clear();
    // An empty value leaves the CLI's own configured default untouched.
    m_effortCombo->addItem(i18n("Default"), QString());
    if (t.efforts.isEmpty()) {
        // Some catalogues advertise efforts per model rather than as a
        // harness-wide setting. Expose their union, then narrow it below.
        const auto choices = HarnessRegistry::self()->modelChoices(
            t.id, selectedProviderId());
        for (const QString &entry : choices.all) {
            const QString model = entry.section(QLatin1Char('|'), 0, 0);
            for (const QString &effort :
                 HarnessRegistry::self()->modelEfforts(t.id, selectedProviderId(), model)) {
                if (!effort.isEmpty() && m_effortCombo->findData(effort) < 0) {
                    m_effortCombo->addItem(HarnessRegistry::effortLabel(effort), effort);
                }
            }
        }
    } else {
        for (const QString &effort : t.efforts) {
            m_effortCombo->addItem(HarnessRegistry::effortLabel(effort), effort);
        }
    }
    const QString saved = KSharedConfig::openConfig()
                              ->group(QStringLiteral("Agent"))
                              .readEntry(t.stickyEffortKey(), QString());
    const int idx = m_effortCombo->findData(saved);
    if (idx >= 0) {
        m_effortCombo->setCurrentIndex(idx);
    }
    applyModelEffortSupport();
}

// applyModelEffortSupport greys out the tiers the SELECTED MODEL cannot run.
// The engine reports per-model effort support alongside its live model
// catalogue (claude's list_models); an empty claim means it said nothing, in
// which case every tier stays selectable — never the reverse.
void AgentPanel::applyModelEffortSupport()
{
    if (!m_effortCombo || !m_modelCombo) {
        return;
    }
    const QStringList supported = HarnessRegistry::self()->modelEfforts(
        currentTraits().id, selectedProviderId(), currentModel());
    auto *model = qobject_cast<QStandardItemModel *>(m_effortCombo->model());
    if (!model) {
        return;
    }
    for (int i = 0; i < m_effortCombo->count(); ++i) {
        const QString value = m_effortCombo->itemData(i).toString();
        // "Default" (an empty value) always stays available: it asks the engine
        // to keep whatever it is already configured with.
        const bool ok = supported.isEmpty() || value.isEmpty()
            || supported.contains(value, Qt::CaseInsensitive);
        QStandardItem *item = model->item(i);
        if (!item) {
            continue;
        }
        item->setEnabled(ok);
        item->setToolTip(ok ? QString()
                            : i18n("%1 does not support this thinking effort.",
                                   currentModel()));
    }
    // If the sticky choice landed on a tier this model cannot run, fall back to
    // the engine default rather than starting on a value that would be refused.
    const int cur = m_effortCombo->currentIndex();
    if (cur >= 0 && model->item(cur) && !model->item(cur)->isEnabled()) {
        m_effortCombo->setCurrentIndex(0);
    }
}

void AgentPanel::maybePushOption(const QString &option, const QString &value)
{
    // Pre-start and dormant changes apply at the (re)start instead; only a
    // live thread takes a mid-session change.
    if (m_threadId.isEmpty() || m_dormant || !m_core || !m_core->isConnected()) {
        return;
    }
    // "Default" (empty) can't be applied to a running session — the CLIs take
    // only concrete values mid-session. It will apply at the next start.
    if (value.isEmpty()) {
        addNote(i18n("The default applies from the next start — the running agent "
                     "keeps its current setting."),
                QStringLiteral("dim"));
        return;
    }
    const QString tid = m_threadId;
    QPointer<AgentPanel> self(this);
    QJsonObject requested;
    if (option == QLatin1String("model")) {
        requested.insert(QStringLiteral("model"), value);
    } else if (option == QLatin1String("effort")) {
        requested.insert(QStringLiteral("reasoningEffort"), value);
    } else {
        requested.insert(QStringLiteral("permissionMode"), value);
    }
    m_core->call(QStringLiteral("agent.updateSettings"),
                 QJsonObject{
                     {QStringLiteral("agentRef"), QJsonObject{{QStringLiteral("threadId"), tid},
                                                               {QStringLiteral("harnessId"), m_backend}}},
                     {QStringLiteral("requested"), requested},
                 },
                 [self, tid, option, value](const QJsonObject &result,
                                            const QJsonObject &error) {
                     if (!self || tid != self->m_threadId) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         // The picker now shows a value the agent refused —
                         // say so rather than silently diverging.
                         self->addNote(
                             i18n("Could not change the %1: %2",
                                  option == QLatin1String("model")
                                      ? i18n("model")
                                      : option == QLatin1String("effort")
                                            ? i18n("thinking effort")
                                            : i18n("approval mode"),
                                  error.value(QStringLiteral("message"))
                                      .toString()
                                      .toHtmlEscaped()),
                             QStringLiteral("err"));
                         // A rejected model is usually a retired/unknown id —
                         // offer a live replacement and apply it if chosen.
                         if (option == QLatin1String("model")) {
                             const QString repl = agentkate::askReplacementModel(
                                 self, self->m_backend, self->providerId(), value);
                             if (!repl.isEmpty() && repl != value) {
                                 self->preselectModel(repl);
                                 self->maybePushOption(QStringLiteral("model"), repl);
                             }
                         }
                         return;
                     }
                     const QJsonObject effective = result.value(QStringLiteral("effective")).toObject();
                     const QString applied = option == QLatin1String("model")
                         ? effective.value(QStringLiteral("model")).toString()
                         : option == QLatin1String("effort")
                             ? effective.value(QStringLiteral("reasoningEffort")).toString()
                             : effective.value(QStringLiteral("permissionMode")).toString();
                     const QString timing = result.value(QStringLiteral("timing")).toString();
                     self->addNote(
                         i18n("%1 changed to <b>%2</b> — applies %3",
                              option == QLatin1String("model")
                                  ? i18n("Model")
                                  : option == QLatin1String("effort")
                                        ? i18n("Thinking effort")
                                        : i18n("Approval mode"),
                              (applied.isEmpty() ? value : applied).toHtmlEscaped(),
                              timing == QLatin1String("live") ? i18n("now")
                                  : timing == QLatin1String("nextTurn") ? i18n("on the next turn")
                                                                            : i18n("at launch")),
                         QStringLiteral("sys"));
                 },
                 this);
}

void AgentPanel::rebuildModelCombo()
{
    if (!m_modelCombo) {
        return; // the engine combo may fire before the model combo is built
    }
    const HarnessTraits t = currentTraits();

    QSignalBlocker block(m_modelCombo);
    m_modelCombo->clear();

    // Every engine discovers its models live now (Claude via `claude -p /model`
    // or a routed provider's /v1/models; Kimi via its handshake). The combo stays
    // editable so a full model id can be typed even before a catalogue is cached;
    // an empty value = the CLI's / provider's own default.
    m_modelCombo->setEditable(true);
    // Never let Qt's default InsertAtBottom append the hand-typed id as a
    // dataless dropdown item: currentModel() would then read its empty itemData
    // and silently send model:"" instead of the typed id.
    m_modelCombo->setInsertPolicy(QComboBox::NoInsert);
    m_modelCombo->lineEdit()->setPlaceholderText(i18n("Default — or type a model id"));

    const auto choices =
        HarnessRegistry::self()->modelChoices(t.id, selectedProviderId());
    const auto addEntries = [this](const QStringList &entries) {
        for (const QString &entry : entries) {
            const QString value = entry.section(QLatin1Char('|'), 0, 0);
            const QString name = entry.section(QLatin1Char('|'), 1);
            if (value.isEmpty() || m_modelCombo->findData(value) >= 0) {
                continue;
            }
            m_modelCombo->addItem(
                (name.isEmpty() || name == value)
                    ? value
                    : QStringLiteral("%1 — %2").arg(name, value),
                value);
        }
    };
    addEntries(choices.recommended);
    if (!choices.recommended.isEmpty() && !choices.all.isEmpty()) {
        m_modelCombo->insertSeparator(m_modelCombo->count());
    }
    addEntries(choices.all);
    m_modelCombo->setCurrentIndex(-1);
    m_modelCombo->lineEdit()->clear();
}

void AgentPanel::reloadProviders()
{
    if (!m_engineCombo || !m_threadId.isEmpty()) {
        return; // frozen once a thread exists
    }
    // The provider profiles are part of the engine list; rebuilding it keeps
    // the current selection (falling back to the bare harness if the selected
    // provider was deleted).
    rebuildEngineCombo();
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
    // The empty state quotes the send key, so it has to be re-composed when the
    // setting flips (audit F44).
    updateFeedEmptyState();
}

bool AgentPanel::eventFilter(QObject *obj, QEvent *event)
{
    // Keep the floating "jump to latest" button anchored to the bottom-right
    // of the feed viewport as it resizes.
    if (m_view && obj == m_view->viewport()
        && event->type() == QEvent::Resize) {
        positionJumpButton();
        updateFeedEmptyState(); // the hint is sized to the viewport
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
        // While the slash popup is up it owns the navigation keys; everything
        // else falls through so typing keeps filtering the list.
        if (m_slashPopup && m_slashPopup->isVisible()) {
            switch (key->key()) {
            case Qt::Key_Up:
            case Qt::Key_Down: {
                const int delta = key->key() == Qt::Key_Down ? 1 : -1;
                const int count = m_slashPopup->count();
                if (count > 0) {
                    int row = m_slashPopup->currentRow() + delta;
                    row = qBound(0, row, count - 1);
                    m_slashPopup->setCurrentRow(row);
                }
                return true;
            }
            case Qt::Key_Return:
            case Qt::Key_Enter:
            case Qt::Key_Tab:
                acceptSlashCompletion();
                return true;
            case Qt::Key_Escape:
                hideSlashPopup();
                return true;
            default:
                break;
            }
        }
        // Composer history (audit F50): Up on the FIRST line walks back through
        // the messages sent this session, Down walks forward and returns the
        // draft that was there. Anywhere else Up/Down are ordinary cursor keys,
        // so a multi-line message is still editable. Session-only and never
        // persisted — a prompt can hold anything the human typed.
        if ((key->key() == Qt::Key_Up || key->key() == Qt::Key_Down)
            && !key->modifiers().testFlag(Qt::ControlModifier)
            && !key->modifiers().testFlag(Qt::ShiftModifier)) {
            const QTextCursor cur = m_input->textCursor();
            const bool onFirstLine = cur.blockNumber() == 0;
            const bool onLastLine = cur.blockNumber() == m_input->blockCount() - 1;
            if (key->key() == Qt::Key_Up && onFirstLine && !m_composerHistory.isEmpty()
                && m_historyIndex != 0) {
                if (m_historyIndex < 0) {
                    m_historyDraft = m_input->toPlainText();
                    m_historyIndex = m_composerHistory.size();
                }
                --m_historyIndex;
                setComposerFromHistory(m_composerHistory.at(m_historyIndex));
                return true;
            }
            if (key->key() == Qt::Key_Down && onLastLine && m_historyIndex >= 0) {
                if (m_historyIndex + 1 < m_composerHistory.size()) {
                    ++m_historyIndex;
                    setComposerFromHistory(m_composerHistory.at(m_historyIndex));
                } else {
                    // Past the newest entry: hand back the draft the walk
                    // interrupted rather than leaving the last message in place.
                    const QString draft = m_historyDraft;
                    m_historyIndex = -1;
                    m_historyDraft.clear();
                    setComposerFromHistory(draft);
                }
                return true;
            }
        }
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
    // Whatever this panel was showing belongs to the outgoing thread, including
    // any usage-limit claim: a dormant agent has no process and is waiting on
    // nothing (audit F43). Withdrawn before the id is overwritten, or the old
    // thread's claim would be stranded in the shared state forever.
    clearRateLimitClaim();
    m_threadId = threadId;
    Q_EMIT threadIdChanged(m_threadId);
    // Same rebind rule as bindStartedThread: the outgoing thread's provisional
    // rows are not this one's to claim.
    resetStreamState();
    m_dormant = true;
    m_isolated = isolated;
    m_backend = backend;
    loadTranscript();
    // Desktop access is persisted per thread, so a restored agent must show its
    // real state rather than the panel's blank default.
    syncCoworkFromCore();
    // Pull the thread's persisted compaction strategy and reflect it in the
    // dropdown — overrides whatever sticky default the panel was showing.
    // Skipped for harnesses without compaction support: nothing to pull.
    if (currentTraits().compaction) {
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
    bindStartedThread(threadId, isolated, backend);
    // Replay the inherited conversation from the source agent (the fork's own
    // session id is minted asynchronously, so its transcript file isn't ready yet).
    loadTranscriptFrom(sourceThreadId);
    addNote(QStringLiteral("forked from %1 — the conversation continues here.")
                .arg(title.toHtmlEscaped()),
            QStringLiteral("sys"));
    emit dormantChanged(false);
    refresh();
}

void AgentPanel::adoptStartedThread(const QString &threadId, const QString &note,
                                    bool isolated, const QString &backend)
{
    bindStartedThread(threadId, isolated, backend);
    // No transcript to replay: this thread was born with its opening message,
    // which streams in live like any other first turn.
    if (!note.isEmpty()) {
        addNote(note.toHtmlEscaped(), QStringLiteral("sys"));
    }
    emit dormantChanged(false);
    refresh();
}

// bindStartedThread is the shared state flip behind both adoptions: take over a
// thread the core has already started, live, with its own fresh cost meter.
void AgentPanel::bindStartedThread(const QString &threadId, bool isolated,
                                   const QString &backend)
{
    m_threadId = threadId;
    Q_EMIT threadIdChanged(m_threadId);
    // Whatever the previous thread was mid-stream belongs to that thread: its
    // provisional rows must not be claimed by the new one's first assistant
    // event, whose block order is unrelated.
    resetStreamState();
    m_dormant = false;
    m_idle = true; // live, waiting for its first turn to stream in
    m_isolated = isolated;
    m_backend = backend;
    m_sessionCostUsd = 0.0;
    m_sessionInTokens = 0;
    m_sessionOutTokens = 0;
    // A fork inherits its source's desktop access (the record is copied), and an
    // ensemble worker may have been launched with it — read the truth rather than
    // leaving the checkbox saying otherwise.
    syncCoworkFromCore();
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
                     // A replay feeds authoritative `assistant` events through
                     // the same branch that claims provisional stream rows, so
                     // any left over from a live stream would be claimed — and
                     // overwritten — by replayed text belonging to another turn.
                     resetStreamState();
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
    // No compaction support on this harness — the core would reject the call.
    if (!currentTraits().compaction) {
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
    const HarnessTraits t = currentTraits();
    // No compaction support on this harness — the core would reject the call.
    if (!t.compaction) {
        addNote(i18n("Compaction is not supported for %1 agents.", t.displayName),
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
    // Every non-hot backend re-reads the stored session; a hot-only harness has
    // nothing to read, and the core refuses the call. Say so here rather than
    // relaying "compact only inside a live session" from the wire.
    if (model != QLatin1String("hot") && !t.coldCompact) {
        addNote(i18n("%1 agents summarize only inside a live session — resume "
                     "the agent and use the live option.", t.displayName),
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
                     // Wording (in-place vs. stored summary) is decided once,
                     // in compactionOutcome().
                     addNote(compactionOutcome(res), QStringLiteral("ok"));
                 },
                 this);
}

void AgentPanel::doResume()
{
    addNote(i18n("resuming the %1 session…", currentTraits().displayName),
            QStringLiteral("sys"));
    QJsonObject params{{QStringLiteral("threadId"), m_threadId}};
    // If this chat's saved model is no longer in the provider's live catalogue,
    // ask for a replacement before resuming rather than failing on a retired id.
    // Send the choice so the core resumes on (and persists) the new model.
    const QString savedModel = currentModel();
    if (!agentkate::modelAvailable(m_backend, providerId(), savedModel)) {
        const QString repl =
            agentkate::askReplacementModel(this, m_backend, providerId(), savedModel);
        if (repl.isEmpty()) {
            addNote(i18n("Resume cancelled — this chat's model is no longer available."),
                    QStringLiteral("dim"));
            return;
        }
        preselectModel(repl);
        params.insert(QStringLiteral("model"), repl);
        addNote(i18n("Model updated to <b>%1</b> for this chat.", repl.toHtmlEscaped()),
                QStringLiteral("sys"));
    }
    // Resume carries only the opaque provider id. The core resolves the
    // profile and credential into its private runtime binding.
    if (!m_startedProviderId.isEmpty()) {
        params.insert(QStringLiteral("providerId"), m_startedProviderId);
    }
    m_core->call(QStringLiteral("agent.resume"), params,
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (!error.isEmpty()) {
                         addNote(QStringLiteral("Could not resume: %1")
                                     .arg(error.value(QStringLiteral("message"))
                                              .toString()
                                              .toHtmlEscaped()),
                                 QStringLiteral("err"));
                         restorePendingQuickAskToComposer();
                     }
                 },
                 this);
}

void AgentPanel::resume()
{
    if (!m_dormant || m_threadId.isEmpty()) {
        return;
    }
    // The recovery flow ends in a COLD compaction (the thread is dormant), so
    // it gates on coldCompact, not compaction: a hot-only harness would reach
    // the model prompt with no summary on disk and every choice refused by the
    // core. Those resume straight away — there is nothing to offer.
    if (!currentTraits().coldCompact) {
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
                                          // Same honest wording as the manual
                                          // "Compact now" path: an in-place
                                          // compaction has no turns or bytes to
                                          // report and must not print zeros.
                                          addNote(compactionOutcome(res),
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
    // Every optional affordance binds to the harness's capability set — the
    // bound thread's traits, or the engine picker's selection before a thread
    // exists (currentTraits() resolves that).
    const HarnessTraits traits = currentTraits();
    m_sendBtn->setEnabled(!m_threadId.isEmpty() || !traits.id.isEmpty());
    if (m_subagentsBtn) {
        // Hidden rather than disabled for an engine that writes no subagent
        // files: a greyed button would suggest the conversations exist and are
        // merely out of reach.
        m_subagentsBtn->setVisible(traits.subagentTranscripts);
        m_subagentsBtn->setEnabled(!m_threadId.isEmpty());
    }
    if (m_forkBtn) {
        m_forkBtn->setEnabled(!m_threadId.isEmpty() && traits.fork);
        m_forkBtn->setToolTip(
            traits.fork
                ? QStringLiteral(
                    "Continue this conversation as a new agent on a different model or "
                    "thinking effort, keeping the full context. The original is untouched.")
                : i18n("Forking is not supported for %1 agents.", traits.displayName));
    }
    // Compact-now needs a thread on disk (running or dormant) and a harness
    // with compaction support. The Hot Opus menu item is the only one that
    // further needs the thread to be live.
    if (m_compactNowBtn) {
        m_compactNowBtn->setEnabled(!m_threadId.isEmpty() && traits.compaction);
        if (auto *menu = m_compactNowBtn->menu()) {
            const auto actions = menu->actions();
            if (!actions.isEmpty()) {
                actions.first()->setEnabled(running); // "Hot Opus (live thread)"
            }
        }
    }
    if (m_compactNowMenu) {
        m_compactNowMenu->setEnabled(!m_threadId.isEmpty() && traits.compaction);
        const auto actions = m_compactNowMenu->actions();
        if (!actions.isEmpty()) {
            actions.first()->setEnabled(running); // "Hot Opus (live thread)"
        }
    }
    // Engine and isolation are baked into the agent's launch — frozen once a
    // thread exists. Desktop access is NOT: its MCP bridge is always wired in and
    // simply reveals or hides its tools, so it is switchable mid-session on any
    // harness that supports Cowork. Model and "when to ask" stay adjustable
    // WHILE THE AGENT RUNS (the core forwards mid-session changes through the
    // harness); thinking effort is live only where the harness says so.
    m_compactCombo->setEnabled(traits.compaction);
    m_compactStrip->setEnabled(traits.compaction);
    m_engineCombo->setEnabled(m_threadId.isEmpty());
    m_modeCombo->setEnabled(m_threadId.isEmpty() || running);
    m_isolationCombo->setEnabled(m_threadId.isEmpty());
    m_effortCombo->setEnabled(traits.effortLive ? (m_threadId.isEmpty() || running)
                                                : m_threadId.isEmpty());
    m_modelCombo->setEnabled(m_threadId.isEmpty() || running);
    m_coworkCheck->setEnabled(traits.cowork);

    // Offer promotion while a thread runs non-isolated in the workspace — but
    // only on a harness that supports it (the core rejects agent.promote
    // otherwise), like fork/compaction/cowork above.
    m_promoteBar->setVisible(!m_threadId.isEmpty() && !m_isolated && !m_promoting
                             && traits.promote);

    // Parked on the account's usage window: nothing is computing, whatever the
    // last turn state said, so neither the working indicator nor the roster's
    // green arc may claim otherwise (audit F43 / plan 28 §Phase 2).
    const bool parked = rateLimitParked();
    m_rateParkedShown = parked;

    // "Agent Kate at work" indicator: animate while a turn is actually computing.
    m_working->setActive(running && !m_idle && !parked && m_permQueue.isEmpty());

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
    } else if (parked) {
        // Ahead of dormant/idle/working on purpose: "waiting for quota" is the
        // true account of an agent whose engine stopped because the window was
        // exhausted, and "Dormant — Resume to continue" would invite the user
        // to do by hand the thing that is already scheduled.
        dot = QStringLiteral("#e0a030");
        const QString clock =
            m_rateWakeAt.isValid()
                ? QLocale().toString(m_rateWakeAt.toLocalTime().time(), QLocale::ShortFormat)
                : QString();
        text = clock.isEmpty()
            ? i18nc("@info roster subtitle, agent parked on the account's usage limit",
                    "Paused by a usage limit")
            : i18nc("@info roster subtitle, parked agent with an automatic resume armed",
                    "Paused by a usage limit — resumes at %1", clock);
        st = AgentRoles::AgentStatus::RateLimited;
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
    // Badge non-default engines so the roster card shows which harness drives
    // this agent (the default engine stays unmarked — the common case). The
    // badge text is harness data, never a hardcoded name.
    if (!m_threadId.isEmpty() && !traits.badge.isEmpty()) {
        text.prepend(traits.badge + QStringLiteral(" · "));
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
    // Context fill — the number that predicts auto-compaction. Only shown
    // once real figures exist (kimi reports no usage, so it never appears
    // there rather than showing a made-up zero).
    if (m_ctxPromptTokens > 0 && m_ctxWindow > 0) {
        text += i18nc("context-fill suffix, percent of context window used",
                      " · ctx %1%", int((m_ctxPromptTokens * 100) / m_ctxWindow));
    }
    // Usage-limit chip. Header-only, deliberately not folded into `text`: the
    // roster subtitle is per-agent, while a rate limit is an account-wide fact
    // that would then read as N separate problems across the roster.
    QString limitChip;
    if (!m_rateLimitStatus.isEmpty()) {
        // rateLimitType is a token like "five_hour" — the window the limit
        // applies to, spelled for a human.
        QString window = m_rateLimitType;
        window.replace(QLatin1Char('_'), QLatin1Char(' '));
        QStringList parts;
        if (!window.isEmpty()) {
            parts << i18nc("rate-limit chip: the window a usage limit covers",
                           "%1 window", window);
        }
        if (!m_rateLimitResets.isEmpty()) {
            parts << i18nc("rate-limit chip: when the usage window resets",
                           "resets %1", m_rateLimitResets);
        }
        if (m_rateLimitOverage) {
            parts << i18nc("rate-limit chip: billing past the included quota",
                           "on overage");
        }
        if (parts.isEmpty()) {
            parts << m_rateLimitStatus;
        }
        // Amber for anything but a plain "allowed": approaching or past a limit
        // is the only state worth pulling the eye.
        // A literal hex, not palette(mid): this is QTextDocument rich text, not
        // a Qt style sheet, so the palette() function does not resolve here —
        // the same reason the status dot above is a literal.
        const bool ok = m_rateLimitStatus == QLatin1String("allowed");
        limitChip = QStringLiteral("&nbsp;&nbsp;<span style='color:%1'>&middot; %2</span>")
                        .arg(ok ? (isDark(this) ? QStringLiteral("#8b91a0")
                                                : QStringLiteral("#5d6471"))
                                : QStringLiteral("#e0a030"),
                             parts.join(QStringLiteral(" &middot; ")).toHtmlEscaped());
    }
    m_header->setText(QStringLiteral("<span style='color:%1'>&#9679;</span>&nbsp;&nbsp;%2%3")
                          .arg(dot, text.toHtmlEscaped(), limitChip));
    m_header->setToolTip(contextTooltip());
    emit statusChanged(int(st));
    emit subtitleChanged(text);
    // Roster card affordance, derived from the same state computed above.
    emit attentionChanged(running && !m_permQueue.isEmpty());
}

// contextTooltip explains the header's "ctx N%" suffix: where the context
// window actually went. The category split comes from the engine's own
// accounting (a `_context` event); without one the tooltip says the figure is
// an estimate, so the number is never read as more authoritative than it is.
QString AgentPanel::contextTooltip() const
{
    if (m_ctxPromptTokens <= 0 || m_ctxWindow <= 0) {
        return QString();
    }
    const QLocale loc;
    QStringList lines;
    lines << (m_ctxExact
                  ? i18n("Context: %1 of %2 tokens used",
                         loc.toString(m_ctxPromptTokens), loc.toString(m_ctxWindow))
                  : i18n("Context: about %1 of %2 tokens used (estimated from the "
                         "last turn's usage)",
                         loc.toString(m_ctxPromptTokens), loc.toString(m_ctxWindow)));
    if (!m_ctxBreakdown.isEmpty()) {
        lines << QString();
        for (const QJsonValue &v : m_ctxBreakdown) {
            const QJsonObject cat = v.toObject();
            const qlonglong tokens =
                cat.value(QStringLiteral("tokens")).toVariant().toLongLong();
            const QString label = cat.value(QStringLiteral("label")).toString();
            if (label.isEmpty() || tokens <= 0) {
                continue;
            }
            lines << i18nc("context breakdown row: category, tokens, share",
                           "%1: %2 (%3%)", label, loc.toString(tokens),
                           int((tokens * 100) / m_ctxWindow));
        }
    }
    return lines.join(QLatin1Char('\n'));
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
    // Notes carry the same live timestamp message cards do (audit F50): "rate
    // limit resets at 15:04" or "agent failed: …" is far less useful when the
    // feed cannot say when it happened. A replayed note gets none — the same
    // rule as addMessageCard, so the feed never stamps history as "now".
    const QString ts = m_replaying
        ? QString()
        : QLocale().toString(QTime::currentTime(), QLocale::ShortFormat);
    m_model->appendNote(html, kind, ts);
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
        // A tool row is paint-only too, and it is where the command that failed
        // and the error it printed actually live — the strings a user pastes
        // into a bug report (audit F48). The inspector could always show them;
        // it could not put them on the clipboard, and getting there was a modal
        // for a copy. Full result, not the display-clipped one.
        const QString full =
            idx.data(TranscriptModel::ToolFullResultRole).toString();
        const QString shown = idx.data(TranscriptModel::ToolResultRole).toString();
        QStringList parts{idx.data(TranscriptModel::ToolNameRole).toString(),
                          idx.data(TranscriptModel::ToolDetailRole).toString(),
                          full.isEmpty() ? shown : full};
        parts.removeAll(QString());
        if (!parts.isEmpty()) {
            const QString text = parts.join(QStringLiteral("\n"));
            QAction *copy = toolMenu.addAction(
                QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy text"));
            connect(copy, &QAction::triggered, this, [text] {
                QGuiApplication::clipboard()->setText(text);
            });
        }
        toolMenu.exec(globalPos);
        return;
    }
    // Notes and reasoning are paint-only: no selection overlay reaches them, so
    // without this the CLI error text and rate-limit reset time a user wants to
    // paste into a bug report can only be retyped (audit F48).
    if (kind == TranscriptModel::Note || kind == TranscriptModel::Thinking) {
        const QString text = idx.data(TranscriptModel::PlainRole).toString();
        if (text.isEmpty()) {
            return;
        }
        QMenu noteMenu(this);
        QAction *copy = noteMenu.addAction(
            QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy text"));
        connect(copy, &QAction::triggered, this, [text] {
            QGuiApplication::clipboard()->setText(text);
        });
        noteMenu.exec(globalPos);
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

// --- empty state ------------------------------------------------------------

// updateFeedEmptyState paints the "nothing here yet" hint over an empty feed
// (audit F44): first launch lands chat-forward on a completely blank box, with
// nothing saying what to type, what will happen to the user's files, or that a
// command palette exists at all.
//
// Two rules the copy obeys:
//   * It names whichever isolation the picker is ACTUALLY on. Promising a
//     private copy to an agent set to "Directly in my files" would be the same
//     class of falsehood F30 removed from the word "sandbox".
//   * It claims no containment. A worktree separates the agent's CHANGES from
//     yours; it does not confine the process, so the sentence is about merging,
//     not about safety (the wording follows NewAgentDialog's).
void AgentPanel::updateFeedEmptyState()
{
    if (!m_feedEmptyHint || !m_view) {
        return;
    }
    const bool empty = m_model->count() == 0;
    m_feedEmptyHint->setVisible(empty);
    if (!empty) {
        return;
    }
    const bool enterSends = KSharedConfig::openConfig()
                                ->group(QStringLiteral("Agent"))
                                .readEntry("enterSends", true);
    const QString send = enterSends ? i18nc("the composer's send key", "Enter")
                                    : i18nc("the composer's send key", "Ctrl+Enter");
    // Before a thread exists the picker decides; afterwards the thread's own
    // isolation is the truth (and the picker is frozen).
    const QString isolation = m_threadId.isEmpty() && m_isolationCombo
        ? m_isolationCombo->currentData().toString()
        : (m_isolated ? QStringLiteral("isolated") : QStringLiteral("workspace"));
    m_feedEmptyHint->setText(agentkate::feedEmptyStateHtml(isolation, send));
    // Centre it on the viewport, inset so long lines wrap instead of touching
    // the edges.
    const QRect vp = m_view->viewport()->rect();
    m_feedEmptyHint->setGeometry(vp.adjusted(24, 0, -24, 0));
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

void AgentPanel::flushDraft()
{
    // Settle the debounce before teardown. Without this, "type a sentence, hit
    // Close" loses it whenever the close lands inside the 400 ms window — the
    // exact case the draft feature exists for.
    if (m_draftTimer && m_draftTimer->isActive()) {
        m_draftTimer->stop();
        saveDraft();
    }
}

void AgentPanel::dropPendingDraftWrite()
{
    // The thread is being destroyed and the dock has already cleared the stored
    // draft; a pending debounce firing after that would write it straight back.
    if (m_draftTimer) {
        m_draftTimer->stop();
    }
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

// setComposerFromHistory replaces the composer with a history entry (or the
// interrupted draft) and parks the cursor at the end, without the textChanged
// handler mistaking our own write for the human editing and cancelling the walk.
void AgentPanel::setComposerFromHistory(const QString &text)
{
    m_historyNavigating = true;
    m_input->setPlainText(text);
    m_input->moveCursor(QTextCursor::End);
    m_historyNavigating = false;
}

// rememberSent pushes a message the human actually sent onto the session-only
// history ring the composer's Up arrow walks. Consecutive duplicates collapse
// (re-sending the same prompt twice should not need two Ups) and the ring is
// bounded.
void AgentPanel::rememberSent(const QString &text)
{
    if (text.isEmpty()) {
        return;
    }
    if (!m_composerHistory.isEmpty() && m_composerHistory.last() == text) {
        return;
    }
    m_composerHistory.append(text);
    if (m_composerHistory.size() > kComposerHistoryMax) {
        m_composerHistory.removeFirst();
    }
    m_historyIndex = -1;
    m_historyDraft.clear();
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
    // Recompute the matching rows. Every row kind is scanned through the model's
    // own search text — notes, tool names/commands/results and reasoning included
    // (audit F48): the error text the user is staring at lives in a note, and
    // scanning message prose alone answered "No matches" for it. The scan goes
    // through the model's CACHED lowercased text (audit F58): searchText()
    // re-joined a Tool row's name + summary + detail + full retained result —
    // up to 128 KB — into a fresh QString per row per keystroke. The delegate
    // paints the per-row highlight from the model's find state (messages and
    // notes; a matched tool/thinking row is scrolled to and counted but not
    // highlighted — its body is behind a collapsed header), so no HTML
    // rewriting happens here.
    m_findHits.clear();
    const QString needleLower = needle.toLower();
    for (int row = 0; row < m_model->count(); ++row) {
        if (m_model->searchTextLower(row).contains(needleLower)) {
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
    // A tool or thinking row matches on its BODY — the command, the output, the
    // reasoning — and that body lives behind a collapsed header. Scrolling to a
    // row whose match is not on screen is barely better than "No matches"
    // (audit F48), so the row being landed on is opened. Only the current hit:
    // expanding every match would rewrite the whole feed's layout.
    const TranscriptModel::Kind kind = m_model->itemAt(curRow).kind;
    if (kind == TranscriptModel::Tool || kind == TranscriptModel::Thinking) {
        m_model->setExpanded(curRow, true);
    }
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
        //
        // The advice used to say "Restart Agent Kate to recover" WHILE the
        // reconnect ladder that usually succeeds seconds later was still
        // running; round 2 replaced it with an unconditional "reconnecting",
        // which is the same falsehood mirrored — the ladder has a terminal
        // state, and after it the banner says it gave up while this said to
        // wait (audit F50, round 3). Ask the client which of the three states
        // it is actually in and say that one.
        const agentkate::LinkState link =
            m_core->reconnectGaveUp()  ? agentkate::LinkState::GaveUp
            : m_core->isReconnecting() ? agentkate::LinkState::Reconnecting
                                       : agentkate::LinkState::NeverConnected;
        emit statusMessage(agentkate::disconnectedSendStatus(link));
        addNote(agentkate::disconnectedSendNote(link).toHtmlEscaped(),
                QStringLiteral("err"));
        return;
    }

    // Resolve the API provider for a fresh start up front, while the composer
    // still holds the message — a missing key aborts cleanly without losing it.
    // (akcore inherits this UI's environment, so if the key can't be resolved
    // here it cannot be resolved at launch either.) Only engines with provider
    // routing carry a provider overlay in the first place.
    QString startedProviderId;
    const HarnessTraits startTraits = currentTraits();
    if (m_threadId.isEmpty() && startTraits.providerRouting) {
        const ProviderProfile prof = ProviderStore::byId(selectedProviderId());
        if (prof.routed()) {
            if (!ProviderStore::keyResolvable(prof)) {
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
        // Measured against the params the drain will actually build. Queuing an
        // oversize message unchecked would refuse it only at the turn boundary,
        // by which point the composer is empty and the message sits at the head
        // of the queue blocking every follow-up behind it.
        const QJsonObject queuedParams{{QStringLiteral("threadId"), m_threadId},
                                       {QStringLiteral("text"), text},
                                       {QStringLiteral("attachments"), m_attachments}};
        if (wouldOverflowFrame(queuedParams)) {
            return; // composer untouched — the human can drop an attachment and retry
        }
        m_input->clear();
        clearDraft();
        rememberSent(text);
        m_sendQueue.append(QueuedMsg{text, m_attachments});
        m_attachments = QJsonArray();
        rebuildAttachChips();
        rebuildQueueChips();
        addNote(QStringLiteral("&#128338; queued — sends when the current turn finishes"),
                QStringLiteral("dim"));
        refresh();
        return;
    }

    // The composer is not cleared until the message is actually committed to a
    // request: everything below can still refuse it (an unavailable model, an
    // oversized frame), and a refused send must leave the human's text and
    // attachment chips exactly where they were.
    const QJsonArray attachments = m_attachments;
    const auto commitComposer = [this, text] {
        m_input->clear();
        clearDraft();
        rememberSent(text);
        m_attachments = QJsonArray();
        rebuildAttachChips();
    };

    if (m_threadId.isEmpty()) {
        // If a stale model id is selected (e.g. a resumed pick, or a hand-typed
        // id no longer offered), ask for a live replacement before starting
        // rather than failing on the CLI.
        QString startModel = currentModel();
        if (!agentkate::modelAvailable(selectedHarnessId(), startedProviderId, startModel)) {
            const QString repl = agentkate::askReplacementModel(
                this, selectedHarnessId(), startedProviderId, startModel);
            if (repl.isEmpty()) {
                addNote(i18n("Start cancelled — the chosen model is no longer available."),
                        QStringLiteral("dim"));
                return;
            }
            startModel = repl;
            preselectModel(repl);
        }

        QJsonObject startParams{
            {QStringLiteral("workspacePath"), m_workspace},
            {QStringLiteral("prompt"), text},
            {QStringLiteral("backend"), selectedHarnessId()},
            // The mode/effort combos hold per-harness vocabularies (Claude's
            // permission modes and --effort levels, or a discovered harness's
            // own config-option values), so both send verbatim.
            {QStringLiteral("permissionMode"), m_modeCombo->currentData().toString()},
            {QStringLiteral("isolation"), m_isolationCombo->currentData().toString()},
            {QStringLiteral("effort"), m_effortCombo->currentData().toString()},
            {QStringLiteral("model"), startModel},
            // Cowork applies only where the harness supports it (the checkbox
            // is disabled otherwise, but the record must never lie).
            {QStringLiteral("coworkEnabled"),
             startTraits.cowork && m_coworkCheck->isChecked()},
            {QStringLiteral("attachments"), attachments}};
        if (!startedProviderId.isEmpty()) {
            startParams.insert(QStringLiteral("providerId"), startedProviderId);
        }
        // The P6 launch options, sent only when the human asked for them (the
        // New Agent dialog offers each field only where the engine can apply
        // it, so an empty list here means "not requested", never "dropped").
        const auto insertList = [&startParams](const char *key, const QStringList &list) {
            if (list.isEmpty()) {
                return;
            }
            QJsonArray arr;
            for (const QString &v : list) {
                arr.append(v);
            }
            startParams.insert(QLatin1String(key), arr);
        };
        insertList("fallbackModels", m_fallbackModels);
        insertList("disallowedTools", m_disallowedTools);
        insertList("addDirs", m_addDirs);
        // Same rule for the scalar half of the sweep: absent means "not
        // requested". A budget of 0 is no budget, not a budget of nothing.
        if (m_strictMcpConfig) {
            startParams.insert(QStringLiteral("strictMcpConfig"), true);
        }
        if (m_maxBudgetUsd > 0.0) {
            startParams.insert(QStringLiteral("maxBudgetUsd"), m_maxBudgetUsd);
        }
        if (wouldOverflowFrame(startParams)) {
            return; // composer untouched — the human can drop an attachment and retry
        }

        commitComposer();
        // A fresh session — start the meters from zero.
        m_sessionCostUsd = 0.0;
        m_sessionInTokens = 0;
        m_sessionOutTokens = 0;
        m_ctxPromptTokens = 0;
        m_ctxWindow = 0;
        m_ctxExact = false;
        m_ctxBreakdown = QJsonArray();
        m_turnDurTotalMs = 0;
        m_turnDurCount = 0;
        m_working->setAverageTurnMs(0);
        addYouCard(text, attachments);
        m_idle = false;
        m_working->setActivity(QString()); // a new turn starts in generic mode
        m_working->setTurnStart(QDateTime::currentMSecsSinceEpoch());

        QString title = text.simplified();
        if (title.isEmpty()) {
            title = QStringLiteral("(attachments)");
        }
        if (title.length() > 26) {
            title = title.left(25) + QChar(0x2026);
        }
        emit titleChanged(title);

        m_startedProviderId = startedProviderId;
        // Held until the agent process actually starts. If the start fails
        // instead, this is what goes back to the composer rather than leaving
        // the human to copy their own first message out of the feed (audit F37).
        m_pendingOpening = QueuedMsg{text, attachments};
        m_core->call(QStringLiteral("agent.start"), startParams,
                     [this](const QJsonObject &result, const QJsonObject &error) {
                         // A "success" without a threadId fails the same way an
                         // error does (audit F67): with an empty id the panel
                         // drops every notification, no _lifecycle/started can
                         // ever arrive, and m_pendingOpening would stay latched
                         // forever — so the prompt goes back to the composer
                         // here too, not only on `error`.
                         const QString failure =
                             agentkate::startFailureReason(result, error);
                         if (!failure.isEmpty()) {
                             addNote(QStringLiteral("Failed to start agent: %1")
                                         .arg(failure.toHtmlEscaped()),
                                     QStringLiteral("err"));
                             restoreUnsentToComposer();
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
        if (!deliverMessage(text, attachments)) {
            return; // refused — the composer keeps the message
        }
        commitComposer();
    }
}

// wouldOverflowFrame refuses a request whose serialized params would not fit
// the core's JSON-RPC frame. The per-attachment and total-attachment budgets
// upstream bound the files; this bounds the WHOLE request, because the message
// text rides in the same frame — 12 MB of images plus a pasted log is over the
// cliff with every individual budget respected.
bool AgentPanel::wouldOverflowFrame(const QJsonObject &params)
{
    if (QJsonDocument(params).toJson(QJsonDocument::Compact).size() <= kMaxSendFrameBytes) {
        return false;
    }
    showAttachNotice(i18n("This message is too large to send — remove an attachment or "
                          "shorten the text."));
    return true;
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
        // Our durable copy of an image's bytes. Carried on the card precisely
        // BECAUSE the body is stripped here: the chip thumbnail is redrawn from
        // a path, and the origin path may be a temp file that is already gone.
        if (a.contains(QStringLiteral("cachePath"))) {
            c[QStringLiteral("cachePath")] = a.value(QStringLiteral("cachePath"));
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
    const QString name = att.value(QStringLiteral("name")).toString();
    const bool image =
        att.value(QStringLiteral("kind")).toString() == QLatin1String("image");

    // resolveAttachmentPath prefers the origin — for a workspace file that is
    // the copy worth opening, since edits there count — but falls back to our
    // cached copy both when the origin is gone AND when it is still there
    // holding different bytes, which is the normal fate of a fixed-name capture
    // file. Opening the screenshot the user never sent is worse than erroring.
    const QString path = agentkate::resolveAttachmentPath(att);
    if (path.isEmpty()) {
        emit statusMessage(
            i18n("Can't open “%1” — the file has moved or been deleted since it was "
                 "attached.",
                 name.isEmpty() ? att.value(QStringLiteral("path")).toString() : name));
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

void AgentPanel::handleTaskEvent(const QString &subtype, const QJsonObject &ev)
{
    // Stale task chatter from a replayed transcript would show dead jobs as
    // running — the tray is live-only.
    if (m_replaying) {
        return;
    }
    if (subtype == QLatin1String("task_started")) {
        const QString id = ev.value(QStringLiteral("task_id")).toString();
        if (id.isEmpty()) {
            return;
        }
        if (auto it = m_bgJobs.find(id); it != m_bgJobs.end()) {
            // A re-announce of a job we already latched terminal means the CLI
            // reused the id for fresh work; leaving the row done/failed would
            // strand it there forever. Restart it in place so it keeps its
            // insertion order, and clear the end stamp or the revived row would
            // inherit the previous run's duration. A re-announce of a RUNNING
            // job is genuine chatter and stays a no-op.
            if (it->done || it->failed) {
                it->done = false;
                it->failed = false;
                it->noted = false;
                it->endedMs = 0;
                it->outputFile.clear();
                it->startedMs = QDateTime::currentMSecsSinceEpoch();
                const QString desc = ev.value(QStringLiteral("description")).toString();
                if (!desc.isEmpty()) {
                    it->description = desc;
                }
                // Refreshed like the description: the reused id can be fresh
                // work of a different kind, and taskType is what decides whether
                // the row is a terminal job or an agent (and so which icon and
                // which open action it gets).
                const QString reType = ev.value(QStringLiteral("task_type")).toString();
                if (!reType.isEmpty()) {
                    it->taskType = reType;
                }
                const QString reToolUseId = ev.value(QStringLiteral("tool_use_id")).toString();
                if (!reToolUseId.isEmpty()) {
                    m_taskByToolUse.insert(reToolUseId, id);
                }
                updateJobsBar();
            }
            return;
        }
        BgJob job;
        job.id = id;
        job.description = ev.value(QStringLiteral("description")).toString();
        job.taskType = ev.value(QStringLiteral("task_type")).toString();
        job.startedMs = QDateTime::currentMSecsSinceEpoch();
        const QString toolUseId = ev.value(QStringLiteral("tool_use_id")).toString();
        if (!toolUseId.isEmpty()) {
            m_taskByToolUse.insert(toolUseId, id);
        }
        job.order = ++m_bgJobSeq; // insertion order; QHash iteration is unordered
        m_bgJobs.insert(id, job);
        updateJobsBar();
        return;
    }
    if (subtype == QLatin1String("task_notification")
        || subtype == QLatin1String("task_updated")) {
        const QString id = ev.value(QStringLiteral("task_id")).toString();
        auto it = m_bgJobs.find(id);
        if (it == m_bgJobs.end()) {
            return;
        }
        const QString outputFile = ev.value(QStringLiteral("output_file")).toString();
        if (!outputFile.isEmpty()) {
            it->outputFile = outputFile;
        }
        const QString status =
            subtype == QLatin1String("task_notification")
                ? ev.value(QStringLiteral("status")).toString()
                : ev.value(QStringLiteral("patch"))
                      .toObject()
                      .value(QStringLiteral("status"))
                      .toString();
        if (!status.isEmpty() && status != QLatin1String("running")) {
            // Applied whatever `done` already says: background_tasks_changed
            // flips done on any task that left the running set, and it cannot
            // know WHY it left. Gating this on !done let that event win the race
            // and render a failed job as a tick, permanently. Only the addNote
            // below is one-shot.
            it->done = true;
            // First terminal report wins the finish stamp: repeated
            // notifications for the same task must not stretch its duration.
            if (it->endedMs == 0) {
                it->endedMs = QDateTime::currentMSecsSinceEpoch();
            }
            // "completed" is the only success terminal state the CLI reports;
            // anything else (failed, cancelled) is a failure the Jobs panel has
            // to show as such rather than as a tick.
            it->failed = status != QLatin1String("completed");
            // The CLI's human-readable completion line, e.g. «Background
            // command "…" completed (exit code 0)».
            const QString summary = ev.value(QStringLiteral("summary")).toString();
            if (!summary.isEmpty() && !it->noted) {
                it->noted = true; // exactly one summary per job, however many arrive
                addNote(summary.toHtmlEscaped(),
                        status == QLatin1String("completed") ? QStringLiteral("ok")
                                                             : QStringLiteral("err"));
            }
        }
        updateJobsBar();
        return;
    }
    if (subtype == QLatin1String("background_tasks_changed")) {
        // The authoritative set of still-running tasks; anything of ours not
        // in it has finished (catches an update we might have missed).
        QSet<QString> live;
        const QJsonArray tasks = ev.value(QStringLiteral("tasks")).toArray();
        for (const QJsonValue &v : tasks) {
            live.insert(v.toObject().value(QStringLiteral("task_id")).toString());
        }
        const qint64 nowMs = QDateTime::currentMSecsSinceEpoch();
        for (auto it = m_bgJobs.begin(); it != m_bgJobs.end(); ++it) {
            if (!live.contains(it.key())) {
                it->done = true;
                if (it->endedMs == 0) {
                    it->endedMs = nowMs;
                }
            }
        }
        updateJobsBar();
    }
}

// forgetFinishedJobs drops this agent's completed job records and republishes.
// The Jobs panel mirrors these snapshots rather than owning them, so its "Clear
// finished" has to act here — clearing the view alone would be undone by the
// next snapshot. Running work is deliberately untouched.
void AgentPanel::forgetFinishedJobs()
{
    bool removed = false;
    QSet<QString> gone;
    for (auto it = m_bgJobs.begin(); it != m_bgJobs.end();) {
        if (it->done) {
            gone.insert(it.key());
            it = m_bgJobs.erase(it);
            removed = true;
        } else {
            ++it;
        }
    }
    dropTaskMappings(gone);
    // The workflow row has no record in m_bgJobs — it is synthesized from the
    // monitor on every publish — so there is nothing to erase and a flag is the
    // only way "Clear finished" can reach it. Cleared by the next launch.
    if (!m_workflowForgotten && m_workflowMonitor && m_workflowMonitor->isValid()) {
        const WorkflowMonitor::State state = m_workflowMonitor->snapshot().state;
        if (state == WorkflowMonitor::State::Completed
            || state == WorkflowMonitor::State::Failed) {
            m_workflowForgotten = true;
            removed = true;
        }
    }
    if (removed) {
        updateJobsBar();
    }
}

// dropTaskMappings forgets tool_use → task_id entries for tasks that no longer
// exist. Only the arriving launch result ever reads one, so an entry that
// outlives its job is dead weight the map would otherwise carry for the session.
void AgentPanel::dropTaskMappings(const QSet<QString> &taskIds)
{
    if (taskIds.isEmpty()) {
        return;
    }
    for (auto it = m_taskByToolUse.begin(); it != m_taskByToolUse.end();) {
        if (taskIds.contains(it.value())) {
            it = m_taskByToolUse.erase(it);
        } else {
            ++it;
        }
    }
}

void AgentPanel::openBackgroundJob(const QString &taskId)
{
    const auto it = m_bgJobs.constFind(taskId);
    if (it == m_bgJobs.constEnd()) {
        return;
    }
    if (it->outputFile.isEmpty()) {
        emit statusMessage(i18n("No output yet — try again in a moment"));
        return;
    }
    // A subagent's output file is a stream-json transcript — show it as a live
    // chat; a shell's output file is plain text — open it in the editor.
    if (it->taskType == QLatin1String("local_bash")) {
        emit openFileRequested(it->outputFile);
        return;
    }
    auto *dlg = new SubAgentTranscriptDialog(
        it->outputFile,
        it->description.isEmpty() ? i18n("Sub-agent") : it->description, this);
    dlg->show();
}

// updateJobsBar rebuilds the in-chat tray and publishes this agent's jobs.
//
// The tray shows RUNNING jobs only, plus one trailing "N finished" chip that
// opens the Jobs panel. It used to keep every chip it had ever made — nothing
// removed them — so a long session buried the composer under dozens of dead ✓
// chips and leaked a QPushButton per task. Finished work still exists, it just
// lives in the panel that can hold it, which is also the only place that can
// show jobs from OTHER agents.
void AgentPanel::updateJobsBar()
{
    if (!m_jobsBar) {
        return;
    }
    // Drop the oldest finished records once retention is exceeded, so a very
    // long session can't grow this map without bound either.
    if (m_bgJobs.size() > kMaxRetainedJobs) {
        QList<QString> finished;
        for (auto it = m_bgJobs.cbegin(); it != m_bgJobs.cend(); ++it) {
            if (it->done) {
                finished.append(it.key());
            }
        }
        std::sort(finished.begin(), finished.end(),
                  [this](const QString &a, const QString &b) {
                      return m_bgJobs.value(a).order < m_bgJobs.value(b).order;
                  });
        QSet<QString> evicted;
        for (int i = 0; i < finished.size() && m_bgJobs.size() > kMaxRetainedJobs; ++i) {
            m_bgJobs.remove(finished.at(i));
            evicted.insert(finished.at(i));
        }
        dropTaskMappings(evicted);
    }

    QVector<agentkate::AgentJob> jobs;
    jobs.reserve(m_bgJobs.size());
    // Copies, not pointers into m_bgJobs: the chips below are built AFTER
    // publishJobs(), and a publish that ever touched the map (a re-entrant
    // consumer, a future async hop) would leave those pointers dangling. Only
    // the handful of fields a chip draws is copied.
    struct ChipJob {
        QString id;
        QString description;
        QString outputFile;
        quint64 order = 0;
        qint64 startedMs = 0;
        bool agent = false; // sub-agent rather than a local shell
    };
    QList<ChipJob> running;
    int finishedCount = 0;
    bool anyRunning = false;
    const qint64 now = QDateTime::currentMSecsSinceEpoch();

    for (auto it = m_bgJobs.cbegin(); it != m_bgJobs.cend(); ++it) {
        agentkate::AgentJob j;
        j.id = it->id;
        j.kind = it->taskType == QLatin1String("local_bash")
            ? agentkate::AgentJob::Kind::Shell
            : agentkate::AgentJob::Kind::Subagent;
        j.description = it->description;
        j.outputFile = it->outputFile;
        j.startedMs = it->startedMs;
        j.endedMs = it->endedMs;
        j.done = it->done;
        j.failed = it->failed;
        jobs.append(j);

        if (it->done) {
            ++finishedCount;
        } else {
            running.append(ChipJob{it->id, it->description, it->outputFile, it->order,
                                   it->startedMs,
                                   it->taskType != QLatin1String("local_bash")});
            anyRunning = true;
        }
    }
    // The workflow run is a job too — it just has its own chip already, so it
    // is published to the panel without being duplicated into the tray.
    if (m_workflowMonitor && m_workflowMonitor->isValid() && !m_workflowForgotten) {
        const WorkflowMonitor::Snapshot snap = m_workflowMonitor->snapshot();
        agentkate::AgentJob j;
        // The run id is parsed out of the launch result and can be missing when
        // that blob is shaped unexpectedly; without SOME id the row is inert
        // (nothing to key an action on), so fall back through the other anchors.
        j.id = !snap.runId.isEmpty()
            ? snap.runId
            : (!snap.taskId.isEmpty() ? snap.taskId : snap.transcriptDir);
        j.kind = agentkate::AgentJob::Kind::Workflow;
        j.description = snap.summary.isEmpty()
            ? i18n("Workflow (%1 sub-agents)", snap.agentCount)
            : snap.summary;
        j.outputFile = snap.transcriptDir;
        // The run's artifacts carry no start time — the launch we watched is
        // the only place it exists.
        j.startedMs = m_workflowStartedMs;
        j.done = snap.state == WorkflowMonitor::State::Completed
            || snap.state == WorkflowMonitor::State::Failed;
        j.failed = snap.state == WorkflowMonitor::State::Failed;
        // Latched on the first terminal snapshot: the run's artifacts carry no
        // finish time, so re-reading them later must not restamp the end and
        // stretch the duration the Jobs panel shows.
        if (j.done && m_workflowEndedMs == 0) {
            m_workflowEndedMs = now;
        }
        j.endedMs = j.done ? m_workflowEndedMs : 0;
        jobs.append(j);
    }

    std::sort(running.begin(), running.end(),
              [](const ChipJob &a, const ChipJob &b) { return a.order < b.order; });

    // chipLabel is shared by the build and the in-place refresh so the two can
    // never disagree about what a chip says.
    const auto chipLabel = [&now](const ChipJob &job) {
        QString label = job.description.simplified();
        if (label.length() > 40) {
            label = label.left(39) + QChar(0x2026);
        }
        QString text =
            (job.agent ? QStringLiteral("\U0001f916 ") : QStringLiteral("⚙ ")) + label;
        // Elapsed suffix for long-running jobs — honest "still going" signal.
        if (job.startedMs > 0) {
            const qint64 mins = (now - job.startedMs) / 60000;
            if (mins >= 1) {
                text += i18nc("background job elapsed minutes", " · %1m", mins);
            }
        }
        return text;
    };

    // Publishing first, and on its own terms. It used to hang off the tray's
    // fingerprint, which answers a narrower question — "do the CHIPS need
    // rebuilding" — and so swallowed every change no chip draws: a finished
    // job's late output path, a failure, a forced republish for a consumer that
    // has seen nothing. The snapshot compares itself instead.
    publishJobs(jobs);

    // Which chips the tray should hold. Recreating widgets on the 15 s tick
    // would flicker the row under the composer for no reason — the elapsed
    // suffix is the only thing that moves between real changes, and it is
    // minute-granular. So rebuild only when the SET changes, and otherwise
    // relabel in place. The workflow is absent by design: it has no chip here
    // (it draws its own bar) and its progress no longer needs to force a
    // rebuild just to reach the Jobs panel.
    QString fingerprint;
    for (const ChipJob &job : std::as_const(running)) {
        fingerprint += job.id + QLatin1Char('|') + job.outputFile + QLatin1Char(';');
    }
    fingerprint += QLatin1Char('#') + QString::number(finishedCount);

    if (fingerprint == m_jobsFingerprint) {
        for (const ChipJob &job : std::as_const(running)) {
            if (QPushButton *chip = m_jobChips.value(job.id)) {
                chip->setText(chipLabel(job));
            }
        }
    } else {
        m_jobsFingerprint = fingerprint;

        while (QLayoutItem *item = m_jobsFlow->takeAt(0)) {
            if (QWidget *w = item->widget()) {
                w->deleteLater();
            }
            delete item;
        }
        m_jobChips.clear();

        for (const ChipJob &job : std::as_const(running)) {
            auto *chip = new QPushButton(chipLabel(job), m_jobsBar);
            chip->setCursor(Qt::PointingHandCursor);
            chip->setToolTip(i18n("Running in the background — click to watch its output")
                             + (job.outputFile.isEmpty()
                                    ? QString()
                                    : QStringLiteral("\n") + job.outputFile));
            const QString id = job.id;
            connect(chip, &QPushButton::clicked, this, [this, id] { openBackgroundJob(id); });
            m_jobsFlow->addWidget(chip);
            m_jobChips.insert(id, chip);
        }
        if (finishedCount > 0) {
            auto *chip = new QPushButton(
                i18ncp("finished background jobs", "✓ %1 finished", "✓ %1 finished",
                       finishedCount),
                m_jobsBar);
            chip->setCursor(Qt::PointingHandCursor);
            chip->setToolTip(
                i18n("Open the Jobs panel to see finished work from every agent"));
            connect(chip, &QPushButton::clicked, this, &AgentPanel::openJobsPanelRequested);
            m_jobsFlow->addWidget(chip);
        }

        m_jobsBar->setVisible(!running.isEmpty() || finishedCount > 0);
    }

    // Tick the elapsed suffixes while anything runs; quiesce when nothing does.
    if (anyRunning) {
        if (!m_jobsTimer) {
            m_jobsTimer = new QTimer(this);
            m_jobsTimer->setInterval(15000);
            connect(m_jobsTimer, &QTimer::timeout, this, &AgentPanel::updateJobsBar);
        }
        if (!m_jobsTimer->isActive()) {
            m_jobsTimer->start();
        }
    } else if (m_jobsTimer) {
        m_jobsTimer->stop();
    }
}

void AgentPanel::publishJobs(QVector<agentkate::AgentJob> jobs)
{
    // Deterministic order first. The records come out of a QHash, so a rehash
    // can reshuffle them and an element-wise compare would then call an
    // unchanged set changed. Nothing downstream depends on this order — the
    // Jobs panel sorts its own rows — but the comparison below does.
    std::sort(jobs.begin(), jobs.end(),
              [](const agentkate::AgentJob &a, const agentkate::AgentJob &b) {
                  return a.startedMs != b.startedMs ? a.startedMs < b.startedMs
                                                    : a.id < b.id;
              });
    // Job rows are addressed by thread. An id-less panel gives the Jobs panel
    // nothing to key on, so it publishes nothing; whatever it is holding is
    // published once a thread is bound and the next task event lands.
    if (m_threadId.isEmpty()) {
        jobs.clear();
    }
    // The id moved under us — a failed start dropped it, "Stop & close" cleared
    // it, a dormant panel rebound. Reap the old id's rows before claiming the
    // new one, or the same work shows twice and the old group never leaves.
    if (!m_publishedThreadId.isEmpty() && m_publishedThreadId != m_threadId) {
        Q_EMIT jobsChanged(m_publishedThreadId, {});
        m_publishedThreadId.clear();
        m_lastPublishedJobs.clear();
    }
    if (jobs == m_lastPublishedJobs) {
        return;
    }
    m_lastPublishedJobs = jobs;
    // Only a non-empty publish leaves rows behind to reap.
    m_publishedThreadId = jobs.isEmpty() ? QString() : m_threadId;
    Q_EMIT jobsChanged(m_threadId, jobs);
}

// republishJobs forces the next publish through. Publishing is change-gated,
// and a consumer that attaches after the work started has nothing to compare
// against — it would sit empty until the next task event, which for a
// long-running job may be never.
void AgentPanel::republishJobs()
{
    m_lastPublishedJobs.clear();
    updateJobsBar();
}

void AgentPanel::noteWorkflowLaunch(const QString &inputJson, const QString &resultText)
{
    m_workflowInput = inputJson;
    m_workflowResult = resultText;
    // "Now" is only the launch time for a live tool_result. During replay the
    // run may be days old, and stamping now would show a finished workflow as
    // having just started; 0 renders as no Elapsed at all.
    m_workflowStartedMs = m_replaying ? 0 : QDateTime::currentMSecsSinceEpoch();
    // The end stamp latches on the first terminal snapshot, so a new run has to
    // clear the previous one's or the fresh row inherits its duration.
    m_workflowEndedMs = 0;
    // A new run outranks any "Clear finished" the human applied to the old one.
    m_workflowForgotten = false;

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
    // The workflow is published to the Jobs panel as a job row too, so its
    // state changes have to re-publish the set, not just relabel the chip.
    connect(m_workflowMonitor, &WorkflowMonitor::changed, this,
            &AgentPanel::updateJobsBar);
    updateWorkflowChip();
    updateJobsBar();
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
bool AgentPanel::deliverMessage(const QString &text, const QJsonArray &attachments)
{
    const QJsonObject params{{QStringLiteral("threadId"), m_threadId},
                             {QStringLiteral("text"), text},
                             {QStringLiteral("attachments"), attachments}};
    // Checked before any side effect: a refusal must leave no You card, no busy
    // turn and no consumed message behind.
    if (wouldOverflowFrame(params)) {
        return false;
    }
    addYouCard(text, attachments);
    m_idle = false;
    m_errored = false; // a fresh turn clears any prior failure state
    m_working->setActivity(QString()); // a new turn starts in generic mode
    m_working->setTurnStart(QDateTime::currentMSecsSinceEpoch());
    m_core->call(QStringLiteral("agent.send"), params, nullptr, this);
    refresh();
    return true;
}

// drainSendQueue fires the next queued follow-up once the thread is idle. It
// is called on every `result`; sending sets m_idle = false again, so the rest
// of the queue waits for the following turn boundary — one message per turn.
void AgentPanel::drainSendQueue()
{
    if (m_sendQueue.isEmpty() || m_threadId.isEmpty() || m_dormant || !m_idle) {
        return;
    }
    const QueuedMsg q = m_sendQueue.constFirst();
    if (!deliverMessage(q.text, q.attachments)) {
        // The queue branch of onSendClicked already refuses an oversize message
        // at queue time, so reaching here means it became unsendable afterwards.
        // Either way the head must be REMOVED: left in place it can never send
        // and nothing behind it ever drains again. The text is handed back to
        // the composer when that is empty, so it is refused, not eaten.
        m_sendQueue.removeFirst();
        rebuildQueueChips();
        // Only into an empty composer: overwriting a draft the human is typing
        // would trade one lost message for another.
        const bool returned =
            m_input->toPlainText().trimmed().isEmpty() && m_attachments.isEmpty();
        if (returned) {
            m_input->setPlainText(q.text);
            m_attachments = q.attachments;
            rebuildAttachChips();
        }
        addNote(returned
                    ? QStringLiteral("A queued message was too large to send — it is back "
                                     "in the composer, where it can be shortened.")
                    : QStringLiteral("A queued message was too large to send and was "
                                     "dropped so the rest of the queue could drain."),
                QStringLiteral("err"));
        refresh();
        return;
    }
    m_sendQueue.removeFirst();
    rebuildQueueChips();
}

// restoreQueuedToComposer moves any still-queued follow-ups back into the
// composer so a stopped/failed turn never silently eats the human's text. The
// messages join with blank lines; if the composer already holds a draft the
// queued text is prepended so nothing typed is clobbered. Attachments from the
// queued messages are restored to the pending bar (deduped by path).
//
// The merge is budgeted, and the composer's own pending attachments claim the
// budget first — they are the live selection the human can see. Each queued
// message was within the total-attachment limit on its own, but their union
// need not be: two 8 MB sets restored into one composer sail past the core's
// frame cap, and the next send is then unsendable. Over-budget files are dropped
// BY NAME in the banner — the message text, which is what the human actually
// typed, never is.
void AgentPanel::restoreQueuedToComposer()
{
    if (m_sendQueue.isEmpty()) {
        return;
    }
    QStringList parts;
    QJsonArray restoredAttachments;
    QSet<QString> seenPaths;
    qsizetype total = 0;
    QStringList droppedNames;
    // Keeps one attachment if it fits the running budget; names it as dropped
    // otherwise. Dedup runs BEFORE the budget and marks the path seen either
    // way: a duplicate of a dropped file must not be weighed — or reported — a
    // second time. Only a non-empty path can collide.
    const auto keep = [&](const QJsonValue &a) {
        const QJsonObject obj = a.toObject();
        const QString path = obj.value(QStringLiteral("path")).toString();
        if (!path.isEmpty()) {
            if (seenPaths.contains(path)) {
                return;
            }
            seenPaths.insert(path);
        }
        const qsizetype cost = attachmentWireCost(obj);
        if (total + cost > kMaxTotalAttachBytes) {
            QString name = obj.value(QStringLiteral("name")).toString();
            if (name.isEmpty()) {
                name = path.isEmpty() ? i18n("(unnamed attachment)")
                                      : QFileInfo(path).fileName();
            }
            droppedNames << name;
            return;
        }
        total += cost;
        restoredAttachments.append(a);
    };

    // The composer's own pending attachments are folded in FIRST: they are the
    // human's live selection, visible in the chip bar right now, and a restored
    // message must never be able to evict one of them.
    for (const QJsonValue &a : std::as_const(m_attachments)) {
        keep(a);
    }
    for (const QueuedMsg &q : m_sendQueue) {
        if (!q.text.isEmpty()) {
            parts << q.text;
        }
        for (const QJsonValue &a : q.attachments) {
            keep(a);
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
    m_attachments = restoredAttachments;
    rebuildAttachChips();
    if (!droppedNames.isEmpty()) {
        showAttachNotice(
            i18np("%1 attachment was dropped (%2) — the restored messages together "
                  "exceed the %3 MB attachment limit.",
                  "%1 attachments were dropped (%2) — the restored messages together "
                  "exceed the %3 MB attachment limit.",
                  droppedNames.size(), droppedNames.join(QStringLiteral(", ")),
                  kMaxTotalAttachBytes / (1024 * 1024)));
    }
}

// restoreUnsentToComposer hands back EVERY message the human typed that never
// reached an agent: the opening prompt of a fresh start that failed (audit F37
// — it was committed to the feed and then stranded there, so the only way to
// retry was to copy it out of the transcript) plus any queued follow-ups. The
// opening prompt goes to the FRONT so the composer reads in the order it was
// typed. Restoring is a no-op once the agent has started: `_lifecycle/started`
// clears the pending opening, because from then on the message did arrive.
void AgentPanel::restoreUnsentToComposer()
{
    if (!m_pendingOpening.text.isEmpty() || !m_pendingOpening.attachments.isEmpty()) {
        m_sendQueue.prepend(m_pendingOpening);
        m_pendingOpening = QueuedMsg{};
    }
    if (m_sendQueue.isEmpty()) {
        return;
    }
    restoreQueuedToComposer();
    addNote(i18n("your message is back in the composer — nothing was sent"),
            QStringLiteral("dim"));
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
    m_attachSkipped += int(skipped.size());
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
    m_attachSkipped += int(skipped.size());
    // One banner listing every rejection, the way attachPaths does it. The
    // per-reason loop this replaces overwrote itself, so dropping five files
    // reported only the last one (audit F50).
    if (!skipped.isEmpty()) {
        showAttachNotice(skipped.size() == 1
                             ? i18n("Couldn't attach %1", skipped.first())
                             : i18n("Couldn't attach some files:\n• %1",
                                    skipped.join(QStringLiteral("\n• "))));
    }
    rebuildAttachChips();
    if (!wholeFile.isEmpty()) {
        attachPaths(wholeFile);
    }
}

void AgentPanel::attachImages(const QList<QImage> &images)
{
    if (images.isEmpty()) {
        return;
    }
    if (m_attachNotice) {
        m_attachNotice->hide(); // clear any stale rejection from a prior attempt
    }
    const QStringList skipped =
        agentkate::buildImageAttachments(images, m_attachments);
    m_attachSkipped += int(skipped.size());
    if (!skipped.isEmpty()) {
        showAttachNotice(skipped.size() == 1
                             ? i18n("Couldn't attach %1", skipped.first())
                             : i18n("Couldn't attach some images:\n• %1",
                                    skipped.join(QStringLiteral("\n• "))));
    }
    rebuildAttachChips();
}

bool AgentPanel::handleComposerPaste(const QMimeData *source)
{
    if (!source) {
        return false;
    }
    // Image FILES first. A copy from a file manager carries both urls and a
    // rendered image, and the file is the better attachment: it keeps its own
    // name, its origin path, and its original encoding. One image in the
    // selection takes the whole selection with it — a paste of "screenshot.png
    // and notes.txt" is one intent, and buildPathAttachments handles both kinds.
    QStringList paths;
    bool anyImageFile = false;
    const auto urls = source->urls();
    for (const QUrl &u : urls) {
        if (!u.isLocalFile()) {
            continue;
        }
        paths << u.toLocalFile();
        anyImageFile = anyImageFile || isImagePath(u.toLocalFile());
    }
    const int before = m_attachments.size();
    m_attachSkipped = 0;
    if (anyImageFile) {
        attachPaths(paths);
    } else if (source->hasImage() && rawImageBeatsText(source)) {
        const QImage image = qvariant_cast<QImage>(source->imageData());
        if (image.isNull()) {
            return false; // an image format we can't decode — paste as text
        }
        attachImages({image});
    } else {
        return false; // not an image paste; the composer inserts it as before
    }
    const int added = m_attachments.size() - before;
    if (added > 0) {
        emit statusMessage(i18np("Attached %1 item as context",
                                 "Attached %1 items as context", added));
    } else if (m_attachSkipped > 0) {
        // Nothing landed, but the paste WAS consumed (see below), so without
        // this the composer just sits there looking like the paste never
        // happened — the banner alone is easy to miss.
        emit statusMessage(i18n("Nothing attached"));
    }
    // Consumed either way: an image that was refused (too large, over budget)
    // has already said so in the notice, and inserting its text form instead
    // would be worse than nothing.
    return true;
}

bool AgentPanel::canAcceptDrop(const QMimeData *mime) const
{
    if (!mime) {
        return false;
    }
    if (mime->hasFormat(QLatin1String(kAttachMime))) {
        return true;
    }
    // Pixels with no file behind them — an image dragged out of a browser, or
    // straight off a capture tool. Attachable: we write our own PNG for it.
    // Same test paste applies, and for the same reason: a drag out of a
    // spreadsheet or an editor carries a rendered bitmap next to the real text,
    // and taking the drop would attach a picture of the text the user dragged.
    // Refusing it here lets the composer's ordinary text drop have it.
    if (mime->hasImage() && rawImageBeatsText(mime)) {
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
    m_attachSkipped = 0;
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
        if (!paths.isEmpty()) {
            attachPaths(paths);
        } else if (mime->hasImage() && rawImageBeatsText(mime)) {
            // No file anywhere — a browser image drag carries a remote URL and
            // the decoded pixels. Attach the pixels; the builder writes the PNG.
            const QImage image = qvariant_cast<QImage>(mime->imageData());
            if (image.isNull()) {
                // canAcceptDrop said yes on hasImage(), so the drop was already
                // promised; a bare "Nothing attached" for a format Qt has no
                // plugin for reads as a bug in the drop target.
                ++m_attachSkipped;
                showAttachNotice(
                    i18n("Couldn't attach — couldn't decode dropped image"));
            } else {
                attachImages({image});
            }
        }
    }
    event->acceptProposedAction();

    const int added = m_attachments.size() - before;
    if (added > 0) {
        emit statusMessage(i18np("Attached %1 item as context",
                                 "Attached %1 items as context", added));
    } else if (m_attachSkipped > 0) {
        // Only when something was actually refused. An unconditional message
        // here claimed a failure for drops that legitimately added nothing —
        // re-dropping files already attached, say — and drowned the notice that
        // does carry a reason.
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
                     // stop against the now-unknown thread — which is also why
                     // the usage-limit claim has to be withdrawn HERE: after
                     // the clear the destructor's forget() has no id to use.
                     self->clearRateLimitClaim();
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
    // An error-reporting reply callback, not nullptr (audit F50): a failed
    // interrupt used to leave the feed reading "interrupting…" forever while
    // the turn kept running — and kept billing — with nothing to say otherwise.
    // Success needs no note: the core answers in-band with the
    // _lifecycle/turn_aborted event the feed already renders.
    m_core->call(QStringLiteral("agent.interrupt"),
                 QJsonObject{{QStringLiteral("threadId"), m_threadId}},
                 [this](const QJsonObject &, const QJsonObject &error) {
                     if (error.isEmpty()) {
                         return;
                     }
                     addNote(i18n("could not interrupt — the turn is still running: %1",
                                  error.value(QStringLiteral("message"))
                                      .toString()
                                      .toHtmlEscaped()),
                             QStringLiteral("err"));
                 },
                 this);
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
                         // HONEST LABELLING (audit F41): an empty diff is not
                         // evidence that the agent changed nothing. The old
                         // wording ("has not changed anything yet") asserted
                         // exactly that, and it was FALSE for the common case
                         // of an agent that only created new files — the core's
                         // diff is `git diff HEAD`, tracked files only. The
                         // core is gaining untracked-file support; this
                         // sentence stays true either way, because a file
                         // written outside the project or into a git-ignored
                         // path is invisible to any diff we can compute.
                         //
                         // Said in TWO places, and about "this agent" rather
                         // than a thread UUID the human never chose and cannot
                         // match to a row: the status bar clears itself, and the
                         // whole complaint behind F41 is a user who could not
                         // find out where their code went. The feed keeps it.
                         const QString why =
                             i18n("Nothing to show — this agent's diff came back "
                                  "empty. Files written outside the project, or into "
                                  "paths git ignores, never appear here.");
                         emit statusMessage(why);
                         addNote(why.toHtmlEscaped(), QStringLiteral("dim"));
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
    const HarnessTraits t = currentTraits();
    if (!t.promote) {
        // The bar is normally hidden for these harnesses; guard anyway so the
        // core never sees a promote it would only reject, mirroring runCompactNow.
        addNote(i18n("Moving to a private copy is not supported for %1 agents.",
                     t.displayName),
                QStringLiteral("dim"));
        return;
    }
    m_promoting = true;
    // Not "its own sandbox" (audit F30) — a worktree separates the agent's
    // changes from yours, it does not confine the process.
    addNote(QStringLiteral("moving to a private copy — the agent will restart in "
                           "its own git worktree…"),
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
    } else if (method == QLatin1String("cowork.enabledChanged")) {
        // Switched somewhere else (the Cowork panel, or an agent's approved
        // request) — keep this panel's checkbox honest.
        if (!m_threadId.isEmpty()
            && params.value(QStringLiteral("threadId")).toString() == m_threadId) {
            setCoworkChecked(params.value(QStringLiteral("enabled")).toBool());
        }
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

void AgentPanel::setCoworkChecked(bool on)
{
    if (m_coworkCheck->isChecked() == on) {
        return;
    }
    // Mirroring core state must not look like the user flicking the switch.
    m_syncingCowork = true;
    m_coworkCheck->setChecked(on);
    m_syncingCowork = false;
}

void AgentPanel::syncCoworkFromCore()
{
    if (m_threadId.isEmpty()) {
        return;
    }
    QPointer<AgentPanel> self(this);
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("cowork.threadState"), {{QStringLiteral("threadId"), tid}},
                 [this, self, tid](const QJsonObject &res, const QJsonObject &err) {
        // The panel may have been rebound to another thread while this was in flight.
        if (!self || !err.isEmpty() || m_threadId != tid) {
            return;
        }
        setCoworkChecked(res.value(QStringLiteral("enabled")).toBool());
    }, this);
}

void AgentPanel::onCoworkToggled(bool on)
{
    if (m_syncingCowork || m_threadId.isEmpty()) {
        return; // no thread yet: it is a start-time choice, read by agent.start
    }
    QPointer<AgentPanel> self(this);
    const QString tid = m_threadId;
    m_core->call(QStringLiteral("cowork.setEnabled"),
                 {{QStringLiteral("threadId"), tid}, {QStringLiteral("enabled"), on}},
                 [this, self, tid, on](const QJsonObject &res, const QJsonObject &err) {
        if (!self || m_threadId != tid) {
            return;
        }
        if (!err.isEmpty()) {
            // Put the switch back where it was: the agent's access did not change.
            setCoworkChecked(!on);
            addNote(QStringLiteral("desktop access unchanged: %1")
                        .arg(err.value(QStringLiteral("message")).toString().toHtmlEscaped()),
                    QStringLiteral("err"));
            return;
        }
        if (!on) {
            addNote(QStringLiteral("desktop tools switched off for this agent."),
                    QStringLiteral("sys"));
            return;
        }
        // Say what actually reached the running agent — the three cases differ in
        // whether it can act on this right now.
        const QString applied = res.value(QStringLiteral("applied")).toString();
        if (applied == QLatin1String("reattach")) {
            addNote(QStringLiteral("desktop tools enabled — this engine cannot add them to a "
                                   "live session, so the agent is re-attaching to its own "
                                   "session (the conversation is kept)."),
                    QStringLiteral("sys"));
        } else if (applied == QLatin1String("nextStart")) {
            addNote(QStringLiteral("desktop tools enabled — they will be available when this "
                                   "agent next starts."),
                    QStringLiteral("sys"));
        } else {
            addNote(QStringLiteral("desktop tools enabled — available from the next message, "
                                   "no restart."),
                    QStringLiteral("sys"));
        }
        emit statusMessage(QStringLiteral("Desktop access enabled — answer the system "
                                          "permission dialog if it appears."));
    }, this);
}

void AgentPanel::onPermissionRequested(const QJsonObject &params)
{
    if (m_threadId.isEmpty()
        || params.value(QStringLiteral("threadId")).toString() != m_threadId) {
        return;
    }
    // Stamp the arrival so the bar can count down to the broker's deadline
    // (the core denies the request when its timeout fires — previously the
    // bar just sat there and an Approve after expiry silently no-oped).
    QJsonObject stamped = params;
    stamped.insert(QStringLiteral("receivedAtMs"),
                   double(QDateTime::currentMSecsSinceEpoch()));
    m_permQueue.append(stamped);
    const QString tool = params.value(QStringLiteral("toolName")).toString();
    if (tool == QLatin1String("AskUserQuestion")) {
        addNote(QStringLiteral("&#10067; the agent is asking a question"),
                QStringLiteral("sys"));
        // Emit before refresh() publishes generic attention.  AgentNotifier
        // latches it so this earns the distinct question alert, not a second
        // generic permission popup.
        emit questionAsked();
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
        startPermCountdown(req);
        return;
    }
    const QString tool = req.value(QStringLiteral("toolName")).toString();
    // The plan-mode exit is a decision, not a tool grant: the agent finished
    // its read-only planning and asks to start making changes. Show the plan
    // itself instead of a raw tool prompt. (Approving lets the CLI leave plan
    // mode; denying keeps it planning.)
    if (tool == QLatin1String("ExitPlanMode")) {
        QString plan = req.value(QStringLiteral("input"))
                           .toObject()
                           .value(QStringLiteral("plan"))
                           .toString()
                           .trimmed();
        if (plan.length() > 1200) {
            plan = plan.left(1200) + QChar(0x2026);
        }
        m_permBaseHtml =
            i18n("&#128203;&nbsp; The agent finished planning and wants to start "
                 "making changes.")
            + (plan.isEmpty()
                   ? QString()
                   : QStringLiteral("<br><tt>%1</tt>")
                         .arg(plan.toHtmlEscaped().replace(QLatin1Char('\n'),
                                                           QLatin1String("<br>"))));
        m_permLabel->setText(m_permBaseHtml);
        // The plan is already shown in full (clipped only at 1200 chars, and
        // it is the agent's own prose, not an argument list) — no second view.
        m_permDetails->setVisible(false);
        m_permBar->setVisible(true);
        startPermCountdown(req);
        return;
    }
    const QJsonObject input = req.value(QStringLiteral("input")).toObject();
    // SECURITY (audit F28): clipped to the bar's budget, and for Bash from the
    // MIDDLE — the tail of a shell command is where a payload hides and the
    // truncation point is attacker-controllable. Whatever the clip drops is one
    // click away in Details… (the raw input is already here in the request).
    const QString summary = agentkate::permPromptSummary(tool, input, kPermSummaryBudget);
    m_permBaseHtml =
        QStringLiteral("&#128274;&nbsp; Allow the agent to use <b>%1</b>?<br><tt>%2</tt>")
            .arg(tool.toHtmlEscaped(), summary.toHtmlEscaped());
    // Say whose ellipsis that is. Unmarked, the "…" reads as part of the
    // command; the human has to know that text was withheld from the line they
    // are about to approve, and where the rest is (audit F28).
    if (summary != agentkate::permSummary(tool, input)) {
        m_permBaseHtml += QStringLiteral("<br><i>")
            + i18n("shortened to fit — press Details… to read the whole request")
            + QStringLiteral("</i>");
    }
    m_permLabel->setText(m_permBaseHtml);
    m_permDetails->setVisible(!input.isEmpty());
    m_permBar->setVisible(true);
    startPermCountdown(req);
}

// showPermissionDetails opens the pending request's COMPLETE input — the text
// the one-line bar had to clip (audit F28). Everything here is agent-authored
// and possibly hostile, so it is rendered by a read-only QPlainTextEdit: plain
// text by construction, no HTML, no markdown, no image loads, nothing that
// could resolve a local path the way a rich document would (audit F15/F21).
void AgentPanel::showPermissionDetails()
{
    if (m_permQueue.isEmpty()) {
        return;
    }
    const QJsonObject req = m_permQueue.constFirst();
    const QString tool = req.value(QStringLiteral("toolName")).toString();
    const QJsonObject input = req.value(QStringLiteral("input")).toObject();

    QDialog dlg(this);
    dlg.setWindowTitle(i18nc("@title:window", "Permission request — %1", tool));
    auto *layout = new QVBoxLayout(&dlg);
    auto *intro = new QLabel(
        i18n("The complete input the agent asked to run. Read it before you approve."),
        &dlg);
    intro->setTextFormat(Qt::PlainText);
    intro->setWordWrap(true);
    layout->addWidget(intro);

    auto *body = new QPlainTextEdit(&dlg);
    body->setReadOnly(true);
    body->setLineWrapMode(QPlainTextEdit::WidgetWidth);
    QFont mono = body->font();
    mono.setFamily(QStringLiteral("monospace"));
    body->setFont(mono);
    // A Bash command reads as the command, not as a JSON object with the
    // command buried in it; every other tool gets its whole argument set.
    const QString command = input.value(QStringLiteral("command")).toString();
    body->setPlainText(
        tool == QLatin1String("Bash") && !command.isEmpty()
            ? command
            : QString::fromUtf8(QJsonDocument(input).toJson(QJsonDocument::Indented)));
    layout->addWidget(body, 1);

    // Close only. The decision stays on the bar: a dialog that could approve
    // would put the risky action behind a default button (audit F31's class).
    auto *bb = new QDialogButtonBox(QDialogButtonBox::Close, &dlg);
    connect(bb, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    connect(bb, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    layout->addWidget(bb);
    dlg.resize(720, 420);
    dlg.exec();
}

void AgentPanel::startPermCountdown(const QJsonObject &req)
{
    const qint64 receivedAt =
        qint64(req.value(QStringLiteral("receivedAtMs")).toDouble());
    const int timeoutSecs = req.value(QStringLiteral("timeoutSeconds")).toInt(480);
    if (receivedAt <= 0 || timeoutSecs <= 0) {
        m_permDeadlineMs = 0;
        return; // no stamp (e.g. a replayed prompt) — no countdown
    }
    m_permDeadlineMs = receivedAt + qint64(timeoutSecs) * 1000;
    if (!m_permTimer) {
        m_permTimer = new QTimer(this);
        m_permTimer->setInterval(1000);
        connect(m_permTimer, &QTimer::timeout, this, &AgentPanel::tickPermCountdown);
    }
    m_permTimer->start();
}

void AgentPanel::tickPermCountdown()
{
    if ((!m_permBar->isVisible() && !m_questionBox->isVisible())
        || m_permDeadlineMs <= 0) {
        if (m_permTimer) {
            m_permTimer->stop();
        }
        return;
    }
    const qint64 remainMs = m_permDeadlineMs - QDateTime::currentMSecsSinceEpoch();
    if (remainMs <= 0) {
        // The broker's deadline passed: the core has already told the agent
        // no. Drop the dead prompt so an Approve can't silently no-op.
        if (m_permTimer) {
            m_permTimer->stop();
        }
        if (!m_permQueue.isEmpty()) {
            m_permQueue.takeFirst();
        }
        m_permBar->setVisible(false);
        m_questionBox->setVisible(false);
        addNote(i18n("&#9200; the permission request timed out — the agent was "
                     "told no"),
                QStringLiteral("err"));
        showNextPermission();
        refresh();
        return;
    }
    // Show the countdown only once it gets close — a "7:59" ticking from the
    // first second reads as pressure, not information.
    if (m_permBar->isVisible() && remainMs <= 120 * 1000) {
        const int secs = int(remainMs / 1000);
        m_permLabel->setText(
            m_permBaseHtml
            + i18nc("permission expiry countdown (m:ss)",
                    "<br><i>expires in %1:%2</i>", secs / 60,
                    QStringLiteral("%1").arg(secs % 60, 2, 10, QLatin1Char('0'))));
    }
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

// seedSlashCommands accepts both shapes the harnesses use: claude's init event
// lists bare names, kimi's `_commands` lists {name, description, hint} objects.
// One parser, so `commands_changed` can reuse whichever its engine sends.
void AgentPanel::seedSlashCommands(const QJsonArray &commands)
{
    if (commands.isEmpty()) {
        return; // an empty list is "nothing reported", not "no commands exist"
    }
    m_slashCommands.clear();
    for (const QJsonValue &v : commands) {
        if (v.isString()) {
            const QString name = v.toString();
            if (!name.isEmpty()) {
                m_slashCommands.append({name, QString(), QString()});
            }
            continue;
        }
        const QJsonObject cmd = v.toObject();
        const QString name = cmd.value(QStringLiteral("name")).toString();
        if (name.isEmpty()) {
            continue;
        }
        m_slashCommands.append({name,
                                cmd.value(QStringLiteral("description")).toString(),
                                cmd.value(QStringLiteral("hint")).toString()});
    }
    updateSlashPopup(); // an open popup must reflect the new list, not the old
}

// adoptModel points the picker at the model the CLI says it is running now.
// Silent: no maybePushOption round-trip, because the CLI is reporting a change
// it has ALREADY made — echoing it back would be a redundant setOption, and on
// a refusal fallback it would try to re-select the model that just failed.
void AgentPanel::adoptModel(const QString &modelId)
{
    if (modelId.isEmpty() || !m_modelCombo) {
        return;
    }
    QSignalBlocker block(m_modelCombo);
    const int idx = m_modelCombo->findData(modelId);
    if (idx >= 0) {
        m_modelCombo->setCurrentIndex(idx);
    } else if (m_modelCombo->isEditable()) {
        // A fallback can land on a model that is not in our catalogue; the
        // editable combo can still show it verbatim, which beats leaving the
        // picker naming a model the agent is no longer using.
        m_modelCombo->setCurrentIndex(-1);
        m_modelCombo->setEditText(modelId);
    }
}

// renderSystemSubtype dispatches the `system` events that are neither init nor
// the task lifecycle. Everything the CLI can emit here is either shown, folded
// into panel state, or listed as a deliberate silence — a subtype nobody
// recognises falls through to the ignore list rather than to a mystery row.
// There is deliberately no "unhandled" outcome to report: every subtype ends in
// one of those three, so the function returns nothing for a caller to check.
void AgentPanel::renderSystemSubtype(const QString &subtype, const QJsonObject &ev)
{
    if (subtype == QLatin1String("compact_boundary")) {
        // The conversation before this point was replaced by a summary. Drawn
        // as a full-width rule so it reads as a break in the transcript rather
        // than as one more status line.
        const qlonglong preTokens =
            ev.value(QStringLiteral("pre_tokens")).toVariant().toLongLong();
        const QString what = preTokens > 0
            ? i18nc("compaction separator, with the token count that was summarized",
                    "context compacted — %1 tokens summarized",
                    QLocale().toString(preTokens))
            : i18n("context compacted");
        addNote(QStringLiteral("<hr>") + what.toHtmlEscaped(), QStringLiteral("sys"));
        return;
    }
    if (subtype == QLatin1String("model_fallback")
        || subtype == QLatin1String("model_refusal_fallback")
        || subtype == QLatin1String("model_consent_fallback")
        || subtype == QLatin1String("model_refusal_no_fallback")) {
        // The CLI switched models under us (capacity, a refusal, a consent
        // gate). The picker has to follow or it names a model that is no longer
        // answering — the stale-mode bug, in the model dimension.
        const QString to = ev.value(QStringLiteral("model")).toString();
        const QString reason = ev.value(QStringLiteral("reason")).toString();
        if (subtype == QLatin1String("model_refusal_no_fallback")) {
            // Nothing to switch TO: the turn is stuck on the current model.
            addNote(i18n("the model declined and no fallback is configured%1",
                         reason.isEmpty() ? QString()
                                          : QStringLiteral(" (%1)").arg(reason))
                        .toHtmlEscaped(),
                    QStringLiteral("err"));
            return;
        }
        adoptModel(to);
        addNote(reason.isEmpty()
                    ? i18n("switched to %1", to).toHtmlEscaped()
                    : i18n("switched to %1 (%2)", to, reason).toHtmlEscaped(),
                QStringLiteral("sys"));
        refresh();
        return;
    }
    if (subtype == QLatin1String("api_error")) {
        // The API failed mid-turn. The CLI spells the cause differently
        // depending on the failure, so take whichever field is populated rather
        // than showing an empty error row.
        QString text = ev.value(QStringLiteral("message")).toString();
        if (text.isEmpty()) {
            text = ev.value(QStringLiteral("error")).toString();
        }
        addNote(text.isEmpty() ? i18n("the API call failed") : text.toHtmlEscaped(),
                QStringLiteral("err"));
        return;
    }
    // State-only subtypes: real information the panel already sources
    // elsewhere, so they update nothing here and must not add a row.
    //   turn_duration        — the `result` event's duration_ms already feeds
    //                          the average-turn readout.
    //   status               — a liveness tick.
    //   session_state_changed— the session id is tracked by the core (run.go),
    //                          which persists it for resume.
    //   post_turn_summary    — a recap of what the turn did; the feed just
    //                          showed all of it.
    if (subtype == QLatin1String("turn_duration") || subtype == QLatin1String("status")
        || subtype == QLatin1String("session_state_changed")
        || subtype == QLatin1String("post_turn_summary")) {
        return;
    }
    if (subtype == QLatin1String("commands_changed")) {
        // A skill or plugin appeared/disappeared mid-session. The event repeats
        // the list in the same shape init seeds it from.
        seedSlashCommands(ev.value(QStringLiteral("slash_commands")).toArray());
        return;
    }
    // Deliberate silence for everything else. The CLI adds system subtypes
    // between releases and an unknown one is not a user-facing event: showing
    // it would put raw protocol chatter in the conversation. Add a case above
    // when a new subtype turns out to be worth surfacing.
}

// applyRateLimit folds one rate_limit_event into the header chip. The CLI emits
// one every turn, so the feed only ever hears about a status TRANSITION —
// per-event rows would bury the conversation.
void AgentPanel::applyRateLimit(const QJsonObject &info)
{
    if (info.isEmpty() || m_replaying) {
        return;
    }
    const QString status = info.value(QStringLiteral("status")).toString();
    const QString previous = m_rateLimitStatus;
    m_rateLimitStatus = status;
    m_rateLimitType = info.value(QStringLiteral("rateLimitType")).toString();
    m_rateLimitOverage = info.value(QStringLiteral("isUsingOverage")).toBool();

    // resetsAt is a unix timestamp on some builds and an ISO-8601 string on
    // others; accept both and show nothing rather than a wrong time.
    const QJsonValue resets = info.value(QStringLiteral("resetsAt"));
    QDateTime when;
    if (resets.isDouble()) {
        when = QDateTime::fromSecsSinceEpoch(qint64(resets.toDouble()));
    } else {
        when = QDateTime::fromString(resets.toString(), Qt::ISODateWithMs);
        if (!when.isValid()) {
            when = QDateTime::fromString(resets.toString(), Qt::ISODate);
        }
    }
    m_rateLimitResetsAt = when;
    m_rateLimitResets = when.isValid()
        ? QLocale().toString(when.toLocalTime().time(), QLocale::ShortFormat)
        : QString();

    // Hoist it out of this widget (audit F43). Until now this state fed the
    // header chip and nothing else, so a parked agent kept showing the roster's
    // green "Working" arc and the only way to learn otherwise was to open it.
    // The shared state is also plan 28 §Phase 2's input — it wants the
    // timestamp, which is why the QDateTime and not the formatted string goes.
    if (!m_threadId.isEmpty()) {
        agentkate::RateLimitState::self()->report(
            m_threadId, agentkate::RateLimitReport{status, m_rateLimitType, when,
                                                   m_rateLimitOverage});
    }

    if (!previous.isEmpty() && previous != status) {
        const bool ok = status == QLatin1String("allowed");
        addNote(m_rateLimitResets.isEmpty()
                    ? i18n("usage limit status: %1", status).toHtmlEscaped()
                    : i18n("usage limit status: %1 — resets at %2", status,
                           m_rateLimitResets)
                          .toHtmlEscaped(),
                ok ? QStringLiteral("ok") : QStringLiteral("err"));
    }
    refresh();
}

// applyRateWake folds one `_ratewake` event in: the core's account of what the
// automatic resume (plan 28 §Phase 2) is doing for THIS thread.
//
// Every state is spoken aloud, and the skip is the important one. Being told
// "resuming at 14:37" and then finding an agent that never moved is worse than
// never having been promised anything, so a resume the core declined to perform
// says so, with its reason, in the conversation it declined to resume.
void AgentPanel::applyRateWake(const QJsonObject &ev)
{
    if (m_replaying) {
        return; // a schedule from a past run is not news on replay
    }
    const QString state = ev.value(QStringLiteral("state")).toString();
    const QString reason = ev.value(QStringLiteral("reason")).toString();
    const QJsonValue at = ev.value(QStringLiteral("at"));
    const QDateTime when = at.isDouble()
        ? QDateTime::fromSecsSinceEpoch(qint64(at.toDouble()))
        : QDateTime();
    const QString clock = when.isValid()
        ? QLocale().toString(when.toLocalTime().time(), QLocale::ShortFormat)
        : QString();

    if (state == QLatin1String("armed")) {
        m_rateWakeAt = when;
        if (!m_threadId.isEmpty()) {
            agentkate::RateLimitState::self()->noteWake(m_threadId, when);
        }
        addNote(clock.isEmpty()
                    ? i18n("Paused by a usage limit — this agent will resume "
                           "itself when the window reopens.")
                          .toHtmlEscaped()
                    : i18n("Paused by a usage limit — this agent will resume "
                           "itself at %1, as long as Agent Kate is still open.",
                           clock)
                          .toHtmlEscaped(),
                QStringLiteral("sys"));
    } else if (state == QLatin1String("fired")) {
        m_rateWakeAt = QDateTime();
        if (!m_threadId.isEmpty()) {
            agentkate::RateLimitState::self()->clearWake(m_threadId);
        }
        addNote(i18n("The usage window reopened — resuming this agent now.")
                    .toHtmlEscaped(),
                QStringLiteral("sys"));
    } else if (state == QLatin1String("skipped")) {
        m_rateWakeAt = QDateTime();
        if (!m_threadId.isEmpty()) {
            agentkate::RateLimitState::self()->clearWake(m_threadId);
        }
        addNote(reason.isEmpty()
                    ? i18n("The scheduled resume did not run.").toHtmlEscaped()
                    : i18n("The scheduled resume did not run: %1", reason).toHtmlEscaped(),
                QStringLiteral("err"));
    } else if (state == QLatin1String("cancelled")) {
        // No feed row: a cancellation means the stall is over, which the status
        // transition note already reports. Just stop claiming a resume.
        m_rateWakeAt = QDateTime();
        if (!m_threadId.isEmpty()) {
            agentkate::RateLimitState::self()->clearWake(m_threadId);
        }
    } else {
        return; // a state this build does not know: change nothing
    }
    refresh();
}

// rateLimitParked: see the header. The armed wake counts on its own because an
// engine that EXITS on an exhausted window takes its last report with it — the
// schedule is then the only remaining evidence that this agent is waiting
// rather than finished.
bool AgentPanel::rateLimitParked() const
{
    const QDateTime now = QDateTime::currentDateTime();
    if (m_rateWakeAt.isValid() && m_rateWakeAt > now) {
        return true;
    }
    if (m_rateLimitStatus.isEmpty()
        || m_rateLimitStatus == QLatin1String("allowed")
        || m_rateLimitStatus == QLatin1String("allowed_warning")) {
        return false;
    }
    // A limit whose reset time has passed is over, whatever the last event
    // said — the parked agent sends nothing more to correct it.
    return !m_rateLimitResetsAt.isValid() || m_rateLimitResetsAt > now;
}

void AgentPanel::clearRateLimitClaim(bool alsoDropArmedResume)
{
    if (!m_threadId.isEmpty()) {
        if (alsoDropArmedResume) {
            agentkate::RateLimitState::self()->forget(m_threadId);
        } else {
            agentkate::RateLimitState::self()->forgetReport(m_threadId);
        }
    }
    if (alsoDropArmedResume) {
        m_rateWakeAt = QDateTime();
    }
    if (m_rateLimitStatus.isEmpty()) {
        return; // nothing claimed; don't repaint the header for nothing
    }
    m_rateLimitStatus.clear();
    m_rateLimitType.clear();
    m_rateLimitResets.clear();
    m_rateLimitResetsAt = QDateTime();
    m_rateLimitOverage = false;
    refresh();
}

// --- claude stream channel ---------------------------------------------------

void AgentPanel::resetStreamState()
{
    if (m_streamFlush) {
        // Helpers share the coalescer, so anything they left dirty must be
        // painted before the timer is stopped or that text is simply lost.
        flushSubagentText();
        m_streamFlush->stop();
    }
    // A stream that ends without its content_block_stop — an interrupt, a CLI
    // crash, a thread rebind — leaves its row holding the escaped plain text
    // the flush ticks painted. Render it properly before dropping the state,
    // or that message stays raw for the rest of the session. Blocks that
    // already settled (the normal path) cost nothing here.
    const QList<int> indices = m_streamBlocks.keys();
    for (int index : indices) {
        const auto it = m_streamBlocks.constFind(index);
        if (it != m_streamBlocks.constEnd() && !it->settled) {
            settleStreamBlock(index);
        }
    }
    m_streamBlocks.clear();
    m_streamTextKeys.clear();
    m_streamClaimed = 0;
    m_streamThinking.clear();
}

int AgentPanel::takeStreamedTextKey()
{
    if (m_streamClaimed >= m_streamTextKeys.size()) {
        return -1;
    }
    return m_streamTextKeys.at(m_streamClaimed++);
}

// streamingHtml renders in-flight text as-is: HTML-escaped, with hard newlines
// kept (QTextDocument collapses them otherwise). No Markdown parse — see
// flushStreamedText for why a partial block must not be parsed.
static QString streamingHtml(const QString &text)
{
    QString html = text.toHtmlEscaped();
    html.replace(QLatin1Char('\n'), QLatin1String("<br>"));
    return html;
}

// Shown tail of a helper's forwarded output, and the size the accumulation is
// allowed to reach before it is cut back to that tail. Trimming at 2x means the
// cut is amortised over ~kMaxSubagentChars characters of stream instead of
// running on every single delta.
constexpr int kMaxSubagentChars = 4000;
constexpr int kSubagentTrimAt = 2 * kMaxSubagentChars;

// subagentShown renders the bounded accumulation the way the row displays it:
// the last kMaxSubagentChars characters, marked with a leading "…" when
// anything before them was dropped.
static QString subagentShown(const QString &text, bool trimmed)
{
    if (text.size() > kMaxSubagentChars) {
        return QStringLiteral("…") + text.right(kMaxSubagentChars);
    }
    return trimmed ? QStringLiteral("…") + text : text;
}

void AgentPanel::flushSubagentText()
{
    for (auto it = m_subagent.begin(); it != m_subagent.end(); ++it) {
        if (!it->dirty) {
            continue;
        }
        if (it->rowKey < 0) {
            // Re-resolve: rowKey is only ever assigned in routeSubagentText, so
            // an entry buffered BEFORE its Task row existed would have kept a
            // stale -1 forever and the "hold it dirty until the row appears"
            // recovery below could never fire. The lookup is what makes it fire.
            it->rowKey = m_toolRows.value(it.key(), -1);
        }
        if (it->rowKey < 0) {
            // Still no Task row to paint into (the tool_use row may still be
            // coming, or tools are hidden / the row was evicted). Leave `dirty`
            // set and keep the buffered text: clearing it here would claim the
            // text was painted and drop it for good. It lands on the first tick
            // after the row resolves.
            continue;
        }
        m_model->setToolProgress(it->rowKey, subagentShown(it->text, it->trimmed));
        it->dirty = false; // cleared only once the text is actually on a row
    }
}

void AgentPanel::flushStreamedText()
{
    // Helpers first: their rows are part of the same repaint this tick pays for.
    flushSubagentText();
    bool painted = false;
    for (auto it = m_streamBlocks.begin(); it != m_streamBlocks.end(); ++it) {
        if (!it->dirty) {
            continue;
        }
        it->dirty = false;
        if (it->thinking) {
            // Reasoning gets no row of its own while it streams — its tail
            // rides the working line, on this same tick so a fast thinking
            // stream costs one label update per 50ms instead of one per token.
            const QString tail = it->text.right(120).simplified();
            if (tail != m_streamThinking) {
                m_streamThinking = tail;
                m_working->setActivity(i18n("thinking… %1", tail));
            }
            continue;
        }
        if (it->key < 0) {
            continue;
        }
        // A still-streaming block renders as escaped plain text, NOT Markdown:
        // markdownToHtml re-parses the whole accumulated message on every tick,
        // which is the dominant cost of a long stream, and a partial delta is
        // not valid Markdown anyway (a delta can split "**bo" / "ld**"), so the
        // parse would be paid for a half-rendered result. settleStreamBlock()
        // runs the real render once the block stops.
        m_model->setMessageBody(it->key, streamingHtml(it->text), it->text);
        painted = true;
    }
    if (painted && m_stickBottom) {
        scrollFeedToBottom();
    }
}

// settleStreamBlock renders a finished text block properly — the one
// markdownToHtml call a streamed message pays, replacing the escaped plain text
// the flush ticks painted. Idempotent: the authoritative `assistant` event
// re-renders the same row with its own (identical) text.
void AgentPanel::settleStreamBlock(int blockIndex)
{
    auto it = m_streamBlocks.find(blockIndex);
    if (it == m_streamBlocks.end()) {
        return;
    }
    it->dirty = false;
    if (it->key < 0 || it->thinking || it->text.isEmpty()) {
        return;
    }
    it->settled = true;
    m_model->setMessageBody(it->key, agentkate::markdownToHtml(it->text), it->text);
    if (m_stickBottom) {
        scrollFeedToBottom();
    }
}

void AgentPanel::renderStreamEvent(const QJsonObject &inner)
{
    // Stored transcripts hold no stream_events, so this never runs during
    // replay; the guard keeps that true if a future transcript format changes.
    if (m_replaying || inner.isEmpty()) {
        return;
    }
    const QString type = inner.value(QStringLiteral("type")).toString();
    const int index = inner.value(QStringLiteral("index")).toInt(-1);

    if (type == QLatin1String("message_start")) {
        // A new assistant message: whatever the previous one left behind is
        // finished with (its authoritative event has already claimed its rows).
        resetStreamState();
        return;
    }
    if (type == QLatin1String("content_block_start")) {
        if (index < 0) {
            return;
        }
        const QString blockType = inner.value(QStringLiteral("content_block"))
                                      .toObject()
                                      .value(QStringLiteral("type"))
                                      .toString();
        StreamBlock block;
        if (blockType == QLatin1String("text")) {
            // Open the provisional row empty; deltas fill it in place.
            block.key = m_model->appendMessage(
                QStringLiteral("Agent Kate"),
                isDark(this) ? QStringLiteral("#5fd3bf") : QStringLiteral("#1a7f6b"),
                QString(), QString(), false,
                QLocale().toString(QTime::currentTime(), QLocale::ShortFormat));
            m_streamTextKeys.append(block.key);
            // Same unread bookkeeping addMessageCard does: a message arriving
            // while the reader is scrolled up flags the jump button, and it
            // should flag on the FIRST token, not when the turn finishes.
            if (!m_stickBottom) {
                m_jumpUnread = true;
                updateJumpButton();
            }
        } else if (blockType == QLatin1String("thinking")) {
            block.thinking = true;
        } else {
            // tool_use and friends: their input arrives as partial JSON, which
            // is unrenderable until complete. The authoritative `assistant`
            // event draws the tool card.
            return;
        }
        m_streamBlocks.insert(index, block);
        return;
    }
    if (type == QLatin1String("content_block_delta")) {
        auto it = m_streamBlocks.find(index);
        if (it == m_streamBlocks.end()) {
            return;
        }
        const QJsonObject delta = inner.value(QStringLiteral("delta")).toObject();
        const QString deltaType = delta.value(QStringLiteral("type")).toString();
        if (deltaType == QLatin1String("text_delta")) {
            it->text += delta.value(QStringLiteral("text")).toString();
        } else if (deltaType == QLatin1String("thinking_delta")) {
            // Reasoning gets no row of its own while it streams — the
            // authoritative event appends the collapsed thinking card. Its tail
            // rides the working line, which is where "what is it doing right
            // now" already lives.
            it->text += delta.value(QStringLiteral("thinking")).toString();
        } else {
            return; // input_json_delta and friends: nothing renderable yet
        }
        // Both kinds go through the coalescing tick: a thinking delta repainting
        // the working line per token is the same cost the text path pays.
        it->dirty = true;
        if (!m_streamFlush->isActive()) {
            m_streamFlush->start();
        }
        return;
    }
    if (type == QLatin1String("content_block_stop")) {
        // The block is complete: swap the escaped plain text the flush ticks
        // painted for the real Markdown render. Unconditional (not gated on
        // `dirty`), because the last tick may have cleared the flag while the
        // row still holds plain text.
        settleStreamBlock(index);
        return;
    }
    // message_delta (stop_reason / usage) and message_stop carry nothing the
    // feed shows: the `result` event owns the turn's usage and its end.
}

// routeSubagentText shows a helper's forwarded output on the Task tool row that
// launched it, growing as it arrives, instead of leaving that row at "⋯" until
// the helper finishes. The row is the transcript's existing subagent surface —
// the Helpers menu still opens the full stored transcript afterwards.
bool AgentPanel::routeSubagentText(const QJsonObject &ev, const QString &parentToolUseId)
{
    if (m_replaying) {
        return false; // a stored transcript is replayed exactly as it was written
    }
    const QString type = ev.value(QStringLiteral("type")).toString();
    QString text;
    if (type == QLatin1String("stream_event")) {
        const QJsonObject inner = ev.value(QStringLiteral("event")).toObject();
        if (inner.value(QStringLiteral("type")).toString()
            != QLatin1String("content_block_delta")) {
            return true; // consumed: a helper's block boundaries are not ours
        }
        const QJsonObject delta = inner.value(QStringLiteral("delta")).toObject();
        if (delta.value(QStringLiteral("type")).toString() != QLatin1String("text_delta")) {
            return true;
        }
        text = delta.value(QStringLiteral("text")).toString();
    } else if (type == QLatin1String("assistant")) {
        // The helper's authoritative message. It repeats text its own deltas
        // already delivered, so it REPLACES the accumulation for this helper
        // rather than adding to it.
        QString whole;
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            if (b.value(QStringLiteral("type")).toString() == QLatin1String("text")) {
                whole += b.value(QStringLiteral("text")).toString();
            }
        }
        if (whole.isEmpty()) {
            return true;
        }
        SubagentText &entry = m_subagent[parentToolUseId];
        entry.text = whole;
        entry.trimmed = false;
        // The authoritative message is the end of this helper's stream and may
        // be followed immediately by its tool_result, so it paints NOW rather
        // than waiting for a tick that could land after the final result.
        entry.rowKey = m_toolRows.value(parentToolUseId, -1);
        if (entry.rowKey >= 0) {
            m_model->setToolProgress(entry.rowKey,
                                     subagentShown(entry.text, entry.trimmed));
            entry.dirty = false;
        } else {
            // No row yet: hold the text dirty so the flush paints it when the
            // Task row appears, instead of losing the helper's final message.
            entry.dirty = true;
        }
        return true;
    } else {
        // A helper's user/tool_result echoes, its own result event: consumed so
        // they are never attributed to this agent, but not shown — the helper's
        // stored transcript is the place for that detail.
        return true;
    }
    if (text.isEmpty()) {
        return true;
    }
    SubagentText &entry = m_subagent[parentToolUseId];
    entry.text += text;
    // Bounded: the tool row shows the tail of a helper's output, and the full
    // conversation is in the helper's own transcript. Without this a long
    // subagent run would grow one transcript row without limit. Cutting back
    // only at twice the cap keeps the per-delta cost O(delta) — the old code
    // re-trimmed once past the cap, i.e. built two ~4000-char strings per token.
    if (entry.text.size() > kSubagentTrimAt) {
        entry.text = entry.text.right(kMaxSubagentChars);
        entry.trimmed = true;
    }
    entry.rowKey = m_toolRows.value(parentToolUseId, -1);
    // Coalesced exactly like the agent's own text deltas: one repaint per tick
    // per row, not one per token.
    entry.dirty = true;
    if (m_streamFlush && !m_streamFlush->isActive()) {
        m_streamFlush->start();
    }
    return true;
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

    // Anything tagged with a parent tool-use id was produced by a HELPER this
    // agent launched (--forward-subagent-text), not by the agent itself.
    // Rendering it as an "Agent Kate" card would attribute a subagent's words
    // to the agent; it belongs on the Task tool row that started it.
    const QString parentToolUse =
        ev.value(QStringLiteral("parent_tool_use_id")).toString();
    if (!parentToolUse.isEmpty() && routeSubagentText(ev, parentToolUse)) {
        return;
    }

    // Completed AskUserQuestion interactions live in Agent Kate's private
    // replay sidecar: neither engine's transcript reliably retains both the
    // prompt and the human's chosen answer.  It is a history row, not a live
    // form — answers always travel through permission.respond while the turn
    // is active.  Escape every field because questions are agent-authored.
    if (type == QLatin1String("_question")) {
        const QJsonObject input = ev.value(QStringLiteral("input")).toObject();
        const QJsonObject answers = ev.value(QStringLiteral("answer"))
                                        .toObject()
                                        .value(QStringLiteral("answers"))
                                        .toObject();
        const bool answered = ev.value(QStringLiteral("answered")).toBool();
        QStringList lines;
        for (const QJsonValue &qv : input.value(QStringLiteral("questions")).toArray()) {
            const QJsonObject question = qv.toObject();
            const QString text = question.value(QStringLiteral("question")).toString();
            if (text.isEmpty()) {
                continue;
            }
            const QJsonValue choice = answers.value(text);
            QString answer;
            if (choice.isArray()) {
                QStringList selected;
                for (const QJsonValue &v : choice.toArray()) {
                    selected << v.toString();
                }
                answer = selected.join(QStringLiteral(", "));
            } else {
                answer = choice.toString();
            }
            lines << QStringLiteral("&#10067; <b>%1</b> &rarr; %2")
                         .arg(text.toHtmlEscaped(),
                              (answered && !answer.isEmpty()
                                   ? answer
                                   : i18n("dismissed")).toHtmlEscaped());
        }
        if (lines.isEmpty()) {
            lines << QStringLiteral("&#10067; %1").arg(
                answered ? i18n("answered the agent's question").toHtmlEscaped()
                         : i18n("the agent's question was dismissed").toHtmlEscaped());
        }
        addNote(lines.join(QStringLiteral("<br>")), answered ? QStringLiteral("ok")
                                                              : QStringLiteral("dim"));
        return;
    }

    if (type == QLatin1String("system")) {
        const QString subtype = ev.value(QStringLiteral("subtype")).toString();
        // The CLI's background-task lifecycle (run_in_background shells and
        // async subagents) drives the jobs tray, not the feed.
        if (subtype == QLatin1String("task_started")
            || subtype == QLatin1String("task_updated")
            || subtype == QLatin1String("task_notification")
            || subtype == QLatin1String("background_tasks_changed")) {
            handleTaskEvent(subtype, ev);
            return;
        }
        // The CLI's live thinking-size ticker: an honest "what is it doing
        // right now" for the working indicator during long reasoning.
        if (subtype == QLatin1String("thinking_tokens")) {
            if (!m_replaying) {
                const qlonglong n =
                    ev.value(QStringLiteral("estimated_tokens")).toVariant().toLongLong();
                if (n > 0) {
                    m_working->setActivity(
                        i18n("Agent Kate is thinking… (~%1 tokens)",
                             QLocale().toString(n)));
                }
            }
            return;
        }
        // Everything else the CLI reports as a `system` event — model
        // fallbacks, compaction boundaries, API errors, rate/status ticks.
        if (subtype != QLatin1String("init")) {
            renderSystemSubtype(subtype, ev);
            return;
        }
        // The claude init event lists the session's slash commands (names
        // only) — the composer's autocomplete feed.
        seedSlashCommands(ev.value(QStringLiteral("slash_commands")).toArray());
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

    } else if (type == QLatin1String("stream_event")) {
        // Token-by-token deltas. Deliberately before the assistant branch: the
        // provisional rows they paint are what the authoritative `assistant`
        // event then overwrites.
        renderStreamEvent(ev.value(QStringLiteral("event")).toObject());

    } else if (type == QLatin1String("assistant")) {
        const QJsonArray content =
            ev.value(QStringLiteral("message")).toObject().value(QStringLiteral("content")).toArray();
        for (const QJsonValue &bv : content) {
            const QJsonObject b = bv.toObject();
            const QString bt = b.value(QStringLiteral("type")).toString();
            if (bt == QLatin1String("text")) {
                const QString t = b.value(QStringLiteral("text")).toString().trimmed();
                // Claim one provisional row for EVERY text block this event
                // covers, empty or not: the stream opened a row per text
                // content_block_start, so skipping the claim for an empty block
                // would shift every later claim onto the wrong row and make the
                // next message replace a card it doesn't own. Only the
                // row-replacement below is conditional on there being text.
                // takeStreamedTextKey() returns -1 during replay and on any
                // turn that produced no stream_events, so a stored transcript
                // replays exactly as before.
                const int streamedKey = takeStreamedTextKey();
                if (!t.isEmpty()) {
                    // This event is authoritative. If the same text already
                    // streamed into a provisional row, overwrite that row —
                    // appending here would show every streamed message twice.
                    if (streamedKey < 0
                        || !m_model->setMessageBody(streamedKey,
                                                    agentkate::markdownToHtml(t), t)) {
                        addMessageCard(QStringLiteral("Agent Kate"),
                                       isDark(this) ? QStringLiteral("#5fd3bf")
                                                    : QStringLiteral("#1a7f6b"),
                                       agentkate::markdownToHtml(t), t, m_replaying);
                    } else if (!m_replaying) {
                        // addMessageCard's roster side-effect, which the
                        // in-place path skips along with the row insert.
                        emit previewChanged(t.simplified());
                    }
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
                    // A helper's text can arrive before its Task row exists;
                    // flushSubagentText keeps that text buffered and dirty
                    // rather than dropping it. The row now exists, so kick a
                    // tick — otherwise the buffered text waits for a delta
                    // that may never come.
                    if (m_streamFlush && m_subagent.contains(id)
                        && m_subagent.value(id).dirty && !m_streamFlush->isActive()) {
                        m_streamFlush->start();
                    }
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
                    // Did the tool FAIL? Both engines say so on the block:
                    // claude natively, kimi's translator on a failed tool call
                    // (core/internal/kimi/translate.go). Never read before, so
                    // a failure rendered as a ✓ (audit F40).
                    const bool toolFailed =
                        b.value(QStringLiteral("is_error")).toBool();
                    m_model->setToolResult(key, shown, full, truncated, toolFailed);
                    // Image blocks in the result (screenshots and the like)
                    // become clickable thumbnail chips on the tool row. The
                    // bytes go to the cache dir — the transcript model holds
                    // only paths, so a screenshot-heavy session can't balloon
                    // the feed's memory.
                    const auto images =
                        agentkate::toolResultImages(b.value(QStringLiteral("content")));
                    if (!images.isEmpty()) {
                        const QString dir =
                            QStandardPaths::writableLocation(QStandardPaths::CacheLocation)
                            + QStringLiteral("/tool-images");
                        QDir().mkpath(dir);
                        QJsonArray atts;
                        for (int i = 0; i < images.size() && i < 6; ++i) {
                            QString ext = QStringLiteral("png");
                            const QString mt = images.at(i).first;
                            if (mt == QLatin1String("image/jpeg")) {
                                ext = QStringLiteral("jpg");
                            } else if (mt == QLatin1String("image/gif")) {
                                ext = QStringLiteral("gif");
                            } else if (mt == QLatin1String("image/webp")) {
                                ext = QStringLiteral("webp");
                            }
                            const QString path = dir
                                + QStringLiteral("/%1-%2-%3.%4")
                                      .arg(m_threadId)
                                      .arg(key)
                                      .arg(i + 1)
                                      .arg(ext);
                            QFile f(path);
                            if (!f.open(QIODevice::WriteOnly)) {
                                continue;
                            }
                            f.write(images.at(i).second);
                            f.close();
                            atts.append(QJsonObject{
                                {QStringLiteral("name"), QFileInfo(path).fileName()},
                                {QStringLiteral("kind"), QStringLiteral("image")},
                                {QStringLiteral("path"), path}});
                        }
                        if (!atts.isEmpty()) {
                            m_model->setToolAttachments(key, atts);
                        }
                    }
                    // A Workflow launch result carries the run's Task ID /
                    // Transcript dir / Run ID — capture it as this thread's latest
                    // followable workflow and reveal the chip.
                    if (m_workflowToolKeys.remove(key)) {
                        noteWorkflowLaunch(m_workflowInputByKey.take(key), full);
                    }
                    // A background task's launch result names its output file
                    // ("Output is being written to: …" for shells,
                    // "output_file: …" for async subagents) — capture it so
                    // the tray chip can open the output before completion.
                    // Taken, not read: this is the one result that consults the
                    // mapping, so leaving the entry behind is pure growth.
                    const QString taskId = m_taskByToolUse.take(id);
                    if (!taskId.isEmpty()) {
                        auto jobIt = m_bgJobs.find(taskId);
                        if (jobIt != m_bgJobs.end() && jobIt->outputFile.isEmpty()) {
                            static const QRegularExpression pathRe(QStringLiteral(
                                "(?:Output is being written to:|output_file:)\\s*(\\S+?\\.output)"));
                            const QRegularExpressionMatch m = pathRe.match(full);
                            if (m.hasMatch()) {
                                jobIt->outputFile = m.captured(1);
                                updateJobsBar();
                            }
                        }
                    }
                    // A tool_use has exactly one tool_result; the mapping is dead
                    // once applied. Dropping it bounds m_toolRows and lets the key
                    // fall away with the row when it is eventually evicted.
                    m_toolRows.remove(id);
                    // If this was a Task row, the helper's forwarded text is
                    // finished with — and a still-pending coalesced paint would
                    // now overwrite the real result with a progress tail.
                    m_subagent.remove(id);
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
        // Whether these numbers are a TURN's spend or a running context
        // snapshot. Only an engine with usageReporting bills per turn; kimi's
        // result usage is its `/usage` readout — a cumulative context fill that
        // repeats most of itself every turn. Summing it produced session totals
        // that grew quadratically and a per-turn line that read as billing that
        // never happened (audit F19b). Note the registry answers an UNKNOWN
        // engine id with claude-shaped defaults, so "unknown" means billed —
        // only a harness that positively declares no usage reporting is
        // excluded, which is exactly the case this guards.
        const bool billed = currentTraits().usageReporting;

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
            QString line =
                billed ? i18nc("turn-usage summary",
                               "%1 · %2 in / %3 out · %4% cache hit",
                               head, loc.toString(inTok), loc.toString(outTok),
                               cacheHitPct)
                       // Not a per-turn spend: say what the number IS. The
                       // engine reported how full the context is, and calling
                       // that "in / out" would invent a bill.
                       : i18nc("turn context summary",
                               "%1 · context %2 tokens", head,
                               loc.toString(promptTotal));
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
            // Accumulate only where the engine reports per-turn spend; a
            // cumulative readout must never be summed (see `billed` above).
            if (!m_replaying && billed) {
                m_sessionCostUsd += costUsd;
                m_sessionInTokens += inTok;
                m_sessionOutTokens += outTok;
            }
            // Context-fill meter: the latest turn's prompt total is what the
            // context currently holds; the window comes from modelUsage (the
            // entry doing the main conversation — the one with the most
            // prompt-side tokens). This is the number that predicts
            // auto-compaction.
            // Estimate only — a `_context` event from the engine's own
            // accounting supersedes it and must not be clobbered here.
            if (promptTotal > 0 && !m_ctxExact) {
                m_ctxPromptTokens = promptTotal;
            }
            const QJsonObject perModel = ev.value(QStringLiteral("modelUsage")).toObject();
            qlonglong best = -1;
            for (auto it = perModel.constBegin(); it != perModel.constEnd(); ++it) {
                const QJsonObject u = it.value().toObject();
                const qlonglong promptSide =
                    u.value(QStringLiteral("inputTokens")).toVariant().toLongLong()
                    + u.value(QStringLiteral("cacheReadInputTokens")).toVariant().toLongLong()
                    + u.value(QStringLiteral("cacheCreationInputTokens"))
                          .toVariant()
                          .toLongLong();
                const qlonglong window =
                    u.value(QStringLiteral("contextWindow")).toVariant().toLongLong();
                if (window > 0 && promptSide > best) {
                    best = promptSide;
                    if (m_ctxWindow <= 0 || !m_ctxExact) {
                        m_ctxWindow = window;
                    }
                }
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
            // A tripped cost budget ends the turn through the same error result,
            // and its `subtype` is the only field that names the cause. Without
            // this the panel would show a bare "turn ended with an error" for a
            // ceiling the human set themselves.
            const QString subtype = ev.value(QStringLiteral("subtype")).toString();
            if (subtype.contains(QLatin1String("budget"), Qt::CaseInsensitive)) {
                addNote(i18n("The cost budget for this agent is used up. Start a "
                             "new agent, or raise the budget and start again."),
                        QStringLiteral("err"));
            }
        }
        // Turn timing: fold this turn into the session's average and stop the
        // elapsed readout. duration_ms is the CLI's own wall time; fall back
        // to nothing rather than guessing.
        if (!m_replaying && durationMs > 0) {
            m_turnDurTotalMs += durationMs;
            ++m_turnDurCount;
            m_working->setAverageTurnMs(m_turnDurTotalMs / m_turnDurCount);
        }
        m_working->setTurnStart(0);
        m_idle = true;
        // The turn is over: any provisional streamed row has been replaced by
        // its authoritative event (or was orphaned by an interrupt), and the
        // block indices restart at 0 on the next turn.
        resetStreamState();
        m_subagent.clear();
        refresh();
        // The turn boundary is the moment a queued follow-up can fire.
        drainSendQueue();

    } else if (type == QLatin1String("rate_limit_event")) {
        // Emitted every turn, so it is header state — never a feed row except
        // on a status transition (see applyRateLimit).
        applyRateLimit(ev.value(QStringLiteral("rate_limit_info")).toObject());

    } else if (type == QLatin1String("_ratewake")) {
        // The core's automatic resume for this thread: armed / cancelled /
        // firing now / deliberately skipped (plan 28 §Phase 2).
        applyRateWake(ev);

    } else if (type == QLatin1String("_commands")) {
        // The kimi CLI's command list (translated from ACP
        // available_commands_update) — replaces the autocomplete feed. Not a
        // feed row; purely composer state.
        seedSlashCommands(ev.value(QStringLiteral("commands")).toArray());

    } else if (type == QLatin1String("_options")) {
        // Native option snapshots are intentionally not a UI discovery path.
        // Current controls change only through typed AppliedSettings replies.

    } else if (type == QLatin1String("_context")) {
        // The core asked the running CLI what its context ACTUALLY holds
        // (claude's get_context_usage control request) after the last turn.
        // This outranks the estimate the result branch derives from prompt-side
        // token sums: those count what was sent, while the window also carries
        // the system prompt, tool schemas, memory files and the autocompact
        // buffer. Engines without the control channel send no such event and
        // the estimate stands.
        const qlonglong used =
            ev.value(QStringLiteral("usedTokens")).toVariant().toLongLong();
        const qlonglong max =
            ev.value(QStringLiteral("maxTokens")).toVariant().toLongLong();
        if (used > 0) {
            m_ctxPromptTokens = used;
            m_ctxExact = true;
        }
        if (max > 0) {
            m_ctxWindow = max;
        }
        m_ctxBreakdown = ev.value(QStringLiteral("breakdown")).toArray();
        refresh();

    } else if (type == QLatin1String("_stderr")) {
        // The CLI's own error channel, rendered in the error colour rather than
        // dim (audit F50): this is where a failed prompt, a bad flag or a
        // provider refusal lands, and dim reads as chatter to skip past.
        addNote(ev.value(QStringLiteral("text")).toString().toHtmlEscaped(),
                QStringLiteral("err"));

    } else if (type == QLatin1String("_lifecycle")) {
        const QString phase = ev.value(QStringLiteral("phase")).toString();
        const QString detail = ev.value(QStringLiteral("detail")).toString().toHtmlEscaped();
        if (phase == QLatin1String("started")) {
            m_isolated = ev.value(QStringLiteral("isolated")).toBool();
            m_branch = ev.value(QStringLiteral("branch")).toString();
            m_workdir = ev.value(QStringLiteral("workdir")).toString();
            emit worktreePathChanged(worktreePath());
            m_errored = false; // a clean start clears any prior failure state
            // The opening prompt reached the agent — it is no longer unsent.
            m_pendingOpening = QueuedMsg{};
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
            // A resumed process bills a fresh session — restart the meters so
            // the header doesn't show stale figures as new turns accrue.
            m_sessionCostUsd = 0.0;
            m_sessionInTokens = 0;
            m_sessionOutTokens = 0;
            m_ctxPromptTokens = 0;
            m_ctxWindow = 0;
            m_ctxExact = false;
            m_ctxBreakdown = QJsonArray();
            m_turnDurTotalMs = 0;
            m_turnDurCount = 0;
            m_working->setAverageTurnMs(0);
            addNote(detail + QStringLiteral(" · ready for a follow-up"),
                    QStringLiteral("sys"));
            emit dormantChanged(false);
            refresh();
            // A quick ask requested while this agent was dormant waits here
            // rather than overwriting the human's dormant composer draft.
            // Send through the ordinary composer (so all queue/frame rules
            // still apply) and restore the exact draft afterwards. If sending
            // is refused, keep BOTH texts visible instead of silently losing
            // either one.
            if (!m_pendingQuickAsk.isEmpty()) {
                const QString ask = std::exchange(m_pendingQuickAsk, QString());
                const QString draft = m_input->toPlainText();
                m_input->setPlainText(ask);
                onSendClicked();
                const QString after = m_input->toPlainText();
                if (after.trimmed().isEmpty()) {
                    m_input->setPlainText(draft);
                } else {
                    const QString combined = draft.isEmpty()
                        ? after : after + QStringLiteral("\n\n") + draft;
                    m_input->setPlainText(combined);
                    addNote(i18n("Quick ask could not be sent — it and your draft are "
                                 "in the composer."), QStringLiteral("err"));
                }
                refresh();
                return;
            }
            // Deliver any message the human typed before pressing Resume.
            if (!m_input->toPlainText().trimmed().isEmpty() || !m_attachments.isEmpty()) {
                onSendClicked();
            }
        } else if (phase == QLatin1String("notice")) {
            // A core-side event this thread should know about but that changes
            // no panel state (today: skills reloaded mid-session after a
            // catalogue install). Reusing _lifecycle keeps one wire contract.
            addNote(detail, QStringLiteral("sys"));
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
            // No `result` event closes an aborted turn, so this is the only
            // place its half-streamed blocks get settled and their unclaimed
            // row keys dropped — the next turn's blocks start at index 0.
            resetStreamState();
            refresh();
            // A follow-up queued during the interrupt can fire now.
            drainSendQueue();
        } else if (phase == QLatin1String("error")) {
            addNote(QStringLiteral("agent failed: %1").arg(detail), QStringLiteral("err"));
            // The turn died without a `result`: settle whatever streamed and
            // drop the state, so a later start doesn't claim these rows.
            resetStreamState();
            // Including the usage-limit claim: a dead turn is not waiting on a
            // quota window, and m_threadId is about to be cleared, after which
            // nothing could withdraw it (audit F43).
            clearRateLimitClaim();
            m_idle = false;
            m_promoting = false;
            m_errored = true; // roster card shows Error until the next send/resume
            if (!m_dormant) {
                m_threadId.clear(); // a fresh start failed — back to a blank panel
                // Silent clear (no threadIdChanged — nothing bound to the dead
                // id should act as if a new one arrived), so reap the job rows
                // published under it here.
                updateJobsBar();
            }
            // Nothing queued here can ever drain now — and if the fresh start
            // is what failed, the opening prompt never reached an agent either
            // (audit F37). Hand every unsent message back to the composer
            // instead of stranding it in the feed.
            restoreUnsentToComposer();
            refresh();
        } else if (phase == QLatin1String("exited")
                   || phase == QLatin1String("interrupted")) {
            const bool wasInterrupt = phase == QLatin1String("interrupted");
            // Background tasks are children of the agent process — they ended
            // with it. Their output files persist, so chips stay clickable.
            // Still-running ones died with the process: that is a failure, not
            // a completion, and the Jobs panel must not tick them.
            const qint64 diedMs = QDateTime::currentMSecsSinceEpoch();
            for (auto it = m_bgJobs.begin(); it != m_bgJobs.end(); ++it) {
                if (!it->done) {
                    it->done = true;
                    it->failed = true;
                    it->endedMs = diedMs;
                }
            }
            updateJobsBar();
            addNote(wasInterrupt
                        ? QStringLiteral("&#9209; stopped (resumable) — send a "
                                         "follow-up to continue this session")
                        : QStringLiteral("agent exited: %1").arg(detail),
                    wasInterrupt ? QStringLiteral("sys") : QStringLiteral("dim"));
            m_idle = false;
            m_permQueue.clear();
            m_permBar->setVisible(false);
            m_questionBox->setVisible(false);
            // The process is gone mid-stream (stopped, crashed): no `result`
            // and no authoritative `assistant` will arrive, so settle the
            // partial rows here rather than leaving them raw and claimable.
            resetStreamState();
            // A stopped or interrupted agent is not parked on a usage window —
            // it is not running at all. Its last rate_limit_event would
            // otherwise keep the roster's "N agents paused" strip up until the
            // panel was closed (audit F43); a resume re-reports on its first
            // turn.
            //
            // Its ARMED automatic resume survives, though: an engine that exits
            // because the account's window is exhausted is precisely the case
            // plan 28 §Phase 2 exists for, and the core still holds that
            // schedule. When the human is the one who stopped the agent, the
            // core cancels the wake and says so — this panel does not have to
            // guess which of the two just happened.
            clearRateLimitClaim(/*alsoDropArmedResume=*/false);
            // The session stopped before the queued follow-ups could fire (and,
            // if it died before it ever started, before the opening prompt did
            // either). Don't discard the human's text — put it back in the
            // composer so it can be re-sent (into a resumed session) with one
            // keystroke.
            restoreUnsentToComposer();
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
