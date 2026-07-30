# Agent Kate — Improvement Plans

A planning set for the next round of Agent Kate improvements, written after extended
daily dogfooding. Each item has its own grounded plan file with the relevant
`file:line` references, a proposed design, and step-by-step implementation notes.

## Index

| # | Improvement | Area | Plan | Rough size |
|---|-------------|------|------|-----------|
| 1 | Agent list → visual card list | UI (C++) | [01-agent-list-cards.md](01-agent-list-cards.md) | M |
| 2 | Markdown preview / raw / split toggle | Editor (C++) | [02-markdown-preview.md](02-markdown-preview.md) | M |
| 3 | Wire the toolbar Search box | UI (C++) | [03-toolbar-search.md](03-toolbar-search.md) | S |
| 4 | True stop / interrupt mid-response | Core (Go) + UI | [04-stop-agent.md](04-stop-agent.md) | M |
| 5 | Prompt queue, send-now, model switching (incl. Fable) | Core (Go) + UI | [05-queue-models-midsession.md](05-queue-models-midsession.md) | L |
| 6 | Fix auto-compaction on exit | Core (Go) | [06-compaction-shutdown.md](06-compaction-shutdown.md) | M |
| 7 | Document & media viewing (PDF/CSV/Office/AV) | Editor (C++) | [07-document-media-viewing.md](07-document-media-viewing.md) | M–L |
| 8 | KDE Plasma Cowork (share/see/control the desktop, consent-gated) | Core (Go) + UI (C++) | [08-kde-cowork/](08-kde-cowork/README.md) | L (phased v1/v2/v3) |
| 10 | Panel responsiveness & resize performance | UI (C++) | [10-panel-responsiveness.md](10-panel-responsiveness.md) | M (Phase 2: chat virtualization) |
| 11 | Third-party API providers (Fireworks, OpenRouter) via the Anthropic endpoint | Core (Go) + UI (C++) | [11-third-party-providers.md](11-third-party-providers.md) | M (phased core/persist/UI) |
| 12 | UI identity & approachability (signature theme, theme override, command palette, Simple/Advanced modes, layouts, panel responsiveness) | UI (C++) | [12-ui-identity-and-approachability.md](12-ui-identity-and-approachability.md) | L (phases 1–3 done; 4–8 planned) |
| 13 | Agent experience overhaul (chat selection/copy, interrupt vs stop, save+autosave, attachments, tool inspector, fork agent, roster cards v2, Git Log v2, Worktrees v2, Cowork control centre, file-browser worktree tabs) | Core (Go) + UI (C++) | [13-agent-experience-overhaul.md](13-agent-experience-overhaul.md) | XL (11 phases, one major release) |
| 14 | Harness abstraction & feature parity (capability registry, Engine picker, thinking/plan/subagent/background-shell rendering, mid-session control, context-fill & token observability) | Core (Go) + UI (C++) | [14-harness-abstraction-and-parity.md](14-harness-abstraction-and-parity.md) | XL (7 phases) |
| 15 | Kimi parity finish (option discovery, engine-aware new-agent quick menu, session browse) | Core (Go) + UI (C++) | [15-kimi-parity-finish.md](15-kimi-parity-finish.md) | M |

Size key: **S** ≈ <½ day, **M** ≈ 1–2 days, **L** ≈ 3–5 days.

## Architecture recap (why each item lands where it does)

Agent Kate is two processes (see [ARCHITECTURE.md](../../ARCHITECTURE.md)):

- **`agentkate`** — C++/Qt6/KF6 UI. Thin: renders, handles input, owns the core's
  lifecycle. Items 1, 2, 3 are mostly here; 4 and 5 have UI halves.
- **`akcore`** — Go orchestration core. Spawns one headless
  `claude -p --output-format stream-json --input-format stream-json` per agent
  thread, manages worktrees, sessions, search, LSP, and the Cooperation MCP.
  Items 4, 5, 6 are mostly here.

They speak newline-delimited JSON-RPC 2.0 over a Unix domain socket. Adding a feature
that spans both processes means: a new/changed RPC method registered in
`core/cmd/akcore/main.go`, a `CoreClient::call(...)` from the UI, and the core-side
handler delegating to `core/internal/...`.

## Suggested sequencing

1. **#3 toolbar search** and **#4 stop** first — small, high daily value, low risk.
   #3 is nearly self-contained; #4 fixes a credit-wasting / safety gap.
2. **#6 compaction** next — it's a correctness bug with a clear diagnosis; fixing it
   protects every long session.
3. **#5 queue / models / mid-session** — largest, but #4 (interrupt) is a natural
   prerequisite for a good "send now vs. queue" UX, so do it after #4.
4. **#1 agent cards**, **#2 markdown preview**, and **#7 document/media viewing** —
   UI-only, no protocol changes; parallelizable, slot in whenever. #2 and #7 share the
   `EditorArea` dispatch rework, so do them together (one ordered file-type resolver).

Items 4–6 touch the agent supervisor and shutdown path in the same files
(`core/internal/agent/agent.go`, `core/cmd/akcore/main.go`); doing them in a
deliberate order avoids merge churn.

## Cross-cutting notes

- **No new heavy dependencies** are required for any item. Markdown preview (#2) uses
  `QTextDocument::setMarkdown` + `QTextBrowser`, both already linked; there is **no**
  QtWebEngine in the build and none is needed.
- **Model IDs** (#5): the current tier→id map in `core/cmd/akcore/main.go`
  (`resolveCompactModel`, ~line 1847) is the single source of truth and is stale
  (`claude-opus-4-7`). Refresh it to the current generation (Opus 4.8, Sonnet 4.6,
  Haiku 4.5) and add Fable as part of #5.
- **Stop + queue + compaction** all hinge on one fact: stdin to the `claude` child is
  kept open across turns, and there is no in-band cancel message — see #4.

## Note on #2 + #7 (shared editor dispatch)

Both add new non-text viewers hosted as editor tabs. They should share **one ordered
file-type resolver** in `EditorArea::openFile` (markdown → csv → image → generic
KPart → media → text) rather than each bolting on its own `if`. #7 also narrows
`ImageView::canDisplay` so PDFs stop being hijacked into a static first-page image and
route to the Okular KPart instead. Plan them as a single editor-dispatch refactor.
