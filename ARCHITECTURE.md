# Agent Kate Architecture

## Two processes

Agent Kate is split into a native UI and a headless orchestration core. They run as
separate processes and speak a local JSON-RPC bus. The UI spawns and owns the
lifecycle of the core.

```
┌─────────────────────────┐         ┌──────────────────────────────┐
│  agentkate  (C++/Qt6)   │  JSON   │  akcore  (Go)                │
│                         │  -RPC   │                              │
│  • KTextEditor views    │ ◄─────► │  • agent supervisor          │
│  • project tree         │  UDS    │  • git worktree manager      │
│  • agent panels         │         │  • workspace manager         │
│  • LSP result rendering │         │  • LSP host                  │
│                         │         │  • Cooperation MCP server    │
└─────────────────────────┘         └──────────────┬───────────────┘
                                                    │ spawns
                                  ┌─────────────────┼─────────────────┐
                                  ▼                 ▼                 ▼
                            headless `claude`  language servers   MCP clients
                            or `kimi acp`
```

### `agentkate` — C++ / Qt6 / KF6

The UI is Qt Widgets because `KTextEditor` (the Kate engine) is a Qt Widgets
component — there is no QML or web binding. It is intentionally thin: it renders
and handles input, and delegates every subprocess and protocol concern to the core.

- One `KTextEditor::View` per open file. `KSyntaxHighlighting` gives instant native
  highlighting for 300+ languages with no language server.
- LSP results from the core are rendered into the editor: completion via
  `KTextEditor::CodeCompletionModel`, diagnostics via the mark/message interfaces.

### `akcore` — Go

A single uniform supervisor for every child process. Independently runnable
headless, which keeps the harness scriptable.

- **Agent supervisor** — one agent thread per child process, events relayed to
  the UI. Two backends, selectable per thread:
  - *Claude Code* — spawns `claude -p --output-format stream-json
    --input-format stream-json --mcp-config <coop.json>`; relays the stream-json
    events verbatim.
  - *Kimi Code* — spawns `kimi acp` (Agent Client Protocol over stdio) and
    translates ACP session updates into the same Claude-shaped stream-json
    events, so the UI renderer is backend-agnostic. Permissions, interrupts,
    follow-ups, and session resume all map onto ACP methods.
- **Worktree manager** — one `git worktree` + branch per agent thread, for true
  parallel isolation.
- **Workspace manager** — multiple projects open at once; each is a workspace with
  N agent threads.
- **LSP host** — spawns/supervises language servers; normalizes results to the UI.
- **Cooperation MCP server** — an MCP server every agent connects to, sharing live
  workspace state (open files, presence, soft locks, notes).

## IPC bus

Unix domain socket, newline-delimited **JSON-RPC 2.0**, bidirectional. `akcore` is
the server; `agentkate` is the client. The UI sends requests; the core replies and
also pushes notifications (e.g. streamed agent output, file changes).

Frames:
- request: `{"jsonrpc":"2.0","id":1,"method":"…","params":{…}}`
- response: `{"jsonrpc":"2.0","id":1,"result":{…}}` / `…,"error":{…}`
- notification: `{"jsonrpc":"2.0","method":"event.…","params":{…}}`

## Cooperation MCP

Tools exposed to every spawned agent so agents — and the human — stay aware of each
other:

| tool | purpose |
|------|---------|
| `list_open_files` | what is open across the workspace, and by whom |
| `get_presence`    | human + agent cursor/selection positions |
| `claim_file` / `release_file` | advisory soft lock to avoid clobbering |
| `post_note` / `read_notes` | message board between agents and the human |
| `whoami` | the agent's thread / worktree identity |
| `request_review` | flag a change for the human in the UI |
| `list_agents` | every agent thread on record — id, status, worktree, parent/role linkage |
| `launch_agent` | start a real worker thread (any registered backend), parented to the caller; reports applied-truth |
| `send_agent` | follow-up message to another thread (outside the caller's subtree: one human approval) |
| `wait_agent` | block until a thread is idle; returns status + its last reply text |
| `close_agent` | polite stop + archive (reversible) |
| `discard_agent` | destructive removal of a thread and its worktree |

## Repository layout

```
ui/      agentkate — C++/Qt6/KF6 UI
core/    akcore — Go orchestration core
docs/    design notes
scripts/ build & run helpers
```
