#include "ControlConsentDialog.h"

#include <KColorScheme>
#include <KLocalizedString>
#include <KMessageWidget>

#include <QDialogButtonBox>
#include <QEvent>
#include <QJsonValue>
#include <QKeyEvent>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QVBoxLayout>

namespace {
// The phrase the user must type to enable Allow. Kept short but deliberate.
const QString kPhrase = QStringLiteral("allow control");

// ReturnGuard swallows Return/Enter inside the phrase field.
//
// SECURITY (audit F3): the typed phrase is the only thing standing between a
// pre-authorized input_inject grant and self-approval of an R2 request. The core-side
// guard now refuses to type into an Agent Kate window at all, but this dialog must not
// depend on that being airtight: a keystroke stream that reaches this field must not be
// able to COMMIT it. So Enter here does nothing — Allow is reachable only by activating
// the button (mouse, or Tab-then-Space), and Escape still cancels.
class ReturnGuard : public QObject
{
public:
    using QObject::QObject;

protected:
    bool eventFilter(QObject *obj, QEvent *ev) override
    {
        if (ev->type() == QEvent::KeyPress || ev->type() == QEvent::KeyRelease) {
            const int key = static_cast<QKeyEvent *>(ev)->key();
            if (key == Qt::Key_Return || key == Qt::Key_Enter) {
                return true; // eaten: never reaches the line edit or the dialog
            }
        }
        return QObject::eventFilter(obj, ev);
    }
};
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

    // Disclose the desktop-wide side effect of desktop access (audit F8). This is a real
    // global permission change — while Cowork is on, every application on this session
    // exports its accessibility tree to any process that asks — and the human deciding
    // this prompt is entitled to know it before saying yes.
    auto *a11yNote = new QLabel(
        i18n("<b>Note:</b> while desktop access is on, Agent Kate switches your session's "
             "accessibility service on (<tt>org.a11y.Status</tt>) so applications expose "
             "their windows and controls. Every app on this desktop becomes readable that "
             "way, by any program in your session. Your original setting is put back when "
             "the last agent's desktop access is switched off, when you hit the kill-switch, "
             "and when Agent Kate exits."),
        this);
    a11yNote->setWordWrap(true);
    a11yNote->setTextFormat(Qt::RichText);
    layout->addWidget(a11yNote);

    auto *prompt = new QLabel(
        i18n("To allow this once, type <b>%1</b> below and click <b>Allow once</b>. "
             "This cannot be remembered.", kPhrase),
        this);
    prompt->setWordWrap(true);
    layout->addWidget(prompt);

    auto *phrase = new QLineEdit(this);
    phrase->setPlaceholderText(kPhrase);
    phrase->installEventFilter(new ReturnGuard(phrase));
    layout->addWidget(phrase);

    auto *buttons = new QDialogButtonBox(this);
    m_allow = buttons->addButton(i18n("Allow once"), QDialogButtonBox::AcceptRole);
    auto *deny = buttons->addButton(i18n("Deny"), QDialogButtonBox::RejectRole);
    m_allow->setEnabled(false);
    // SECURITY (audit F3): Allow is NEVER the default and never auto-defaults, so no
    // Enter/Return anywhere in this dialog can activate it — not from the phrase field
    // (the ReturnGuard eats it there), and not from any other focused child (Deny holds
    // the default, so a stray Enter can only DENY, which is the safe direction).
    // QDialogButtonBox promotes the first AcceptRole button to default when no default
    // button exists, so Deny must be set explicitly and must stay set.
    m_allow->setAutoDefault(false);
    m_allow->setDefault(false);
    deny->setAutoDefault(true);
    deny->setDefault(true);
    phrase->setFocus();
    connect(phrase, &QLineEdit::textChanged, this, [this](const QString &t) {
        // Typing the phrase only ENABLES Allow; committing it is a separate, deliberate
        // activation of the button.
        //
        // A keystroke stream that reached this dialog could still Tab to Allow and press
        // Space; blocking that here would mean making Allow keyboard-unreachable, which
        // locks keyboard-only humans out of their own consent prompt. So the closure is
        // upstream, in three layers, none of which this dialog relies on being perfect:
        //   * core, before the ops leave: resolveInjectTarget refuses to START typing into
        //     an Agent Kate window, and focusVerifiedInjectTarget re-proves focus AFTER the
        //     consent wait (a failure there is fatal to the batch, not a warning);
        //   * core, for the whole span of a TIMED script: a KWin window-activation watch
        //     aborts the remaining ops the moment focus leaves the granted window;
        //   * UI, the backstop that needs no compositor: CoworkPortal hooks
        //     QGuiApplication::focusWindowChanged and aborts any playback in flight as soon
        //     as one of OUR windows — this one included — takes focus.
        m_allow->setEnabled(t.trimmed().compare(kPhrase, Qt::CaseInsensitive) == 0);
    });
    connect(m_allow, &QPushButton::clicked, this, [this] {
        m_allowed = true;
        accept();
    });
    connect(deny, &QPushButton::clicked, this, &QDialog::reject);
    layout->addWidget(buttons);
}
