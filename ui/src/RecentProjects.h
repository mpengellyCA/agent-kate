#pragma once

#include <QStringList>

class QDateTime;

// RecentProjects is the small KSharedConfig-backed list of project directories
// the user has opened. The welcome screen reads it on launch and MainWindow
// pushes to it whenever a project is opened. Most-recent first; capped.
//
// Beyond the bare recent list it tracks pinned (favourite) projects, which the
// welcome screen surfaces above the rest, and a per-project last-opened
// timestamp for a friendly "opened 2 days ago" hint.
namespace RecentProjects {

constexpr int kMaxEntries = 12;

QStringList load();
void remember(const QString &path); // moves to front, dedupes, prunes, stamps time
void forget(const QString &path);
QString last();                     // most recently opened, or empty

// Pinned / favourite projects. Pinned entries are never pruned by remember()
// and are not subject to kMaxEntries.
QStringList pinned();
bool isPinned(const QString &path);
void pin(const QString &path);
void unpin(const QString &path);

// Most recent open time for a project (invalid QDateTime if unknown).
QDateTime lastOpened(const QString &path);

} // namespace RecentProjects
