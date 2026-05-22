// agentkate — native multi-agent coding arena (C++/Qt6/KF6 KDE application).
#include "MainWindow.h"

#include <QApplication>
#include <QCommandLineParser>

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

    KAboutData aboutData(QStringLiteral("agentkate"),
                         i18n("AgentKate"),
                         QStringLiteral("0.1.0"),
                         i18n("Native multi-agent coding arena"),
                         KAboutLicense::MIT,
                         i18n("© 2026 Leadrix"));
    aboutData.addAuthor(i18n("Mike"), i18n("Creator"), QStringLiteral("mike@leadrix.io"));
    aboutData.setHomepage(QStringLiteral("https://leadrix.io"));
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

    auto *window = new MainWindow(openPath);
    window->show();
    return app.exec();
}
