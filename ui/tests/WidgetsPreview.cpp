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
#include "theme/ThemeManager.h"

#include <QAction>
#include <QApplication>
#include <QKeySequence>
#include <QLabel>
#include <QMainWindow>

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

int main(int argc, char **argv)
{
    QApplication app(argc, argv);
    const QString mode = argc > 1 ? QString::fromUtf8(argv[1]) : QStringLiteral("appearance");
    const QString themeId = argc > 2 ? QString::fromUtf8(argv[2]) : QStringLiteral("midnight");
    ThemeManager::instance()->applyTheme(themeId, /*persist=*/false);

    if (mode == QLatin1String("palette")) {
        auto *backdrop = new QMainWindow;
        backdrop->setWindowTitle(QStringLiteral("Command Palette preview"));
        backdrop->resize(900, 560);
        auto *label = new QLabel(QStringLiteral("Command Palette preview backdrop"));
        label->setAlignment(Qt::AlignCenter);
        backdrop->setCentralWidget(label);
        backdrop->show();
        auto *pal = new CommandPalette(backdrop);
        pal->setActions(sampleActions(&app));
        pal->showPalette();
    } else {
        auto *dlg = new AppearanceDialog;
        dlg->show();
    }
    return app.exec();
}
