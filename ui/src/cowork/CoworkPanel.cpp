#include "CoworkPanel.h"

#include "BrowserLaunch.h"
#include "CapabilityTile.h"
#include "ConsentDialog.h"
#include "ControlConsentDialog.h"
#include "ipc/CoreClient.h"
#include "shell/ElidingLabel.h"
#include "shell/FlowLayout.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KMessageBox>
#include <KMessageWidget>
#include <KSharedConfig>

#include <QAction>
#include <QComboBox>
#include <QDateTime>
#include <QDialog>
#include <QDialogButtonBox>
#include <QFileDialog>
#include <QFileInfo>
#include <QFrame>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QIcon>
#include <QInputDialog>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QMenu>
#include <QPlainTextEdit>
#include <QPointer>
#include <QPushButton>
#include <QScrollArea>
#include <QSignalBlocker>
#include <QSizePolicy>
#include <QSlider>
#include <QSpinBox>
#include <QToolButton>
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
    if (kind == QLatin1String("screen")) {
        return i18n("the whole screen");
    }
    if (kind == QLatin1String("vdesktop") || kind == QLatin1String("sandbox")) {
        return i18n("the sandbox desktop");
    }
    if (kind == QLatin1String("any")) {
        return i18n("the whole desktop");
    }
    return kind.isEmpty() ? i18n("your desktop") : kind;
}

// Human, one-word-verb phrasing for a capability, used inside the grant sentence
// ("Agent X can <verb> on …"). Kept lower-case so it flows in the sentence.
QString capVerb(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("see open windows");
    if (key == QLatin1String("screenshot")) return i18n("take screenshots");
    if (key == QLatin1String("a11y_read")) return i18n("read app contents");
    if (key == QLatin1String("screencast")) return i18n("watch the screen");
    if (key == QLatin1String("launch_browser")) return i18n("open a browser");
    if (key == QLatin1String("vd_sandbox")) return i18n("use a sandbox desktop");
    if (key == QLatin1String("a11y_action")) return i18n("click buttons and controls as you");
    if (key == QLatin1String("input_inject")) return i18n("type and press keys as you");
    if (key == QLatin1String("pointer_control")) return i18n("move and click the mouse as you");
    return key;
}

// Tile title (Title-ish case) for the control-centre grid.
QString capTitle(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("See open windows");
    if (key == QLatin1String("screenshot")) return i18n("Take screenshots");
    if (key == QLatin1String("a11y_read")) return i18n("Read app contents");
    if (key == QLatin1String("screencast")) return i18n("Watch the screen");
    if (key == QLatin1String("launch_browser")) return i18n("Open a browser");
    if (key == QLatin1String("vd_sandbox")) return i18n("Sandbox desktop");
    if (key == QLatin1String("a11y_action")) return i18n("Click controls");
    if (key == QLatin1String("input_inject")) return i18n("Type as you");
    if (key == QLatin1String("pointer_control")) return i18n("Move the mouse");
    return key;
}

// One-line, plain-language description shown under the tile title.
QString capDesc(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("List the windows you have open");
    if (key == QLatin1String("screenshot")) return i18n("Capture what's on your screen");
    if (key == QLatin1String("a11y_read")) return i18n("Read the text and controls in apps");
    if (key == QLatin1String("screencast")) return i18n("Watch your screen live as it changes");
    if (key == QLatin1String("launch_browser")) return i18n("Open a browser it can read and use");
    if (key == QLatin1String("vd_sandbox")) return i18n("Work on a separate virtual desktop");
    if (key == QLatin1String("a11y_action")) return i18n("Click buttons and controls as you");
    if (key == QLatin1String("input_inject")) return i18n("Type text and press keys as you");
    if (key == QLatin1String("pointer_control")) return i18n("Move the pointer and click as you");
    return QString();
}

// A recognisable theme icon per capability. Falls back gracefully if a name is
// missing from the active icon set.
QString capIcon(const QString &key)
{
    if (key == QLatin1String("window_list")) return QStringLiteral("window");
    if (key == QLatin1String("screenshot")) return QStringLiteral("camera-photo");
    if (key == QLatin1String("a11y_read")) return QStringLiteral("format-text-underline");
    if (key == QLatin1String("screencast")) return QStringLiteral("camera-web");
    if (key == QLatin1String("launch_browser")) return QStringLiteral("internet-web-browser");
    if (key == QLatin1String("vd_sandbox")) return QStringLiteral("virtual-desktops");
    if (key == QLatin1String("a11y_action")) return QStringLiteral("input-tablet");
    if (key == QLatin1String("input_inject")) return QStringLiteral("input-keyboard");
    if (key == QLatin1String("pointer_control")) return QStringLiteral("input-mouse");
    return QStringLiteral("preferences-desktop");
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

    // Enable-for-active-agent row. A FlowLayout so the long "Enable Cowork…"
    // button wraps below the label when the panel is dragged narrow instead of
    // clipping. The label keeps rich text (it shows the agent in bold), so it
    // stays a QLabel with an Ignored width policy rather than an eliding label.
    auto *enableRow = new FlowLayout(0, -1, -1);
    m_activeLabel = new QLabel(i18n("No active agent."), this);
    m_activeLabel->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Preferred);
    m_enableBtn = new QPushButton(i18n("Enable Cowork for this agent"), this);
    m_enableBtn->setIcon(QIcon::fromTheme(QStringLiteral("dialog-ok-apply")));
    m_enableBtn->setEnabled(false);
    connect(m_enableBtn, &QPushButton::clicked, this, &CoworkPanel::enableForActiveThread);
    enableRow->addWidget(m_activeLabel);
    enableRow->addWidget(m_enableBtn);
    layout->addLayout(enableRow);

    // Capability tiles: flip a tile ON to pre-authorize it for any cowork-enabled
    // agent — no per-action prompt while on (the kill-switch + activity log remain
    // the safety net). Populated from cowork.getPolicy as large control-centre
    // toggles instead of a checkbox column.
    auto *capsBox = new QGroupBox(i18n("What agents may do"), this);
    m_capsLayout = new QVBoxLayout(capsBox);
    auto *capsHint = new QLabel(i18n("Turn on what agents can do without asking each time."),
                                capsBox);
    capsHint->setWordWrap(true);
    capsHint->setStyleSheet(QStringLiteral("color: palette(mid);"));
    m_capsLayout->addWidget(capsHint);
    m_tilesFlow = new FlowLayout(0, 6, 6);
    m_capsLayout->addLayout(m_tilesFlow);
    m_capsEmpty = new QLabel(i18n("Loading…"), capsBox);
    m_capsEmpty->setStyleSheet(QStringLiteral("color: palette(mid);"));
    m_capsLayout->addWidget(m_capsEmpty);
    layout->addWidget(capsBox);

    // Active grants, rendered as plain-language sentences with a per-row Revoke.
    auto *grantsBox = new QGroupBox(i18n("Active access"), this);
    auto *grantsOuter = new QVBoxLayout(grantsBox);
    auto *grantsScroll = new QScrollArea(grantsBox);
    grantsScroll->setWidgetResizable(true);
    grantsScroll->setFrameShape(QFrame::NoFrame);
    auto *grantsHost = new QWidget(grantsScroll);
    m_grantsLayout = new QVBoxLayout(grantsHost);
    m_grantsLayout->setContentsMargins(0, 0, 0, 0);
    m_grantsLayout->setSpacing(4);
    m_grantsEmpty = new QLabel(i18n("No agent has any desktop access right now."), grantsHost);
    m_grantsEmpty->setWordWrap(true);
    m_grantsEmpty->setStyleSheet(QStringLiteral("color: palette(mid);"));
    m_grantsLayout->addWidget(m_grantsEmpty);
    m_grantsLayout->addStretch(1);
    grantsScroll->setWidget(grantsHost);
    grantsOuter->addWidget(grantsScroll);
    layout->addWidget(grantsBox, 1);

    // Kill-switch — stays prominent, full-width, below the tiles/grants.
    m_killBtn = new QPushButton(i18n("Stop ALL desktop access"), this);
    m_killBtn->setIcon(QIcon::fromTheme(QStringLiteral("process-stop")));
    connect(m_killBtn, &QPushButton::clicked, this, &CoworkPanel::toggleKill);
    layout->addWidget(m_killBtn);

    // Advanced surfaces behind buttons — the panel body stays plain-language.
    auto *advancedRow = new FlowLayout(0, 6, 6);
    auto *logBtn = new QPushButton(i18n("Activity log…"), this);
    logBtn->setIcon(QIcon::fromTheme(QStringLiteral("view-list-text")));
    connect(logBtn, &QPushButton::clicked, this, &CoworkPanel::showActivityLog);
    auto *pointerBtn = new QPushButton(i18n("Pointer settings…"), this);
    pointerBtn->setIcon(QIcon::fromTheme(QStringLiteral("input-mouse")));
    connect(pointerBtn, &QPushButton::clicked, this, &CoworkPanel::showPointerSettings);
    auto *browserBtn = new QPushButton(i18n("Browser tools…"), this);
    browserBtn->setIcon(QIcon::fromTheme(QStringLiteral("internet-web-browser")));
    connect(browserBtn, &QPushButton::clicked, this, &CoworkPanel::showBrowserTools);
    advancedRow->addWidget(logBtn);
    advancedRow->addWidget(pointerBtn);
    advancedRow->addWidget(browserBtn);
    layout->addLayout(advancedRow);

    connect(m_core, &CoreClient::notification, this, &CoworkPanel::onNotification);
    connect(m_core, &CoreClient::connected, this, &CoworkPanel::refresh);
    // Push the persisted pointer bounds to the core on every (re)connect. Without
    // this the core starts each session with unbounded defaults until the user
    // happens to nudge a slider, silently ignoring stricter saved limits.
    connect(m_core, &CoreClient::connected, this, &CoworkPanel::savePointerBounds);
    if (m_core->isConnected()) {
        refresh();
        savePointerBounds();
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
    QPointer<CoworkPanel> self(this);
    m_core->call(QStringLiteral("cowork.getPolicy"), {}, [this, self](const QJsonObject &res, const QJsonObject &err) {
        if (!self || !err.isEmpty()) {
            return;
        }
        const QJsonArray caps = res.value(QStringLiteral("capabilities")).toArray();
        if (m_capsEmpty && !caps.isEmpty()) {
            m_capsEmpty->hide();
        }
        for (const QJsonValue &cv : caps) {
            const QJsonObject c = cv.toObject();
            const QString key = c.value(QStringLiteral("key")).toString();
            const bool enabled = c.value(QStringLiteral("enabled")).toBool();
            const bool dangerous = c.value(QStringLiteral("tier")).toString() == QLatin1String("R2");
            CapabilityTile *tile = m_tiles.value(key, nullptr);
            if (!tile) {
                tile = new CapabilityTile(key, capTitle(key), capDesc(key), capIcon(key), dangerous, this);
                if (dangerous) {
                    tile->setToolTip(i18n("High-risk: lets the agent act as you (type, click). "
                                          "The kill-switch and activity log are your safety net."));
                } else {
                    tile->setToolTip(capDesc(key));
                }
                connect(tile, &CapabilityTile::toggled, this, [this, self](const QString &k, bool on) {
                    if (!self) {
                        return;
                    }
                    m_core->call(QStringLiteral("cowork.setPolicy"),
                                 {{QStringLiteral("capability"), k}, {QStringLiteral("enabled"), on}},
                                 nullptr, this);
                });
                m_tilesFlow->addWidget(tile);
                m_tiles.insert(key, tile);
            }
            tile->setChecked(enabled); // silent — no toggled() echo
        }
    }, this);
}

void CoworkPanel::savePointerBounds()
{
    // The pointer controls only exist while the settings dialog is open; when it is
    // closed we push the persisted config values instead. This keeps the core in
    // sync on connect and after every dialog edit.
    KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Cowork"));
    int spd, acc, settle;
    if (m_pointerSpeed && m_pointerAccuracy && m_pointerSettle) {
        spd = m_pointerSpeed->currentData().toInt();
        acc = m_pointerAccuracy->value();
        settle = m_pointerSettle->value();
        cfg.writeEntry("PointerSpeed", spd);
        cfg.writeEntry("PointerAccuracy", acc);
        cfg.writeEntry("PointerSettleMs", settle);
        cfg.sync();
    } else {
        spd = cfg.readEntry("PointerSpeed", 1600);
        acc = cfg.readEntry("PointerAccuracy", 100);
        settle = cfg.readEntry("PointerSettleMs", 30);
    }

    // Inform the core so it clamps each agent's per-call pointer values to these bounds.
    // The core's accuracy is a float in 0..1 (1 = straight line, lands exact); the slider
    // is an integer percent, so scale it — otherwise clampProfile pins every value >1 to
    // 1.0 and the slider is dead (only 0% would differ).
    m_core->call(QStringLiteral("cowork.setPointerBounds"),
                 {{QStringLiteral("speed"), spd},
                  {QStringLiteral("accuracy"), acc / 100.0},
                  {QStringLiteral("settleMs"), settle}},
                 nullptr, this);
}

void CoworkPanel::refreshStatus()
{
    QPointer<CoworkPanel> self(this);
    m_core->call(QStringLiteral("cowork.status"), {}, [this, self](const QJsonObject &res, const QJsonObject &err) {
        if (!self || !err.isEmpty()) {
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
    QPointer<CoworkPanel> self(this);
    m_core->call(QStringLiteral("cowork.listGrants"), {}, [this, self](const QJsonObject &res, const QJsonObject &err) {
        if (!self || !err.isEmpty()) {
            return;
        }
        // Clear existing grant rows (everything before the trailing stretch), but
        // keep the empty-state label and the stretch.
        while (m_grantsLayout->count() > 0) {
            QLayoutItem *item = m_grantsLayout->itemAt(0);
            if (!item) {
                break;
            }
            if (item->widget() == m_grantsEmpty || item->spacerItem()) {
                break; // the empty label + stretch sit last; stop when we reach them
            }
            m_grantsLayout->takeAt(0);
            delete item->widget();
            delete item;
        }

        int shown = 0;
        const QJsonArray grants = res.value(QStringLiteral("grants")).toArray();
        for (const QJsonValue &gv : grants) {
            const QJsonObject g = gv.toObject();
            // Skip already-revoked grants (the store keeps history).
            if (!g.value(QStringLiteral("revokedAt")).toString().isEmpty()) {
                continue;
            }
            const QString id = g.value(QStringLiteral("id")).toString();
            const QString threadId = g.value(QStringLiteral("threadId")).toString();
            const QString cap = g.value(QStringLiteral("capability")).toString();
            const QString target = targetSummary(g.value(QStringLiteral("target")).toObject());
            const QString until = expiryText(g);
            const bool r2 = g.value(QStringLiteral("tier")).toString() == QLatin1String("R2");

            auto *row = new QWidget;
            auto *rowLay = new QHBoxLayout(row);
            rowLay->setContentsMargins(0, 0, 0, 0);
            rowLay->setSpacing(6);

            if (r2) {
                auto *warn = new QLabel(row);
                warn->setPixmap(QIcon::fromTheme(QStringLiteral("dialog-warning")).pixmap(16, 16));
                warn->setToolTip(i18n("High-risk: this agent can act as you."));
                rowLay->addWidget(warn, 0, Qt::AlignTop);
            }

            // The sentence, bold-highlighting the agent, action, target and expiry.
            auto *sentence = new QLabel(row);
            sentence->setWordWrap(true);
            sentence->setTextFormat(Qt::RichText);
            sentence->setSizePolicy(QSizePolicy::Ignored, QSizePolicy::Preferred);
            sentence->setText(i18n("<b>%1</b> can <i>%2</i> on <i>%3</i> until <b>%4</b>.",
                                   threadId.toHtmlEscaped(), capVerb(cap).toHtmlEscaped(),
                                   target.toHtmlEscaped(), until.toHtmlEscaped()));
            rowLay->addWidget(sentence, 1);

            auto *revoke = new QToolButton(row);
            revoke->setText(i18n("Revoke"));
            revoke->setIcon(QIcon::fromTheme(QStringLiteral("edit-delete")));
            revoke->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
            revoke->setToolTip(i18n("Immediately remove this access"));
            connect(revoke, &QToolButton::clicked, this, [this, id] { revokeGrant(id); });
            rowLay->addWidget(revoke, 0, Qt::AlignTop);

            // Insert above the empty-label + stretch (they always sit last).
            m_grantsLayout->insertWidget(shown, row);
            ++shown;
        }
        m_grantsEmpty->setVisible(shown == 0);
    }, this);
}

void CoworkPanel::renderAudit()
{
    if (!m_audit) {
        return;
    }
    const QString filter = m_auditFilter ? m_auditFilter->currentData().toString() : QString();
    QStringList lines;
    for (const QJsonValue &ev : std::as_const(m_auditEntries)) {
        const QJsonObject e = ev.toObject();
        if (!filter.isEmpty()
            && !filter.split(QLatin1Char(',')).contains(e.value(QStringLiteral("kind")).toString())) {
            continue;
        }
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
}

void CoworkPanel::refreshAudit()
{
    QPointer<CoworkPanel> self(this);
    QJsonObject p{{QStringLiteral("limit"), 100}};
    m_core->call(QStringLiteral("cowork.listAudit"), p, [this, self](const QJsonObject &res, const QJsonObject &err) {
        if (!self || !err.isEmpty()) {
            return;
        }
        m_auditEntries = res.value(QStringLiteral("entries")).toArray();
        renderAudit(); // no-op when the log dialog isn't open (m_audit == nullptr)
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

void CoworkPanel::revokeGrant(const QString &id)
{
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
    QPointer<CoworkPanel> self(this);
    QJsonObject p{{QStringLiteral("threadId"), m_activeThread}, {QStringLiteral("enabled"), true}};
    m_core->call(QStringLiteral("cowork.setEnabled"), p, [this, self](const QJsonObject &, const QJsonObject &err) {
        if (!self || !err.isEmpty()) {
            return;
        }
        m_status->setMessageType(KMessageWidget::Positive);
        m_status->setText(i18n("Cowork enabled for this agent. Restart or resume it to load the desktop tools."));
    }, this);
}

// ---------------------------------------------------------------------------
// Advanced dialogs
// ---------------------------------------------------------------------------

void CoworkPanel::showActivityLog()
{
    QDialog dlg(this);
    dlg.setWindowTitle(i18n("Cowork — activity log"));
    dlg.resize(560, 420);
    auto *v = new QVBoxLayout(&dlg);

    auto *filterRow = new QHBoxLayout;
    filterRow->addWidget(new QLabel(i18n("Show:"), &dlg));
    m_auditFilter = new QComboBox(&dlg);
    m_auditFilter->addItem(i18n("Everything"), QString());
    m_auditFilter->addItem(i18n("Granted"), QStringLiteral("grant"));
    m_auditFilter->addItem(i18n("Denied"), QStringLiteral("deny"));
    m_auditFilter->addItem(i18n("Revoked"), QStringLiteral("revoke"));
    m_auditFilter->addItem(i18n("Used"), QStringLiteral("action"));
    m_auditFilter->addItem(i18n("Kill switch"), QStringLiteral("kill,rearm"));
    connect(m_auditFilter, &QComboBox::currentIndexChanged, this, [this] { renderAudit(); });
    filterRow->addWidget(m_auditFilter, 1);
    v->addLayout(filterRow);

    m_audit = new QPlainTextEdit(&dlg);
    m_audit->setReadOnly(true);
    m_audit->setMaximumBlockCount(2000);
    v->addWidget(m_audit, 1);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, &dlg);
    connect(buttons, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    connect(buttons, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    v->addWidget(buttons);

    refreshAudit();  // repopulate m_auditEntries and render
    renderAudit();
    dlg.exec();

    // The view lives only for the dialog's lifetime.
    m_audit = nullptr;
    m_auditFilter = nullptr;
}

void CoworkPanel::showPointerSettings()
{
    // Pointer motion defaults: sane USER-set bounds the core clamps every agent
    // pointer move to. The agent may still ask for slower/less-exact motion within
    // these limits, but never faster or jerkier. Saved in KConfig (group "Cowork").
    QDialog dlg(this);
    dlg.setWindowTitle(i18n("Cowork — pointer settings"));
    auto *v = new QVBoxLayout(&dlg);

    auto *hint = new QLabel(i18n("Limits every agent's mouse movement. Agents can go slower or "
                                 "less exact, but never faster or jerkier than this."),
                            &dlg);
    hint->setWordWrap(true);
    v->addWidget(hint);

    const KConfigGroup cfg = KSharedConfig::openConfig()->group(QStringLiteral("Cowork"));
    const int savedSpeed = cfg.readEntry("PointerSpeed", 1600);
    const int savedAccuracy = cfg.readEntry("PointerAccuracy", 100);
    const int savedSettle = cfg.readEntry("PointerSettleMs", 30);

    auto *speedRow = new QHBoxLayout;
    speedRow->addWidget(new QLabel(i18n("Speed:"), &dlg));
    m_pointerSpeed = new QComboBox(&dlg);
    m_pointerSpeed->setToolTip(i18n("How fast the agent's pointer travels. 'Instant' "
                                    "teleports straight to the target with no visible motion."));
    m_pointerSpeed->addItem(i18n("Instant"), 0);     // teleport, no animation
    m_pointerSpeed->addItem(i18n("Fast"), 3000);
    m_pointerSpeed->addItem(i18n("Normal"), 1600);   // default
    m_pointerSpeed->addItem(i18n("Slow"), 800);
    {
        const int idx = m_pointerSpeed->findData(savedSpeed);
        m_pointerSpeed->setCurrentIndex(idx >= 0 ? idx : m_pointerSpeed->findData(1600));
    }
    speedRow->addWidget(m_pointerSpeed, 1);
    v->addLayout(speedRow);

    m_pointerAccuracyLabel = new QLabel(i18n("Accuracy: %1%", savedAccuracy), &dlg);
    m_pointerAccuracy = new QSlider(Qt::Horizontal, &dlg);
    m_pointerAccuracy->setRange(0, 100);
    m_pointerAccuracy->setValue(savedAccuracy);
    m_pointerAccuracy->setToolTip(i18n("100% = straight, exact, robotic motion. Lower adds a "
                                       "more human-like path (easing, overshoot, jitter) — but "
                                       "the click always lands exactly on the target."));
    v->addWidget(m_pointerAccuracyLabel);
    v->addWidget(m_pointerAccuracy);

    auto *settleRow = new QHBoxLayout;
    auto *settleLabel = new QLabel(i18n("Settle before click (ms)"), &dlg);
    settleLabel->setWordWrap(true);
    m_pointerSettle = new QSpinBox(&dlg);
    m_pointerSettle->setRange(0, 500);
    m_pointerSettle->setValue(savedSettle);
    m_pointerSettle->setToolTip(i18n("Pause after the pointer arrives, before clicking, so the "
                                     "target has time to react to hover."));
    settleRow->addWidget(settleLabel, 1);
    settleRow->addWidget(m_pointerSettle);
    v->addLayout(settleRow);

    connect(m_pointerSpeed, &QComboBox::currentIndexChanged, this, &CoworkPanel::savePointerBounds);
    connect(m_pointerAccuracy, &QSlider::valueChanged, this, [this](int val) {
        m_pointerAccuracyLabel->setText(i18n("Accuracy: %1%", val));
        savePointerBounds();
    });
    connect(m_pointerSettle, &QSpinBox::valueChanged, this, &CoworkPanel::savePointerBounds);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, &dlg);
    connect(buttons, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    connect(buttons, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    v->addWidget(buttons);

    dlg.exec();

    m_pointerSpeed = nullptr;
    m_pointerAccuracy = nullptr;
    m_pointerAccuracyLabel = nullptr;
    m_pointerSettle = nullptr;
}

void CoworkPanel::showBrowserTools()
{
    QDialog dlg(this);
    dlg.setWindowTitle(i18n("Cowork — browser tools"));
    auto *v = new QVBoxLayout(&dlg);

    // Browser launcher. Browsers hide their web content from the accessibility tree
    // unless started with the right flag/env, so agents can't read or click page
    // elements. This opens one with accessibility forced on.
    auto *info = new QLabel(i18n("Browsers hide their page content from agents unless started "
                                 "with accessibility enabled. Launch one from here so agents can "
                                 "read and click page elements — fully quit the browser first if "
                                 "it is already running."),
                            &dlg);
    info->setWordWrap(true);
    v->addWidget(info);

    auto *browserRow = new QHBoxLayout;
    browserRow->addWidget(new QLabel(i18n("Open a browser agents can read:"), &dlg), 1);
    m_browserBtn = new QToolButton(&dlg);
    m_browserBtn->setText(i18n("Launch browser"));
    m_browserBtn->setIcon(QIcon::fromTheme(QStringLiteral("internet-web-browser")));
    m_browserBtn->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_browserBtn->setPopupMode(QToolButton::InstantPopup);
    m_browserMenu = new QMenu(m_browserBtn);
    m_browserBtn->setMenu(m_browserMenu);
    connect(m_browserMenu, &QMenu::aboutToShow, this, &CoworkPanel::rebuildBrowserMenu);
    browserRow->addWidget(m_browserBtn);
    v->addLayout(browserRow);

    // Which browser an agent opens when it calls desktop_open_browser itself.
    auto *prefRow = new QHBoxLayout;
    prefRow->addWidget(new QLabel(i18n("Agent's default browser:"), &dlg));
    m_agentBrowserCombo = new QComboBox(&dlg);
    m_agentBrowserCombo->setToolTip(i18n(
        "When an agent opens a browser on its own, it uses this one. The agent can "
        "only open browsers listed here — never an arbitrary program."));
    connect(m_agentBrowserCombo, &QComboBox::currentIndexChanged, this, [this] {
        const QString cmd = m_agentBrowserCombo->currentData().toString();
        if (!cmd.isEmpty()) {
            BrowserLaunch::setPreferred(cmd);
        }
    });
    prefRow->addWidget(m_agentBrowserCombo, 1);
    v->addLayout(prefRow);
    refreshBrowserPrefCombo();

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, &dlg);
    connect(buttons, &QDialogButtonBox::rejected, &dlg, &QDialog::reject);
    connect(buttons, &QDialogButtonBox::accepted, &dlg, &QDialog::accept);
    v->addWidget(buttons);

    dlg.exec();

    m_browserBtn = nullptr;
    m_browserMenu = nullptr;
    m_agentBrowserCombo = nullptr;
}

void CoworkPanel::rebuildBrowserMenu()
{
    if (!m_browserMenu) {
        return;
    }
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
    const int guessIdx = BrowserLaunch::guessFamily(path) == QLatin1String("chromium") ? 1 : 0;
    bool ok = false;
    const QString choice = QInputDialog::getItem(
        this, i18n("Browser engine"),
        i18n("Which engine is this browser built on? It decides how accessibility is enabled."),
        engines, guessIdx, false, &ok);
    if (!ok) {
        return;
    }
    const QString family =
        engines.indexOf(choice) == 1 ? QStringLiteral("chromium") : QStringLiteral("firefox");
    const QString name = QFileInfo(path).fileName();
    BrowserLaunch::addCustom({name, path, family});
    refreshBrowserPrefCombo(); // the new browser becomes selectable as the agent default
    launchBrowserAndReport(name, path, family);
}

void CoworkPanel::refreshBrowserPrefCombo()
{
    if (!m_agentBrowserCombo) {
        return;
    }
    const QSignalBlocker block(m_agentBrowserCombo); // repopulating must not persist a choice
    m_agentBrowserCombo->clear();
    const QList<BrowserLaunch::Browser> browsers = BrowserLaunch::all();
    if (browsers.isEmpty()) {
        m_agentBrowserCombo->addItem(i18n("(none configured)"), QString());
        m_agentBrowserCombo->setEnabled(false);
        return;
    }
    m_agentBrowserCombo->setEnabled(true);
    for (const BrowserLaunch::Browser &b : browsers) {
        const QString engine = b.family == QLatin1String("chromium") ? i18n("Chromium") : i18n("Firefox");
        m_agentBrowserCombo->addItem(i18n("%1  (%2)", b.name, engine), b.command);
    }
    const int idx = m_agentBrowserCombo->findData(BrowserLaunch::preferred().command);
    if (idx >= 0) {
        m_agentBrowserCombo->setCurrentIndex(idx);
    }
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
