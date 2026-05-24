// agentkate — native multi-agent coding arena (C++/Qt6/KF6 KDE application).
#include "MainWindow.h"
#include "WelcomeDialog.h"

#include <QApplication>
#include <QCommandLineParser>
#include <QIcon>
#include <QImage>

#include <KAboutData>
#include <KLocalizedString>

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
                         i18n("AgentKate"),
                         QStringLiteral("0.1.0"),
                         i18n("Native multi-agent coding arena"),
                         KAboutLicense::LGPL_V2,
                         i18n("© 2026 The AgentKate Authors"));
    aboutData.setProgramLogo(QImage(QStringLiteral(":/branding/logo.png")));
    KAboutData::setApplicationData(aboutData);

    QCommandLineParser parser;
    aboutData.setupCommandLine(&parser);
    parser.addPositionalArgument(QStringLiteral("path"),
                                 i18n("File or project directory to open."));
    parser.process(app);
    aboutData.processCommandLine(&parser);

    QString openPath;
    if (!parser.positionalArguments().isEmpty()) {
        openPath = parser.positionalArguments().constFirst();
    }

    // No path on the command line? Show the welcome dialog so the user can
    // pick a recent project, open a folder, or create a new one — otherwise
    // we would silently fall back to the current working directory (which is
    // $HOME for the installed .desktop launcher).
    if (openPath.isEmpty()) {
        WelcomeDialog welcome;
        if (welcome.exec() != QDialog::Accepted) {
            return 0;
        }
        openPath = welcome.selectedPath();
        if (openPath.isEmpty()) {
            return 0;
        }
    }

    auto *window = new MainWindow(openPath);
    window->show();
    return app.exec();
}
