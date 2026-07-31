// Regression guard for plan 17: "on startup, Agent Kate reopens files that were
// open in unrelated projects".
//
// The old session keys were "agent-<N>" where N came from a per-run UI counter
// shared by every open project, and restore's only filter was "does the file
// exist". So launching into project A replayed whatever project B had left in
// agent-1. These tests drive the real rules — key derivation, the containment
// filter, the schema-version cut-off and the sweep — against a real KConfig
// file, so they pin the observable behaviour rather than the string format.

#include "EditorSession.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QDir>
#include <QTemporaryDir>
#include <QtTest>

namespace {
// Create `relative` under dirPath and return its absolute path.
QString touchAt(const QString &dirPath, const QString &relative)
{
    const QString path = QDir(dirPath).filePath(relative);
    QDir().mkpath(QFileInfo(path).absolutePath());
    QFile f(path);
    if (f.open(QIODevice::WriteOnly)) {
        f.write("x");
        f.close();
    }
    return path;
}

QString touch(const QTemporaryDir &dir, const QString &relative)
{
    return touchAt(dir.path(), relative);
}
} // namespace

class EditorSessionTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void init();
    void trailingSlashMapsToSameKey();
    void agentKeyUsesThreadIdNotCounter();
    void pendingKeysAreNeverPersisted();
    void projectForKeyRoundTrips();
    void restoresOwnProjectFiles();
    void filtersForeignProjectFiles();
    void capsRestoreButKeepsActive();
    void ignoresLegacyAgentGroups();
    void sweepDropsLegacyAndDeadGroups();
    void sweepKeepsOtherLiveProjects();
    void groupNamesSurviveKConfig();

private:
    KConfigGroup sessions();
    QTemporaryDir m_cfgDir;   // holds the throwaway config file
    QString m_cfgPath;
};

KConfigGroup EditorSessionTest::sessions()
{
    return KSharedConfig::openConfig(m_cfgPath, KConfig::SimpleConfig)
        ->group(QStringLiteral("Editor"))
        .group(QStringLiteral("Sessions"));
}

void EditorSessionTest::init()
{
    // Fresh config per test: KSharedConfig caches by name, so use a new one.
    m_cfgPath = m_cfgDir.filePath(
        QStringLiteral("session-%1.rc").arg(QTest::currentTestFunction()));
}

void EditorSessionTest::trailingSlashMapsToSameKey()
{
    QCOMPARE(EditorSession::projectKey(QStringLiteral("/home/u/Proj/")),
             EditorSession::projectKey(QStringLiteral("/home/u/Proj")));
    QCOMPARE(EditorSession::projectKey(QStringLiteral("/home/u/Proj/./sub/..")),
             EditorSession::projectKey(QStringLiteral("/home/u/Proj")));
    QCOMPARE(EditorSession::agentKey(QStringLiteral("/home/u/Proj/"),
                                     QStringLiteral("t-abc")),
             EditorSession::agentKey(QStringLiteral("/home/u/Proj"),
                                     QStringLiteral("t-abc")));
    QVERIFY(EditorSession::projectKey(QString()).isEmpty());
}

void EditorSessionTest::agentKeyUsesThreadIdNotCounter()
{
    const QString a = EditorSession::agentKey(QStringLiteral("/home/u/A"),
                                              QStringLiteral("t-1"));
    const QString b = EditorSession::agentKey(QStringLiteral("/home/u/B"),
                                              QStringLiteral("t-1"));
    // Same thread id in different projects must never collide...
    QVERIFY(a != b);
    // ...and two agents of one project are told apart by their thread ids.
    QVERIFY(a
            != EditorSession::agentKey(QStringLiteral("/home/u/A"),
                                       QStringLiteral("t-2")));
    // A thread-less agent has no stable key at all.
    QVERIFY(EditorSession::agentKey(QStringLiteral("/home/u/A"), QString()).isEmpty());
}

void EditorSessionTest::pendingKeysAreNeverPersisted()
{
    const QString pending = EditorSession::pendingKey(QStringLiteral("/home/u/A"), 1);
    QVERIFY(!pending.isEmpty());
    QVERIFY(!EditorSession::isPersistable(pending));
    QVERIFY(EditorSession::isPersistable(
        EditorSession::agentKey(QStringLiteral("/home/u/A"), QStringLiteral("t-1"))));
    QVERIFY(!EditorSession::isPersistable(QString()));

    // write() is the guard of last resort: a pending key reaching it writes
    // nothing, so next run can never replay a per-run group.
    KConfigGroup grp = sessions();
    EditorSession::write(grp, pending, {QStringLiteral("/home/u/A/x.md")}, QString());
    QVERIFY(!grp.hasGroup(pending));
}

void EditorSessionTest::projectForKeyRoundTrips()
{
    const QString project = QStringLiteral("/home/u/My Proj:1");
    QCOMPARE(EditorSession::projectForKey(EditorSession::projectKey(project)), project);
    QCOMPARE(EditorSession::projectForKey(
                 EditorSession::agentKey(project, QStringLiteral("t-abc"))),
             project);
    QCOMPARE(EditorSession::projectForKey(EditorSession::pendingKey(project, 3)), project);
}

void EditorSessionTest::restoresOwnProjectFiles()
{
    QTemporaryDir proj;
    const QString readme = touch(proj, QStringLiteral("README.md"));
    const QString nested = touch(proj, QStringLiteral("src/main.cpp"));
    // A worktree lives inside the project, so project containment alone covers it.
    const QString wt = touch(proj, QStringLiteral(".agentkate/worktrees/t-1/a.go"));
    const QString gone = proj.filePath(QStringLiteral("deleted.md"));

    KConfigGroup grp = sessions();
    const QString key = EditorSession::agentKey(proj.path(), QStringLiteral("t-1"));
    EditorSession::write(grp, key, {readme, gone, nested, wt}, nested);

    const EditorSession::Session s = EditorSession::read(grp, key, {proj.path()});
    QCOMPARE(s.files, QStringList({readme, nested, wt}));
    QCOMPARE(s.active, nested); // the focused file survived the filter
}

void EditorSessionTest::filtersForeignProjectFiles()
{
    QTemporaryDir proj;
    QTemporaryDir other;
    const QString mine = touch(proj, QStringLiteral("mine.md"));
    const QString theirs = touch(other, QStringLiteral("theirs.md"));
    // A directory whose path merely shares a textual prefix with the project
    // ("/tmp/projX" vs project "/tmp/proj") must not sneak past the filter.
    const QString siblingDir = proj.path() + QStringLiteral("-other");
    const QString sibling = touchAt(siblingDir, QStringLiteral("x.md"));

    KConfigGroup grp = sessions();
    const QString key = EditorSession::agentKey(proj.path(), QStringLiteral("t-1"));
    EditorSession::write(grp, key, {theirs, mine, sibling}, theirs);

    const EditorSession::Session s = EditorSession::read(grp, key, {proj.path()});
    QDir(siblingDir).removeRecursively();
    QCOMPARE(s.files, QStringList({mine}));
    QVERIFY2(s.active.isEmpty(), "a foreign active file must not be re-focused");

    // An out-of-project worktree is only allowed when it is passed as a root.
    QTemporaryDir wtDir;
    const QString wtFile = touch(wtDir, QStringLiteral("w.go"));
    EditorSession::write(grp, key, {mine, wtFile}, wtFile);
    QCOMPARE(EditorSession::read(grp, key, {proj.path()}).files, QStringList({mine}));
    QCOMPARE(EditorSession::read(grp, key, {proj.path(), wtDir.path()}).files,
             QStringList({mine, wtFile}));
}

void EditorSessionTest::capsRestoreButKeepsActive()
{
    QTemporaryDir proj;
    QStringList all;
    for (int i = 0; i < EditorSession::kMaxRestore + 5; ++i) {
        all << touchAt(proj.path(), QStringLiteral("f%1.md").arg(i));
    }
    KConfigGroup grp = sessions();
    const QString key = EditorSession::agentKey(proj.path(), QStringLiteral("t-1"));
    EditorSession::write(grp, key, all, all.constLast()); // focus beyond the cap

    const EditorSession::Session s = EditorSession::read(grp, key, {proj.path()});
    QCOMPARE(s.files.size(), EditorSession::kMaxRestore);
    QCOMPARE(s.active, all.constLast());
    QVERIFY2(s.files.contains(all.constLast()),
             "the focused tab must survive the cap");
    QCOMPARE(s.files.constFirst(), all.constFirst()); // order is still the human's
}

void EditorSessionTest::ignoresLegacyAgentGroups()
{
    QTemporaryDir proj;
    const QString mine = touch(proj, QStringLiteral("mine.md"));

    // Exactly what a pre-plan-17 rc file holds: an unversioned "agent-1" group.
    KConfigGroup grp = sessions();
    KConfigGroup legacy = grp.group(QStringLiteral("agent-1"));
    legacy.writeEntry("openFiles", QStringList({mine}));
    legacy.writeEntry("active", mine);

    const EditorSession::Session s =
        EditorSession::read(grp, QStringLiteral("agent-1"), {proj.path()});
    QVERIFY2(s.files.isEmpty(),
             "legacy groups have no stable identity — they must never replay");
    QVERIFY(s.active.isEmpty());
}

void EditorSessionTest::sweepDropsLegacyAndDeadGroups()
{
    QTemporaryDir proj;
    const QString mine = touch(proj, QStringLiteral("mine.md"));
    KConfigGroup grp = sessions();

    KConfigGroup legacy = grp.group(QStringLiteral("agent-7"));
    legacy.writeEntry("openFiles", QStringList({mine}));

    const QString live = EditorSession::agentKey(proj.path(), QStringLiteral("t-1"));
    EditorSession::write(grp, live, {mine}, mine);

    const QString empty = EditorSession::agentKey(proj.path(), QStringLiteral("t-2"));
    EditorSession::write(grp, empty, {}, QString());

    QTemporaryDir doomed;
    const QString dead = EditorSession::agentKey(doomed.path(), QStringLiteral("t-3"));
    EditorSession::write(grp, dead, {doomed.filePath(QStringLiteral("z.md"))}, QString());
    QVERIFY(doomed.remove()); // project directory gone

    EditorSession::sweep(grp);

    QVERIFY2(!grp.hasGroup(QStringLiteral("agent-7")), "legacy group must be swept");
    QVERIFY2(!grp.hasGroup(empty), "an empty group is clutter");
    QVERIFY2(!grp.hasGroup(dead), "a vanished project can never replay");
    QVERIFY2(grp.hasGroup(live), "the live group must survive");
}

void EditorSessionTest::sweepKeepsOtherLiveProjects()
{
    // Persistence covers every open project, and quitting a run that only
    // opened project A must not discard project B's remembered tabs.
    QTemporaryDir a;
    QTemporaryDir b;
    const QString fa = touch(a, QStringLiteral("a.md"));
    const QString fb = touch(b, QStringLiteral("b.md"));
    KConfigGroup grp = sessions();
    const QString ka = EditorSession::agentKey(a.path(), QStringLiteral("t-a"));
    const QString kb = EditorSession::agentKey(b.path(), QStringLiteral("t-b"));
    EditorSession::write(grp, ka, {fa}, fa);
    EditorSession::write(grp, kb, {fb}, fb);

    EditorSession::sweep(grp);
    QVERIFY(grp.hasGroup(ka));
    QVERIFY(grp.hasGroup(kb));
    QCOMPARE(EditorSession::read(grp, kb, {b.path()}).files, QStringList({fb}));
}

void EditorSessionTest::groupNamesSurviveKConfig()
{
    // KConfig group names sit between literal brackets in the ini file. Round-trip
    // a path full of hostile characters through an actual file to prove the key
    // encoding holds (and that a re-read finds the same group).
    QTemporaryDir proj;
    const QString hostile = touch(proj, QStringLiteral("we[ir]d dir/f=x.md"));
    const QString key = EditorSession::agentKey(QFileInfo(hostile).absolutePath(),
                                                QStringLiteral("t-x"));
    {
        KConfigGroup grp = sessions();
        EditorSession::write(grp, key, {hostile}, hostile);
        grp.sync();
    }
    // Re-open the file from scratch — a broken group header would lose the entry.
    KSharedConfig::Ptr reread = KSharedConfig::openConfig(m_cfgPath, KConfig::SimpleConfig);
    reread->reparseConfiguration();
    KConfigGroup grp = reread->group(QStringLiteral("Editor")).group(QStringLiteral("Sessions"));
    QVERIFY2(grp.hasGroup(key), "the encoded group name must survive a write/read cycle");
    QCOMPARE(EditorSession::read(grp, key, {QFileInfo(hostile).absolutePath()}).files,
             QStringList({hostile}));
}

QTEST_MAIN(EditorSessionTest)
#include "EditorSessionTest.moc"
