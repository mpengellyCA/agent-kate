// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "EnsembleCatalog.h"
#include "ipc/CoreClient.h"

#include <QJsonArray>
#include <QPointer>

namespace {
EnsembleMember memberFromJson(const QJsonObject &o)
{
    EnsembleMember m;
    m.role = o.value(QStringLiteral("role")).toString();
    m.backend = o.value(QStringLiteral("backend")).toString();
    m.model = o.value(QStringLiteral("model")).toString();
    m.permissionMode = o.value(QStringLiteral("permissionMode")).toString();
    m.effort = o.value(QStringLiteral("effort")).toString();
    m.isolation = o.value(QStringLiteral("isolation")).toString();
    m.notes = o.value(QStringLiteral("notes")).toString();
    return m;
}

QJsonObject memberToJson(const EnsembleMember &m)
{
    return QJsonObject{
        {QStringLiteral("role"), m.role},
        {QStringLiteral("backend"), m.backend},
        {QStringLiteral("model"), m.model},
        {QStringLiteral("permissionMode"), m.permissionMode},
        {QStringLiteral("effort"), m.effort},
        {QStringLiteral("isolation"), m.isolation},
        {QStringLiteral("notes"), m.notes},
    };
}

bool sameMember(const EnsembleMember &a, const EnsembleMember &b)
{
    return a.role == b.role && a.backend == b.backend && a.model == b.model
        && a.permissionMode == b.permissionMode && a.effort == b.effort
        && a.isolation == b.isolation && a.notes == b.notes;
}
} // namespace

bool Ensemble::operator==(const Ensemble &o) const
{
    if (name != o.name || description != o.description || masterPrompt != o.masterPrompt
        || builtIn != o.builtIn || workers.size() != o.workers.size()
        || !sameMember(controller, o.controller)) {
        return false;
    }
    for (int i = 0; i < workers.size(); ++i) {
        if (!sameMember(workers.at(i), o.workers.at(i))) {
            return false;
        }
    }
    return true;
}

EnsembleCatalog *EnsembleCatalog::self()
{
    static EnsembleCatalog instance;
    return &instance;
}

Ensemble EnsembleCatalog::fromJson(const QJsonObject &o)
{
    Ensemble e;
    e.name = o.value(QStringLiteral("name")).toString();
    e.description = o.value(QStringLiteral("description")).toString();
    e.controller = memberFromJson(o.value(QStringLiteral("controller")).toObject());
    const QJsonArray workers = o.value(QStringLiteral("workers")).toArray();
    for (const QJsonValue &v : workers) {
        e.workers.append(memberFromJson(v.toObject()));
    }
    e.masterPrompt = o.value(QStringLiteral("masterPrompt")).toString();
    e.builtIn = o.value(QStringLiteral("builtIn")).toBool();
    return e;
}

QJsonObject EnsembleCatalog::toJson(const Ensemble &e)
{
    QJsonArray workers;
    for (const EnsembleMember &w : e.workers) {
        workers.append(memberToJson(w));
    }
    return QJsonObject{
        {QStringLiteral("name"), e.name},
        {QStringLiteral("description"), e.description},
        {QStringLiteral("controller"), memberToJson(e.controller)},
        {QStringLiteral("workers"), workers},
        {QStringLiteral("masterPrompt"), e.masterPrompt},
    };
}

void EnsembleCatalog::fetch(CoreClient *core)
{
    if (!core || !core->isConnected()) {
        return;
    }
    core->call(
        QStringLiteral("mode.list"), QJsonObject{},
        [this](const QJsonObject &result, const QJsonObject &error) {
            if (!error.isEmpty()) {
                return; // an older core has no ensembles; keep what we have
            }
            QList<Ensemble> fresh;
            const QJsonArray modes = result.value(QStringLiteral("modes")).toArray();
            for (const QJsonValue &v : modes) {
                const Ensemble e = fromJson(v.toObject());
                if (!e.name.isEmpty()) {
                    fresh.append(e);
                }
            }
            const QString prompt =
                result.value(QStringLiteral("defaultMasterPrompt")).toString();
            if (fresh == m_list && prompt == m_defaultPrompt) {
                return; // unchanged — don't rebuild every picker for nothing
            }
            m_list = fresh;
            m_defaultPrompt = prompt;
            emit changed();
        },
        this);
}

bool EnsembleCatalog::contains(const QString &name) const
{
    for (const Ensemble &e : m_list) {
        if (e.name == name) {
            return true;
        }
    }
    return false;
}

Ensemble EnsembleCatalog::get(const QString &name) const
{
    for (const Ensemble &e : m_list) {
        if (e.name == name) {
            return e;
        }
    }
    return {};
}

void EnsembleCatalog::save(CoreClient *core, const Ensemble &e,
                           std::function<void(const QString &)> onDone)
{
    if (!core || !core->isConnected()) {
        if (onDone) {
            onDone(QStringLiteral("not connected to the core"));
        }
        return;
    }
    QPointer<EnsembleCatalog> self(this);
    core->call(
        QStringLiteral("mode.save"), QJsonObject{{QStringLiteral("mode"), toJson(e)}},
        [self, core, onDone](const QJsonObject &, const QJsonObject &error) {
            if (onDone) {
                onDone(error.value(QStringLiteral("message")).toString());
            }
            if (self && error.isEmpty()) {
                self->fetch(core);
            }
        },
        this);
}

void EnsembleCatalog::remove(CoreClient *core, const QString &name,
                             std::function<void(const QString &)> onDone)
{
    if (!core || !core->isConnected()) {
        if (onDone) {
            onDone(QStringLiteral("not connected to the core"));
        }
        return;
    }
    QPointer<EnsembleCatalog> self(this);
    core->call(
        QStringLiteral("mode.delete"), QJsonObject{{QStringLiteral("name"), name}},
        [self, core, onDone](const QJsonObject &, const QJsonObject &error) {
            if (onDone) {
                onDone(error.value(QStringLiteral("message")).toString());
            }
            if (self && error.isEmpty()) {
                self->fetch(core);
            }
        },
        this);
}
