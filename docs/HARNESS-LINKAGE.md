# Harness Linkage contract

Harness Linkage is the public neutral contract owned by
`core/internal/harness`.

`HarnessDescriptor` carries a stable ID, display identity, version/health
metadata, and typed `OperationDescriptor` entries. Operations are opt-in:
there is no boolean capability matrix and no UI inference from a harness ID.

`CatalogueSnapshot` is revisioned by harness/provider scope and contains:

- `ModelDescriptor` with an ID, display name, model-specific reasoning efforts
  and UI metadata;
- `SettingDescriptor` with a typed key, choices, dependencies, default and
  effective values, and an application timing of `launch`, `nextTurn`, or
  `live`.

Lifecycle calls use `AgentLaunch`, `AgentRef`, `AgentSettings`,
`AgentSnapshot`, and `AppliedSettings`. `AppliedSettings` distinguishes the
requested values from effective native values and names rejected settings.

The core exposes `harness.list` and `harness.catalog`; clients send provider
identity only. Credentials, provider URLs, environment overlays, ACP server
definitions, bridge secrets, and native protocol blobs are internal runtime
bindings. `agent.updateSettings` is the one typed running-agent mutation.

Existing flat session records are normalized to `AgentRef`, requested settings,
and effective settings on read, then persisted only in the new shape. Native
event translation and canonical transcript event shapes are unchanged.
