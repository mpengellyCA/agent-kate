// Audit F49 / F30 — the guided New Agent dialog's isolation control.
//
// F49: the recommended default (the checkbox is on) mapped to a hard
// "isolated", and worktree.Create refuses that on a repo with no commit to
// branch from ("isolation needs at least one commit"), so starting an agent in
// a brand-new project failed with raw git-speak in the conversation. "auto"
// isolates where isolation is possible and degrades — visibly, the panel says
// "Working directly in your files" — where it is not.
//
// F30: the control must not call a git worktree a "sandbox". It isolates
// checkout state, not the process; docs/security-model.md says in bold that it
// is not a sandbox and does not pretend to be one. This dialog's wording is the
// canonical honest form the rest of the UI is aligned to, so it is pinned here.

// F30, second pass: removing the word "sandbox" did not remove the false
// promise — it moved into the checkbox label at the decision point ("Work in a
// private copy, so changes don't touch my files until I approve"), which is
// wrong twice: a worktree does not stop an agent writing absolute paths outside
// it, and the SAME round mapped that box to "auto", which degrades to the
// user's own files on a repo with nothing to branch from. The tests below pin
// the label making no promise, and the dialog resolving the degradation before
// it can be accepted.

// F12's class, reintroduced and removed again: the probe below used to run
// inside the constructor with waitForStarted(1000) + waitForFinished(1500), so
// opening New Agent froze the whole GUI for up to three seconds on a cold cache
// or a network mount. It is asynchronous now, and probeDoesNotBlockTheDialog
// pins that with a git that deliberately takes its time.

#include "NewAgentDialog.h"
#include "ProviderConfig.h"
#include "state/EngineAvailability.h"
#include "state/HarnessTraits.h"

#include <QCheckBox>
#include <QComboBox>
#include <QDir>
#include <QElapsedTimer>
#include <QFile>
#include <QLabel>
#include <QProcess>
#include <QStandardItemModel>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QtTest>

#include <optional>

namespace {
// The isolation checkbox is the dialog's only checkbox outside the Advanced
// section, and it is the one whose text names a private copy.
QCheckBox *isolationBox(QWidget *dlg)
{
    const auto boxes = dlg->findChildren<QCheckBox *>();
    for (QCheckBox *b : boxes) {
        if (b->text().contains(QStringLiteral("private copy"))) {
            return b;
        }
    }
    return nullptr;
}

// The sentence under the checkbox — the one that has to be true whatever the
// checkbox promises. Located by content: it is the only label carrying one of
// IsolationCopy's three notes.
QString isolationNoteText(QWidget *dlg)
{
    using IsolationCopy::isolationNote;
    const QStringList known{
        isolationNote(IsolationCopy::Availability::Unavailable, true),
        isolationNote(IsolationCopy::Availability::Available, true),
        isolationNote(IsolationCopy::Availability::Unknown, true),
        isolationNote(IsolationCopy::Availability::Unknown, false),
    };
    const auto labels = dlg->findChildren<QLabel *>();
    for (QLabel *l : labels) {
        if (known.contains(l->text())) {
            return l->text();
        }
    }
    return {};
}

// Drive the asynchronous probe to an answer. The production caller repaints
// from the event loop; a test has to spin it, which is the honest shape — a
// synchronous wrapper would let the blocking version pass again.
std::optional<IsolationCopy::Availability> probeAnswer(const QString &path,
                                                       int timeoutMs = 15000)
{
    QObject context;
    std::optional<IsolationCopy::Availability> got;
    IsolationCopy::probeIsolationAsync(
        path, &context, [&got](IsolationCopy::Availability a) { got = a; });
    QElapsedTimer clock;
    clock.start();
    while (!got.has_value() && clock.elapsed() < timeoutMs) {
        QCoreApplication::processEvents(QEventLoop::AllEvents, 20);
    }
    return got;
}

// The engine picker: the only combo in the dialog carrying harness ids.
QComboBox *engineCombo(QWidget *dlg)
{
    const auto combos = dlg->findChildren<QComboBox *>();
    for (QComboBox *c : combos) {
        if (c->findData(QStringLiteral("claude")) >= 0) {
            return c;
        }
    }
    return nullptr;
}

bool entryEnabled(const QComboBox *combo, int index)
{
    auto *model = qobject_cast<const QStandardItemModel *>(combo->model());
    const QStandardItem *item = model ? model->item(index) : nullptr;
    return item ? item->isEnabled() : true;
}

// Put a directory holding exactly `executables` at the front of $PATH. Each is
// a shell script running `body`.
void writeFakeExecutable(const QString &dir, const QString &name, const QString &body)
{
    QFile f(dir + QLatin1Char('/') + name);
    QVERIFY2(f.open(QIODevice::WriteOnly), qPrintable(f.errorString()));
    f.write(("#!/bin/sh\n" + body + "\n").toUtf8());
    f.close();
    QVERIFY(f.setPermissions(QFile::ReadOwner | QFile::WriteOwner | QFile::ExeOwner));
}

int runGit(const QString &dir, const QStringList &args)
{
    QProcess p;
    p.setWorkingDirectory(dir);
    p.start(QStringLiteral("git"), args);
    if (!p.waitForFinished(10000)) {
        return -1;
    }
    return p.exitCode();
}

// A git repository with no commit in it — the F49 project, and the one where
// "auto" hands the agent the user's own files.
bool initEmptyRepo(const QString &dir)
{
    return runGit(dir, {QStringLiteral("init"), QStringLiteral("-q")}) == 0
        && runGit(dir, {QStringLiteral("config"), QStringLiteral("user.email"),
                        QStringLiteral("test@agentkate")}) == 0
        && runGit(dir, {QStringLiteral("config"), QStringLiteral("user.name"),
                        QStringLiteral("Agent Kate Test")}) == 0;
}

bool initRepoWithCommit(const QString &dir)
{
    if (!initEmptyRepo(dir)) {
        return false;
    }
    QFile f(dir + QStringLiteral("/a.txt"));
    if (!f.open(QIODevice::WriteOnly)) {
        return false;
    }
    f.write("hello\n");
    f.close();
    return runGit(dir, {QStringLiteral("add"), QStringLiteral(".")}) == 0
        && runGit(dir, {QStringLiteral("-c"), QStringLiteral("commit.gpgsign=false"),
                        QStringLiteral("commit"), QStringLiteral("-q"),
                        QStringLiteral("-m"), QStringLiteral("init")}) == 0;
}
} // namespace

class NewAgentDialogTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void cleanup();

    void recommendedDefaultRequestsAuto();
    void uncheckedRequestsWorkspace();
    void isolationCopyNeverSaysSandbox();
    void labelMakesNoContainmentPromise();
    void probeAnswersWhatCreateWouldDo();
    void unknownProjectKeepsTheWordingConditional();
    void noCommitsProjectWithdrawsThePromiseAndStillLaunches();
    void committedProjectKeepsTheRecommendedDefault();
    void probeDoesNotBlockTheDialog();
    void isolationModeLabelsNeverPromiseWhatAutoMayDecline();
    void absentEngineAndUnkeyedProviderCannotBeChosen();
    void disclosureNoteIsQuietButNotGreyedOut();

private:
    QByteArray m_originalPath;
};

void NewAgentDialogTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    m_originalPath = qgetenv("PATH");
}

void NewAgentDialogTest::cleanup()
{
    qputenv("PATH", m_originalPath);
    qunsetenv("FIREWORKS_API_KEY");
    EngineAvailability::invalidate();
}

// The recommended default must be a mode that can succeed on ANY project,
// including one with no commits. "isolated" is not that mode.
void NewAgentDialogTest::recommendedDefaultRequestsAuto()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    QVERIFY2(box->isChecked(), "the private-copy default must stay on");
    QCOMPARE(dlg.choices().isolation, QStringLiteral("auto"));
}

void NewAgentDialogTest::uncheckedRequestsWorkspace()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    box->setChecked(false);
    QCOMPARE(dlg.choices().isolation, QStringLiteral("workspace"));
}

// A worktree is not a sandbox. The label and its tooltip are the decision
// point, so neither may teach the containment belief the mechanism does not
// deliver — and the tooltip must say what it actually does instead.
void NewAgentDialogTest::isolationCopyNeverSaysSandbox()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    QVERIFY(!box->text().contains(QStringLiteral("sandbox"), Qt::CaseInsensitive));
    const QString tip = box->toolTip();
    QVERIFY(!tip.isEmpty());
    QVERIFY(tip.contains(QStringLiteral("not a security sandbox")));
    QVERIFY(tip.contains(QStringLiteral("git worktree")));
    // The "sandbox" the tooltip may still mention is the one it denies.
    QVERIFY(!tip.contains(QStringLiteral("own sandbox")));
}

// The finding itself. The label is read while deciding how much rope to give an
// agent, so it may state the mechanism and nothing more: a worktree does not
// stop a tool writing an absolute path outside it, so "changes don't touch my
// files" is false, and "until I approve" is false for the same reason.
void NewAgentDialogTest::labelMakesNoContainmentPromise()
{
    const QString label = IsolationCopy::checkboxLabel();
    QVERIFY(!label.contains(QStringLiteral("touch"), Qt::CaseInsensitive));
    QVERIFY(!label.contains(QStringLiteral("approve"), Qt::CaseInsensitive));
    QVERIFY(!label.contains(QStringLiteral("sandbox"), Qt::CaseInsensitive));
    QVERIFY(label.contains(QStringLiteral("private copy")));
    // Whatever the checkbox says, the note under it must never claim the copy
    // contains the agent — every branch of it discloses the reach.
    const QString available =
        IsolationCopy::isolationNote(IsolationCopy::Availability::Available, true);
    QVERIFY(available.contains(QStringLiteral("outside the copy")));
    // And the branch for a project that cannot be copied must say what actually
    // happens instead, rather than repeating the offer.
    const QString unavailable =
        IsolationCopy::isolationNote(IsolationCopy::Availability::Unavailable, true);
    QVERIFY(unavailable.contains(QStringLiteral("directly in your own files")));
    QVERIFY(!unavailable.contains(QStringLiteral("its own copy")));
    // No raw git-speak from the core's failure path (audit F49).
    QVERIFY(!unavailable.contains(QStringLiteral("isolation needs")));
}

// The probe must agree with worktree.EffectiveIsolation on the same directory,
// and must fail closed in both directions: never "Available" for a project that
// cannot be copied, never "Unavailable" for a question we could not ask.
void NewAgentDialogTest::probeAnswersWhatCreateWouldDo()
{
    using IsolationCopy::Availability;
    QCOMPARE(probeAnswer(QString()), std::optional(Availability::Unknown));

    QTemporaryDir plain;
    QVERIFY(plain.isValid());
    QCOMPARE(probeAnswer(plain.path()), std::optional(Availability::Unavailable));

    QTemporaryDir fresh;
    QVERIFY(fresh.isValid());
    QVERIFY(initEmptyRepo(fresh.path()));
    QCOMPARE(probeAnswer(fresh.path()), std::optional(Availability::Unavailable));

    QTemporaryDir committed;
    QVERIFY(committed.isValid());
    QVERIFY(initRepoWithCommit(committed.path()));
    QCOMPARE(probeAnswer(committed.path()), std::optional(Availability::Available));
}

// F12's class. The probe used to run inside the constructor behind
// waitForStarted(1000) + waitForFinished(1500), so opening the dialog blocked
// the GUI thread for as long as git took — on a cold cache or a network mount,
// seconds, at the exact moment the user was trying to start work.
//
// A git that sleeps stands in for the slow disk. The dialog must appear at
// once, wearing the Unknown wording (which promises nothing and is true while
// the question is open), and take the answer when it lands.
void NewAgentDialogTest::probeDoesNotBlockTheDialog()
{
    QTemporaryDir bin;
    QVERIFY(bin.isValid());
    // `git rev-parse --verify HEAD` that takes two seconds and then says yes.
    writeFakeExecutable(bin.path(), QStringLiteral("git"),
                        QStringLiteral("sleep 2; exit 0"));
    qputenv("PATH", (bin.path() + QLatin1Char(':')
                     + QString::fromLocal8Bit(m_originalPath))
                        .toLocal8Bit());

    QTemporaryDir project;
    QVERIFY(project.isValid());

    QElapsedTimer clock;
    clock.start();
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr, nullptr, project.path());
    const qint64 constructMs = clock.elapsed();
    QVERIFY2(constructMs < 500,
             qPrintable(QStringLiteral("opening the dialog blocked the GUI "
                                       "thread for %1 ms waiting on git")
                            .arg(constructMs)));

    // Nothing is promised while the question is open.
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    QCOMPARE(isolationNoteText(&dlg),
             IsolationCopy::isolationNote(IsolationCopy::Availability::Unknown, true));

    // ...and the answer still arrives, without anyone having waited for it.
    QTRY_VERIFY_WITH_TIMEOUT(
        isolationNoteText(&dlg)
            == IsolationCopy::isolationNote(IsolationCopy::Availability::Available, true),
        10000);
}

// Item 2 of the convergence: three controls choose isolation and every one of
// them names "auto". "auto" isolates only where the project has history to
// branch from, so no label for it may promise a copy — and the promise must not
// be able to creep back into one picker while the others are clean, which is
// what a shared namespace is for.
void NewAgentDialogTest::isolationModeLabelsNeverPromiseWhatAutoMayDecline()
{
    const QString autoLabel = IsolationCopy::modeLabel(QStringLiteral("auto"));
    // Conditional, not a promise: no bare "Private copy", no "recommended"
    // dressing on an outcome we may not deliver.
    QVERIFY(autoLabel.contains(QStringLiteral("where the project allows")));
    QVERIFY(!autoLabel.contains(QStringLiteral("Always")));
    // The unconditional mode may say so — "isolated" really is unconditional
    // (the start fails rather than degrading).
    QVERIFY(IsolationCopy::modeLabel(QStringLiteral("isolated"))
                .contains(QStringLiteral("Always")));
    QVERIFY(IsolationCopy::modeLabel(QStringLiteral("workspace"))
                .contains(QStringLiteral("Directly in my files")));
    // An unrecognised token falls back to the wording that promises least.
    QCOMPARE(IsolationCopy::modeLabel(QStringLiteral("nonsense")), autoLabel);
    // And no picker's tooltip may teach containment (audit F30).
    const QString tip = IsolationCopy::modeTooltip();
    QVERIFY(tip.contains(QStringLiteral("not a security sandbox")));
    QVERIFY(tip.contains(QStringLiteral("does not confine the program")));
}

// Audit F37/F46 at the guided front door: it starts the agent the moment it is
// accepted, so an engine whose CLI is missing or a route whose key does not
// resolve must be visibly dead AND unselectable — otherwise the user spends a
// task description to be told something we already knew.
void NewAgentDialogTest::absentEngineAndUnkeyedProviderCannotBeChosen()
{
    QTemporaryDir bin;
    QVERIFY(bin.isValid());
    // `kimi` present, `claude` absent — and no key anywhere for the routed
    // presets, which are seeded with none.
    writeFakeExecutable(bin.path(), QStringLiteral("kimi"), QStringLiteral("exit 0"));
    qputenv("PATH", bin.path().toLocal8Bit());
    qunsetenv("FIREWORKS_API_KEY");
    EngineAvailability::invalidate();

    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QComboBox *engines = engineCombo(&dlg);
    QVERIFY(engines != nullptr);

    const int claudeIdx = engines->findData(QStringLiteral("claude"));
    QVERIFY(claudeIdx >= 0);
    QVERIFY2(engines->itemText(claudeIdx).contains(QStringLiteral("not installed")),
             "a missing engine must say so");
    QVERIFY2(!entryEnabled(engines, claudeIdx),
             "a missing engine must not be selectable");
    QVERIFY2(engines->currentIndex() != claudeIdx,
             "and the picker must not be resting on it");

    // Every routed provider entry: keyless, so dead for the same reason.
    int routedRows = 0;
    for (int i = 0; i < engines->count(); ++i) {
        const QString data = engines->itemData(i).toString();
        const QString providerId = data.section(QLatin1Char('|'), 1);
        if (providerId.isEmpty()) {
            continue;
        }
        ++routedRows;
        QVERIFY2(!entryEnabled(engines, i),
                 qPrintable(QStringLiteral("route %1 has no key and is still "
                                           "selectable").arg(data)));
        QVERIFY(engines->itemText(i).contains(QStringLiteral("no API key set")));
        QVERIFY2(engines->currentIndex() != i, "and must not be preselected");
    }
    QVERIFY2(routedRows > 0, "the seeded presets should have produced routed rows");
}

// Audit item 6. A disclosure is the sentence that tells you what will actually
// happen to your files; rendering it in QPalette::Disabled makes it look like
// something that does not apply — the opposite of its purpose — and drops it
// under every contrast floor. Quieter than body text, yes; greyed out, no.
void NewAgentDialogTest::disclosureNoteIsQuietButNotGreyedOut()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QLabel *note = nullptr;
    const auto labels = dlg.findChildren<QLabel *>();
    for (QLabel *l : labels) {
        if (l->text() == isolationNoteText(&dlg) && !l->text().isEmpty()) {
            note = l;
            break;
        }
    }
    QVERIFY(note != nullptr);

    const QPalette pal = note->palette();
    const QColor shown = pal.color(QPalette::Active, QPalette::WindowText);
    const QColor bg = pal.color(QPalette::Active, QPalette::Window);
    const QColor greyedOut = dlg.palette().color(QPalette::Disabled, QPalette::WindowText);
    QVERIFY2(shown != greyedOut,
             "disclosure text must not be painted in the toolkit's "
             "\"this does not apply to you\" colour");

    // Relative luminance, the WCAG definition — the disclosure must be further
    // from the background than the disabled colour it replaced.
    const auto luminance = [](const QColor &c) {
        const auto channel = [](double v) {
            v /= 255.0;
            return v <= 0.03928 ? v / 12.92 : std::pow((v + 0.055) / 1.055, 2.4);
        };
        return 0.2126 * channel(c.red()) + 0.7152 * channel(c.green())
            + 0.0722 * channel(c.blue());
    };
    const auto contrast = [&luminance](const QColor &a, const QColor &b) {
        const double la = luminance(a);
        const double lb = luminance(b);
        return (std::max(la, lb) + 0.05) / (std::min(la, lb) + 0.05);
    };
    const double shownRatio = contrast(shown, bg);
    QVERIFY2(shownRatio > contrast(greyedOut, bg),
             "the disclosure must read more strongly than disabled text, not less");
    QVERIFY2(shownRatio >= 4.5,
             qPrintable(QStringLiteral("disclosure contrast is only %1:1")
                            .arg(shownRatio, 0, 'f', 2)));
}

// With no project path the dialog cannot know, so it must not decide either
// way: the request stays "auto" (which still launches anywhere) and the wording
// covers both outcomes instead of promising one.
void NewAgentDialogTest::unknownProjectKeepsTheWordingConditional()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    QVERIFY(box->isChecked());
    QVERIFY(box->isEnabled());
    QCOMPARE(dlg.choices().isolation, QStringLiteral("auto"));
    const QString note = isolationNoteText(&dlg);
    QVERIFY2(!note.isEmpty(), "the isolation note must be shown");
    QVERIFY(note.contains(QStringLiteral("directly in your own files")));
}

// F30 second pass + F49 together: on a repo with nothing to branch from, "auto"
// degrades to the user's own files. The dialog must have withdrawn the promise
// BEFORE it is accepted — and the launch must still work, which is what F49's
// fix bought and what this must not spend.
void NewAgentDialogTest::noCommitsProjectWithdrawsThePromiseAndStillLaunches()
{
    QTemporaryDir fresh;
    QVERIFY(fresh.isValid());
    QVERIFY(initEmptyRepo(fresh.path()));

    NewAgentDialog dlg(QStringLiteral("proj"), nullptr, nullptr, fresh.path());
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    // The probe is asynchronous now (see probeDoesNotBlockTheDialog): the
    // dialog opens on the conditional wording and withdraws the offer when git
    // answers. The withdrawal is what matters, not when it happens.
    QTRY_VERIFY_WITH_TIMEOUT(!box->isChecked(), 10000);
    QVERIFY2(!box->isChecked(),
             "a private copy that cannot be made must not be offered as ticked");
    QVERIFY2(!box->isEnabled(),
             "and must not be tickable — the note explains why");
    // The honest user's launch: "workspace" is exactly what auto would have
    // degraded to, so a brand-new project still starts.
    QCOMPARE(dlg.choices().isolation, QStringLiteral("workspace"));
    const QString note = isolationNoteText(&dlg);
    QVERIFY(note.contains(QStringLiteral("directly in your own files")));
    QVERIFY(note.contains(QStringLiteral("first commit")));
}

// The recommended default is untouched where it can be honoured.
void NewAgentDialogTest::committedProjectKeepsTheRecommendedDefault()
{
    QTemporaryDir repo;
    QVERIFY(repo.isValid());
    QVERIFY(initRepoWithCommit(repo.path()));

    NewAgentDialog dlg(QStringLiteral("proj"), nullptr, nullptr, repo.path());
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    // Wait for the asynchronous probe to upgrade the conditional wording.
    QTRY_VERIFY_WITH_TIMEOUT(
        isolationNoteText(&dlg)
            == IsolationCopy::isolationNote(IsolationCopy::Availability::Available, true),
        10000);
    QVERIFY(box->isChecked());
    QVERIFY(box->isEnabled());
    QCOMPARE(dlg.choices().isolation, QStringLiteral("auto"));
    const QString note = isolationNoteText(&dlg);
    QVERIFY(note.contains(QStringLiteral("its own copy")));
    QVERIFY(note.contains(QStringLiteral("outside the copy")));
}

QTEST_MAIN(NewAgentDialogTest)
#include "NewAgentDialogTest.moc"
