// agentkate — native multi-agent coding arena (C++/Qt6/KF6 KDE application).
#include "MainWindow.h"
#include "WelcomeDialog.h"
#include "theme/ThemeManager.h"

#include <QApplication>
#include <QCommandLineParser>
#include <QDialog>
#include <QDir>
#include <QIcon>
#include <QImage>
#include <QPointer>
#include <QWindow>

#include <KAboutData>
#include <KDBusService>
#include <KLocalizedString>
#include <KWindowSystem>

#include <cstdio>

// Flushing log handler: Qt's default handler block-buffers stderr when it is
// not a tty, which loses output on abnormal exit. Flushing every line keeps
// the UI's own logs and the relayed akcore logs visible and ordered.
static void messageHandler(QtMsgType type, const QMessageLogContext &, const QString &msg)
{
    const char *level = "INFO ";
    switch (type) {
    case QtDebugMsg:    level = "DEBUG"; break;
    case QtInfoMsg:     level = "INFO "; break;
    case QtWarningMsg:  level = "WARN "; break;
    case QtCriticalMsg: level = "ERROR"; break;
    case QtFatalMsg:    level = "FATAL"; break;
    }
    std::fprintf(stderr, "%s [agentkate] %s\n", level, qPrintable(msg));
    std::fflush(stderr);
}

int main(int argc, char *argv[])
{
    qInstallMessageHandler(messageHandler);

    QApplication app(argc, argv);
    KLocalizedString::setApplicationDomain("agentkate");

    // Apply Agent Kate's own appearance before any window is shown. This lets
    // the app wear its signature identity (or a deliberately different KDE
    // scheme) independent of the rest of the desktop. Must run while qApp's
    // palette is still the genuine system one so "Follow System" can restore it.
    ThemeManager::instance()->applySavedOrDefault();

    // The hicolor PNGs are installed system-wide by CMake, but also live in
    // the binary as Qt resources so the icon shows up when running straight
    // from the build directory (uninstalled dogfood loop) and on systems where
    // the hicolor theme cache hasn't picked them up yet.
    QIcon::setFallbackSearchPaths(QIcon::fallbackSearchPaths()
                                  << QStringLiteral(":/icons/hicolor/256x256/apps"));
    QIcon appIcon = QIcon::fromTheme(QStringLiteral("agentkate"));
    if (appIcon.isNull()) {
        appIcon = QIcon(QStringLiteral(":/icons/hicolor/256x256/apps/agentkate.png"));
        for (const QString &size : {QStringLiteral("32"), QStringLiteral("48"),
                                    QStringLiteral("64"), QStringLiteral("128"),
                                    QStringLiteral("256")}) {
            appIcon.addFile(QStringLiteral(":/icons/hicolor/%1x%1/apps/agentkate.png").arg(size));
        }
    }
    QApplication::setWindowIcon(appIcon);

    KAboutData aboutData(QStringLiteral("agentkate"),
                         i18n("Agent Kate"),
                         QStringLiteral("0.1.0"),
                         i18n("Native multi-agent coding arena"),
                         KAboutLicense::LGPL_V2,
                         i18n("© 2026 The Agent Kate Authors"));
    aboutData.setProgramLogo(QImage(QStringLiteral(":/branding/logo.png")));
    // Names the installed .desktop entry. It is what KDBusService derives its
    // unique bus name from, what a Wayland compositor matches the window to its
    // launcher by, and what the notifyrc's DesktopEntry= points back at.
    aboutData.setDesktopFileName(QStringLiteral("org.kde.agentkate"));
    KAboutData::setApplicationData(aboutData);

    QCommandLineParser parser;
    aboutData.setupCommandLine(&parser);
    parser.addPositionalArgument(QStringLiteral("path"),
                                 i18n("File or project directory to open."));
    parser.process(app);
    aboutData.processCommandLine(&parser);

    // Single instance. Constructed AFTER parser.process (so --help/--version
    // still work standalone) and BEFORE the welcome dialog, so a second launch
    // pokes the running window instead of putting up a dialog it will discard.
    // The second process exits from inside this constructor.
    //
    // The bus name is the reversed organization domain plus the application
    // name, so pinning the domain is what makes it org.kde.agentkate — the same
    // id as the .desktop entry — instead of an implicit "local." fallback.
    // NoExitOnFailure keeps a session with no (or a broken) D-Bus launchable:
    // uniqueness cannot be enforced there anyway, and refusing to start is a
    // far worse answer than starting twice.
    app.setOrganizationDomain(QStringLiteral("kde.org"));
    KDBusService service(KDBusService::Unique | KDBusService::NoExitOnFailure);

    QString openPath;
    if (!parser.positionalArguments().isEmpty()) {
        openPath = parser.positionalArguments().constFirst();
    }

    // A second `agentkate /some/project` does not start a process that lives;
    // KDBusService forwards its command line to us and exits. Recover the same
    // positional argument our own startup honours, resolved against the OTHER
    // process's working directory — relative paths mean nothing in ours.
    //
    // The remote parser is built exactly the way startup's is: KAboutData adds
    // the value-taking options (--desktopfile <name>, --author, …), and a parser
    // that does not know an option takes a value mistakes that value for the
    // positional path — `agentkate --desktopfile org.kde.agentkate` would then
    // "open" a project called org.kde.agentkate.
    auto pathFromArguments = [&aboutData](const QStringList &arguments,
                                          const QString &workingDirectory) -> QString {
        QCommandLineParser remote;
        aboutData.setupCommandLine(&remote);
        remote.addPositionalArgument(QStringLiteral("path"), QString());
        remote.parse(arguments);
        const QStringList positional = remote.positionalArguments();
        if (positional.isEmpty()) {
            return QString();
        }
        const QString path = positional.constFirst();
        if (workingDirectory.isEmpty()) {
            return path;
        }
        return QDir(workingDirectory).absoluteFilePath(path);
    };

    // The unique bus name is taken the moment KDBusService is constructed, so
    // activations can land while the welcome dialog is still up and the window
    // does not exist. Answer them from here until it does, otherwise those
    // launches are silently swallowed: a forwarded path becomes the dialog's
    // answer, a bare "activate" just brings the dialog forward.
    QString forwardedPath;
    // The activation token that came with a forwarded path, held until there is
    // a window to spend it on (see below).
    QString forwardedToken;
    QPointer<QDialog> activeDialog;
    const QMetaObject::Connection earlyActivation = QObject::connect(
        &service, &KDBusService::activateRequested, &app,
        [&](const QStringList &arguments, const QString &workingDirectory) {
            const QString path = pathFromArguments(arguments, workingDirectory);
            if (!path.isEmpty()) {
                forwardedPath = path;
                // KDBusService parks the XDG activation token in the environment
                // for the duration of this signal only, and the window it should
                // activate does not exist yet — take it now and spend it once the
                // window is up, or the launch the user just made comes up behind
                // whatever they were looking at.
                forwardedToken = qEnvironmentVariable("XDG_ACTIVATION_TOKEN");
                qunsetenv("XDG_ACTIVATION_TOKEN");
                if (activeDialog) {
                    activeDialog->reject(); // the forwarded path IS the choice
                }
                return;
            }
            if (activeDialog) {
                activeDialog->show();
                KWindowSystem::updateStartupId(activeDialog->windowHandle());
                activeDialog->raise();
                KWindowSystem::activateWindow(activeDialog->windowHandle());
            }
        });

    // No path on the command line? Show the welcome dialog so the user can
    // pick a recent project, open a folder, or create a new one — otherwise
    // we would silently fall back to the current working directory (which is
    // $HOME for the installed .desktop launcher).
    if (openPath.isEmpty()) {
        WelcomeDialog welcome;
        activeDialog = &welcome;
        const int result = welcome.exec();
        activeDialog.clear();
        if (!forwardedPath.isEmpty()) {
            openPath = forwardedPath;
            forwardedPath.clear();
        } else if (result != QDialog::Accepted) {
            return 0;
        } else {
            openPath = welcome.selectedPath();
            if (openPath.isEmpty()) {
                return 0;
            }
        }
    }

    auto *window = new MainWindow(openPath);
    window->show();
    if (!forwardedToken.isEmpty()) {
        window->raiseAndActivate(forwardedToken);
        forwardedToken.clear();
    }

    // Re-launching (taskbar, KRunner, the .desktop entry, `agentkate <path>`)
    // feeds the existing window instead of starting a second arena: a forwarded
    // path is opened exactly as a startup argument would be, and either way the
    // window comes forward. updateStartupId consumes the XDG activation token
    // KDBusService parked in the environment for exactly the duration of this
    // signal — without it a Wayland compositor refuses the activation and only
    // flags the task bar entry.
    QObject::disconnect(earlyActivation);
    QObject::connect(&service, &KDBusService::activateRequested, window,
                     [window, pathFromArguments](const QStringList &arguments,
                                                 const QString &workingDirectory) {
                         const QString path =
                             pathFromArguments(arguments, workingDirectory);
                         if (!path.isEmpty()) {
                             window->openLaunchPath(path);
                         }
                         KWindowSystem::updateStartupId(window->windowHandle());
                         window->raiseAndActivate();
                     });

    return app.exec();
}
