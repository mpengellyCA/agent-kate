// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "DraftStore.h"

#include <QCryptographicHash>

#include <KConfigGroup>
#include <KSharedConfig>

namespace {
// Same group AgentPanel writes drafts into. If either of these two constants
// or the hash below drifts from AgentPanel::draftKey(), the cleanup silently
// stops finding anything — which is why DraftStoreTest pins the produced keys
// against literal strings rather than against this code.
const char *kGroup = "Agent";
} // namespace

namespace DraftStore {

QString threadKey(const QString &threadId)
{
    if (threadId.isEmpty()) {
        return QString();
    }
    return QStringLiteral("draft-") + threadId;
}

QString workspaceKey(const QString &workspacePath)
{
    if (workspacePath.isEmpty()) {
        return QString();
    }
    const QByteArray h = QCryptographicHash::hash(workspacePath.toUtf8(),
                                                  QCryptographicHash::Md5);
    return QStringLiteral("draft-new-") + QString::fromLatin1(h.toHex().left(12));
}

void clear(const QString &key)
{
    if (key.isEmpty()) {
        return;
    }
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QString::fromLatin1(kGroup));
    if (!cfg.hasKey(key)) {
        return;
    }
    cfg.deleteEntry(key);
    cfg.sync();
}

} // namespace DraftStore
