# Agent Kate

![Agent Kate Logo](./AgentKate.png)


A **native multi-agent coding arena**. AgentKate is a fast, non-webview desktop
application that embeds the **Kate editor engine** (`KTextEditor`) and orchestrates
multiple **Claude Code** instances working across one or many projects at once —
with agents and the human cooperating through a shared MCP server.

It is our own *harness and arena* for coding agents. Where others wraps a web GUI
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

Early but Usable. Agent Kate has been used mostly to develop its self and have been successful. It inherits all of Claude Code while offering a more interactive and developer friendly multi-agent environment. 

## Goal

I am building Agent Kate as a KDE Plasma user whom is tired of slow Chromium and Java based IDEs and wanted something that is tailored to Agentic Development on Linux with Claude Code. I am open to adding other harnesses like Codex and OpenCode but I have no need for myself so open a PR. This was by and large coded directly by Claude Opus 4.7. 

I use CachyOS on Arch and Fedora 44 in my testing. Older systems may have dependancy issues and I dont plan to address so again PRs are welcome. 

I intend this project to be a true KDE style project in every way and should feel like a native part of the KDE Plasma Ecosystem. I may make mistakes - Please feel free to correct me. 

## License

Agent Kate is licensed under the **GNU Lesser General Public License, version 2 or
later** (LGPL-2.0-or-later) — the same license as the Kate editor and
`KTextEditor`. See [`COPYING.LIB`](COPYING.LIB) for the full text.
