// The chat panel's two honest-labelling surfaces, exercised on the real widget
// rather than on the free functions behind it — which is the point.
//
// F44 (second pass): the empty-state hint tells a first-time user what will
// happen to their files when they send, and it reads the isolation picker to do
// it. The copy was already covered by a test on feedEmptyStateHtml(); what was
// NOT covered, and what was missing, is the WIRING — nothing connected the
// picker's currentIndexChanged to updateFeedEmptyState, so changing isolation
// left the previous promise on screen. A test on the string builder can never
// see that, so this one drives the combo.
//
// F30/F49 (convergence): this panel is the Ctrl+N path — the isolation picker
// people actually use — and it was still calling "auto" a private copy after
// the guided dialog had stopped. Its labels must be IsolationCopy's.
//
// F43: a rate-limited agent's claim is account-level and outlives the panel's
// attention span. It must be withdrawn when the agent stops being a running
// agent, not only when the panel is destroyed.

#include "AgentPanel.h"
#include "AgentCardDelegate.h"
#include "AgentChatHelpers.h"
#include "TranscriptDelegate.h"
#include "TranscriptModel.h"
#include "NewAgentDialog.h" // IsolationCopy — the shared isolation wording
#include "ipc/CoreClient.h"
#include "state/DraftStore.h"
#include "state/RateLimitState.h"
#include "state/ChatAppearance.h"

#include <QAbstractItemModel>
#include <QComboBox>
#include <QFrame>
#include <QJsonArray>
#include <QLabel>
#include <QListView>
#include <QListWidget>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QScrollBar>
#include <QStandardPaths>
#include <QTextBrowser>
#include <QToolButton>
#include <QtTest>

#include <KConfigGroup>
#include <KSharedConfig>

namespace {

// The "Where it works" picker: the only combo carrying all three isolation
// tokens (the permission picker also has an "auto", which is why all three).
QComboBox *isolationCombo(QWidget *panel)
{
    const auto combos = panel->findChildren<QComboBox *>();
    for (QComboBox *c : combos) {
        if (c->findData(QStringLiteral("auto")) >= 0
            && c->findData(QStringLiteral("isolated")) >= 0
            && c->findData(QStringLiteral("workspace")) >= 0) {
            return c;
        }
    }
    return nullptr;
}

// The empty-state hint over the transcript viewport, found by its content so
// the test is not coupled to the widget tree.
QLabel *emptyStateHint(QWidget *panel)
{
    const auto labels = panel->findChildren<QLabel *>();
    for (QLabel *l : labels) {
        if (l->text().contains(QStringLiteral("command palette"))) {
            return l;
        }
    }
    return nullptr;
}

// One event, wrapped in the "agent.event" notification shape the core sends,
// addressed to the thread these tests bind.
QJsonObject threadEvent(const QJsonObject &event)
{
    return QJsonObject{
        {QStringLiteral("threadId"), QStringLiteral("t-one")},
        {QStringLiteral("events"), QJsonArray{event}},
    };
}

// The conversation feed's model, found by content rather than by widget tree.
QAbstractItemModel *feedModel(QWidget *panel)
{
    const auto views = panel->findChildren<QListView *>();
    for (QListView *v : views) {
        if (v->model()) {
            return v->model();
        }
    }
    return nullptr;
}

QListView *feedView(QWidget *panel)
{
    const auto views = panel->findChildren<QListView *>();
    for (QListView *view : views) {
        if (view->model()) {
            return view;
        }
    }
    return nullptr;
}

QJsonObject assistantTextEvent(const QString &text)
{
    const QJsonObject block{{QStringLiteral("type"), QStringLiteral("text")},
                            {QStringLiteral("text"), text}};
    return QJsonObject{{QStringLiteral("type"), QStringLiteral("assistant")},
                       {QStringLiteral("message"),
                        QJsonObject{{QStringLiteral("content"), QJsonArray{block}}}}};
}

int feedRowCount(QWidget *panel)
{
    QAbstractItemModel *m = feedModel(panel);
    return m ? m->rowCount() : -1;
}

// The plain-text source of the last feed row (PlainRole is what copy + search
// read, so it is the honest "what does this row say").
QString lastFeedText(QWidget *panel)
{
    QAbstractItemModel *m = feedModel(panel);
    if (!m || m->rowCount() == 0) {
        return {};
    }
    const QModelIndex idx = m->index(m->rowCount() - 1, 0);
    const QString plain = m->data(idx, TranscriptModel::PlainRole).toString();
    return plain.isEmpty() ? m->data(idx, TranscriptModel::HtmlRole).toString() : plain;
}

agentkate::RateLimitReport limitedReport()
{
    agentkate::RateLimitReport r;
    r.status = QStringLiteral("rejected");
    r.rateLimitType = QStringLiteral("five_hour");
    r.resetsAt = QDateTime::currentDateTime().addSecs(3600);
    return r;
}

} // namespace

class AgentPanelTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void cleanup();

    void aParkedAgentStopsClaimingItIsWorking();
    void aSkippedResumeIsAnnouncedNotSwallowed();
    void isolationLabelsComeFromTheSharedCopy();
    void changingIsolationRepaintsTheEmptyState();
    void rebindingTheThreadWithdrawsItsUsageLimitClaim();
    void closingInsideTheAutosaveWindowKeepsTheDraft();
    void aDestroyedThreadsDraftIsNotWrittenBackByThePendingTimer();
    void dormantQuickAskPreservesWhitespaceDraftWhenSendFails();
    void toolRouterDiagnosticsNameTheirSourceAndTool();
    void composerGrowsToItsCapInsideTheComposerSurface();
    void slashPopupTracksComposerHeight();
    void detachedTranscriptMarksUnreadAndJumpsToLatest();
    void selectionOverlayUsesFreshGeometryAfterAppearanceChange();
};

void AgentPanelTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    // Nothing here may reach a notification server.
    agentkate::RateLimitState::setDesktopAlertsEnabled(false);
}

void AgentPanelTest::cleanup()
{
    agentkate::RateLimitState::self()->forget(QStringLiteral("t-one"));
    agentkate::RateLimitState::self()->forget(QStringLiteral("t-two"));
}

// Plan 28 §Phase 2 / audit F43's other half. An agent parked on the account's
// usage window is not working, and its card must stop saying so — the green
// "computing" arc over a thread that cannot spend a token until 14:37 is the
// symptom people actually reported. And once the core arms the resume, the
// panel says WHEN, because that is the entire difference between "stalled until
// someone comes back" and "resumes at 14:37".
//
// Driven through onNotification, the real wire: what was missing here has
// always been wiring, and a test on the private helpers would not see it.
void AgentPanelTest::aParkedAgentStopsClaimingItIsWorking()
{
    CoreClient core;
    AgentPanel panel(&core);
    auto *state = agentkate::RateLimitState::self();
    // A bound thread always has a workspace; without one the header is still in
    // its "open a folder to begin" state and no thread status applies.
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-ratewake-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("claude"));

    QSignalSpy status(&panel, &AgentPanel::statusChanged);
    QSignalSpy subtitle(&panel, &AgentPanel::subtitleChanged);

    // The engine reports the account's window as exhausted.
    const QDateTime resets = QDateTime::currentDateTimeUtc().addSecs(900);
    panel.onNotification(QStringLiteral("agent.event"), threadEvent(QJsonObject{
        {QStringLiteral("type"), QStringLiteral("rate_limit_event")},
        {QStringLiteral("rate_limit_info"), QJsonObject{
            {QStringLiteral("status"), QStringLiteral("rejected")},
            {QStringLiteral("rateLimitType"), QStringLiteral("five_hour")},
            {QStringLiteral("resetsAt"), double(resets.toSecsSinceEpoch())},
        }},
    }));

    QVERIFY(!status.isEmpty());
    QCOMPARE(status.constLast().at(0).toInt(),
             int(AgentRoles::AgentStatus::RateLimited));
    QCOMPARE(state->limitedCount(), 1);

    // ...and the core arms the resume.
    const QDateTime wakeAt = resets.addSecs(30);
    panel.onNotification(QStringLiteral("agent.event"), threadEvent(QJsonObject{
        {QStringLiteral("type"), QStringLiteral("_ratewake")},
        {QStringLiteral("state"), QStringLiteral("armed")},
        {QStringLiteral("at"), double(wakeAt.toSecsSinceEpoch())},
    }));

    QCOMPARE(status.constLast().at(0).toInt(),
             int(AgentRoles::AgentStatus::RateLimited));
    QVERIFY(state->resumeArmed(QStringLiteral("t-one")));
    const QString said = subtitle.constLast().at(0).toString();
    QVERIFY2(said.contains(QStringLiteral("resumes at")),
             qPrintable(QStringLiteral("the card does not say when it resumes: ") + said));
    QVERIFY2(!said.contains(QStringLiteral("Working")), qPrintable(said));
}

// The honesty half. A resume the core declined to perform — the thread's
// permission mode moved, the agent was closed, the key is in the wallet — has
// to be SAID. Being promised "resumes at 14:37" and finding an agent that never
// moved, with nothing in the conversation explaining why, is worse than never
// having been promised anything.
void AgentPanelTest::aSkippedResumeIsAnnouncedNotSwallowed()
{
    CoreClient core;
    AgentPanel panel(&core);
    auto *state = agentkate::RateLimitState::self();
    // A bound thread always has a workspace; without one the header is still in
    // its "open a folder to begin" state and no thread status applies.
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-ratewake-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("claude"));

    const QDateTime wakeAt = QDateTime::currentDateTimeUtc().addSecs(900);
    panel.onNotification(QStringLiteral("agent.event"), threadEvent(QJsonObject{
        {QStringLiteral("type"), QStringLiteral("_ratewake")},
        {QStringLiteral("state"), QStringLiteral("armed")},
        {QStringLiteral("at"), double(wakeAt.toSecsSinceEpoch())},
    }));
    QVERIFY(state->resumeArmed(QStringLiteral("t-one")));
    const int before = feedRowCount(&panel);

    const QString why = QStringLiteral("its \"when to ask\" setting changed after "
                                       "the resume was scheduled");
    panel.onNotification(QStringLiteral("agent.event"), threadEvent(QJsonObject{
        {QStringLiteral("type"), QStringLiteral("_ratewake")},
        {QStringLiteral("state"), QStringLiteral("skipped")},
        {QStringLiteral("reason"), why},
    }));

    QVERIFY2(!state->resumeArmed(QStringLiteral("t-one")),
             "a resume that did not happen was still being promised");
    QCOMPARE(state->limitedCount(), 0);
    QVERIFY2(feedRowCount(&panel) > before,
             "the skipped resume left nothing in the conversation");
    QVERIFY2(lastFeedText(&panel).contains(QStringLiteral("when to ask")),
             qPrintable(QStringLiteral("the reason was swallowed: ")
                        + lastFeedText(&panel)));
}

// The convergence: this picker's words are the shared ones, so "auto" cannot go
// back to being called a private copy here while the other two are honest.
void AgentPanelTest::isolationLabelsComeFromTheSharedCopy()
{
    CoreClient core;
    AgentPanel panel(&core);
    QComboBox *isolation = isolationCombo(&panel);
    QVERIFY(isolation != nullptr);
    for (const char *mode : {"auto", "isolated", "workspace"}) {
        const QString id = QString::fromLatin1(mode);
        const int idx = isolation->findData(id);
        QVERIFY(idx >= 0);
        QCOMPARE(isolation->itemText(idx), IsolationCopy::modeLabel(id));
    }
    QCOMPARE(isolation->toolTip(), IsolationCopy::modeTooltip());
}

// The F44 wiring. The hint describes what sending will do to the user's files;
// change the picker and it must stop describing the old choice.
void AgentPanelTest::changingIsolationRepaintsTheEmptyState()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.resize(800, 600);

    QComboBox *isolation = isolationCombo(&panel);
    QVERIFY(isolation != nullptr);
    QLabel *hint = emptyStateHint(&panel);
    QVERIFY2(hint != nullptr, "the empty feed must carry its hint");

    isolation->setCurrentIndex(isolation->findData(QStringLiteral("workspace")));
    const QString workspaceText = hint->text();
    QVERIFY2(!workspaceText.contains(QStringLiteral("private copy")),
             "the hint promised a private copy to an agent set to work "
             "directly in the user's files");
    QVERIFY(workspaceText.contains(QStringLiteral("directly")));

    isolation->setCurrentIndex(isolation->findData(QStringLiteral("isolated")));
    const QString isolatedText = hint->text();
    QVERIFY2(isolatedText != workspaceText,
             "changing isolation left a stale promise on screen");
    QVERIFY(isolatedText.contains(QStringLiteral("private copy")));

    // And back: this is not a one-way latch.
    isolation->setCurrentIndex(isolation->findData(QStringLiteral("workspace")));
    QCOMPARE(hint->text(), workspaceText);
}

// F43. A usage-limit report is a fact about a RUNNING agent. When this panel
// stops being that agent — it goes dormant, or is rebound to another thread —
// the account-level claim must go with it, or the roster's "N agents paused by
// a usage limit" strip keeps counting a thread nobody is waiting on.
void AgentPanelTest::rebindingTheThreadWithdrawsItsUsageLimitClaim()
{
    CoreClient core;
    AgentPanel panel(&core);
    auto *state = agentkate::RateLimitState::self();

    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("claude"));
    state->report(QStringLiteral("t-one"), limitedReport());
    QCOMPARE(state->limitedCount(), 1);

    // The panel is now a different agent. The old thread is not parked on a
    // usage window — it is not running at all.
    panel.setDormant(QStringLiteral("t-two"), QStringLiteral("two"), false,
                     QStringLiteral("claude"));
    QCOMPARE(state->limitedCount(), 0);
}

// The draft's teardown contract, from the panel's side.
//
// The composer autosaves on a 400 ms debounce whose QTimer is a CHILD of this
// panel. "Type a sentence, hit Close" therefore destroys the timer before it
// fires and loses the text — the one case the feature exists for. flushDraft()
// settles the debounce, and AgentDock calls it on every KEEP teardown.
void AgentPanelTest::closingInsideTheAutosaveWindowKeepsTheDraft()
{
    const QString workspace = QStringLiteral("/tmp/agentkate-draft-flush-test");
    const QString key = DraftStore::workspaceKey(workspace);
    QVERIFY(!key.isEmpty());
    DraftStore::clear(key);

    {
        CoreClient core;
        AgentPanel panel(&core);
        panel.setWorkspace(workspace);
        QPlainTextEdit *composer = panel.findChild<QPlainTextEdit *>();
        QVERIFY(composer != nullptr);
        composer->setPlainText(QStringLiteral("half a thought, not yet sent"));
        // Deliberately do NOT spin the event loop: this is the close that lands
        // inside the debounce window.
        panel.flushDraft();
    }

    QCOMPARE(KSharedConfig::openConfig()
                 ->group(QStringLiteral("Agent"))
                 .readEntry(key, QString()),
             QStringLiteral("half a thought, not yet sent"));
    DraftStore::clear(key);
}

// The other side of the same contract. On a real discard the dock clears the
// stored draft and THEN lets the panel die (deleteLater), so a debounce still
// pending would write the text of a destroyed thread straight back into the
// config it was just purged from.
void AgentPanelTest::aDestroyedThreadsDraftIsNotWrittenBackByThePendingTimer()
{
    const QString workspace = QStringLiteral("/tmp/agentkate-draft-forget-test");
    const QString key = DraftStore::workspaceKey(workspace);
    DraftStore::clear(key);

    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(workspace);
    QPlainTextEdit *composer = panel.findChild<QPlainTextEdit *>();
    QVERIFY(composer != nullptr);
    composer->setPlainText(QStringLiteral("words of a thread being discarded"));

    // What AgentDock::removeAgentEntry does on DraftDisposition::Forget, in
    // order: silence the pending write, then clear.
    panel.dropPendingDraftWrite();
    DraftStore::clear(key);

    // Give any surviving 400 ms timer more than its interval to fire.
    QTest::qWait(600);
    QVERIFY2(KSharedConfig::openConfig()
                 ->group(QStringLiteral("Agent"))
                 .readEntry(key, QString())
                 .isEmpty(),
             "a pending autosave resurrected the draft of a destroyed thread");
}

// Plan 27 §3: Quick Ask must not turn a dormant agent's automatic-resume
// behaviour into a draft shredder. A disconnected core is refused BEFORE it
// starts resuming, leaving the quick ask in its dialog for retry and preserving
// the exact whitespace-bearing dormant draft in the composer.
void AgentPanelTest::dormantQuickAskPreservesWhitespaceDraftWhenSendFails()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-quick-ask-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     // Kimi is hot-only, so resume() goes straight to its
                     // asynchronous request rather than opening Claude's
                     // cold-summary recovery chooser in this headless test.
                     QStringLiteral("kimi"));

    QPlainTextEdit *composer = panel.findChild<QPlainTextEdit *>();
    QVERIFY(composer != nullptr);
    const QString draft = QStringLiteral("  keep these spaces  ");
    composer->setPlainText(draft);
    QVERIFY(!panel.quickAsk(QStringLiteral("please check the tests")));
    QCOMPARE(composer->toPlainText(), draft);
}

// Phase 4: the input is allowed to grow with a real multi-line draft, but it
// has a hard seven-line cap so a long thought cannot collapse the transcript.
// This drives the actual QPlainTextEdit rather than a height helper: wrapping,
// the layout and the input's own scrollbar must agree.
void AgentPanelTest::composerGrowsToItsCapInsideTheComposerSurface()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.resize(640, 520);
    panel.show();
    QCoreApplication::processEvents();

    auto *composer = panel.findChild<QPlainTextEdit *>(QStringLiteral("composerInput"));
    auto *surface = panel.findChild<QFrame *>(QStringLiteral("composerContainer"));
    QVERIFY(composer != nullptr);
    QVERIFY(surface != nullptr);
    QCOMPARE(composer->parentWidget(), surface);
    QVERIFY(surface->findChild<QPushButton *>(QStringLiteral("composerSend")) != nullptr);

    const int initialHeight = composer->height();
    QStringList lines;
    for (int i = 0; i < 20; ++i) {
        lines << QStringLiteral("A readable line in a deliberately long draft.");
    }
    composer->setPlainText(lines.join(QLatin1Char('\n')));
    QCoreApplication::processEvents();
    QVERIFY(composer->height() > initialHeight);

    const int chrome = 2 * composer->frameWidth() + composer->contentsMargins().top()
        + composer->contentsMargins().bottom();
    const int cap = 7 * composer->fontMetrics().lineSpacing() + chrome;
    QVERIFY2(composer->height() <= cap,
             qPrintable(QStringLiteral("composer exceeded its seven-line cap: %1 > %2")
                            .arg(composer->height()).arg(cap)));
    QVERIFY(composer->verticalScrollBar()->maximum() > 0);
}

void AgentPanelTest::slashPopupTracksComposerHeight()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-slash-placement-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("kimi"));
    panel.resize(250, 520);
    panel.show();
    QCoreApplication::processEvents();

    const QString command(50, QLatin1Char('a'));
    panel.onNotification(QStringLiteral("agent.event"), threadEvent(QJsonObject{
        {QStringLiteral("type"), QStringLiteral("_commands")},
        {QStringLiteral("commands"), QJsonArray{QJsonObject{
            {QStringLiteral("name"), command},
            {QStringLiteral("description"), QStringLiteral("Long command")}}}},
    }));
    auto *composer = panel.findChild<QPlainTextEdit *>(QStringLiteral("composerInput"));
    auto *popup = panel.findChild<QListWidget *>();
    QVERIFY(composer != nullptr);
    QVERIFY(popup != nullptr);
    const int initialHeight = composer->height();

    composer->setPlainText(QStringLiteral("/") + command);
    QTRY_VERIFY(popup->isVisible());
    // The auto-grow cap itself is exercised above with real multi-line prose.
    // Slash completion permits no spaces or newlines, and QPlainTextEdit does
    // not wrap a single unbroken command token on every platform. Change the
    // input's real geometry here, then re-trigger popup placement through the
    // normal text-changed path.
    composer->setFixedHeight(initialHeight + composer->fontMetrics().lineSpacing());
    composer->setPlainText(QStringLiteral("/") + command.left(command.size() - 1));
    composer->setPlainText(QStringLiteral("/") + command);
    QTRY_COMPARE(composer->height(), initialHeight + composer->fontMetrics().lineSpacing());
    const QPoint composerTop = composer->mapTo(&panel, QPoint(0, 0));
    QCOMPARE(popup->width(), composer->width());
    QVERIFY2(popup->geometry().bottom() < composerTop.y(),
             "slash popup must stay above the composer after it grows");
}

void AgentPanelTest::detachedTranscriptMarksUnreadAndJumpsToLatest()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-unread-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("claude"));
    panel.resize(560, 420);
    panel.show();

    // Enough short real events to make the virtual feed scrollable.
    QJsonArray events;
    for (int i = 0; i < 40; ++i) {
        events.append(assistantTextEvent(QStringLiteral("A useful transcript line %1.").arg(i)));
    }
    panel.onNotification(QStringLiteral("agent.event"), QJsonObject{
        {QStringLiteral("threadId"), QStringLiteral("t-one")},
        {QStringLiteral("events"), events},
    });
    QCoreApplication::processEvents();

    auto *view = feedView(&panel);
    QVERIFY(view != nullptr);
    QScrollBar *bar = view->verticalScrollBar();
    QVERIFY(bar->maximum() > 48);
    bar->setValue(0);
    QCoreApplication::processEvents();

    panel.onNotification(QStringLiteral("agent.event"),
                         threadEvent(assistantTextEvent(QStringLiteral("Unread reply."))));
    QToolButton *jump = nullptr;
    for (QToolButton *candidate : panel.findChildren<QToolButton *>()) {
        if (candidate->accessibleName().startsWith(QStringLiteral("Jump to latest"))) {
            jump = candidate;
            break;
        }
    }
    QVERIFY(jump != nullptr);
    QTRY_VERIFY(jump->isVisible());
    QVERIFY(jump->accessibleName().contains(QStringLiteral("new messages")));
    QCOMPARE(jump->text(), QStringLiteral("•"));

    QTest::mouseClick(jump, Qt::LeftButton);
    QTRY_COMPARE(bar->value(), bar->maximum());
    QCOMPARE(jump->accessibleName(), QStringLiteral("Jump to latest"));
}

void AgentPanelTest::selectionOverlayUsesFreshGeometryAfterAppearanceChange()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-overlay-appearance-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("claude"));
    panel.resize(620, 460);
    panel.show();
    panel.onNotification(QStringLiteral("agent.event"),
                         threadEvent(assistantTextEvent(QStringLiteral(
                             "A selectable reply with enough prose to read."))));
    QCoreApplication::processEvents();

    auto *view = feedView(&panel);
    QVERIFY(view != nullptr);
    auto *delegate = qobject_cast<TranscriptDelegate *>(view->itemDelegate());
    QVERIFY(delegate != nullptr);
    QModelIndex index;
    for (int row = 0; row < view->model()->rowCount(); ++row) {
        const QModelIndex candidate = view->model()->index(row, 0);
        if (candidate.data(TranscriptModel::KindRole).toInt() == TranscriptModel::Message) {
            index = candidate;
            break;
        }
    }
    QVERIFY(index.isValid());
    QTRY_VERIFY(view->visualRect(index).isValid());
    QStyleOptionViewItem opt;
    opt.initFrom(view);
    opt.font = view->font();
    opt.rect = view->visualRect(index);
    const QRect body = delegate->messageBodyRect(opt.rect, opt, index);
    QVERIFY(body.isValid());
    QTest::mouseClick(view->viewport(), Qt::LeftButton, Qt::NoModifier, body.center());
    QTRY_VERIFY(!panel.findChildren<QTextBrowser *>().isEmpty());

    auto *appearance = ChatAppearance::instance();
    const auto oldDensity = appearance->density();
    const int oldScale = appearance->textScale();
    appearance->set(oldDensity == ChatAppearance::Density::Spacious
                        ? ChatAppearance::Density::Comfortable
                        : ChatAppearance::Density::Spacious,
                    oldScale == 1 ? 0 : 1, false);
    QTRY_VERIFY(panel.findChildren<QTextBrowser *>().isEmpty());

    opt.initFrom(view);
    opt.font = view->font();
    opt.rect = view->visualRect(index);
    const QRect freshBody = delegate->messageBodyRect(opt.rect, opt, index);
    QTest::mouseClick(view->viewport(), Qt::LeftButton, Qt::NoModifier, freshBody.center());
    QTRY_VERIFY(!panel.findChildren<QTextBrowser *>().isEmpty());
    const auto overlays = panel.findChildren<QTextBrowser *>();
    QCOMPARE(overlays.constFirst()->geometry(), freshBody);

    appearance->set(oldDensity, oldScale, false);
}

// Codex reports client-tool failures on its stderr tracing channel. This is a
// useful error, but it used to look like an unowned red note (often beside a
// Bash card) and leaked its ANSI colour bytes literally into the feed. The UI
// must preserve the reason while saying it came from the Codex tool router.
void AgentPanelTest::toolRouterDiagnosticsNameTheirSourceAndTool()
{
    CoreClient core;
    AgentPanel panel(&core);
    panel.setWorkspace(QStringLiteral("/tmp/agentkate-codex-diagnostic-test"));
    panel.setDormant(QStringLiteral("t-one"), QStringLiteral("one"), false,
                     QStringLiteral("codex"));

    const int before = feedRowCount(&panel);
    panel.onNotification(QStringLiteral("agent.event"), threadEvent(QJsonObject{
        {QStringLiteral("type"), QStringLiteral("_stderr")},
        {QStringLiteral("source"), QStringLiteral("Codex CLI")},
        {QStringLiteral("severity"), QStringLiteral("error")},
        {QStringLiteral("component"), QStringLiteral("codex_core::tools::router")},
        {QStringLiteral("tool"), QStringLiteral("apply_patch")},
        {QStringLiteral("text"), QStringLiteral("\x1b[31mverification failed: expected lines moved\x1b[0m")},
    }));

    QCOMPARE(feedRowCount(&panel), before + 1);
    const QString said = lastFeedText(&panel);
    QVERIFY2(said.contains(QStringLiteral("Tool call failed: apply_patch")),
             qPrintable(said));
    QVERIFY2(said.contains(QStringLiteral("Reported by Codex CLI")), qPrintable(said));
    QVERIFY2(said.contains(QStringLiteral("codex_core::tools::router")), qPrintable(said));
    QVERIFY2(said.contains(QStringLiteral("verification failed: expected lines moved")),
             qPrintable(said));
    QVERIFY2(!said.contains(QChar(0x1b)), qPrintable(said));
}

QTEST_MAIN(AgentPanelTest)
#include "AgentPanelTest.moc"
