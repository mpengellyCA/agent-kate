package main

import (
	"log/slog"

	"agentkate/internal/harness"
)

// transcriptDeleter is implemented by harnesses that own the transcript store
// they replay from, and can therefore delete it when a thread is destroyed. It
// is an OPTIONAL interface asserted at the call site (the same pattern as
// subagentTranscriber and modelDiscoverer) rather than a method on
// harness.Harness: a harness whose transcript belongs to its CLI must not be
// made to carry a stub that would tempt someone to fill it in.
//
// kimi implements it: `kimi-events/<threadId>.jsonl` is Agent Kate's own file,
// written by us, and until now nothing ever deleted it — not on discard, not on
// cleanup — so the data directory kept a full transcript of every thread the
// user had ever destroyed, forever (audit F10).
//
// claude deliberately does NOT: `~/.claude/projects/**/<session>.jsonl` is the
// CLI's file, and cleanup's own contract is that an archived thread stays
// recoverable *because* the transcript is left on disk.
type transcriptDeleter interface {
	DeleteTranscript(threadID string) error
}

// deleteThreadTranscript drops a destroyed thread's core-owned transcript.
//
// Resolve h while the thread's session record still exists — harnessFor falls
// back to the DEFAULT harness once the record is gone, which for a kimi thread
// would silently route the delete to claude (a no-op) and leak the log.
//
// Never fatal, and never propagated: a thread whose worktree has already been
// removed is gone whether or not its log could be unlinked, and failing the RPC
// here would leave the caller believing the destructive part did not happen.
func deleteThreadTranscript(h harness.Harness, threadID string, log *slog.Logger) {
	d, ok := h.(transcriptDeleter)
	if !ok {
		return
	}
	if err := d.DeleteTranscript(threadID); err != nil && log != nil {
		log.Warn("could not delete thread transcript", "thread", threadID, "err", err)
	}
}
