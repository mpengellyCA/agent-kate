// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "HarnessTraits.h"
#include "../ProviderConfig.h"
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

QString HarnessTraits::modelCacheKey(const QString &providerId) const
{
    const QString provider =
        providerId.isEmpty() ? QStringLiteral("direct") : providerId;
    return id + QLatin1Char('@') + provider + QStringLiteral("Opt-model");
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
    t.transcriptPreview = true; // claude keeps the on-disk session store
    // --append-system-prompt and --agents, both verified in print mode.
    t.systemPrompt = t.customSubagents = true;
    // --fallback-model / --disallowedTools / --add-dir, verified on 2.1.220.
    t.fallbackModels = t.disallowedTools = t.addDirs = true;
    // Models are discovered live (`claude -p /model`, or a routed provider's
    // /v1/models); mode/effort stay static vocabularies below.
    t.modelPicker = QStringLiteral("discovered");
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
    t.sessionBrowse = true; // session/list via a one-shot probe (mirrors the Go adapter)
    // transcriptPreview stays false: kimi's transcript is the core's event log,
    // not a previewable/forgettable on-disk store. systemPrompt and
    // customSubagents stay false too: `kimi acp` takes no system-prompt channel
    // and resolves subagents from a compiled-in set (see harness_kimi.go).
    // The P6 sweep is off for the same reason: `kimi acp` takes no
    // harness-shaping flags, and ACP session/new carries one cwd.
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
    t.transcriptPreview = o.value(QStringLiteral("transcriptPreview")).toBool();
    t.systemPrompt = o.value(QStringLiteral("systemPrompt")).toBool();
    t.customSubagents = o.value(QStringLiteral("customSubagents")).toBool();
    t.fallbackModels = o.value(QStringLiteral("fallbackModels")).toBool();
    t.disallowedTools = o.value(QStringLiteral("disallowedTools")).toBool();
    t.addDirs = o.value(QStringLiteral("addDirs")).toBool();
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

// recommendedFrom picks the short "recommended" group out of a full catalogue
// (entries are "value|name"). For Claude direct the live aliases already map to
// the newest model per family, so the families themselves are the recommendation.
// For a routed provider the operator's configured slot models are the picks.
QStringList recommendedFrom(const QString &harnessId, const QString &providerId,
                            const QStringList &entries)
{
    QMap<QString, QString> byValue; // value -> "value|name"
    for (const QString &e : entries) {
        const int bar = e.indexOf(QLatin1Char('|'));
        byValue.insert(bar >= 0 ? e.left(bar) : e, e);
    }
    const bool isDirect =
        providerId.isEmpty() || providerId == ProviderStore::directId();
    QStringList out;
    if (isDirect) {
        // Claude's family aliases (opus/sonnet/haiku/fable/best) each resolve to
        // the newest model; other direct engines (kimi) expose no families, so
        // the full list is the picker and the recommended group stays empty.
        if (harnessId == QLatin1String("claude")) {
            for (const QString &alias :
                 {QStringLiteral("opus"), QStringLiteral("sonnet"), QStringLiteral("haiku"),
                  QStringLiteral("fable"), QStringLiteral("best")}) {
                const auto it = byValue.constFind(alias);
                if (it != byValue.constEnd()) {
                    out << *it;
                }
            }
        }
        return out;
    }
    const ProviderProfile p = ProviderStore::byId(providerId);
    for (const QString &slot : ProviderStore::modelSlots()) {
        const QString id = p.models.value(slot);
        if (id.isEmpty()) {
            continue;
        }
        const auto it = byValue.constFind(id);
        out << (it != byValue.constEnd() ? *it : id + QLatin1Char('|') + id);
    }
    out.removeDuplicates();
    return out;
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

void HarnessRegistry::applyDiscoveredOptions(const QString &harnessId,
                                             const QJsonArray &configOptions)
{
    persistDiscoveredOptions(harnessId, configOptions);
    // Mirror the discoverOptions probe path (ensureDiscovered): notify so the
    // roster "+ New Agent" menu and any open pickers rebuild from the fresh
    // cache instead of only picking it up on the next app start.
    emit changed();
}

void HarnessRegistry::discoverModels(CoreClient *core, const QString &harnessId,
                                     const QString &providerId, const QJsonObject &provider)
{
    if (!core || !core->isConnected()) {
        return;
    }
    const QString flight = harnessId + QLatin1Char('@') + providerId;
    if (m_discoveringModels.contains(flight)) {
        return;
    }
    m_discoveringModels.insert(flight);
    const QString cacheKey = traits(harnessId).modelCacheKey(providerId);
    QJsonObject params{{QStringLiteral("backend"), harnessId}};
    if (!provider.isEmpty()) {
        params.insert(QStringLiteral("provider"), provider);
    }
    core->call(
        QStringLiteral("agent.discoverModels"), params,
        [this, flight, cacheKey](const QJsonObject &result, const QJsonObject &error) {
            m_discoveringModels.remove(flight);
            if (!error.isEmpty()) {
                return; // best-effort — keep the last cache
            }
            const QJsonArray models = result.value(QStringLiteral("models")).toArray();
            if (models.isEmpty()) {
                return; // never blank a populated picker (throttled/offline/no key)
            }
            QStringList entries;
            for (const QJsonValue &mv : models) {
                const QJsonObject m = mv.toObject();
                const QString value = m.value(QStringLiteral("value")).toString();
                if (value.isEmpty()) {
                    continue;
                }
                QString name = m.value(QStringLiteral("name")).toString();
                entries << value + QLatin1Char('|') + (name.isEmpty() ? value : name);
            }
            if (entries.isEmpty()) {
                return;
            }
            KSharedConfig::openConfig()
                ->group(QStringLiteral("Agent"))
                .writeEntry(cacheKey, entries);
            emit changed();
        },
        this);
}

void HarnessRegistry::discoverAll(CoreClient *core)
{
    if (!core || !core->isConnected()) {
        return;
    }
    const QList<ProviderProfile> providers = ProviderStore::load();
    for (const HarnessTraits &t : all()) {
        // Non-model discovered vocabularies (kimi thinking/mode, and kimi's own
        // model list via configOptions). No-op for static vocabularies.
        ensureDiscovered(core, t.id);
        // Direct model list — Claude answers via `claude -p /model`; harnesses
        // without HTTP/exec model discovery return an empty list and write nothing.
        discoverModels(core, t.id, ProviderStore::directId(), QJsonObject{});
        if (!t.providerRouting) {
            continue;
        }
        for (const ProviderProfile &p : providers) {
            if (!p.routed()) {
                continue;
            }
            discoverModels(core, t.id, p.id, ProviderStore::toJson(p));
        }
    }
}

HarnessRegistry::ModelChoices
HarnessRegistry::modelChoices(const QString &harnessId, const QString &providerId) const
{
    const HarnessTraits t = traits(harnessId);
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    QStringList entries = cfg.readEntry(t.modelCacheKey(providerId), QStringList());
    if (entries.isEmpty()) {
        // Legacy per-harness cache (kimi, or a Claude cache written before the
        // per-provider split) so nothing regresses before the first probe lands.
        entries = cfg.readEntry(t.optionKey(QStringLiteral("model")), QStringList());
    }
    ModelChoices out;
    out.all = entries;
    out.recommended = recommendedFrom(harnessId, providerId, entries);
    return out;
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
