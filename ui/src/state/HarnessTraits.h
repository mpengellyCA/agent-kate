// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QHash>
#include <QJsonObject>
#include <QList>
#include <QObject>
#include <QSet>
#include <QString>
#include <QStringList>

class CoreClient;
class QJsonArray;
class QJsonObject;

// HarnessTraits is the UI projection of one HarnessDescriptor returned by
// harness.list. It deliberately has no built-in values: a current core
// descriptor is the authority for every affordance shown by the UI.
struct HarnessTraits {
    QString id;
    QString displayName;
    QString badge; // roster subtitle prefix; empty = the unmarked default engine

    bool fork = false;
    // Compaction splits in two under the descriptor contract. compaction: the thread can be
    // compacted at all — it gates the strategy/status RPCs and the HOT
    // (in-session) mechanism. coldCompact: the harness can also compact a
    // DORMANT thread and hand back summary text. Kimi has the first and not
    // the second, so every flow that produces a summary before the session is
    // live — the pre-resume recovery prompt, the cold "Summarize now" entries —
    // must gate on coldCompact, or it offers choices the core refuses.
    bool compaction = false;
    bool coldCompact = false;
    bool promote = false;
    bool providerRouting = false;
    // providerRegistry: the ENGINE keeps its own persistent provider registry
    // in its home directory (kimi's `kimi provider` family, plan 26), whose
    // credentials the engine holds itself — a different mechanism from
    // providerRouting's per-launch env injection, hence a second flag. Gates
    // the "Kimi provider registry" section of ProvidersDialog.
    bool providerRegistry = false;
    bool cowork = false;
    bool effortLive = false; // thinking effort adjustable mid-session
    bool usageReporting = false;
    bool sessionBrowse = false;
    bool transcriptPreview = false; // previewable/forgettable on-disk transcript
    // Persona channels. systemPrompt: the engine runs caller-supplied persona
    // text alongside its own system prompt. customSubagents: it accepts
    // caller-defined subagent profiles for the session. Where either is false
    // the core reports the request as a downgrade instead of emulating it, and
    // the persona belongs in the opening message.
    bool systemPrompt = false;
    bool customSubagents = false;
    // The launch-option sweep (plan 16 P6). Each gates one advanced field in
    // the New Agent dialog — an engine that cannot express the option is not
    // offered it, which is why the core never has to report these as
    // downgrades on a UI-driven start.
    bool fallbackModels = false;  // ordered model fallbacks
    bool disallowedTools = false; // per-session tool deny-list
    bool addDirs = false;         // extra reachable directories
    // The control-channel sweep. strictMcpConfig: the thread can be isolated
    // from the human's globally-configured MCP servers. costBudget: the engine
    // enforces a per-session spend ceiling itself.
    bool strictMcpConfig = false;
    bool costBudget = false;
    // subagentTranscripts: the engine writes a per-subagent conversation file
    // the UI can tail (the panel's "Helpers" menu).
    bool subagentTranscripts = false;
    // skillReload: a RUNNING session can be told to re-read its skill
    // directories, so a skill installed from the catalogue reaches an agent
    // that is already working. False = the session read its skills at start
    // and only a restart changes that, which the core says out loud in the
    // thread's own transcript when a reload skips it (audit F50).
    bool skillReload = false;
    // commands: this harness can supply its native command catalogue for the
    // composer's slash-command completion. Raw slash-prefixed text is not a
    // substitute for a catalogue.
    bool commands = false;
    bool continuation = false; // host-owned, bounded continuation loop

    // Values mapped from the current catalogue. Empty means the catalogue has
    // not declared that setting, never that the UI should invent a fallback.
    QStringList permissionModes;
    QStringList efforts;

    // The mode a picker lands on when nothing has been chosen yet.
    // permissionModes is the engine's own wire vocabulary in the CLI's order,
    // so its first entry is whatever that engine happens to list first — never
    // a UI default. Prefers "acceptEdits", then "default", then the first
    // listed, so a reordering upstream cannot silently re-arm a different
    // supervision level for fresh profiles.
    QString defaultPermissionMode() const;

    // Per-harness KConfig keys retained for user preferences. Catalogue data
    // itself is held only in the revisioned in-memory catalogue cache.
    QString stickyModeKey() const;
    QString stickyEffortKey() const;
};

// HarnessRegistry holds descriptors and revisioned catalogues from the core.
// It is empty until harness.list succeeds; callers must disable actions that
// require a descriptor while it is empty.
class HarnessRegistry : public QObject
{
    Q_OBJECT
public:
    static HarnessRegistry *self();

    // Fetch (or re-fetch) descriptors from the core; emits changed() when the
    // registered descriptor set changes.
    void fetch(CoreClient *core);

    // Compatibility entry point for callers that need a catalogue. It only
    // requests harness.catalog; it never probes a native protocol directly.
    void ensureDiscovered(CoreClient *core, const QString &harnessId);

    // Ensure the selected engine/provider's live model catalogue is cached.
    // This is deliberately separate from ensureDiscovered(): Kimi exposes its
    // models in ACP configOptions, while Claude Code and Codex expose them via
    // their own catalogue APIs. A picker must ask for both paths when it opens
    // or switches engines; relying only on the startup refresh races the
    // capability fetch and leaves a just-opened New Agent dialog blank.
    void ensureModels(CoreClient *core, const QString &harnessId,
                      const QString &providerId = QString());

    // A model catalogue split into a short "recommended" group and the full
    // live list, both as "value|name" entries (the picker format).
    struct ModelChoices {
        QStringList recommended;
        QStringList all;
    };

    // Query each registered harness's direct catalogue at connect. Fully
    // non-blocking — a failed or empty request leaves the cache empty.
    void discoverAll(CoreClient *core);

    // Cached model choices for a harness + provider (providerId empty = direct),
    // recommended group first. Empty lists when nothing has been discovered yet.
    ModelChoices modelChoices(const QString &harnessId, const QString &providerId) const;
    // The reasoning-effort tiers one model supports, as last discovered. An
    // EMPTY list means the engine said nothing about this model and every tier
    // in the harness vocabulary is offered — it never means "no tiers".
    QStringList modelEfforts(const QString &harnessId, const QString &providerId,
                             const QString &modelValue) const;
    // Traits for a current descriptor. Unknown ids return an empty projection.
    HarnessTraits traits(const QString &id) const;
    // All harnesses in the core's registration order — the engine-picker order.
    QList<HarnessTraits> all() const;

    // Test fixture seam. Production descriptors enter only through
    // harness.list; tests supply explicit descriptor projections rather than
    // depending on an offline fallback.
    void replaceDescriptorsForTest(const QList<HarnessTraits> &descriptors);

    // Human labels for the known static vocabulary values; unknown values
    // fall back to the raw token so a newer core still renders.
    static QString modeLabel(const QString &value);
    static QString effortLabel(const QString &value);

Q_SIGNALS:
    void changed();

private:
    HarnessRegistry();

    // Fetch one revisioned Harness Linkage catalogue. The cache is in-memory
    // and keyed by harness/provider/revision; native config never enters it.
    void discoverModels(CoreClient *core, const QString &harnessId,
                        const QString &providerId, const QJsonObject &provider);

    QHash<QString, HarnessTraits> m_traits;
    QHash<QString, QJsonObject> m_catalogues;
    QStringList m_order;
    QSet<QString> m_discoveringModels; // "harness@provider" keys with a models probe in flight
};
