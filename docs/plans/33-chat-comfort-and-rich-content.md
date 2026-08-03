# 33 — Chat Comfort, Rhythm & Rich Content

> **Status: proposed — pending approval.** UI-only, staged around the existing
> virtualized transcript. No core/RPC or transcript-format change is required.

## Goal

Make the Agent Chat Interface feel like a pleasant, modern conversation rather
than a dense event inspector: easier to read for a long time, calmer to scroll,
and better at incorporating tools, attachments, links, plans, images and other
agent activity inline. The reference is the *interaction quality* of Slack and
Discord—clear speaker hierarchy, compact activity, useful rich objects—not a
pixel-for-pixel imitation.

Performance remains a release gate. The answer is **not** to bring back a
`QScrollArea` full of live message widgets. Keep the `TranscriptModel` →
`QListView` → `TranscriptDelegate` architecture, its bounded caches, its
visible-row resize settle pass, and its bounded 5,000-row in-memory window.
This plan changes the semantic data, layout metrics, palette tokens and painting
of those rows.

## What the current implementation gets right—and where it creates friction

The performance refactor in `ab4cab7` was justified: the prior widget feed
relayouted every message on each resize; the current view draws only what it
needs and remeasures only visible rows after the resize settles. Since then,
streaming, image thumbnail caching, selection overlays, tool inspection and
accessibility have made the feed substantially more capable.

The current surface still reads as one uniform stack of full-width boxed rows:

- Every user and assistant message uses the same `AlternateBase` card surface;
  the role's coloured label is the only meaningful distinction
  (`ui/src/TranscriptDelegate.cpp:638-748`). This gives the human's messages no
  chat-bubble ownership and makes a long turn visually monotone.
- The fixed 4 px outer margin, 8 px row gap, 9/11 px message padding and repeated
  role/timestamp row create a rigid beat rather than a conversational rhythm
  (`TranscriptDelegate.cpp:35-55, 656-692`).
- Markdown is converted safely once, but then receives only the application
  default document styling (`AgentChatHelpers.cpp:191-204`,
  `TranscriptDelegate.cpp:219-230`). Paragraphs, lists, quotes, inline code and
  code blocks therefore do not have a deliberately readable transcript type
  system.
- Tool calls contain good information, but each is a bordered mini-card with
  raw-oriented symbols and persistent copy/inspect glyphs
  (`TranscriptDelegate.cpp:862-1069`). They compete with conversation instead
  of reading as compact activity attached to it.
- Attachments are durable, clickable and cached correctly, but are small
  filename chips (`TranscriptDelegate.cpp:241-278, 700-745`) rather than rich,
  scan-friendly objects.
- The composer is a fixed 94 px editor followed by a separate action flow
  (`AgentPanel.cpp:782-809, 1309-1346`), which feels more like a form than the
  natural end of a conversation.

The design must preserve existing strengths: sticky-bottom plus unread state,
in-conversation find, per-message selection/copy, external-link policy,
attachments opening in the editor/preview, tool inspector, collapsed reasoning,
checklists, permission questions, streamed text, replay, keyboard access, and
all engine-neutral event rendering.

## Experience decisions

1. **Comfortable is the default.** It provides noticeably more breathing room
   and readable prose than today's card stack. Users may opt into **Compact**
   or **Spacious** density; density changes spacing and type metrics, never hides
   information.
2. **Messages establish the conversation; events support it.** Human and agent
   messages get distinct, asymmetric bubbles. Tools, thinking and ordinary
   lifecycle notes become a quieter activity stream. Errors, permission prompts,
   questions and the active plan retain strong, distinct treatment because they
   require action or attention.
3. **Rich content belongs in the flow.** Attachments, screenshots, links,
   code, tool output and plan updates remain directly reachable at their point
   in the transcript. Details remain on demand, without making every scroll
   position look like an IDE panel.
4. **Use semantic theme tokens, not fixed colours or global QSS.** Built-in
   Midnight/Daylight and system/KDE schemes must all preserve text and control
   contrast. The transcript may use a local `QTextDocument` stylesheet for
   markdown typography, but application chrome stays palette-driven.
5. **No performance trade for polish.** No per-row widgets, no markdown
   conversion in `paint()`, no filesystem stat/image decode in the scroll path,
   and no full-model remeasure when width, density or theme changes.

## Target conversation language

| Transcript element | Target presentation | Existing interaction retained |
|---|---|---|
| Your message | Right-aligned, tinted bubble (up to 82% of the usable width); compact attachment tiles beneath its text. | Text selection, named attachments, file/image open, copy menu, replay. |
| Agent message | Left-aligned, calm raised surface with a readable maximum line length (up to 820 logical px; full usable width in a narrow pane). Identity appears at the start of a run, not as repeated visual chrome. | Markdown, links, selection overlay, code-block copy, streaming and roster preview. |
| Consecutive messages | Adjacent messages from the same speaker share a visual run: the first shows identity/time, continuations have a reduced gap and no redundant header. A tool, note, thinking row or speaker change ends a run. | One model row per message; individual copy/selection/replay semantics stay unchanged. |
| Tool activity | Compact left-rail event with an icon, plain-language action/summary and clear running/success/error state. It opens its familiar disclosure or inspector for input and output. Consecutive activity rows visually read as one run, without being merged in the model. | Tool visibility preference, partial output, expand, full-output link, copy, inspector and image result chips. |
| Thinking and ordinary notes | Quiet, borderless disclosure/divider; system timing and status do not masquerade as chat messages. Errors remain high-contrast and copyable. | Expand/copy, timestamps, find and accessibility text. |
| Checklist, permission and question | Purposeful cards with clear state, adequate line spacing and action focus. They remain visually stronger than passive activity. | In-place checklist updates; all permission/question actions and timeout handling. |
| Attachments and tool images | Typed, clickable mini-objects: thumbnail where available, otherwise a semantic file icon, filename and concise type/size metadata. | Current durable cache/origin fallback and editor/image-preview routing. |
| Composer | A rounded, auto-growing message surface with attachment state and primary send/interrupt action visually attached to it. Secondary agent controls remain available but no longer compete with composing. | Draft persistence, image paste/drop, slash completion, queueing, Enter preference, send and interrupt. |

### Layout and type scale

- Add a `ChatAppearance` settings object, persisted in the existing `[Appearance]`
  configuration group. It supplies `density` (`compact`, `comfortable`,
  `spacious`) and `textScale` (`-1`, `0`, `+1` relative to the application font).
  `comfortable` / `0` is the default. No migration is needed: absent settings
  resolve to those values.
- Centralize all delegate geometry in a small `TranscriptMetrics` value generated
  from `ChatAppearance`, the viewport/device scale and the active palette. It
  owns outer insets, message width caps, grouping gaps, card radius, activity
  rail widths, attachment tile sizes and the body/meta/code fonts. `sizeHint`,
  `paint`, attachment hit-testing and the selection overlay must consume this
  same value—never parallel constants.
- Body prose is one step larger at the default scale, with relaxed line height
  and an 8 px paragraph/list rhythm. Metadata is deliberately smaller but meets
  contrast requirements. Heading, quote, list, inline-code, preformatted-code,
  table and link treatment is supplied by a Qt-supported document stylesheet
  installed before the cached body document receives its HTML.
- Code blocks use a distinct low-glare surface, padding and a monospace font at
  the body size (not the current 90% shrink). They remain wrapped, as they are
  today, so a narrow chat pane cannot acquire a horizontal scroll trap.
- Assistant prose width is capped for readable scanning while user bubbles are
  narrower and right-aligned. At widths below either cap plus side insets both
  use the full available width; the conversation remains usable in the narrow
  layouts enabled by plan 10.

### Contrast tokens

Extend `AkColors` with transcript-specific semantic colours, resolved for both
built-in and system/KDE themes:

- `chatAssistantSurface`, `chatUserSurface`, `chatActivitySurface`,
  `chatCodeSurface`, `chatBorder`, `chatRail`, `chatMetadata`, and
  `chatAttachmentSurface`;
- status colours continue to come from `positive`, `negative`, `neutral` and
  `info`, rather than introducing meaning by hue alone;
- a small colour-contrast helper test verifies normal body and metadata text
  against every surface that carries it (4.5:1 for normal text; 3:1 minimum
  for icons, rails and borders). Built-in themes use explicit reviewed values;
  system schemes derive surfaces by blending their supplied palette rather than
  assuming dark or light hex values.

The existing delegate already watches `ApplicationPaletteChange` and keys body
documents by palette generation (`TranscriptDelegate.h:125-174`). Settings and
appearance changes will extend that invalidation key, so cached documents never
retain an earlier theme or density's fonts/colours.

## Implementation plan

### Phase 1 — Appearance foundation and review harness — **M**

1. Add `ui/src/state/ChatAppearance.{h,cpp}` as the single source for density,
   text scale, derived `TranscriptMetrics`, settings persistence and a `changed()`
   signal. It must be cheap to query in delegate paths; configuration reads occur
   only at load/update, not per row.
2. Add the transcript semantic tokens to `ui/src/theme/Theme.h` and resolve them
   in `ThemeManager.cpp` for Midnight, Daylight, Follow System and discovered
   KDE schemes. Keep `ThemeManager` as the only appearance owner.
3. Extend `AppearanceDialog` with a compact **Chat readability** section:
   density choices with a one-line outcome, and `Smaller / Default / Larger`
   transcript text. Preview changes live, apply/cancel exactly as the theme
   dialog already does. Do not add a second settings window.
4. Add an `akwidgets-preview transcript [theme] [density]` fixture mode. Host a
   real `AgentPanel` and drive its public notification path with safe fixture
   events, so both the virtual feed and its surrounding composer/permission
   widgets are genuine. Cover short and long agent/user messages, markdown
   (heading/list/quote/code/link/table), a queued user run, success/running/error
   tools, a screenshot and text attachment, thinking, a checklist, a permission
   request and an error note. This is the visual review surface for each phase;
   it does not replace real-session smoke testing.
5. Add unit tests for default/migrated settings, live setting notification and
   built-in-theme contrast. A density/type change must invalidate an already
   cached transcript document while preserving visible-row-only synchronous
   remeasurement.

### Phase 2 — Semantic transcript model and chat-native message geometry — **L**

1. Replace the presentation-critical string/hex convention in
   `TranscriptModel::appendMessage` with a semantic `Speaker` (`User`, `Agent`)
   and a `MessageRunPosition` role (`Single`, `First`, `Middle`, `Last`). Keep
   display labels, raw markdown, timestamp and accessible text as they are.
   `AgentPanel::addMessageCard` passes the semantic speaker directly rather than
   testing `role == "You"`.
2. Compute runs in O(1) on append. A same-speaker message continues a run only
   when the immediately preceding model item is a message from that speaker;
   every tool, note, thinking, checklist or status event is a hard
   boundary. When append changes the previous row from `Single` to `First` or
   `Last` to `Middle`, emit its targeted `dataChanged` and height invalidation.
   Replay uses the same rule, so restored sessions look like live ones.
3. Refactor the message branch of `TranscriptDelegate::layoutRow` into shared
   metric/geometry helpers: `messageBubbleRect`, `messageHeaderRect`,
   `messageBodyRect` and `attachmentRects`. Paint user and agent surfaces from
   semantic tokens, align them independently and show identity/time only at the
   start of a run. The last message in a run retains enough lower spacing to
   make a speaker turn obvious.
4. Refactor `createEditor`, `updateEditorGeometry`, link hit-testing and
   attachment hit-testing to use those same helpers. Selection overlay geometry
   must remain pixel-aligned after a density, font, width, speaker or grouping
   change; it closes on the existing mutation rules.
5. Introduce `configureTranscriptDocument` beside the existing body-document
   setup. It applies the body/meta/code fonts and a **Qt-supported** default CSS
   stylesheet before loading the safe rendered HTML. Cover paragraphs, heading
   hierarchy, lists, block quotes, inline code, preformatted code, tables and
   links; leave raw model HTML disabled through `MarkdownUtil::setMarkdownSafe`.
   The explicit stylesheet must also be applied to the selection overlay so
   selection never causes a visual jump.
6. Preserve the existing cache contract: body-document cache keys include text,
   width, font, palette generation and chat-appearance generation. `sizeHint`
   still returns a cheap stale estimate during interactive resize; only the
   current viewport plus existing overscan is exactly measured on settle.

### Phase 3 — Activity stream and rich objects — **M–L**

1. Give tool, thinking and ordinary-note rows a shared compact activity layout:
   an accessible status icon/rail, concise action text, an optional summary and
   clear error/running state. Determine consecutive activity neighbours from
   their adjacent model rows at paint time (with narrowly invalidated neighbour
   repaint on insertion); do **not** merge rows or alter tool-result keys.
2. Keep the tool card's current actions, but make the primary row read as a
   human action first: tool name/action + the existing safe summary, then status.
   Move secondary copy/inspect affordances to a calm trailing action area (visible
   on focus/hover and always available by keyboard/context menu). Expanded input
   and result use the new code surface and more generous spacing. Failed results
   remain unmistakable without relying only on red.
3. Retain a stronger standalone treatment for plan checklists, permission
   banners and user questions. Re-space their contents with the same type scale
   and contrast tokens so interaction controls do not look bolted onto the feed.
4. Upgrade the existing attachment chip layout to typed attachment tiles.
   Derive icon/type from the already stored `kind`/`mediaType`; retain the
   current cached thumbnail path for images and never store additional file
   bytes in the model. Show compact metadata only when it can be supplied without
   synchronous I/O during paint; filename remains the authoritative label.
5. Tool-result image tiles and sent-message attachment tiles share the same
   painter/hit-test layout. All retain the current origin → cache-path fallback,
   path tooltip, outside-workspace marker and `openAttachment` dispatch.

### Phase 4 — Composer and scroll comfort — **M**

1. Replace the fixed 94 px composer height with an auto-growing `ComposerEdit`:
   minimum two readable lines, growth through a sensible seven-line cap, then
   its own vertical scrollbar. It must never resize the feed on every cursor
   blink; resize only when document block count/contents height crosses a
   threshold.
2. Place the editor, attachment state, send control and in-flight Interrupt
   affordance in one palette-driven rounded composer container. Keep the
   secondary setup, memory, changes, helpers, fork and stop controls in the
   responsive flow below it, with the existing narrow-panel wrapping behavior.
3. Make the jump-to-latest control read as an unread affordance when applicable
   (count or clear dot plus its existing tooltip), retain its keyboard reach and
   preserve the sticky-bottom 48 px tolerance. Do not add animated scrolling or
   per-frame effects.
4. Recheck composer drag/drop, image paste, slash popup anchoring, queued-message
   chips, draft restoration, empty state, permission focus and the Escape
   interrupt shortcut against the new hierarchy.

### Phase 5 — Verification, accessibility and performance gate — **M**

1. Extend `TranscriptModelTest` with semantic-speaker/run boundaries, message
   alignment/width geometry at wide and narrow viewports, density/appearance
   cache invalidation, new document metrics, activity-row state and attachment
   tile hit-testing. Keep coverage for tool result mutation, clipping and model
   eviction.
2. Extend `AgentPanelTest` with composer auto-grow/cap, attachment/drop and
   slash-popup placement after a composer height change, sticky-bottom/unread
   behavior, and selection-overlay alignment after an appearance update.
3. Extend the accessible-text tests so the visual simplification does not hide
   speaker identity, tool status, attachment labels, error state, plan progress
   or question/permission meaning from Orca. Focus/keyboard invocation of all
   tool actions must remain possible.
4. Add a deterministic offscreen regression fixture of 5,000 mixed rows and
   assert the existing invariants: row cap remains 5,000, document caches stay
   capped, changing viewport width returns cached estimates before the settle
   pass, and exact measuring walks only the viewport/overscan. Time observations
   belong in a non-flaky developer benchmark, not a CI threshold; record before
   and after scroll/resize traces on an ordinary 4K image + expanded-tool fixture.
5. Build with `scripts/build.sh`, run `ctest --test-dir build`, then manually
   review `akwidgets-preview transcript` in Midnight, Daylight and Follow System
   at compact/comfortable/spacious density. Finally smoke a live Claude and Kimi
   session with markdown, an attachment, a tool failure, a screenshot result,
   streamed response, a question and a long transcript resize/scroll.

## File map

| Change | Files |
|---|---|
| Chat settings, metrics and persistence | New `ui/src/state/ChatAppearance.{h,cpp}`; `ui/src/AppearanceDialog.{h,cpp}` |
| Semantic transcript colours | `ui/src/theme/Theme.h`, `ui/src/theme/ThemeManager.cpp` |
| Semantic speaker/run data and targeted mutations | `ui/src/TranscriptModel.{h,cpp}`, `ui/src/AgentPanel.cpp` |
| Bubbles, typography, activity rows, attachment tiles, hit-testing and cache keys | `ui/src/TranscriptDelegate.{h,cpp}` |
| Composer container and auto-sizing | `ui/src/AgentPanel.{h,cpp}` |
| Visual review fixture | `ui/tests/WidgetsPreview.cpp`, `ui/CMakeLists.txt` |
| Unit/widget/contrast regression coverage | `ui/tests/TranscriptModelTest.cpp`, `ui/tests/AgentPanelTest.cpp`, new focused `ui/tests/ChatAppearanceTest.cpp` if its dependencies stay small, `ui/CMakeLists.txt` |
| Plan index | `docs/plans/README.md` |

## Explicit non-goals

- No return to per-message widgets or an unbounded transcript model.
- No new network service, core protocol, transcript file format or model-specific
  presentation branch.
- No automatic media preview that reads arbitrary model-authored paths; the
  existing `SafeContent` policy and durable attachment cache remain mandatory.
- No full Slack/Discord clone: reactions, channels, multi-user presence and
  threaded replies are separate product decisions.
- No broad application stylesheet or fixed light/dark colours that break KDE
  schemes.

## Acceptance criteria

- A long conversation is visibly easier to scan: user and agent turns are
  distinguishable at a glance, prose has deliberate rhythm, and routine tool/
  system traffic recedes without becoming hidden.
- A code-heavy assistant reply, a link, a quote, a list, a table, a sent file,
  a screenshot result, a live tool, a failed tool, a checklist and an agent
  question are all legible, actionable and visually coherent in one feed.
- Compact/comfortable/spacious and text-size preferences apply live, persist,
  work under every bundled/system theme and meet the defined contrast checks.
- The transcript retains every current rich interaction and its screen-reader
  names; no click target or keyboard path regresses.
- The long-chat scalability contract remains demonstrably intact: model/view
  virtualization, per-row targeted invalidation, bounded caches/rows, cached
  thumbnail resolution, 50 ms stream batching and visible-row-only exact
  remeasurement all remain in place.

## Delivery order

Land phases **1 → 2 → 3 → 4 → 5**, one reviewable commit per phase. Phase 1
gives visual review and the tokens/settings every later phase consumes. Phase 2
is the highest-value readability change and must settle the shared geometry
contract before rich-object work. Phase 3 builds on that contract. Phase 4 is
intentionally last among visible work so a stable feed is not disrupted while
the composer is re-housed. Phase 5 gates the series; no earlier phase is called
complete without its focused tests and a visual preview check.

**Rough size: L–XL** (a focused UI release, with no core work).
