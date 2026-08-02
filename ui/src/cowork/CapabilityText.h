#pragma once

#include <KLocalizedString>

#include <QString>

// The single plain-language vocabulary for Cowork capability keys.
//
// It used to exist twice: a fuller map in CoworkPanel.cpp and a shorter one in
// ConsentDialog.cpp that was missing launch_browser, vd_sandbox and all three R2
// keys — so the browser-launch consent prompt literally read "access your desktop
// (launch_browser)" (audit F50). A consent prompt the user cannot read is not
// consent, so both now render from here and a new capability key is translated in
// exactly one place.
//
// Naming honesty (audit F32): vd_sandbox is an ORGANIZATIONAL boundary — a separate
// KDE virtual desktop so the agent's windows stay out of the user's way. It contains
// nothing (same uid, same session, same files). It is therefore never called a
// "sandbox" in the UI; "separate desktop" is what it actually is.
namespace CoworkCaps {

// Lower-case verb phrase, used inside a sentence ("Agent X can <verb> on …",
// "An agent is asking to <verb>.").
inline QString verb(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("see open windows");
    if (key == QLatin1String("screenshot")) return i18n("take screenshots");
    if (key == QLatin1String("a11y_read")) return i18n("read app contents");
    if (key == QLatin1String("screencast")) return i18n("watch the screen");
    if (key == QLatin1String("launch_browser")) return i18n("open a browser");
    if (key == QLatin1String("vd_sandbox")) return i18n("use a separate desktop");
    if (key == QLatin1String("a11y_action")) return i18n("click buttons and controls as you");
    if (key == QLatin1String("input_inject")) return i18n("type and press keys as you");
    if (key == QLatin1String("pointer_control")) return i18n("move and click the mouse as you");
    // Unknown key: never leak the raw internal token into a consent sentence — say
    // plainly that we do not know what it does, which is the honest prompt to deny.
    return i18n("do something on your desktop we cannot describe");
}

// Title-ish case, for the control-centre tile headline.
inline QString title(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("See open windows");
    if (key == QLatin1String("screenshot")) return i18n("Take screenshots");
    if (key == QLatin1String("a11y_read")) return i18n("Read app contents");
    if (key == QLatin1String("screencast")) return i18n("Watch the screen");
    if (key == QLatin1String("launch_browser")) return i18n("Open a browser");
    if (key == QLatin1String("vd_sandbox")) return i18n("Separate desktop");
    if (key == QLatin1String("a11y_action")) return i18n("Click controls");
    if (key == QLatin1String("input_inject")) return i18n("Type as you");
    if (key == QLatin1String("pointer_control")) return i18n("Move the mouse");
    // SECURITY (audit F35): the same rule verb() follows. `return key;` put the raw
    // internal token ("frobnicate_v9") on a control-centre tile — the exact leak F50
    // was about, left in the register that headlines a standing-grant switch. Unreachable
    // while the tile list comes from AllToggleable(), but this file is now the single
    // authority for user-facing capability copy, so it must be safe on its own terms.
    return i18n("Unrecognised desktop permission");
}

// One-line description shown under the tile title / as its tooltip.
inline QString description(const QString &key)
{
    if (key == QLatin1String("window_list")) return i18n("List the windows you have open");
    if (key == QLatin1String("screenshot")) return i18n("Capture what's on your screen");
    if (key == QLatin1String("a11y_read")) return i18n("Read the text and controls in apps");
    if (key == QLatin1String("screencast")) return i18n("Watch your screen live as it changes");
    if (key == QLatin1String("launch_browser")) return i18n("Open a browser it can read and use");
    if (key == QLatin1String("vd_sandbox")) return i18n("Work on a separate virtual desktop, out of your way");
    if (key == QLatin1String("a11y_action")) return i18n("Click buttons and controls as you");
    if (key == QLatin1String("input_inject")) return i18n("Type text and press keys as you");
    if (key == QLatin1String("pointer_control")) return i18n("Move the pointer and click as you");
    // An empty description reads as "nothing to worry about" under a tile whose switch
    // arms a standing, no-prompt grant. Say the honest thing instead (audit F35).
    return i18n("Agent Kate does not recognise this permission and cannot tell you what it allows. "
                "Leave it off.");
}

} // namespace CoworkCaps
