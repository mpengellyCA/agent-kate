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
#include <QToolButton>
#include <QVBoxLayout>

namespace {
// Each session widget carries the absolute project path it was started under,
// stashed as a dynamic property so we can show/hide on project switch without
// a side-table.
constexpr const char *kProjectProp = "ak.projectPath";
} // namespace

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
    // First terminal is created lazily by setWorkingDirectory once the active
    // project is known — opening it eagerly here would anchor the session to
    // the home directory.
}

void TerminalPanel::setWorkingDirectory(const QString &dir)
{
    if (dir.isEmpty() || dir == m_workdir) {
        // Same project — but on the very first call we may still have no tabs
        // because the constructor deferred to us. Make sure one exists.
        if (dir == m_workdir && m_tabs->count() == 0 && !m_konsoleMissing) {
            newTerminal();
        }
        return;
    }
    m_workdir = dir;
    applyVisibility();
    // Ensure the active project has at least one visible tab.
    bool anyVisible = false;
    for (int i = 0; i < m_tabs->count(); ++i) {
        if (m_tabs->isTabVisible(i)) {
            anyVisible = true;
            break;
        }
    }
    if (!anyVisible && !m_konsoleMissing) {
        newTerminal();
    }
}

void TerminalPanel::applyVisibility()
{
    int firstVisible = -1;
    for (int i = 0; i < m_tabs->count(); ++i) {
        QWidget *w = m_tabs->widget(i);
        const QString proj = w ? w->property(kProjectProp).toString() : QString();
        const bool visible = (proj == m_workdir);
        m_tabs->setTabVisible(i, visible);
        if (visible && firstVisible < 0) {
            firstVisible = i;
        }
    }
    if (firstVisible >= 0) {
        m_tabs->setCurrentIndex(firstVisible);
    }
}

void TerminalPanel::newTerminal()
{
    openTerminalAt(m_workdir);
}

void TerminalPanel::openTerminalAt(const QString &dir)
{
    if (m_konsoleMissing) {
        return;
    }
    const QString cwd = dir.isEmpty() ? m_workdir : dir;
    QWidget *session = createSession(cwd);
    if (!session) {
        return;
    }
    // Tab is still owned by the active project for show/hide scoping; only the
    // initial shell CWD differs from the project root.
    session->setProperty(kProjectProp, m_workdir);
    const int idx = m_tabs->addTab(session, i18n("Terminal %1", ++m_counter));
    // Re-apply visibility so a freshly created tab in a non-active project
    // (shouldn't happen, but defensive) doesn't accidentally show up.
    applyVisibility();
    if (m_tabs->isTabVisible(idx)) {
        m_tabs->setCurrentIndex(idx);
        if (QWidget *fw = session->focusWidget()) {
            fw->setFocus();
        }
    }
}

// createSession builds one tab: a container hosting a fresh Konsole KPart, or a
// message label if the terminal could not be loaded.
QWidget *TerminalPanel::createSession(const QString &cwd)
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
        terminal->showShellInDir(cwd);
    }
    return container;
}
