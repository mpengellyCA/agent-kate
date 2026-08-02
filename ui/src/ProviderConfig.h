#pragma once

#include <QList>
#include <QMap>
#include <QString>

class QJsonObject;

// A configured third-party API provider profile. Non-secret fields persist in
// KConfig (group [Providers][<id>]); the API key is brokered separately through
// KWallet (or resolved from the named environment variable). An empty baseUrl is
// the built-in "Claude (direct)" sentinel — selecting it injects nothing and the
// agent talks to Anthropic exactly as before.
//
// See docs/plans/11-third-party-providers.md.
struct ProviderProfile {
    QString id;
    QString name;
    QString baseUrl;
    QString envVar; // optional: resolve the API key from this env var
    QMap<QString, QString> models; // slot ("main","opus","sonnet","haiku","subagent") -> model id
    bool builtin = false;          // a seeded preset (still editable; not deletable)

    bool routed() const { return !baseUrl.trimmed().isEmpty(); }
};

// ProviderStore loads/saves provider profiles and brokers their API keys. The
// secret never touches our config files: it lives in KWallet, or is read from
// the profile's environment variable at resolve time.
namespace ProviderStore {

// Model slots, in display order. Each maps to a Claude Code override variable
// (see core/internal/agent/provider.go).
const QStringList &modelSlots();

// The "Claude (direct)" sentinel id.
QString directId();

// All profiles, Claude-direct first then saved profiles in order. Seeds the
// built-in presets (Fireworks, OpenRouter) on first run.
QList<ProviderProfile> load();

// Persist the full set of (non-direct) profiles' non-secret fields, pruning any
// removed ones. API keys are written separately via setKey().
void save(const QList<ProviderProfile> &profiles);

// Look up one profile by id. An empty/unknown/`directId()` id yields the
// Claude-direct sentinel (routed() == false).
ProviderProfile byId(const QString &id);

// Secret brokerage. setKey stores to KWallet (empty value clears the entry);
// key() resolves from KWallet first, then the profile's envVar, then "".
bool walletAvailable();
bool setKey(const QString &id, const QString &key);
QString key(const ProviderProfile &p);
bool hasStoredKey(const QString &id);

// Can this profile actually start an agent right now? True for Claude-direct
// (it needs no key of ours) and for a routed profile whose key resolves from
// KWallet or its environment variable. The two seeded presets ship with no key,
// so on a fresh profile they were offered as engine choices that could only
// ever abort (audit F46) — every picker that lists routed providers must gate
// or annotate on this.
bool keyResolvable(const ProviderProfile &p);

// The name to show in an engine picker: the profile's own name, suffixed when
// no key resolves, so the choice is visibly dead before it is made.
QString pickerLabel(const ProviderProfile &p);

// Build the agent.start/agent.resume "provider" JSON for a profile, resolving
// the key. Returns an empty object for Claude-direct (the caller omits the field).
QJsonObject toJson(const ProviderProfile &p);

} // namespace ProviderStore
