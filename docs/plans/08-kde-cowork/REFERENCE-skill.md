# REFERENCE — KDE Plasma Cowork API skill (verbatim)

> Persisted reference for the Cowork design team. Authoritative D-Bus signatures, options
> vardicts, AtspiRole table, and Go snippets. The Context Pack (`00-context-pack.md`) is the
> grounded codebase substrate + design invariants; THIS file is the external-API reference.

> ## ⚠️ CORRECTIONS (from the API-correctness review — see `08-review-findings.md` §D)
> This skill (the upstream source doc) contained errors. Corrected here:
> - **AtspiRole table below is REPLACED** with verified values from
>   `/usr/include/at-spi-2.0/atspi/atspi-constants.h`. The original were legacy pyatspi/ATK ordinals,
>   wrong for the D-Bus `Role()→u` wire values. **`PASSWORD_TEXT=40` (redaction-critical) was missing.**
> - **Portal Go snippet (Layer 1):** it extracts the `Response`-signal results into `results` then
>   **discards them** (`_ = results`) and returns the request path. WRONG — the artifacts you need
>   (`uri`/`streams`/`restore_token`) are IN that vardict; return `results`, not the path.
> - **`OpenPipeWireRemote`** returns the PipeWire FD directly via SCM_RIGHTS in the method reply (read
>   with `UnixFD`, UI-side) — it does NOT use the Request/Response pattern.
> - **`Notify*` signatures** need the `options a{sv}` arg + typed params:
>   `NotifyPointerMotionAbsolute(session:o, options:a{sv}, stream:u, x:d, y:d)`,
>   `NotifyKeyboardKeysym(session:o, options:a{sv}, keysym:i, state:u)`.
> - **Portal injection coords are STREAM-relative** (require a paired ScreenCast stream), not global/
>   window-local — affects coord reconciliation.
> - **`window.desktops` is `VirtualDesktop[]` objects** (`.id` QUuid), not ints; emit `d.id`.
>   `workspace.createDesktop` may return void on some 6.x.
> - **ScreenCast `types` bit-4 "Virtual"** (whole-VD source) is a KDE-specific, **unverified** claim.
> - **`QT_ACCESSIBILITY=1` enables Qt apps only**; Chromium/Firefox often expose no a11y tree on Wayland.

## Architecture rule (from skill)
XDG portal sessions MUST be initiated from `agentkate` (Qt6) because they require a valid
`parent_window` handle (Wayland: `wayland:surface_id`). Pass session tokens/FDs back to `akcore`
over the JSON-RPC bus. KWin scripting and AT-SPI2 can be called directly from `akcore` via D-Bus.
(NB: our Context Pack INV-1 refines this — FDs cannot cross our bus, so PipeWire stays UI-side and
only tokens/node-ids/stills cross.)

---

## Layer 1 — XDG Desktop Portal
Bus `org.freedesktop.portal.Desktop`, path `/org/freedesktop/portal/desktop`. Async
Request/Response: call method → get Request object path → subscribe to `Response` signal on it →
response code `0` success / `1` cancelled / `2` other, plus a results vardict.

### org.freedesktop.portal.Screenshot
`Screenshot(parent_window: s, options: a{sv}) → handle: o`. Options: `handle_token`(s),
`interactive`(b: true=picker, false=active window). Requires `.desktop`
`X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2` for the lower-level KWin path.

### org.freedesktop.portal.ScreenCast
Sequence: `CreateSession(options)→session_handle` → `SelectSources(session,options)` (user picks) →
`Start(session,parent,options)` (returns stream node ids) → `OpenPipeWireRemote(session,options)`
→ FD for `pw_context_connect_fd()`.
`SelectSources` options: `types`(u bitmask 1=Monitor 2=Window 4=Virtual[Plasma 6.5+]),
`multiple`(b), `persist_mode`(u: 0 none/1 transient/2 persistent), `restore_token`(s).
`Start` Response: `streams`(a(ua{sv}) array of (node_id, props)), `restore_token`(s, single-use).
PipeWire Go side: pass FD to a PipeWire context (cgo wrapper or shell to pw-cat/pw-record).

### org.freedesktop.portal.RemoteDesktop
Sequence: `CreateSession` → `SelectDevices(session,options)` → [optional
`ScreenCast.SelectSources`] → `Start` → `NotifyPointerMotionAbsolute(session,options,stream,x,y)` /
`NotifyKeyboardKeysym(session,options,keysym,state)`.
`SelectDevices` options: `types`(u: 1 keyboard/2 pointer/4 touchscreen), `persist_mode`(u).
EIS path (Plasma 6.3+): after `Start`, `ConnectToEIS(session,options)` → libei FD for lower-latency
input — modern replacement for the Notify* calls.

### KDE pre-authorization (Plasma 6.3+)
```bash
flatpak permission-set kde-authorized remote-desktop "" yes
flatpak permission-set kde-authorized screencast "" yes
# or by app-id:
flatpak permission-set kde-authorized remote-desktop io.agentkate.agentkate yes
```
Writes the `kde-authorized` permission-store table. Plasma 6.5+ System Settings → Application
Permissions exposes a GUI incl. revoking saved sessions.

### Go portal Request pattern (skill snippet)
```go
func callPortal(conn *dbus.Conn, method string, args ...interface{}) (dbus.ObjectPath, error) {
    obj := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
    token := fmt.Sprintf("agentkate_%d", time.Now().UnixNano())
    options := map[string]dbus.Variant{"handle_token": dbus.MakeVariant(token)}
    sender := strings.ReplaceAll(conn.Names()[0][1:], ".", "_")
    requestPath := dbus.ObjectPath(fmt.Sprintf(
        "/org/freedesktop/portal/desktop/request/%s/%s", sender, token))
    sigCh := make(chan *dbus.Signal, 1); conn.Signal(sigCh)
    conn.AddMatchSignal(dbus.WithMatchObjectPath(requestPath),
        dbus.WithMatchInterface("org.freedesktop.portal.Request"),
        dbus.WithMatchMember("Response"))
    allArgs := append(args, options) // options always last
    call := obj.Call("org.freedesktop.portal."+method, 0, allArgs...)
    if call.Err != nil { return "", call.Err }
    select {
    case sig := <-sigCh:
        code := sig.Body[0].(uint32); results := sig.Body[1].(map[string]dbus.Variant)
        if code != 0 { return "", fmt.Errorf("portal response code %d", code) }
        _ = results; return requestPath, nil
    case <-time.After(120 * time.Second): return "", fmt.Errorf("portal timeout")
    }
}
```

---

## Layer 2 — KWin D-Bus Scripting (no consent)
Bus `org.kde.KWin`, path `/KWin`. `org.kde.kwin.Scripting.loadScript(path,pluginName)→scriptId:i`
then `start()`. Pattern: write a JS file, load it, run it; the script reports back via
`callDBus(...)` to a service akcore registers, or writes to a well-known path.

### Enumerate windows (skill JS)
```javascript
(function() {
  var result = [];
  for (var client of workspace.stackingOrder) {
    result.push({caption:client.caption, resourceClass:client.resourceClass,
      resourceName:client.resourceName, pid:client.pid, active:client.active,
      fullScreen:client.fullScreen, onAllDesktops:client.onAllDesktops,
      skipTaskbar:client.skipTaskbar, internalId:client.internalId.toString(),
      stackingOrder:client.stackingOrder, x:client.x, y:client.y,
      width:client.width, height:client.height});
  }
  callDBus("io.agentkate.Cowork", "/WindowList", "io.agentKate.Cowork.WindowList",
           "Report", JSON.stringify(result));
})();
```

### Window properties (KWin 6.x)
caption, resourceClass, resourceName, pid, internalId(QUuid), stackingOrder, active, fullScreen,
desktops(VirtualDesktop[]), onAllDesktops, skipTaskbar, x/y/width/height, managed, specialWindow,
transient, transientFor(Window), wantsInput, activities(string[]).

### Workspace signals (persistent script)
windowAdded(window), windowRemoved(window), windowActivated(window),
currentDesktopChanged(old,window), currentActivityChanged(id), screensChanged().

### Go inject-script (skill snippet)
```go
func runKWinScript(conn *dbus.Conn, scriptContent string) error {
    f, _ := os.CreateTemp("", "ak_kwin_*.js"); f.WriteString(scriptContent); f.Close()
    defer os.Remove(f.Name())
    kwin := conn.Object("org.kde.KWin", "/Scripting")
    var scriptId int32
    if err := kwin.Call("org.kde.kwin.Scripting.loadScript", 0, f.Name(),
        "agentkate_tmp").Store(&scriptId); err != nil { return err }
    script := conn.Object("org.kde.KWin",
        dbus.ObjectPath(fmt.Sprintf("/Scripting/Script%d", scriptId)))
    script.Call("org.kde.kwin.Script.run", 0); return nil
}
```

### KWin ScreenShot2 (fast, no dialog)
Bus `org.kde.KWin.ScreenShot2`, path `/org/kde/KWin/ScreenShot2`. Methods:
`CaptureActiveWindow(opts)→fd`, `CaptureWindow(handle,opts)`, `CaptureArea(x,y,w,h,opts)`,
`CaptureScreen(screen,opts)`, `CaptureActiveScreen(opts)`. Opts: `include-cursor`(b),
`native-resolution`(b). Returns a **pipe FD** carrying raw RGBA → read, encode PNG. Returns
`org.kde.KWin.ScreenShot2.Error.NoAuthorized` without the `.desktop` declaration. ~30–70 ms/frame;
on-demand only, not streaming. (NB: pipe FD is local to whichever process calls it — in our
architecture this is a candidate for the UI process, or Go if it can hold the restricted-interface
authorization; the `.desktop` decl is tied to the installed app id.)

---

## Layer 3 — AT-SPI2 Accessibility Tree
Separate bus. 1) session bus `org.a11y.Bus` `/org/a11y/bus` `GetAddress()→s`; check
`org.a11y.Status.IsEnabled`. 2) open a NEW dbus connection to that address. 3) query
`org.a11y.atspi.Registry` at `/org/a11y/atspi/accessible/root`. 4) walk via
`org.a11y.atspi.Accessible`. Force-enable via `QT_ACCESSIBILITY=1`.

### Interfaces
- Registry: `org.a11y.atspi.Accessible.GetChildren()→a(so)` (one (bus_name,object_path) per app).
- Accessible: props Name(s), Role(u), RoleName(s), Description(s), ChildCount(i); methods
  GetChildren()→a(so), GetParent()→(so), GetIndexInParent()→i, GetApplication()→(so),
  GetAttributes()→a{ss}, GetState()→au (bitmask).
- Component: GetExtents(coord_type:u)→(x,y,w,h) [0=screen-global,1=window-local], Contains,
  GetPosition, GetSize. **On Wayland coord_type=0 returns window-local; use 1, reconcile with KWin
  geometry for absolute.**
- Action: GetNActions, GetName(i)→s ("click"/"toggle"/"expand"), DoAction(i)→bool, GetDescription,
  GetKeyBinding.
- Text: GetText(start,end)→s [(0,-1)=all], GetCharacterCount, GetCaretOffset.
- Value: CurrentValue(d), MaximumValue(d), MinimumValue(d), MinimumIncrement(d).

### AtspiRole (CORRECTED — verified vs atspi-constants.h; these are the D-Bus `Role()→u` wire values)
0 INVALID, 7 CHECK_BOX, 8 CHECK_MENU_ITEM, 11 COMBO_BOX, 16 DIALOG, 23 FRAME (top-level window),
26 ICON, 29 LABEL, 31 LIST, 32 LIST_ITEM, 33 MENU, 34 MENU_BAR, 35 MENU_ITEM, 37 PAGE_TAB,
**40 PASSWORD_TEXT** (redaction-critical), 43 BUTTON (PUSH_BUTTON), 44 RADIO_BUTTON, 48 SCROLL_BAR,
51 SLIDER, 52 SPIN_BUTTON, 54 STATUS_BAR, 55 TABLE, 56 TABLE_CELL, 57 TABLE_COLUMN_HEADER,
58 TABLE_ROW_HEADER, 60 TERMINAL, 61 TEXT, 62 TOGGLE_BUTTON, 63 TOOL_BAR, 64 TOOL_TIP, 65 TREE,
66 TREE_TABLE, 68 VIEWPORT, 69 WINDOW, 79 ENTRY, 90 TABLE_ROW.
(Full enum: regenerate from `/usr/include/at-spi-2.0/atspi/atspi-constants.h` before the v2 a11y work.)

### Go walk (skill snippet)
```go
func getA11yBusAddress(c *dbus.Conn) (string, error) {
    var addr string
    err := c.Object("org.a11y.Bus","/org/a11y/bus").Call("org.a11y.Bus.GetAddress",0).Store(&addr)
    return addr, err
}
func connectA11yBus(addr string) (*dbus.Conn, error) { return dbus.Dial(addr) }
// registry GetChildren → a(so) apps; walkNode reads Name/Role props then recurses GetChildren,
// depth-bounded (skill uses maxDepth 3).
```

---

## MCP tool mapping (skill recommendation — our INV-5 renames to desktop_*)
list_windows(KWin,none) · get_window_screenshot/get_active_screenshot(ScreenShot2,desktop-decl) ·
start/stop_screencast(portal+PipeWire,dialog) · read_accessibility_tree/find_ui_element/
click_ui_element/read_element_text(AT-SPI,none) · take_screenshot_portal/start_remote_desktop(portal,dialog).

## Dependencies
`xdg-desktop-portal xdg-desktop-portal-kde at-spi2-core libatspi2.0-dev pipewire
libpipewire-0.3-dev` (Arch: `xdg-desktop-portal xdg-desktop-portal-kde at-spi2-core pipewire`).
Go: `github.com/godbus/dbus/v5 v5.1.0` (+ PipeWire cgo/shell). Verify services:
`systemctl --user status xdg-desktop-portal xdg-desktop-portal-kde`; `dbus-send --session
--print-reply --dest=org.a11y.Bus /org/a11y/bus org.a11y.Bus.GetAddress`; `qdbus org.kde.KWin
/KWin org.kde.KWin.supportInformation`.

## Gotchas
- **Plasma 6.5.x portal dialogs ignore virtual input** (ydotool/VNC) — fixed 6.6. On 6.5.x need
  physical click for the first consent, then persistent token; or pre-authorize to bypass.
- **AT-SPI Wayland coords** window-local — combine with KWin geometry for absolute.
- **ScreenShot2 needs the `.desktop` decl** or `NoAuthorized`; must be installed to
  `~/.local/share/applications/` or `/usr/share/applications/`.
- **Portal sessions need a valid parent_window** (`wayland:surface_id`) — from Qt6 UI only.
- **ScreenCast restore tokens are single-use** — each use returns a new one; rotate or re-prompt.
- **KWin scripts persist until KWin restarts** — `org.kde.kwin.Script.stop()` after one-shot reads;
  persistent event scripts → install as packages via `kpackagetool6`.

## Reference links
KWin Scripting API https://develop.kde.org/docs/plasma/kwin/api/ · ScreenCast spec
https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.ScreenCast.html ·
RemoteDesktop spec
https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.RemoteDesktop.html ·
AT-SPI2 https://www.freedesktop.org/wiki/Accessibility/AT-SPI2/ + https://github.com/GNOME/at-spi2-core ·
Portal pre-auth https://develop.kde.org/docs/administration/portal-permissions/ ·
kwin-mcp https://github.com/isac322/kwin-mcp · godbus https://github.com/godbus/dbus
