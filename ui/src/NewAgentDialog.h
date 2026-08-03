// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "state/Reactive.h"

#include <KLocalizedString>

#include <QColor>
#include <QDialog>
#include <QList>
#include <QPalette>
#include <QString>
#include <QStringList>
#include <QWidget>

#include <functional>

// DisclosureStyle paints the quiet sentence under a control — the one that says
// what will ACTUALLY happen ("the agent will work directly in your own files",
// "this discards your own uncommitted work too").
//
// It must read as quiet, not as inapplicable. These labels used to be rendered
// in QPalette::Disabled, which is the exact colour the toolkit uses for a
// control that does not apply to you: a sentence in it looks like something the
// UI has switched off, which is the opposite of what a disclosure is for — and
// on Breeze it lands around 2.4:1 against the window, under every contrast
// floor there is. So the colour here is the ENABLED foreground, blended part of
// the way to the background: quieter than body text, still plainly addressed to
// the reader.
namespace DisclosureStyle {

// How much of the enabled foreground survives the blend. High enough to stay
// legible, low enough that the label does not compete with the question above
// it.
inline constexpr qreal kForegroundWeight = 0.75;

inline void apply(QWidget *label)
{
    if (!label) {
        return;
    }
    QPalette pal = label->palette();
    const QColor fg = pal.color(QPalette::Active, QPalette::WindowText);
    const QColor bg = pal.color(QPalette::Active, QPalette::Window);
    const auto mix = [](int a, int b) {
        return qRound(a * kForegroundWeight + b * (1.0 - kForegroundWeight));
    };
    const QColor muted(mix(fg.red(), bg.red()), mix(fg.green(), bg.green()),
                       mix(fg.blue(), bg.blue()));
    pal.setColor(QPalette::WindowText, muted);
    label->setPalette(pal);
}

} // namespace DisclosureStyle

class QPlainTextEdit;
class QComboBox;
class QCheckBox;
class QJsonObject;
class QLabel;
class QLineEdit;
class QDoubleSpinBox;
class QFormLayout;
class QToolButton;
class QVBoxLayout;
class QWidget;
class CoreClient;
class ElidingLabel;

// EngineCheck / EngineHealth mirror one engine's verdict from the core's
// engine.health RPC (plan 26; core/internal/harness Health/Check). Value
// types with full-field equality, because the preflight card publishes them
// through a Reactive<EngineHealth>: an identical verdict — the common case,
// with the core caching health for 30 s — repaints nothing.
struct EngineCheck {
    QString name;   // "binary", "config", "auth", "models", …
    QString state;  // "ok" | "warn" | "bad" | "unknown"
    QString detail;
    // A command the user can run, verbatim (e.g. "kimi login") — the engine's
    // own advertised remedy, never one the UI invents.
    QString remedy;
    bool operator==(const EngineCheck &) const = default;
};

struct EngineHealth {
    QString engineId;
    QString state; // worst of checks, rolled up core-side
    QString version;
    int models = 0;
    QList<EngineCheck> checks;
    bool operator==(const EngineHealth &) const = default;
    static EngineHealth fromJson(const QJsonObject &o);
};

// HealthChip — the traffic light: one ChipPainter pill whose colours come
// from KColorScheme's status roles (Positive/Neutral/Negative), so it stays
// native under every colour scheme.
class HealthChip : public QWidget
{
    Q_OBJECT
public:
    explicit HealthChip(QWidget *parent = nullptr);
    void setVerdict(const QString &state, const QString &text);
    QSize sizeHint() const override;

protected:
    void paintEvent(QPaintEvent *event) override;

private:
    QString m_state;
    QString m_text;
};

// PreflightCard — the collapsible engine-health card under the New Agent
// dialog's engine combo (plan 26 phase 2). Header: a traffic-light chip plus
// a disclosure toggle; body: one line per non-OK check, and a copyable
// command for any check that carries a remedy.
//
// It WARNS ONLY. Start is never blocked on a bad verdict — a health check
// that is wrong must never be the thing that stops work — so nothing here
// gates the dialog's buttons. (Offering to RUN the remedy in a terminal is
// deferred: the terminal lives on MainWindow, which this change does not
// touch; the command is copyable instead.)
//
// Flicker rule: the verdict lives in a Reactive<EngineHealth> with
// full-field equality, so publishing the same health twice rebuilds and
// repaints nothing — PreflightCardTest pins that, via rebuilds().
class PreflightCard : public QWidget
{
    Q_OBJECT
public:
    explicit PreflightCard(QWidget *parent = nullptr);

    // The engine combo moved and the probe is in flight: show a quiet
    // "checking…" chip for that engine. A verdict already held for the SAME
    // engine stays put — the cache will answer with it anyway.
    void setPending(const QString &engineId);
    // Publish a verdict. Identical health is silently ignored (Reactive).
    void setHealth(const EngineHealth &health);

    // How many times the card actually rebuilt — the test's observable.
    int rebuilds() const { return m_rebuilds; }

private:
    void rebuild(const EngineHealth &health);

    Reactive<EngineHealth> m_health;
    int m_rebuilds = 0;
    HealthChip *m_chip = nullptr;
    QToolButton *m_toggle = nullptr;
    ElidingLabel *m_summary = nullptr;
    QWidget *m_body = nullptr;
    QVBoxLayout *m_bodyLayout = nullptr;
    bool m_autoExpanded = false; // expand once on the first non-OK verdict
};

// The choices a guided New Agent dialog collects. Empty strings mean "leave the
// agent's sticky default" (the data values match AgentPanel's combos).
struct NewAgentChoices {
    QString task;           // optional first task, pre-filled into the composer
    // ensemble names a controller/worker recipe (plan 16 P4) instead of a single
    // agent. When set, every field below except task is the ensemble's business:
    // the caller applies it core-side (mode.apply) rather than creating a panel.
    QString ensemble;
    QString backend;        // harness id from the engine picker (claude, kimi, …)
    QString providerId;     // optional provider overlay; "" = direct
    QString modelId;        // tier token / discovered model id; "" = default
    QString isolation;      // auto | isolated | workspace
    QString permissionMode; // the harness's own mode vocabulary; "" = default
    QString effort;         // the harness's own effort vocabulary; "" = default
    // The launch-option sweep (plan 16 P6), each offered only when the chosen
    // engine declares the capability. Empty lists mean "not requested".
    QStringList fallbackModels;  // models to fall back to, in order
    QStringList disallowedTools; // tool names this agent may not use
    QStringList addDirs;         // extra directories its tools may reach
    // The control-channel sweep. strictMcpConfig runs the agent with only the
    // MCP servers Agent Kate wires in, ignoring the user's global ones;
    // maxBudgetUsd is a hard spend ceiling the engine enforces (0 = uncapped).
    bool strictMcpConfig = false;
    double maxBudgetUsd = 0.0;
};

// IsolationCopy holds the app's isolation wording, and the probe that decides
// which of it is true, apart from the widgets that show them — the WorktreeCopy
// pattern, for the same reason: this is the sentence a user reads while
// deciding how much rope to give an agent, and a sentence only reachable
// through exec() cannot be asserted on.
//
// SHARED, not the dialog's private property. Three controls choose isolation —
// this dialog's checkbox, AgentPanel's "Where it works" combo (the Ctrl+N path,
// used more than the guided dialog) and EnsembleDialog's controller/worker rows
// — and each used to word it itself. Two of them still called "auto" a private
// copy after the third had stopped, which is the exact failure this namespace
// exists to make impossible: the labels live here, so a fix lands in all of
// them or in none.
//
// HONEST LABELLING (audit F30, second pass). The first pass removed the word
// "sandbox"; the claim simply moved into the checkbox at the decision point,
// which read "Work in a private copy, so changes don't touch my files until I
// approve". That sentence is false twice over:
//
//   1. A git worktree isolates CHECKOUT STATE, not the process.
//      docs/security-model.md says in bold that it is not a sandbox and does
//      not pretend to be one — the agent runs at your uid, and a tool writing
//      an absolute path outside the copy touches your files immediately and is
//      invisible to every diff and dirty count we show.
//   2. The checkbox requests "auto", not "isolated" (audit F49 — see choices()).
//      On a project with no commit to branch from, "auto" DEGRADES to the
//      workspace: the agent works directly in the user's files while the label
//      promises the opposite, in the one case the user cannot see.
//
// So the label states the MECHANISM and promises nothing, and the dialog
// resolves the degradation BEFORE it is accepted: probeIsolationAsync() asks
// the project whether a copy can be made at all, and the note under the
// checkbox says what will actually happen. Where we cannot answer (no project
// path, no git, a probe that has not come back yet) the copy stays conditional
// — it never promises isolation we have not confirmed.
namespace IsolationCopy {

// What probeIsolationAsync() found. Unknown is the fail-closed answer, and also
// the STARTING one — the probe does not block, so every caller renders this
// until git answers. It promises
// nothing, because a promise we cannot check is exactly what F30 is about.
enum class Availability {
    Unknown,     // not asked, or the question could not be answered
    Available,   // the project has a commit to branch a worktree from
    Unavailable, // nothing committed yet (or not a git repo): no copy possible
};

// The checkbox label. Names the mechanism; claims no containment.
inline QString checkboxLabel()
{
    return i18n("Work in a private copy of the project (a git worktree)");
}

// The tooltip. Says what the mechanism is and, in the same breath, what it is
// not — the security model's own words, at the decision point.
inline QString checkboxTooltip()
{
    return i18n(
        "Recommended. The agent gets its own checkout of the project (a git "
        "worktree) and you merge its work back when you are happy with it, so "
        "it is not editing the files you have open.\n\n"
        "This is not a security sandbox. The agent runs as you, with your "
        "permissions, your network and your credentials, and anything it runs "
        "can still write to files outside the copy — Agent Kate only shows you "
        "what changed inside it.");
}

// The line under the checkbox: what will actually happen to THIS project, given
// what the probe found and whether the box is ticked.
inline QString isolationNote(Availability a, bool wantsPrivateCopy)
{
    if (a == Availability::Unavailable) {
        // The honest half of audit F49, said before the launch rather than
        // apologised for after it — and without the core's git-speak ("isolation
        // needs at least one commit"), which is what used to reach the user.
        return i18n(
            "This project has nothing saved in git yet, so there is no copy to "
            "make: the agent will work directly in your own files. A private "
            "copy becomes possible once the project has its first commit.");
    }
    if (!wantsPrivateCopy) {
        return i18n(
            "The agent will work directly in your own files, mixed in with any "
            "changes of your own.");
    }
    if (a == Availability::Unknown) {
        // Conditional on purpose: this is the "auto" request with the answer
        // still open, and the one thing we must not do is promise the isolated
        // outcome and let the user discover the other one afterwards.
        return i18n(
            "Where this project has git history to copy from, the agent works "
            "in its own copy; where it does not, it works directly in your own "
            "files — Agent Kate says which when the agent starts.");
    }
    return i18n(
        "The agent will work in its own copy of this project. It still runs "
        "with your permissions and can reach files outside the copy, so review "
        "what it did rather than treating the copy as a fence.");
}

// modeLabel is the same three choices as a PICKER offers them — the isolation
// vocabulary agent.start takes ("auto" / "isolated" / "workspace"). One wording
// for every combo that lists them, because "auto" is the trap: it isolates only
// where the project has history to branch from and works directly in the user's
// own files where it does not, so its label must state the CONDITION and never
// the outcome. An unknown token falls back to "auto"'s wording, which promises
// least.
inline QString modeLabel(const QString &mode)
{
    if (mode == QLatin1String("isolated")) {
        return i18n("Always in a private copy (a git worktree)");
    }
    if (mode == QLatin1String("workspace")) {
        return i18n("Directly in my files");
    }
    return i18n("A private copy where the project allows one");
}

// The tooltip beside such a picker: what each of the three actually does, and —
// in the same breath, as at every other decision point — what none of them do.
inline QString modeTooltip()
{
    return i18n(
        "Where this agent does its work. Fixed once it starts.\n\n"
        "• A private copy where the project allows one — its own git worktree "
        "wherever the project has history to branch from; where it has none "
        "(nothing committed yet, or not a git repository) the agent works "
        "directly in your files instead, and Agent Kate says so when it "
        "starts.\n"
        "• Always in a private copy — its own git worktree, or no start at all: "
        "a project with nothing committed yet cannot give it one, and the "
        "attempt fails rather than quietly using your files.\n"
        "• Directly in my files — changes land in your project immediately, "
        "mixed in with any of your own.\n\n"
        "This is not a security sandbox. A private copy separates the agent's "
        "CHANGES from yours; it does not confine the program, which runs as "
        "you, with your files, your network and your credentials.");
}

// probeIsolationAsync asks whether projectPath has a commit for `git worktree
// add` to branch from — the same question core's worktree.EffectiveIsolation
// asks of the same repo, so the dialog's answer and the launch's behaviour
// agree. It reports through `done`:
//
//   Available   — `git rev-parse HEAD` resolved: a copy can be made.
//   Unavailable — git answered and there is no HEAD (a fresh repo), or the path
//                 is not a git repository at all. Create() will degrade to the
//                 workspace, so we say so up front.
//   Unknown     — we could not ask: no path, no git on PATH, or the probe did
//                 not answer. Never reported as Available (no promise), and
//                 never as Unavailable either: guessing "no copy" would push a
//                 perfectly isolable project into the user's own files because
//                 a subprocess was slow.
//
// ASYNCHRONOUS, and this is not a preference. The first version ran the probe
// with waitForStarted(1000) + waitForFinished(1500) inside the dialog's
// constructor, so opening New Agent could freeze the whole GUI for seconds on a
// cold cache or a network mount — the blocking-call-on-the-GUI-thread class
// audit F12 covers, at the exact moment the user is trying to start work. The
// caller therefore renders the Unknown wording (which promises nothing and is
// true while the question is open) and repaints when the answer lands.
//
// `context` is the object the callback writes to; it must not be null, and the
// callback is DROPPED if it dies first — a dialog closed while git is still
// running must not be written to (this codebase has a documented
// use-after-free class of exactly that shape). `done` runs on the caller's
// thread, either synchronously (for the no-path case, which needs no
// subprocess) or from the event loop.
void probeIsolationAsync(const QString &projectPath, QObject *context,
                         const std::function<void(Availability)> &done);

} // namespace IsolationCopy

// NewAgentDialog — a friendly front door for starting an agent: describe the
// task in plain words, pick how clever it should be and whether it works in a
// private copy, with the power options tucked behind "Advanced". It only
// collects choices; the caller creates the agent and pre-fills the task.
class NewAgentDialog : public QDialog
{
    Q_OBJECT
public:
    // projectPath is the workspace directory the agent will run in. It trails
    // `parent` (rather than sitting next to projectName, where it belongs)
    // purely so the existing three-argument call site keeps compiling — pass it
    // whenever you have it: without it the dialog cannot tell the user whether
    // the private copy it is offering is available for THIS project, and falls
    // back to conditional wording (audit F30/F49).
    explicit NewAgentDialog(const QString &projectName, CoreClient *core,
                            QWidget *parent = nullptr,
                            const QString &projectPath = QString());

    NewAgentChoices choices() const;

private:
    // Enable/disable the single-agent pickers: an ensemble brings its own
    // controller engine, model and options, so leaving them live would offer
    // choices the launch ignores.
    void applyEnsembleMode();
    // Repaint the isolation note and re-decide whether the checkbox may be
    // ticked at all. Called on construction, on every toggle, and from
    // applyEnsembleMode — the enablement has two independent owners (the
    // ensemble picker and the isolation probe) and neither may clobber the
    // other.
    void updateIsolationState();
    // Ask the core for the selected engine's preflight health (engine.health,
    // 30 s cache core-side) and publish it into the card. Fired from the
    // engine combo's currentIndexChanged handler; never blocks, never gates
    // the Create button.
    void refreshPreflight();

    CoreClient *m_core = nullptr; // for the lazy discovered-option probe
    QString m_projectPath;        // so claude's doctor reads the right directory
    QPlainTextEdit *m_task = nullptr;
    QComboBox *m_ensemble = nullptr; // "Single agent" or one ensemble
    QLabel *m_ensembleHint = nullptr;
    QComboBox *m_engine = nullptr; // harness + optional provider overlay
    PreflightCard *m_preflight = nullptr; // engine health, under the engine combo
    QComboBox *m_model = nullptr;
    QCheckBox *m_sandbox = nullptr;
    QLabel *m_isolationNote = nullptr;
    // What the project can actually do, resolved once at construction. Drives
    // both the note and whether the checkbox may be ticked, so the dialog never
    // accepts a request for a private copy it already knows cannot be made.
    IsolationCopy::Availability m_isolationAvailability =
        IsolationCopy::Availability::Unknown;
    QWidget *m_advanced = nullptr;
    QComboBox *m_permission = nullptr;
    QComboBox *m_effort = nullptr;
    // Sweep fields + their form rows, so a row can be hidden entirely for an
    // engine that cannot express it (offering it would be a lie).
    QLineEdit *m_fallbackModels = nullptr;
    QLineEdit *m_disallowedTools = nullptr;
    QLineEdit *m_addDirs = nullptr;
    QCheckBox *m_strictMcp = nullptr;
    QDoubleSpinBox *m_budget = nullptr;
    QFormLayout *m_advancedForm = nullptr;
    // Which sweep options the CURRENTLY selected engine can express — the same
    // capabilities that drive setRowVisible above, remembered so choices() can
    // ask "was this offered?" without asking the widget. Widget visibility is
    // NOT a stand-in: the whole Advanced section hides on the collapse toggle,
    // so reading isVisibleTo() there would silently drop a budget or isolation
    // flag the user set and then collapsed.
    struct SweepSupport {
        bool fallbackModels = false;
        bool disallowedTools = false;
        bool addDirs = false;
        bool strictMcpConfig = false;
        bool costBudget = false;
    };
    SweepSupport m_sweepSupport;
};
