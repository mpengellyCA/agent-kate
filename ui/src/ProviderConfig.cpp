#include "ProviderConfig.h"

#include <QJsonObject>
#include <QJsonArray>
#include <QJsonDocument>
#include <QDir>
#include <QSaveFile>
#include <QStandardPaths>

#include <KConfigGroup>
#include <KLocalizedString>
#include <KSharedConfig>

namespace {

const char *kGroup = "Providers";

const QStringList &slotList()
{
    static const QStringList s{QStringLiteral("main"), QStringLiteral("opus"),
                               QStringLiteral("sonnet"), QStringLiteral("haiku"),
                               QStringLiteral("subagent")};
    return s;
}

// seedPresets writes the built-in Fireworks and OpenRouter starting points the
// first time the Providers group is created. The user edits keys/models from
// there; nothing here is secret. Model ids are provider-specific starting points
// (Fire Pass routes every alias to one router; OpenRouter slugs come from the
// provider's catalog) and are meant to be edited.
void seedPresets(KConfigGroup &g)
{
    struct Preset {
        const char *id;
        const char *name;
        const char *baseUrl;
        const char *envVar;
        const char *main; // applied to every slot as the starting point
    };
    const Preset presets[] = {
        {"fireworks", "Fireworks (Fire Pass)", "https://api.fireworks.ai/inference",
         "FIREWORKS_API_KEY", "accounts/fireworks/routers/glm-5p2-fast"},
        {"openrouter", "OpenRouter", "https://openrouter.ai/api/v1",
         "OPENROUTER_API_KEY", "z-ai/glm-4.6"},
    };
    QStringList ids;
    for (const auto &p : presets) {
        const QString id = QString::fromLatin1(p.id);
        ids << id;
        KConfigGroup pg = g.group(id);
        pg.writeEntry("name", QString::fromLatin1(p.name));
        pg.writeEntry("baseUrl", QString::fromLatin1(p.baseUrl));
        pg.writeEntry("envVar", QString::fromLatin1(p.envVar));
        pg.writeEntry("builtin", true);
        for (const QString &slot : slotList()) {
            pg.writeEntry("model_" + slot, QString::fromLatin1(p.main));
        }
    }
    g.writeEntry("ids", ids);
    g.sync();
}

} // namespace

namespace ProviderStore {

const QStringList &modelSlots() { return slotList(); }

QString directId() { return QStringLiteral("claude-direct"); }

QList<ProviderProfile> load()
{
    QList<ProviderProfile> out;

    ProviderProfile direct;
    direct.id = directId();
    direct.name = QStringLiteral("Claude (direct)");
    direct.builtin = true;
    out.append(direct);

    KConfigGroup g = KSharedConfig::openConfig()->group(QString::fromLatin1(kGroup));
    if (!g.hasKey("ids")) {
        seedPresets(g);
    }
    const QStringList ids = g.readEntry("ids", QStringList());
    for (const QString &id : ids) {
        KConfigGroup pg = g.group(id);
        ProviderProfile p;
        p.id = id;
        p.name = pg.readEntry("name", id);
        p.baseUrl = pg.readEntry("baseUrl", QString());
        p.envVar = pg.readEntry("envVar", QString());
        p.builtin = pg.readEntry("builtin", false);
        for (const QString &slot : slotList()) {
            const QString m = pg.readEntry("model_" + slot, QString());
            if (!m.isEmpty()) {
                p.models.insert(slot, m);
            }
        }
        out.append(p);
    }
    return out;
}

void save(const QList<ProviderProfile> &profiles)
{
    KConfigGroup g = KSharedConfig::openConfig()->group(QString::fromLatin1(kGroup));
    const QStringList oldIds = g.readEntry("ids", QStringList());

    QStringList ids;
    for (const ProviderProfile &p : profiles) {
        if (p.id == directId() || p.id.isEmpty()) {
            continue; // the direct sentinel is synthesized, never stored
        }
        ids << p.id;
        KConfigGroup pg = g.group(p.id);
        pg.writeEntry("name", p.name);
        pg.writeEntry("baseUrl", p.baseUrl);
        pg.writeEntry("envVar", p.envVar);
        pg.writeEntry("builtin", p.builtin);
        for (const QString &slot : slotList()) {
            pg.writeEntry("model_" + slot, p.models.value(slot));
        }
    }
    // Drop config groups (and stored keys) for profiles the user removed.
    for (const QString &oldId : oldIds) {
        if (!ids.contains(oldId)) {
            g.deleteGroup(oldId);
        }
    }
    g.writeEntry("ids", ids);
    g.sync();

    // akcore owns runtime provider bindings. Give it a key-free mirror of the
    // selected profile metadata; credentials come only from the named
    // environment and are resolved inside the core process.
    QJsonArray encoded;
    for (const ProviderProfile &p : profiles) {
        if (!p.routed() || p.id.isEmpty()) {
            continue;
        }
        QJsonObject profile{{QStringLiteral("id"), p.id},
                            {QStringLiteral("name"), p.name},
                            {QStringLiteral("baseUrl"), p.baseUrl.trimmed()},
                            {QStringLiteral("envVar"), p.envVar}};
        QJsonObject models;
        for (auto it = p.models.constBegin(); it != p.models.constEnd(); ++it) {
            if (!it.value().trimmed().isEmpty()) {
                models.insert(it.key(), it.value().trimmed());
            }
        }
        profile.insert(QStringLiteral("models"), models);
        encoded.append(profile);
    }
    const QString directory = QStandardPaths::writableLocation(
        QStandardPaths::GenericConfigLocation) + QStringLiteral("/agentkate");
    if (QDir().mkpath(directory)) {
        QSaveFile mirror(directory + QStringLiteral("/providers.json"));
        if (mirror.open(QIODevice::WriteOnly)) {
            mirror.write(QJsonDocument(QJsonObject{{QStringLiteral("profiles"), encoded}})
                             .toJson(QJsonDocument::Compact));
            mirror.commit();
        }
    }
}

ProviderProfile byId(const QString &id)
{
    if (id.isEmpty() || id == directId()) {
        ProviderProfile direct;
        direct.id = directId();
        direct.name = QStringLiteral("Claude (direct)");
        direct.builtin = true;
        return direct;
    }
    const QList<ProviderProfile> all = load();
    for (const ProviderProfile &p : all) {
        if (p.id == id) {
            return p;
        }
    }
    return byId(directId());
}

bool walletAvailable()
{
    return false;
}

bool setKey(const QString &id, const QString &keyValue)
{
    Q_UNUSED(id);
    Q_UNUSED(keyValue);
    return false;
}

bool hasStoredKey(const QString &id)
{
    Q_UNUSED(id);
    return false;
}

bool keyResolvable(const ProviderProfile &p)
{
    if (!p.routed()) {
        return true; // Claude-direct: the CLI brings its own credential
    }
    // akcore resolves credentials in its own process, so the launchable
    // credential source is the profile's named environment variable.
    if (!p.envVar.isEmpty()
        && !qgetenv(p.envVar.toLocal8Bit().constData()).isEmpty()) {
        return true;
    }
    return false;
}

QString pickerLabel(const ProviderProfile &p)
{
    if (keyResolvable(p)) {
        return p.name;
    }
    return i18nc("provider entry in an engine picker with no credential stored",
                 "%1 (no API key set)", p.name);
}

} // namespace ProviderStore
