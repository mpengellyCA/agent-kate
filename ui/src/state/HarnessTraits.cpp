// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "HarnessTraits.h"
#include "../ProviderConfig.h"
#include "../ipc/CoreClient.h"

#include <KLocalizedString>

#include <QJsonArray>
#include <QJsonObject>

QString HarnessTraits::stickyModeKey() const
{
    return id + QStringLiteral("Mode");
}

QString HarnessTraits::stickyEffortKey() const
{
    return id + QStringLiteral("Thinking");
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
HarnessTraits fromJson(const QJsonObject &o)
{
    HarnessTraits t;
    t.id = o.value(QStringLiteral("id")).toString();
    t.displayName = o.value(QStringLiteral("displayName")).toString();
    t.badge = o.value(QStringLiteral("badge")).toString();
    QSet<QString> operations;
    const QJsonArray declared = o.value(QStringLiteral("operations")).toArray();
    for (const QJsonValue &value : declared) {
        operations.insert(value.toObject().value(QStringLiteral("kind")).toString());
    }
    const auto has = [&operations](const char *kind) {
        return operations.contains(QLatin1String(kind));
    };
    t.fork = has("fork");
    t.compaction = has("compaction");
    t.coldCompact = has("coldCompaction");
    t.promote = has("promote");
    t.providerRouting = has("providerRouting");
    t.providerRegistry = has("providerRegistry");
    t.cowork = has("cowork");
    t.usageReporting = has("usageReporting");
    t.sessionBrowse = has("sessionBrowse");
    t.transcriptPreview = has("transcriptPreview");
    t.systemPrompt = has("systemPrompt");
    t.customSubagents = has("customSubagents");
    t.fallbackModels = has("fallbackModels");
    t.disallowedTools = has("disallowedTools");
    t.addDirs = has("addDirectories");
    t.strictMcpConfig = has("strictMcpConfig");
    t.costBudget = has("costBudget");
    t.subagentTranscripts = has("subagentTranscripts");
    t.skillReload = has("skillReload");
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
    // Deliberately empty. A launcher has no harness data until the current
    // core returns harness.list; static UI guesses are not a compatibility
    // path for this atomic contract release.
}

void HarnessRegistry::fetch(CoreClient *core)
{
    if (!core || !core->isConnected()) {
        return;
    }
    core->call(
        QStringLiteral("harness.list"), QJsonObject{},
        [this, core](const QJsonObject &result, const QJsonObject &error) {
            if (!error.isEmpty()) {
                return; // no descriptor means launch remains disabled
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
            // The startup caller deliberately starts capability and catalogue
            // discovery together so neither delays the first frame. Re-run it
            // now that the core's actual registry has arrived: without this,
            // a newly registered harness can miss the only catalogue pass and
            // its picker remains at "Use my default" until restart.
            discoverAll(core);
        },
        this);
}

void HarnessRegistry::ensureDiscovered(CoreClient *core, const QString &harnessId)
{
    ensureModels(core, harnessId);
}

void HarnessRegistry::ensureModels(CoreClient *core, const QString &harnessId,
                                   const QString &providerId)
{
    const HarnessTraits t = traits(harnessId);
    if (t.id.isEmpty()) {
        return;
    }
    const QString provider = providerId.isEmpty() ? ProviderStore::directId() : providerId;
    const QString key = t.id + QLatin1Char('@') + provider;
    if (m_catalogues.contains(key)) {
        return;
    }
    discoverModels(core, t.id, provider, QJsonObject{});
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
    Q_UNUSED(provider)
    QJsonObject params{{QStringLiteral("harnessId"), harnessId}};
    if (providerId != ProviderStore::directId()) {
        params.insert(QStringLiteral("providerId"), providerId);
    }
    core->call(
        QStringLiteral("harness.catalog"), params,
        [this, flight](const QJsonObject &result,
                                             const QJsonObject &error) {
            m_discoveringModels.remove(flight);
            if (!error.isEmpty()) {
                return; // best-effort — keep the last cache
            }
            if (result.value(QStringLiteral("revision")).toString().isEmpty()) {
                return;
            }
            m_catalogues.insert(flight, result);
            const QString harnessId = flight.section(QLatin1Char('@'), 0, 0);
            auto descriptor = m_traits.find(harnessId);
            if (descriptor != m_traits.end()) {
                descriptor->permissionModes.clear();
                descriptor->efforts.clear();
                descriptor->effortLive = false;
                for (const QJsonValue &settingValue : result.value(QStringLiteral("settings")).toArray()) {
                    const QJsonObject setting = settingValue.toObject();
                    const QString key = setting.value(QStringLiteral("key")).toString();
                    QStringList *choices = nullptr;
                    if (key == QLatin1String("permissionMode")) {
                        choices = &descriptor->permissionModes;
                    } else if (key == QLatin1String("reasoningEffort")) {
                        choices = &descriptor->efforts;
                        descriptor->effortLive = setting.value(QStringLiteral("timing")).toString()
                            == QLatin1String("live");
                    }
                    if (!choices) {
                        continue;
                    }
                    for (const QJsonValue &choiceValue : setting.value(QStringLiteral("choices")).toArray()) {
                        const QString value = choiceValue.toObject().value(QStringLiteral("value")).toString();
                        if (!value.isEmpty()) {
                            *choices << value;
                        }
                    }
                }
            }
            emit changed();
        },
        this);
}

void HarnessRegistry::discoverAll(CoreClient *core)
{
    if (!core || !core->isConnected()) {
        return;
    }
    for (const HarnessTraits &t : all()) {
        // Provider identity is the only provider data this protocol carries.
        // The direct catalogue is always safe to refresh at connect.
        ensureModels(core, t.id, ProviderStore::directId());
    }
}

HarnessRegistry::ModelChoices
HarnessRegistry::modelChoices(const QString &harnessId, const QString &providerId) const
{
    ModelChoices out;
    const QString provider = providerId.isEmpty() ? ProviderStore::directId() : providerId;
    const QJsonObject catalogue = m_catalogues.value(harnessId + QLatin1Char('@') + provider);
    for (const QJsonValue &value : catalogue.value(QStringLiteral("models")).toArray()) {
        const QJsonObject model = value.toObject();
        const QString id = model.value(QStringLiteral("id")).toString();
        if (!id.isEmpty()) {
            const QString name = model.value(QStringLiteral("displayName")).toString();
            out.all << id + QLatin1Char('|') + (name.isEmpty() ? id : name);
        }
    }
    return out;
}

QStringList HarnessRegistry::modelEfforts(const QString &harnessId,
                                          const QString &providerId,
                                          const QString &modelValue) const
{
    if (modelValue.isEmpty()) {
        return {};
    }
    const QString provider = providerId.isEmpty() ? ProviderStore::directId() : providerId;
    const QJsonObject catalogue = m_catalogues.value(harnessId + QLatin1Char('@') + provider);
    for (const QJsonValue &value : catalogue.value(QStringLiteral("models")).toArray()) {
        const QJsonObject model = value.toObject();
        if (model.value(QStringLiteral("id")).toString() == modelValue) {
            QStringList efforts;
            for (const QJsonValue &effort : model.value(QStringLiteral("supportedReasoningEfforts")).toArray()) {
                efforts << effort.toString();
            }
            return efforts;
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
    return {};
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

void HarnessRegistry::replaceDescriptorsForTest(const QList<HarnessTraits> &descriptors)
{
    m_traits.clear();
    m_order.clear();
    m_catalogues.clear();
    for (const HarnessTraits &descriptor : descriptors) {
        if (descriptor.id.isEmpty()) {
            continue;
        }
        m_traits.insert(descriptor.id, descriptor);
        m_order << descriptor.id;
    }
    emit changed();
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
    if (value == QLatin1String("untrusted")) {
        return i18n("Ask before untrusted actions");
    }
    if (value == QLatin1String("on-request")) {
        return i18n("Ask when an action needs approval");
    }
    if (value == QLatin1String("never")) {
        return i18n("Never ask — proceed without prompts");
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
