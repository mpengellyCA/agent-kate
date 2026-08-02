// The window title had two writers and no composition. AgentDock's attention
// counter wrote "(3) Agent Kate"; selecting an agent wrote "Agent Kate — repo"
// straight over it. Because attentionCountChanged is change-gated, the count
// was never re-announced, so answering the prompt was the only thing that could
// clear a number the user could no longer see.
//
// These pin the composition: neither input may erase the other.

#include "state/WindowTitle.h"

#include <QtTest>

class WindowTitleTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void bareApplication();
    void projectIsNamed();
    void attentionIsCounted();
    void attentionSurvivesAProjectSwitch();
    void noAttentionMeansNoPrefix();
};

void WindowTitleTest::bareApplication()
{
    QCOMPARE(WindowTitle::compose(QString(), 0), QStringLiteral("Agent Kate"));
}

void WindowTitleTest::projectIsNamed()
{
    const QString t = WindowTitle::compose(QStringLiteral("myrepo"), 0);
    QVERIFY(t.contains(QLatin1String("myrepo")));
    QVERIFY(t.contains(QLatin1String("Agent Kate")));
}

void WindowTitleTest::attentionIsCounted()
{
    const QString t = WindowTitle::compose(QString(), 2);
    QVERIFY2(t.contains(QLatin1String("(2)")), qPrintable(t));
}

// The regression itself: with a project open AND agents waiting, the title has
// to carry both. Before the fix the project write dropped the count entirely.
void WindowTitleTest::attentionSurvivesAProjectSwitch()
{
    const QString t = WindowTitle::compose(QStringLiteral("myrepo"), 3);
    QVERIFY2(t.contains(QLatin1String("(3)")), qPrintable(t));
    QVERIFY2(t.contains(QLatin1String("myrepo")), qPrintable(t));
    // The count leads, so a truncating task bar still shows it.
    QVERIFY2(t.startsWith(QLatin1String("(3)")), qPrintable(t));
}

void WindowTitleTest::noAttentionMeansNoPrefix()
{
    QVERIFY(!WindowTitle::compose(QStringLiteral("myrepo"), 0)
                 .startsWith(QLatin1Char('(')));
    // A negative count is a bug elsewhere, not a reason to print "(-1)".
    QVERIFY(!WindowTitle::compose(QStringLiteral("myrepo"), -1)
                 .startsWith(QLatin1Char('(')));
}

QTEST_MAIN(WindowTitleTest)
#include "WindowTitleTest.moc"
