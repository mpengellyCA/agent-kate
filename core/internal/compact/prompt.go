package compact

// CompactPrompt is harness-neutral: it is an instruction to a model, not a CLI
// invocation. The mechanism that delivers it (an in-session turn, or a fresh
// pass over a stored session) belongs to the thread's harness — see
// harness.CompactSpec and each adapter's Compact.
//
// CompactPrompt is the user message sent to the compactor model. It asks for
// a dense, decision-aware summary the next session can resume from. Crafted
// to favour facts, decisions and pending work over restating tool output.
const CompactPrompt = `This conversation is being compacted before it is resumed in a new session. Produce a concise, information-dense summary that the next session can start from.

Include:
1. The original task and any evolved goal.
2. Key decisions made and the reasoning behind them (constraints, gotchas, anything that would surprise a fresh reader).
3. Files touched and their current state.
4. Open TODOs or unfinished work.
5. The most recent user request, verbatim.

Output only the summary, in markdown, with no preamble or sign-off. Aim for under 5,000 tokens.`
