#include "CoworkPanel.h"

#include "BrowserLaunch.h"
#include "CapabilityText.h"
#include "CapabilityTile.h"
#include "ConsentDialog.h"
#include "ControlConsentDialog.h"
#include "CoworkPortal.h"
#include "ipc/CoreClient.h"
#include "shell/ElidingLabel.h"
#include "shell/FlowLayout.h"

#include <KConfigGroup>
#include <KLocalizedString>
#include <KMessageBox>
#include <KMessageWidget>
#include <KSharedConfig>
#include <KStandardGuiItem>

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
#include <QPalette>
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

// Tint a hint label with the muted Mid role via its palette instead of a
// per-widget "color: palette(mid)" stylesheet (which fights the app's
// palette-only theming convention). Mirrors CapabilityTile::restyle. Set once at
// construction; these labels aren't in a palette handler, so a live scheme switch
// won't recolor them — acceptable for static hint copy.
void tintHint(QLabel *label)
{
    QPalette p = label->palette();
    p.setColor(QPalette::WindowText, p.color(QPalette::Mid));
    label->setPalette(p);
}

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
        // Honesty (audit F32): organizational boundary, not containment — see
        // CapabilityText.h. Never "sandbox" in the UI.
        return i18n("a separate desktop");
    }
    if (kind == QLatin1String("any")) {
        return i18n("the whole desktop");
    }
    return kind.isEmpty() ? i18n("your desktop") : kind;
}

// The capability vocabulary (verb / title / description) now lives in
// CapabilityText.h so this panel and the consent prompt cannot drift apart — the
// prompt's copy used to be missing half the keys and rendered them raw (audit F50).

// The activity log's capability column. It printed the raw internal key straight from
// the core's audit entry, which is the same leak in the one surface a non-technical
// user actually opens to check what agents did (audit F35). Entries with no capability
// (kill / rearm / rotate) keep the column empty rather than being described as an
// unrecognised permission.
QString auditCapabilityText(const QString &key)
{
    return key.isEmpty() ? QString() : CoworkCaps::title(key);
}

// A recognisable theme icon per capability. Falls back gracefully if a name is
// missing from the active icon set.
QString capIcon(const QString &key)
{
    if (key == QLatin1String("window_list")) return QStringLiteral("window");
    // camera-photo / camera-web / input-tablet carry malformed <path> data in the
    // shipped Breeze SVGs and emit QtSvg "Invalid path data; path truncated"
    // warnings when the icon engine rasterizes them at the 28px tile size. Swap to
    // clean equivalents that render without warnings (verified in-app): screenshot
    // → camera, screencast → camera-video, a11y_action → hand.
    if (key == QLatin1String("screenshot")) return QStringLiteral("camera");
    if (key == QLatin1String("a11y_read")) return QStringLiteral("format-text-underline");
    if (key == QLatin1String("screencast")) return QStringLiteral("camera-video");
    if (key == QLatin1String("launch_browser")) return QStringLiteral("internet-web-browser");
    if (key == QLatin1String("vd_sandbox")) return QStringLiteral("virtual-desktops");
    if (key == QLatin1String("a11y_action")) return QStringLiteral("hand");
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
    // The OS-level grant (accessibility + the remote-control/screen-share portal) is
    // taken at enable time, but KDE refuses to let a remote-desktop grant persist, so
    // it lapses whenever Agent Kate restarts or the kill-switch fires. This button
    // takes it back without waiting for an agent to trip over the missing permission.
    m_preflightBtn = new QPushButton(i18n("Grant desktop access now"), this);
    m_preflightBtn->setIcon(QIcon::fromTheme(QStringLiteral("preferences-desktop-accessibility")));
    m_preflightBtn->setToolTip(i18n("Ask the desktop for screen and input permission now, so "
                                    "agents never stall on the system dialog mid-task. Granting "
                                    "it also switches your session's accessibility service on, "
                                    "desktop-wide, until desktop access is turned off."));
    m_preflightBtn->setEnabled(false);
    connect(m_preflightBtn, &QPushButton::clicked, this, &CoworkPanel::requestPreflight);
    enableRow->addWidget(m_activeLabel);
    enableRow->addWidget(m_enableBtn);
    enableRow->addWidget(m_preflightBtn);
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
    tintHint(capsHint);
    m_capsLayout->addWidget(capsHint);
    m_tilesFlow = new FlowLayout(0, 6, 6);
    m_capsLayout->addLayout(m_tilesFlow);
    m_capsEmpty = new QLabel(i18n("Loading…"), capsBox);
    tintHint(m_capsEmpty);
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
    tintHint(m_grantsEmpty);
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
    } else if (method == QLatin1String("cowork.enableRequested")) {
        handleEnableRequested(params);
    } else if (method == QLatin1String("cowork.enabledChanged")) {
        // Someone (this panel, the agent's settings, or an approved agent request)
        // switched Cowork for a thread. Say what actually happened to the running
        // agent — the old text always told the user to restart it, which is no
        // longer true on an engine that can reveal tools live.
        const bool on = params.value(QStringLiteral("enabled")).toBool();
        const QString applied = params.value(QStringLiteral("applied")).toString();
        m_status->setMessageType(KMessageWidget::Positive);
        if (!on) {
            m_status->setText(i18n("Desktop tools switched off for that agent."));
        } else if (applied == QLatin1String("reattach")) {
            m_status->setText(i18n("Desktop tools enabled. That agent's session is being "
                                   "re-attached to load them — its conversation is kept."));
        } else if (applied == QLatin1String("nextStart")) {
            m_status->setText(i18n("Desktop tools enabled. They will be there when that "
                                   "agent next starts."));
        } else {
            m_status->setText(i18n("Desktop tools enabled — the agent can use them right "
                                   "away, no restart."));
        }
    } else if (method == QLatin1String("cowork.preflightResult")) {
        if (params.value(QStringLiteral("ok")).toBool()) {
            m_status->setMessageType(KMessageWidget::Positive);
            m_status->setText(i18n("Desktop access granted. Agents can see and control the "
                                   "screen without another system prompt this session."));
        } else {
            // No "it will prompt again" reassurance: the common failure here is not a
            // decline but the portal missing its remote-control backend, which will
            // fail identically until it is fixed. The detail carries the fix when
            // there is one (CoworkPortal::portalFailureDetail).
            m_status->setMessageType(KMessageWidget::Warning);
            m_status->setText(i18n("Desktop permission is not available: %1",
                                   params.value(QStringLiteral("error")).toString()));
        }
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
                tile = new CapabilityTile(key, CoworkCaps::title(key),
                                          CoworkCaps::description(key), capIcon(key),
                                          dangerous, this);
                if (dangerous) {
                    tile->setToolTip(i18n("High-risk: lets the agent act as you (type, click). "
                                          "The kill-switch and activity log are your safety net."));
                } else {
                    tile->setToolTip(CoworkCaps::description(key));
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
        // The OS grant is worth asking for whenever the desktop is reachable and not
        // killed — it is not tied to any one agent.
        m_preflightBtn->setEnabled(m_available && !m_killed && !tampered);
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
                                   threadId.toHtmlEscaped(), CoworkCaps::verb(cap).toHtmlEscaped(),
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
                          auditCapabilityText(e.value(QStringLiteral("capability")).toString()),
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
        // SECURITY (audit F50): turning the panic button back OFF is the one direction
        // that widens authority, and it used to be a single unconfirmed click on the
        // same button the user just hit in a hurry. Confirm it, defaulting to Cancel
        // (Dangerous), and say plainly what does and does not come back — the kill
        // cleared every toggle and grant core-side, so nothing is restored by this.
        const auto ans = KMessageBox::warningContinueCancel(
            this,
            i18n("Allow agents to ask for desktop access again?\n\n"
                 "The kill-switch cleared every standing permission and every live grant, "
                 "and this does not bring any of them back — agents start from nothing and "
                 "must ask you."),
            i18n("Re-enable desktop access"),
            KGuiItem(i18n("Allow asking again"), QStringLiteral("dialog-ok-apply")),
            KStandardGuiItem::cancel(), QString(),
            KMessageBox::Options(KMessageBox::Notify | KMessageBox::Dangerous));
        if (ans != KMessageBox::Continue) {
            return;
        }
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

bool CoworkPanel::confirmDesktopAccessibilityFlip(const QString &what)
{
    // Already flipped by us: the human consented to it earlier in this run and nothing
    // new happens here, so do not nag. Also the honest answer when there is no portal to
    // flip anything through.
    if (!m_portal || m_portal->desktopAccessibilityFlipped()) {
        return true;
    }
    // SECURITY (audit F31): warningTwoActions, not questionTwoActions — its default
    // button is the SECONDARY one (Cancel), per the KF6 header contract. This dialog is
    // long and is raised by a click the user already made, which is exactly the shape
    // where Enter gets pressed through; the risky side must never be the default.
    const auto answer = KMessageBox::warningTwoActions(
        this,
        i18n("<p>To do this, Agent Kate switches your session's accessibility service on "
             "(<tt>org.a11y.Status</tt>) so applications expose their windows and controls.</p>"
             "<p><b>That is a desktop-wide change.</b> While it is on, every application in "
             "this session exports its contents and controls to <i>any</i> program running as "
             "you — not only to Agent Kate — and assistive technologies may start themselves.</p>"
             "<p>Your original setting is put back when the last agent's desktop access is "
             "switched off, when you hit the kill-switch, and when Agent Kate exits "
             "(including after a crash).</p>"),
        what,
        KGuiItem(i18n("Turn it on and continue"), QStringLiteral("preferences-desktop-accessibility")),
        KGuiItem(i18n("Cancel"), QStringLiteral("dialog-cancel")));
    return answer == KMessageBox::PrimaryAction;
}

void CoworkPanel::enableForActiveThread()
{
    if (m_activeThread.isEmpty()) {
        return;
    }
    // Enabling runs the OS preflight, which ends in the desktop-wide accessibility flip
    // once the portal grant lands — so this click is where that has to be disclosed
    // (audit F8). Declining changes nothing: no flip, no enable, nothing to restore.
    if (!confirmDesktopAccessibilityFlip(i18n("Give this agent desktop access?"))) {
        return;
    }
    QPointer<CoworkPanel> self(this);
    QJsonObject p{{QStringLiteral("threadId"), m_activeThread}, {QStringLiteral("enabled"), true}};
    // The reply says how it reached the running agent (live / re-attach / next start)
    // and the core also broadcasts cowork.enabledChanged; both render the same text,
    // so the panel reads correctly whether the switch came from here or elsewhere.
    // The core raises the OS permission dialog straight after — no extra call here.
    m_core->call(QStringLiteral("cowork.setEnabled"), p, [this, self](const QJsonObject &res, const QJsonObject &err) {
        if (!self) {
            return;
        }
        if (!err.isEmpty()) {
            m_status->setMessageType(KMessageWidget::Error);
            m_status->setText(i18n("Could not enable desktop tools: %1",
                                   err.value(QStringLiteral("message")).toString()));
            return;
        }
        QJsonObject changed = res;
        changed.insert(QStringLiteral("enabled"), true);
        onNotification(QStringLiteral("cowork.enabledChanged"), changed);
    }, this);
}

void CoworkPanel::requestPreflight()
{
    // Same disclosure as the enable button: a granted preflight is what flips the
    // desktop-wide accessibility flags (CoworkPortal::becomeReady). Declining here asks
    // the portal for nothing at all, so the desktop is untouched (audit F8).
    if (!confirmDesktopAccessibilityFlip(i18n("Grant desktop access now?"))) {
        return;
    }
    QPointer<CoworkPanel> self(this);
    m_status->setMessageType(KMessageWidget::Information);
    m_status->setText(i18n("Asking the desktop for screen and input permission — answer the "
                           "system dialog."));
    m_core->call(QStringLiteral("cowork.preflight"),
                 {{QStringLiteral("threadId"), m_activeThread}},
                 [this, self](const QJsonObject &res, const QJsonObject &err) {
        if (!self) {
            return;
        }
        QJsonObject out = res;
        if (!err.isEmpty()) {
            out.insert(QStringLiteral("ok"), false);
            out.insert(QStringLiteral("error"), err.value(QStringLiteral("message")).toString());
        }
        onNotification(QStringLiteral("cowork.preflightResult"), out);
    }, this);
}

void CoworkPanel::handleEnableRequested(const QJsonObject &params)
{
    // SECURITY (audit F3/F8/quality): the dialog used to promise "Every individual action
    // still asks for its own permission" unconditionally. That is FALSE for any capability
    // whose standing toggle is on — and input_inject being pre-authorized is precisely the
    // precondition of the injection self-approval attack. An approval the human cannot
    // understand is not a control, so the live policy is read HERE, at dialog time, and the
    // sentence is built from what is actually true right now.
    QPointer<CoworkPanel> self(this);
    m_core->call(QStringLiteral("cowork.getPolicy"), {},
                 [this, self, params](const QJsonObject &res, const QJsonObject &err) {
        if (!self) {
            return;
        }
        QStringList standing;
        bool anyR2 = false;
        // FAIL CLOSED on a policy read error: say we could not determine the standing
        // grants rather than implying there are none.
        const bool policyKnown = err.isEmpty();
        const QJsonArray caps = res.value(QStringLiteral("capabilities")).toArray();
        for (const QJsonValue &cv : caps) {
            const QJsonObject c = cv.toObject();
            if (!c.value(QStringLiteral("enabled")).toBool()) {
                continue;
            }
            const QString key = c.value(QStringLiteral("key")).toString();
            standing << CoworkCaps::verb(key).toHtmlEscaped();
            if (c.value(QStringLiteral("tier")).toString() == QLatin1String("R2")) {
                anyR2 = true;
            }
        }
        showEnableRequestDialog(params, standing, anyR2, policyKnown);
    }, this);
}

void CoworkPanel::showEnableRequestDialog(const QJsonObject &params, const QStringList &standing,
                                          bool anyR2, bool policyKnown)
{
    const QString requestId = params.value(QStringLiteral("requestId")).toString();
    const QString title = params.value(QStringLiteral("title")).toString();
    const QString reason = params.value(QStringLiteral("reason")).toString();
    const bool selfAsk = params.value(QStringLiteral("self")).toBool();

    const QString who = title.isEmpty() ? i18n("An agent") : title;
    const QString what = selfAsk
        ? i18n("<b>%1</b> is asking for access to your desktop.", who.toHtmlEscaped())
        : i18n("An agent is asking for desktop access on behalf of <b>%1</b>.", who.toHtmlEscaped());

    // What the per-action prompting really is, given the toggles that are on right now.
    QString gating;
    if (!policyKnown) {
        gating = i18n("<p><b>Your standing permissions could not be read just now</b>, so this "
                      "dialog cannot tell you which actions would run without asking. Check the "
                      "Cowork panel's switches before allowing.</p>");
    } else if (standing.isEmpty()) {
        gating = i18n("<p>Every individual action still asks for its own permission, and the "
                      "kill-switch stops everything at once.</p>");
    } else {
        gating = i18n("<p><b>These actions are already switched on and will NOT ask again</b> "
                      "for this or any agent: %1. Everything else still asks for its own "
                      "permission, and the kill-switch stops all of it at once.</p>",
                      standing.join(i18nc("list separator", ", ")));
        if (anyR2) {
            gating += i18n("<p>That includes acting as you — typing, clicking or moving the "
                           "pointer without a prompt. You can turn those switches off in the "
                           "Cowork panel before allowing this.</p>");
        }
    }

    // Disclose the desktop-wide side effect (audit F8): the same text the per-action
    // control prompt carries, because this is where most users say yes for the first time.
    const QString a11y = i18n("<p>While desktop access is on, Agent Kate switches your session's "
                              "accessibility service on so applications expose their windows and "
                              "controls. Every app on this desktop becomes readable that way, by "
                              "any program in your session. Your original setting is put back when "
                              "the last agent's desktop access is switched off, when you hit the "
                              "kill-switch, and when Agent Kate exits.</p>");

    // SECURITY (audit F31): warningTwoActions defaults to the SECONDARY action ("Keep
    // it off") — Enter can no longer hand an agent screen-read plus type/click-as-you.
    // This prompt is AGENT-initiated and long (reason + standing-grant disclosure +
    // a11y disclosure): the tired-user default has to be the safe one. Matches
    // ControlConsentDialog's Deny default.
    const auto answer = KMessageBox::warningTwoActions(
        this,
        i18n("<p>%1</p><p>Its reason: <i>%2</i></p><p>If you allow this, that agent can see "
             "your screen, read window contents, and move the pointer, type and click as you.</p>"
             "%3%4",
             what, reason.toHtmlEscaped(), gating, a11y),
        i18n("Give an agent desktop access?"),
        KGuiItem(i18n("Allow desktop access"), QStringLiteral("preferences-desktop-accessibility")),
        KGuiItem(i18n("Keep it off"), QStringLiteral("process-stop")));

    m_core->call(QStringLiteral("permission.respond"),
                 {{QStringLiteral("requestId"), requestId},
                  {QStringLiteral("allow"), answer == KMessageBox::PrimaryAction}},
                 nullptr, this);
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

void CoworkPanel::setPortal(CoworkPortal *portal)
{
    m_portal = portal;
}

void CoworkPanel::launchBrowserAndReport(const QString &name, const QString &command,
                                         const QString &family)
{
    // A Chromium browser reads org.a11y.Status ONCE, at launch, to decide whether to
    // export its page over AT-SPI — so the desktop-wide flag has to be on before the
    // process starts. BrowserLaunch::launch deliberately no longer does this itself
    // (audit F8/F12): the flip is a global permission change that must be parked
    // before it happens and undone on teardown, and the portal is where that lives.
    //
    // This path is the HUMAN pressing the panel's own launch button — unlike the
    // agent-facing path in CoworkPortal::handleLaunchBrowser, which may only re-assert a
    // flip the human already granted. But a button press is only consent if the human
    // knows what it does: this one used to flip the whole desktop into accessibility mode
    // silently, and it can do so with Cowork never having been enabled at all (audit F8).
    // So it is disclosed here, and declining launches nothing and flips nothing.
    bool a11yFlipped = true;
    if (family == QLatin1String("chromium")) {
        if (m_portal) {
            if (!confirmDesktopAccessibilityFlip(i18n("Launch %1 with accessibility on?", name))) {
                return;
            }
            m_portal->enableAtspiForUserLaunch();
        } else {
            // No portal to park-and-flip through. Say so rather than launching a
            // browser and claiming accessibility is on when it is not.
            a11yFlipped = false;
        }
    }

    QString err;
    if (!BrowserLaunch::launch({name, command, family}, &err)) {
        m_status->setMessageType(KMessageWidget::Error);
        m_status->setText(i18n("Could not launch %1: %2", name, err));
        return;
    }
    if (a11yFlipped) {
        m_status->setMessageType(KMessageWidget::Positive);
        m_status->setText(i18n("Launched %1 with accessibility enabled. If it was already running, "
                               "fully quit it and launch again so the setting takes effect.", name));
    } else {
        m_status->setMessageType(KMessageWidget::Warning);
        m_status->setText(i18n("Launched %1, but desktop accessibility could not be switched on, "
                               "so agents will not be able to read its pages.", name));
    }
}
