// Audit F37. Agent Kate contains no model: every agent is an external CLI that
// akcore spawns off $PATH. Before this check existed, a machine with neither
// `claude` nor `kimi` installed let the user open a project, pick an engine,
// write "fix the login bug", press Enter — and only then read
// `exec: "claude": executable file not found in $PATH`.
//
// These drive the real probe against a real $PATH, because the whole value of
// the module is that it agrees with what the core will actually find.

#include "state/EngineAvailability.h"
#include "state/HarnessTraits.h"

#include <QDir>
#include <QFile>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QtTest>

class EngineAvailabilityTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void init();
    void cleanupTestCase();

    void reportsAnInstalledEngineAsPresent();
    void reportsKimiInCoreAugmentedUserBinAsPresent();
    void reportsAMissingEngineAsAbsent();
    void noEngineAtAllIsTheBannerCase();
    void oneEngineInstalledIsNotABannerCase();
    void unknownEngineIsNotDeclaredMissing();

private:
    // Point $PATH at a directory holding exactly the named executables.
    void setPathWith(const QStringList &executables);
    void removeUserKimi();

    QTemporaryDir m_dir;
    QTemporaryDir m_home;
    QByteArray m_originalPath;
    QByteArray m_originalHome;
};

void EngineAvailabilityTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    m_originalPath = qgetenv("PATH");
    m_originalHome = qgetenv("HOME");
    QVERIFY(m_dir.isValid());
    QVERIFY(m_home.isValid());
    qputenv("HOME", m_home.path().toLocal8Bit());
    HarnessTraits claude;
    claude.id = QStringLiteral("claude");
    claude.displayName = QStringLiteral("Claude Code");
    HarnessTraits kimi;
    kimi.id = QStringLiteral("kimi");
    kimi.displayName = QStringLiteral("Kimi Code");
    HarnessRegistry::self()->replaceDescriptorsForTest({claude, kimi});
}

void EngineAvailabilityTest::init()
{
    removeUserKimi();
    EngineAvailability::invalidate();
}

void EngineAvailabilityTest::cleanupTestCase()
{
    qputenv("PATH", m_originalPath);
    qputenv("HOME", m_originalHome);
    EngineAvailability::invalidate();
}

void EngineAvailabilityTest::setPathWith(const QStringList &executables)
{
    QDir dir(m_dir.path());
    const QStringList existing = dir.entryList(QDir::Files);
    for (const QString &name : existing) {
        QVERIFY(QFile::remove(dir.filePath(name)));
    }
    for (const QString &name : executables) {
        const QString path = dir.filePath(name);
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly));
        f.write("#!/bin/sh\n");
        f.close();
        QVERIFY(f.setPermissions(QFile::ReadOwner | QFile::WriteOwner
                                 | QFile::ExeOwner));
    }
    qputenv("PATH", m_dir.path().toLocal8Bit());
    EngineAvailability::invalidate();
}

void EngineAvailabilityTest::removeUserKimi()
{
    const QString kimi = QDir(m_home.path()).filePath(QStringLiteral(".kimi-code/bin/kimi"));
    QFile::remove(kimi);
}

void EngineAvailabilityTest::reportsAnInstalledEngineAsPresent()
{
    setPathWith({QStringLiteral("claude")});
    QVERIFY(EngineAvailability::isPresent(QStringLiteral("claude")));

    const auto engines = EngineAvailability::scan();
    QVERIFY(!engines.isEmpty());
    bool sawClaude = false;
    for (const auto &e : engines) {
        if (e.id == QLatin1String("claude")) {
            sawClaude = true;
            QVERIFY(e.present);
            // The label is the plain name — an installed engine is not annotated.
            QCOMPARE(EngineAvailability::pickerLabel(e), e.displayName);
            // The executable must be the name the core actually spawns.
            QCOMPARE(e.executable, QStringLiteral("claude"));
        }
    }
    QVERIFY2(sawClaude, "the registry's built-in engine list lost claude");
}

void EngineAvailabilityTest::reportsKimiInCoreAugmentedUserBinAsPresent()
{
    // A desktop-launched app may have a PATH that contains neither Kimi's
    // standard install directory nor another engine. akcore repairs that PATH
    // before launch, so this preflight must use the same directory too.
    setPathWith({});
    const QString binDir = QDir(m_home.path()).filePath(QStringLiteral(".kimi-code/bin"));
    QVERIFY(QDir().mkpath(binDir));
    QFile kimi(QDir(binDir).filePath(QStringLiteral("kimi")));
    QVERIFY(kimi.open(QIODevice::WriteOnly));
    kimi.write("#!/bin/sh\n");
    kimi.close();
    QVERIFY(kimi.setPermissions(QFile::ReadOwner | QFile::WriteOwner | QFile::ExeOwner));
    EngineAvailability::invalidate();

    QVERIFY(EngineAvailability::isPresent(QStringLiteral("kimi")));
}

void EngineAvailabilityTest::reportsAMissingEngineAsAbsent()
{
    setPathWith({QStringLiteral("claude")});
    QVERIFY(!EngineAvailability::isPresent(QStringLiteral("kimi")));

    const auto engines = EngineAvailability::scan();
    for (const auto &e : engines) {
        if (e.id == QLatin1String("kimi")) {
            QVERIFY(!e.present);
            // A dead picker row must SAY it is dead.
            QVERIFY(EngineAvailability::pickerLabel(e) != e.displayName);
            QVERIFY(EngineAvailability::pickerLabel(e).contains(e.displayName));
        }
    }
}

void EngineAvailabilityTest::noEngineAtAllIsTheBannerCase()
{
    setPathWith({});
    const auto engines = EngineAvailability::scan();
    QVERIFY(EngineAvailability::noneAvailable(engines));

    const QString message = EngineAvailability::missingEnginesMessage(engines);
    QVERIFY(!message.isEmpty());
    // It must name the commands a human would type, not our harness ids alone.
    QVERIFY(message.contains(QLatin1String("claude")));
    QVERIFY(message.contains(QLatin1String("kimi")));
    // …and it must offer somewhere to go, which is the half the raw core error
    // never had.
    QVERIFY(EngineAvailability::installUrl(engines).startsWith(
        QLatin1String("https://")));
}

void EngineAvailabilityTest::oneEngineInstalledIsNotABannerCase()
{
    setPathWith({QStringLiteral("kimi")});
    const auto engines = EngineAvailability::scan();
    QVERIFY(!EngineAvailability::noneAvailable(engines));
    // A window-wide banner for "one of two engines is missing" would be noise;
    // that case is said in the picker instead.
    QVERIFY(EngineAvailability::missingEnginesMessage(engines).isEmpty());
}

void EngineAvailabilityTest::unknownEngineIsNotDeclaredMissing()
{
    setPathWith({});
    // A newer core can register engines this build has never heard of. Refusing
    // to offer them would be worse than a start that fails with the core's own
    // message, so the answer is fail-open.
    QVERIFY(EngineAvailability::isPresent(QStringLiteral("some-future-engine")));
}

QTEST_MAIN(EngineAvailabilityTest)
#include "EngineAvailabilityTest.moc"
