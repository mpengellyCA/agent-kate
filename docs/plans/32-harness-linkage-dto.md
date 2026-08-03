# 32 — Harness Linkage DTO cutover

## Outcome

Harness integrations expose one versioned neutral contract from
`core/internal/harness`. The UI learns harnesses through `harness.list` and
gets one revisioned provider-scoped catalogue through `harness.catalog`; it no
longer calls separate option/model discovery endpoints.

## Contract

- `HarnessDescriptor` identifies a harness and lists tested
  `OperationDescriptor` entries. Missing operations are unsupported.
- `CatalogueSnapshot` carries a revision, `ModelDescriptor` values and typed
  `SettingDescriptor` values. A setting declares `launch`, `nextTurn`, or
  `live` timing.
- `AgentRef`, `AgentSettings`, `AgentSnapshot`, and `AppliedSettings` make
  requested versus effective state explicit. `agent.updateSettings` returns
  the application timing and effective values.
- Requests name a harness and optional provider ID only. Native URLs,
  credentials, environment overlays, bridge secrets, ACP objects, and native
  configuration stay in adapter runtime bindings.

## Adapter mapping

- Claude supplies static permission/effort settings and maps its live model
  list to model descriptors; its controls are live.
- Kimi maps ACP `model`, `thinking`, and `mode` options to neutral keys and
  reads back ACP's applied state after a live update.
- Codex maps app-server `model/list`; model, reasoning effort, and approval
  policy are queued for `turn/start`, so their timing is `nextTurn`.

## Persistence

Thread records now serialize `schemaVersion`, `agentRef`,
`requestedSettings`, and `effectiveSettings`. The store accepts old flat
`backend`, `sessionId`, `model`, `effort`, and `permissionMode` fields on
read, normalizes them losslessly, and writes only the linkage shape after the
read.

## Verification

The harness package validates descriptors/catalogues; adapter tests cover the
protocol fixtures already used by Claude/Kimi/Codex supervisors. The full Go
suite and UI build are the release checks, with socket/desktop sandbox limits
reported separately when applicable.
