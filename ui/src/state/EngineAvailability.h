// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QList>
#include <QString>

// EngineAvailability answers one question, in one place: is the command-line
// program that actually runs an agent installed on this machine?
//
// Agent Kate does not contain a model. Every agent is an external CLI that
// akcore spawns by name off $PATH (`claude` — core/internal/agent/agent.go;
// `kimi` — core/internal/kimi/thread.go), so a machine without that program
// can open a project, pick an engine, write a task and only then be told
// `exec: "claude": executable file not found in $PATH` (audit F37). This
// module is the pre-flight that moves the discovery to before the typing.
//
// DELIBERATELY ONE MODULE. Plan 26 adds a real preflight health card built on
// `claude doctor` / `kimi doctor`, which answers a superset of this question
// (installed? authenticated? config valid?). When it lands it should REPLACE
// this file wholesale — every caller goes through scan()/isPresent(), so the
// seam is the two functions, not a check scattered through the widgets.
namespace EngineAvailability {

// One engine's install state. `executable` is the program name akcore spawns,
// which is the only thing that can be probed from here — presence says nothing
// about authentication or configuration (that is plan 26's card).
struct Engine {
    QString id;          // harness id, e.g. "claude"
    QString displayName; // "Claude Code"
    QString executable;  // the program looked up on $PATH
    QString installUrl;  // where a human gets it; empty when we have no honest link
    bool present = false;
};

// One row per harness the registry knows, in engine-picker order. Results are
// cached for the run (a $PATH probe per registered engine is cheap but this is
// called from menu builds); invalidate() forces a re-probe.
QList<Engine> scan();
void invalidate();

// Is this harness's CLI on $PATH? An unknown harness id is reported present:
// a newer core knows engines this build does not, and refusing to offer them
// would be worse than a start that fails with the core's own message.
bool isPresent(const QString &harnessId);

// True when not one engine resolved — the state that strands a new user, and
// the only one worth a persistent banner. One missing engine out of two is
// merely a picker entry that says so.
bool noneAvailable(const QList<Engine> &engines);

// The banner sentence for that state, naming the CLIs by the exact command a
// human types. Empty when at least one engine is present.
QString missingEnginesMessage(const QList<Engine> &engines);

// The install link to offer beside that sentence — the first missing engine
// that has an honest one. Empty when none of them do.
QString installUrl(const QList<Engine> &engines);

// Picker label for one engine: the display name, suffixed when its CLI is
// missing so a dead choice is visibly dead.
QString pickerLabel(const Engine &engine);

// The same label for callers that iterate HarnessRegistry rather than scan()
// — the New Agent dialog, the panel's engine combo and the ensemble editor all
// have a harness id and a display name in hand, and before this overload each
// spelled "not installed" itself. One suffix, one place: four pickers that
// disagree about whether an engine is dead are four chances to be believed.
QString pickerLabel(const QString &harnessId, const QString &displayName);

} // namespace EngineAvailability
