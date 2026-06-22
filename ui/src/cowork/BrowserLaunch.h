#pragma once

#include <QList>
#include <QString>

// BrowserLaunch opens a web browser with its accessibility tree forced on, so a
// Cowork agent can read and activate page elements via AT-SPI. Browsers don't expose
// web content over AT-SPI by default: Chromium-family browsers need the
// --force-renderer-accessibility flag, and Firefox-family browsers need
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

// launch starts b with accessibility forced on for its engine family. Returns false
// and sets *error on failure. NOTE: the flag/env only takes effect on a FRESH browser
// process — if the browser is already running, the new invocation just signals the
// existing instance and accessibility is not enabled.
bool launch(const Browser &b, QString *error);

} // namespace BrowserLaunch
