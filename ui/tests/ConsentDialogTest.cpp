// The Cowork consent prompt's honesty properties (audit F50/F31).
//
// This dialog is the last thing between a prompt-injected agent and the user's
// screen/keyboard, so what it SAYS and what it PRESELECTS are security surface:
//  - it must never render a raw internal capability key at the user ("access your
//    desktop (launch_browser)"), and it must share one vocabulary with the Cowork
//    panel so the two cannot drift;
//  - it must preselect the narrowest scope, never the core's wider suggestion;
//  - the target fragment interpolates an attacker-chosen window title into a
//    RichText label, so it must be escaped.
//
// Deny-is-default is asserted too — that is the pattern F31 points the KMessageBox
// dialogs at, and it would be quietly lost in a refactor.

#include "cowork/CapabilityText.h"
#include "cowork/ConsentDialog.h"

#include <QApplication>
#include <QComboBox>
#include <QDialog>
#include <QJsonObject>
#include <QLabel>
#include <QPushButton>
#include <QStringList>
#include <QTest>
#include <QTimer>

namespace {

QJsonObject request(const QString &cap, const QJsonObject &target = {},
                    const QString &suggested = QString())
{
    QJsonObject r{{QStringLiteral("capability"), cap},
                  {QStringLiteral("threadTitle"), QStringLiteral("agent-7")},
                  {QStringLiteral("target"), target}};
    if (!suggested.isEmpty()) {
        r.insert(QStringLiteral("suggestedScope"), suggested);
    }
    return r;
}

QString allText(const ConsentDialog &dlg)
{
    QString out;
    const auto labels = dlg.findChildren<QLabel *>();
    for (const QLabel *l : labels) {
        out += l->text() + QLatin1Char('\n');
    }
    return out;
}

} // namespace

class ConsentDialogTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    // Every capability the core can ask for must have human copy in the shared map,
    // in all three registers. A missing entry is what produced "(launch_browser)".
    void sharedVocabularyCoversEveryCapability()
    {
        const QStringList keys{QStringLiteral("window_list"),   QStringLiteral("screenshot"),
                               QStringLiteral("a11y_read"),     QStringLiteral("screencast"),
                               QStringLiteral("launch_browser"), QStringLiteral("vd_sandbox"),
                               QStringLiteral("a11y_action"),   QStringLiteral("input_inject"),
                               QStringLiteral("pointer_control")};
        for (const QString &k : keys) {
            QVERIFY2(!CoworkCaps::verb(k).isEmpty(), qPrintable(k));
            QVERIFY2(CoworkCaps::verb(k) != k, qPrintable(k));
            QVERIFY2(CoworkCaps::title(k) != k, qPrintable(k));
            QVERIFY2(!CoworkCaps::description(k).isEmpty(), qPrintable(k));
        }
    }

    // "Sandbox" claims containment; a separate virtual desktop is an organizational
    // boundary only (audit F32).
    void separateDesktopIsNotCalledASandbox()
    {
        const QString k = QStringLiteral("vd_sandbox");
        QVERIFY(!CoworkCaps::verb(k).contains(QLatin1String("sandbox"), Qt::CaseInsensitive));
        QVERIFY(!CoworkCaps::title(k).contains(QLatin1String("sandbox"), Qt::CaseInsensitive));
        QVERIFY(!CoworkCaps::description(k).contains(QLatin1String("sandbox"), Qt::CaseInsensitive));

        ConsentDialog dlg(request(QStringLiteral("screenshot"),
                                  QJsonObject{{QStringLiteral("kind"), QStringLiteral("vdesktop")}}));
        QVERIFY(!allText(dlg).contains(QLatin1String("sandbox"), Qt::CaseInsensitive));
    }

    void promptNeverShowsARawCapabilityKey()
    {
        ConsentDialog browser(request(QStringLiteral("launch_browser")));
        const QString text = allText(browser);
        QVERIFY(!text.contains(QLatin1String("launch_browser")));
        QVERIFY(text.contains(QLatin1String("open a browser")));

        // A key this build has never heard of must still not leak; the copy says so.
        ConsentDialog unknown(request(QStringLiteral("frobnicate_v9")));
        QVERIFY(!allText(unknown).contains(QLatin1String("frobnicate_v9")));
    }

    // The core suggests "session" for browser/screencast. The preselection is the
    // user's default answer, so it must be the narrowest scope regardless.
    void scopePreselectsOnceEvenWhenTheCoreSuggestsWider()
    {
        for (const QString &suggested : {QStringLiteral("session"),
                                         QStringLiteral("until_revoked"),
                                         QStringLiteral("timed")}) {
            ConsentDialog dlg(request(QStringLiteral("launch_browser"), {}, suggested));
            QCOMPARE(dlg.scope(), QStringLiteral("once"));
            QCOMPARE(dlg.expiresInSec(), 0);
        }
        // …and the wider scopes are still offered, one selection away.
        ConsentDialog dlg(request(QStringLiteral("screenshot")));
        auto *combo = dlg.findChild<QComboBox *>();
        QVERIFY(combo);
        QVERIFY(combo->findData(QStringLiteral("session")) >= 0);
        QVERIFY(combo->findData(QStringLiteral("until_revoked")) >= 0);
    }

    // A window titles itself; the fragment lands in a RichText label.
    void attackerControlledWindowTitleIsEscaped()
    {
        ConsentDialog dlg(request(
            QStringLiteral("screenshot"),
            QJsonObject{{QStringLiteral("kind"), QStringLiteral("window")},
                        {QStringLiteral("label"), QStringLiteral("<b>Bank</b> — safe")}}));
        const QString text = allText(dlg);
        QVERIFY(text.contains(QLatin1String("&lt;b&gt;Bank&lt;/b&gt;")));
        QVERIFY(!text.contains(QLatin1String("<b>Bank</b>")));
    }

    // SECURITY (audit F35): this test used to loop over the buttons and assert something
    // only if one of them ALREADY reported isDefault() — so deleting
    // `deny->setDefault(true); deny->setFocus();` from ConsentDialog.cpp left it green.
    // A test that passes with the fix removed certifies the bug. Two pins now, and the
    // deletion fails both: the widget state here, and what Enter actually does below.
    void denyIsTheDefaultButtonAndHoldsFocus()
    {
        ConsentDialog dlg(request(QStringLiteral("a11y_read")));
        QVERIFY(!dlg.allowed());

        QPushButton *def = nullptr;
        const auto buttons = dlg.findChildren<QPushButton *>();
        QVERIFY(!buttons.isEmpty());
        for (QPushButton *b : buttons) {
            if (b->isDefault()) {
                QVERIFY2(def == nullptr, "two default buttons: which one Enter fires is luck");
                def = b;
            }
        }
        QVERIFY2(def != nullptr,
                 "no button claims the default — Qt then assigns one at show time, and "
                 "QDialogButtonBox assigns the first AcceptRole button, i.e. Allow");
        QVERIFY2(def->text().contains(QLatin1String("Deny")), qPrintable(def->text()));
        QCOMPARE(dlg.focusWidget(), static_cast<QWidget *>(def));
    }

    // …and the property the widget state is a proxy for, observed on the live modal
    // dialog: a stray Enter on the highest-authority prompt in the product must DENY.
    void pressingEnterOnTheLiveDialogDenies()
    {
        ConsentDialog dlg(request(QStringLiteral("input_inject")));
        bool pressed = false;
        QTimer::singleShot(0, &dlg, [&dlg, &pressed] {
            // The shown, modal dialog — this is the point at which Qt has finished
            // assigning the default button.
            QWidget *modal = QApplication::activeModalWidget();
            pressed = true;
            QTest::keyClick(modal ? modal : &dlg, Qt::Key_Return);
        });
        // Watchdog: never hang the suite, and never let a hang read as a pass — done(-1)
        // is neither Accepted nor Rejected.
        bool timedOut = false;
        QTimer::singleShot(5000, &dlg, [&dlg, &timedOut] {
            timedOut = true;
            dlg.done(-1);
        });

        const int rc = dlg.exec();
        QVERIFY(pressed);
        QVERIFY2(!timedOut, "the dialog never acted on Enter");
        QCOMPARE(rc, int(QDialog::Rejected));
        QVERIFY2(!dlg.allowed(), "Enter allowed desktop access");
    }

    // The shared vocabulary is now the single authority for user-facing capability copy,
    // so its FALLBACKS have to be safe on their own terms: title() returned the raw key
    // and description() an empty string for anything it did not know (audit F35).
    void unknownCapabilityNeverLeaksItsRawKeyInAnyRegister()
    {
        const QString bogus = QStringLiteral("frobnicate_v9");
        QVERIFY(!CoworkCaps::verb(bogus).contains(bogus));
        QVERIFY(!CoworkCaps::title(bogus).contains(bogus));
        QVERIFY(!CoworkCaps::description(bogus).contains(bogus));
        // …and none of them is silently blank, which reads as "nothing to worry about".
        QVERIFY(!CoworkCaps::verb(bogus).isEmpty());
        QVERIFY(!CoworkCaps::title(bogus).isEmpty());
        QVERIFY(!CoworkCaps::description(bogus).isEmpty());
    }

    // SECURITY (audit F35, round 4): a whole-screen capture cannot be refused by name and
    // the blackout the core computes rectangles for is not implemented, so the core marks
    // the target `includesAgentKate` and the human must be TOLD. Before this the dialog
    // rendered "a whole screen" and threw the Label — the only channel the warning had —
    // away, so nobody ever saw it. An honest gap beats an unenforced refusal; a silent gap
    // beats neither.
    void wholeScreenCaptureSaysItIncludesAgentKateItself()
    {
        ConsentDialog warned(request(
            QStringLiteral("screenshot"),
            QJsonObject{{QStringLiteral("kind"), QStringLiteral("screen")},
                        {QStringLiteral("label"), QStringLiteral("the whole screen — includes the Agent Kate window")},
                        {QStringLiteral("includesAgentKate"), true}}));
        const QString text = allText(warned);
        QVERIFY2(text.contains(QLatin1String("Agent Kate window")), qPrintable(text));
        // …and specifically what is in it, since "our window" means nothing to a user.
        QVERIFY2(text.contains(QLatin1String("emergency stop"), Qt::CaseInsensitive)
                     || text.contains(QLatin1String("kill"), Qt::CaseInsensitive),
                 qPrintable(text));

        // A clean frame must NOT carry the warning: a caution shown every time is one the
        // user learns to click past, and the whole value of this sentence is that it is true.
        ConsentDialog clean(request(
            QStringLiteral("screenshot"),
            QJsonObject{{QStringLiteral("kind"), QStringLiteral("screen")},
                        {QStringLiteral("label"), QStringLiteral("the whole screen")}}));
        QVERIFY(!allText(clean).contains(QLatin1String("Agent Kate window")));
    }

    // A region capture is described by the rectangle the core resolved (it never passes the
    // agent's own label through for a region), and the fragment is still escaped.
    void regionTargetIsDescribedRatherThanCalledYourDesktop()
    {
        ConsentDialog dlg(request(
            QStringLiteral("screenshot"),
            QJsonObject{{QStringLiteral("kind"), QStringLiteral("region")},
                        {QStringLiteral("label"), QStringLiteral("a 60×30 <b>region</b> of the screen at (200,150)")}}));
        const QString text = allText(dlg);
        QVERIFY2(text.contains(QStringLiteral("60×30")), qPrintable(text));
        QVERIFY2(text.contains(QLatin1String("&lt;b&gt;region&lt;/b&gt;")), qPrintable(text));
        QVERIFY(!text.contains(QLatin1String("<b>region</b>")));

        // With no description at all it must still not be called "your desktop" — that is
        // the catch-all for an unrecognised kind and it understates a targeted grab.
        ConsentDialog bare(request(QStringLiteral("screenshot"),
                                   QJsonObject{{QStringLiteral("kind"), QStringLiteral("region")}}));
        QVERIFY2(allText(bare).contains(QLatin1String("area of your screen")), qPrintable(allText(bare)));
    }
};

QTEST_MAIN(ConsentDialogTest)
#include "ConsentDialogTest.moc"
