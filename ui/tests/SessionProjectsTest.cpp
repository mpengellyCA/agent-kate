// Audit F47. Relaunching offered exactly one project, so anyone working across
// several repositories re-added the rest by hand every single session (and
// their dormant agents came back only once the folder was re-added). The store
// under test is what makes "Reopen session (N projects)" possible.

#include "state/SessionProjects.h"

#include <QDir>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QtTest>

class SessionProjectsTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();

    void roundTripsTheWholeSet();
    void dropsProjectsThatNoLongerExist();
    void dropsDuplicates();
    void anEmptySaveReallyClearsIt();

private:
    QTemporaryDir m_dir;
    QString sub(const QString &name);
};

void SessionProjectsTest::initTestCase()
{
    // Never write the developer's own agentkaterc.
    QStandardPaths::setTestModeEnabled(true);
    QVERIFY(m_dir.isValid());
}

QString SessionProjectsTest::sub(const QString &name)
{
    QDir d(m_dir.path());
    d.mkpath(name);
    return d.filePath(name);
}

void SessionProjectsTest::roundTripsTheWholeSet()
{
    const QStringList set{sub(QStringLiteral("a")), sub(QStringLiteral("b")),
                          sub(QStringLiteral("c"))};
    SessionProjects::save(set);
    // Order is the order they were opened — the reopen must land the user back
    // in the same arrangement, not a re-sorted one.
    QCOMPARE(SessionProjects::load(), set);
}

void SessionProjectsTest::dropsProjectsThatNoLongerExist()
{
    const QString alive = sub(QStringLiteral("alive"));
    const QString dead = m_dir.filePath(QStringLiteral("deleted-since"));
    SessionProjects::save({alive, dead});
    // Offering a folder that has been deleted or unmounted since is worse than
    // offering nothing: it fails at the moment the user is trying to get back
    // to work.
    QCOMPARE(SessionProjects::load(), QStringList{alive});
}

void SessionProjectsTest::dropsDuplicates()
{
    const QString one = sub(QStringLiteral("one"));
    SessionProjects::save({one, one});
    QCOMPARE(SessionProjects::load(), QStringList{one});
}

void SessionProjectsTest::anEmptySaveReallyClearsIt()
{
    SessionProjects::save({sub(QStringLiteral("temp"))});
    QVERIFY(!SessionProjects::load().isEmpty());
    // Closing every project is a real state. Resurrecting the old set on the
    // next launch would be the app overriding a decision the user made.
    SessionProjects::save({});
    QVERIFY(SessionProjects::load().isEmpty());
}

QTEST_MAIN(SessionProjectsTest)
#include "SessionProjectsTest.moc"
