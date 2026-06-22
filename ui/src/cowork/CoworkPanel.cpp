#include "CoworkPanel.h"

#include "BrowserLaunch.h"
#include "ConsentDialog.h"
#include "ControlConsentDialog.h"
#include "ipc/CoreClient.h"

#include <KLocalizedString>
#include <KMessageBox>
#include <KMessageWidget>

#include <QAction>
#include <QCheckBox>
#include <QDateTime>
#include <QFileDialog>
#include <QFileInfo>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QMenu>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QToolButton>
#include <QTreeWidget>
#include <QVBoxLayout>

namespace {

QString targetSummary(const QJsonObject &t)
{
    const QString label = t.value(QStringLiteral("label")).toString();
    if (!label.isEmpty()) {
        return label;
    }
    const QString kind = t.value(QStringLiteral("kind")).toString();
    if (kind == QLatin1String("window")) {
        const QString rc = t.value(QStringLiteral("resourceClass")).toString();
        return rc.isEmpty() ? i18n("a window") : rc;
    }
    if (kind == QLatin1String("any")) {
        return i18n("whole desktop");
    }
    return kind;
}

QString capLabel(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("See open windows");
    if (key == QLatin1String("screenshot")) return i18n("Take screenshots");
    if (key == QLatin1String("a11y_read")) return i18n("Read on-screen text");
    if (key == QLatin1String("screencast")) return i18n("Watch the screen live");
    if (key == QLatin1String("vd_sandbox")) return i18n("Use a sandbox desktop");
    if (key == QLatin1String("a11y_action")) return i18n("Click buttons & controls");
    if (key == QLatin1String("input_inject")) return i18n("Type & click as me");
    return key;
}

QString expiryText(const QJsonObject &g)
{
    const QString exp = g.value(QStringLiteral("expiresAt")).toString();
    if (exp.isEmpty()) {
        const QString scope = g.value(QStringLiteral("scope")).toString();
        if (scope == QLatin1String("until_revoked")) {
            return i18n("until revoked");
        }
        if (scope == QLatin1String("session")) {
            return i18n("this session");
        }
        return i18n("once");
    }
    const QDateTime t = QDateTime::fromString(exp, Qt::ISODateWithMs);
    return t.isValid() ? t.toLocalTime().toString(QStringLiteral("HH:mm:ss")) : exp;
}

} // namespace

CoworkPanel::CoworkPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent), m_core(core)
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(8, 8, 8, 8);

    auto *title = new QLabel(i18n("<b>Cowork</b> — share your KDE desktop with agents"), this);
    title->setWordWrap(true);
    layout->addWidget(title);

    m_status = new KMessageWidget(this);
    m_status->setCloseButtonVisible(false);
    m_status->setText(i18n("Checking desktop integration…"));
    layout->addWidget(m_status);

    // Enable-for-active-agent row.
    auto *enableRow = new QHBoxLayout;
    m_activeLabel = new QLabel(i18n("No active agent."), this);
    m_activeLabel->setWordWrap(true);
    m_enableBtn = new QPushButton(i18n("Enable Cowork for this agent"), this);
    m_enableBtn->setIcon(QIcon::fromTheme(QStringLiteral("dialog-ok-apply")));
    m_enableBtn->setEnabled(false);
    connect(m_enableBtn, &QPushButton::clicked, this, &CoworkPanel::enableForActiveThread);
    enableRow->addWidget(m_activeLabel, 1);
    enableRow->addWidget(m_enableBtn);
    layout->addLayout(enableRow);

    // Capability switchboard: flip a capability ON to pre-authorize it for any
    // cowork-enabled agent — no per-action prompt while on (the kill-switch + audit
    // log remain the safety net). Populated from cowork.getPolicy.
    auto *capsBox = new QGroupBox(i18n("What agents may do (on = allowed without asking)"), this);
    m_capsLayout = new QVBoxLayout(capsBox);
    layout->addWidget(capsBox);

    // Browser launcher. Browsers hide their web content from the accessibility tree
    // unless started with the right flag/env, so agents can't read or click page
    // elements. This opens one with accessibility forced on (the menu lists browsers
    // found on PATH plus an "Other browser…" picker).
    auto *browserRow = new QHBoxLayout;
    auto *browserLabel = new QLabel(i18n("Open a browser agents can read:"), this);
    browserLabel->setWordWrap(true);
    m_browserBtn = new QToolButton(this);
    m_browserBtn->setText(i18n("Launch browser"));
    m_browserBtn->setIcon(QIcon::fromTheme(QStringLiteral("internet-web-browser")));
    m_browserBtn->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_browserBtn->setPopupMode(QToolButton::InstantPopup);
    m_browserBtn->setToolTip(i18n(
        "Launch a web browser with its accessibility tree enabled so agents can read\n"
        "and click page elements. The browser must be started fresh from here — if it\n"
        "is already running, quit it first."));
    m_browserMenu = new QMenu(m_browserBtn);
    m_browserBtn->setMenu(m_browserMenu);
    connect(m_browserMenu, &QMenu::aboutToShow, this, &CoworkPanel::rebuildBrowserMenu);
    browserRow->addWidget(browserLabel, 1);
    browserRow->addWidget(m_browserBtn);
    layout->addLayout(browserRow);

    // Active grants.
    auto *grantsBox = new QGroupBox(i18n("Active access"), this);
    auto *grantsLayout = new QVBoxLayout(grantsBox);
    m_grants = new QTreeWidget(grantsBox);
    m_grants->setRootIsDecorated(false);
    m_grants->setHeaderLabels({i18n("Agent"), i18n("Can"), i18n("Target"), i18n("Until")});
    m_grants->header()->setSectionResizeMode(QHeaderView::ResizeToContents);
    grantsLayout->addWidget(m_grants);
    m_revokeBtn = new QPushButton(i18n("Revoke selected"), grantsBox);
    m_revokeBtn->setIcon(QIcon::fromTheme(QStringLiteral("edit-delete")));
    connect(m_revokeBtn, &QPushButton::clicked, this, &CoworkPanel::revokeSelected);
    grantsLayout->addWidget(m_revokeBtn, 0, Qt::AlignRight);
    layout->addWidget(grantsBox, 1);

    // Kill-switch.
    m_killBtn = new QPushButton(i18n("Stop ALL desktop access"), this);
    m_killBtn->setIcon(QIcon::fromTheme(QStringLiteral("process-stop")));
    connect(m_killBtn, &QPushButton::clicked, this, &CoworkPanel::toggleKill);
    layout->addWidget(m_killBtn);

    // Audit log.
    auto *auditBox = new QGroupBox(i18n("Recent activity"), this);
    auto *auditLayout = new QVBoxLayout(auditBox);
    m_audit = new QPlainTextEdit(auditBox);
    m_audit->setReadOnly(true);
    m_audit->setMaximumBlockCount(2000);
    m_audit->setFrameShape(QFrame::NoFrame);
    auditLayout->addWidget(m_audit);
    layout->addWidget(auditBox, 1);

    connect(m_core, &CoreClient::notification, this, &CoworkPanel::onNotification);
    connect(m_core, &CoreClient::connected, this, &CoworkPanel::refresh);
    if (m_core->isConnected()) {
        refresh();
    }
}

void CoworkPanel::setActiveThread(const QString &threadId, const QString &title)
{
    m_activeThread = threadId;
    m_activeTitle = title;
    if (threadId.isEmpty()) {
        m_activeLabel->setText(i18n("No active agent."));
        m_enableBtn->setEnabled(false);
    } else {
        m_activeLabel->setText(i18n("Active agent: <b>%1</b>",
                                    (title.isEmpty() ? threadId : title).toHtmlEscaped()));
        m_enableBtn->setEnabled(m_available);
    }
}

void CoworkPanel::onNotification(const QString &method, const QJsonObject &params)
{
    if (method == QLatin1String("cowork.grantRequested")) {
        handleGrantRequested(params);
    } else if (method == QLatin1String("cowork.grantsChanged")) {
        refreshGrants();
        refreshAudit();
    } else if (method == QLatin1String("cowork.killSwitch")) {
        m_killed = params.value(QStringLiteral("on")).toBool();
        refreshStatus();
        refreshGrants();
        refreshPolicy(); // kill clears the toggles
    } else if (method == QLatin1String("cowork.policyChanged")) {
        refreshPolicy();
    }
}

void CoworkPanel::refresh()
{
    refreshStatus();
    refreshPolicy();
    refreshGrants();
    refreshAudit();
}

void CoworkPanel::refreshPolicy()
{
    m_core->call(QStringLiteral("cowork.getPolicy"), {}, [this](const QJsonObject &res, const QJsonObject &err) {
        if (!err.isEmpty()) {
            return;
        }
        const QJsonArray caps = res.value(QStringLiteral("capabilities")).toArray();
        for (const QJsonValue &cv : caps) {
            const QJsonObject c = cv.toObject();
            const QString key = c.value(QStringLiteral("key")).toString();
            const bool enabled = c.value(QStringLiteral("enabled")).toBool();
            const bool dangerous = c.value(QStringLiteral("tier")).toString() == QLatin1String("R2");
            QCheckBox *box = m_policyChecks.value(key, nullptr);
            if (!box) {
                box = new QCheckBox(dangerous ? i18n("⚠ %1", capLabel(key)) : capLabel(key), this);
                if (dangerous) {
                    box->setToolTip(i18n("High-risk: lets the agent act as you (type, click). "
                                         "The kill-switch and audit log are your safety net."));
                }
                // clicked() fires on user interaction only — not on setChecked() below.
                connect(box, &QCheckBox::clicked, this, [this, key](bool on) {
                    m_core->call(QStringLiteral("cowork.setPolicy"),
                                 {{QStringLiteral("capability"), key}, {QStringLiteral("enabled"), on}},
                                 nullptr, this);
                });
                m_capsLayout->addWidget(box);
                m_policyChecks.insert(key, box);
            }
            box->setChecked(enabled);
        }
    }, this);
}

void CoworkPanel::refreshStatus()
{
    m_core->call(QStringLiteral("cowork.status"), {}, [this](const QJsonObject &res, const QJsonObject &err) {
        if (!err.isEmpty()) {
            return;
        }
        m_available = res.value(QStringLiteral("available")).toBool();
        m_killed = res.value(QStringLiteral("killed")).toBool();
        const bool tampered = res.value(QStringLiteral("tampered")).toBool();
        if (tampered) {
            m_status->setMessageType(KMessageWidget::Error);
            m_status->setText(i18n("Consent audit integrity check FAILED — desktop access is disabled."));
        } else if (m_killed) {
            m_status->setMessageType(KMessageWidget::Error);
            m_status->setText(i18n("Kill-switch engaged — all desktop access is blocked."));
        } else if (!m_available) {
            m_status->setMessageType(KMessageWidget::Warning);
            m_status->setText(i18n("Desktop integration unavailable (no KDE portal/KWin on this session)."));
        } else {
            m_status->setMessageType(KMessageWidget::Information);
            m_status->setText(i18n("Desktop integration ready. Agents can only act with your explicit consent."));
        }
        m_killBtn->setText(m_killed ? i18n("Re-enable desktop access") : i18n("Stop ALL desktop access"));
        m_enableBtn->setEnabled(m_available && !m_activeThread.isEmpty());
    }, this);
}

void CoworkPanel::refreshGrants()
{
    m_core->call(QStringLiteral("cowork.listGrants"), {}, [this](const QJsonObject &res, const QJsonObject &err) {
        if (!err.isEmpty()) {
            return;
        }
        m_grants->clear();
        const QJsonArray grants = res.value(QStringLiteral("grants")).toArray();
        for (const QJsonValue &gv : grants) {
            const QJsonObject g = gv.toObject();
            // Skip already-revoked grants (the store keeps history).
            if (!g.value(QStringLiteral("revokedAt")).toString().isEmpty()) {
                continue;
            }
            auto *item = new QTreeWidgetItem(m_grants);
            item->setText(0, g.value(QStringLiteral("threadId")).toString());
            item->setText(1, g.value(QStringLiteral("capability")).toString());
            item->setText(2, targetSummary(g.value(QStringLiteral("target")).toObject()));
            item->setText(3, expiryText(g));
            item->setData(0, Qt::UserRole, g.value(QStringLiteral("id")).toString());
            if (g.value(QStringLiteral("tier")).toString() == QLatin1String("R2")) {
                item->setIcon(1, QIcon::fromTheme(QStringLiteral("dialog-warning")));
            }
        }
        m_revokeBtn->setEnabled(m_grants->topLevelItemCount() > 0);
    }, this);
}

void CoworkPanel::refreshAudit()
{
    QJsonObject p{{QStringLiteral("limit"), 100}};
    m_core->call(QStringLiteral("cowork.listAudit"), p, [this](const QJsonObject &res, const QJsonObject &err) {
        if (!err.isEmpty()) {
            return;
        }
        QStringList lines;
        const QJsonArray entries = res.value(QStringLiteral("entries")).toArray();
        for (const QJsonValue &ev : entries) {
            const QJsonObject e = ev.toObject();
            const QDateTime at = QDateTime::fromString(e.value(QStringLiteral("at")).toString(), Qt::ISODateWithMs);
            const QString ts = at.isValid() ? at.toLocalTime().toString(QStringLiteral("HH:mm:ss")) : QString();
            lines << QStringLiteral("%1  %2  %3  %4  %5")
                         .arg(ts,
                              e.value(QStringLiteral("kind")).toString(),
                              e.value(QStringLiteral("capability")).toString(),
                              e.value(QStringLiteral("threadId")).toString(),
                              e.value(QStringLiteral("detail")).toString());
        }
        m_audit->setPlainText(lines.join(QLatin1Char('\n')));
    }, this);
}

void CoworkPanel::handleGrantRequested(const QJsonObject &params)
{
    const QString requestId = params.value(QStringLiteral("requestId")).toString();
    const QString tier = params.value(QStringLiteral("riskTier")).toString();

    bool allow = false;
    QString scope = QStringLiteral("once");
    int expiresInSec = 0;

    if (tier == QLatin1String("R2")) {
        ControlConsentDialog dlg(params, this);
        dlg.exec();
        allow = dlg.allowed();
        scope = dlg.scope();
        expiresInSec = dlg.expiresInSec();
    } else {
        ConsentDialog dlg(params, this);
        dlg.exec();
        allow = dlg.allowed();
        scope = dlg.scope();
        expiresInSec = dlg.expiresInSec();
    }

    QJsonObject resp{
        {QStringLiteral("requestId"), requestId},
        {QStringLiteral("allow"), allow},
        {QStringLiteral("scope"), scope},
        {QStringLiteral("expiresInSec"), expiresInSec},
    };
    m_core->call(QStringLiteral("cowork.respondGrant"), resp, nullptr, this);
}

void CoworkPanel::revokeSelected()
{
    auto *item = m_grants->currentItem();
    if (!item) {
        return;
    }
    const QString id = item->data(0, Qt::UserRole).toString();
    if (id.isEmpty()) {
        return;
    }
    m_core->call(QStringLiteral("cowork.revokeGrant"),
                 {{QStringLiteral("id"), id}, {QStringLiteral("reason"), QStringLiteral("revoked from the Cowork panel")}},
                 nullptr, this);
}

void CoworkPanel::toggleKill()
{
    if (m_killed) {
        m_core->call(QStringLiteral("cowork.killSwitch"), {{QStringLiteral("on"), false}}, nullptr, this);
        return;
    }
    const auto ans = KMessageBox::warningContinueCancel(
        this,
        i18n("Immediately revoke ALL desktop access for every agent and tear down any live "
             "capture? Agents will have to ask again."),
        i18n("Stop all desktop access"),
        KGuiItem(i18n("Stop everything"), QStringLiteral("process-stop")));
    if (ans == KMessageBox::Continue) {
        m_core->call(QStringLiteral("cowork.killSwitch"), {{QStringLiteral("on"), true}}, nullptr, this);
    }
}

void CoworkPanel::enableForActiveThread()
{
    if (m_activeThread.isEmpty()) {
        return;
    }
    QJsonObject p{{QStringLiteral("threadId"), m_activeThread}, {QStringLiteral("enabled"), true}};
    m_core->call(QStringLiteral("cowork.setEnabled"), p, [this](const QJsonObject &, const QJsonObject &err) {
        if (err.isEmpty()) {
            m_status->setMessageType(KMessageWidget::Positive);
            m_status->setText(i18n("Cowork enabled for this agent. Restart or resume it to load the desktop tools."));
        }
    }, this);
}

void CoworkPanel::rebuildBrowserMenu()
{
    m_browserMenu->clear();
    const QList<BrowserLaunch::Browser> browsers = BrowserLaunch::all();
    for (const BrowserLaunch::Browser &b : browsers) {
        const QString engine = b.family == QLatin1String("chromium") ? i18n("Chromium") : i18n("Firefox");
        QAction *act = m_browserMenu->addAction(i18n("%1  (%2)", b.name, engine));
        const QString name = b.name, cmd = b.command, fam = b.family;
        connect(act, &QAction::triggered, this, [this, name, cmd, fam] {
            launchBrowserAndReport(name, cmd, fam);
        });
    }
    if (!browsers.isEmpty()) {
        m_browserMenu->addSeparator();
    }
    QAction *other = m_browserMenu->addAction(i18n("Other browser…"));
    connect(other, &QAction::triggered, this, &CoworkPanel::pickCustomBrowser);
}

void CoworkPanel::pickCustomBrowser()
{
    const QString path = QFileDialog::getOpenFileName(
        this, i18n("Choose a browser executable"), QStringLiteral("/usr/bin"));
    if (path.isEmpty()) {
        return;
    }
    const QStringList engines{i18n("Firefox-based (Zen, Firefox, LibreWolf…)"),
                              i18n("Chromium-based (Helium, Chrome, Brave…)")};
    bool ok = false;
    const QString choice = QInputDialog::getItem(
        this, i18n("Browser engine"),
        i18n("Which engine is this browser built on? It decides how accessibility is enabled."),
        engines, 0, false, &ok);
    if (!ok) {
        return;
    }
    const QString family =
        engines.indexOf(choice) == 1 ? QStringLiteral("chromium") : QStringLiteral("firefox");
    const QString name = QFileInfo(path).fileName();
    BrowserLaunch::addCustom({name, path, family});
    launchBrowserAndReport(name, path, family);
}

void CoworkPanel::launchBrowserAndReport(const QString &name, const QString &command,
                                         const QString &family)
{
    QString err;
    if (BrowserLaunch::launch({name, command, family}, &err)) {
        m_status->setMessageType(KMessageWidget::Positive);
        m_status->setText(i18n("Launched %1 with accessibility enabled. If it was already running, "
                               "fully quit it and launch again so the setting takes effect.", name));
    } else {
        m_status->setMessageType(KMessageWidget::Error);
        m_status->setText(i18n("Could not launch %1: %2", name, err));
    }
}
