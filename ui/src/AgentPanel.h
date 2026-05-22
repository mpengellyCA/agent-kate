#pragma once

#include <QJsonArray>
#include <QJsonObject>
#include <QList>
#include <QPair>
#include <QString>
#include <QWidget>

class CoreClient;
class QAbstractButton;
class QComboBox;
class QFrame;
class QHBoxLayout;
class QLabel;
class QPlainTextEdit;
class QPushButton;
class QTextEdit;
class QVBoxLayout;

// AgentPanel drives one agent thread: it starts a headless `claude` via the
// core, streams the conversation, sends follow-ups, surfaces per-tool approval
// requests and AskUserQuestion forms, and can show the thread's diff. Many
// panels run side by side in an AgentDock; each routes events by its thread id.
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
    void resume();

Q_SIGNALS:
    void statusMessage(const QString &text);
    void titleChanged(const QString &title);
    void stateChanged(const QString &dotColorHex);
    void dormantChanged(bool dormant);
    void openDiff(const QString &title, const QString &diffText);

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
    void showNextPermission();
    void answerPermission(bool allow);
    void buildQuestionForm(const QJsonObject &req);
    void onQuestionSubmit();
    void append(const QString &html);
    void refresh();

    CoreClient *m_core = nullptr;
    QString m_workspace;
    QString m_threadId;
    QString m_branch;
    bool m_isolated = false;
    bool m_idle = false;      // turn finished, awaiting a follow-up
    bool m_dormant = false;   // has a thread id, but no live process — resumable
    bool m_promoting = false; // a promote-to-worktree is in flight

    QLabel *m_header = nullptr;
    QTextEdit *m_transcript = nullptr;
    QPlainTextEdit *m_input = nullptr;
    QComboBox *m_modeCombo = nullptr;
    QComboBox *m_isolationCombo = nullptr;
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
