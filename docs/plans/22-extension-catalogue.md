# 22 — The extension catalogue: plugins that subsume Skills, granularly and cross-engine

**Status: PLANNED.** Covers IDEAS #3 (plugin manager panel), with the user's
note as the governing constraint. Program context:
[20-approved-features-program.md](20-approved-features-program.md).

**Size: L.**

> **User note:** *"This should encompass the existing Skills functionality and
> it should take a more granular and cross agent compatible approach."*

Three words in that note decide the whole design: **encompass** (one catalogue,
not two), **granular** (the component is the unit, not the package), and
**cross agent compatible** (kimi is a first-class target, not an afterthought).

## Why

AgentKate has a Skills catalogue — `core/internal/skills/skills.go`, a directory
of `SKILL.md` files at `~/.local/share/agentkate/skills`, installed into a
project by symlinking into `.claude/skills/` and `.agents/skills/`
(`skillDirs`, `skills.go:34-37`), surfaced by `ui/src/SkillsDialog.cpp` and six
`skills.*` RPCs (`handlers.go:1640-1742`). It is a good, small thing.

It is also the *smallest* unit in an ecosystem that has moved on. `claude
plugin` is a full package manager, verified against 2.1.220:

```
claude plugin marketplace add|list|remove|update    # marketplaces
claude plugin install|uninstall|update|enable|disable
claude plugin list [--available] [--json]
claude plugin details <name>       # component inventory + projected token cost
claude plugin init|validate|tag|eval|prune
claude plugin install --scope user|project|local --config key=value
```

plus, per session, `--plugin-dir <path>` and `--plugin-url <url>` (both
repeatable) and a `reload_plugins` control request for hot swap. A plugin
carries skills **and** slash commands **and** subagent definitions **and** hooks
**and** MCP servers. The user's whole configured setup travels as plugins; it
does not travel as loose `SKILL.md` files.

So the gap is not "we lack a plugins panel beside the skills panel". The gap is
that our unit of distribution is one component kind out of five, and the user's
note says to fix that by making the model *more* granular, not less: the thing
you install into an agent should be a **component**, and a plugin is the crate
it arrives in.

## Verified facts

| Fact | How verified | Consequence |
|---|---|---|
| `claude plugin list --json` and `list --available --json` exist | `claude plugin list --help` | The catalogue reads structured data, never scraped prose |
| `claude plugin details <name>` reports a component inventory **and projected token cost** | `claude plugin --help` | Token cost is the number that makes granularity worth having — it is what turns "install everything" into a decision |
| Install scope is `user` \| `project` \| `local` | `claude plugin install --help` | AgentKate's per-agent selection is a *fourth* scope and must not be confused with these three (see open question 1) |
| `--plugin-dir` / `--plugin-url` are **per-session, repeatable** | `claude --help` | Per-agent component sets need no global installs — this is the mechanism |
| `reload_plugins` exists as a control_request subtype (8 refs in the 2.1.220 bundle) | Bundle strings; `reload_skills` was probed live and returns the reloaded catalogue | Hot swap without relaunching a thread — the same shape plan 18 built for Cowork |
| **`kimi acp` accepts no options except `--login`** | `kimi acp --help`, run live | Kimi component delivery **cannot** use a flag. `--skills-dir` exists on `kimi` but not on `kimi acp` |
| kimi's config carries `extraSkillDirs` and `mergeAllAvailableSkills` | Config decoder in the 0.30.0 bundle (`nk()`: `mergeAllAvailableSkills: e.merge_all_available_skills, extraSkillDirs: e.extra_skill_dirs`) | The kimi delivery path is a `config.toml` in the thread's `KIMI_CODE_HOME` |
| `StartSpec.Env` already relocates a kimi thread's whole home via `KIMI_CODE_HOME` | `docs/HARNESSES.md`, shipped in plan 16 P6 | The per-thread home already exists. This plan writes one more file into it |
| kimi's ACP handshake advertises `availableCommands` including skill-backed commands, and re-emits `available_commands_update` | Live probe (this session) and `translate.go:200-268` | After a kimi component change we get command-list convergence for free |

## The model

One noun replaces two.

```go
// core/internal/extensions/extensions.go  (was core/internal/skills)

// Kind is one component kind an extension can contain. The vocabulary is
// deliberately the CLIs' own, not an invented abstraction.
const (
    KindSkill   = "skill"   // a SKILL.md capability
    KindCommand = "command" // a slash command
    KindAgent   = "agent"   // a subagent profile
    KindHook    = "hook"    // a lifecycle hook
    KindMCP     = "mcp"     // an MCP server definition
)

// Component is the unit the user selects and an agent receives.
type Component struct {
    ID          string `json:"id"`          // "<bundle>/<kind>/<name>", stable
    Bundle      string `json:"bundle"`      // owning extension; "" for a loose skill
    Kind        string `json:"kind"`
    Name        string `json:"name"`
    Description string `json:"description"`
    Path        string `json:"path"`        // absolute source path
    // TokenCost is the projected context cost, from `claude plugin details`
    // where available and 0 where not. Never estimated by us.
    TokenCost int `json:"tokenCost,omitempty"`
}

// Extension is a bundle of components: a claude plugin, or the degenerate
// one-component case that a loose SKILL.md in our own catalogue is.
type Extension struct {
    Name        string      `json:"name"`
    Version     string      `json:"version,omitempty"`
    Source      string      `json:"source"`      // "catalog" | "marketplace" | "local"
    Marketplace string      `json:"marketplace,omitempty"`
    Enabled     bool        `json:"enabled"`
    Components  []Component `json:"components"`
}
```

**A skill is an extension with one `KindSkill` component.** Every skill in the
existing catalogue keeps working, listed by the same directory scan
(`skills.go:81` `List`), because the migration is a widening of the type, not a
change of the data on disk.

**The capability is a list, not a bool.** In
`core/internal/harness/harness.go`:

```go
// ExtensionKinds names the component kinds this harness can be given at
// launch. Empty = the harness takes no extensions at all. A list rather than
// a bool because the engines differ per KIND, not per feature: claude takes
// all five, kimi takes skills only (probed: `kimi acp` accepts no flags, and
// its only component channel is extraSkillDirs in the thread's KIMI_CODE_HOME).
ExtensionKinds []string `json:"extensionKinds"`
// ExtensionHotReload: components can be added to a RUNNING thread
// (claude reload_plugins). False = the thread must be re-attached, which the
// plan-18 reattach machinery already does.
ExtensionHotReload bool `json:"extensionHotReload"`
```

Claude: `["skill","command","agent","hook","mcp"]`, `ExtensionHotReload: true`.
Kimi: `["skill"]`, `ExtensionHotReload: false`.

This is what makes the picker honest: the four kinds kimi cannot take are
**greyed with a reason**, not hidden — the user learns the difference between
the engines instead of wondering where their hooks went.

## Phase 1 — Rename and widen the catalogue (core, no behaviour change)

Pure refactor, so the risky part lands with nothing depending on it yet.

- `core/internal/skills/` → **`core/internal/extensions/`**. `Skill` becomes a
  `Component` with `Kind: KindSkill`. `Catalog.List` (`skills.go:81`),
  `Get` (`:119`), `ReadContent` (`:148`), `Create` (`:178`), `Install` (`:228`),
  `Uninstall` (`:266`), `ListInstalled` (`:311`) keep their semantics; the
  symlink targets in `skillDirs` (`:34`) become a per-kind table.
- `core/cmd/akcore/handlers.go:1640-1742` — the six `skills.*` handlers stay
  registered and delegate to the new package, so **no UI change is required to
  land this phase**. New `extensions.*` handlers are added beside them.
- `skills.*` is marked deprecated in `docs/HARNESSES.md` and removed one release
  after `SkillsDialog` is replaced (Phase 4).

**Why the rename is worth the churn:** two packages named `skills` and
`plugins` would immediately grow two `Install` functions with different symlink
rules, and the "encompass" instruction would be lost on the first merge.

## Phase 2 — Ingest the plugin ecosystem (core)

New `core/internal/extensions/claudeplugins.go`:

- `ListInstalled()` — `claude plugin list --json`.
- `ListAvailable()` — `claude plugin list --available --json`.
- `Details(name)` — `claude plugin details <name>`, parsed for the component
  inventory and `TokenCost`. **This is the one place that parses CLI prose.**
  Isolate it, test it against a captured fixture, and degrade to
  `TokenCost: 0` on any parse failure — never to a guess. (`parseClaudeModelList`
  at `agent.go:762` is the cautionary tale: a prose parser that became
  load-bearing. Here it must not be.)
- `Marketplaces()` / `AddMarketplace()` / `RemoveMarketplace()` — thin wrappers.
- New RPCs in a new `core/cmd/akcore/extensions.go`: `extensions.list`,
  `extensions.components`, `extensions.install`, `extensions.uninstall`,
  `extensions.enable`, `extensions.disable`, `extensions.marketplaces`,
  `extensions.addMarketplace`.

Every one of these shells out. They are slow (network for marketplaces), so
they run under `safe.Go` with the same not-fatal discipline as
`session.browse` (`handlers.go:540-544`): a failing marketplace is skipped and
named, never fatal to the list.

## Phase 3 — Per-agent component sets (core, both engines)

The heart of the feature. A thread carries a chosen component set; each adapter
delivers it the only way its CLI can.

**`session.Record`** gains, persisted **as requested** (there is nothing to
negotiate — a component either reached the CLI or was reported unapplied):

```go
// Components are the extension components this thread was launched with,
// as component IDs. Persisted because a resume without them would hand the
// thread a different capability set than the conversation was built on.
Components []string `json:"components,omitempty"`
```

**`harness.StartSpec`** gains `Components []extensions.Component`, gated by
`Capabilities.ExtensionKinds` and reported per-kind in
`Launched.UnappliedOptions` — the established pattern (`harness.go:256-264`).

**Claude delivery** (`core/cmd/akcore/harness_claude.go`, `Launch`):

- Materialise the selected components into a per-thread staging directory
  (beside the MCP config `writeMCPConfig` already writes), shaped as a
  synthetic plugin: `plugin.json` + `skills/` + `commands/` + `agents/` +
  `hooks/`. Pass `--plugin-dir <staging>` in `buildStartArgs`
  (`core/internal/agent/agent.go:134`), next to the existing `--agents` /
  `--mcp-config` block.
- `KindMCP` components merge into the `--mcp-config` payload
  `writeMCPConfig` already builds, so `mcp_servers` in the init event reports
  the truth (this composes with `--strict-mcp-config`, landing now).
- `KindHook` components are **deferred to plan 24 §4–6**, which owns the
  `--settings` trust decision. Until then, hook components are listed and
  reported unapplied with that reason.
- Argv safety: the staging dir is a *path*, so `maxArgBytes`
  (`harness_claude.go:105`, the MAX_ARG_STRLEN trap) is not a risk here —
  unlike `--agents`, which inlines JSON. Note that in the code so nobody
  "simplifies" it back to an inline payload.

**Kimi delivery** (`core/cmd/akcore/harness_kimi.go`, `Launch`):

- `kimi acp` takes no flags, so delivery is through the thread's
  `KIMI_CODE_HOME` (already carried on `StartSpec.Env`). Write
  `<home>/config.toml` with `extra_skill_dirs = [<staging>/skills]`.
- Only `KindSkill` survives; the other four are reported unapplied with
  engine-specific reasons alongside the existing `kimiNo*` constants
  (`harness_kimi.go:93-109`) — the same discipline, so a downgrade reads
  identically whatever caused it.
- **A thread with no `KIMI_CODE_HOME` cannot take components at all** — its home
  is the user's real one and writing a `config.toml` there would change the
  user's own CLI. If components are requested for such a thread, either mint a
  home (a real behaviour change; see open question 2) or report unapplied. The
  plan's position: **report unapplied and say so**, and let the New Agent dialog
  offer "give this agent its own kimi home" as an explicit choice.

## Phase 4 — Hot reload, and replacing the Skills dialog

**Hot reload (claude).** After `extensions.install` or a per-agent component
change, fan out a `reload_plugins` control_request to every live thread whose
`WorkDir` is under the target — using the shared `control()` helper landing with
the current work, and using the **returned** catalogue to confirm the component
registered rather than assuming it (the applied-truth discipline
`Launched.UnappliedOptions` established). Pairs with `commands_changed`: a new
command appearing in the composer's autocomplete is the same user-visible moment.

**Hot reload (kimi).** `ExtensionHotReload: false`. Changing a running kimi
thread's components re-attaches it through `reattachForCowork`'s machinery
(`core/cmd/akcore/cowork_enable.go`) — generalise that function into
`reattachThread(threadID, reason)` and have both callers use it. It already
does the right things: wait for the current turn (`TurnTracker.Wait` — never
throw away a turn in progress), stop, wait for the reap, resume the same
session. Report `applied: "reattach"` exactly as Cowork does.

**UI.** New `ui/src/ExtensionsPanel.{h,cpp}` — a panel, not a dialog, because
it is now a place you work rather than a thing you open:

- Left: extensions tree (Marketplaces → available; Installed; Catalogue — our
  own loose skills). Right: the selected extension's **components**, each with
  kind icon, description and token cost, each independently checkable.
- A per-agent "Components" section in `ui/src/NewAgentDialog.cpp`, using the
  existing `setRowVisible` capability gating (`NewAgentDialog.cpp:184-186`) and
  a `FlowLayout` chip row for the selection (house rule for chip rows).
- Component chips greyed with a tooltip reason when the selected engine's
  `extensionKinds` excludes them — read from `HarnessTraits`
  (`ui/src/state/HarnessTraits.h`), never from a backend string compare.
- A running agent's panel gets "Components…" in its menu, which applies live on
  claude and offers the re-attach on kimi, with the plan-18 wording that
  distinguishes *live* from *re-attach* from *next start*.
- **`ui/src/SkillsDialog.{h,cpp}` is deleted**, and
  `MainWindow.cpp:1154`'s action re-points at the panel. `ExtensionsDialog`
  (VS Code extensions, `core/internal/vsix`) is a **different feature** and is
  not touched — but the two names will confuse people, so rename that one to
  `EditorExtensionsDialog` in the same commit.
- Every new action registers with the `KActionCollection` from plan 27 §1.

## Verify

| Phase | What proves it |
|---|---|
| 1 | `go test ./internal/extensions/...` — the existing `skills_test.go` cases (`TestListAndInstall`, `TestUninstallLeavesForeignLinksAlone`, `TestUninstallRefusesNonSymlink`, `TestValidateName`, `TestCreateAndRead`) all pass unchanged against the renamed package. That is the whole point of doing the rename as a no-behaviour-change phase. |
| 2 | `TestParsePluginDetails` against a captured `claude plugin details` fixture, plus a **malformed** fixture asserting `TokenCost == 0` and no error — the parser must never become load-bearing. |
| 2 | Manual: `extensions.list` over the socket returns the same plugins `claude plugin list --json` prints. |
| 3 | `TestLaunchStagesComponentsForClaude` — a spec with one skill, one command and one hook produces a staging dir with the right layout, a `--plugin-dir` argv entry, and one `UnappliedOption` for the hook. Extend `core/internal/agent/startargs_test.go`, which already tests exactly this class of flag plumbing. |
| 3 | `TestLaunchStagesSkillsForKimi` — the same spec on kimi produces `extra_skill_dirs` in the thread home's `config.toml` and **four** `UnappliedOption`s, one per unsupported kind. And `TestKimiComponentsWithoutHomeAreUnapplied`. |
| 4 | Live smoke `scripts/smoke-extensions-hotswap.py`, in the shape of `scripts/smoke-cowork-enable.py`: claude thread running → add a skill component → `reload_plugins` acked → the skill is callable **on the very next turn** (not a turn later — that race is what plan 18 §"Wait for the re-list" was written about). |
| 4 | Live smoke kimi: component change → `applied: "reattach"` → a codeword set before the change is still known after → the new skill's command appears in `available_commands_update`. |
| 4 | Qt test `ui/tests/ExtensionsPanelTest.cpp`: with kimi traits, the four non-skill kinds render disabled with a non-empty tooltip; with claude traits, all five are enabled. |

## Non-goals

- **Being a plugin author's tool.** `claude plugin init`, `validate`, `eval` and
  `tag` are development commands. AgentKate consumes plugins; it does not
  scaffold or score them. (`eval` is genuinely interesting and belongs in a
  future plan, not this one.)
- **Our own marketplace.** We surface the marketplaces `claude plugin
  marketplace` knows about. We do not host, mirror or curate.
- **Emulating unsupported kinds on kimi.** No writing subagent `.md` files into
  a kimi worktree in the hope the v2 engine reads them — plan 16 P3 already
  proved ACP never runs v2 (`harness_kimi.go:70-88`), and dead files in a user's
  tree are worse than an honest downgrade.
- **Hooks.** Component kind `hook` is defined here and delivered by
  [plan 24](24-agent-questions-and-hooks.md), which owns the trust question.

## Open questions for the user

1. **Scope collision (program open question 5).** ~~Should choosing components
   for an agent *also* install them at project scope, so your own terminal
   sessions in that repo get them too?~~
   **RESOLVED 2026-08-01: purely session-scoped.** Per-agent component sets are
   a temporary, thread-scoped concept (`--plugin-dir` / per-thread home
   materialisation only) — AgentKate keeps absolute control and **never mutates
   the user's or the project's CLI config** from a picker action. Anyone who
   wants a component permanently installs it with the CLI themselves.
2. **Minting a kimi home.** A kimi thread only takes components if it has its
   own `KIMI_CODE_HOME`. Should selecting components for a kimi agent silently
   give it one — which also un-authenticates it until `kimi login` runs in that
   home (documented in `docs/HARNESSES.md`) — or refuse and make the user opt in?
3. **Token cost as a budget.** `claude plugin details` gives a projected token
   cost per component. Should the picker enforce a per-agent ceiling (refuse a
   selection over N tokens), warn, or only display?
