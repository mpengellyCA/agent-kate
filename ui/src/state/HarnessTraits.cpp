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

QString HarnessTraits::modelEffortsKey(const QString &providerId) const
{
    return modelCacheKey(providerId) + QStringLiteral("-efforts");
}

QString HarnessTraits::defaultPermissionMode() const
{
    for (const QString &preferred :
         {QStringLiteral("acceptEdits"), QStringLiteral("default")}) {
        if (permissionModes.contains(preferred)) {
            return preferred;
        }
    }
    return permissionModes.value(0);
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
    // `claude --print --resume` runs a pass over a dormant session and
    // session.ReadTranscript reads its on-disk transcript, so the cold path is
    // real here. LOCKSTEP with harness_claude.go Capabilities().ColdCompact.
    t.coldCompact = true;
    t.providerRouting = t.cowork = true;
    t.usageReporting = t.sessionBrowse = true;
    t.transcriptPreview = true; // claude keeps the on-disk session store
    // --append-system-prompt and --agents, both verified in print mode.
    t.systemPrompt = t.customSubagents = true;
    // --fallback-model / --disallowedTools / --add-dir, verified on 2.1.220.
    t.fallbackModels = t.disallowedTools = t.addDirs = true;
    // --strict-mcp-config / --max-budget-usd, verified in `claude -p --help`.
    t.strictMcpConfig = t.costBudget = true;
    // set_max_thinking_tokens is accepted mid-session and the effort tiers are
    // thinking-token budgets underneath, so the quick menu changes effort on a
    // running thread instead of asking for a relaunch. LOCKSTEP with
    // core/cmd/akcore/harness_claude.go Capabilities().EffortLive.
    t.effortLive = true;
    t.subagentTranscripts = true; // subagents/agent-<id>.jsonl beside the session
    // The reload_skills control request on the live session's stdin, so a skill
    // installed mid-session lands without a relaunch. LOCKSTEP with
    // core/cmd/akcore/harness_claude.go Capabilities().SkillReload.
    t.skillReload = true;
    // Models are discovered live (`claude -p /model`, or a routed provider's
    // /v1/models); mode/effort stay static vocabularies below.
    t.modelPicker = QStringLiteral("discovered");
    // The modes claude 2.1.220 accepts AND HONORS in print mode, in the
    // engine's own order. LOCKSTEP with harness_claude.go
    // Capabilities().PermissionModes, which replaces this list once
    // agent.capabilities answers — a fallback ordered differently would
    // reshuffle the picker on connect. ORDER IS LOAD-BEARING there
    // (permissiveModes() takes the LAST entry as the unattended end), so
    // bypassPermissions stays last with dontAsk just short of it. `manual` is
    // deliberately absent: the flag is accepted and then silently downgraded to
    // "default" in the init event, so offering it would promise supervision the
    // session does not have (see the Go comment for the re-probe recipe). No
    // picker may treat position 0 as the default: defaultPermissionMode() names
    // it instead, which is what keeps acceptEdits this app's default.
    t.permissionModes = {QStringLiteral("acceptEdits"), QStringLiteral("default"),
                         QStringLiteral("plan"), QStringLiteral("auto"),
                         QStringLiteral("dontAsk"),
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
    // `/compact` sent as prompt text really compacts the live session (probed
    // on 0.30.0), so the compaction affordances belong on kimi too. It is
    // HOT-ONLY — coldCompact stays false (LOCKSTEP with harness_kimi.go), so
    // the dormant-thread flows skip the summary-recovery prompt entirely
    // instead of offering cold models the core refuses.
    t.compaction = true;
    // One wire log per subagent under <session-dir>/agents/<id>/ (probed on 0.30.0).
    t.subagentTranscripts = true;
    // skillReload stays false: ACP has no reload-skills request and `kimi acp`
    // resolves its skill directories once, at session/new — so a skill the
    // human installs while a kimi thread runs needs a restart to take effect.
    // The core says so in the thread's own transcript rather than skipping it
    // in silence (audit F50). LOCKSTEP with harness_kimi.go.
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
    t.coldCompact = o.value(QStringLiteral("coldCompact")).toBool();
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
    t.strictMcpConfig = o.value(QStringLiteral("strictMcpConfig")).toBool();
    t.costBudget = o.value(QStringLiteral("costBudget")).toBool();
    t.subagentTranscripts = o.value(QStringLiteral("subagentTranscripts")).toBool();
    t.skillReload = o.value(QStringLiteral("skillReload")).toBool();
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
    const QString effortsKey = traits(harnessId).modelEffortsKey(providerId);
    QJsonObject params{{QStringLiteral("backend"), harnessId}};
    if (!provider.isEmpty()) {
        params.insert(QStringLiteral("provider"), provider);
    }
    core->call(
        QStringLiteral("agent.discoverModels"), params,
        [this, flight, cacheKey, effortsKey](const QJsonObject &result,
                                             const QJsonObject &error) {
            m_discoveringModels.remove(flight);
            if (!error.isEmpty()) {
                return; // best-effort — keep the last cache
            }
            const QJsonArray models = result.value(QStringLiteral("models")).toArray();
            if (models.isEmpty()) {
                return; // never blank a populated picker (throttled/offline/no key)
            }
            QStringList entries;
            QStringList effortEntries;
            for (const QJsonValue &mv : models) {
                const QJsonObject m = mv.toObject();
                const QString value = m.value(QStringLiteral("value")).toString();
                if (value.isEmpty()) {
                    continue;
                }
                QString name = m.value(QStringLiteral("name")).toString();
                entries << value + QLatin1Char('|') + (name.isEmpty() ? value : name);
                // Per-model effort support, when the engine reported any. A
                // model the engine said nothing about contributes no row, which
                // modelEfforts() reads as "every tier".
                QStringList efforts;
                const QJsonArray ea = m.value(QStringLiteral("efforts")).toArray();
                for (const QJsonValue &ev : ea) {
                    const QString e = ev.toString();
                    if (!e.isEmpty()) {
                        efforts << e;
                    }
                }
                if (!efforts.isEmpty()) {
                    effortEntries << value + QLatin1Char('|')
                            + efforts.join(QLatin1Char(','));
                }
            }
            if (entries.isEmpty()) {
                return;
            }
            KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
            cfg.writeEntry(cacheKey, entries);
            // Rewritten (not merged) with every catalogue: a model that lost its
            // effort claim must lose the stale one too, or the picker would keep
            // greying tiers on evidence the engine has withdrawn.
            cfg.writeEntry(effortsKey, effortEntries);
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

QStringList HarnessRegistry::modelEfforts(const QString &harnessId,
                                          const QString &providerId,
                                          const QString &modelValue) const
{
    if (modelValue.isEmpty()) {
        return {};
    }
    const QStringList rows = KSharedConfig::openConfig()
                                 ->group(QStringLiteral("Agent"))
                                 .readEntry(traits(harnessId).modelEffortsKey(providerId),
                                            QStringList());
    for (const QString &row : rows) {
        const int bar = row.indexOf(QLatin1Char('|'));
        if (bar > 0 && row.left(bar) == modelValue) {
            return row.mid(bar + 1).split(QLatin1Char(','), Qt::SkipEmptyParts);
        }
    }
    return {}; // no claim for this model — every tier stays offered
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
    if (value == QLatin1String("manual")) {
        return i18n("Manual — confirm every action");
    }
    if (value == QLatin1String("dontAsk")) {
        return i18n("Don't ask — proceed without prompts");
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
