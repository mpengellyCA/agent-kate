// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "EngineAvailability.h"
#include "HarnessTraits.h"

#include <KLocalizedString>

#include <QHash>
#include <QStandardPaths>

namespace {

// The program each harness's supervisor spawns, and where a human gets it.
//
// NOT a capability — nothing here mirrors core/internal/harness's Capabilities
// struct, so the LOCKSTEP rule does not apply. What it does mirror is the
// *binary name* the core defaults to when it is given none:
// agent.NewSupervisor("") → "claude" (core/internal/agent/agent.go) and
// kimi.NewSupervisor("") → "kimi" (core/internal/kimi/thread.go), and Codex
// defaults to "codex" (core/internal/codex), all from run.go. An engine this
// table does not know falls back to its harness id,
// which is what a third adapter would name its binary anyway (docs/HARNESSES.md).
struct EngineFacts {
    const char *executable;
    const char *installUrl;
};

const QHash<QString, EngineFacts> &knownEngines()
{
    static const QHash<QString, EngineFacts> facts = {
        {QStringLiteral("claude"),
         {"claude", "https://docs.claude.com/en/docs/claude-code/setup"}},
        // No install URL for kimi: we have no documented page we can promise
        // still exists, and a dead link at the moment someone is stuck is
        // worse than naming the command and letting them search for it.
        {QStringLiteral("kimi"), {"kimi", ""}},
        {QStringLiteral("codex"), {"codex", "https://developers.openai.com/codex/cli/"}},
    };
    return facts;
}

QList<EngineAvailability::Engine> *cache()
{
    static QList<EngineAvailability::Engine> cached;
    return &cached;
}

} // namespace

namespace EngineAvailability {

QList<Engine> scan()
{
    QList<Engine> *cached = cache();
    const QList<HarnessTraits> registered = HarnessRegistry::self()->all();
    // Self-invalidating on the engine SET, not just on an explicit call: the
    // capability fetch can add or drop a harness, and every consumer of this
    // rebuilds off HarnessRegistry::changed() in an order nobody controls. A
    // cache that survived that would report on engines the core no longer has.
    if (!cached->isEmpty()) {
        bool sameSet = cached->size() == registered.size();
        for (int i = 0; sameSet && i < registered.size(); ++i) {
            sameSet = cached->at(i).id == registered.at(i).id;
        }
        if (sameSet) {
            return *cached;
        }
        cached->clear();
    }
    for (const HarnessTraits &t : registered) {
        if (t.id.isEmpty()) {
            continue;
        }
        Engine e;
        e.id = t.id;
        e.displayName = t.displayName.isEmpty() ? t.id : t.displayName;
        const auto it = knownEngines().constFind(t.id);
        e.executable = it != knownEngines().constEnd()
            ? QString::fromLatin1(it->executable)
            : t.id;
        if (it != knownEngines().constEnd()) {
            e.installUrl = QString::fromLatin1(it->installUrl);
        }
        e.present = !QStandardPaths::findExecutable(e.executable).isEmpty();
        cached->append(e);
    }
    return *cached;
}

void invalidate()
{
    cache()->clear();
}

bool isPresent(const QString &harnessId)
{
    const QList<Engine> engines = scan();
    for (const Engine &e : engines) {
        if (e.id == harnessId) {
            return e.present;
        }
    }
    return true; // unknown engine — see the header
}

bool noneAvailable(const QList<Engine> &engines)
{
    if (engines.isEmpty()) {
        return false; // nothing registered yet; nothing honest to claim
    }
    for (const Engine &e : engines) {
        if (e.present) {
            return false;
        }
    }
    return true;
}

QString missingEnginesMessage(const QList<Engine> &engines)
{
    if (!noneAvailable(engines)) {
        return QString();
    }
    QStringList commands;
    for (const Engine &e : engines) {
        commands << QStringLiteral("%1 (%2)").arg(e.displayName, e.executable);
    }
    return i18n(
        "Agent Kate does not contain an AI — it drives an agent command-line "
        "tool, and none is installed. Looked for: %1. Install one and restart "
        "Agent Kate; until then, starting an agent will fail.",
        commands.join(i18nc("separator in a list of programs", ", ")));
}

QString installUrl(const QList<Engine> &engines)
{
    for (const Engine &e : engines) {
        if (!e.present && !e.installUrl.isEmpty()) {
            return e.installUrl;
        }
    }
    return QString();
}

// The one "this engine is dead" suffix in the product. Both pickerLabel
// overloads go through it so a picker cannot invent a second spelling; the
// parenthetical form composes with the provider annotation the routed entries
// carry ("Claude Code (not installed) via Fireworks (no API key set)").
static QString missingSuffix(const QString &displayName)
{
    return i18nc("engine whose command-line program is not installed",
                 "%1 (not installed)", displayName);
}

QString pickerLabel(const Engine &engine)
{
    return engine.present ? engine.displayName : missingSuffix(engine.displayName);
}

QString pickerLabel(const QString &harnessId, const QString &displayName)
{
    const QString name = displayName.isEmpty() ? harnessId : displayName;
    // isPresent() answers permissively for a harness this build does not know
    // (see the header), so an engine a newer core added is never labelled dead.
    return isPresent(harnessId) ? name : missingSuffix(name);
}

} // namespace EngineAvailability
