// Audit F46. On a fresh profile the engine picker listed "Claude Code via
// Fireworks" and "via OpenRouter" — presets that are seeded unconditionally and
// carry no API key — so the two most prominent alternatives to the default were
// choices that could only ever abort. keyResolvable/pickerLabel are what a
// picker gates or annotates on.
//
// Built without AK_HAVE_KWALLET (the definition is on the app target only), so
// this exercises the environment-variable resolution path — which is also the
// path that must be checked FIRST, since reaching into KWallet to render a
// label can pop a synchronous unlock prompt.

#include "ProviderConfig.h"

#include <QStandardPaths>
#include <QtTest>

class ProviderConfigTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void cleanup();

    void directNeedsNoKeyOfOurs();
    void seededPresetWithNoKeyIsNotUsable();
    void anEnvironmentVariableMakesItUsable();

private:
    ProviderProfile routedPreset() const;
};

void ProviderConfigTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
}

void ProviderConfigTest::cleanup()
{
    qunsetenv("FIREWORKS_API_KEY");
}

ProviderProfile ProviderConfigTest::routedPreset() const
{
    // load() seeds the built-in presets on a fresh config — the very behaviour
    // that puts these entries in front of a user with no keys.
    const QList<ProviderProfile> all = ProviderStore::load();
    for (const ProviderProfile &p : all) {
        if (p.id == QLatin1String("fireworks")) {
            return p;
        }
    }
    return {};
}

void ProviderConfigTest::directNeedsNoKeyOfOurs()
{
    const ProviderProfile direct = ProviderStore::byId(ProviderStore::directId());
    QVERIFY(!direct.routed());
    QVERIFY(ProviderStore::keyResolvable(direct));
    QCOMPARE(ProviderStore::pickerLabel(direct), direct.name);
}

void ProviderConfigTest::seededPresetWithNoKeyIsNotUsable()
{
    const ProviderProfile p = routedPreset();
    QCOMPARE(p.id, QStringLiteral("fireworks"));
    QVERIFY(p.routed());
    QVERIFY(!p.envVar.isEmpty());

    QVERIFY(!ProviderStore::keyResolvable(p));
    const QString label = ProviderStore::pickerLabel(p);
    QVERIFY(label != p.name);          // it must not read as a live choice
    QVERIFY(label.contains(p.name));   // …while still naming the provider
}

void ProviderConfigTest::anEnvironmentVariableMakesItUsable()
{
    const ProviderProfile p = routedPreset();
    QCOMPARE(p.envVar, QStringLiteral("FIREWORKS_API_KEY"));
    qputenv("FIREWORKS_API_KEY", "sk-not-a-real-key");
    QVERIFY(ProviderStore::keyResolvable(p));
    QCOMPARE(ProviderStore::pickerLabel(p), p.name);
}

QTEST_MAIN(ProviderConfigTest)
#include "ProviderConfigTest.moc"
