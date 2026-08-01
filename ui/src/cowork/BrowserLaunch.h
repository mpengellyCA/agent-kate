#pragma once

#include <QList>
#include <QString>
#include <QStringList>

// BrowserLaunch opens a web browser with its accessibility tree forced on, so a
// Cowork agent can read and activate page elements via AT-SPI. Browsers don't expose
// web content over AT-SPI by default: Chromium-family browsers need the
// --force-renderer-accessibility=complete flag, and Firefox-family browsers need
// GNOME_ACCESSIBILITY=1 in the environment at startup. We set the right one per
// engine family. This is user-initiated (a menu in the Cowork panel) — launching
// arbitrary executables is deliberately not an agent capability.
namespace BrowserLaunch {

struct Browser {
    QString name;    // display name, e.g. "Zen"
    QString command; // executable: a name on PATH or an absolute path
    QString family;  // "firefox" | "chromium" — selects the a11y mechanism
};

// detected probes PATH for well-known browsers and returns those present, in display
// order, de-duplicated by display name.
QList<Browser> detected();

// custom returns the user-added browsers persisted in KConfig.
QList<Browser> custom();

// addCustom persists a user-added browser (no-op if an identical entry exists).
void addCustom(const Browser &b);

// all returns detected() + custom(), de-duplicated by command.
QList<Browser> all();

// names returns the display names from all(), for showing the agent its options.
QStringList names();

// find resolves a browser by display name or command (case-insensitive) against
// all(); the returned Browser has an empty command if there is no match.
Browser find(const QString &nameOrCommand);

// preferred returns the browser the agent should open by default: the user's chosen
// one (KConfig) if still available, else the first of all(). Empty command if none.
Browser preferred();

// setPreferred records the user's default browser for agents (by command).
void setPreferred(const QString &command);

// guessFamily infers the engine family ("firefox" | "chromium") from an executable's
// name, so the custom-browser picker can pre-select the right one.
QString guessFamily(const QString &commandOrPath);

// launch starts b with the PER-PROCESS accessibility switch for its engine family:
// GNOME_ACCESSIBILITY=1 in the child's environment (firefox family) or
// --force-renderer-accessibility=complete on its command line (chromium family).
// Returns false and sets *error on failure. NOTE: these only take effect on a FRESH
// browser process — if the browser is already running, the new invocation just
// signals the existing instance and accessibility is not enabled.
//
// IT DOES NOT TOUCH org.a11y.Status. Chromium additionally needs that DESKTOP-WIDE
// flag on at launch, but flipping it is a global permission change the whole session
// sees, so it belongs to the caller who knows whether the human consented:
//
//   - Two audits landed on this function. F8: the flip was undisclosed, was left on
//     after a decline or the kill-switch, and — because it happened HERE, inside the
//     launch — it ran even when CoworkPortal had just decided not to flip, so an
//     agent-triggered `desktop_launch_browser` still switched the desktop into
//     accessibility mode out of a refused grant. Worse, this function never parked
//     the pre-flip values, so nothing could put them back: the change outlived the
//     app. F12: it flipped through a QDBusInterface, whose constructor performs a
//     SYNCHRONOUS introspection call (~25 s default timeout) followed by two blocking
//     Set calls — up to ~75 s of frozen GUI on a wedged org.a11y.Bus, on the very
//     thread that paints the window.
//   - CoworkPortal::enableAtspiStatusForLaunch is the one place that does it right:
//     it parks the originals on disk BEFORE the flip (so a crash is recoverable),
//     adopts another instance's parked values rather than capturing already-flipped
//     ones, uses bounded 2 s raw D-Bus messages, and restores on teardown.
//
// So: call CoworkPortal::enableAtspiStatusForLaunch() (or the panel's portal-backed
// path) first when the flip is warranted, then call this.
bool launch(const Browser &b, QString *error);

} // namespace BrowserLaunch
