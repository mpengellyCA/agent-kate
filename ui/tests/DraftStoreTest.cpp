// Audit F50, "draft entries leak": closing an agent that had text in its
// composer left a `draft-…` entry in the config forever, because clearDraft()
// was never called on the teardown path.
//
// The keys are pinned against LITERAL strings rather than against this module's
// own output on purpose. AgentPanel::draftKey() still derives the same two keys
// with its own copy of the rules; if either drifts, the cleanup would silently
// stop finding anything and a test written in terms of DraftStore alone would
// go on passing.

#include "state/DraftStore.h"

#include <QStandardPaths>
#include <QtTest>

#include <KConfigGroup>
#include <KSharedConfig>

class DraftStoreTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();

    void threadKeyMatchesAgentPanel();
    void workspaceKeyMatchesAgentPanel();
    void emptyIdentityHasNoKey();
    void clearRemovesTheEntry();
    void clearLeavesOtherDraftsAlone();
    void clearOfNothingIsHarmless();
};

void DraftStoreTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
}

void DraftStoreTest::threadKeyMatchesAgentPanel()
{
    // AgentPanel::draftKey(): "draft-" + m_threadId.
    QCOMPARE(DraftStore::threadKey(QStringLiteral("t-abc123")),
             QStringLiteral("draft-t-abc123"));
}

void DraftStoreTest::workspaceKeyMatchesAgentPanel()
{
    // AgentPanel::draftKey(): "draft-new-" + md5(workspace).toHex().left(12).
    // The digest below is `printf %s /home/u/proj | md5sum`, computed outside
    // this code base.
    QCOMPARE(DraftStore::workspaceKey(QStringLiteral("/home/u/proj")),
             QStringLiteral("draft-new-4fafce67053e"));
}

void DraftStoreTest::emptyIdentityHasNoKey()
{
    QVERIFY(DraftStore::threadKey(QString()).isEmpty());
    QVERIFY(DraftStore::workspaceKey(QString()).isEmpty());
}

void DraftStoreTest::clearRemovesTheEntry()
{
    const QString key = DraftStore::threadKey(QStringLiteral("t-gone"));
    KConfigGroup g = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    g.writeEntry(key, QStringLiteral("half-written task"));
    g.sync();
    QVERIFY(KSharedConfig::openConfig()
                ->group(QStringLiteral("Agent"))
                .hasKey(key));

    DraftStore::clear(key);

    QVERIFY2(!KSharedConfig::openConfig()
                  ->group(QStringLiteral("Agent"))
                  .hasKey(key),
             "closing an agent must not leave its draft behind forever");
}

// The workspace-scoped key is SHARED by every not-yet-started agent in a
// project, which is why AgentDock only clears it once the last one is gone.
// Clearing one key must never touch another.
void DraftStoreTest::clearLeavesOtherDraftsAlone()
{
    const QString mine = DraftStore::threadKey(QStringLiteral("t-mine"));
    const QString theirs = DraftStore::workspaceKey(QStringLiteral("/home/u/proj"));
    KConfigGroup g = KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    g.writeEntry(mine, QStringLiteral("mine"));
    g.writeEntry(theirs, QStringLiteral("theirs"));
    g.sync();

    DraftStore::clear(mine);

    const KConfigGroup after =
        KSharedConfig::openConfig()->group(QStringLiteral("Agent"));
    QVERIFY(!after.hasKey(mine));
    QCOMPARE(after.readEntry(theirs, QString()), QStringLiteral("theirs"));
}

void DraftStoreTest::clearOfNothingIsHarmless()
{
    DraftStore::clear(QString());
    DraftStore::clear(QStringLiteral("draft-never-existed"));
}

QTEST_MAIN(DraftStoreTest)
#include "DraftStoreTest.moc"
