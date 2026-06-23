#include "ControlConsentDialog.h"

#include <KColorScheme>
#include <KLocalizedString>
#include <KMessageWidget>

#include <QDialogButtonBox>
#include <QJsonValue>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QVBoxLayout>

namespace {
// The phrase the user must type to enable Allow. Kept short but deliberate.
const QString kPhrase = QStringLiteral("allow control");
} // namespace

ControlConsentDialog::ControlConsentDialog(const QJsonObject &request, QWidget *parent)
    : QDialog(parent)
{
    setWindowTitle(i18n("Agent Kate — desktop CONTROL request"));
    setModal(true);

    const QString thread = request.value(QStringLiteral("threadTitle")).toString(
        request.value(QStringLiteral("threadId")).toString());
    const QJsonObject preview = request.value(QStringLiteral("actionPreview")).toObject();

    auto *layout = new QVBoxLayout(this);

    auto *banner = new KMessageWidget(this);
    banner->setMessageType(KMessageWidget::Error);
    banner->setCloseButtonVisible(false);
    banner->setIcon(QIcon::fromTheme(QStringLiteral("dialog-warning")));
    banner->setText(i18n("An agent is asking to CONTROL your desktop — to act as if it "
                         "were you. Read the exact action below before allowing."));
    layout->addWidget(banner);

    // Render the literal action (never a bare tool name).
    auto *detail = new QLabel(this);
    detail->setWordWrap(true);
    detail->setTextFormat(Qt::RichText);
    QString mechanism = preview.value(QStringLiteral("mechanism")).toString();
    QString app = preview.value(QStringLiteral("appName")).toString();
    QString win = preview.value(QStringLiteral("windowTitle")).toString();
    QString element = preview.value(QStringLiteral("element")).toString();
    QString extra = preview.value(QStringLiteral("detail")).toString();
    QString body = i18n("<b>%1</b> wants to perform this action:<br><br>", thread.toHtmlEscaped());
    if (!mechanism.isEmpty()) {
        body += i18n("&nbsp;&nbsp;Mechanism: <b>%1</b><br>", mechanism.toHtmlEscaped());
    }
    if (!app.isEmpty()) {
        body += i18n("&nbsp;&nbsp;Application: <b>%1</b><br>", app.toHtmlEscaped());
    }
    if (!win.isEmpty()) {
        body += i18n("&nbsp;&nbsp;Window: <b>%1</b><br>", win.toHtmlEscaped());
    }
    if (!element.isEmpty()) {
        body += i18n("&nbsp;&nbsp;Element: <b>%1</b><br>", element.toHtmlEscaped());
    }
    if (!extra.isEmpty()) {
        body += i18n("&nbsp;&nbsp;Detail: <b>%1</b><br>", extra.toHtmlEscaped());
    }
    detail->setText(body);
    layout->addWidget(detail);

    // Dangerous accent (native Breeze negative colour, no hard-coded RGB).
    KColorScheme scheme(QPalette::Active, KColorScheme::View);
    QPalette pal = detail->palette();
    pal.setBrush(QPalette::WindowText, scheme.foreground(KColorScheme::NegativeText));
    detail->setPalette(pal);

    auto *prompt = new QLabel(
        i18n("To allow this once, type <b>%1</b> below. This cannot be remembered.", kPhrase), this);
    prompt->setWordWrap(true);
    layout->addWidget(prompt);

    auto *phrase = new QLineEdit(this);
    phrase->setPlaceholderText(kPhrase);
    layout->addWidget(phrase);

    auto *buttons = new QDialogButtonBox(this);
    m_allow = buttons->addButton(i18n("Allow once"), QDialogButtonBox::AcceptRole);
    auto *deny = buttons->addButton(i18n("Deny"), QDialogButtonBox::RejectRole);
    m_allow->setEnabled(false);
    // Both can be the Enter target; we swap which is the default below as the phrase is
    // typed. Focus the input so the user can type immediately.
    m_allow->setAutoDefault(true);
    deny->setAutoDefault(true);
    deny->setDefault(true); // until the phrase matches, Enter is the safe Deny
    phrase->setFocus();
    connect(phrase, &QLineEdit::textChanged, this, [this, deny](const QString &t) {
        const bool ok = t.trimmed().compare(kPhrase, Qt::CaseInsensitive) == 0;
        m_allow->setEnabled(ok);
        // Once the user has typed the confirmation phrase, pressing Enter must ALLOW
        // (that is the whole point of typing it) — so make Allow the default and demote
        // Deny. While the phrase is incomplete, Enter stays the safe Deny.
        m_allow->setDefault(ok);
        deny->setDefault(!ok);
    });
    connect(m_allow, &QPushButton::clicked, this, [this] {
        m_allowed = true;
        accept();
    });
    connect(deny, &QPushButton::clicked, this, &QDialog::reject);
    layout->addWidget(buttons);
}
