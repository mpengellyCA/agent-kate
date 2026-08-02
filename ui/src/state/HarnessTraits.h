// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QHash>
#include <QList>
#include <QObject>
#include <QSet>
#include <QString>
#include <QStringList>

class CoreClient;
class QJsonArray;
class QJsonObject;

// HarnessTraits mirrors one harness's capability set from the core's
// agent.capabilities RPC (core/internal/harness). Every backend-specific
// affordance in the UI — which pickers exist, what they list, what the
// roster badge says — binds to these fields; nothing outside this file may
// compare a backend id to a string literal.
struct HarnessTraits {
    QString id = QStringLiteral("claude");
    QString displayName = QStringLiteral("Claude Code");
    QString badge; // roster subtitle prefix; empty = the unmarked default engine

    bool fork = false;
    // Compaction splits in two, exactly as the core's Capabilities does (see
    // core/internal/harness/harness.go). compaction: the thread can be
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
    // thread's own transcript when a reload skips it (audit F50). LOCKSTEP
    // with harness.Capabilities.SkillReload (core/internal/harness/harness.go).
    bool skillReload = false;

    QString modelPicker = QStringLiteral("tiers"); // "tiers" | "discovered"
    // Static vocabularies (wire values; the UI owns the human labels). Empty =
    // discovered per session from the CLI's configOptions and persisted under
    // optionKey().
    QStringList permissionModes;
    QStringList efforts;

    // The mode a picker lands on when nothing has been chosen yet.
    // permissionModes is the engine's own wire vocabulary in the CLI's order,
    // so its first entry is whatever that engine happens to list first — never
    // a UI default. Prefers "acceptEdits", then "default", then the first
    // listed, so a reordering upstream cannot silently re-arm a different
    // supervision level for fresh profiles.
    QString defaultPermissionMode() const;

    // Per-harness KConfig keys, [Agent] group. The default engine keeps its
    // historical key names so existing settings survive; other harnesses get
    // id-prefixed keys (kimi → "kimiMode"/"kimiThinking"/"kimiOpt-model").
    QString stickyModeKey() const;
    QString stickyEffortKey() const;
    QString optionKey(const QString &optionId) const; // discovered enumerations
    // Per-provider model-catalogue key ([Agent] group). Catalogues are
    // provider-scoped — Fireworks and OpenRouter are both the "claude" harness
    // but expose different models — so the model list can't share the
    // per-harness optionKey(). An empty providerId is the direct sentinel.
    QString modelCacheKey(const QString &providerId) const;
    // Sibling of modelCacheKey holding each model's supported reasoning-effort
    // tiers ("value|low,medium"), as reported by the engine's model discovery.
    // Separate from the catalogue entry so the "value|name" picker format —
    // which several callers split on — stays exactly as it was.
    QString modelEffortsKey(const QString &providerId) const;
};

// HarnessRegistry holds the traits for every harness the core registered,
// fetched once per connection from agent.capabilities. Until the fetch lands
// it serves built-in defaults that mirror the core's adapters, so the UI
// behaves identically offline or against an older core.
class HarnessRegistry : public QObject
{
    Q_OBJECT
public:
    static HarnessRegistry *self();

    // Fetch (or re-fetch) the capability sets from the core; emits changed()
    // when they differ from what is currently served.
    void fetch(CoreClient *core);

    // Ensure the discovered option enumerations (model / thinking / mode) for
    // a discovered-model harness are cached locally. No-op for "tiers"
    // harnesses, when the cache (Agent/<id>Opt-model) is already populated, or
    // while a probe is in flight. On success persists the same "value|name"
    // entries the init event writes, then emits changed() so open pickers
    // rebuild. A failed probe leaves today's placeholders (and is retried on
    // the next call) — it never blocks an agent start.
    void ensureDiscovered(CoreClient *core, const QString &harnessId);

    // A model catalogue split into a short "recommended" group and the full
    // live list, both as "value|name" entries (the picker format).
    struct ModelChoices {
        QStringList recommended;
        QStringList all;
    };

    // Query every engine/provider's live model catalogue at connect and cache
    // it. For each engine this probes the discovered-option vocabularies (kimi
    // thinking/mode) and the Claude-direct model list; for provider-routing
    // engines it also queries each configured provider's /v1/models. Fully
    // non-blocking — a failed or empty probe leaves the last cache intact.
    void discoverAll(CoreClient *core);

    // Cached model choices for a harness + provider (providerId empty = direct),
    // recommended group first. Empty lists when nothing has been discovered yet.
    ModelChoices modelChoices(const QString &harnessId, const QString &providerId) const;
    // The reasoning-effort tiers one model supports, as last discovered. An
    // EMPTY list means the engine said nothing about this model and every tier
    // in the harness vocabulary is offered — it never means "no tiers".
    QStringList modelEfforts(const QString &harnessId, const QString &providerId,
                             const QString &modelValue) const;
    // Persist a configOptions array (the init-event / agent.discoverOptions
    // shape) to the harness's per-option KConfig keys — the single writer for
    // the discovered "value|name" enumerations.
    static void persistDiscoveredOptions(const QString &harnessId,
                                         const QJsonArray &configOptions);
    // Persist discovered options AND emit changed() so the roster quick menu
    // and open pickers rebuild — the notifying counterpart of the static
    // persist, for callers (a live session's init event) outside the probe path.
    void applyDiscoveredOptions(const QString &harnessId,
                                const QJsonArray &configOptions);

    // Traits for a harness id. The empty id (legacy records) resolves to the
    // default engine; an unknown id gets claude-shaped defaults under its own
    // name, so nothing crashes against a newer core.
    HarnessTraits traits(const QString &id) const;
    // All harnesses in the core's registration order — the engine-picker order.
    QList<HarnessTraits> all() const;

    // Human labels for the known static vocabulary values; unknown values
    // fall back to the raw token so a newer core still renders.
    static QString modeLabel(const QString &value);
    static QString effortLabel(const QString &value);

Q_SIGNALS:
    void changed();

private:
    HarnessRegistry();

    // Probe one harness/provider's model catalogue via agent.discoverModels and
    // cache it under modelCacheKey; a non-empty result emits changed().
    void discoverModels(CoreClient *core, const QString &harnessId,
                        const QString &providerId, const QJsonObject &provider);

    QHash<QString, HarnessTraits> m_traits;
    QStringList m_order;
    QSet<QString> m_discovering;      // harness ids with a discoverOptions probe in flight
    QSet<QString> m_discoveringModels; // "harness@provider" keys with a models probe in flight
};
