// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QHash>
#include <QList>
#include <QObject>
#include <QString>
#include <QStringList>

class CoreClient;

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
    bool compaction = false;
    bool promote = false;
    bool providerRouting = false;
    bool cowork = false;
    bool effortLive = false; // thinking effort adjustable mid-session
    bool usageReporting = false;
    bool sessionBrowse = false;

    QString modelPicker = QStringLiteral("tiers"); // "tiers" | "discovered"
    // Static vocabularies (wire values; the UI owns the human labels). Empty =
    // discovered per session from the CLI's configOptions and persisted under
    // optionKey().
    QStringList permissionModes;
    QStringList efforts;

    // Per-harness KConfig keys, [Agent] group. The default engine keeps its
    // historical key names so existing settings survive; other harnesses get
    // id-prefixed keys (kimi → "kimiMode"/"kimiThinking"/"kimiOpt-model").
    QString stickyModeKey() const;
    QString stickyEffortKey() const;
    QString optionKey(const QString &optionId) const; // discovered enumerations
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

    QHash<QString, HarnessTraits> m_traits;
    QStringList m_order;
};
