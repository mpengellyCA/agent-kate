// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "HarnessTraits.h"
#include "../ipc/CoreClient.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QJsonArray>
#include <QJsonObject>

QString HarnessTraits::stickyModeKey() const
{
    return id == QLatin1String("claude") ? QStringLiteral("permissionMode")
                                         : id + QStringLiteral("Mode");
}

QString HarnessTraits::stickyEffortKey() const
{
    return id == QLatin1String("claude") ? QStringLiteral("effort")
                                         : id + QStringLiteral("Thinking");
}

QString HarnessTraits::optionKey(const QString &optionId) const
{
    return id + QStringLiteral("Opt-") + optionId;
}

namespace {
// Built-in fallbacks mirroring the core adapters (cmd/akcore/harness_*.go),
// served until agent.capabilities answers. Keeping them in lockstep is what
// makes the pickers correct on the very first frame and against older cores.
HarnessTraits claudeDefaults()
{
    HarnessTraits t;
    t.id = QStringLiteral("claude");
    t.displayName = QStringLiteral("Claude Code");
    t.fork = t.compaction = t.promote = true;
    t.providerRouting = t.cowork = true;
    t.usageReporting = t.sessionBrowse = true;
    t.modelPicker = QStringLiteral("tiers");
    t.permissionModes = {QStringLiteral("acceptEdits"), QStringLiteral("default"),
                         QStringLiteral("plan"), QStringLiteral("auto"),
                         QStringLiteral("bypassPermissions")};
    t.efforts = {QStringLiteral("low"), QStringLiteral("medium"), QStringLiteral("high"),
                 QStringLiteral("xhigh"), QStringLiteral("max")};
    return t;
}

HarnessTraits kimiDefaults()
{
    HarnessTraits t;
    t.id = QStringLiteral("kimi");
    t.displayName = QStringLiteral("Kimi Code");
    t.badge = QStringLiteral("Kimi");
    t.effortLive = true;
    t.modelPicker = QStringLiteral("discovered");
    return t;
}

HarnessTraits fromJson(const QJsonObject &o)
{
    HarnessTraits t;
    t.id = o.value(QStringLiteral("id")).toString();
    t.displayName = o.value(QStringLiteral("displayName")).toString();
    t.badge = o.value(QStringLiteral("badge")).toString();
    t.fork = o.value(QStringLiteral("fork")).toBool();
    t.compaction = o.value(QStringLiteral("compaction")).toBool();
    t.promote = o.value(QStringLiteral("promote")).toBool();
    t.providerRouting = o.value(QStringLiteral("providerRouting")).toBool();
    t.cowork = o.value(QStringLiteral("cowork")).toBool();
    t.effortLive = o.value(QStringLiteral("effortLive")).toBool();
    t.usageReporting = o.value(QStringLiteral("usageReporting")).toBool();
    t.sessionBrowse = o.value(QStringLiteral("sessionBrowse")).toBool();
    t.modelPicker = o.value(QStringLiteral("modelPicker")).toString();
    t.permissionModes.clear();
    const QJsonArray modes = o.value(QStringLiteral("permissionModes")).toArray();
    for (const QJsonValue &v : modes) {
        t.permissionModes << v.toString();
    }
    t.efforts.clear();
    const QJsonArray efforts = o.value(QStringLiteral("efforts")).toArray();
    for (const QJsonValue &v : efforts) {
        t.efforts << v.toString();
    }
    return t;
}
} // namespace

HarnessRegistry *HarnessRegistry::self()
{
    static HarnessRegistry instance;
    return &instance;
}

HarnessRegistry::HarnessRegistry()
{
    const HarnessTraits claude = claudeDefaults();
    const HarnessTraits kimi = kimiDefaults();
    m_traits.insert(claude.id, claude);
    m_traits.insert(kimi.id, kimi);
    m_order = {claude.id, kimi.id};
}

void HarnessRegistry::fetch(CoreClient *core)
{
    if (!core || !core->isConnected()) {
        return;
    }
    core->call(
        QStringLiteral("agent.capabilities"), QJsonObject{},
        [this](const QJsonObject &result, const QJsonObject &error) {
            if (!error.isEmpty()) {
                return; // keep serving the built-in defaults
            }
            const QJsonArray harnesses =
                result.value(QStringLiteral("harnesses")).toArray();
            if (harnesses.isEmpty()) {
                return;
            }
            QHash<QString, HarnessTraits> fresh;
            QStringList order;
            for (const QJsonValue &v : harnesses) {
                const HarnessTraits t = fromJson(v.toObject());
                if (t.id.isEmpty()) {
                    continue;
                }
                fresh.insert(t.id, t);
                order << t.id;
            }
            if (order.isEmpty()) {
                return;
            }
            m_traits = fresh;
            m_order = order;
            emit changed();
        },
        this);
}

void HarnessRegistry::ensureDiscovered(CoreClient *core, const QString &harnessId)
{
    const HarnessTraits t = traits(harnessId);
    if (t.modelPicker != QLatin1String("discovered")) {
        return; // static vocabulary — nothing to probe
    }
    // Already cached, from an earlier probe or a past session's init event?
    // The model list is the sentinel: it is always present when any probe or
    // init event has been persisted.
    const QStringList cached =
        KSharedConfig::openConfig()
            ->group(QStringLiteral("Agent"))
            .readEntry(t.optionKey(QStringLiteral("model")), QStringList());
    if (!cached.isEmpty()) {
        return;
    }
    if (!core || !core->isConnected() || m_discovering.contains(t.id)) {
        return;
    }
    m_discovering.insert(t.id);
    const QString id = t.id;
    core->call(
        QStringLiteral("agent.discoverOptions"),
        QJsonObject{{QStringLiteral("backend"), id}},
        [this, id](const QJsonObject &result, const QJsonObject &error) {
            m_discovering.remove(id);
            if (!error.isEmpty()) {
                // Failed probe (CLI missing, not logged in, …): keep today's
                // placeholder pickers; the next selection retries.
                return;
            }
            const QJsonArray configOptions =
                result.value(QStringLiteral("configOptions")).toArray();
            if (configOptions.isEmpty()) {
                return;
            }
            persistDiscoveredOptions(id, configOptions);
            emit changed(); // open pickers rebuild from the fresh cache
        },
        this);
}

void HarnessRegistry::persistDiscoveredOptions(const QString &harnessId,
                                               const QJsonArray &configOptions)
{
    const HarnessTraits t = self()->traits(harnessId);
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    for (const QJsonValue &ov : configOptions) {
        const QJsonObject opt = ov.toObject();
        const QString id = opt.value(QStringLiteral("id")).toString();
        if (id.isEmpty()) {
            continue;
        }
        QStringList entries;
        const QJsonArray values = opt.value(QStringLiteral("options")).toArray();
        for (const QJsonValue &vv : values) {
            const QJsonObject val = vv.toObject();
            entries << val.value(QStringLiteral("value")).toString()
                           + QLatin1Char('|')
                           + val.value(QStringLiteral("name")).toString();
        }
        if (!entries.isEmpty()) {
            cfg.writeEntry(t.optionKey(id), entries);
        }
    }
}

HarnessTraits HarnessRegistry::traits(const QString &id) const
{
    // The empty id is the legacy record spelling of the default engine.
    const QString key = id.isEmpty() ? m_order.value(0) : id;
    const auto it = m_traits.constFind(key);
    if (it != m_traits.constEnd()) {
        return *it;
    }
    // Unknown id (a newer core?): claude-shaped defaults under its own name,
    // so pickers still render and nothing dereferences a missing entry.
    HarnessTraits t = claudeDefaults();
    t.id = key;
    t.displayName = key;
    return t;
}

QList<HarnessTraits> HarnessRegistry::all() const
{
    QList<HarnessTraits> out;
    out.reserve(m_order.size());
    for (const QString &id : m_order) {
        out << m_traits.value(id);
    }
    return out;
}

QString HarnessRegistry::modeLabel(const QString &value)
{
    if (value == QLatin1String("acceptEdits")) {
        return i18n("Apply edits automatically");
    }
    if (value == QLatin1String("default")) {
        return i18n("Ask before each step");
    }
    if (value == QLatin1String("plan")) {
        return i18n("Plan first — read-only until approved");
    }
    if (value == QLatin1String("auto")) {
        return i18n("Work freely");
    }
    if (value == QLatin1String("bypassPermissions")) {
        return i18n("Expert — never ask");
    }
    return value;
}

QString HarnessRegistry::effortLabel(const QString &value)
{
    if (value == QLatin1String("low")) {
        return i18n("Low");
    }
    if (value == QLatin1String("medium")) {
        return i18n("Medium");
    }
    if (value == QLatin1String("high")) {
        return i18n("High");
    }
    if (value == QLatin1String("xhigh")) {
        return i18n("Extra-high");
    }
    if (value == QLatin1String("max")) {
        return i18n("Maximum");
    }
    return value;
}
