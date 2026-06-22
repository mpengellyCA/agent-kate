# 08 — KDE Plasma Cowork

Let the user share parts of their KDE Plasma desktop with Agent Kate — see a window / a screen / the
whole desktop / an isolated virtual-desktop sandbox — and optionally let the agent *control* it, with
**every access gated by explicit, auditable, revocable user permission.** Three API layers (XDG
Desktop Portal capture/control, KWin D-Bus scripting, AT-SPI2 accessibility), split across the Go core
(consent authority + sensing) and the Qt6 UI (the only process with a Wayland surface, so it owns all
portals/PipeWire).

## Read in this order

| File | What it fixes |
|---|---|
| [00-context-pack.md](00-context-pack.md) | The constitution: grounded `file:line` facts + invariants **INV-1..INV-7**. Read first. |
| [01-consent-spine.md](01-consent-spine.md) | **Canonical** consent/grant/audit/kill-switch/RPC contract. Slices conform to it. |
| [02-capture.md](02-capture.md) | Screenshot + screencast + PipeWire + the core↔UI portal round-trip (UI-side). |
| [03-introspection.md](03-introspection.md) | KWin window list + events + AT-SPI2 tree (Go-side D-Bus). |
| [04-control.md](04-control.md) | RemoteDesktop input injection + AT-SPI actions (highest risk, R2). |
| [05-sandbox.md](05-sandbox.md) | KWin virtual-desktop sandbox confinement (organizational, not a security boundary). |
| [06-ui-panel.md](06-ui-panel.md) | Cowork panel, consent dialogs, active-grants, live preview, kill-switch (KDE-native). |
| [**07-wiring-and-roadmap.md**](07-wiring-and-roadmap.md) | **The reconciled contract**: unified wire surface, collision resolutions, package layout, exact wiring edits, v1/v2/v3 roadmap, spike register, trim list. **Wins on naming/shape conflicts.** |
| [**08-review-findings.md**](08-review-findings.md) | **Adversarial review gate**: the findings (2 root causes + 2 API dead-ends), remediations, the **amended v1 scope** (adds the per-connection identity keystone), and the **open trust-boundary decision** for the user. **Amends 07 §4.1.** |
| [REFERENCE-skill.md](REFERENCE-skill.md) | API reference (CORRECTED per review §D): D-Bus signatures, options vardicts, AtspiRole table, Go/JS snippets. |

## v1 walking skeleton (frozen — see 07 §4.1)

Build system + `godbus` dep + opt-in `cowork` MCP server (off by default) · consent spine
(store/`Authorize`/grant-broker/audit/kill-switch) · `desktop_list_windows` (no portal) ·
consent-gated `desktop_screenshot` via the portal round-trip · Cowork panel (active-grants +
kill-switch) · `ControlConsentDialog` R2 shell (wired to no R2 tool yet). **No control, screencast,
a11y, or sandbox in v1.**

## Director ratifications (resolved 2026-06-21)

Recorded in **[07 §0 Decisions (ratified)](07-wiring-and-roadmap.md)**: separate opt-in `cowork` MCP
server (off by default); timed-R2 grants only when sandbox-confined; hash-chain audit for v1 (signed →
v3); `respondGrant` origin-assertion → v2; `remote_desktop` capability + temp-file PNG delivery cut.
The plan is ready for user sign-off before implementation.
