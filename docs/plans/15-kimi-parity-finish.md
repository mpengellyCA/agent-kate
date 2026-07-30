# Plan 15 — Kimi parity finish (option discovery, engine-aware quick menu, session browse)

Closes the remaining Plan 14 gaps that make the Kimi engine feel incomplete in
the UI. All three features are additive; none may introduce a `backend == "kimi"`
string compare outside the kimi package / its adapter (see docs/HARNESSES.md).

## Why

Kimi's model / thinking / mode vocabularies are **discovered per session** from
the `session/new` handshake's `configOptions`. Today the UI only caches them
(KConfig `Agent/kimiOpt-*`) *after* a Kimi agent has already started and emitted
its init event. So on the **first** Kimi agent the pickers are empty — Model
shows only the `CLI default…` placeholder, When-to-ask shows only `CLI default`,
Thinking shows only `Default`. The user cannot pick a model or change options
before the first run. Two smaller gaps: the roster split-button quick menu is
hardcoded to Claude tiers, and Kimi session browse (advertised via
`sessionCapabilities.list`) is unimplemented.

## Verified protocol facts (probed against real kimi 0.30.0)

ACP is newline-delimited JSON-RPC 2.0 over stdio (see `core/internal/kimi/acp.go`).

`initialize` (params: `protocolVersion:1`, `clientCapabilities`, `clientInfo`)
→ result includes `agentCapabilities.sessionCapabilities: {list:{}, resume:{}}`.

`session/new` (params: `cwd`, `mcpServers:[]`) → result:
```json
{ "sessionId": "session_…",
  "configOptions": [
    {"type":"select","id":"model","name":"Model","category":"model",
     "currentValue":"kimi-code/k3",
     "options":[{"value":"kimi-code/kimi-for-coding","name":"K2.7 Coding"},
                {"value":"kimi-code/kimi-for-coding-highspeed","name":"K2.7 Coding Highspeed"},
                {"value":"kimi-code/k3","name":"K3"},
                {"value":"kimi-code/k3-256k","name":"K3-256k"}]},
    {"type":"select","id":"thinking","name":"Thinking","currentValue":"high",
     "options":[{"value":"low","name":"Low"},{"value":"high","name":"High"},{"value":"max","name":"Max"}]},
    {"type":"select","id":"mode","name":"Mode","currentValue":"default",
     "options":[{"value":"default","name":"Default",...},{"value":"plan",...},{"value":"auto",...},{"value":"yolo",...}]}
  ] }
```
No prompt is sent, so **no model inference / token spend** occurs. (Note:
`session/new` does persist a throwaway session in kimi's store — acceptable.)

`session/list` (params: optional `cwd` to filter) → result:
```json
{ "sessions": [ {"sessionId":"session_…","cwd":"/path","title":"…","updatedAt":"2026-07-30T18:21:45.691Z"} ],
  "nextCursor": null }
```
Filtering by `cwd` returns only sessions started in that directory.

Each IPC request is dispatched on its own goroutine (bounded per connection), so
a ~1–2s probe RPC never blocks other calls.

---

## Feature 1 — Lazy option discovery

Goal: when a discovered-model engine (Kimi) is selected in the UI, populate the
real model / thinking / mode lists *before* any agent starts, by probing the CLI
once and caching the result.

### Core

**`core/internal/harness/harness.go`** — add neutral option types + one
interface method:
```go
// DiscoveredOption mirrors one CLI config-option enumeration (the shape the
// init event already carries), for harnesses whose vocabulary is discovered.
type DiscoveredOption struct {
    ID      string                 `json:"id"`
    Name    string                 `json:"name"`
    Options []DiscoveredOptionValue `json:"options"`
}
type DiscoveredOptionValue struct {
    Value string `json:"value"`
    Name  string `json:"name"`
}
```
Add to the `Harness` interface:
```go
    // DiscoverOptions probes the harness's live configuration vocabulary
    // (model / effort / mode enumerations, with display names) without
    // starting a thread. Static-vocabulary harnesses (ModelPicker "tiers")
    // return (nil, nil). Implementations may cache.
    DiscoverOptions() ([]DiscoveredOption, error)
```

**`core/cmd/akcore/harness_claude.go`** — add:
```go
func (h *claudeHarness) DiscoverOptions() ([]harness.DiscoveredOption, error) { return nil, nil }
```

**`core/internal/kimi/` (new file `discover.go`)** — add to `Supervisor` a
cached one-shot probe. Reuse `acpClient` and mirror `Start`'s spawn (pgid,
`s.kimiBin acp`). Do NOT create a full `Thread`.
```go
// DiscoverOptions returns kimi's live config-option vocabulary, cached for the
// process lifetime; a failed probe is retried on the next call.
func (s *Supervisor) DiscoverOptions() ([]ConfigOption, error)
```
Implementation notes:
- `os.MkdirTemp("", "akcore-kimi-probe-")`; `defer os.RemoveAll`.
- `exec.Command(s.kimiBin, "acp")`, `cmd.Dir = tmp`, `SysProcAttr{Setpgid:true}`.
- stdin/stdout/stderr pipes; `go io.Copy(io.Discard, stderr)`.
- `client := newACPClient(stdin, s.log)`; `onNotification`=no-op;
  `onRequest`=`client.respondError(f.ID, codeMethodNotFound, "probe")`;
  `go client.readLoop(stdout)`.
- `ctx` with a 20s timeout. `call("initialize", initParams, nil)` then
  `call("session/new", {cwd:tmp,"mcpServers":[]MCPServer{}}, &res)` where
  `res struct{ ConfigOptions []ConfigOption }`.
- teardown: close stdin, kill the process group (mirror how `reap`/`Stop`
  signals `-pgid`), `cmd.Wait()`.
- cache under a mutex (`discoverMu`, `discovered bool`, `discoveredOpts`,
  reuse on hit). The `initialize` params are identical to `handshake`'s.

**`core/cmd/akcore/harness_kimi.go`** — implement the interface method by
mapping `[]kimi.ConfigOption` → `[]harness.DiscoveredOption`.

**`core/cmd/akcore/handlers.go`** — new RPC (place near `agent.capabilities`):
```go
d.srv.Handle("agent.discoverOptions", func(_ context.Context, raw json.RawMessage) (any, error) {
    var p struct{ Backend string `json:"backend"` }
    if err := json.Unmarshal(raw, &p); err != nil {
        return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
    }
    h, ok := d.harnesses.Get(p.Backend)
    if !ok {
        return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+p.Backend)
    }
    opts, err := h.DiscoverOptions()
    if err != nil {
        return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
    }
    if opts == nil { opts = []harness.DiscoveredOption{} }
    return map[string]any{"configOptions": opts}, nil
})
```

### UI

**`ui/src/state/HarnessTraits.h/.cpp`** — add to `HarnessRegistry`:
```cpp
// Ensure the discovered option enumerations (model/effort/mode) for a
// discovered-model harness are cached locally. No-op for "tiers" harnesses or
// when the cache (Agent/<id>Opt-model) is already populated or a probe is in
// flight. On success persists the same "value|name" entries the init event
// writes, then emits changed() so open pickers rebuild.
void ensureDiscovered(CoreClient *core, const QString &harnessId);
// Persist configOptions (init-event / discoverOptions shape) to KConfig keys.
static void persistDiscoveredOptions(const QString &harnessId, const QJsonArray &configOptions);
```
- `ensureDiscovered`: `traits(id).modelPicker != "discovered"` → return; if
  `optionValues`-equivalent for `optionKey("model")` non-empty → return; guard
  with a `QSet<QString> m_discovering`; `core->call("agent.discoverOptions",
  {backend:id}, cb)`. In cb, remove from in-flight set, call
  `persistDiscoveredOptions(id, result["configOptions"])`, `emit changed()`.
- `persistDiscoveredOptions`: for each option object write KConfig group `Agent`
  key `traits(id).optionKey(opt.id)` = list of `value|name`. This is the same
  loop as `AgentPanel::handleSystemEvent`'s init-event block (factor that block
  to call this helper too, to keep one writer).

**`ui/src/AgentPanel.cpp`** — in the engine-combo `currentIndexChanged` handler
(the one that calls `rebuildModelCombo`), also call
`HarnessRegistry::self()->ensureDiscovered(m_core, selectedHarnessId())`. The
existing `HarnessRegistry::changed` connection already rebuilds the combos.
Also call it once at panel construction if the sticky engine is discovered.

**`ui/src/NewAgentDialog.cpp/.h`** — take a `CoreClient *core` ctor arg (update
the one call site in `AgentDock`). On engine change and at construction call
`HarnessRegistry::self()->ensureDiscovered(core, id)`; connect
`HarnessRegistry::changed` → `rebuildBackendChoices`.

---

## Feature 2 — Engine-aware quick "+ New Agent" menu

Today `ui/src/AgentDock.cpp` seeds `m_roster->setModelChoices({claude tiers})`
and `AgentRoster` emits `newAgentWithModelRequested(project, model)`.

Make the quick menu list **engines**, each with its models:
- Replace `setModelChoices` with `setEngineChoices` taking a structure that
  carries, per entry, `{backend, providerId, model, label}`, grouped by engine
  under section headers or submenus. Build it in `AgentDock` from
  `HarnessRegistry::self()->all()`:
  - tiers harness (Claude): Default / Opus / Sonnet / Haiku / Fable.
  - discovered harness (Kimi): "Kimi Code (default model)" + one entry per
    cached discovered model (from `Agent/kimiOpt-model`; may be empty until a
    probe has run — that is fine, the "New Agent…" dialog remains the full path).
  - provider overlays are out of scope for the quick menu.
- New signal `newAgentWithEngineRequested(project, backend, model)`; connect to
  `addAgent(project, model, backend)` (already exists).
- Keep the value-unchanged rebuild guard.
- Call `AgentDock`'s seeding again on `HarnessRegistry::changed` and after a
  Kimi probe so newly-discovered models appear.

---

## Feature 3 — Kimi session browse

Let the "Resume a Session…" browser list Kimi sessions too.

### Core

**`core/internal/kimi/discover.go`** — add:
```go
type SessionInfo struct {
    SessionID string `json:"sessionId"`
    Cwd       string `json:"cwd"`
    Title     string `json:"title"`
    UpdatedAt string `json:"updatedAt"`
}
// ListSessions runs a throwaway acp handshake and calls session/list (optionally
// filtered by cwd). Same spawn/teardown as DiscoverOptions.
func (s *Supervisor) ListSessions(cwd string) ([]SessionInfo, error)
```

**`core/internal/harness/harness.go`** — neutral browse type + optional method:
```go
type BrowsableSession struct {
    SessionID string `json:"sessionId"`
    Backend   string `json:"backend"`   // owning harness id
    Project   string `json:"project"`   // cwd
    Title     string `json:"title"`
    Updated   string `json:"updated"`   // RFC3339
    Attached  bool   `json:"attached"`  // filled by the handler
}
    // BrowseSessions returns this harness's discoverable past sessions. Only
    // called for harnesses whose Capabilities().SessionBrowse is true.
    BrowseSessions() ([]BrowsableSession, error)
```
- claude adapter: wrap `session.Discover()` into `[]BrowsableSession` with
  `Backend = session.BackendClaude`/its id (currently the handler does this
  inline — move it behind the method so both harnesses look the same).
- kimi adapter: map `ksup.ListSessions("")` → `[]BrowsableSession` with
  `Backend = session.BackendKimi`.
- Set `SessionBrowse: true` in `kimiHarness.Capabilities()` and update
  `core/cmd/akcore/harness_caps_test.go`.

**`core/cmd/akcore/handlers.go`** — rework `session.browse` to merge every
harness with `Capabilities().SessionBrowse`, tag `Attached` from
`d.sessions.List("")`/`GetBySession`, cap the merged list, sort newest-first.
Extend `session.attach` with a `backend` param; create the dormant record with
`Backend: backend` (empty ⇒ claude). Kimi records: `PermissionMode: ""` (its
mode is discovered), `Isolated: false`.

### UI

**`ui/src/SessionBrowserDialog.cpp/.h`** — show a per-row engine badge from the
new `backend` field; pass `backend` to `session.attach`. `loadPreview`/
`session.preview` is Claude-only (reads the Claude transcript) — for a Kimi row
skip the preview and show the row metadata (title / cwd / updated) instead. The
attach flow (`attachRequested` → `AgentDock::attachSession` → resume) already
routes by the record's backend, so no change past the dialog.

---

## Conventions (must hold)

- No `backend == "kimi"`/`== "claude"` compares outside the kimi package or the
  adapters/traits fallback. Everything binds to `HarnessTraits` /
  `Capabilities` / capability booleans.
- Keep `HarnessTraits.cpp` built-in fallbacks in lockstep with the Go adapters.
- A failed probe degrades to today's behaviour (empty/placeholder), never a
  crash or a failed start.
- UI: `QSignalBlocker` around combo repopulation; value-unchanged guards.

## Verify

- `cd core && go build ./... && go test ./...` (esp. `harness_caps_test.go`,
  `kimi` package).
- `cmake --build build` (C++ UI).
- Manual: pick Kimi in Setup → Model lists K2.7 Coding / Highspeed / K3 /
  K3-256k, Thinking low/high/max, When-to-ask default/plan/auto/yolo, with no
  prior Kimi run. Quick "+ New Agent" menu shows both engines. "Resume a
  Session…" lists Kimi sessions and attaches one.
