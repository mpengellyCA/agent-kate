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
class QDragMoveEvent;
class QDropEvent;
class QEvent;
class QFrame;
class QHBoxLayout;
class QLabel;
class QMenu;
class QPlainTextEdit;
class QPushButton;
class QScrollArea;
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

Q_SIGNALS:
    void statusMessage(const QString &text);
    void titleChanged(const QString &title);
    void stateChanged(const QString &dotColorHex);
    // Human-readable one-line status (isolation / worktree branch / idle), used
    // as the roster card's subtitle. Tracks the same state as stateChanged.
    void subtitleChanged(const QString &text);
    void dormantChanged(bool dormant);
    void openDiff(const QString &title, const QString &diffText);

protected:
    bool eventFilter(QObject *obj, QEvent *event) override;
    void dragEnterEvent(QDragEnterEvent *event) override;
    void dragMoveEvent(QDragMoveEvent *event) override;
    void dropEvent(QDropEvent *event) override;

private:
    // One clarifying question currently shown to the human.
    struct QuestionField {
        QString question;
        bool multiSelect = false;
        QList<QPair<QString, QAbstractButton *>> options; // label -> button
    };

    void onSendClicked();
    void onStopClicked();
    void onChangesClicked();
    void onPromoteClicked();
    void onAttachClicked();
    void rebuildAttachChips();
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
    void addMessageCard(const QString &role, const QString &accentHex,
                        const QString &bodyHtml);
    void addNote(const QString &html, const QString &kind);
    void scrollFeedToBottom();

    CoreClient *m_core = nullptr;
    QString m_workspace;
    QString m_threadId;
    QString m_branch;
    bool m_isolated = false;
    bool m_idle = false;      // turn finished, awaiting a follow-up
    bool m_dormant = false;   // has a thread id, but no live process — resumable
    bool m_promoting = false; // a promote-to-worktree is in flight

    QLabel *m_header = nullptr;
    QScrollArea *m_feedScroll = nullptr;
    QWidget *m_feed = nullptr;
    QVBoxLayout *m_feedLayout = nullptr;
    bool m_stickBottom = true; // auto-scroll until the user scrolls upward
    QHash<QString, ToolCard *> m_toolCards; // keyed by tool_use id
    WorkingIndicator *m_working = nullptr;
    QPlainTextEdit *m_input = nullptr;
    QComboBox *m_modeCombo = nullptr;
    QComboBox *m_isolationCombo = nullptr;
    QComboBox *m_effortCombo = nullptr;
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
    QPushButton *m_diffBtn = nullptr;
    QPushButton *m_attachBtn = nullptr;

    // Pending attachments for the next message (each {kind,name,mediaType,…}).
    QWidget *m_attachBar = nullptr;
    QHBoxLayout *m_attachLayout = nullptr;
    QJsonArray m_attachments;

    // Per-tool approval banner and the queue of pending requests.
    QFrame *m_permBar = nullptr;
    QLabel *m_permLabel = nullptr;
    QPushButton *m_permAllow = nullptr;
    QPushButton *m_permDeny = nullptr;
    QList<QJsonObject> m_permQueue;

    // Promote-to-worktree bar, shown while a thread runs non-isolated.
    QFrame *m_promoteBar = nullptr;
    QPushButton *m_promoteBtn = nullptr;

    // AskUserQuestion form, built dynamically when the agent asks a question.
    QFrame *m_questionBox = nullptr;
    QVBoxLayout *m_questionLayout = nullptr;
    QList<QuestionField> m_questionFields;
    QJsonObject m_questionReq;
};
