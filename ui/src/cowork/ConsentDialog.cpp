#include "ConsentDialog.h"

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

QString capabilityVerb(const QString &cap)
{
    if (cap == QLatin1String("window_list")) {
        return i18n("see the list of windows open on your desktop");
    }
    if (cap == QLatin1String("a11y_read")) {
        return i18n("read the on-screen text and controls of an application");
    }
    if (cap == QLatin1String("screenshot")) {
        return i18n("take a screenshot");
    }
    if (cap == QLatin1String("screencast")) {
        return i18n("continuously watch your screen");
    }
    return i18n("access your desktop (%1)", cap);
}

QString targetText(const QJsonObject &t)
{
    const QString kind = t.value(QStringLiteral("kind")).toString();
    const QString label = t.value(QStringLiteral("label")).toString();
    if (kind == QLatin1String("window")) {
        const QString rc = t.value(QStringLiteral("resourceClass")).toString();
        if (!label.isEmpty()) {
            return i18n("the window “%1”", label);
        }
        if (!rc.isEmpty()) {
            return i18n("a %1 window", rc);
        }
        return i18n("a specific window");
    }
    if (kind == QLatin1String("app")) {
        return i18n("the application “%1”", t.value(QStringLiteral("resourceClass")).toString());
    }
    if (kind == QLatin1String("screen")) {
        return i18n("a whole screen");
    }
    if (kind == QLatin1String("vdesktop") || kind == QLatin1String("sandbox")) {
        return i18n("the sandbox virtual desktop");
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
    banner->setText(i18n("An agent is asking to %1.", capabilityVerb(cap)));
    layout->addWidget(banner);

    auto *detail = new QLabel(this);
    detail->setWordWrap(true);
    detail->setTextFormat(Qt::RichText);
    detail->setText(i18n("<b>%1</b> wants to <b>%2</b><br>on <b>%3</b>.<br><br>"
                         "Only allow this if you understand what the agent will see. "
                         "Anything visible may be read by the AI.",
                         thread.toHtmlEscaped(), capabilityVerb(cap), targetText(target)));
    layout->addWidget(detail);

    auto *form = new QFormLayout;
    m_scope = new QComboBox(this);
    m_scope->addItem(i18n("Just this once"), QStringLiteral("once"));
    m_scope->addItem(i18n("For this agent session"), QStringLiteral("session"));
    m_scope->addItem(i18n("For 15 minutes"), QStringLiteral("timed"));
    m_scope->addItem(i18n("Until I revoke it"), QStringLiteral("until_revoked"));
    // Default to the request's suggestion if present, else the safest (once).
    const QString suggested = request.value(QStringLiteral("suggestedScope")).toString();
    int idx = m_scope->findData(suggested.isEmpty() ? QStringLiteral("once") : suggested);
    if (idx >= 0) {
        m_scope->setCurrentIndex(idx);
    }
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
