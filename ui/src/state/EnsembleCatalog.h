// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QJsonObject>
#include <QList>
#include <QObject>
#include <QString>

class CoreClient;

// One member of an ensemble: the controller, or a worker role it may launch.
// Every field is the harness's own vocabulary, passed through untouched — the
// UI never resolves a model id (see EnsembleCatalog).
struct EnsembleMember {
    QString role; // worker only; the label the controller launches it by
    QString backend;
    QString model;
    QString permissionMode;
    QString effort;
    QString isolation;
    QString notes; // one-line hint, rendered into the controller's roster
};

// Ensemble mirrors one core-side "mode": a controller, its worker roster and
// the master prompt that briefs it.
struct Ensemble {
    QString name;
    QString description;
    EnsembleMember controller;
    QList<EnsembleMember> workers;
    QString masterPrompt; // empty = the core's default template
    bool builtIn = false; // ships with Agent Kate; editing shadows it

    bool operator==(const Ensemble &o) const;
};

// EnsembleCatalog mirrors the core's ensemble store (mode.list) for every UI
// surface that offers ensembles: the New Agent dialog, the roster quick menu
// and the ensemble editor. One shared copy, refreshed on demand, so a save in
// the editor updates the pickers without each of them polling.
//
// It holds no ensemble knowledge of its own: the built-in catalogue, the
// default master prompt and all validation live core-side, and an empty list
// (no core, old core) simply means the UI offers no ensembles.
class EnsembleCatalog : public QObject
{
    Q_OBJECT
public:
    static EnsembleCatalog *self();

    // Refresh from the core; emits changed() when the list differs.
    void fetch(CoreClient *core);

    QList<Ensemble> list() const { return m_list; }
    bool contains(const QString &name) const;
    Ensemble get(const QString &name) const;
    // The core's default master-prompt template, for the editor's placeholder
    // help. Empty until the first successful fetch.
    QString defaultMasterPrompt() const { return m_defaultPrompt; }

    // Persist one ensemble (mode.save) and re-fetch on success. onDone reports
    // an empty string on success, or the core's error message.
    void save(CoreClient *core, const Ensemble &e,
              std::function<void(const QString &error)> onDone = {});
    // Delete by name (mode.delete) and re-fetch on success.
    void remove(CoreClient *core, const QString &name,
                std::function<void(const QString &error)> onDone = {});

    // JSON round-trip helpers, shared by the catalogue and the save path.
    static Ensemble fromJson(const QJsonObject &o);
    static QJsonObject toJson(const Ensemble &e);

Q_SIGNALS:
    void changed();

private:
    EnsembleCatalog() = default;

    QList<Ensemble> m_list;
    QString m_defaultPrompt;
};
