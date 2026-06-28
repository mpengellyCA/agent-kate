#include "ProviderConfig.h"

#include <QJsonObject>

#include <KConfigGroup>
#include <KSharedConfig>

#ifdef AK_HAVE_KWALLET
#include <KWallet>
#endif

namespace {

const char *kGroup = "Providers";
const char *kWalletFolder = "agentkate";

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

#ifdef AK_HAVE_KWALLET
// walletHandle returns a cached, open wallet positioned on our folder, or
// nullptr when KWallet is disabled / unavailable. Opening is synchronous and may
// prompt the user to unlock on first use.
KWallet::Wallet *walletHandle()
{
    static KWallet::Wallet *w = nullptr;
    if (w && w->isOpen()) {
        return w;
    }
    delete w;
    w = nullptr;
    if (!KWallet::Wallet::isEnabled()) {
        return nullptr;
    }
    w = KWallet::Wallet::openWallet(KWallet::Wallet::NetworkWallet(), 0,
                                    KWallet::Wallet::Synchronous);
    if (!w) {
        return nullptr;
    }
    const QString folder = QString::fromLatin1(kWalletFolder);
    if (!w->hasFolder(folder)) {
        w->createFolder(folder);
    }
    w->setFolder(folder);
    return w;
}
#endif

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
            setKey(oldId, QString());
        }
    }
    g.writeEntry("ids", ids);
    g.sync();
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
#ifdef AK_HAVE_KWALLET
    return walletHandle() != nullptr;
#else
    return false;
#endif
}

bool setKey(const QString &id, const QString &keyValue)
{
#ifdef AK_HAVE_KWALLET
    KWallet::Wallet *w = walletHandle();
    if (!w) {
        return false;
    }
    if (keyValue.isEmpty()) {
        if (w->hasEntry(id)) {
            return w->removeEntry(id) == 0;
        }
        return true;
    }
    return w->writePassword(id, keyValue) == 0;
#else
    Q_UNUSED(id);
    Q_UNUSED(keyValue);
    return false;
#endif
}

QString key(const ProviderProfile &p)
{
#ifdef AK_HAVE_KWALLET
    if (KWallet::Wallet *w = walletHandle()) {
        QString v;
        if (w->hasEntry(p.id) && w->readPassword(p.id, v) == 0 && !v.isEmpty()) {
            return v;
        }
    }
#endif
    if (!p.envVar.isEmpty()) {
        const QByteArray e = qgetenv(p.envVar.toLocal8Bit().constData());
        if (!e.isEmpty()) {
            return QString::fromLocal8Bit(e);
        }
    }
    return QString();
}

bool hasStoredKey(const QString &id)
{
#ifdef AK_HAVE_KWALLET
    if (KWallet::Wallet *w = walletHandle()) {
        return w->hasEntry(id);
    }
#endif
    Q_UNUSED(id);
    return false;
}

QJsonObject toJson(const ProviderProfile &p)
{
    QJsonObject o;
    if (!p.routed()) {
        return o; // Claude direct — caller omits the field entirely
    }
    o.insert(QStringLiteral("id"), p.id);
    o.insert(QStringLiteral("name"), p.name);
    o.insert(QStringLiteral("baseUrl"), p.baseUrl.trimmed());
    if (!p.envVar.isEmpty()) {
        o.insert(QStringLiteral("envVar"), p.envVar);
    }
    const QString k = key(p);
    if (!k.isEmpty()) {
        o.insert(QStringLiteral("authToken"), k);
    }
    QJsonObject models;
    for (auto it = p.models.constBegin(); it != p.models.constEnd(); ++it) {
        if (!it.value().trimmed().isEmpty()) {
            models.insert(it.key(), it.value().trimmed());
        }
    }
    if (!models.isEmpty()) {
        o.insert(QStringLiteral("models"), models);
    }
    return o;
}

} // namespace ProviderStore
