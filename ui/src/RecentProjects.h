#pragma once

#include <QStringList>

// RecentProjects is the small KSharedConfig-backed list of project directories
// the user has opened. The welcome screen reads it on launch and MainWindow
// pushes to it whenever a project is opened. Most-recent first; capped.
namespace RecentProjects {

constexpr int kMaxEntries = 12;

QStringList load();
void remember(const QString &path); // moves to front, dedupes, prunes
void forget(const QString &path);
QString last();                     // most recently opened, or empty

} // namespace RecentProjects
