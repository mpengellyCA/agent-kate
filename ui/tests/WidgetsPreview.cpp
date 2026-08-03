// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// WidgetsPreview — a tiny developer harness that shows the new appearance
// dialog and command palette under a chosen Agent Kate theme, so they can be
// screenshotted/verified in isolation. Not installed; build-tree only.
//
//   akwidgets-preview appearance      # show the Appearance dialog
//   akwidgets-preview palette         # show the Command Palette over a backdrop
//   (optional 2nd arg: a theme id, e.g. midnight | daylight | system)

#include "AppearanceDialog.h"
#include "CommandPalette.h"
#include "DiffView.h"
#include "theme/ThemeManager.h"

#include <QAction>
#include <QApplication>
#include <QKeySequence>
#include <QLabel>
#include <QMainWindow>
#include <QString>

static QList<QAction *> sampleActions(QObject *parent)
{
    struct C { const char *text; const char *shortcut; const char *icon; };
    static const C cmds[] = {
        {"Open Project…", "Ctrl+O", "folder-open"},
        {"New Terminal", "Ctrl+Shift+T", "utilities-terminal"},
        {"Command Palette…", "Ctrl+Shift+P", "show-menu"},
        {"Appearance…", "", "preferences-desktop-color"},
        {"Find in Project…", "Ctrl+Shift+F", "edit-find"},
        {"Go to Definition", "F12", ""},
        {"Show Git Blame", "Ctrl+Shift+B", ""},
        {"Code Focus", "Ctrl+Shift+1", ""},
        {"Chat Focus", "Ctrl+Shift+2", ""},
        {"Review Layout", "Ctrl+Shift+3", ""},
        {"Configure API Providers…", "", ""},
        {"Manage Claude Skills…", "", "preferences-plugin"},
        {"Format Document", "Ctrl+Alt+L", ""},
        {"Toggle Bottom Panel", "Ctrl+J", ""},
    };
    QList<QAction *> out;
    for (const C &c : cmds) {
        auto *a = new QAction(QString::fromUtf8(c.text), parent);
        if (c.shortcut[0])
            a->setShortcut(QKeySequence(QString::fromUtf8(c.shortcut)));
        if (c.icon[0])
            a->setIcon(QIcon::fromTheme(QString::fromUtf8(c.icon)));
        out << a;
    }
    return out;
}

static QList<CommandPalette::Entry> sampleEntries(QObject *parent)
{
    QList<CommandPalette::Entry> entries;
    for (QAction *action : sampleActions(parent)) {
        entries << CommandPalette::Entry{action, QStringLiteral("Preview"), false, true};
    }
    return entries;
}

int main(int argc, char **argv)
{
    QApplication app(argc, argv);
    const QString mode = argc > 1 ? QString::fromUtf8(argv[1]) : QStringLiteral("appearance");
    const QString themeId = argc > 2 ? QString::fromUtf8(argv[2]) : QStringLiteral("midnight");
    ThemeManager::instance()->applyTheme(themeId, /*persist=*/false);

    if (mode == QLatin1String("diff")) {
        // Host the real DiffView in a deliberately NARROW window so its top-bar
        // FlowLayout (summary / split toggle / "Jump to:" / file combo) is seen
        // wrapping onto extra rows instead of clipping.
        const QString sample = QStringLiteral(
            "diff --git a/src/main.py b/src/main.py\n"
            "index 1111111..2222222 100644\n"
            "--- a/src/main.py\n"
            "+++ b/src/main.py\n"
            "@@ -1,3 +1,4 @@\n"
            " def main():\n"
            "-    print(\"old\")\n"
            "+    print(\"new\")\n"
            "+    print(\"added line\")\n"
            " \n"
            "diff --git a/README.md b/README.md\n"
            "index 3333333..4444444 100644\n"
            "--- a/README.md\n"
            "+++ b/README.md\n"
            "@@ -1,2 +1,2 @@\n"
            "-# Old Title\n"
            "+# New Title\n");
        const int w = argc > 3 ? QString::fromUtf8(argv[3]).toInt() : 360;
        auto *diff = new DiffView(sample);
        diff->setWindowTitle(QStringLiteral("DiffView (narrow)"));
        diff->resize(w > 0 ? w : 360, 600);
        diff->show();
        return app.exec();
    }

    if (mode == QLatin1String("palette")) {
        auto *backdrop = new QMainWindow;
        backdrop->setWindowTitle(QStringLiteral("Command Palette preview"));
        backdrop->resize(900, 560);
        auto *label = new QLabel(QStringLiteral("Command Palette preview backdrop"));
        label->setAlignment(Qt::AlignCenter);
        backdrop->setCentralWidget(label);
        backdrop->show();
        auto *pal = new CommandPalette(backdrop);
        pal->setActions(sampleEntries(&app));
        pal->showPalette();
    } else {
        auto *dlg = new AppearanceDialog;
        dlg->show();
    }
    return app.exec();
}
