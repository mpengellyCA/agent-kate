#include "ConsentDialog.h"

#include "CapabilityText.h"

#include <KLocalizedString>
#include <KMessageWidget>

#include <QComboBox>
#include <QDialogButtonBox>
#include <QFormLayout>
#include <QJsonValue>
#include <QLabel>
#include <QPushButton>
#include <QVBoxLayout>

namespace {

// The target sentence fragment. Window titles and resource classes are ATTACKER
// CONTROLLABLE (any app on the desktop picks its own title) and this fragment is
// substituted into a Qt::RichText label, so every interpolated value is escaped
// here — a consent prompt whose text can be rewritten by the thing being consented
// to is not a consent prompt.
QString targetText(const QJsonObject &t)
{
    const QString kind = t.value(QStringLiteral("kind")).toString();
    const QString label = t.value(QStringLiteral("label")).toString().toHtmlEscaped();
    if (kind == QLatin1String("window")) {
        const QString rc = t.value(QStringLiteral("resourceClass")).toString().toHtmlEscaped();
        if (!label.isEmpty()) {
            return i18n("the window “%1”", label);
        }
        if (!rc.isEmpty()) {
            return i18n("a %1 window", rc);
        }
        return i18n("a specific window");
    }
    if (kind == QLatin1String("app")) {
        return i18n("the application “%1”",
                    t.value(QStringLiteral("resourceClass")).toString().toHtmlEscaped());
    }
    if (kind == QLatin1String("screen")) {
        return i18n("a whole screen");
    }
    if (kind == QLatin1String("vdesktop") || kind == QLatin1String("sandbox")) {
        // Honesty (audit F32): a separate virtual desktop is an ORGANIZATIONAL
        // boundary, not containment — never call it a sandbox in the UI.
        return i18n("a separate virtual desktop");
    }
    if (kind == QLatin1String("any")) {
        return i18n("your desktop");
    }
    return i18n("your desktop");
}

} // namespace

ConsentDialog::ConsentDialog(const QJsonObject &request, QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18n("Agent Kate — desktop access request"));
    setModal(true);

    const QString cap = request.value(QStringLiteral("capability")).toString();
    const QString thread = request.value(QStringLiteral("threadTitle")).toString(
        request.value(QStringLiteral("threadId")).toString());
    const QJsonObject target = request.value(QStringLiteral("target")).toObject();

    auto *layout = new QVBoxLayout(this);

    auto *banner = new KMessageWidget(this);
    banner->setMessageType(KMessageWidget::Warning);
    banner->setCloseButtonVisible(false);
    banner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-password")));
    banner->setText(i18n("An agent is asking to %1.", CoworkCaps::verb(cap)));
    layout->addWidget(banner);

    auto *detail = new QLabel(this);
    detail->setWordWrap(true);
    detail->setTextFormat(Qt::RichText);
    detail->setText(i18n("<b>%1</b> wants to <b>%2</b><br>on <b>%3</b>.<br><br>"
                         "Only allow this if you understand what the agent will see. "
                         "Anything visible may be read by the AI.",
                         thread.toHtmlEscaped(), CoworkCaps::verb(cap), targetText(target)));
    layout->addWidget(detail);

    auto *form = new QFormLayout;
    m_scope = new QComboBox(this);
    m_scope->addItem(i18n("Just this once"), QStringLiteral("once"));
    m_scope->addItem(i18n("For this agent session"), QStringLiteral("session"));
    m_scope->addItem(i18n("For 15 minutes"), QStringLiteral("timed"));
    m_scope->addItem(i18n("Until I revoke it"), QStringLiteral("until_revoked"));
    // SECURITY (audit F50): the preselection is ALWAYS the safest scope, never the
    // core's `suggestedScope`. That suggestion is a convenience hint set per
    // capability (session for browser/screencast) and it decided, by default, how long
    // an agent keeps access — so the click-through path handed out the widest scope on
    // offer. The wider scopes stay one keystroke away in the list; the user picks them.
    m_scope->setCurrentIndex(m_scope->findData(QStringLiteral("once")));
    form->addRow(i18n("Allow for:"), m_scope);
    layout->addLayout(form);

    auto *buttons = new QDialogButtonBox(this);
    auto *allow = buttons->addButton(i18n("Allow"), QDialogButtonBox::AcceptRole);
    auto *deny = buttons->addButton(i18n("Deny"), QDialogButtonBox::RejectRole);
    deny->setDefault(true); // safe default
    deny->setFocus();
    allow->setIcon(QIcon::fromTheme(QStringLiteral("dialog-ok")));
    connect(allow, &QPushButton::clicked, this, [this] {
        m_allowed = true;
        accept();
    });
    connect(deny, &QPushButton::clicked, this, &QDialog::reject);
    layout->addWidget(buttons);
}

QString ConsentDialog::scope() const
{
    return m_scope ? m_scope->currentData().toString() : QStringLiteral("once");
}

int ConsentDialog::expiresInSec() const
{
    return scope() == QLatin1String("timed") ? 15 * 60 : 0;
}
