#pragma once

#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QPair>
#include <QString>
#include <QWidget>

class CoreClient;
class ToolCard;
class WorkingIndicator;
class QAbstractButton;
class QCheckBox;
class QComboBox;
class QDragEnterEvent;
class QDragLeaveEvent;
class QDragMoveEvent;
class QDropEvent;
class QEvent;
class QMimeData;
class QPaintEvent;
class QFrame;
class QHBoxLayout;
class QLabel;
class QLineEdit;
class QMenu;
class QPlainTextEdit;
class QPushButton;
class QScrollArea;
class QTimer;
class KMessageWidget;
class QToolButton;
class QVBoxLayout;

// AgentPanel drives one agent thread: it starts a headless `claude` via the
// core, streams the conversation as a feed of cards, sends follow-ups, surfaces
// per-tool approval requests and AskUserQuestion forms, and can show the
// thread's diff. Many panels run side by side in an AgentDock; each routes
// events by its thread id.
class AgentPanel : public QWidget
{
    Q_OBJECT
public:
    explicit AgentPanel(CoreClient *core, QWidget *parent = nullptr);
    ~AgentPanel() override;

    void setWorkspace(const QString &path);
    QString threadId() const { return m_threadId; }
    bool isIsolated() const { return m_isolated; }
    // A live agent has a thread and a running process (not dormant/resumable).
    bool isRunning() const { return !m_threadId.isEmpty() && !m_dormant; }

    // Pre-pick the start model by its id ("opus", "sonnet", …) before the first
    // start. No-op once a thread exists (the combo is frozen then) or if the id
    // isn't an offered choice.
    void preselectModel(const QString &modelId);

    // Bind this panel to a persisted-but-not-running thread; resume() relaunches
    // it through the core's agent.resume.
    void setDormant(const QString &threadId, const QString &title, bool isolated);
    // resume() may first prompt the user to choose a compaction model when no
    // current summary is on disk, then dispatch to doResume() for the actual
    // agent.resume call.
    void resume();

    // Re-read chat preferences (send key, tool-card visibility) from KConfig.
    void applyChatSettings();

    // Attach a set of local file paths as context for the next message. Used
    // by drag-and-drop from ProjectTree (and by the Attach… button).
    void attachPaths(const QStringList &paths);

    // Attach a custom-MIME payload of {path,line,endLine} items. Ranged items
    // become a text excerpt named "file:start-end"; whole-file items defer to
    // attachPaths. Used by drops carrying line ranges from the search results.
    void attachItems(const QJsonArray &items);

Q_SIGNALS:
    void statusMessage(const QString &text);
    void titleChanged(const QString &title);
    void stateChanged(const QString &dotColorHex);
    // Human-readable one-line status (isolation / worktree branch / idle), used
    // as the roster card's subtitle. Tracks the same state as stateChanged.
    void subtitleChanged(const QString &text);
    void dormantChanged(bool dormant);
    // Roster card affordance: attention = a turn is waiting on the user's input
    // (a permission prompt). The roster paints this as a card marker.
    void attentionChanged(bool attention);
    void openDiff(const QString &title, const QString &diffText);

protected:
    bool eventFilter(QObject *obj, QEvent *event) override;
    void dragEnterEvent(QDragEnterEvent *event) override;
    void dragMoveEvent(QDragMoveEvent *event) override;
    void dragLeaveEvent(QDragLeaveEvent *event) override;
    void dropEvent(QDropEvent *event) override;
    void paintEvent(QPaintEvent *event) override;

private:
    // True when a drag carries our custom attachment MIME or at least one
    // local-file URL — used to reject pure-text/remote drags.
    bool canAcceptDrop(const QMimeData *mime) const;
    // One clarifying question currently shown to the human.
    struct QuestionField {
        QString question;
        bool multiSelect = false;
        QList<QPair<QString, QAbstractButton *>> options; // label -> button
    };

    void onSendClicked();
    // Append a "You" message card to the feed for the given outgoing message.
    void addYouCard(const QString &text, const QJsonArray &attachments);
    // Send a message to the live thread now: adds the You card, marks the turn
    // busy, and calls agent.send. Assumes a running (non-dormant) thread.
    void deliverMessage(const QString &text, const QJsonArray &attachments);
    // Fire the next queued follow-up, if any, once the thread is idle. Called
    // on every `result` event; sends one message per turn boundary.
    void drainSendQueue();
    // Rebuild the "queued messages" chip bar from m_sendQueue.
    void rebuildQueueChips();
    void onStopClicked();
    void onInterruptClicked();
    void onChangesClicked();
    void onPromoteClicked();
    void onAttachClicked();
    void rebuildAttachChips();
    // Surface why files were rejected (binary, too large, unreadable) in a
    // prominent inline banner rather than a transient status-bar message.
    void showAttachNotice(const QString &text);
    void onNotification(const QString &method, const QJsonObject &params);
    void renderEvent(const QJsonObject &event);
    void onPermissionRequested(const QJsonObject &params);
    // Pull the persisted Claude Code transcript and replay it into the feed so
    // a reopened dormant thread shows its prior conversation.
    void loadTranscript();
    // Send the current compact-combo + strip values to the core for this
    // thread. No-op when no thread exists yet — the choice is then sticky
    // local-only until a thread is created.
    void pushCompactStrategy();
    // Dispatch an on-demand compaction with the given model token
    // ("hot", "opus", "sonnet", "haiku", "local"). Reports progress and
    // result in the feed; does not change the thread's scheduled strategy.
    void runCompactNow(const QString &model);
    // Issue the actual agent.resume call. Called by resume() after any
    // pre-resume compaction has run (or been declined).
    void doResume();
    void showNextPermission();
    void answerPermission(bool allow);
    void buildQuestionForm(const QJsonObject &req);
    void onQuestionSubmit();
    void refresh();

    // The conversation feed.
    void appendToFeed(QWidget *entry);
    // addMessageCard renders one role-tagged card. `plainText` is the raw
    // (Markdown / plain) source kept for copy + search; pass empty when none.
    // `replayed` cards (transcript restore) carry no live timestamp.
    void addMessageCard(const QString &role, const QString &accentHex,
                        const QString &bodyHtml, const QString &plainText = QString(),
                        bool replayed = false);
    void addNote(const QString &html, const QString &kind);
    void scrollFeedToBottom();

    // Jump-to-latest floating button: reposition over the feed viewport and
    // toggle visibility based on the sticky-bottom state.
    void positionJumpButton();
    void updateJumpButton();

    // Draft persistence (KConfig "Agent" group, draft-<id>): save on edit,
    // restore when (re)bound to a workspace/thread, clear on send.
    QString draftKey() const;
    void saveDraft();
    void restoreDraft();
    void clearDraft();

    // In-conversation find bar.
    void toggleFindBar();
    void runFind(int direction); // -1 prev, +1 next, 0 re-run from current
    void clearFindHighlights();

    CoreClient *m_core = nullptr;
    QString m_workspace;
    QString m_threadId;
    QString m_branch;
    bool m_isolated = false;
    bool m_idle = false;      // turn finished, awaiting a follow-up
    bool m_dormant = false;   // has a thread id, but no live process — resumable
    bool m_promoting = false; // a promote-to-worktree is in flight
    bool m_replaying = false; // inside loadTranscript() — don't double-count cost
    bool m_dragActive = false; // an acceptable drag is hovering the panel

    // Running per-session usage totals, accumulated from each `result` event's
    // top-level usage block. Surfaced as a compact suffix on the header
    // subtitle. Reset on a fresh start and on resume.
    double m_sessionCostUsd = 0.0;
    qlonglong m_sessionInTokens = 0;
    qlonglong m_sessionOutTokens = 0;

    QLabel *m_header = nullptr;
    QScrollArea *m_feedScroll = nullptr;
    QWidget *m_feed = nullptr;
    QVBoxLayout *m_feedLayout = nullptr;
    bool m_stickBottom = true; // auto-scroll until the user scrolls upward
    // Floating "jump to latest" button over the feed viewport, shown when the
    // feed is scrolled up away from the bottom.
    QToolButton *m_jumpBtn = nullptr;
    bool m_jumpUnread = false; // a card arrived while detached from the bottom
    QHash<QString, ToolCard *> m_toolCards; // keyed by tool_use id

    // Debounced draft autosave for the composer.
    QTimer *m_draftTimer = nullptr;

    // In-conversation find bar (hidden by default; toggled with Ctrl+F).
    QFrame *m_findBar = nullptr;
    QLineEdit *m_findEdit = nullptr;
    QLabel *m_findStatus = nullptr;
    // Registry of searchable message bodies: the body QLabel, its plain-text
    // source, and the original (un-highlighted) HTML to restore.
    struct Searchable {
        QLabel *body = nullptr;
        QString plain;
        QString html;
        QWidget *card = nullptr;
    };
    QList<Searchable> m_searchables;
    QList<int> m_findHits; // indices into m_searchables that currently match
    int m_findIndex = -1;
    WorkingIndicator *m_working = nullptr;
    QPlainTextEdit *m_input = nullptr;
    QComboBox *m_modeCombo = nullptr;
    QComboBox *m_isolationCombo = nullptr;
    QComboBox *m_effortCombo = nullptr;
    QComboBox *m_modelCombo = nullptr;
    // Compaction strategy + strip flag — controls how the thread's transcript
    // is condensed to keep resume cost down. Both are sticky to last used.
    QComboBox *m_compactCombo = nullptr;
    QCheckBox *m_compactStrip = nullptr;
    // On-demand compactor: pick any backend (Hot Opus on the live thread, or
    // cold Opus/Sonnet/Haiku/Local) without changing the scheduled strategy.
    QToolButton *m_compactNowBtn = nullptr;
    // "Compact now" submenu inside the Compaction popup. Held so refresh()
    // can disable it (and its Hot Opus entry) based on thread state.
    QMenu *m_compactNowMenu = nullptr;
    QPushButton *m_sendBtn = nullptr;
    QPushButton *m_stopBtn = nullptr;
    QPushButton *m_interruptBtn = nullptr;
    QPushButton *m_diffBtn = nullptr;
    QPushButton *m_attachBtn = nullptr;

    // Pending attachments for the next message (each {kind,name,mediaType,…}).
    QWidget *m_attachBar = nullptr;
    QHBoxLayout *m_attachLayout = nullptr;
    KMessageWidget *m_attachNotice = nullptr;
    QJsonArray m_attachments;

    // Per-tool approval banner and the queue of pending requests.
    QFrame *m_permBar = nullptr;
    QLabel *m_permLabel = nullptr;
    QPushButton *m_permAllow = nullptr;
    QPushButton *m_permDeny = nullptr;
    QList<QJsonObject> m_permQueue;

    // FIFO of follow-up messages typed while a turn was in progress. The
    // `claude` CLI buffers a second stdin user message until the current turn
    // ends, so we hold them here and fire one on each `result`. Mirrors the
    // m_permQueue pattern. Each chip in m_queueBar can be removed before it
    // fires.
    struct QueuedMsg {
        QString text;
        QJsonArray attachments;
    };
    QList<QueuedMsg> m_sendQueue;
    QFrame *m_queueBar = nullptr;
    QHBoxLayout *m_queueLayout = nullptr;

    // Promote-to-worktree bar, shown while a thread runs non-isolated.
    QFrame *m_promoteBar = nullptr;
    QPushButton *m_promoteBtn = nullptr;

    // AskUserQuestion form, built dynamically when the agent asks a question.
    QFrame *m_questionBox = nullptr;
    QVBoxLayout *m_questionLayout = nullptr;
    QList<QuestionField> m_questionFields;
    QJsonObject m_questionReq;
};
