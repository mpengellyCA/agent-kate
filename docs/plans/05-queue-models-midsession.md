# 05 — Prompt Queue, Send-Now / Mid-Session Context, and Model Switching

Three related capabilities that all hinge on the same fact: stdin to the `claude`
child stays open across turns, so we can write to it any time.

- **(a) Queue a message** while the agent is responding, auto-sent when it finishes.
- **(b) Send now / inject context mid-session** — push input into the live turn
  rather than waiting.
- **(c) Switch models per agent**, including the new **Fable** model.

## Current state

### Send path & stdin lifecycle

- UI: `AgentPanel::onSendClicked` (`AgentPanel.cpp:1194`). Two paths — fresh agent →
  RPC `agent.start` (`:1241-1275`); existing agent → RPC `agent.send` with
  `{threadId, text, attachments}` (`:1277-1280`).
- Core: `agent.send` handler (`main.go:457-466`) → `sup.Send` (`agent.go:264-288`).
  Each turn is one JSON line written to stdin (`agent.go:269-275,284`). **stdin is
  kept open across turns** (opened `:207`, closed only in `Stop` `:300`). Multi-turn
  follow-ups already work this way.
- buildUserContent (`agent.go:79-106`) assembles text + file/image content blocks.

### "Responding" vs "idle" state

- No busy/responding flag in the Go `Thread` struct (`agent.go:108-122`) — only
  `alive`. Turn completion is the stream-json **`result`** event.
- The **UI** already tracks idle: `AgentPanel::m_idle` (`AgentPanel.h:124`), set false
  on send (`:1233`), true on `result` (`:1750`)/resume (`:1769`), false on error/exit
  (`:1786,:1794`). This is where a queue UX naturally hangs.
- **Precedent for queueing already exists**: `AgentPanel::m_permQueue`
  (`AgentPanel.h:164`, used `:1083,:1096,:1795`) defers tool-approval prompts while
  busy. Mirror this pattern for user messages.

### Model selection

- The plumbing is **already present but unused by the UI**:
  - `StartOptions.Model` (`agent.go:71`) → `--model <id>` flag when non-empty
    (`agent.go:183-185`).
  - `agentStartParams.Model` (`main.go:235`), passed through on start (`:1579`) and
    restored on resume (`:1650`); persisted as `session.Record.Model` (`session.go:37`).
  - **No model combo in the UI** — but `AgentPanel` has the exact pattern in
    `m_modeCombo` / `m_isolationCombo` / `m_effortCombo` (`AgentPanel.cpp:429-515`):
    combo → sticky `KConfig` → start param, disabled once the thread runs (`:1075-1077`).
  - Tier→id map lives in `resolveCompactModel` (`main.go:~1847-1865`) and is **stale**:
    `opus→claude-opus-4-7`, `sonnet→claude-sonnet-4-6`, `haiku→claude-haiku-4-5-...`.

## Proposed design

### (c) Model switching — do this first (smallest, unblocks the rest)

1. **Refresh + centralize the model map.** Update `resolveCompactModel`'s table to the
   current generation and add Fable; extract it into one helper (e.g.
   `resolveModel(tier string) string`) used by both spawn and compaction so there's a
   single source of truth. Current IDs to use:
   `opus→claude-opus-4-8`, `sonnet→claude-sonnet-4-6`, `haiku→claude-haiku-4-5`,
   `fable→claude-fable-5`. (Verify exact IDs via the claude-api skill before coding.)
2. **Add `m_modelCombo` to `AgentPanel`** next to the effort/mode combos
   (`AgentPanel.cpp:489-515` pattern): items Opus / Sonnet / Haiku / Fable (+ a
   "default" = leave unset). Sticky via `KConfig`. Pass the chosen tier as
   `model` in `agent.start`.
3. **Mid-session model change.** `--model` is a spawn flag, so changing model on a
   *running* thread means: persist the new `Model` in the session record and apply it
   on the **next resume** (relaunch with the new `--model`). For an immediate switch,
   interrupt+resume (see [04-stop-agent.md]) under the new model. Make the combo
   editable after start, but label it "applies on next turn/resume" so the behavior is
   honest. (If the live stream-json protocol exposes a model-switch control frame,
   prefer that — verify against the CLI.)

### (a) Prompt queue

Track per-agent **responding** state and a FIFO of pending messages.

- **Where:** simplest and lowest-risk in the **UI** (`AgentPanel`), reusing the
  `m_permQueue` pattern — add `QList<QueuedMsg> m_sendQueue`. While `!m_idle`, a normal
  send enqueues instead of calling `agent.send`; on the `result` event (`:1750`) drain
  the queue one message at a time.
- **UI affordance:** a visible queue chip/list under the input ("2 queued") with the
  ability to edit/remove before they fire. Show clearly that they're pending.
- Alternatively track responding state in the **core** `Thread` (set on first
  `assistant` event, clear on `result`) and queue there — more robust if multiple
  clients ever drive one thread, but heavier. **Recommend UI-side for v1**, core-side
  later if needed.

### (b) Send now / mid-session context

- Because stdin is always open, "send now" is literally calling `agent.send` during a
  turn — the question is whether the running `claude` consumes a second user message
  mid-turn or only at the next read boundary. **Spike required:** write a second
  `{"type":"user",...}` line while a turn is streaming and observe whether the current
  CLI injects it into the active turn or buffers it until the turn ends.
  - If it injects → "Send now" is a direct extra `agent.send`; "Queue" is the held
    FIFO above. Two distinct buttons, exactly the user's ask.
  - If it buffers → "Send now" and "Queue" converge to the same effect; surface one
    "send (will be delivered at next turn boundary)" and revisit when the CLI gains
    true mid-turn input.
- Provide both buttons in the UI regardless; wire their behavior to whatever the spike
  proves. Keep the JSON shape identical to `Send()` (`agent.go:269-275`).

## Implementation steps

1. **Model map**: refresh/centralize in `main.go`; confirm IDs via claude-api skill.
2. **Model combo**: add `m_modelCombo` + sticky config + `agent.start` param wiring in
   `AgentPanel`; thread it through `agentStartParams`/`StartOptions` (already exist).
3. **Persist + resume** under the chosen/changed model (already supported; just feed it).
4. **Queue**: add `m_sendQueue` to `AgentPanel`, enqueue-while-busy, drain on `result`;
   queue UI chip with edit/remove.
5. **Send-now / mid-session spike**: test second-message-mid-turn behavior against the
   installed CLI; implement "Send now" vs "Queue" buttons per the result.
6. **Model-switch-mid-session**: persist new model, apply on resume; optionally
   interrupt+resume for immediate effect (depends on [04]).

## Risks / considerations

- **Model IDs drift.** Don't hard-code from memory — resolve via the claude-api skill
  and keep the single `resolveModel` map authoritative (it already feeds compaction;
  divergence would mean compaction and agents disagree on what "opus" means).
- **Mid-turn input is CLI-dependent.** The whole (b) UX branches on a behavior we must
  verify empirically; don't ship "Send now" as truly-immediate until the spike confirms
  it. Stay within the documented stream-json interface ([claude-code-compliant-interface]).
- **Queue + interrupt interplay** ([04]): interrupting should also clear or pause the
  pending queue (don't auto-fire queued prompts into a session the user just halted).
  Define this explicitly — likely: interrupt pauses the queue and asks before draining.
- **Effort/mode/model are start-time today.** Be honest in the UI about what applies
  immediately vs. on next resume.

## Acceptance

- A model combo (incl. Fable) sets the agent's model; new agents launch with `--model`;
  changing it takes effect on the next turn/resume.
- Typing while the agent is responding offers Queue vs Send-now; queued messages fire
  in order when the turn completes; the queue is visible and editable.
