# Adding an agent harness

Harness integrations are built around the versioned neutral contract in
`core/internal/harness`. A harness adapter owns its native CLI/ACP/app-server
binding; it must not expose native request objects, environment overlays,
credentials, bridge secrets, or raw configuration in a JSON DTO.

## Onboarding contract

1. Implement `Descriptor() HarnessDescriptor`. Declare only tested
   `OperationDescriptor` entries; an absent operation is unsupported.
2. Implement `Catalogue(ctx, scope) CatalogueSnapshot`. It returns models and
   typed setting descriptors with dependencies, defaults, and `launch`,
   `nextTurn`, or `live` application timing. The scope contains a harness and
   provider ID only.
3. Map `AgentLaunch` and `UpdateSettings` to native launch/control requests.
   Return `AppliedSettings`, including effective values, timing, and any
   explicit rejected settings.
4. Translate native events into the canonical transcript event shapes. Keep a
   normalized event log if the native harness cannot replay one itself.
5. Supply a fake CLI/server fixture and run the shared conformance coverage:
   descriptor/catalogue validation, model-effort filtering, launch, settings,
   cancellation, native failure, and transcript recovery.

The core publishes descriptors with `harness.list`, a provider-scoped
catalogue with `harness.catalog`, and running-session changes with
`agent.updateSettings`. UI code must wait for a current descriptor and
catalogue rather than carrying a harness-name fallback.

## Adapter rules

- Keep protocol vocabulary inside the adapter: Kimi's ACP `thinking`, Codex
  app-server requests, and Claude control messages map to neutral setting
  keys at this boundary.
- Do not emulate missing native behavior. Declare support only once an
  end-to-end fixture proves it.
- Codex model/effort/approval changes become effective on the next
  `turn/start`; report `nextTurn`. Kimi ACP configuration updates report the
  actual live values returned by ACP. Claude reports its control-channel
  result.
- Persisted sessions use `AgentRef`, requested settings, and effective
  settings. The store migrates old flat session records when it reads them.

Adding a new harness requires registration in `run.go` and no New Agent or
running-agent setup special case.
