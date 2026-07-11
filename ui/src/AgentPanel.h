#pragma once

#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QPair>
#include <QPersistentModelIndex>
#include <QPointer>
#include <QString>
#include <QWidget>

class CoreClient;
class TranscriptModel;
class TranscriptDelegate;
class WorkingIndicator;
class QListView;
class QModelIndex;
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
class FlowLayout;
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

    // The model tier / effort this agent runs on, read from its (frozen-once-
    // started) pickers. Used to prefill the Fork dialog from the source agent.
    QString currentModel() const;
    QString currentEffort() const;

    // Bind this fresh panel to a thread the core has ALREADY started (a fork):
    // adopt the running thread id and go live. The fork's own session id is
    // minted asynchronously (--fork-session), so the inherited conversation is
    // replayed from sourceThreadId — the agent it was forked from — which already
    // has the transcript on disk. Unlike setDormant, the process is running, so
    // there is no Resume step.
    void adoptRunningThread(const QString &threadId, const QString &sourceThreadId,
                            const QString &title, bool isolated);

    // Pre-pick the start model by its id ("opus", "sonnet", …) before the first
    // start. No-op once a thread exists (the combo is frozen then) or if the id
    // isn't an offered choice.
    void preselectModel(const QString &modelId);

    // Pre-pick the other start-time settings before the first start, for the
    // guided New Agent dialog. All no-ops once a thread exists.
    void preselectIsolation(const QString &isolation); // "auto" | "isolated" | "workspace"
    void preselectPermission(const QString &mode);     // permission-mode data value
    void preselectEffort(const QString &effort);       // "" | low | medium | high | xhigh | max
    // Pre-fill the composer with a first task (the user still presses Start/Send).
    void setComposerText(const QString &text);

    // Bind this panel to a persisted-but-not-running thread; resume() relaunches
    // it through the core's agent.resume.
    void setDormant(const QString &threadId, const QString &title, bool isolated);
    // resume() may first prompt the user to choose a compaction model when no
    // current summary is on disk, then dispatch to doResume() for the actual
    // agent.resume call.
    void resume();

    // Re-read chat preferences (send key, tool-card visibility) from KConfig.
    void applyChatSettings();

    // Re-read the configured API provider profiles (after the Providers settings
    // dialog closes) and rebuild the provider picker. No-op once a thread exists,
    // since the picker is frozen then.
    void reloadProviders();

    // Attach a set of local file paths as context for the next message. Used
    // by drag-and-drop from ProjectTree (and by the Attach… button).
    void attachPaths(const QStringList &paths);

    // Attach a custom-MIME payload of {path,line,endLine} items. Ranged items
    // become a text excerpt named "file:start-end"; whole-file items defer to
    // attachPaths. Used by drops carrying line ranges from the search results.
    void attachItems(const QJsonArray &items);

    // Has a thread id but no live process — resumable (vs isRunning()).
    bool isDormant() const { return m_dormant; }

    // Public equivalents of the composer-toolbar buttons, so the window's Agent
    // menu / command palette can drive the active panel.
    void stop() { onStopClicked(); }
    void promptAttach() { onAttachClicked(); }
    void showChanges() { onChangesClicked(); }

Q_SIGNALS:
    void statusMessage(const QString &text);
    void titleChanged(const QString &title);
    // Emitted when this panel's backing thread id is assigned or changes — a fresh
    // agent gets its id asynchronously when its session starts, after activation, so
    // consumers keyed on the thread (Cowork panel, Git Log) must refresh on this.
    void threadIdChanged(const QString &threadId);
    void stateChanged(const QString &dotColorHex);
    // Human-readable one-line status (isolation / worktree branch / idle), used
    // as the roster card's subtitle. Tracks the same state as stateChanged.
    void subtitleChanged(const QString &text);
    void dormantChanged(bool dormant);
    // Roster card affordance: attention = a turn is waiting on the user's input
    // (a permission prompt). The roster paints this as a card marker.
    void attentionChanged(bool attention);
    void openDiff(const QString &title, const QString &diffText);
    // A text/file attachment chip was clicked — ask the window to open the file
    // in the editor (and make the editor visible if it was chat-only).
    void openFileRequested(const QString &path);
    // Emitted after a "Stop & close" archives this agent on the core — asks the
    // dock to remove this panel and its roster entry (the terminal close path).
    void closeRequested();
    // The user picked "Fork…" from this panel's header — asks the dock to run
    // the fork flow for this agent (dialog → agent.fork → adopt the new thread).
    void forkRequested();

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
    // Replay a thread's persisted transcript into this feed. loadTranscript()
    // uses this panel's own thread; a fork passes its source thread so the
    // inherited conversation appears before the fork's own session id exists.
    void loadTranscriptFrom(const QString &fromThreadId);
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
    // Rebuild the model combo to match the selected provider: Claude tiers for
    // Claude-direct, or the provider's own model ids otherwise.
    void rebuildModelCombo();
    void showNextPermission();
    void answerPermission(bool allow);
    void buildQuestionForm(const QJsonObject &req);
    void onQuestionSubmit();
    void refresh();

    // The conversation feed — a virtualized model/view (plan 10 phase 2).
    // addMessageCard appends one role-tagged message row. `plainText` is the raw
    // (Markdown / plain) source kept for copy + search; pass empty when none.
    // `replayed` cards (transcript restore) carry no live timestamp.
    void addMessageCard(const QString &role, const QString &accentHex,
                        const QString &bodyHtml, const QString &plainText = QString(),
                        bool replayed = false, const QJsonArray &attachments = {});
    // Strip the heavy body (dataB64 / text) from a live attachment array, leaving
    // the compact chip metadata (name/kind/path/mediaType/outside) the feed row
    // and the delegate carry. Mirrors what the core sidecar persists for replay.
    static QJsonArray compactAttachments(const QJsonArray &attachments);
    // Open a clicked attachment chip: image → in-place preview dialog reusing
    // ImageView; text/file → openFileRequested so MainWindow shows it in the editor.
    void openAttachment(const QJsonObject &att);
    // Open the full-size tool-call inspector modal for a Tool row (plan 13
    // phase 5): tool-aware Overview, full input JSON, searchable result.
    void openToolInspector(const QModelIndex &idx);
    void addNote(const QString &html, const QString &kind);
    void scrollFeedToBottom();
    // Show the whole-message + code-block copy menu for a feed row.
    void showFeedContextMenu(const QModelIndex &idx, const QPoint &globalPos);

    // In-place selectable text overlay (plan 13 phase 1): a click on a message
    // body opens a persistent, frameless QTextBrowser over that row's text so an
    // arbitrary substring can be selected and Ctrl+C'd. Only one is open at a
    // time; it closes on Esc, on a click outside it, on that row's data changing,
    // and on model reset/eviction.
    void openSelectionOverlay(const QModelIndex &idx);
    void closeSelectionOverlay();
    // After an interactive resize settles, re-measure exactly the rows currently
    // visible in the view (the rest keep their cheap estimate until shown). This
    // is what keeps resize cost O(visible rows) — see TranscriptDelegate.
    void remeasureVisibleRows();

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
    // Attachment sidecar turns (name/kind/path/mediaType/outside per sent
    // message that had attachments), returned by agent.transcript and consumed in
    // order as the matching user messages are replayed so the You cards regain
    // their chips after a resume. Cleared once replay finishes.
    QJsonArray m_replayAttachTurns;
    bool m_dragActive = false; // an acceptable drag is hovering the panel

    // Running per-session usage totals, accumulated from each `result` event's
    // top-level usage block. Surfaced as a compact suffix on the header
    // subtitle. Reset on a fresh start and on resume.
    double m_sessionCostUsd = 0.0;
    qlonglong m_sessionInTokens = 0;
    qlonglong m_sessionOutTokens = 0;

    QLabel *m_header = nullptr;
    // Virtualized transcript: a QListView over a TranscriptModel, painted by a
    // TranscriptDelegate. Replaces the old QScrollArea + per-message-widget feed
    // so resize cost is O(visible rows) and memory stays flat for long chats.
    QListView *m_view = nullptr;
    TranscriptModel *m_model = nullptr;
    TranscriptDelegate *m_delegate = nullptr;
    bool m_stickBottom = true; // auto-scroll until the user scrolls upward
    // Floating "jump to latest" button over the feed viewport, shown when the
    // feed is scrolled up away from the bottom.
    QToolButton *m_jumpBtn = nullptr;
    bool m_jumpUnread = false; // a card arrived while detached from the bottom
    QHash<QString, int> m_toolRows; // tool_use id -> stable transcript key
    // The Message row whose selection overlay is currently open (invalid = none).
    // Persistent so it tracks the row across insertions/scroll.
    QPersistentModelIndex m_selectionRow;
    // Handle to the open overlay editor (the delegate hands it over via
    // editorCreated), so the panel can focus it and filter Esc / outside clicks.
    QPointer<QWidget> m_selectionEditor;
    // Coalesces interactive resize into a single exact re-measure of the visible
    // rows once the drag settles (~80ms), mirroring the ImageView/RichTextView
    // debounce from phase 1.
    QTimer *m_resizeSettle = nullptr;

    // Debounced draft autosave for the composer.
    QTimer *m_draftTimer = nullptr;

    // In-conversation find bar (hidden by default; toggled with Ctrl+F).
    QFrame *m_findBar = nullptr;
    QLineEdit *m_findEdit = nullptr;
    QLabel *m_findStatus = nullptr;
    QList<int> m_findHits; // model rows that currently match the needle
    int m_findIndex = -1;
    WorkingIndicator *m_working = nullptr;
    QPlainTextEdit *m_input = nullptr;
    QComboBox *m_modeCombo = nullptr;
    QComboBox *m_isolationCombo = nullptr;
    QComboBox *m_effortCombo = nullptr;
    QComboBox *m_providerCombo = nullptr; // third-party API provider (or Claude direct)
    QComboBox *m_modelCombo = nullptr;
    // Id of the provider this thread was started with, so a same-session resume
    // can re-attach a KWallet-held API token the core never persists. Empty for
    // Claude direct.
    QString m_startedProviderId;
    QCheckBox *m_coworkCheck = nullptr; // start this agent with the Cowork desktop tools wired in
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
    QPushButton *m_forkBtn = nullptr;
    QPushButton *m_attachBtn = nullptr;

    // Pending attachments for the next message (each {kind,name,mediaType,…}).
    QWidget *m_attachBar = nullptr;
    FlowLayout *m_attachLayout = nullptr;
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
    FlowLayout *m_queueLayout = nullptr;

    // Promote-to-worktree bar, shown while a thread runs non-isolated.
    QFrame *m_promoteBar = nullptr;
    QPushButton *m_promoteBtn = nullptr;

    // AskUserQuestion form, built dynamically when the agent asks a question.
    QFrame *m_questionBox = nullptr;
    QVBoxLayout *m_questionLayout = nullptr;
    QList<QuestionField> m_questionFields;
    QJsonObject m_questionReq;
};
