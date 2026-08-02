#pragma once

#include "state/AgentJob.h"
#include "state/HarnessTraits.h"

#include <QDateTime>
#include <QHash>
#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QPair>
#include <QPersistentModelIndex>
#include <QPointer>
#include <QSet>
#include <QString>
#include <QWidget>

class CoreClient;
class TranscriptModel;
class TranscriptDelegate;
class WorkflowMonitor;
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
class QImage;
class QMimeData;
class QPaintEvent;
class QFrame;
class FlowLayout;
class QHBoxLayout;
class QLabel;
class QLineEdit;
class QMenu;
class QListWidget;
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
    // The thread id this panel's last non-empty job publish was keyed on. A
    // panel can lose or change its thread id (a "Stop & close" reply clears it
    // before the close; a failed start falls back to a blank panel), so reaping
    // its job rows has to target the id they were published under.
    QString publishedThreadId() const { return m_publishedThreadId; }
    bool isIsolated() const { return m_isolated; }
    // The agent's isolated worktree directory (the working dir a lifecycle
    // event / dormant record reports). Empty when the agent runs directly in
    // the workspace (non-isolated) — callers use this to scope the file
    // browser's Worktree tab.
    QString worktreePath() const { return m_isolated ? m_workdir : QString(); }
    // A live agent has a thread and a running process (not dormant/resumable).
    bool isRunning() const { return !m_threadId.isEmpty() && !m_dormant; }

    // The model tier / effort this agent runs on, read from its (frozen-once-
    // started) pickers. Used to prefill the Fork dialog from the source agent.
    QString currentModel() const;
    QString currentEffort() const;

    // The backend harness driving this thread: "" or "claude" = Claude Code,
    // "kimi" = Kimi Code. Empty until the thread is bound (start reply, fork
    // adoption or dormant restore).
    QString backend() const { return m_backend; }

    // The provider id this agent runs on (ProviderStore::directId() for
    // Claude-direct). Used to prefill the Fork dialog's live model list from the
    // source agent's provider catalogue.
    QString providerId() const { return selectedProviderId(); }

    // Bind this fresh panel to a thread the core has ALREADY started (a fork):
    // adopt the running thread id and go live. The fork's own session id is
    // minted asynchronously (--fork-session), so the inherited conversation is
    // replayed from sourceThreadId — the agent it was forked from — which already
    // has the transcript on disk. Unlike setDormant, the process is running, so
    // there is no Resume step.
    void adoptRunningThread(const QString &threadId, const QString &sourceThreadId,
                            const QString &title, bool isolated,
                            const QString &backend = QString());

    // Bind this fresh panel to a thread the core has already started that has
    // NO prior conversation to inherit — an ensemble controller (mode.apply
    // launched it with its briefing as the opening message). Same live
    // adoption, minus the transcript replay; note is shown as a system line.
    void adoptStartedThread(const QString &threadId, const QString &note, bool isolated,
                            const QString &backend = QString());

    // Pre-pick the engine (harness, direct API) before the first start. No-op
    // once a thread exists (the combo is frozen then) or if the id isn't an
    // offered choice.
    void preselectBackend(const QString &backend);
    // Pre-pick the full engine — harness plus optional provider overlay — for
    // the guided New Agent dialog. No-op once a thread exists.
    void preselectEngine(const QString &backend, const QString &providerId);

    // Pre-pick the start model by its id ("opus", "sonnet", …) before the first
    // start. No-op once a thread exists (the combo is frozen then) or if the id
    // isn't an offered choice.
    void preselectModel(const QString &modelId);

    // Pre-pick the other start-time settings before the first start, for the
    // guided New Agent dialog. All no-ops once a thread exists.
    void preselectIsolation(const QString &isolation); // "auto" | "isolated" | "workspace"
    void preselectPermission(const QString &mode);     // permission-mode data value
    void preselectEffort(const QString &effort);       // "" | low | medium | high | xhigh | max
    // Pre-set the plan 16 P6 launch options for the first start (fallback
    // models, a tool deny-list, extra reachable directories). They ride on
    // agent.start only — mid-session the CLI has already been launched — and
    // are only ever collected for an engine that declares the capability.
    // Rebuild the "Helpers" menu (this thread's subagent transcripts) from the
    // core; called each time the menu opens, since subagents appear over time.
    void refreshSubagentMenu(QMenu *menu);
    void preselectLaunchOptions(const QStringList &fallbackModels,
                                const QStringList &disallowedTools,
                                const QStringList &addDirs,
                                bool strictMcpConfig = false,
                                double maxBudgetUsd = 0.0);
    // Pre-fill the composer with a first task (the user still presses Start/Send).
    void setComposerText(const QString &text);

    // Bind this panel to a persisted-but-not-running thread; resume() relaunches
    // it through the core's agent.resume.
    void setDormant(const QString &threadId, const QString &title, bool isolated,
                    const QString &backend = QString());
    // resume() may first prompt the user to choose a compaction model when no
    // current summary is on disk, then dispatch to doResume() for the actual
    // agent.resume call.
    void resume();

    // Re-read chat preferences (send key, tool-card visibility) from KConfig.
    void applyChatSettings();

    // Show the thread's desktop-access state without acting on it — used when
    // binding to an existing thread and when the core reports a change made
    // somewhere else (the Cowork panel, or an agent's approved request).
    void setCoworkChecked(bool on);

    // Re-read the configured API provider profiles (after the Providers settings
    // dialog closes) and rebuild the engine picker's provider entries. No-op once
    // a thread exists, since the picker is frozen then.
    void reloadProviders();

    // Attach a set of local file paths as context for the next message. Used
    // by drag-and-drop from ProjectTree (and by the Attach… button).
    void attachPaths(const QStringList &paths);

    // Attach a custom-MIME payload of {path,line,endLine} items. Ranged items
    // become a text excerpt named "file:start-end"; whole-file items defer to
    // attachPaths. Used by drops carrying line ranges from the search results.
    void attachItems(const QJsonArray &items);

    // Attach raw images — pixels with no file behind them: a clipboard paste
    // (Ctrl+V after a Spectacle capture) or a drag out of a browser. They are
    // encoded to PNG and stored durably — that copy is the only copy there is.
    void attachImages(const QList<QImage> &images);

    // Has a thread id but no live process — resumable (vs isRunning()).
    bool isDormant() const { return m_dormant; }

    // Public equivalents of the composer-toolbar buttons, so the window's Agent
    // menu / command palette can drive the active panel.
    void stop() { onStopClicked(); }
    void promptAttach() { onAttachClicked(); }
    void showChanges() { onChangesClicked(); }

    // Jobs-panel entry points. That panel mirrors what this one publishes, so
    // acting on a job routes back here, where the state actually lives.
    void forgetFinishedJobs();
    void showWorkflowMonitor() { openWorkflowMonitor(); }
    // Re-publish this agent's jobs — used when a consumer attaches after the
    // work started, so it doesn't sit empty until the next task event. Forces
    // the emit: the publish path is otherwise change-gated, and a fresh
    // consumer has seen nothing at all.
    void republishJobs();

    // Draft persistence at teardown. The composer draft is written by a 400 ms
    // debounce timer (m_draftTimer), so a user who types and closes the agent
    // inside that window loses the text: the timer is a child of this panel and
    // dies with it, unfired. AgentDock calls flushDraft() on the KEEP teardown
    // path (plain Close, project close) to settle the debounce deterministically
    // before the panel goes, and dropPendingDraftWrite() on the FORGET path so a
    // still-pending timer cannot re-create the draft the dock has just cleared.
    //
    // Deliberately NOT done in ~AgentPanel: the destructor runs from
    // deleteLater, i.e. AFTER removeAgentEntry's DraftStore::clear, so saving
    // there would resurrect the draft of a genuinely destroyed thread.
    void flushDraft();
    void dropPendingDraftWrite();

Q_SIGNALS:
    void statusMessage(const QString &text);
    void titleChanged(const QString &title);
    // Emitted when this panel's backing thread id is assigned or changes — a fresh
    // agent gets its id asynchronously when its session starts, after activation, so
    // consumers keyed on the thread (Cowork panel, Git Log) must refresh on this.
    void threadIdChanged(const QString &threadId);
    // Emitted when this agent's isolated worktree path becomes known or changes
    // (started/resumed/promoted lifecycle, or a dormant restore). Carries the
    // effective worktree path — empty when the agent is non-isolated — so the
    // file browser can re-root its Worktree tab live (e.g. after a promote).
    void worktreePathChanged(const QString &worktreePath);
    // The card's status enum (AgentRoles::AgentStatus as int): the single source
    // of truth for the roster badge (symbol + semantic colour). Replaces the old
    // raw-hex dot colour.
    void statusChanged(int status);
    // Human-readable one-line status detail (isolation / worktree branch / cost
    // / tokens). Now the roster card's tooltip; the card body shows a preview.
    void subtitleChanged(const QString &text);
    // The latest chat line for the roster card preview ("You: …" for the user's
    // own messages). Emitted once per live append; during transcript replay it
    // fires once at the end with the final line. `activityEpoch` is the seconds-
    // since-epoch to stamp as the card's last-activity time; 0 means "stamp now"
    // (live messages) and a replay with no usable timestamp leaves it unstamped.
    void previewChanged(const QString &preview, qint64 activityEpoch = 0);
    void dormantChanged(bool dormant);
    // Roster card affordance: attention = a turn is waiting on the user's input
    // (a permission prompt). The roster paints this as a card marker.
    void attentionChanged(bool attention);
    // This agent's complete background-work set, re-published as a snapshot on
    // every change (a job starting, finishing, or gaining an output path). The
    // Jobs panel keys on threadId and replaces that agent's rows wholesale, so
    // it never has to reason about deltas — or hold a pointer into this panel.
    void jobsChanged(const QString &threadId, const QVector<agentkate::AgentJob> &jobs);
    // The tray's "N finished" chip: raise the Jobs panel, which is where
    // finished work (and every other agent's) now lives.
    void openJobsPanelRequested();
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
    // Take over a composer paste/drop that carries an image (pixels, or image
    // file URLs) and attach it instead of inserting text. Returns false for
    // everything else, which the composer then pastes exactly as before.
    bool handleComposerPaste(const QMimeData *source);
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
    // False = refused before any side effect (the frame guard); the caller still
    // owns the text and must not discard it.
    bool deliverMessage(const QString &text, const QJsonArray &attachments);
    // Refuse a request whose finalized params would overflow the core's
    // JSON-RPC frame cap (16 MB, core/internal/ipc/server.go). An oversize
    // frame never reaches a handler, so the message would just vanish; refusing
    // here keeps it in the composer where it can still be edited. Reports it in
    // the attach banner and returns true.
    bool wouldOverflowFrame(const QJsonObject &params);
    // Fire the next queued follow-up, if any, once the thread is idle. Called
    // on every `result` event; sends one message per turn boundary.
    void drainSendQueue();
    // Move still-queued follow-ups back into the composer when a turn stops or
    // fails, so the human's text is never silently discarded.
    void restoreQueuedToComposer();
    // restoreQueuedToComposer plus the opening prompt of a fresh start that
    // never reached an agent (audit F37). Use this at every failure site: the
    // opening prompt is committed to the feed before the start RPC, so without
    // it the human has to copy their own first message out of the transcript.
    void restoreUnsentToComposer();
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
    // --- claude stream channel (--include-partial-messages) ----------------
    // One `stream_event`: the raw Anthropic SSE envelope the CLI forwards.
    // Text deltas paint a provisional message row, thinking deltas drive the
    // working indicator; the authoritative `assistant` event that follows
    // REPLACES the provisional rows via takeStreamedTextKey().
    void renderStreamEvent(const QJsonObject &inner);
    // Push every pending block's accumulated text into its row. Called from a
    // short coalescing timer so a fast token stream costs one repaint per tick
    // per row rather than one per token.
    void flushStreamedText();
    // Render one finished text block's accumulated Markdown into its row — the
    // single markdownToHtml call a streamed message pays, replacing the escaped
    // plain text the flush ticks painted. Takes the SSE content-block index
    // (m_streamBlocks' key); a no-op for an unknown one.
    void settleStreamBlock(int blockIndex);
    // The next provisional text row this turn opened, or -1 once they are all
    // claimed. The assistant branch consumes them in block order so streamed
    // text is overwritten in place instead of duplicated.
    int takeStreamedTextKey();
    // Drop all provisional-row state (turn ended, interrupted, thread rebound).
    void resetStreamState();
    // --- forwarded subagent text (--forward-subagent-text) -----------------
    // Text an event carries under a parent_tool_use_id belongs to a helper, not
    // to this agent: it streams into the Task tool row that launched it, which
    // is where the subagent's work already lives in the feed. Returns true when
    // the event was consumed and must not render as the agent's own message.
    bool routeSubagentText(const QJsonObject &ev, const QString &parentToolUseId);
    // Paint every helper whose forwarded text changed since the last tick. Runs
    // off the same 50ms coalescer the agent's own text deltas use — a helper
    // streams just as fast, and repainting its row per token cost one model
    // mutation (and one row re-layout) per token.
    void flushSubagentText();
    // --- system-event subtypes ---------------------------------------------
    // Dispatch one `system` event that is not init and not a task-lifecycle
    // report. Every subtype is shown, folded into state, or deliberately
    // silent — there is no "unhandled" outcome, hence no return value.
    void renderSystemSubtype(const QString &subtype, const QJsonObject &ev);
    // Replace the composer's autocomplete feed. Shared by the init event and by
    // the `commands_changed` system event, which carries the same array.
    void seedSlashCommands(const QJsonArray &commands);
    // Point the model picker at what the CLI says it is now running (a fallback
    // switched models under us). Never pushes the change back to the CLI.
    void adoptModel(const QString &modelId);
    // Fold one rate_limit_event into the header chip, noting only TRANSITIONS
    // in the feed — the CLI emits one of these every turn.
    void applyRateLimit(const QJsonObject &info);
    // Withdraw this agent's usage-limit claim — both the account-level one the
    // roster strip aggregates and this panel's own header chip (audit F43).
    //
    // A rate_limit_event is a fact about a RUNNING turn, and it was only ever
    // withdrawn in the destructor: an agent that was stopped, interrupted, went
    // dormant or died mid-turn went on being counted as "paused by a usage
    // limit" for as long as its panel stayed open, which is a status that
    // outlives its condition — the same class of falsehood as a limit that
    // survives its own reset time.
    void clearRateLimitClaim();
    // Drive the mode/model/thinking pickers to the values a kimi `_options`
    // event reports, for each id it lists as changed.
    void adoptDiscoveredOptions(const QJsonArray &configOptions,
                                const QJsonArray &changed);
    void onPermissionRequested(const QJsonObject &params);
    // Desktop access, toggled while the agent exists: the core switches the
    // thread's Cowork tools on or off in place (or re-attaches the session on an
    // engine that cannot reveal tools live) and raises the OS permission dialog.
    // Before the thread exists this is just a start-time choice, read at launch.
    void onCoworkToggled(bool on);
    // Read the thread's current desktop-access state from the core into the
    // checkbox. Cheap; called when a thread is bound.
    void syncCoworkFromCore();
    // Pull the persisted Claude Code transcript and replay it into the feed so
    // a reopened dormant thread shows its prior conversation.
    void loadTranscript();
    // Replay a thread's persisted transcript into this feed. loadTranscript()
    // uses this panel's own thread; a fork passes its source thread so the
    // inherited conversation appears before the fork's own session id exists.
    void loadTranscriptFrom(const QString &fromThreadId);
    // Shared state flip behind both adoptions of an already-started thread.
    void bindStartedThread(const QString &threadId, bool isolated, const QString &backend);
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
    // Open the pending request's complete, unabridged input in a read-only
    // scrollable view. The bar's one-line summary is clipped (and, for Bash,
    // clipped in the middle) — this is the only way to read the rest before
    // deciding (audit F28).
    void showPermissionDetails();
    void buildQuestionForm(const QJsonObject &req);
    void onQuestionSubmit();
    // Count the visible permission prompt down to the core broker's deadline
    // (advertised as timeoutSeconds on permission.requested); when it passes,
    // drop the dead prompt — the core has already answered "no".
    void startPermCountdown(const QJsonObject &req);
    void tickPermCountdown();
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
    // Background work tray (plan 14 P4). The claude CLI reports its
    // background tasks — run_in_background shells and async subagents — as
    // system events (task_started / task_updated / task_notification /
    // background_tasks_changed, verified against claude 2.1.220). Each task
    // gets a chip: running "⚙ description", done "✓ description"; clicking
    // opens the task's output file (a live subagent transcript viewer for
    // agent tasks, the editor for shell output).
    void handleTaskEvent(const QString &subtype, const QJsonObject &ev);
    void openBackgroundJob(const QString &taskId);
    void updateJobsBar();
    // Emit jobsChanged when the snapshot — or the thread it belongs to —
    // actually changed. A panel whose thread id moved first reaps the rows it
    // published under the old one, or the Jobs panel keeps a duplicate group
    // for a thread that no longer exists, permanently.
    void publishJobs(QVector<agentkate::AgentJob> jobs);
    // Drop tool_use → task_id entries whose task record is gone. The map is
    // only consulted while that task's launch result is still arriving, so an
    // entry outliving its job is pure growth.
    void dropTaskMappings(const QSet<QString> &taskIds);
    // Remember the most recent `Workflow` tool launch on this thread (its input +
    // launch result), spin up a WorkflowMonitor for the chip's live label, and
    // reveal the "Workflow" chip. Called when a Workflow tool_result lands.
    void noteWorkflowLaunch(const QString &inputJson, const QString &resultText);
    // Refresh the "Workflow" chip label/visibility from the monitor's state.
    void updateWorkflowChip();
    // Open the dedicated WorkflowMonitorDialog for the remembered workflow.
    void openWorkflowMonitor();
    // The traits driving this panel's affordances: the bound thread's harness,
    // or (before a thread exists) the engine picker's selection. Every
    // backend-specific decision binds to these — never to an id compare.
    HarnessTraits currentTraits() const;
    // The engine picker's selection, split into its two axes. A bound thread
    // answers from its own state (m_backend / the started provider).
    QString selectedHarnessId() const;
    QString selectedProviderId() const;
    // Repopulate the engine picker (harnesses × provider overlays), restoring
    // the sticky choice (with one-time migration from the legacy backend +
    // provider keys).
    void rebuildEngineCombo();
    // Slash-command autocomplete (plan 14 P3): typing "/" as the first
    // character of the composer opens a popup listing the harness's commands
    // (claude: the init event's slash_commands; kimi: ACP
    // available_commands_update via the _commands event). Enter/Tab inserts,
    // Esc dismisses, Up/Down navigate.
    void updateSlashPopup();
    void acceptSlashCompletion();
    void hideSlashPopup();
    // Repopulate the "when to ask" / thinking-effort combos for the selected
    // backend (Claude's fixed vocabularies vs kimi's discovered config
    // options), restoring the per-backend sticky choice.
    void rebuildModeCombo();
    void rebuildEffortCombo();
    // Grey out the thinking-effort tiers the selected model cannot run, from
    // the per-model effort support the engine reported with its model catalogue.
    void applyModelEffortSupport();
    // The header's "ctx N%" tooltip: total fill plus, when the engine reported
    // one, the per-category breakdown of where the window went.
    QString contextTooltip() const;
    // Apply a model / effort / permission-mode change to the RUNNING agent via
    // agent.setOption (mid-session); a no-op before start or while dormant —
    // those apply at the next (re)start through the normal launch params.
    void maybePushOption(const QString &option, const QString &value);
    void addNote(const QString &html, const QString &kind);
    // Append a collapsed thinking card for a reasoning block (both harnesses
    // emit the same thinking-block shape; kimi via the translator).
    void addThinkingCard(const QString &thought);
    // Fold a TodoWrite tool_use into the plan checklist card: the existing
    // card updates in place; a fresh one is appended if none exists (or it
    // was evicted).
    void updateChecklistCard(const QJsonArray &todos);
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

    // Show/hide (and re-word) the empty-feed hint (audit F44). Public-ish only
    // to the panel: driven by the model's row signals, the viewport resize and
    // a chat-settings change, because all three can change what it should say.
    void updateFeedEmptyState();

    // Draft persistence (KConfig "Agent" group, draft-<id>): save on edit,
    // restore when (re)bound to a workspace/thread, clear on send.
    QString draftKey() const;
    void saveDraft();
    void restoreDraft();
    void clearDraft();

    // Composer history (audit F50): Up on the first line walks back through the
    // messages sent this session, Down walks forward and hands back the draft
    // the walk interrupted. Session-only, never persisted.
    void rememberSent(const QString &text);
    void setComposerFromHistory(const QString &text);

    // In-conversation find bar.
    void toggleFindBar();
    void runFind(int direction); // -1 prev, +1 next, 0 re-run from current
    void clearFindHighlights();

    CoreClient *m_core = nullptr;
    QString m_workspace;
    QString m_threadId;
    QString m_branch;
    // The working directory the agent's process runs in, as reported by
    // lifecycle events / the dormant record. When m_isolated this is the
    // isolated worktree; otherwise it mirrors the workspace.
    QString m_workdir;
    bool m_isolated = false;
    bool m_idle = false;      // turn finished, awaiting a follow-up
    bool m_dormant = false;   // has a thread id, but no live process — resumable
    bool m_promoting = false; // a promote-to-worktree is in flight
    bool m_replaying = false; // inside loadTranscript() — don't double-count cost
    bool m_errored = false;   // the last start/turn failed — card shows Error
    // During replay we accumulate the final preview line + its event timestamp
    // and emit a single previewChanged at the end, so a dormant agent's card
    // isn't repainted N times nor re-stamped "just now" for historical lines.
    QString m_replayLastPreview;
    qint64 m_replayEventEpoch = 0; // timestamp of the event currently rendering
    qint64 m_replayLastEpoch = 0;  // epoch paired with m_replayLastPreview
    // Attachment sidecar turns (name/kind/path/mediaType/outside per sent
    // message that had attachments), returned by agent.transcript and consumed in
    // order as the matching user messages are replayed so the You cards regain
    // their chips after a resume. Cleared once replay finishes.
    QJsonArray m_replayAttachTurns;
    bool m_dragActive = false; // an acceptable drag is hovering the panel
    // Reasons the attach builders refused something during the CURRENT paste or
    // drop. Reset by that handler, added to by every attach* helper it calls, so
    // "nothing was added" can be told apart from "nothing was offered" — a paste
    // of a deleted file's URL adds nothing and must still say so.
    int m_attachSkipped = 0;

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
    // The "nothing here yet" hint over an empty feed (audit F44). A child of the
    // view's viewport, like the roster's — never a transcript row, so a replayed
    // conversation cannot inherit it.
    QLabel *m_feedEmptyHint = nullptr;
    bool m_stickBottom = true; // auto-scroll until the user scrolls upward
    // Floating "jump to latest" button over the feed viewport, shown when the
    // feed is scrolled up away from the bottom.
    QToolButton *m_jumpBtn = nullptr;
    bool m_jumpUnread = false; // a card arrived while detached from the bottom
    QHash<QString, int> m_toolRows; // tool_use id -> stable transcript key
    // --- token-by-token streaming state ------------------------------------
    // One in-flight content block of the message currently streaming. Text
    // blocks own a provisional transcript row; a thinking block owns none (its
    // deltas drive the working indicator) and so keeps key == -1.
    struct StreamBlock {
        int key = -1;      // provisional message row, -1 for a non-text block
        QString text;      // accumulated deltas
        bool dirty = false; // text changed since the last flush
        bool thinking = false;
        // The row holds the real Markdown render, not the escaped plain text
        // the flush ticks paint. Set by settleStreamBlock().
        bool settled = false;
    };
    // Keyed by the SSE content-block index, which is unique within the message
    // being streamed; message_start clears the map, so indices never collide
    // across messages.
    QHash<int, StreamBlock> m_streamBlocks;
    // Provisional text-row keys in block order, and how many the authoritative
    // `assistant` event has claimed so far.
    QList<int> m_streamTextKeys;
    int m_streamClaimed = 0;
    QTimer *m_streamFlush = nullptr; // coalesces delta repaints
    // Latest thinking text of the current block, shown on the working line.
    QString m_streamThinking;
    // --- forwarded subagent text -------------------------------------------
    // One helper's forwarded output, accumulated so its Task tool row can show
    // it growing. Bounded WITHOUT re-trimming per delta: the accumulation is
    // allowed to run to twice the shown cap before it is cut back, so a token
    // costs an append, not two copies of the whole tail.
    struct SubagentText {
        QString text;         // the tail kept in RAM (see kSubagentTrimAt)
        bool trimmed = false; // earlier output was dropped → shown with a leading "…"
        bool dirty = false;   // has unpainted text; painted by the flush tick
        int rowKey = -1;      // the Task tool row it paints into (-1 = none visible)
    };
    // parent_tool_use_id -> that helper's forwarded text.
    QHash<QString, SubagentText> m_subagent;
    // --- rate limit readout -------------------------------------------------
    QString m_rateLimitStatus;   // "allowed" / "allowed_warning" / "rejected" / …
    QString m_rateLimitType;     // e.g. "five_hour"
    QString m_rateLimitResets;   // pre-formatted local time, empty when unknown
    // The same reset as DATA. The formatted copy above is for this panel's own
    // chip; the timestamp is what leaves the widget — plan 28 §Phase 2 arms a
    // wake timer on it, and a scheduler cannot parse "3:07 PM" back into one
    // (audit F43).
    QDateTime m_rateLimitResetsAt;
    bool m_rateLimitOverage = false;
    // Stable key of the plan checklist card (-1 = none yet). Each TodoWrite /
    // ACP plan update rewrites this one card in place, so the feed carries the
    // current plan rather than a trail of stale copies.
    int m_checklistKey = -1;
    // One harness slash command feeding the composer's autocomplete. `hint` is
    // the argument hint kimi reports ("<branch>"); claude's init event lists
    // names only, so both other fields are empty there.
    struct SlashCommand {
        QString name;
        QString description;
        QString hint;
    };
    QList<SlashCommand> m_slashCommands;
    QListWidget *m_slashPopup = nullptr;
    // One background task (shell or async subagent) reported by the CLI. The
    // tray's chips are rebuilt from this map rather than stored on it: chips now
    // come and go as jobs finish, and a retained pointer to a deleted one is a
    // crash waiting to happen.
    struct BgJob {
        QString id;          // CLI task_id
        QString description;
        QString taskType;    // "local_bash" | agent kinds
        QString outputFile;  // parsed from the tool result / task_notification
        qint64 startedMs = 0;
        qint64 endedMs = 0;  // stamped on the first terminal report; 0 = running
        bool done = false;
        bool failed = false; // done with a non-"completed" status, or killed with the agent
        bool noted = false;  // terminal summary already added to the feed (emit once)
        quint64 order = 0;   // insertion sequence — QHash iteration is unordered
    };
    QHash<QString, BgJob> m_bgJobs;         // by task_id
    quint64 m_bgJobSeq = 0;
    // Retention for finished job records. They are no longer drawn in the tray,
    // but the Jobs panel lists them, so they outlive their chips — bounded here
    // so a marathon session doesn't accumulate them forever.
    static constexpr int kMaxRetainedJobs = 500;
    QHash<QString, QString> m_taskByToolUse; // tool_use_id -> task_id
    QFrame *m_jobsBar = nullptr;
    FlowLayout *m_jobsFlow = nullptr;
    // Live chips by task id, and a fingerprint of what the tray currently shows
    // (running ids + their output paths, the finished count). The 15 s tick only
    // moves a minute-granular elapsed suffix, so rebuilding the widgets each
    // time would flicker the row under the composer for nothing: an unchanged
    // fingerprint relabels in place instead. It covers the CHIPS and nothing
    // else — publishing compares m_lastPublishedJobs.
    QHash<QString, QPushButton *> m_jobChips;
    QString m_jobsFingerprint;
    QTimer *m_jobsTimer = nullptr; // refreshes running chips' elapsed suffix
    // What the last jobsChanged carried, and the thread id it was keyed on.
    // Publishing is compared against these rather than against the tray's
    // fingerprint: the fingerprint answers "do the CHIPS need rebuilding", and
    // deliberately ignores everything no chip draws (a finished job's late
    // output path, a failure, a record dropped by "Clear finished").
    QVector<agentkate::AgentJob> m_lastPublishedJobs;
    QString m_publishedThreadId;
    // Observability (plan 14 P5). Context fill: the latest turn's prompt-side
    // tokens vs the model's context window (from modelUsage) — the number
    // that predicts auto-compaction. Turn timing: running average of the
    // CLI-reported per-turn wall times, shown on the working indicator.
    qlonglong m_ctxPromptTokens = 0;
    qlonglong m_ctxWindow = 0;
    // m_ctxExact: the fill above came from the engine's own context accounting
    // (a `_context` event) rather than the result-event estimate, so the
    // estimate must stop overwriting it. m_ctxBreakdown is that reading's
    // per-category split ([{label, tokens}]), shown in the header tooltip.
    bool m_ctxExact = false;
    QJsonArray m_ctxBreakdown;
    qlonglong m_turnDurTotalMs = 0;
    int m_turnDurCount = 0;
    // Permission countdown: the broker denies after its timeout; the bar
    // counts down to that deadline and expires the prompt in step with it.
    QTimer *m_permTimer = nullptr;
    QString m_permBaseHtml;
    qint64 m_permDeadlineMs = 0;

    // Background-Workflow tracking. A `Workflow` tool_use is recorded by its
    // transcript key (with its input JSON) so the paired tool_result — which
    // carries the run's Task ID / Transcript dir / Run ID — can be captured as the
    // thread's latest followable workflow. The chip + monitor surface its status.
    QSet<int> m_workflowToolKeys;
    QHash<int, QString> m_workflowInputByKey;
    QString m_workflowInput;
    QString m_workflowResult;
    WorkflowMonitor *m_workflowMonitor = nullptr; // drives the chip label (running/done)
    // When the workflow run was launched — the monitor reads the run's on-disk
    // artifacts, which carry no start time, so the job row's Elapsed would
    // otherwise be blank for the longest-running job on the panel.
    qint64 m_workflowStartedMs = 0;
    // When the monitor was FIRST seen in a terminal state. The artifacts carry
    // no finish time either, so this is the only end stamp the row can have;
    // observed rather than reported, hence latched once.
    qint64 m_workflowEndedMs = 0;
    // "Clear finished" suppressing a terminal workflow row. The row is
    // synthesized from the monitor on every publish rather than stored in
    // m_bgJobs, so there is no record to erase — only a flag, cleared by the
    // next launch.
    bool m_workflowForgotten = false;
    QFrame *m_workflowBar = nullptr;
    QPushButton *m_workflowChip = nullptr;
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

    // Composer history ring: messages sent this session, oldest first. -1 means
    // "not walking"; while walking, m_historyDraft holds the text the walk
    // interrupted so Down can put it back. m_historyNavigating tells the
    // textChanged handler that a write is ours, not the human's.
    QStringList m_composerHistory;
    int m_historyIndex = -1;
    QString m_historyDraft;
    bool m_historyNavigating = false;

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
    // One "who runs this agent" picker: each entry is a harness, optionally
    // overlaid with a third-party API provider (data "harness" or
    // "harness|providerId"). Replaces the former backend + provider combos.
    QComboBox *m_engineCombo = nullptr;
    QComboBox *m_modelCombo = nullptr;
    // Backend of the bound thread ("" or "claude" = Claude Code, "kimi" = Kimi
    // Code), taken from the agent.start reply or the dormant-thread record.
    QString m_backend;
    // Id of the provider this thread was started with, so a same-session resume
    // can re-attach a KWallet-held API token the core never persists. Empty for
    // Claude direct.
    QString m_startedProviderId;
    // The P6 launch options this panel starts with, set before the first start.
    QStringList m_fallbackModels;
    QStringList m_disallowedTools;
    QStringList m_addDirs;
    // The control-channel launch sweep: isolate from the human's global MCP
    // servers, and a CLI-enforced spend ceiling for the session (0 = uncapped).
    bool m_strictMcpConfig = false;
    double m_maxBudgetUsd = 0.0;
    QToolButton *m_subagentsBtn = nullptr; // "Helpers ▾" — subagent transcripts
    QCheckBox *m_coworkCheck = nullptr; // this agent's Cowork desktop tools (switchable mid-session)
    bool m_syncingCowork = false;       // guards the toggle handler while we mirror core state
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
    QPushButton *m_permDetails = nullptr; // full raw input (audit F28)
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
    // The opening prompt of a fresh start, held from the moment the composer is
    // cleared until `_lifecycle/started` proves the agent got it. A start that
    // fails hands it back instead of stranding it in the feed (audit F37).
    QueuedMsg m_pendingOpening;

    // Promote-to-worktree bar, shown while a thread runs non-isolated.
    QFrame *m_promoteBar = nullptr;
    QPushButton *m_promoteBtn = nullptr;

    // AskUserQuestion form, built dynamically when the agent asks a question.
    QFrame *m_questionBox = nullptr;
    QVBoxLayout *m_questionLayout = nullptr;
    QList<QuestionField> m_questionFields;
    QJsonObject m_questionReq;
};
