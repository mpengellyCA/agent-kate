package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentkate/internal/compact"
	"agentkate/internal/harness"
	"agentkate/internal/kimi"
)

// fakeKimiCompactScript is a minimal `kimi acp` stand-in whose `/compact`
// reply length is chosen by the caller: short enough to be a status line, or
// long enough to be a real summary. Everything else is the smallest handshake
// the supervisor accepts.
func fakeKimiCompactScript(t *testing.T, reply string) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	path := filepath.Join(t.TempDir(), "fake-kimi-compact")
	body, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	script := `#!/usr/bin/env python3
import json, sys

sid = "session_compact-0001"
REPLY = ` + string(body) + `  # a JSON string literal is a Python one too

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    f = json.loads(line)
    method = f.get("method")
    fid = f.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": fid, "result": {
            "protocolVersion": 1, "agentCapabilities": {}, "authMethods": [],
            "agentInfo": {"name": "fake-kimi", "version": "0"}}})
    elif method in ("session/new", "session/resume"):
        send({"jsonrpc": "2.0", "id": fid,
              "result": {"sessionId": sid, "configOptions": []}})
    elif method == "session/prompt":
        text = "".join(b.get("text", "") for b in f["params"]["prompt"]
                       if b.get("type") == "text").strip()
        out = REPLY if text == "/compact" else "ok"
        send({"jsonrpc": "2.0", "method": "session/update",
              "params": {"sessionId": sid, "update": {
                  "sessionUpdate": "agent_message_chunk",
                  "content": {"type": "text", "text": out}}}})
        send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kimi: %v", err)
	}
	return path
}

// kimiHarnessOver starts a kimi thread against the given fake CLI and returns
// the harness adapter wrapping it.
func kimiHarnessOver(t *testing.T, exe, threadID string) *kimiHarness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ksup := kimi.NewSupervisor(exe, log, func(string, []json.RawMessage) {}, nil, t.TempDir())
	h := newKimiHarness(ksup, "", "")
	if _, err := ksup.Start(kimi.StartOptions{ID: threadID, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start kimi thread: %v", err)
	}
	t.Cleanup(func() { _ = ksup.Stop(threadID) })
	return h
}

// TestKimiHarnessCompactRefusesCold: kimi has no `--resume --print` pass and no
// claude-shaped transcript on disk, so a cold request is refused outright
// rather than answered with a summary of nothing. No supervisor is touched —
// the refusal happens before any thread is consulted.
func TestKimiHarnessCompactRefusesCold(t *testing.T) {
	h := newKimiHarness(nil, "", "")
	_, err := h.Compact(context.Background(), harness.CompactSpec{
		ThreadID: "t1", SessionID: "s1", Prompt: compact.CompactPrompt,
	})
	if err == nil {
		t.Fatal("a cold compaction was accepted")
	}
	if !strings.Contains(err.Error(), "live session") {
		t.Errorf("refusal %q does not say the session must be live", err)
	}
	// And the capability says the same thing, so the RPC layer never asks.
	descriptor := h.Descriptor()
	if !descriptor.Supports(harness.OperationCompaction) || descriptor.Supports(harness.OperationColdCompaction) {
		t.Errorf("descriptor compaction/coldCompaction = %v/%v, want true/false",
			descriptor.Supports(harness.OperationCompaction), descriptor.Supports(harness.OperationColdCompaction))
	}
}

// TestKimiHarnessCompactShortReplyIsInPlace: `/compact` answers with a status
// line, not a summary. Storing that line would be catastrophic — resume seeds
// a BRAND NEW session from a stored summary — so anything under the guard is
// reported as the in-place sentinel and nothing is handed back.
func TestKimiHarnessCompactShortReplyIsInPlace(t *testing.T) {
	h := kimiHarnessOver(t, fakeKimiCompactScript(t, "Compacted 12 messages."), "t-inplace")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body, err := h.Compact(ctx, harness.CompactSpec{ThreadID: "t-inplace", Hot: true})
	if !errors.Is(err, ErrCompactedInPlace) {
		t.Fatalf("Compact err = %v, want ErrCompactedInPlace", err)
	}
	if body != "" {
		t.Errorf("a status line was handed back as a summary: %q", body)
	}
}

// TestKimiHarnessCompactLongReplyIsASummary: the guard is a length test, not a
// blanket refusal — a reply substantial enough to be a real summary is stored.
func TestKimiHarnessCompactLongReplyIsASummary(t *testing.T) {
	long := strings.Repeat("the session so far. ", minKimiSummaryBytes/10)
	h := kimiHarnessOver(t, fakeKimiCompactScript(t, long), "t-summary")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body, err := h.Compact(ctx, harness.CompactSpec{ThreadID: "t-summary", Hot: true})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(body) < minKimiSummaryBytes {
		t.Fatalf("body is %d bytes, want the CLI's full reply", len(body))
	}
}

// TestKimiHarnessCompactNeedsARunningThread keeps the hot-only contract honest
// at the adapter: there is no mechanism to reach a dormant kimi session.
func TestKimiHarnessCompactNeedsARunningThread(t *testing.T) {
	h := kimiHarnessOver(t, fakeKimiCompactScript(t, "x"), "t-live")
	_, err := h.Compact(context.Background(), harness.CompactSpec{
		ThreadID: "t-not-running", Hot: true,
	})
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("Compact on a dead thread err = %v, want a resume-it-first refusal", err)
	}
}

// TestCompactNowReportsInPlaceAsSuccess is the other half of the sentinel: a
// caller must convert it, or an engine whose compaction WORKED reports failure
// to the user. Nothing is stored and the session is left alone — the compacted
// context lives inside the engine's own session.
func TestCompactNowReportsInPlaceAsSuccess(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	fake := &compactFake{
		fakeHarness: &fakeHarness{}, capable: true, running: true,
		hotOnly: true, inPlace: true,
	}
	client := compactTestCore(t, fake, sessions)

	var out struct {
		OK               bool   `json:"ok"`
		CompactedInPlace bool   `json:"compactedInPlace"`
		Detail           string `json:"detail"`
	}
	if err := client.Call("agent.compactNow",
		map[string]any{"threadId": rec.ThreadID, "model": "hot"}, &out); err != nil {
		t.Fatalf("hot compactNow reported the in-place sentinel as an error: %v", err)
	}
	if !out.OK || !out.CompactedInPlace {
		t.Fatalf("reply = %+v, want ok + compactedInPlace", out)
	}

	// No summary was invented, so a later resume re-attaches to the engine's
	// own (already compacted) session instead of seeding a fresh one from a
	// body no model ever wrote.
	var status struct {
		HasSummary bool `json:"hasSummary"`
	}
	if err := client.Call("agent.summaryStatus",
		map[string]any{"threadId": rec.ThreadID}, &status); err != nil {
		t.Fatalf("summaryStatus: %v", err)
	}
	if status.HasSummary {
		t.Error("an in-place compaction stored a summary; resume would seed from it")
	}
}

// TestColdCompactRefusedOnAHotOnlyEngine: the cold branch reads the session
// back from disk in the claude format and seeds a NEW session from what it
// finds. On an engine that has no such transcript that is destructive, so the
// gate refuses before the read — with a message that says what to do instead.
func TestColdCompactRefusedOnAHotOnlyEngine(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	fake := &compactFake{
		fakeHarness: &fakeHarness{}, capable: true, running: true, hotOnly: true,
	}
	client := compactTestCore(t, fake, sessions)

	for _, model := range []string{"sonnet", "local"} {
		err := client.Call("agent.compactNow",
			map[string]any{"threadId": rec.ThreadID, "model": model}, nil)
		if err == nil {
			t.Fatalf("cold compactNow (%s) ran on a hot-only engine", model)
		}
		if !strings.Contains(err.Error(), "live session") {
			t.Errorf("refusal %q does not tell the user to resume and compact hot", err)
		}
	}
	if fake.calls != 0 {
		t.Errorf("the harness was asked for a cold pass anyway (%d calls)", fake.calls)
	}

	// The hot mechanism is unaffected — the split gates the cold branch only.
	if err := client.Call("agent.compactNow",
		map[string]any{"threadId": rec.ThreadID, "model": "hot"}, nil); err != nil {
		t.Fatalf("hot compactNow on a hot-only engine: %v", err)
	}
}

// TestExitCompactSkipsHotOnlyEngines: the shutdown drain must not spawn a cold
// pass for an engine that only compacts live sessions. It would read nothing,
// store a summary of nothing, and make the next resume seed a fresh session
// from it — wiping the thread's continuity at quit.
func TestExitCompactSkipsHotOnlyEngines(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	rec.CompactStrategy = string(compact.ExitSonnetCold)
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	summaries, err := compact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("summary store: %v", err)
	}
	fake := &compactFake{fakeHarness: &fakeHarness{}, capable: true, hotOnly: true}
	harnesses := harness.NewRegistry("fake")
	harnesses.Register(fake)

	tracker := &exitCompactTracker{ctx: t.Context(), harnesses: harnesses}
	tracker.spawn(log, sessions, summaries, rec, compact.ExitSonnetCold)
	tracker.drain(func() {}, 2*time.Second, log)
	if fake.calls != 0 {
		t.Errorf("spawned a cold pass for a hot-only engine (%d calls)", fake.calls)
	}
	if sum, _ := summaries.Get(rec.ThreadID); sum != nil {
		t.Error("a summary was stored for a thread that was never cold-compacted")
	}
}
