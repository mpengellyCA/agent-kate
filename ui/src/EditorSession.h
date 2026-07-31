// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QString>
#include <QStringList>

class KConfigGroup;

// EditorSession owns the naming and filtering rules for the editor's persisted
// tab sessions ([Editor][Sessions][<key>] in agentkaterc). It exists as a free
// unit — rather than more MainWindow members — so the rules that decide *which
// files may reopen where* are covered by tests (ui/tests/EditorSessionTest).
//
// Why the rules matter (plan 17): session keys used to be "agent-<N>" where N
// came from a per-run UI counter shared across every open project. The counter
// restarts at 1 each launch, so the first agent of whatever project was opened
// inherited the previous run's "agent-1" tabs — reopening a *different
// project's* files. Two independent layers fix that and both live here:
//
//   1. Keys are derived from stable identity: the normalized project path, plus
//      the core threadId in tabs-by-agent mode. Nothing per-run remains.
//   2. Restore filters every path against the roots it is allowed to come from,
//      so even a stale or hand-edited group can never reopen foreign files.
//
// Group entries are schema-versioned: only version 2 groups are read, which
// retires the entire legacy "agent-N" corpus (its ints map to nothing) without
// a bespoke migration.
namespace EditorSession {

// Bumped when the meaning of a group's entries changes. Groups written by an
// older schema (or with no version at all) are ignored on read and swept.
constexpr int kSchemaVersion = 2;

// Cap on tabs replayed for one group, so a session full of heavy viewers
// (PDFs, spreadsheets) can't stall startup. The rest stay one click away.
constexpr int kMaxRestore = 20;

// Lexically absolute, cleaned, trailing-slash-free form of a project path. Used
// for keys and containment; deliberately NOT canonicalFilePath() — that needs
// the path to exist and would resolve symlinks, breaking containment for
// projects reached through a symlinked parent.
QString normalizedProject(const QString &projectPath);

// Group key for tabs grouped by project (the default mode).
QString projectKey(const QString &projectPath);

// Group key for tabs grouped by agent: project scope plus the agent's core
// thread id, which is stable across runs.
QString agentKey(const QString &projectPath, const QString &threadId);

// Group key for an agent that has no thread yet (a fresh, unstarted agent).
// Unique within the run so two new agents don't share tabs, and deliberately
// NOT persistable — the id means nothing next run. MainWindow renames the
// group to agentKey() once the thread id arrives.
QString pendingKey(const QString &projectPath, int agentId);

// False for pending (thread-less) keys, which must never reach the config.
bool isPersistable(const QString &key);

// The project path a key is scoped to, or empty if the key carries none.
QString projectForKey(const QString &key);

// True when path is one of the roots or lives underneath one. Roots are
// normalized here, so callers may pass raw paths.
bool isContained(const QString &path, const QStringList &roots);

struct Session {
    QStringList files; // contained, existing, capped at kMaxRestore
    QString active;    // the file to focus, empty if it did not survive filtering
};

// Read one group's replayable session. Returns an empty Session for a legacy /
// unversioned group, and drops every path that is not under one of roots or no
// longer exists on disk.
Session read(const KConfigGroup &sessions, const QString &key, const QStringList &roots);

// Write one group's tabs. Records the schema version and the owning project so
// sweep() can recognise the group later.
void write(KConfigGroup &sessions, const QString &key, const QStringList &files,
           const QString &active);

// Drop groups that can never be replayed usefully: legacy/unversioned ones, and
// current ones that are empty or whose project directory is gone. Run after the
// live groups have been written.
void sweep(KConfigGroup &sessions);

} // namespace EditorSession
