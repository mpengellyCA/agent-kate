# AgentKate

A **native multi-agent coding arena**. AgentKate is a fast, non-webview desktop
application that embeds the **Kate editor engine** (`KTextEditor`) and orchestrates
multiple **Claude Code** instances working across one or many projects at once —
with agents and the human cooperating through a shared MCP server.

It is our own *harness and arena* for coding agents. Where t3code wraps a web GUI
and puts each agent thread on a bare git branch, AgentKate renders natively and
isolates every agent in its own **git worktree** for true parallelism.

## Architecture

Two processes, one repo, connected by a local JSON-RPC bus:

- **`agentkate`** — C++/Qt6/KF6 UI. Embeds `KTextEditor::View` per file (instant
  native syntax highlighting), project tree, agent panels. Pure rendering + input.
- **`akcore`** — Go orchestration core. Supervises every subprocess: headless
  `claude` agents, language servers, and the Cooperation MCP server. Manages git
  worktrees and workspaces. Runnable headless.

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for detail.

## Build

Prerequisites (Arch): `qt6-base`, `ktexteditor`, `syntax-highlighting`,
`extra-cmake-modules`, `cmake`, `ninja`, `go`, plus the `claude` CLI.

```sh
scripts/build.sh      # configures + builds both agentkate and akcore
scripts/run.sh        # launches the app (the UI spawns akcore itself)
```

## Status

Early. Milestones M0 (scaffold + build + IPC handshake) and M1 (walking skeleton:
editor + one live agent in a worktree + Cooperation MCP) are the current focus.
The goal of M1 is **self-hosting** — developing AgentKate from inside AgentKate.
