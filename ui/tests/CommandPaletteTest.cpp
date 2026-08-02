// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// Plan 27 §1: the palette is fed from the action collection, not the menu bar,
// and Simple mode stops being a wall.
//
// The behaviour under test is one decision made twice over: WHICH commands the
// palette will show. It used to drop every invisible action, which meant that
// in Simple mode — the mode a newcomer is in, and the mode where finding things
// by name matters most — searching for a hidden command returned nothing at
// all. The palette is supposed to be the escape hatch out of Simple mode; a
// silent empty result makes it a second wall.
//
// So: hidden commands are LISTED AND TAGGED, unavailable ones are still dropped
// (they cannot run; offering them is a dead end), and a panel's commands carry
// their panel's name so they can be found by it.
//
// The subtlety worth pinning, and the reason Entry carries `available` at all:
// QAction::setVisible(false) clears the action's `enabled` flag as a Qt SIDE
// EFFECT, so a hidden action always answers "disabled" and cannot be asked
// whether it would work. Reading that answer literally is what dropped every
// hidden command; ignoring it entirely would be worse — it would let the
// palette run a command the menu bar refuses to offer, which in this
// application means Create Pull Request on an agent with no branch to open one
// from. Both directions are tested.

#include "CommandPalette.h"

#include <QAction>
#include <QApplication>
#include <QLineEdit>
#include <QListWidget>
#include <QListWidgetItem>
#include <QSignalSpy>
#include <QString>
#include <QStringList>
#include <QTest>
#include <QWidget>

namespace {

QListWidget *listOf(CommandPalette *palette)
{
    return palette->findChild<QListWidget *>(QStringLiteral("commandPaletteList"));
}

// The rows currently offered, by display text.
QStringList rowTexts(CommandPalette *palette)
{
    QStringList out;
    QListWidget *list = listOf(palette);
    if (!list) {
        return out;
    }
    for (int i = 0; i < list->count(); ++i) {
        out << list->item(i)->text();
    }
    return out;
}

QListWidgetItem *rowNamed(CommandPalette *palette, const QString &text)
{
    QListWidget *list = listOf(palette);
    if (!list) {
        return nullptr;
    }
    for (int i = 0; i < list->count(); ++i) {
        if (list->item(i)->text() == text) {
            return list->item(i);
        }
    }
    return nullptr;
}

} // namespace

class CommandPaletteTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void hiddenCommandsAreListedAndTagged();
    void visibleCommandsAreNotTagged();
    void callerCanTagACommandTheAppStillShows();
    void disabledCommandsAreDropped();
    void hiddenAndUnavailableCommandsAreDropped();
    void panelGroupPrefixesAndIsSearchable();
    void triggeringARowRunsTheAction();

private:
    QWidget m_host;
};

// The core of it. An action MainWindow has hidden for Simple mode must still be
// reachable by name — and must say why it is not in a menu.
void CommandPaletteTest::hiddenCommandsAreListedAndTagged()
{
    CommandPalette palette(&m_host);
    QAction hidden(QStringLiteral("&Format Document"), this);
    hidden.setVisible(false);
    // Qt has just cleared enabled as a side effect — the caller's `available`
    // is the only trustworthy answer for a hidden action.
    QVERIFY(!hidden.isEnabled());

    palette.setActions({{&hidden, QString(), false, /*available=*/true}});
    palette.showPalette();

    QCOMPARE(rowTexts(&palette), QStringList{QStringLiteral("Format Document")});
    QListWidgetItem *row = rowNamed(&palette, QStringLiteral("Format Document"));
    QVERIFY(row);
    QVERIFY2(row->data(CommandPalette::AdvancedRole).toBool(),
             "a command hidden by Simple mode must be tagged Advanced, so the "
             "user knows why it is not in a menu");
    // The delegate PAINTS the tag, and painted text reaches no screen reader.
    // The tooltip is the accessible copy of the same fact.
    QVERIFY2(row->toolTip().contains(QStringLiteral("Advanced")),
             qPrintable(QStringLiteral("tag missing from the tooltip: ")
                        + row->toolTip()));
}

void CommandPaletteTest::visibleCommandsAreNotTagged()
{
    CommandPalette palette(&m_host);
    QAction shown(QStringLiteral("&New Agent"), this);

    palette.setActions({{&shown, QString(), false, true}});
    palette.showPalette();

    QListWidgetItem *row = rowNamed(&palette, QStringLiteral("New Agent"));
    QVERIFY(row);
    QVERIFY2(!row->data(CommandPalette::AdvancedRole).toBool(),
             "an ordinary visible command must not be tagged Advanced");
    QVERIFY2(!row->toolTip().contains(QStringLiteral("Advanced")),
             "an ordinary visible command must not be tagged Advanced");
}

// The Code menu is hidden wholesale in Simple mode by hiding its MENU action —
// which leaves its children individually visible. Visibility alone therefore
// cannot classify them, so the caller says so, and the palette must honour it.
void CommandPaletteTest::callerCanTagACommandTheAppStillShows()
{
    CommandPalette palette(&m_host);
    QAction childOfHiddenMenu(QStringLiteral("Go to &Definition"), this);
    QVERIFY(childOfHiddenMenu.isVisible()); // the child itself was never hidden

    palette.setActions({{&childOfHiddenMenu, QString(), /*advanced=*/true, true}});
    palette.showPalette();

    QListWidgetItem *row = rowNamed(&palette, QStringLiteral("Go to Definition"));
    QVERIFY(row);
    QVERIFY2(row->data(CommandPalette::AdvancedRole).toBool(),
             "the caller marked this Advanced (it lives in a menu Simple mode "
             "hides whole) and the palette ignored it");
}

// The counterpart, and the reason "list everything" is not the rule: a disabled
// command cannot run. Offering it would be a dead end, and unlike a hidden one
// there is nothing the user can do about it from here.
void CommandPaletteTest::disabledCommandsAreDropped()
{
    CommandPalette palette(&m_host);
    QAction unavailable(QStringLiteral("&Merge the Agent's Changes…"), this);
    unavailable.setEnabled(false);
    QAction usable(QStringLiteral("&New Agent"), this);

    palette.setActions({{&unavailable, QString(), false, true},
                        {&usable, QString(), false, true}});
    palette.showPalette();

    QCOMPARE(rowTexts(&palette), QStringList{QStringLiteral("New Agent")});
}

// The security half, and the reason `available` is not simply assumed true for
// hidden commands. Simple mode hides Create Pull Request; an agent working in
// the user's own checkout has no branch to open one from, so the &Agent menu
// disables it and the core refuses it outright. If the palette listed it
// anyway, the audit's gate would have a second door standing open — the palette
// triggers actions directly, and QAction::trigger() does not re-check
// enablement.
void CommandPaletteTest::hiddenAndUnavailableCommandsAreDropped()
{
    CommandPalette palette(&m_host);
    QAction gated(QStringLiteral("Create &Pull Request…"), this);
    gated.setVisible(false);          // Simple mode
    QAction usable(QStringLiteral("&New Agent"), this);

    palette.setActions({{&gated, QString(), true, /*available=*/false},
                        {&usable, QString(), false, true}});
    palette.showPalette();

    QCOMPARE(rowTexts(&palette), QStringList{QStringLiteral("New Agent")});
}

// A panel's own toolbar commands appear in no menu. They arrive through
// MainWindow::registerCommands with the panel's name, which both namespaces
// them and makes them findable by typing the panel.
void CommandPaletteTest::panelGroupPrefixesAndIsSearchable()
{
    CommandPalette palette(&m_host);
    QAction warn(QStringLiteral("Show warnings"), this);

    palette.setActions({{&warn, QStringLiteral("Problems"), false, true}});
    palette.showPalette();

    QCOMPARE(rowTexts(&palette),
             QStringList{QStringLiteral("Problems: Show warnings")});

    // Typing the panel's name finds it — that is the point of the prefix.
    auto *search = palette.findChild<QLineEdit *>(QStringLiteral("commandPaletteSearch"));
    QVERIFY(search);
    search->setText(QStringLiteral("problems"));
    QCOMPARE(rowTexts(&palette).size(), 1);
}

// The palette must still run what it lists — including a hidden command, which
// is the whole reason for listing it.
void CommandPaletteTest::triggeringARowRunsTheAction()
{
    CommandPalette palette(&m_host);
    QAction hidden(QStringLiteral("&Format Document"), this);
    hidden.setVisible(false);
    QSignalSpy fired(&hidden, &QAction::triggered);

    palette.setActions({{&hidden, QString(), false, true}});
    palette.showPalette();
    QListWidget *list = listOf(&palette);
    QVERIFY(list);
    QCOMPARE(list->count(), 1);
    list->setCurrentRow(0);

    // The palette closes first and triggers on the event loop, so pump it.
    QTest::keyClick(palette.findChild<QLineEdit *>(QStringLiteral("commandPaletteSearch")),
                    Qt::Key_Return);
    QTRY_COMPARE(fired.count(), 1);
}

QTEST_MAIN(CommandPaletteTest)
#include "CommandPaletteTest.moc"
