package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentkate/internal/harness"
)

// TestPersonaRoundTrip pins the on-disk shape of the persona a thread was
// launched with (plan 16 P3): it must survive a store reopen intact, since a
// resume rebuilds the launch from the record alone.
func TestPersonaRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rec := sampleRecord("t-persona")
	rec.SystemPrompt = "You are the arena's scout."
	rec.Agents = []harness.AgentProfile{{
		Name:        "reviewer",
		Description: "Reviews code",
		Prompt:      "You review.",
		Tools:       []string{"Read", "Glob"},
		Model:       "sonnet",
	}}
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("t-persona")
	if !ok {
		t.Fatal("record missing after reopen")
	}
	if got.SystemPrompt != rec.SystemPrompt {
		t.Errorf("SystemPrompt = %q", got.SystemPrompt)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("Agents = %+v", got.Agents)
	}
	a := got.Agents[0]
	if a.Name != "reviewer" || a.Description != "Reviews code" || a.Prompt != "You review." ||
		a.Model != "sonnet" || len(a.Tools) != 2 || a.Tools[0] != "Read" {
		t.Errorf("profile = %+v", a)
	}

	// The JSON keys are the documented ones, and a persona-less record writes
	// neither — an old record round-trips byte-identically.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, key := range []string{`"systemPrompt"`, `"agents"`, `"name"`, `"tools"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("on-disk JSON is missing %s: %s", key, raw)
		}
	}
}

// TestPersonaOmittedWhenEmpty keeps the persona keys out of every record that
// has none — the overwhelming majority — so the store stays readable and
// pre-P3 records are unchanged by a rewrite.
func TestPersonaOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(sampleRecord("t-plain"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"systemPrompt"`, `"agents"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("%s written for a record with no persona: %s", key, b)
		}
	}
}

// TestPersonaAbsentDecodes covers the backward-compatibility half: a record
// written before P3 decodes with an empty persona, which resumes exactly as it
// always did.
func TestPersonaAbsentDecodes(t *testing.T) {
	var rec Record
	if err := json.Unmarshal([]byte(
		`{"threadId":"t-old","sessionId":"s","project":"/p","status":"dormant"}`,
	), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if rec.SystemPrompt != "" || rec.Agents != nil {
		t.Errorf("pre-P3 record decoded with a persona: %q / %+v",
			rec.SystemPrompt, rec.Agents)
	}
}
