# 06 — Fix Auto-Compaction on Exit

## Symptom

Compaction works on **restore/resume** but is unreliable **on exit** — the program
doesn't wait for it, so the summary is often lost. (You suspected exactly this.)

## How compaction is structured

`core/internal/compact/`:

- **Strategies** (`compact.go:27-51`): `ExitOpusHot` (compact the live, cache-warm
  thread *before* it's reaped), `ExitSonnetCold` (spawn a fresh `claude --resume`
  *after* the thread exits), `Resume{Haiku,Sonnet,Opus}Cold` (defer to next resume),
  `ResumeLocal` (free programmatic strip).
- **Programmatic** (`programmatic.go`): deterministic, free, strips `tool_result`
  bodies but keeps decisions. Used as the no-quota fallback.
- **LLM** (`llm.go:42-90`): spawns a cold `claude --resume` to produce a dense summary.
- **Storage** (`storage.go:51-64`): atomic temp-file + `os.Rename`, one JSON per thread
  under `~/.local/share/agentkate/summaries/{threadId}.json`. **No corruption risk** —
  a half-write never replaces a good file. The failure mode is a *missing* summary,
  not a corrupt one.

## Root-cause diagnosis (the exit race)

Two exit-compaction paths, only one of which is awaited:

### Hot path — correct

At shutdown, `runCore` calls `runHotCompactsAtShutdown(deps)` **before**
`sup.StopAll()` (`main.go:214-215`). That helper uses a `sync.WaitGroup`, so hot
compactions complete before exit. Also the interactive `agent.stop` handler runs hot
compaction synchronously before stopping (`main.go:476`). **These work.**

### Cold path — broken (fire-and-forget)

Cold-exit strategies are launched from the lifecycle handler:

```go
// main.go:162-173
if probe.Type == "_lifecycle" && probe.Phase == "exited" {
    ...
    if strat.RunsOnExit() && strat != compact.ExitOpusHot {
        go runExitCompact(log, sessions, summaries, rec, strat)   // main.go:170
    }
}
```

The sequence at shutdown is:

1. `srv.Serve(ctx)` returns (UI gone / signal) — `main.go:210`.
2. `runHotCompactsAtShutdown(deps)` waits for **hot** compactions only — `:214`.
3. `sup.StopAll()` closes stdin on every thread and schedules a 5s backstop kill
   (`agent.go:318-327` → `Stop` `:292-316`) — `:215`. It does **not** wait for
   `reap()` to finish.
4. `runCore` returns → `main()` returns → **process exits**.
5. Meanwhile each thread's `reap()` (`agent.go:458-486`) eventually emits the
   `_lifecycle/exited` event, which spawns `go runExitCompact(...)`. These goroutines
   are **never tracked and never awaited** — and they're spawned *after* StopAll,
   right as the process is leaving.

So cold-exit compactions are racing a process exit they will almost always lose.
Secondary issue: `runExitCompact` runs under `context.Background()` (its own 5-min
timeout) rather than a shutdown-aware context, but that's moot when the OS reaps the
whole process first.

## Fix — two options

### Option A (recommended): track and await cold compactions in-process

Make the cold-exit goroutines first-class so shutdown blocks on them.

1. **A tracked group for exit work.** Add a package-level / `deps`-held
   `sync.WaitGroup` (e.g. `exitCompactWG`). In the lifecycle handler, `wg.Add(1)`
   before `go runExitCompact(...)` and `defer wg.Done()` inside it (replacing the bare
   `go` at `main.go:170`).
2. **Drain threads, then their compactions, before exit.** Reorder the shutdown tail:
   - `runHotCompactsAtShutdown(deps)` (unchanged).
   - `sup.StopAll()` — but make StopAll/`reap` **synchronously deliver** the
     `_lifecycle/exited` events for every thread (or have StopAll return once all
     `reap()`s have run), so the cold-exit goroutines are all *spawned* before we wait.
   - `exitCompactWG.Wait()` with a sane overall deadline (e.g. cap total exit-compaction
     time so a hung `claude --resume` can't wedge shutdown forever — log + proceed on
     timeout).
   - then return / `os.Exit`.
3. **Shutdown-aware context.** Give `runExitCompact` a context derived from a fresh
   `context.WithTimeout` at shutdown (not `Background`) so the deadline is honored and
   logged, and so a global shutdown cap can cancel stragglers.
4. **UI side — don't kill the core too early.** The UI owns the core's lifecycle
   (ARCHITECTURE.md). On app quit, the UI currently disconnects → `OnAllClientsGone`
   → `stop()` (`main.go:205-208`). Ensure the UI **waits for the core process to
   actually exit** (not just sends the disconnect) before the UI process terminates,
   otherwise the OS may tear down the core mid-drain. Verify/extend the UI's core
   shutdown join (look at where `agentkate` reaps the core child on quit).

This keeps everything in one process, is the smallest correct change, and reuses the
WaitGroup pattern already proven by the hot path.

### Option B (your idea): hand off to a detached finisher process

If we want compaction to survive even a hard UI/core teardown, spawn a **slim detached
helper** (`akcore --finish-compactions <threadIds...>` or a tiny dedicated binary)
that:

- is started with `Setsid`/its own session so it is **not** killed when the parent
  core exits;
- reads the same session store + summary store, runs the pending cold compactions to
  completion, writes them atomically (storage is already safe for this), and exits.

`runCore` then returns immediately after launching the finisher, and the heavy
`claude --resume` work outlives the UI/core.

**Trade-off:** more moving parts (a second entrypoint, dedup so the finisher and a
later interactive resume don't both compact the same thread, lockfile on the summary
file). Use this only if Option A's "block briefly on exit" is too slow at quit time.

**Recommendation:** Ship **Option A** first (correctness with minimal surface). Keep
**Option B** as the escalation if exit latency from waiting on `claude --resume`
proves annoying — at which point a detached finisher is the right call.

## Implementation steps (Option A)

1. Add `exitCompactWG sync.WaitGroup` to `handlerDeps` (or a struct the lifecycle
   closure captures).
2. Wrap the `go runExitCompact(...)` at `main.go:170` with `Add(1)` / `defer Done()`.
3. Make `sup.StopAll()` (`agent.go:318`) wait for every `reap()` to complete (so all
   exit lifecycle events — and thus all `runExitCompact` spawns — have happened) before
   it returns; e.g. StopAll joins on each thread's reap-done.
4. After `StopAll()` in `runCore` (`main.go:215`), add
   `waitWithDeadline(exitCompactWG, <cap>)` then proceed.
5. Switch `runExitCompact`'s context from `Background` to a shutdown-scoped
   `WithTimeout`.
6. Confirm the UI joins on the core child exit at quit.
7. Add/extend a test alongside `compact_test.go` simulating "thread exits, then
   shutdown" and asserting the summary file exists afterward.

## Risks / considerations

- **Quit latency.** Waiting on a cold `claude --resume` at exit can take seconds.
  Bound it (per-thread + overall caps), log what was skipped on timeout, and consider
  Option B if users notice a slow quit. Per [self-hosted-dogfooding], a sluggish quit
  while dogfooding will be obvious — make the cap conservative.
- **Double-compaction.** A thread compacted at exit shouldn't be re-compacted on the
  next resume. The `LastTurnAt` vs summary-timestamp staleness check (`main.go:149-156`,
  storage) should already gate this — verify it holds when the exit compaction lands.
- **Don't touch the hot path.** It works; scope the change to the cold path + shutdown
  ordering.
- **Atomic storage is fine as-is** (`storage.go:51-64`) — no change needed there.

## Acceptance

Configure a thread for a cold-exit strategy (`ExitSonnetCold`), use it, quit the app →
on next launch the thread resumes from a freshly written summary (no "missing summary"
recovery, no replay of the whole transcript). A test reproduces the exit→summary-exists
guarantee.
