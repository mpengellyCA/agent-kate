#include "TerminalPanel.h"

#include <KLocalizedString>
#include <KPluginFactory>
#include <KPluginMetaData>
#include <KParts/ReadOnlyPart>

#include <kde_terminal_interface.h>

#include <QDir>
#include <QIcon>
#include <QLabel>
#include <QTabWidget>
#include <QTimer>
#include <QToolButton>
#include <QVBoxLayout>

TerminalPanel::TerminalPanel(QWidget *parent)
    : QWidget(parent)
    , m_workdir(QDir::homePath())
{
    m_tabs = new QTabWidget(this);
    m_tabs->setTabsClosable(true);
    m_tabs->setMovable(true);
    m_tabs->setDocumentMode(true);
    connect(m_tabs, &QTabWidget::tabCloseRequested, this, [this](int index) {
        QWidget *w = m_tabs->widget(index);
        m_tabs->removeTab(index);
        if (w) {
            w->deleteLater(); // destroys the container -> the KPart -> the shell
        }
    });

    // A "+" button in the tab-bar corner opens another terminal.
    auto *addButton = new QToolButton(this);
    addButton->setIcon(QIcon::fromTheme(QStringLiteral("list-add")));
    addButton->setAutoRaise(true);
    addButton->setToolTip(i18n("New Terminal"));
    connect(addButton, &QToolButton::clicked, this, &TerminalPanel::newTerminal);
    m_tabs->setCornerWidget(addButton, Qt::TopRightCorner);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->addWidget(m_tabs);

    // Open the first terminal once the window has finished assembling, so it
    // picks up the active project's directory.
    QTimer::singleShot(0, this, &TerminalPanel::newTerminal);
}

void TerminalPanel::setWorkingDirectory(const QString &dir)
{
    if (!dir.isEmpty()) {
        m_workdir = dir;
    }
}

void TerminalPanel::newTerminal()
{
    if (m_konsoleMissing) {
        return;
    }
    QWidget *session = createSession();
    if (!session) {
        return;
    }
    const int idx = m_tabs->addTab(session, i18n("Terminal %1", ++m_counter));
    m_tabs->setCurrentIndex(idx);
    if (QWidget *fw = session->focusWidget()) {
        fw->setFocus();
    }
}

// createSession builds one tab: a container hosting a fresh Konsole KPart, or a
// message label if the terminal could not be loaded.
QWidget *TerminalPanel::createSession()
{
    auto *container = new QWidget(m_tabs);
    auto *layout = new QVBoxLayout(container);
    layout->setContentsMargins(0, 0, 0, 0);

    const auto factoryResult =
        KPluginFactory::loadFactory(KPluginMetaData(QStringLiteral("kf6/parts/konsolepart")));
    if (!factoryResult) {
        m_konsoleMissing = true;
        auto *msg = new QLabel(
            i18n("The Konsole terminal could not be loaded.\n"
                 "Install the 'konsole' package to use the integrated terminal."),
            container);
        msg->setAlignment(Qt::AlignCenter);
        msg->setWordWrap(true);
        layout->addWidget(msg);
        return container;
    }

    KParts::ReadOnlyPart *part =
        factoryResult.plugin->create<KParts::ReadOnlyPart>(container, container);
    if (!part || !part->widget()) {
        auto *msg = new QLabel(i18n("Could not start a terminal session."), container);
        msg->setAlignment(Qt::AlignCenter);
        layout->addWidget(msg);
        return container;
    }
    layout->addWidget(part->widget());

    if (auto *terminal = qobject_cast<TerminalInterface *>(part)) {
        terminal->showShellInDir(m_workdir);
    }
    return container;
}
