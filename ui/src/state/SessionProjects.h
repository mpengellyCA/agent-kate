// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QStringList>

// SessionProjects remembers WHICH PROJECTS WERE OPEN when Agent Kate last shut
// down — the whole set, not just the most recent one.
//
// This is deliberately not RecentProjects. That list is a history ("places you
// have been"), ordered by recency and pruned to twelve. This one is a snapshot
// of a working session ("the four projects you had open at once"), and losing
// it is what made every relaunch a manual redo for anyone who works across more
// than one repository: the welcome screen offered exactly one project, and the
// dormant agents of the others came back only after re-adding each folder by
// hand (audit F47).
//
// Stored under [Session] in the normal config, so it survives a crash as well
// as a clean exit.
namespace SessionProjects {

// The project set as of the last save, in the order it was opened. Paths that
// no longer exist on disk are filtered out on read — a reopen must not offer a
// folder that has since been deleted or unmounted.
QStringList load();

// Replace the remembered set. An empty list clears it (the user closed every
// project, which is a real state and must not resurrect the old one).
void save(const QStringList &paths);

} // namespace SessionProjects
