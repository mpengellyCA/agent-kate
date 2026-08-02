// Audit F47 and F50. The welcome screen is the first thing a new user sees and
// the thing a returning user sees every launch, and it had two holes:
//
//   * it offered exactly ONE project, so a multi-project session had to be
//     rebuilt by hand every time, and
//   * with no history at all its recents list was a blank box — the one list in
//     the app with no empty state.

#include "RecentProjects.h"
#include "WelcomeDialog.h"
#include "state/SessionProjects.h"

#include <QDir>
#include <QLabel>
#include <QListWidget>
#include <QPushButton>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QtTest>

class WelcomeDialogTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void init();

    void offersTheWholeSessionWhenItHadSeveralProjects();
    void reopeningTheSessionReturnsEveryProject();
    void doesNotOfferASessionOfOne();
    void emptyRecentsSayWhatWouldFillThem();

private:
    QString sub(const QString &name);
    QTemporaryDir m_dir;
};

void WelcomeDialogTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    QVERIFY(m_dir.isValid());
}

void WelcomeDialogTest::init()
{
    SessionProjects::save({});
    for (const QString &p : RecentProjects::load()) {
        RecentProjects::forget(p);
    }
}

QString WelcomeDialogTest::sub(const QString &name)
{
    QDir d(m_dir.path());
    d.mkpath(name);
    return d.filePath(name);
}

void WelcomeDialogTest::offersTheWholeSessionWhenItHadSeveralProjects()
{
    SessionProjects::save({sub(QStringLiteral("p1")), sub(QStringLiteral("p2")),
                           sub(QStringLiteral("p3"))});
    WelcomeDialog dlg;
    auto *session = dlg.findChild<QPushButton *>(QStringLiteral("reopenSessionButton"));
    QVERIFY(session);
    QVERIFY2(session->isVisibleTo(&dlg), "the multi-project reopen is not offered");
    QVERIFY2(session->text().contains(QLatin1String("3")),
             "the button does not say how many projects come back");
    // The set is the better guess than its newest member, so Enter takes it.
    QVERIFY(session->isDefault());
    auto *reopen = dlg.findChild<QPushButton *>(QStringLiteral("reopenButton"));
    QVERIFY(reopen);
    QVERIFY(!reopen->isDefault());
}

void WelcomeDialogTest::reopeningTheSessionReturnsEveryProject()
{
    const QStringList set{sub(QStringLiteral("a")), sub(QStringLiteral("b"))};
    SessionProjects::save(set);
    WelcomeDialog dlg;
    auto *session = dlg.findChild<QPushButton *>(QStringLiteral("reopenSessionButton"));
    QVERIFY(session);
    session->click();

    QCOMPARE(dlg.result(), int(QDialog::Accepted));
    QCOMPARE(dlg.selectedPaths(), set);
    // A caller that only understands one project still gets a sane answer.
    QCOMPARE(dlg.selectedPath(), set.constFirst());
}

void WelcomeDialogTest::doesNotOfferASessionOfOne()
{
    SessionProjects::save({sub(QStringLiteral("only"))});
    WelcomeDialog dlg;
    auto *session = dlg.findChild<QPushButton *>(QStringLiteral("reopenSessionButton"));
    QVERIFY(session);
    // With one project "Reopen" already IS the whole session; two buttons that
    // do the same thing is worse than one.
    QVERIFY(!session->isVisibleTo(&dlg));
}

void WelcomeDialogTest::emptyRecentsSayWhatWouldFillThem()
{
    WelcomeDialog dlg;
    auto *list = dlg.findChild<QListWidget *>();
    QVERIFY(list);
    QCOMPARE(list->count(), 0);
    QVERIFY2(!list->isVisibleTo(&dlg), "an empty list is still taking the space");

    bool hint = false;
    const auto labels = dlg.findChildren<QLabel *>();
    for (QLabel *l : labels) {
        if (l->isVisibleTo(&dlg) && l->text().contains(QLatin1String("Nothing here yet"))) {
            hint = true;
        }
    }
    QVERIFY2(hint, "no empty state on the recents list");
}

QTEST_MAIN(WelcomeDialogTest)
#include "WelcomeDialogTest.moc"
